package loop

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/approval"
	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/desk"
	"github.com/Blacklotus89898/Tulip/internal/journal"
	"github.com/Blacklotus89898/Tulip/internal/marketdata"
	"github.com/Blacklotus89898/Tulip/internal/portfolio"
	"github.com/Blacklotus89898/Tulip/internal/shadow"
	"github.com/Blacklotus89898/Tulip/internal/strategy"
	"github.com/Blacklotus89898/Tulip/internal/thorn"
)

var syms = []string{"VFV.TO", "XEF.TO", "XIU.TO", "ZAG.TO"}

type fakeMD struct {
	mu  sync.Mutex
	px  map[string]float64
	now func() time.Time
}

func (f *fakeMD) Daily(context.Context, string) ([]marketdata.Candle, error) { return nil, nil }
func (f *fakeMD) Quote(_ context.Context, s string) (marketdata.Quote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return marketdata.Quote{Symbol: s, Price: f.px[s], Time: f.now().Add(-15 * time.Minute)}, nil
}

// fakeHist: uptrends for XIU/VFV, a downtrend for XEF, flat ZAG — 300 weekdays
// ending before the month; plus one bar INSIDE the month that must be ignored.
type fakeHist struct{ monthStart time.Time }

func (h fakeHist) HistorySince(_ context.Context, s string, _ time.Time) ([]marketdata.Candle, error) {
	var out []marketdata.Candle
	d := h.monthStart.AddDate(0, 0, -1)
	for len(out) < 300 {
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
			out = append([]marketdata.Candle{{Day: d}}, out...)
		}
		d = d.AddDate(0, 0, -1)
	}
	for i := range out {
		x := float64(i)
		px := map[string]float64{"XIU.TO": 30 + 0.05*x, "VFV.TO": 80 + 0.1*x, "XEF.TO": 45 - 0.05*x, "ZAG.TO": 15 + 0.01*float64(i%3)}[s] // ZAG nearly flat (not zero-vol: inverse-vol needs some)
		out[i].Open, out[i].High, out[i].Low, out[i].Close = px, px, px, px
	}
	// A crash on the 1st of the month: completed only "today" — must not count.
	out = append(out, marketdata.Candle{Day: h.monthStart, Open: 1, High: 1, Low: 1, Close: 1})
	return out, nil
}

const pass = "a long operator passphrase"

func harness(t *testing.T, now *time.Time) (*Loop, *desk.Desk) {
	t.Helper()
	j, err := journal.Open(context.Background(), filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	clock := func() time.Time { return *now }
	md := &fakeMD{px: map[string]float64{"XIU.TO": 45, "VFV.TO": 110, "XEF.TO": 30, "ZAG.TO": 15}, now: clock}
	th := thorn.DefaultConfig()
	th.Allowlist, th.MaxOrderValue, th.MaxPositionWeight, th.MaxSuggestionsPerDay = syms, 1e5, 0.4, 20
	th.MaxWeightBySymbol = map[string]float64{"ZAG.TO": 1}
	hash, err := approval.HashPassphrase(pass)
	if err != nil {
		t.Fatal(err)
	}
	d := &desk.Desk{J: j, MD: md, Cfg: desk.Config{Thorn: th}, PassphraseHash: hash, Limiter: &approval.Limiter{}, Now: clock}
	if err := d.EnsureFunded(context.Background()); err != nil {
		t.Fatal(err)
	}
	ms := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, calendar.Toronto)
	l := &Loop{Desk: d, MD: md, Symbols: syms, Now: clock,
		Allocator: strategy.TrendFilter{Risk: []string{"VFV.TO", "XEF.TO", "XIU.TO"}, Safe: "ZAG.TO"}, Hist: fakeHist{ms}}
	return l, d
}

func pendingBy(t *testing.T, d *desk.Desk) map[string]desk.Suggestion {
	st, err := d.LoadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]desk.Suggestion{}
	for _, s := range st.Pending {
		out[s.Symbol] = s
	}
	return out
}

