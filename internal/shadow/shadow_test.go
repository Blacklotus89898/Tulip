package shadow

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/desk"
	"github.com/Blacklotus89898/Tulip/internal/journal"
	"github.com/Blacklotus89898/Tulip/internal/marketdata"
	"github.com/Blacklotus89898/Tulip/internal/portfolio"
	"github.com/Blacklotus89898/Tulip/internal/strategy"
	"github.com/Blacklotus89898/Tulip/internal/thorn"
)

var syms = []string{"VFV.TO", "XEF.TO", "XIU.TO", "ZAG.TO"}

func bars() map[string][]marketdata.Candle {
	out := map[string][]marketdata.Candle{}
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, s := range syms {
		for i := 0; i < 300; i++ {
			x := float64(i)
			c := map[string]float64{"VFV.TO": 80 + 0.1*x, "XIU.TO": 30 + 0.05*x, "XEF.TO": 45 - 0.05*x, "ZAG.TO": 15 + 0.001*x}[s]
			out[s] = append(out[s], marketdata.Candle{Day: start.AddDate(0, 0, i), Open: c, High: c, Low: c, Close: c})
		}
	}
	return out
}

func quotes(scale float64) map[string]marketdata.Quote {
	q := map[string]marketdata.Quote{}
	for s, p := range map[string]float64{"VFV.TO": 110, "XEF.TO": 30, "XIU.TO": 45, "ZAG.TO": 15} {
		q[s] = marketdata.Quote{Symbol: s, Price: p * scale, Time: time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)}
	}
	return q
}

