package strategy

import (
	"fmt"
	"math"

	"github.com/Blacklotus89898/Tulip/internal/marketdata"
)

// Allocator is a portfolio-level strategy: from COMPLETED bars (each slice
// ends at the decision day) it returns target weights summing to ≤ 1; the
// rest is cash. Pure — no clock, no I/O — so backtests and the live loop run
// the same code. Rebalancing cadence and order generation live outside.
type Allocator interface {
	Name() string
	Targets(bars map[string][]marketdata.Candle) (map[string]float64, error)
}

// EqualWeight holds every symbol at 1/n — the baseline a timing strategy must
// beat after costs.
type EqualWeight struct{ Symbols []string }

func (EqualWeight) Name() string { return "equal-weight" }

func (e EqualWeight) Targets(map[string][]marketdata.Candle) (map[string]float64, error) {
	out := map[string]float64{}
	for _, s := range e.Symbols {
		out[s] = 1 / float64(len(e.Symbols))
	}
	return out, nil
}

// TrendFilter gives each risk symbol 1/n while its close is above its N-day
// mean; a symbol below its mean hands its share to the Safe symbol.
type TrendFilter struct {
	Risk []string
	Safe string
	Days int // default 200
}

func (TrendFilter) Name() string { return "trend-filter-200" }

func (t TrendFilter) Targets(bars map[string][]marketdata.Candle) (map[string]float64, error) {
	days := t.Days
	if days == 0 {
		days = 200
	}
	out := map[string]float64{}
	w := 1 / float64(len(t.Risk))
	for _, s := range t.Risk {
		b := bars[s]
		if len(b) < days {
			return nil, fmt.Errorf("%s: need %d bars", s, days)
		}
		if b[len(b)-1].Close > meanClose(b[len(b)-days:]) {
			out[s] += w
		} else {
			out[t.Safe] += w
		}
	}
	return out, nil
}

// DualMomentum holds 100% of the risk symbol with the best lookback return if
// that return beats the safe symbol's; otherwise 100% safe.
type DualMomentum struct {
	Risk     []string
	Safe     string
	Lookback int // trading days, default 252 (~12 months)
}

func (DualMomentum) Name() string { return "dual-momentum-12m" }

func (d DualMomentum) Targets(bars map[string][]marketdata.Candle) (map[string]float64, error) {
	lb := d.Lookback
	if lb == 0 {
		lb = 252
	}
	ret := func(s string) (float64, error) {
		b := bars[s]
		if len(b) <= lb {
			return 0, fmt.Errorf("%s: need %d bars", s, lb+1)
		}
		return b[len(b)-1].Close/b[len(b)-1-lb].Close - 1, nil
	}
	best, bestRet := "", math.Inf(-1)
	for _, s := range d.Risk {
		r, err := ret(s)
		if err != nil {
			return nil, err
		}
		if r > bestRet {
			best, bestRet = s, r
		}
	}
	safeRet, err := ret(d.Safe)
	if err != nil {
		return nil, err
	}
	if bestRet > safeRet {
		return map[string]float64{best: 1}, nil
	}
	return map[string]float64{d.Safe: 1}, nil
}

// InverseVol weights symbols by 1/volatility (Days-day daily returns).
type InverseVol struct {
	Symbols []string
	Days    int // default 60
}

func (InverseVol) Name() string { return "inverse-vol-60" }

func (v InverseVol) Targets(bars map[string][]marketdata.Candle) (map[string]float64, error) {
	days := v.Days
	if days == 0 {
		days = 60
	}
	inv := map[string]float64{}
	total := 0.0
	for _, s := range v.Symbols {
		b := bars[s]
		if len(b) <= days {
			return nil, fmt.Errorf("%s: need %d bars", s, days+1)
		}
		closes := make([]float64, 0, days+1)
		for _, c := range b[len(b)-1-days:] {
			closes = append(closes, c.Close)
		}
		vol := dailyVol(closes)
		if vol <= 0 {
			return nil, fmt.Errorf("%s: zero volatility", s)
		}
		inv[s] = 1 / vol
		total += 1 / vol
	}
	for s := range inv {
		inv[s] /= total
	}
	return inv, nil
}

func meanClose(b []marketdata.Candle) float64 {
	s := 0.0
	for _, c := range b {
		s += c.Close
	}
	return s / float64(len(b))
}
