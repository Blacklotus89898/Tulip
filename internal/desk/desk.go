// Package desk turns proposals into suggestions and — only with the
// operator's approval — into paper fills (ADR 0002 §3). It is the ONLY place
// broker.Fill is called for the strategy book: from Approve, or — in the
// operator-enabled paper-training mode (ADR 0005) — from AutoExecute, for the
// rules' own suggestions only.
//
// All state is derived from journal events; the desk serialises its own
// writes with a mutex (one process, one SQLite writer).
package desk

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/approval"
	"github.com/Blacklotus89898/Tulip/internal/broker"
	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/journal"
	"github.com/Blacklotus89898/Tulip/internal/marketdata"
	"github.com/Blacklotus89898/Tulip/internal/portfolio"
	"github.com/Blacklotus89898/Tulip/internal/strategy"
	"github.com/Blacklotus89898/Tulip/internal/thorn"
)

// Event kinds owned by the desk.
const (
	KindSuggestionCreated  = "suggestion.created"
	KindSuggestionRejected = "suggestion.rejected" // Thorn said no at propose time
	KindSuggestionDeclined = "suggestion.declined"
	KindSuggestionExpired  = "suggestion.expired"
	KindApprovalAccepted   = "approval.accepted"
	KindApprovalDenied     = "approval.denied"
	KindOrderRejected      = "order.rejected" // approved, but failed the execution checks
	KindKillArmed          = "killswitch.armed"
	KindKillDisarmed       = "killswitch.disarmed"
	KindMark               = "portfolio.mark"
)

// Config tunes the desk.
type Config struct {
	Thorn       thorn.Config
	TTL         time.Duration // suggestion lifetime (default 30m)
	BandPct     float64       // max |quote-ref|/ref at execution (default 0.03)
	MaxQuoteAge time.Duration // freshest acceptable quote (default 30m; TSX free data is ~15m delayed)
	InitialCash float64       // per book (default 10000)
	Benchmark   string        // buy-and-hold symbol (default XIU.TO)
}

func (c Config) withDefaults() Config {
	if c.TTL == 0 {
		c.TTL = 30 * time.Minute
	}
	if c.BandPct == 0 {
		c.BandPct = 0.03
	}
	if c.MaxQuoteAge == 0 {
		c.MaxQuoteAge = 30 * time.Minute
	}
	if c.InitialCash == 0 {
		c.InitialCash = 10000
	}
	if c.Benchmark == "" {
		c.Benchmark = "XIU.TO"
	}
	if c.Thorn.MaxOrderValue == 0 {
		c.Thorn = thorn.DefaultConfig()
	}
	return c
}

// Suggestion is a Thorn-approved proposal waiting for the operator.
type Suggestion struct {
	ID         string              `json:"id"`
	Created    time.Time           `json:"created"`
	Expires    time.Time           `json:"expires"`
	Symbol     string              `json:"symbol"`
	Side       string              `json:"side"`
	Qty        int                 `json:"qty"`
	RefPrice   float64             `json:"ref_price"`
	BandPct    float64             `json:"band_pct"`
	SignalOn   string              `json:"signal_on,omitempty"` // date of the signal bar
	Source     string              `json:"source"`              // rules | brain | claude-code
	Rationale  string              `json:"rationale"`
	Confidence *float64            `json:"confidence,omitempty"`
	Candidate  *strategy.Candidate `json:"candidate,omitempty"`
	Hash       string              `json:"hash"` // binds an approval to exactly this proposal
}

// ProposeRequest is a trade idea from the rules, the brain or Claude Code.
type ProposeRequest struct {
	Symbol, Side string
	Qty          int
	Source       string
	Rationale    string
	Confidence   *float64
	Candidate    *strategy.Candidate
}

