package backtest

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/marketdata"
	"github.com/Blacklotus89898/Tulip/internal/strategy"
	"github.com/Blacklotus89898/Tulip/internal/thorn"
)

// series makes n weekday bars: a rise, then a fall (one entry, one exit).
func series(n int, f func(i int) float64) []marketdata.Candle {
	var out []marketdata.Candle
	d := time.Date(2020, 1, 1, 0, 0, 0, 0, calendar.Toronto)
	for i := 0; len(out) < n; i++ {
		day := d.AddDate(0, 0, i)
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		k := len(out)
		px := f(k)
		// a little noise so volatility is non-zero
		px *= 1 + 0.004*math.Sin(float64(k))
		out = append(out, marketdata.Candle{Day: day, Open: px * 0.999, High: px * 1.01, Low: px * 0.99, Close: px})
	}
	return out
}

func upThenDown(i int) float64 {
	if i < 200 {
		return 50 + 0.1*float64(i)
	}
	return 70 - 0.15*float64(i-200)
}

func cfg() Config {
	t := thorn.DefaultConfig()
	t.Allowlist = []string{"AAA.TO", "XIU.TO"}
	return Config{Thorn: t}
}

func TestRunTradesTheTrendAndFillsNextOpen(t *testing.T) {
	bars := map[string][]marketdata.Candle{"AAA.TO": series(320, upThenDown), "XIU.TO": series(320, func(int) float64 { return 50 })}
	res, err := Run(bars, []string{"AAA.TO"}, cfg())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RoundTrips) == 0 || res.Trades < 2 {
		t.Fatalf("expected an entry and an exit, got %d trades / %d round trips (rejected %v)", res.Trades, len(res.RoundTrips), res.Rejected)
	}
	first := res.TradeLog[0]
	if first.Day <= first.Signal {
		t.Errorf("fill %s must be AFTER its signal bar %s (no same-bar fills)", first.Day, first.Signal)
	}
	// Filled at that day's open + 10 bps, never at the signal close.
	for _, c := range bars["AAA.TO"] {
		if key(c.Day) == first.Day && math.Abs(first.Price-round4(c.Open*1.001)) > 1e-9 {
			t.Errorf("fill price %.4f, want next open+slippage %.4f", first.Price, c.Open*1.001)
		}
	}
	if res.Strategy.MaxDrawdown > 0 || res.BenchXIU.TotalReturn > 0.01 {
		t.Errorf("metrics look wrong: %+v / %+v", res.Strategy, res.BenchXIU)
	}
}

// Changing bars AFTER day t must not change any decision up to t.
func TestNoLookahead(t *testing.T) {
	a := series(320, upThenDown)
	b := series(320, upThenDown)
	cut := 260
	for i := cut + 1; i < len(b); i++ { // a wildly different future
		b[i].Open, b[i].High, b[i].Low, b[i].Close = 200, 210, 190, 205
	}
	flat := series(320, func(int) float64 { return 50 })
	ra, err := Run(map[string][]marketdata.Candle{"AAA.TO": a, "XIU.TO": flat}, []string{"AAA.TO"}, cfg())
	if err != nil {
		t.Fatal(err)
	}
	rb, err := Run(map[string][]marketdata.Candle{"AAA.TO": b, "XIU.TO": flat}, []string{"AAA.TO"}, cfg())
	if err != nil {
		t.Fatal(err)
	}
	cutDay := key(a[cut].Day)
	upTo := func(ts []Trade) []Trade {
		var out []Trade
		for _, x := range ts {
			if x.Signal <= cutDay {
				out = append(out, x)
			}
		}
		return out
	}
	if !reflect.DeepEqual(upTo(ra.TradeLog), upTo(rb.TradeLog)) {
		t.Errorf("decisions up to %s depend on later bars:\n%v\n%v", cutDay, upTo(ra.TradeLog), upTo(rb.TradeLog))
	}
}

func TestCommissionCostsShowUp(t *testing.T) {
	bars := map[string][]marketdata.Candle{"AAA.TO": series(320, upThenDown), "XIU.TO": series(320, func(int) float64 { return 50 })}
	free, _ := Run(bars, []string{"AAA.TO"}, cfg())
	c := cfg()
	c.Commission = 9.95
	paid, _ := Run(bars, []string{"AAA.TO"}, c)
	if !(paid.FinalEquity < free.FinalEquity) {
		t.Errorf("commission must cost: free %.2f vs paid %.2f", free.FinalEquity, paid.FinalEquity)
	}
}

func relaxed(syms ...string) Config {
	t := thorn.DefaultConfig()
	t.Allowlist, t.MaxOrderValue, t.MaxPositionWeight = syms, 1e9, 1
	return Config{Thorn: t, Warmup: 260}
}

func TestEqualWeightAllocatorTracksBuyAndHold(t *testing.T) {
	up := series(600, func(i int) float64 { return 50 + 0.02*float64(i) })
	bars := map[string][]marketdata.Candle{"AAA.TO": up, "XIU.TO": series(600, func(i int) float64 { return 40 + 0.01*float64(i) })}
	syms := []string{"AAA.TO", "XIU.TO"}
	res, err := RunAllocator(bars, syms, strategy.EqualWeight{Symbols: syms}, relaxed(syms...))
	if err != nil {
		t.Fatal(err)
	}
	if res.Trades == 0 || res.Exposure < 0.9 {
		t.Fatalf("equal weight should be ~fully invested: trades %d exposure %.2f (rejected %v)", res.Trades, res.Exposure, res.Rejected)
	}
	if d := math.Abs(res.Strategy.TotalReturn - res.BenchEqual.TotalReturn); d > 0.02 {
		t.Errorf("rebalanced equal weight %.4f should be close to equal buy-and-hold %.4f", res.Strategy.TotalReturn, res.BenchEqual.TotalReturn)
	}
}

func TestAllocatorNoLookahead(t *testing.T) {
	mk := func(future float64) map[string][]marketdata.Candle {
		a := series(600, upThenDown2)
		if future > 0 {
			for i := 450; i < len(a); i++ {
				a[i].Open, a[i].High, a[i].Low, a[i].Close = future, future, future, future
			}
		}
		return map[string][]marketdata.Candle{"AAA.TO": a, "ZAG.TO": series(600, func(int) float64 { return 20 }), "XIU.TO": series(600, func(int) float64 { return 50 })}
	}
	syms := []string{"AAA.TO", "ZAG.TO"}
	alloc := strategy.TrendFilter{Risk: []string{"AAA.TO"}, Safe: "ZAG.TO"}
	ra, _ := RunAllocator(mk(0), syms, alloc, relaxed(syms...))
	rb, _ := RunAllocator(mk(500), syms, alloc, relaxed(syms...))
	cut := key(series(600, upThenDown2)[449].Day)
	upTo := func(ts []Trade) []Trade {
		var out []Trade
		for _, x := range ts {
			if x.Signal < cut {
				out = append(out, x)
			}
		}
		return out
	}
	if len(upTo(ra.TradeLog)) == 0 || !reflect.DeepEqual(upTo(ra.TradeLog), upTo(rb.TradeLog)) {
		t.Errorf("allocator decisions before %s depend on later bars", cut)
	}
}

func upThenDown2(i int) float64 {
	if i < 380 {
		return 50 + 0.05*float64(i)
	}
	return 69 - 0.08*float64(i-380)
}
