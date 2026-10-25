package strategy

import (
	"math"
	"sort"
)

// Order is a whole-share delta: positive buys, negative sells.
type Order struct {
	Symbol string  `json:"symbol"`
	Qty    int     `json:"qty"`
	Target float64 `json:"target_weight"`
	Now    float64 `json:"current_weight"`
}

// Rebalance turns target weights into orders — the ONE implementation the
// backtester and the live loop share. Whole shares, floor-sized after
// slippage so buys fit; an order is skipped when the position is already held
// and within band of its target (fewer, cheaper trades). Exits and first
// entries are never skipped. Sells and buys come back separately, each
// sorted by symbol: sells execute first so buys can use their cash.
func Rebalance(symbols []string, targets map[string]float64, held map[string]int, px map[string]float64,
	equity, band, slip float64) (sells, buys []Order) {
	if equity <= 0 {
		return nil, nil
	}
	syms := append([]string{}, symbols...)
	sort.Strings(syms)
	for _, s := range syms {
		p := px[s]
		if p <= 0 {
			continue
		}
		w := targets[s]
		cur := float64(held[s]) * p / equity
		want := int(math.Floor(w * equity / (p * (1 + slip))))
		if want == held[s] || (w > 0 && held[s] > 0 && math.Abs(cur-w) < band) {
			continue
		}
		o := Order{Symbol: s, Qty: want - held[s], Target: w, Now: cur}
		if o.Qty < 0 {
			sells = append(sells, o)
		} else {
			buys = append(buys, o)
		}
	}
	return sells, buys
}