// Desk is the suggestion → approval → execution path.
type Desk struct {
	J              *journal.Journal
	MD             marketdata.Source
	Broker         broker.Paper
	Cfg            Config
	PassphraseHash string // TULIP_APPROVAL_PASSPHRASE_HASH; empty = approvals disabled
	Limiter        *approval.Limiter
	Notify         func(Suggestion) // set by the service (ntfy); may be nil
	Account        AccountSource    // co-pilot mode (ADR 0003): the real account; nil = paper
	AutoPaper      bool             // paper training (ADR 0005): rules suggestions fill without approval
	Now            func() time.Time

	mu sync.Mutex
}

// Now0 is the desk's clock (exported for status reporting).
func (d *Desk) Now0() time.Time { return d.now() }

func (d *Desk) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Desk) cfg() Config { return d.Cfg.withDefaults() }

// Errors the approval path returns (always journaled as approval.denied).
var (
	ErrApprovalsDisabled = errors.New("approvals are disabled: no passphrase hash configured")
	ErrLocked            = errors.New("too many wrong passphrases — approvals locked for a while")
	ErrBadPassphrase     = errors.New("wrong passphrase")
	ErrNotPending        = errors.New("suggestion is not pending (approved, declined, expired or unknown)")
	ErrChanged           = errors.New("the proposal changed since it was displayed")
	ErrExpired           = errors.New("suggestion expired")
	ErrMarketClosed      = errors.New("TSX is closed — approve during regular hours (the suggestion stays pending until it expires)")
)

// ---- state, derived from the journal ----

// State is the replayed view the tools and the loop need.
type State struct {
	Books       map[string]*portfolio.Book
	Pending     []Suggestion
	Expired     []Suggestion // expired without being acted on (co-pilot reconciliation)
	KillSwitch  bool
	KillReason  string
	CreatedDays map[string]int // suggestions created per Toronto date
}

// LoadState replays the journal.
func (d *Desk) LoadState(ctx context.Context) (*State, error) {
	evs, err := d.J.ByKinds(ctx, portfolio.KindFunded, portfolio.KindFill,
		KindSuggestionCreated, KindSuggestionDeclined, KindSuggestionExpired, KindOrderRejected,
		KindSuggestionActed, KindKillArmed, KindKillDisarmed)
	if err != nil {
		return nil, err
	}
	books, err := portfolio.Replay(evs, portfolio.Strategy, portfolio.Benchmark)
	if err != nil {
		return nil, err
	}
	st := &State{Books: books, CreatedDays: map[string]int{}}
	pending := map[string]Suggestion{}
	expired := map[string]Suggestion{}
	var order []string
	for _, e := range evs {
		switch e.Kind {
		case KindSuggestionCreated:
			var s Suggestion
			if json.Unmarshal(e.Payload, &s) == nil {
				pending[s.ID] = s
				order = append(order, s.ID)
				st.CreatedDays[s.Created.In(calendar.Toronto).Format("2006-01-02")]++
			}
		case KindSuggestionDeclined, KindSuggestionExpired, KindOrderRejected, KindSuggestionActed:
			var t struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(e.Payload, &t) == nil {
				if s, ok := pending[t.ID]; ok && e.Kind == KindSuggestionExpired {
					expired[t.ID] = s
				}
				delete(pending, t.ID)
				if e.Kind == KindSuggestionActed {
					delete(expired, t.ID)
				}
			}
		case portfolio.KindFill:
			var f portfolio.Fill
			if json.Unmarshal(e.Payload, &f) == nil && f.SuggestionID != "" {
				delete(pending, f.SuggestionID)
			}
		case KindKillArmed:
			var k struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(e.Payload, &k)
			st.KillSwitch, st.KillReason = true, k.Reason
		case KindKillDisarmed:
			st.KillSwitch, st.KillReason = false, ""
		}
	}
	for _, id := range order {
		if s, ok := pending[id]; ok {
			st.Pending = append(st.Pending, s)
		}
		if s, ok := expired[id]; ok {
			st.Expired = append(st.Expired, s)
		}
	}
	return st, nil
}

