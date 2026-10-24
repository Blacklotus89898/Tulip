package loop

import (
	"context"
	"encoding/json"
	"fmt"
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
func (l *Loop) rebalance(ctx context.Context, cycle string, res *Result) error {
	j := l.Desk.J
	now := l.now().In(calendar.Toronto)
	month := now.Format("2006-01")
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, calendar.Toronto)

	if done, err := l.monthDone(ctx, month); err != nil || done {
		return err
	}
	n, err := tradingDaysSince(monthStart, now)
	if err != nil {
		return err
	}
	if n > rebalanceWindow {
		return nil // window passed; next month
	}

	p, err := l.plan(ctx, cycle, monthStart, res)
	if err != nil || p == nil {
		return err
	}
	targets, asOf, sells, buys := p.Targets, p.AsOf, p.Sells, p.Buys
	_, _ = j.Append(ctx, KindTargets, cycle, map[string]any{"strategy": l.Allocator.Name(), "month": month,
		"as_of": asOf, "targets": targets, "equity": p.Equity})

	st, err := l.Desk.LoadState(ctx)
	if err != nil {
		return err
	}
	pending := map[string]string{}
	for _, s := range st.Pending {
		pending[s.Symbol] = s.Side
	}
	if len(sells) == 0 && len(buys) == 0 && len(pending) == 0 {
		_, err := j.Append(ctx, KindRebalanceDone, cycle, map[string]any{"month": month, "strategy": l.Allocator.Name(), "targets": targets})
		return err
	}

	propose := func(o strategy.Order) {
		if _, busy := pending[o.Symbol]; busy {
			return
		}
		side, qty := "buy", o.Qty
		if qty < 0 {
			side, qty = "sell", -qty
		}
		why := fmt.Sprintf("%s rebalance %s: target %.0f%% (now %.0f%%), signals as of %s",
			l.Allocator.Name(), month, o.Target*100, o.Now*100, asOf)
		s, _, err := l.Desk.Propose(ctx, cycle, desk.ProposeRequest{Symbol: o.Symbol, Side: side, Qty: qty, Source: "rules", Rationale: why})
		switch {
		case err != nil:
			_, _ = j.Append(ctx, "propose.error", cycle, map[string]string{"symbol": o.Symbol, "error": err.Error()})
		case s == nil:
			res.Rejected++
		default:
			res.Suggestions = append(res.Suggestions, s.ID)
			pending[o.Symbol] = side
		}
	}
	for _, o := range sells {
		propose(o)
	}
	for _, side := range pending {
		if side == "sell" {
			return nil // buys wait for the sells' cash
		}
	}
	for _, o := range buys {
		propose(o)
	}
	return nil
}

// Plan is what the allocator wants now and the orders that get there.
type Plan struct {
	Strategy string             `json:"strategy"`
	AsOf     string             `json:"signals_as_of"`
	Targets  map[string]float64 `json:"targets"`
	Weights  map[string]float64 `json:"current_weights"`
	Equity   float64            `json:"equity"`
	Sells    []strategy.Order   `json:"sells"`
	Buys     []strategy.Order   `json:"buys"`
}

// plan computes targets from bars completed BEFORE cutoff and the orders
// from the current book at live quotes. nil plan (no error) = data
// unavailable, already journaled (fail closed).
func (l *Loop) plan(ctx context.Context, cycle string, cutoff time.Time, res *Result) (*Plan, error) {
	j := l.Desk.J
	hist := map[string][]marketdata.Candle{}
	asOf := ""
	for _, s := range l.Symbols {
		bars, err := l.Hist.HistorySince(ctx, s, cutoff.AddDate(-2, 0, 0))
		if err != nil {
			res.Unavailable = append(res.Unavailable, s)
			_, _ = j.Append(ctx, "data.unavailable", cycle, map[string]string{"symbol": s, "error": err.Error()})
			return nil, nil // fail closed: no partial rebalance on partial data
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
	targets, err := l.Allocator.Targets(hist)
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
	sells, buys := strategy.Rebalance(l.Symbols, targets, held, px, equity, l.band(), 0.001)
	return &Plan{Strategy: l.Allocator.Name(), AsOf: asOf, Targets: targets, Weights: weights, Equity: equity, Sells: sells, Buys: buys}, nil
}

// Preview is the plan from today's completed bars — read-only: nothing is
// proposed or journaled except data errors.
func (l *Loop) Preview(ctx context.Context) (*Plan, []string, error) {
	if l.Allocator == nil {
		return nil, nil, fmt.Errorf("no allocator configured (strategy %s)", l.StrategyName())
	}
	now := l.now().In(calendar.Toronto)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, calendar.Toronto)
	cutoff := today
	if calendar.SessionClosed(today, now) {
		cutoff = today.AddDate(0, 0, 1) // today's bar is complete
	}
	var res Result
	p, err := l.plan(ctx, "preview", cutoff, &res)
	return p, res.Unavailable, err
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
