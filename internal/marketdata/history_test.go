package marketdata

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestHistoryIsDividendAdjusted(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	raw := chartJSON([]string{"2026-10-21", "2026-10-22"}, []*float64{f(50), f(52)}, 52, now)
	// Yahoo's adjclose: the first bar predates a distribution (factor 0.98).
	adj := strings.Replace(raw, `"quote":[`, `"adjclose":[{"adjclose":[49,52]}],"quote":[`, 1)
	y := &Yahoo{Base: serve(t, 200, adj).URL, Now: func() time.Time { return now }}
	bars, err := y.History(context.Background(), "XIU.TO")
	if err != nil || len(bars) != 2 {
		t.Fatalf("history = (%v, %v)", bars, err)
	}
	if bars[0].Close != 49 || math.Abs(bars[0].Open-49) > 1e-9 || bars[1].Close != 52 {
		t.Errorf("OHLC must be scaled by adjclose/close: %+v", bars)
	}
	// No adjclose → refuse (raw closes would skew a dividend ETF backtest).
	y2 := &Yahoo{Base: serve(t, 200, raw).URL, Now: func() time.Time { return now }}
	if _, err := y2.History(context.Background(), "XIU.TO"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("missing adjclose must fail, got %v", err)
	}
}
