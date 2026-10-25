package loop

import (
	"context"
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
}
