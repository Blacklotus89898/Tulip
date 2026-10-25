package loop

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/marketdata"
	"github.com/Blacklotus89898/Tulip/internal/shadow"
)

// shadowCycle feeds the shadow books (ADR 0004) the same view of the market
// the live strategy had this cycle: bars completed before the month started,
// this cycle's quotes, the same rebalance window. It never fails the cycle.
func (l *Loop) shadowCycle(ctx context.Context, cycle string, hist HistorySource) {
	now := l.now().In(calendar.Toronto)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, calendar.Toronto)
	in := shadow.Input{Cycle: cycle, Day: now.Format("2006-01-02"), Month: now.Format("2006-01"), Now: now,
		Quotes: map[string]marketdata.Quote{}}
	n, err := tradingDaysSince(monthStart, now)
	in.InWindow = err == nil && n <= rebalanceWindow
	if in.InWindow {
		var res Result
		if bars, asOf, ok := l.completedBars(ctx, cycle, hist, monthStart, &res); ok {
			in.Bars, in.AsOf = bars, asOf
		}
	}
	for _, s := range l.Symbols {
		q, err := l.MD.Quote(ctx, s)
		if err != nil {
			_, _ = l.Desk.J.Append(ctx, shadow.KindError, cycle, map[string]string{"symbol": s, "error": err.Error()})
			return // no marks or fills on partial quotes
		}
		in.Quotes[s] = q
	}
	l.Shadow.Cycle(ctx, in)
	if in.InWindow {
		l.monthlyReport(ctx, cycle, in.Month)
	}
}

// KindScorecardSent closes a month's scorecard push (once per month).
const KindScorecardSent = "scorecard.sent"

// monthlyReport pushes the scorecard once per month, right after that
// month's shadow rebalance. A failed send is journaled and retried next cycle.
func (l *Loop) monthlyReport(ctx context.Context, cycle, month string) {
	if l.Report == nil {
		return
	}
	evs, err := l.Desk.J.ByKinds(ctx, shadow.KindRebalanced, KindScorecardSent)
	if err != nil {
		return
	}
	rebalanced := false
	for _, e := range evs {
		var m struct{ Month string }
		if json.Unmarshal(e.Payload, &m) != nil || m.Month != month {
			continue
		}
		if e.Kind == KindScorecardSent {
			return // already sent this month
		}
		rebalanced = true
	}
	if !rebalanced {
		return
	}
	card, err := shadow.Build(ctx, l.Desk.J)
	if err != nil {
		_, _ = l.Desk.J.Append(ctx, "scorecard.error", cycle, map[string]string{"month": month, "error": err.Error()})
		return
	}
	label := month
	if t, err := time.Parse("2006-01", month); err == nil {
		label = t.Format("January 2006")
	}
	title, body := card.Summary(label, l.StrategyName())
	if err := l.Report(ctx, title, body); err != nil {
		_, _ = l.Desk.J.Append(ctx, "scorecard.error", cycle, map[string]string{"month": month, "error": err.Error()})
		return
	}
	_, _ = l.Desk.J.Append(ctx, KindScorecardSent, cycle, map[string]string{"month": month, "title": title})
}
