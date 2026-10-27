package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/desk"
	"github.com/Blacklotus89898/Tulip/internal/marketdata"
	"github.com/Blacklotus89898/Tulip/internal/strategy"
)

// HistorySource supplies dividend-adjusted daily history (the same basis the
// allocator was backtested on). *marketdata.Yahoo implements it.
type HistorySource interface {
	HistorySince(ctx context.Context, symbol string, since time.Time) ([]marketdata.Candle, error)
}

// Journal kinds of the allocator path.
const (
	KindTargets       = "allocator.targets"
	KindRebalanceDone = "rebalance.done"
)

// rebalanceWindow: trading days at the start of a month during which the
// loop keeps working towards the month's targets (approvals can be late).
const rebalanceWindow = 5

// Rebalance is one allocator cycle — the live twin of backtest.RunAllocator:
// targets from bars COMPLETED by the end of last month, orders from the
// shared strategy.Rebalance, sells proposed before buys (buys wait until no
// sell is pending, so they can use its cash), never a second suggestion for a
// symbol that already has one pending. Idempotent: safe at every cycle; the
// month is closed (rebalance.done) once nothing is left to do.
//
// In paper training (ADR 0005) the rules' suggestions fill at once; after
// sells fill, the cycle re-plans once so the buys are sized from the book
// the sells left behind.
func (l *Loop) rebalance(ctx context.Context, cycle string, hist HistorySource, res *Result) error {
	sold, err := l.rebalanceOnce(ctx, cycle, hist, res)
	if err != nil || !sold {
		return err
	}
	_, err = l.rebalanceOnce(ctx, cycle, hist, res)
	return err
}

// rebalanceOnce is one planning pass; sold reports that a sell auto-filled.
func (l *Loop) rebalanceOnce(ctx context.Context, cycle string, hist HistorySource, res *Result) (sold bool, err error) {
	j := l.Desk.J
	now := l.now().In(calendar.Toronto)
	month := now.Format("2006-01")
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, calendar.Toronto)

	if done, err := l.monthDone(ctx, month); err != nil || done {
		return false, err
	}
	n, err := tradingDaysSince(monthStart, now)
	if err != nil {
		return false, err
	}
	if n > rebalanceWindow {
		return false, nil // window passed; next month
	}

	p, err := l.plan(ctx, cycle, hist, monthStart, res)
	if err != nil || p == nil {
		return false, err
	}
	targets, asOf, sells, buys := p.Targets, p.AsOf, p.Sells, p.Buys
	_, _ = j.Append(ctx, KindTargets, cycle, map[string]any{"strategy": l.Allocator.Name(), "month": month,
		"as_of": asOf, "targets": targets, "reasons": p.Reasons, "equity": p.Equity})

	st, err := l.Desk.LoadState(ctx)
	if err != nil {
		return false, err
	}
	pending := map[string]string{}
	for _, s := range st.Pending {
		pending[s.Symbol] = s.Side
	}
	if len(sells) == 0 && len(buys) == 0 && len(pending) == 0 {
		_, err := j.Append(ctx, KindRebalanceDone, cycle, map[string]any{"month": month, "strategy": l.Allocator.Name(), "targets": targets})
		return false, err
	}

	propose := func(o strategy.Order) {
		if _, busy := pending[o.Symbol]; busy {
			return
		}
		side, qty := "buy", o.Qty
		if qty < 0 {
			side, qty = "sell", -qty
		}
		why := p.Justify(o, month, l.Desk.Broker.FeePerOrder)
		s, _, err := l.Desk.Propose(ctx, cycle, desk.ProposeRequest{Symbol: o.Symbol, Side: side, Qty: qty, Source: "rules", Rationale: why})
		switch {
		case err != nil:
			_, _ = j.Append(ctx, "propose.error", cycle, map[string]string{"symbol": o.Symbol, "error": err.Error()})
		case s == nil:
			res.Rejected++
		default:
			res.Suggestions = append(res.Suggestions, s.ID)
			pending[o.Symbol] = side
			if !l.Desk.AutoPaper {
				return
			}
			// Paper training (ADR 0005): fill now. A failed execution is
			// journaled (order.rejected) and closes the suggestion either way.
			if _, err := l.Desk.AutoExecute(ctx, s.ID); err != nil {
				_, _ = j.Append(ctx, "autoexecute.error", cycle, map[string]string{"id": s.ID, "symbol": o.Symbol, "error": err.Error()})
				return
			}
			delete(pending, o.Symbol)
			if side == "sell" {
				sold = true
			}
		}
	}
	for _, o := range sells {
		propose(o)
	}
	if sold {
		return true, nil // re-plan the buys from the book the sells left
	}
	for _, side := range pending {
		if side == "sell" {
			return false, nil // buys wait for the sells' cash
		}
	}
	for _, o := range buys {
		propose(o)
	}
	return false, nil
}