func TestMonthlyRebalanceProposesApprovesAndCloses(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 11, 0, 0, 0, calendar.Toronto) // first trading day of November
	l, d := harness(t, &now)

	res, err := l.Cycle(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	p := pendingBy(t, d)
	// XEF is in a downtrend → its third goes to ZAG: buys for VFV, XIU, ZAG only.
	if len(p) != 3 || p["XEF.TO"].ID != "" || p["ZAG.TO"].Side != "buy" || len(res.Suggestions) != 3 {
		t.Fatalf("want buys VFV/XIU/ZAG, got %v (res %+v)", p, res)
	}
	// ~1/3 each of 10k: VFV 110 → 30 shares, XIU 45 → 74, ZAG 15 → 222.
	if p["VFV.TO"].Qty != 30 || p["XIU.TO"].Qty != 74 || p["ZAG.TO"].Qty != 222 {
		t.Errorf("sizes = VFV %d XIU %d ZAG %d", p["VFV.TO"].Qty, p["XIU.TO"].Qty, p["ZAG.TO"].Qty)
	}
	// Every suggestion carries its justification: the rule with its numbers,
	// what the order does, and the signal date.
	for sym, s := range p {
		for _, want := range []string{"Why: ", "Order: Buy ", "Rule: trend-filter-200 monthly rebalance for 2026-11"} {
			if !strings.Contains(s.Rationale, want) {
				t.Errorf("%s justification lacks %q:\n%s", sym, want, s.Rationale)
			}
		}
	}
	if r := p["ZAG.TO"].Rationale; !strings.Contains(r, "takes the share of every ETF below its 200-day average (XEF.TO)") {
		t.Errorf("ZAG must say whose share it takes:\n%s", r)
	}
	if r := p["VFV.TO"].Rationale; !strings.Contains(r, "above its 200-day average") {
		t.Errorf("VFV must cite its trend:\n%s", r)
	}
	// Another cycle the same day: nothing duplicated.
	if res, _ := l.Cycle(ctx, "test"); len(res.Suggestions) != 0 {
		t.Fatalf("re-proposed while pending: %v", res.Suggestions)
	}
	// The operator approves all three.
	for _, s := range p {
		if _, err := d.Approve(ctx, s.ID, s.Hash, pass, desk.Approver{IP: "test"}); err != nil {
			t.Fatalf("approve %s: %v", s.Symbol, err)
		}
	}
	// Next cycle: on target → month closed; later cycles do nothing.
	if res, _ := l.Cycle(ctx, "test"); len(res.Suggestions) != 0 {
		t.Fatalf("on target, yet proposed %v", res.Suggestions)
	}
	if done, _ := l.monthDone(ctx, "2026-11"); !done {
		t.Error("rebalance.done not journaled once on target")
	}
}

func TestSellsGoFirstAndBuysWait(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 11, 0, 0, 0, calendar.Toronto)
	l, d := harness(t, &now)
	// Start fully in XEF via an approved one-off suggestion.
	s, v, err := d.Propose(ctx, "seed", desk.ProposeRequest{Rationale: testWhy, Symbol: "XEF.TO", Side: "buy", Qty: 130, Source: "test"})
	if err != nil || s == nil {
		t.Fatalf("seed: %v %v %v", s, v, err)
	}
	if _, err := d.Approve(ctx, s.ID, s.Hash, pass, desk.Approver{IP: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Cycle(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	p := pendingBy(t, d)
	if p["XEF.TO"].Side != "sell" || len(p) != 1 {
		t.Fatalf("the XEF exit must be proposed alone (buys wait for its cash), got %v", p)
	}
	// A sell in a cash account says what it realizes, from the book's cost.
	if r := p["XEF.TO"].Rationale; !strings.Contains(r, "Tax: realizes an estimated") || !strings.Contains(r, "cash account") ||
		!strings.Contains(r, "below its 200-day average") || !strings.Contains(r, "Order: Sell 130 XEF.TO") {
		t.Errorf("sell justification:\n%s", r)
	}
}

func TestOutsideTheWindowNothingHappens(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 11, 0, 0, 0, calendar.Toronto) // 9th trading day
	l, d := harness(t, &now)
	if _, err := l.Cycle(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if p := pendingBy(t, d); len(p) != 0 {
		t.Errorf("rebalanced outside the month's window: %v", p)
	}
}

func TestPreviewIsReadOnly(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 11, 0, 0, 0, calendar.Toronto) // outside the window: preview still works
	l, d := harness(t, &now)
	l.Hist = fakeHist{time.Date(2026, 11, 12, 0, 0, 0, 0, calendar.Toronto)}
	p, unavailable, err := l.Preview(ctx)
	if err != nil || p == nil {
		t.Fatalf("preview = (%v, %v, %v)", p, unavailable, err)
	}
	if len(p.Buys) != 3 || p.Targets["XEF.TO"] != 0 {
		t.Errorf("preview plan = %+v", p)
	}
	if pend := pendingBy(t, d); len(pend) != 0 {
		t.Errorf("preview must not propose, got %v", pend)
	}
	evs, _ := d.J.ByKinds(ctx, KindTargets, "suggestion.created")
	if len(evs) != 0 {
		t.Errorf("preview journaled %d events", len(evs))
	}
}

const testWhy = "test: a justification long enough to pass"

func TestShadowsRunAlongsideWithoutTouchingTheLiveBook(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 11, 0, 0, 0, calendar.Toronto)
	l, d := harness(t, &now)
	l.Shadow = &shadow.Runner{J: d.J, Allocators: strategy.Candidates(syms), Symbols: syms, Thorn: d.Cfg.Thorn}
	res, err := l.Cycle(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Suggestions) != 3 || len(pendingBy(t, d)) != 3 {
		t.Fatalf("live suggestions changed with shadows on: %+v", res)
	}
	evs, _ := d.J.ByKinds(ctx, shadow.KindRebalanced)
	if len(evs) != len(strategy.Candidates(syms)) {
		t.Errorf("shadow rebalances = %d, want one per candidate", len(evs))
	}
	fills, _ := d.J.ByKinds(ctx, portfolio.KindFill)
	for _, e := range fills {
		if strings.Contains(string(e.Payload), `"book":"strategy"`) {
			t.Errorf("nothing was approved, yet the strategy book has a fill: %s", e.Payload)
		}
	}
	st, _ := d.LoadState(ctx)
	if b := st.Books[portfolio.Strategy]; len(b.Positions) != 0 || b.Cash != 10000 {
		t.Errorf("live book moved without an approval: %+v", b)
	}
}