func runner(t *testing.T) (*Runner, *journal.Journal) {
	t.Helper()
	j, err := journal.Open(context.Background(), filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	th := thorn.DefaultConfig()
	th.Allowlist, th.MaxOrderValue, th.MaxPositionWeight, th.MaxSuggestionsPerDay = syms, 1e5, 0.4, 8
	th.MaxWeightBySymbol = map[string]float64{"ZAG.TO": 1}
	return &Runner{J: j, Allocators: strategy.Candidates(syms), Symbols: syms, Thorn: th}, j
}

func count(t *testing.T, j *journal.Journal, kind string) int {
	t.Helper()
	evs, err := j.ByKinds(context.Background(), kind)
	if err != nil {
		t.Fatal(err)
	}
	return len(evs)
}

func TestShadowBooksRebalanceMonthlyAndMarkDaily(t *testing.T) {
	ctx := context.Background()
	r, j := runner(t)
	in := Input{Cycle: "c1", Day: "2026-11-02", Month: "2026-11", InWindow: true, AsOf: "2026-10-30", Bars: bars(), Quotes: quotes(1)}
	r.Cycle(ctx, in)
	n := len(r.Allocators)
	if got := count(t, j, KindFunded); got != n {
		t.Errorf("funded %d books, want %d", got, n)
	}
	if got := count(t, j, KindRebalanced); got != n {
		t.Errorf("rebalanced %d, want %d", got, n)
	}
	if count(t, j, KindFill) == 0 || count(t, j, KindMark) != n {
		t.Errorf("fills %d marks %d", count(t, j, KindFill), count(t, j, KindMark))
	}
	// Same day again (the 15:00 cycle): nothing new.
	in.Cycle = "c2"
	r.Cycle(ctx, in)
	if count(t, j, KindRebalanced) != n || count(t, j, KindMark) != n || count(t, j, KindFunded) != n {
		t.Error("a second cycle the same day must not rebalance, mark or fund again")
	}
	// Next day, still in the window: a mark, no second rebalance this month.
	in.Day, in.Cycle = "2026-11-03", "c3"
	r.Cycle(ctx, in)
	if count(t, j, KindRebalanced) != n || count(t, j, KindMark) != 2*n {
		t.Errorf("day 2: rebalanced %d (want %d), marks %d (want %d)", count(t, j, KindRebalanced), n, count(t, j, KindMark), 2*n)
	}
	// The real ledger is untouched: no order.filled, no suggestions.
	if count(t, j, portfolio.KindFill) != 0 || count(t, j, desk.KindSuggestionCreated) != 0 {
		t.Error("shadow books must never write the live ledger")
	}
}

func TestShadowOrdersPassTheLiveLimits(t *testing.T) {
	ctx := context.Background()
	r, j := runner(t)
	// Position caps are relaxed for shadows: dual momentum may hold 100%...
	r.Allocators = []strategy.Allocator{strategy.DualMomentum{Risk: syms[:3], Safe: "ZAG.TO", Lookback: 200}}
	r.Cycle(ctx, Input{Cycle: "c1", Day: "2026-11-02", Month: "2026-11", InWindow: true, Bars: bars(), Quotes: quotes(1)})
	if count(t, j, KindRejected) != 0 || count(t, j, KindFill) != 1 {
		t.Errorf("dual momentum: rejected %d fills %d — want one 100%% buy", count(t, j, KindRejected), count(t, j, KindFill))
	}
	// ...but every other live rule stands: an off-allowlist symbol is refused.
	r, j = runner(t)
	r.Thorn.Allowlist = []string{"XEF.TO", "XIU.TO", "ZAG.TO"} // VFV not allowed
	r.Allocators = []strategy.Allocator{strategy.EqualWeight{Symbols: syms}}
	r.Cycle(ctx, Input{Cycle: "c1", Day: "2026-11-02", Month: "2026-11", InWindow: true, Bars: bars(), Quotes: quotes(1)})
	if count(t, j, KindRejected) != 1 {
		t.Errorf("off-allowlist VFV must be rejected, got %d rejections", count(t, j, KindRejected))
	}
	evs, _ := j.ByKinds(ctx, KindRebalanced)
	var m struct{ Rejected int }
	_ = json.Unmarshal(evs[0].Payload, &m)
	if m.Rejected == 0 {
		t.Error("rebalanced event should count the rejection")
	}
}

func TestOutsideTheWindowOnlyMarks(t *testing.T) {
	ctx := context.Background()
	r, j := runner(t)
	r.Cycle(ctx, Input{Cycle: "c1", Day: "2026-11-12", Month: "2026-11", InWindow: false, Quotes: quotes(1)})
	if count(t, j, KindRebalanced) != 0 || count(t, j, KindFill) != 0 || count(t, j, KindMark) != len(r.Allocators) {
		t.Error("outside the window: fund and mark only")
	}
}

func TestScorecardFromTheCommonStart(t *testing.T) {
	ctx := context.Background()
	r, j := runner(t)
	card, err := Build(ctx, j)
	if err != nil || card.Since != "" || card.Note == "" {
		t.Fatalf("empty journal: %+v %v", card, err)
	}
	r.Cycle(ctx, Input{Cycle: "c1", Day: "2026-11-02", Month: "2026-11", InWindow: true, Bars: bars(), Quotes: quotes(1)})
	r.Cycle(ctx, Input{Cycle: "c2", Day: "2026-11-03", Month: "2026-11", InWindow: true, Bars: bars(), Quotes: quotes(0.9)})
	r.Cycle(ctx, Input{Cycle: "c3", Day: "2026-11-04", Month: "2026-11", InWindow: true, Bars: bars(), Quotes: quotes(1.05)})
	card, err = Build(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	if card.Since == "" || len(card.Rows) != 2+len(r.Allocators) {
		t.Fatalf("card = %+v", card)
	}
	for _, row := range card.Rows {
		if row.Role != "candidate" {
			continue
		}
		if row.Marks != 3 || row.Trades == 0 || row.Targets == nil {
			t.Errorf("%s: %+v", row.Book, row)
		}
		// Fully invested books fall with a 10% drop and recover past the start.
		if row.MaxDDPct >= -5 || row.ReturnPct <= 0 {
			t.Errorf("%s: return %.2f%% drawdown %.2f%% — expected a dip then a gain", row.Book, row.ReturnPct, row.MaxDDPct)
		}
	}
}

func TestLiveMarkKindIsTheDesks(t *testing.T) {
	if LiveMarkKind != desk.KindMark {
		t.Fatalf("LiveMarkKind %q != desk.KindMark %q", LiveMarkKind, desk.KindMark)
	}
}