// Plan is what the allocator wants now and the orders that get there.
type Plan struct {
	Strategy string             `json:"strategy"`
	AsOf     string             `json:"signals_as_of"`
	Targets  map[string]float64 `json:"targets"`
	Reasons  map[string]string  `json:"reasons"` // why each symbol has its target (same pass as Targets)
	Weights  map[string]float64 `json:"current_weights"`
	Equity   float64            `json:"equity"`
	Sells    []strategy.Order   `json:"sells"`
	Buys     []strategy.Order   `json:"buys"`
	Prices   map[string]float64 `json:"prices"`
	AvgCost  map[string]float64 `json:"avg_cost,omitempty"` // 0/missing = unknown (co-pilot snapshots)
}

// Justify is the human-readable case for one order: the strategy's reason
// for the symbol, what the order does to the book, and — for a sell — the
// estimated realized gain (taxable: the operator trades a cash account).
func (p *Plan) Justify(o strategy.Order, month string, fee float64) string {
	side, qty := "Buy", o.Qty
	if qty < 0 {
		side, qty = "Sell", -qty
	}
	px := p.Prices[o.Symbol]
	reason := p.Reasons[o.Symbol]
	if reason == "" {
		reason = fmt.Sprintf("%s targets %.0f%% for %s.", p.Strategy, o.Target*100, o.Symbol)
	}
	lines := []string{
		"Why: " + reason,
		fmt.Sprintf("Order: %s %d %s at ~%.2f ≈ $%.0f — moves it from %.0f%% to its %.0f%% target of the $%.0f book.",
			side, qty, o.Symbol, px, float64(qty)*px, o.Now*100, o.Target*100, p.Equity),
	}
	if fee > 0 {
		lines = append(lines, fmt.Sprintf("Cost: ~$%.2f commission.", fee))
	}
	if o.Qty < 0 {
		if c := p.AvgCost[o.Symbol]; c > 0 {
			gain := float64(qty)*(px-c) - fee
			if gain >= 0 {
				lines = append(lines, fmt.Sprintf("Tax: realizes an estimated gain of $%.0f (avg cost %.2f) — a taxable capital gain in a cash account.", gain, c))
			} else {
				lines = append(lines, fmt.Sprintf("Tax: realizes an estimated loss of $%.0f (avg cost %.2f) — a capital loss in a cash account (can offset gains).", math.Abs(gain), c))
			}
		} else {
			lines = append(lines, "Tax: realizes a capital gain or loss; cost basis unknown here — check your account.")
		}
	}
	lines = append(lines, fmt.Sprintf("Rule: %s monthly rebalance for %s, on prices up to %s.", p.Strategy, month, p.AsOf))
	return strings.Join(lines, "\n")
}

// plan computes targets from bars completed BEFORE cutoff and the orders
// from the current book at live quotes. nil plan (no error) = data
// unavailable, already journaled (fail closed).
func (l *Loop) plan(ctx context.Context, cycle string, src HistorySource, cutoff time.Time, res *Result) (*Plan, error) {
	j := l.Desk.J
	hist, asOf, ok := l.completedBars(ctx, cycle, src, cutoff, res)
	if !ok {
		return nil, nil // fail closed: no partial rebalance on partial data
	}
	dec, err := strategy.Decide(l.Allocator, hist)
	targets := dec.Targets
	if err != nil {
		_, _ = j.Append(ctx, "strategy.skipped", cycle, map[string]string{"strategy": l.Allocator.Name(), "error": err.Error()})
		return nil, nil
	}
	book, _, err := l.Desk.SizingBook(ctx, cycle)
	if err != nil {
		res.Unavailable = append(res.Unavailable, "account")
		return nil, nil
	}
	px := map[string]float64{}
	held := map[string]int{}
	for _, s := range l.Symbols {
		q, err := l.MD.Quote(ctx, s)
		if err != nil {
			res.Unavailable = append(res.Unavailable, s)
			_, _ = j.Append(ctx, "data.unavailable", cycle, map[string]string{"symbol": s, "error": err.Error()})
			return nil, nil
		}
		px[s] = q.Price
		held[s] = book.Held(s)
	}
	equity := book.Cash // at live quotes, not at cost
	for s, q := range held {
		equity += float64(q) * px[s]
	}
	weights := map[string]float64{}
	for s, q := range held {
		if q > 0 && equity > 0 {
			weights[s] = float64(q) * px[s] / equity
		}
	}
	avgCost := map[string]float64{}
	for s := range held {
		if c := book.Positions[s].AvgCost; c > 0 {
			avgCost[s] = c
		}
	}
	sells, buys := strategy.Rebalance(l.Symbols, targets, held, px, equity, l.band(), 0.001)
	return &Plan{Strategy: l.Allocator.Name(), AsOf: asOf, Targets: targets, Reasons: dec.Reasons, Weights: weights,
		Equity: equity, Sells: sells, Buys: buys, Prices: px, AvgCost: avgCost}, nil
}