func TestScorecardPushedOncePerMonth(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 11, 0, 0, 0, calendar.Toronto)
	l, d := harness(t, &now)
	l.Shadow = &shadow.Runner{J: d.J, Allocators: strategy.Candidates(syms), Symbols: syms, Thorn: d.Cfg.Thorn}
	var titles []string
	l.Report = func(_ context.Context, title, body string) error {
		titles = append(titles, title)
		return nil
	}
	for _, at := range []time.Time{now, now.Add(4 * time.Hour), now.AddDate(0, 0, 1)} {
		now = at
		if _, err := l.Cycle(ctx, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if len(titles) != 1 || titles[0] != "Tulip scorecard — November 2026" {
		t.Errorf("pushes = %v, want exactly one for November", titles)
	}
}

// Paper training (ADR 0005): the month's rebalance fills in one cycle — sells
// first, then buys re-planned from the cash the sells freed — with nothing
// left pending and no approval journaled.
func TestPaperTrainingRebalanceFillsWithoutApproval(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 11, 0, 0, 0, calendar.Toronto)
	l, d := harness(t, &now)
	d.AutoPaper = true
	// Seed a holding the trend filter wants gone: XEF is in a downtrend.
	q, _ := l.MD.Quote(ctx, "XEF.TO")
	f, err := d.Broker.Fill(portfolio.Strategy, "buy", "XEF.TO", 100, q, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.J.Append(ctx, portfolio.KindFill, "", f); err != nil {
		t.Fatal(err)
	}

	if _, err := l.Cycle(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	st, err := d.LoadState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b := st.Books[portfolio.Strategy]
	if len(st.Pending) != 0 || b.Held("XEF.TO") != 0 {
		t.Fatalf("pending=%d XEF=%d, want everything filled and XEF sold", len(st.Pending), b.Held("XEF.TO"))
	}
	for _, s := range []string{"VFV.TO", "XIU.TO", "ZAG.TO"} {
		if b.Held(s) == 0 {
			t.Errorf("%s not bought in the same cycle", s)
		}
	}
	// The buys were sized with the sale's cash: little is left over.
	if b.Cash > 0.05*10000 {
		t.Errorf("cash left %.0f — buys not re-planned after the sell", b.Cash)
	}
	if evs, _ := d.J.ByKinds(ctx, desk.KindApprovalAccepted); len(evs) != 0 {
		t.Errorf("no human approval may be journaled, got %d", len(evs))
	}
	if evs, _ := d.J.ByKinds(ctx, desk.KindAutoExecuted); len(evs) != 4 {
		t.Errorf("auto-executed %d orders, want 4 (1 sell + 3 buys)", len(evs))
	}
	// Next cycle closes the month.
	now = now.Add(4 * time.Hour)
	if _, err := l.Cycle(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if done, _ := l.monthDone(ctx, "2026-11"); !done {
		t.Error("month not closed after everything filled")
	}
}
