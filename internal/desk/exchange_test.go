package desk

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/strategy"
)

// ADR 0006: each desk trades on its own exchange's calendar. A nil Cal is
// the TSX desk exactly as before.
func TestDeskUsesItsOwnExchangeCalendar(t *testing.T) {
	cases := []struct {
		name string
		cal  *calendar.Exchange
		day  time.Time
		sig  string
		open bool
	}{
		{"TSX desk on Canada Day", nil, time.Date(2026, 7, 1, 11, 0, 0, 0, calendar.Toronto), "2026-06-30", false},
		{"US desk on Canada Day", calendar.NYSE, time.Date(2026, 7, 1, 11, 0, 0, 0, calendar.Toronto), "2026-06-30", true},
		{"TSX desk on US Thanksgiving", nil, time.Date(2026, 11, 26, 11, 0, 0, 0, calendar.Toronto), "2026-11-25", true},
		{"US desk on US Thanksgiving", calendar.NYSE, time.Date(2026, 11, 26, 11, 0, 0, 0, calendar.Toronto), "2026-11-25", false},
		{"US desk after the NYSE early close", calendar.NYSE, time.Date(2026, 11, 27, 14, 0, 0, 0, calendar.Toronto), "2026-11-25", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			now := c.day
			h.d.Now = func() time.Time { return now }
			h.d.Cal = c.cal
			h.d.AutoPaper = true
			h.md.set("XIU.TO", 50, now.Add(-15*time.Minute))
			cand := &strategy.Candidate{Strategy: "trend-filter-200", Symbol: "XIU.TO", Side: strategy.Buy, Qty: 5, RefPrice: 50, SignalOn: c.sig}
			s, v, err := h.d.Propose(ctx, "c1", ProposeRequest{Rationale: testWhy, Symbol: "XIU.TO", Side: "buy", Qty: 5, Source: "rules", Candidate: cand})
			if err != nil || s == nil {
				t.Fatalf("propose: (%v, %+v, %v)", s, v, err)
			}
			fill, err := h.d.AutoExecute(ctx, s.ID)
			if c.open && (err != nil || fill == nil) {
				t.Fatalf("market open: want a fill, got (%v, %v)", fill, err)
			}
			if !c.open && !errors.Is(err, ErrMarketClosed) {
				t.Fatalf("market closed: want ErrMarketClosed, got (%v, %v)", fill, err)
			}
		})
	}
}