// completedBars fetches adjusted history for every symbol and keeps only the
// bars completed BEFORE cutoff. ok=false: some symbol is unavailable (journaled).
func (l *Loop) completedBars(ctx context.Context, cycle string, src HistorySource, cutoff time.Time, res *Result) (map[string][]marketdata.Candle, string, bool) {
	hist := map[string][]marketdata.Candle{}
	asOf := ""
	for _, s := range l.Symbols {
		bars, err := src.HistorySince(ctx, s, cutoff.AddDate(-2, 0, 0))
		if err != nil {
			res.Unavailable = append(res.Unavailable, s)
			_, _ = l.Desk.J.Append(ctx, "data.unavailable", cycle, map[string]string{"symbol": s, "error": err.Error()})
			return nil, "", false
		}
		k := len(bars)
		for k > 0 && !bars[k-1].Day.Before(cutoff) {
			k--
		}
		hist[s] = bars[:k]
		if k > 0 {
			asOf = bars[k-1].Day.Format("2006-01-02")
		}
	}
	return hist, asOf, true
}

// previewCutoff: bars up to today count once today's session has closed.
func (l *Loop) previewCutoff() time.Time {
	now := l.now().In(calendar.Toronto)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, calendar.Toronto)
	if calendar.SessionClosed(today, now) {
		return today.AddDate(0, 0, 1) // today's bar is complete
	}
	return today
}

// Preview is the plan from today's completed bars — read-only: nothing is
// proposed or journaled except data errors.
func (l *Loop) Preview(ctx context.Context) (*Plan, []string, error) {
	if l.Allocator == nil {
		return nil, nil, fmt.Errorf("no allocator configured (strategy %s)", l.StrategyName())
	}
	var res Result
	p, err := l.plan(ctx, "preview", l.Hist, l.previewCutoff(), &res)
	return p, res.Unavailable, err
}

// CandidateDecisions is what every pre-registered candidate would hold on
// today's completed bars, with its reasons — read-only (scorecard tool).
func (l *Loop) CandidateDecisions(ctx context.Context) (map[string]strategy.Decision, string, []string, error) {
	var res Result
	hist, asOf, ok := l.completedBars(ctx, "scorecard", l.Hist, l.previewCutoff(), &res)
	if !ok {
		return nil, "", res.Unavailable, nil
	}
	out := map[string]strategy.Decision{}
	for _, a := range strategy.Candidates(l.Symbols) {
		d, err := strategy.Decide(a, hist)
		if err != nil {
			return nil, asOf, nil, fmt.Errorf("%s: %w", a.Name(), err)
		}
		out[a.Name()] = d
	}
	return out, asOf, nil, nil
}

// memoHist shares one history fetch per symbol within a cycle between the
// live rebalance and the shadow books (Yahoo rate limits).
type memoHist struct {
	src HistorySource
	m   map[string][]marketdata.Candle
	err map[string]error
}

func newMemoHist(src HistorySource) *memoHist {
	return &memoHist{src: src, m: map[string][]marketdata.Candle{}, err: map[string]error{}}
}

func (h *memoHist) HistorySince(ctx context.Context, s string, since time.Time) ([]marketdata.Candle, error) {
	key := s + "|" + since.Format("2006-01-02")
	if b, ok := h.m[key]; ok {
		return b, nil
	}
	if err, ok := h.err[key]; ok {
		return nil, err
	}
	b, err := h.src.HistorySince(ctx, s, since)
	if err != nil {
		h.err[key] = err
		return nil, err
	}
	h.m[key] = b
	return b, nil
}

func (l *Loop) band() float64 {
	if l.Band > 0 {
		return l.Band
	}
	return 0.03
}

func (l *Loop) monthDone(ctx context.Context, month string) (bool, error) {
	evs, err := l.Desk.J.ByKinds(ctx, KindRebalanceDone)
	if err != nil {
		return false, err
	}
	for _, e := range evs {
		var m struct {
			Month string `json:"month"`
		}
		if json.Unmarshal(e.Payload, &m) == nil && m.Month == month {
			return true, nil
		}
	}
	return false, nil
}

// tradingDaysSince counts TSX trading days from start through day (inclusive).
func tradingDaysSince(start, day time.Time) (int, error) {
	n := 0
	for d := start; !d.After(day); d = d.AddDate(0, 0, 1) {
		ok, err := calendar.IsTradingDay(d)
		if err != nil {
			return 0, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// StrategyName is what the service reports.
func (l *Loop) StrategyName() string {
	if l.Allocator != nil {
		return l.Allocator.Name()
	}
	return strings.TrimSpace(l.Strategy.Name())
}
