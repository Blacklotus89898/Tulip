package strategy

import (
	"math"
	"testing"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/marketdata"
)

// series builds n daily bars whose close follows fn(i).
func series(n int, fn func(i int) float64) []marketdata.Candle {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	out := make([]marketdata.Candle, n)
	for i := range out {
		c := fn(i)
		out[i] = marketdata.Candle{Day: start.AddDate(0, 0, i), Open: c, High: c, Low: c, Close: c}
	}
	return out
}

// A steady uptrend with a little noise (so volatility isn't zero).
func uptrend(i int) float64   { return 50 + 0.1*float64(i) + 0.3*math.Sin(float64(i)) }
func downtrend(i int) float64 { return 70 - 0.1*float64(i) + 0.3*math.Sin(float64(i)) }

func TestMomentumEntersAnUptrendWhenFlat(t *testing.T) {
	c, err := Momentum{}.Evaluate("XIU.TO", series(80, uptrend), 0, 10000)
	if err != nil || c == nil {
		t.Fatalf("expected a buy candidate, got (%+v, %v)", c, err)
	}
	if c.Side != Buy || c.Qty < 1 || c.Strategy != "momentum-v1" || c.SignalOn != "2026-03-22" {
		t.Errorf("candidate = %+v", c)
	}
	// Size never exceeds MaxWeight (20%) of equity.
	if float64(c.Qty)*c.RefPrice > 0.20*10000+c.RefPrice {
		t.Errorf("position %d × %.2f exceeds the 20%% cap", c.Qty, c.RefPrice)
	}
}

func TestMomentumExitsWhenTheTrendBreaks(t *testing.T) {
	c, _ := Momentum{}.Evaluate("XIU.TO", series(80, downtrend), 12, 10000)
	if c == nil || c.Side != Sell || c.Qty != 12 {
		t.Fatalf("held 12 in a downtrend: expected sell 12, got %+v", c)
	}
}

func TestMomentumStaysPut(t *testing.T) {
	if c, _ := (Momentum{}).Evaluate("XIU.TO", series(80, downtrend), 0, 10000); c != nil {
		t.Errorf("flat in a downtrend: no candidate, got %+v", c)
	}
	if c, _ := (Momentum{}).Evaluate("XIU.TO", series(80, uptrend), 5, 10000); c != nil {
		t.Errorf("already held in an uptrend: no candidate, got %+v", c)
	}
}

func TestMomentumNeedsHistory(t *testing.T) {
	if _, err := (Momentum{}).Evaluate("XIU.TO", series(30, uptrend), 0, 10000); err == nil {
		t.Error("30 bars < 50-day mean: expected an error")
	}
}

// Determinism: same input, same output — the backtest guarantee.
func TestMomentumIsPure(t *testing.T) {
	bars := series(80, uptrend)
	a, _ := Momentum{}.Evaluate("XIU.TO", bars, 0, 10000)
	b, _ := Momentum{}.Evaluate("XIU.TO", bars, 0, 10000)
	if a.Qty != b.Qty || a.Reason != b.Reason {
		t.Error("Evaluate must be deterministic")
	}
}