// EnsureFunded seeds both books once.
func (d *Desk) EnsureFunded(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, err := d.LoadState(ctx)
	if err != nil {
		return err
	}
	for _, b := range []string{portfolio.Strategy, portfolio.Benchmark} {
		if !st.Books[b].Funded {
			if _, err := d.J.Append(ctx, portfolio.KindFunded, "", portfolio.Funded{Book: b, Cash: d.cfg().InitialCash}); err != nil {
				return err
			}
		}
	}
	return nil
}

// dayPnL is today's change in the strategy book's equity since its first mark
// of the day (0 if not marked yet today).
func (d *Desk) dayPnL(ctx context.Context, equity float64) float64 {
	if d.CoPilot() {
		return d.brokerDayPnL(ctx, equity)
	}
	evs, err := d.J.ByKinds(ctx, KindMark)
	if err != nil {
		return 0
	}
	today := d.now().In(calendar.Toronto).Format("2006-01-02")
	for _, e := range evs {
		var m struct {
			Book   string  `json:"book"`
			Day    string  `json:"day"`
			Equity float64 `json:"equity"`
		}
		if json.Unmarshal(e.Payload, &m) == nil && m.Book == portfolio.Strategy && m.Day == today {
			return equity - m.Equity
		}
	}
	return 0
}

func (d *Desk) quotes(ctx context.Context, b *portfolio.Book) map[string]float64 {
	px := map[string]float64{}
	for sym := range b.Positions {
		if q, err := d.MD.Quote(ctx, sym); err == nil {
			px[sym] = q.Price
		}
	}
	return px
}

// ---- propose ----

// MinRationale is the shortest justification a suggestion may carry: every
// suggestion, from any source, must say why (operator requirement 2026-10-25).
const MinRationale = 20

// ErrNoJustification rejects a proposal without a real rationale.
var ErrNoJustification = errors.New("a suggestion needs a justification (rationale of at least 20 characters)")

// Propose runs Thorn and, if it passes, creates a pending suggestion and
// notifies the operator. Thorn rejections are journaled too.
func (d *Desk) Propose(ctx context.Context, cycle string, r ProposeRequest) (*Suggestion, thorn.Verdict, error) {
	if len(strings.TrimSpace(r.Rationale)) < MinRationale {
		return nil, thorn.Verdict{}, ErrNoJustification
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cfg := d.cfg()
	st, err := d.LoadState(ctx)
	if err != nil {
		return nil, thorn.Verdict{}, err
	}
	book, equity, err := d.sizingBook(ctx, cycle, st)
	if err != nil {
		return nil, thorn.Verdict{}, err
	}

	ref, signalOn := 0.0, ""
	if r.Candidate != nil {
		ref, signalOn = r.Candidate.RefPrice, r.Candidate.SignalOn
	} else {
		q, err := d.MD.Quote(ctx, r.Symbol)
		if err != nil {
			_, _ = d.J.Append(ctx, "data.unavailable", cycle, map[string]string{"symbol": r.Symbol, "error": err.Error()})
			return nil, thorn.Verdict{}, err
		}
		ref = q.Price
	}
	now := d.now()
	verdict := thorn.Check(cfg.Thorn, thorn.Request{
		Stage: thorn.Propose, Symbol: r.Symbol, Side: r.Side, Qty: r.Qty, Price: ref,
		Held: book.Held(r.Symbol), Cash: book.Cash, Equity: equity, DayPnL: d.dayPnL(ctx, equity),
		Confidence: r.Confidence, SuggestionsToday: st.CreatedDays[now.In(calendar.Toronto).Format("2006-01-02")],
		KillSwitch: st.KillSwitch,
	})
	if !verdict.Allowed {
		_, err := d.J.Append(ctx, KindSuggestionRejected, cycle, map[string]any{
			"symbol": r.Symbol, "side": r.Side, "qty": r.Qty, "source": r.Source, "reasons": verdict.Reasons,
		})
		return nil, verdict, err
	}

	expires := now.Add(cfg.TTL)
	if d.CoPilot() {
		// The operator places it by hand: it stays open until the session closes.
		expires = sessionClose(now, cfg.TTL)
	}
	s := Suggestion{
		ID: newID("sug"), Created: now.UTC(), Expires: expires.UTC(),
		Symbol: r.Symbol, Side: r.Side, Qty: r.Qty, RefPrice: ref, BandPct: cfg.BandPct,
		SignalOn: signalOn, Source: r.Source, Rationale: r.Rationale, Confidence: r.Confidence, Candidate: r.Candidate,
	}
	s.Hash = proposalHash(s)
	if _, err := d.J.Append(ctx, KindSuggestionCreated, cycle, s); err != nil {
		return nil, verdict, err
	}
	if d.Notify != nil && !(d.AutoPaper && s.Source == "rules") { // auto-filled: nothing to approve
		d.Notify(s)
	}
	return &s, verdict, nil
}

// proposalHash binds an approval to the exact terms that were displayed.
func proposalHash(s Suggestion) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d|%.6f|%.6f|%s",
		s.ID, s.Symbol, s.Side, s.Qty, s.RefPrice, s.BandPct, s.Expires.UTC().Format(time.RFC3339))))
	return hex.EncodeToString(h[:16])
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// ---- risk-reducing actions (any caller) ----

