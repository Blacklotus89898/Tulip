package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
)

// History fetches a symbol's full daily history, DIVIDEND-ADJUSTED (every
// OHLC scaled by Yahoo's adjclose/close), for backtests. Separate from the
// live path (Daily/Quote): no cache, its own range. Fails if adjusted closes
// are missing — raw closes would understate dividend-paying ETFs (ZAG, XIU)
// and silently skew any comparison.
func (y *Yahoo) History(ctx context.Context, symbol string) ([]Candle, error) {
	base := y.Base
	if base == "" {
		base = "https://query1.finance.yahoo.com"
	}
	// Explicit period1/period2: range=max silently downgrades long histories
	// to monthly bars (dataGranularity) — checked below too.
	u := fmt.Sprintf("%s/v8/finance/chart/%s?period1=0&period2=%d&interval=1d&events=div%%7Csplit",
		base, url.PathEscape(symbol), y.now().Unix())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Tulip paper-trading; +https://github.com/Blacklotus89898/Tulip)")
	cl := y.Client
	if cl == nil {
		cl = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s HTTP %d", ErrUnavailable, symbol, resp.StatusCode)
	}
	var body struct {
		Chart struct {
			Result []historyResult `json:"result"`
		} `json:"chart"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrUnavailable, err)
	}
	if len(body.Chart.Result) == 0 {
		return nil, fmt.Errorf("%w: %s: empty chart", ErrUnavailable, symbol)
	}
	if g := body.Chart.Result[0].Meta.DataGranularity; g != "" && g != "1d" {
		return nil, fmt.Errorf("%w: %s: got %s bars, want 1d", ErrUnavailable, symbol, g)
	}
	return adjustedBars(body.Chart.Result[0], y.now())
}

type historyResult struct {
	chartResult
	Meta struct {
		DataGranularity string `json:"dataGranularity"`
	} `json:"meta"`
	Indicators struct {
		Quote []struct {
			Open   []*float64 `json:"open"`
			High   []*float64 `json:"high"`
			Low    []*float64 `json:"low"`
			Close  []*float64 `json:"close"`
			Volume []*float64 `json:"volume"`
		} `json:"quote"`
		AdjClose []struct {
			AdjClose []*float64 `json:"adjclose"`
		} `json:"adjclose"`
	} `json:"indicators"`
}

func adjustedBars(res historyResult, now time.Time) ([]Candle, error) {
	if len(res.Indicators.Quote) == 0 {
		return nil, fmt.Errorf("%w: no quote series", ErrUnavailable)
	}
	if len(res.Indicators.AdjClose) == 0 || len(res.Indicators.AdjClose[0].AdjClose) == 0 {
		return nil, fmt.Errorf("%w: no adjusted closes (refusing raw prices for a backtest)", ErrUnavailable)
	}
	q, adj := res.Indicators.Quote[0], res.Indicators.AdjClose[0].AdjClose
	out := make([]Candle, 0, len(res.Timestamp))
	for i, ts := range res.Timestamp {
		if i >= len(q.Close) || i >= len(adj) || q.Close[i] == nil || q.Open[i] == nil ||
			q.High[i] == nil || q.Low[i] == nil || adj[i] == nil || *q.Close[i] <= 0 {
			continue
		}
		t := time.Unix(ts, 0).In(calendar.Toronto)
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, calendar.Toronto)
		if calendar.SameDay(day, now) && !calendar.SessionClosed(day, now) {
			continue
		}
		f := *adj[i] / *q.Close[i]
		var vol float64
		if i < len(q.Volume) && q.Volume[i] != nil {
			vol = *q.Volume[i]
		}
		out = append(out, Candle{Day: day, Open: *q.Open[i] * f, High: *q.High[i] * f, Low: *q.Low[i] * f, Close: *adj[i], Volume: vol})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no completed bars", ErrUnavailable)
	}
	return out, nil
}
