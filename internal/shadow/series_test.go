package shadow

import (
	"context"
	"testing"
)

func TestSeriesMatchesTheScorecard(t *testing.T) {
	ctx := context.Background()
	r, j := runner(t)
	if since, s, err := Series(ctx, j); err != nil || since != "" || len(s) != 0 {
		t.Fatalf("empty journal: %q %v %v", since, s, err)
	}
	r.Cycle(ctx, Input{Cycle: "c1", Day: "2026-11-02", Month: "2026-11", InWindow: true, Bars: bars(), Quotes: quotes(1)})
	r.Cycle(ctx, Input{Cycle: "c2", Day: "2026-11-03", Month: "2026-11", InWindow: true, Bars: bars(), Quotes: quotes(0.9)})
	r.Cycle(ctx, Input{Cycle: "c3", Day: "2026-11-04", Month: "2026-11", InWindow: true, Bars: bars(), Quotes: quotes(1.05)})
	since, series, err := Series(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	card, _ := Build(ctx, j)
	if since != card.Since || len(series) != len(card.Rows) {
		t.Fatalf("series since %s × %d books; card since %s × %d", since, len(series), card.Since, len(card.Rows))
	}
	if series[0].Role != "live" || series[1].Role != "benchmark" {
		t.Errorf("live and benchmark come first: %s, %s", series[0].Role, series[1].Role)
	}
	for i, s := range series {
		row := card.Rows[i]
		if s.Book != row.Book || len(s.Points) != row.Marks {
			t.Errorf("%s: %d points, card row %s has %d marks", s.Book, len(s.Points), row.Book, row.Marks)
		}
		if n := len(s.Points); n > 0 && s.Points[n-1].Equity != row.Equity {
			t.Errorf("%s: last point %.2f, card equity %.2f", s.Book, s.Points[n-1].Equity, row.Equity)
		}
	}
}