// Decline closes a pending suggestion without trading.
func (d *Desk) Decline(ctx context.Context, id, by, reason string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.pending(ctx, id); err != nil {
		return err
	}
	_, err := d.J.Append(ctx, KindSuggestionDeclined, "", map[string]string{"id": id, "by": by, "reason": reason})
	return err
}

// ArmKillSwitch stops all execution until a human disarms it.
func (d *Desk) ArmKillSwitch(ctx context.Context, by, reason string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.J.Append(ctx, KindKillArmed, "", map[string]string{"by": by, "reason": reason})
	return err
}

// ExpireDue closes suggestions past their TTL.
func (d *Desk) ExpireDue(ctx context.Context) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, err := d.LoadState(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range st.Pending {
		if d.now().After(s.Expires) {
			if _, err := d.J.Append(ctx, KindSuggestionExpired, "", map[string]string{"id": s.ID}); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

func (d *Desk) pending(ctx context.Context, id string) (*Suggestion, error) {
	st, err := d.LoadState(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range st.Pending {
		if s.ID == id {
			return &s, nil
		}
	}
	return nil, ErrNotPending
}

// ---- the human gate ----

// Approver identifies who approved, for the journal.
type Approver struct{ IP, UserAgent string }

// DisarmKillSwitch is a human action: it needs the passphrase.
func (d *Desk) DisarmKillSwitch(ctx context.Context, passphrase string, who Approver) error {
	if err := d.checkPassphrase(ctx, "killswitch", passphrase, who); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.J.Append(ctx, KindKillDisarmed, "", map[string]string{"ip": who.IP, "user_agent": who.UserAgent})
	return err
}

func (d *Desk) deny(ctx context.Context, id string, who Approver, err error) error {
	_, _ = d.J.Append(ctx, KindApprovalDenied, "", map[string]string{
		"id": id, "reason": err.Error(), "ip": who.IP, "user_agent": who.UserAgent,
	})
	return err
}

func (d *Desk) checkPassphrase(ctx context.Context, id, passphrase string, who Approver) error {
	if d.PassphraseHash == "" {
		return d.deny(ctx, id, who, ErrApprovalsDisabled)
	}
	if d.Limiter != nil && d.Limiter.Locked() {
		return d.deny(ctx, id, who, ErrLocked)
	}
	if !approval.Verify(d.PassphraseHash, passphrase) {
		if d.Limiter != nil {
			d.Limiter.Fail()
		}
		return d.deny(ctx, id, who, ErrBadPassphrase)
	}
	return nil
}

// Approve is the operator approving one suggestion. On success the paper
// order executes immediately (subject to the execution checks) and the fill
// is returned. This is the only path to broker.Fill for the strategy book.
func (d *Desk) Approve(ctx context.Context, id, displayedHash, passphrase string, who Approver) (*portfolio.Fill, error) {
	if d.CoPilot() {
		return nil, ErrCoPilot // Tulip never fills against a real account
	}
	if err := d.checkPassphrase(ctx, id, passphrase, who); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	s, err := d.pending(ctx, id)
	if err != nil {
		return nil, d.deny(ctx, id, who, err)
	}
	if displayedHash != s.Hash {
		return nil, d.deny(ctx, id, who, ErrChanged)
	}
	now := d.now()
	if now.After(s.Expires) {
		_, _ = d.J.Append(ctx, KindSuggestionExpired, "", map[string]string{"id": s.ID})
		return nil, d.deny(ctx, id, who, ErrExpired)
	}
	if open, _ := calendar.IsOpen(now); !open {
		return nil, d.deny(ctx, id, who, ErrMarketClosed) // not claimed: retry within TTL
	}
	if err := d.J.ClaimApproval(ctx, id); err != nil {
		return nil, d.deny(ctx, id, who, err)
	}
	_, _ = d.J.Append(ctx, KindApprovalAccepted, "", map[string]string{"id": id, "ip": who.IP, "user_agent": who.UserAgent})
	return d.execute(ctx, s, now)
}

// execute runs the execution checks — fresh quote, no lookahead, band, Thorn
// again — and fills. The caller holds d.mu and has claimed the suggestion.
func (d *Desk) execute(ctx context.Context, s *Suggestion, now time.Time) (*portfolio.Fill, error) {
	cfg := d.cfg()
	id := s.ID
	reject := func(reasons ...string) (*portfolio.Fill, error) {
		_, _ = d.J.Append(ctx, KindOrderRejected, "", map[string]any{"id": id, "reasons": reasons})
		return nil, fmt.Errorf("order rejected at execution: %s", strings.Join(reasons, "; "))
	}
	q, err := d.MD.Quote(ctx, s.Symbol)
	if err != nil {
		return reject("no quote: " + err.Error())
	}
	if now.Sub(q.Time) > cfg.MaxQuoteAge {
		return reject(fmt.Sprintf("quote is %s old (max %s)", now.Sub(q.Time).Round(time.Second), cfg.MaxQuoteAge))
	}
	if s.SignalOn != "" {
		day, err := time.ParseInLocation("2006-01-02", s.SignalOn, calendar.Toronto)
		_, signalClose := calendar.SessionBounds(day)
		if err != nil || !q.Time.After(signalClose) {
			return reject("quote is not newer than the signal bar's close (would fill at the signal price)")
		}
	}
	if move := math.Abs(q.Price-s.RefPrice) / s.RefPrice; move > s.BandPct {
		return reject(fmt.Sprintf("price moved %.1f%% from %.2f to %.2f (band %.0f%%)", 100*move, s.RefPrice, q.Price, 100*s.BandPct))
	}
	st, err := d.LoadState(ctx)
	if err != nil {
		return reject("state: " + err.Error())
	}
	book := st.Books[portfolio.Strategy]
	equity := book.Equity(d.quotes(ctx, book))
	v := thorn.Check(cfg.Thorn, thorn.Request{
		Stage: thorn.Execute, Symbol: s.Symbol, Side: s.Side, Qty: s.Qty, Price: q.Price,
		Held: book.Held(s.Symbol), Cash: book.Cash, Equity: equity, DayPnL: d.dayPnL(ctx, equity),
		Confidence: s.Confidence, KillSwitch: st.KillSwitch,
	})
	if !v.Allowed {
		return reject(v.Reasons...)
	}
	fill, err := d.Broker.Fill(portfolio.Strategy, s.Side, s.Symbol, s.Qty, q, s.ID)
	if err != nil {
		return reject(err.Error())
	}
	if _, err := d.J.Append(ctx, portfolio.KindFill, "", fill); err != nil {
		return nil, err
	}
	return &fill, nil
}

// ---- paper training: auto-execution (Tulip ADR 0005) ----

// KindAutoExecuted marks a fill that no human approved: paper training. Never
// approval.accepted — the journal must keep the two periods apart.
const KindAutoExecuted = "order.auto_executed"

// ErrAutoExecute refuses auto-execution outside its narrow lane.
var ErrAutoExecute = errors.New("auto-execution is paper-only and for the rules' own suggestions")

// AutoExecute fills a pending RULES suggestion in the paper book without an
// approval — the operator's paper-training mode (ADR 0005, set by the
// operator via TULIP_AUTOEXECUTE=paper). Only the loop calls it: there is no
// HTTP route or MCP tool, and suggestions from any other source (brain,
// claude-code) are refused, so a proposal can never approve itself. Same
// single-use claim and execution checks as Approve; never in co-pilot mode.
func (d *Desk) AutoExecute(ctx context.Context, id string) (*portfolio.Fill, error) {
	if !d.AutoPaper || d.CoPilot() {
		return nil, ErrAutoExecute
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s, err := d.pending(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.Source != "rules" {
		return nil, ErrAutoExecute
	}
	now := d.now()
	if open, _ := calendar.IsOpen(now); !open {
		return nil, ErrMarketClosed
	}
	if err := d.J.ClaimApproval(ctx, id); err != nil {
		return nil, err
	}
	_, _ = d.J.Append(ctx, KindAutoExecuted, "", map[string]string{"id": id, "by": "paper-training"})
	return d.execute(ctx, s, now)
}

// ---- benchmark + marks (no human involved: accounting, not trading) ----

// InitBenchmark buys the benchmark with the benchmark book's cash, once,
// during market hours.
func (d *Desk) InitBenchmark(ctx context.Context) (*portfolio.Fill, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cfg := d.cfg()
	st, err := d.LoadState(ctx)
	if err != nil {
		return nil, err
	}
	b := st.Books[portfolio.Benchmark]
	if !b.Funded || len(b.Positions) > 0 || b.Fills > 0 {
		return nil, nil
	}
	if open, _ := calendar.IsOpen(d.now()); !open {
		return nil, nil
	}
	q, err := d.MD.Quote(ctx, cfg.Benchmark)
	if err != nil {
		return nil, err
	}
	qty := int(math.Floor(b.Cash / (q.Price * 1.002)))
	if qty < 1 {
		return nil, nil
	}
	fill, err := d.Broker.Fill(portfolio.Benchmark, "buy", cfg.Benchmark, qty, q, "")
	if err != nil {
		return nil, err
	}
	_, err = d.J.Append(ctx, portfolio.KindFill, "", fill)
	return &fill, err
}

// Mark records each book's equity once per Toronto day (the daily P&L base
// and the strategy-vs-benchmark series).
func (d *Desk) Mark(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, err := d.LoadState(ctx)
	if err != nil {
		return err
	}
	today := d.now().In(calendar.Toronto).Format("2006-01-02")
	marks, _ := d.J.ByKinds(ctx, KindMark)
	done := map[string]bool{}
	for _, e := range marks {
		var m struct{ Book, Day string }
		if json.Unmarshal(e.Payload, &m) == nil && m.Day == today {
			done[m.Book] = true
		}
	}
	for name, b := range st.Books {
		if done[name] || !b.Funded {
			continue
		}
		if _, err := d.J.Append(ctx, KindMark, "", map[string]any{
			"book": name, "day": today, "equity": b.Equity(d.quotes(ctx, b)), "cash": b.Cash,
		}); err != nil {
			return err
		}
	}
	return nil
}
