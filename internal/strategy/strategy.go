// Package strategy holds Tulip's baseline strategies (ADR 0002 §1): pure
// functions of completed daily candles — no network, no clock, no randomness —
// so the same code runs live, in backtests and in Petal pipelines, and is
// unit-tested on fixture candles.
package strategy

import (
	"fmt"
	"math"

	"github.com/Blacklotus89898/Tulip/internal/marketdata"
)

// Side of a candidate order.
type Side string

const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

// Candidate is a rule-generated trade idea — input to the brain and to Thorn,
// never an order by itself.
type Candidate struct {
	Strategy string             `json:"strategy"`
	Symbol   string             `json:"symbol"`
	Side     Side               `json:"side"`
	Qty      int                `json:"qty"`
	RefPrice float64            `json:"ref_price"` // close of the signal bar
	SignalOn string             `json:"signal_on"` // date of the signal bar
	Reason   string             `json:"reason"`
	Signals  map[string]float64 `json:"signals"`
}

// Momentum is trend-following: hold while the close is above its N-day mean
// and the M-day return is positive; size inversely to volatility.
type Momentum struct {
	MeanDays   int     // default 50
	ReturnDays int     // default 20
	VolDays    int     // default 20
	TargetVol  float64 // annualised vol a full position should carry, default 0.10
	MaxWeight  float64 // cap per position as a fraction of equity, default 0.20
}

func (m Momentum) withDefaults() Momentum {
	if m.MeanDays == 0 {
		m.MeanDays = 50
	}
	if m.ReturnDays == 0 {
		m.ReturnDays = 20
	}
	if m.VolDays == 0 {
		m.VolDays = 20
	}
	if m.TargetVol == 0 {
		m.TargetVol = 0.10
	}
	if m.MaxWeight == 0 {
		m.MaxWeight = 0.20
	}
	return m
}

// Name of the strategy (journaled with every candidate).
func (Momentum) Name() string { return "momentum-v1" }

// Evaluate returns at most one candidate for symbol: an entry when flat and
// the trend is up, an exit when held and the trend broke. held is the current
// whole-share position, equity the portfolio's total value.
func (m Momentum) Evaluate(symbol string, bars []marketdata.Candle, held int, equity float64) (*Candidate, error) {
	m = m.withDefaults()
	need := max(m.MeanDays, m.ReturnDays+1, m.VolDays+1)
	if len(bars) < need {
		return nil, fmt.Errorf("momentum: need %d completed bars, have %d", need, len(bars))
	}
	last := bars[len(bars)-1]
	closes := make([]float64, len(bars))
	for i, b := range bars {
		closes[i] = b.Close
	}
	mean := avg(closes[len(closes)-m.MeanDays:])
	ret := last.Close/closes[len(closes)-1-m.ReturnDays] - 1
	vol := dailyVol(closes[len(closes)-1-m.VolDays:]) * math.Sqrt(252)
	signals := map[string]float64{
		"close": last.Close, "mean": round(mean), "return": round(ret), "ann_vol": round(vol),
	}
	up := last.Close > mean && ret > 0

	switch {
	case held == 0 && up:
		if vol <= 0 || equity <= 0 || last.Close <= 0 {
			return nil, nil
		}
		weight := math.Min(m.MaxWeight, m.TargetVol/vol)
		qty := int(math.Floor(equity * weight / last.Close))
		if qty < 1 {
			return nil, nil
		}
		signals["weight"] = round(weight)
		return &Candidate{
			Strategy: m.Name(), Symbol: symbol, Side: Buy, Qty: qty, RefPrice: last.Close,
			SignalOn: last.Day.Format("2006-01-02"),
			Reason:   fmt.Sprintf("close %.2f > %d-day mean %.2f and %d-day return %+.1f%%", last.Close, m.MeanDays, mean, m.ReturnDays, ret*100),
			Signals:  signals,
		}, nil
	case held > 0 && !up:
		return &Candidate{
			Strategy: m.Name(), Symbol: symbol, Side: Sell, Qty: held, RefPrice: last.Close,
			SignalOn: last.Day.Format("2006-01-02"),
			Reason:   fmt.Sprintf("trend broke: close %.2f vs mean %.2f, %d-day return %+.1f%%", last.Close, mean, m.ReturnDays, ret*100),
			Signals:  signals,
		}, nil
	}
	return nil, nil
}

func avg(xs []float64) float64 {
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// dailyVol is the sample stdev of simple daily returns over the closes.
func dailyVol(closes []float64) float64 {
	if len(closes) < 3 {
		return 0
	}
	rets := make([]float64, 0, len(closes)-1)
	for i := 1; i < len(closes); i++ {
		rets = append(rets, closes[i]/closes[i-1]-1)
	}
	m := avg(rets)
	ss := 0.0
	for _, r := range rets {
		ss += (r - m) * (r - m)
	}
	return math.Sqrt(ss / float64(len(rets)-1))
}

func round(x float64) float64 { return math.Round(x*1e4) / 1e4 }
