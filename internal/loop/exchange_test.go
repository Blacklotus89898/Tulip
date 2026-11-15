package loop

import (
	"testing"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
)

// The monthly rebalance window ("first 5 trading days") counts the desk's own
// exchange (ADR 0006): 2026-07-01 is Canada Day (TSX closed, NYSE open) and
// 2026-07-03 is US Independence Day observed (NYSE closed, TSX open).
func TestTradingDaysSinceUsesTheDesksExchange(t *testing.T) {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, calendar.Toronto)
	day := time.Date(2026, 7, 7, 12, 0, 0, 0, calendar.Toronto)
	nyse, err1 := tradingDaysSince(calendar.NYSE, start, day)
	tsx, err2 := tradingDaysSince(calendar.TSX, start, day)
	none, err3 := tradingDaysSince(nil, start, day)
	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatal(err1, err2, err3)
	}
	// Jul 1-7: Wed 1, Thu 2, Fri 3, Mon 6, Tue 7 (weekend 4-5).
	if nyse != 4 || tsx != 4 || none != tsx {
		t.Errorf("trading days: NYSE %d TSX %d nil %d, want 4, 4 and nil == TSX", nyse, tsx, none)
	}
	for _, c := range []struct {
		x       *calendar.Exchange
		d       int
		counted bool
	}{{calendar.NYSE, 1, true}, {calendar.NYSE, 3, false}, {calendar.TSX, 1, false}, {calendar.TSX, 3, true}} {
		d := time.Date(2026, 7, c.d, 0, 0, 0, 0, calendar.Toronto)
		n, _ := tradingDaysSince(c.x, d, d)
		if (n == 1) != c.counted {
			t.Errorf("%s 2026-07-0%d counted=%v, want %v", c.x, c.d, n == 1, c.counted)
		}
	}
}
