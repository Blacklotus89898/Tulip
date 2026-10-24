// Package marketdata fetches daily candles and quotes behind an interface
// (ADR 0001 §6). v1 uses Yahoo's chart endpoint — unofficial, ~15 min delayed
// for TSX — so every read fails CLOSED: errors, empty or stale data become
// ErrUnavailable and the loop proposes nothing (ADR 0002 §2).
package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
)

// ErrUnavailable means no trustworthy data right now.
var ErrUnavailable = errors.New("market data unavailable")

// Candle is one completed daily bar.
type Candle struct {
	Day                    time.Time `json:"day"` // session date (Toronto midnight)
	Open, High, Low, Close float64
	Volume                 float64
}

// Quote is the latest observable price and when it was observed.
type Quote struct {
	Symbol   string    `json:"symbol"`
	Price    float64   `json:"price"`
	Time     time.Time `json:"time"`
	Currency string    `json:"currency"`
}

// Source is what the loop and the executor depend on.
type Source interface {
	// Daily returns COMPLETED daily bars, oldest first: a bar for today is
	// dropped unless today's session has closed (no lookahead).
	Daily(ctx context.Context, symbol string) ([]Candle, error)
	Quote(ctx context.Context, symbol string) (Quote, error)
}

// Yahoo implements Source with a small TTL cache.
type Yahoo struct {
	Base   string // default https://query1.finance.yahoo.com
	Client *http.Client
	Now    func() time.Time
	TTL    time.Duration // candle cache TTL (default 30m)

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	at     time.Time
	result chartResult
}

type chartResult struct {
	Meta struct {
		Currency           string  `json:"currency"`
		RegularMarketPrice float64 `json:"regularMarketPrice"`
		RegularMarketTime  int64   `json:"regularMarketTime"`
	} `json:"meta"`
	Timestamp  []int64 `json:"timestamp"`
	Indicators struct {
		Quote []struct {
			Open   []*float64 `json:"open"`
			High   []*float64 `json:"high"`
			Low    []*float64 `json:"low"`
			Close  []*float64 `json:"close"`
			Volume []*float64 `json:"volume"`
		} `json:"quote"`
	} `json:"indicators"`
}

func (y *Yahoo) now() time.Time {
	if y.Now != nil {
		return y.Now()
	}
	return time.Now()
}

func (y *Yahoo) chart(ctx context.Context, symbol string, fresh bool) (chartResult, error) {
	ttl := y.TTL
	if ttl == 0 {
		ttl = 30 * time.Minute
	}
	y.mu.Lock()
	if c, ok := y.cache[symbol]; ok && !fresh && y.now().Sub(c.at) < ttl {
		y.mu.Unlock()
		return c.result, nil
	}
	y.mu.Unlock()

	base := y.Base
	if base == "" {
		base = "https://query1.finance.yahoo.com"
	}
	u := fmt.Sprintf("%s/v8/finance/chart/%s?range=1y&interval=1d", base, url.PathEscape(symbol))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return chartResult{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Tulip paper-trading; +https://github.com/Blacklotus89898/Tulip)")
	cl := y.Client
	if cl == nil {
		cl = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return chartResult{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return chartResult{}, fmt.Errorf("%w: %s HTTP %d", ErrUnavailable, symbol, resp.StatusCode)
	}
	var body struct {
		Chart struct {
			Result []chartResult `json:"result"`
			Error  any           `json:"error"`
		} `json:"chart"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return chartResult{}, fmt.Errorf("%w: decode: %v", ErrUnavailable, err)
	}
	if len(body.Chart.Result) == 0 || len(body.Chart.Result[0].Timestamp) == 0 {
		return chartResult{}, fmt.Errorf("%w: %s: empty chart", ErrUnavailable, symbol)
	}
	res := body.Chart.Result[0]
	y.mu.Lock()
	if y.cache == nil {
		y.cache = map[string]cached{}
	}
	y.cache[symbol] = cached{at: y.now(), result: res}
	y.mu.Unlock()
	return res, nil
}

// Daily implements Source.
func (y *Yahoo) Daily(ctx context.Context, symbol string) ([]Candle, error) {
	res, err := y.chart(ctx, symbol, false)
	if err != nil {
		return nil, err
	}
	return completedBars(res, y.now())
}

// completedBars converts a chart to candles, skipping null rows and today's
// still-forming bar.
func completedBars(res chartResult, now time.Time) ([]Candle, error) {
	if len(res.Indicators.Quote) == 0 {
		return nil, fmt.Errorf("%w: no quote series", ErrUnavailable)
	}
	q := res.Indicators.Quote[0]
	out := make([]Candle, 0, len(res.Timestamp))
	for i, ts := range res.Timestamp {
		if i >= len(q.Close) || q.Close[i] == nil || q.Open[i] == nil || q.High[i] == nil || q.Low[i] == nil {
			continue
		}
		t := time.Unix(ts, 0).In(calendar.Toronto)
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, calendar.Toronto)
		if calendar.SameDay(day, now) && !calendar.SessionClosed(day, now) {
			continue // today's bar is still forming: using it would be lookahead
		}
		var vol float64
		if i < len(q.Volume) && q.Volume[i] != nil {
			vol = *q.Volume[i]
		}
		out = append(out, Candle{Day: day, Open: *q.Open[i], High: *q.High[i], Low: *q.Low[i], Close: *q.Close[i], Volume: vol})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no completed bars", ErrUnavailable)
	}
	return out, nil
}

// Quote implements Source; always fetched fresh (it's what fills use).
func (y *Yahoo) Quote(ctx context.Context, symbol string) (Quote, error) {
	res, err := y.chart(ctx, symbol, true)
	if err != nil {
		return Quote{}, err
	}
	if res.Meta.RegularMarketPrice <= 0 || res.Meta.RegularMarketTime == 0 {
		return Quote{}, fmt.Errorf("%w: %s: no quote", ErrUnavailable, symbol)
	}
	return Quote{
		Symbol: symbol, Price: res.Meta.RegularMarketPrice,
		Time: time.Unix(res.Meta.RegularMarketTime, 0), Currency: res.Meta.Currency,
	}, nil
}
