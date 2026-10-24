// Package portfolio derives cash and positions by replaying journal events —
// nothing is stored as mutable state (ADR 0002 §2). Two books run side by side:
// "strategy" (Tulip's trades) and "benchmark" (buy-and-hold, ADR 0002 §1.4).
package portfolio

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/Blacklotus89898/Tulip/internal/journal"
)

// Book names.
const (
	Strategy  = "strategy"
	Benchmark = "benchmark"
)

// Event kinds this package reads and writes.
const (
	KindFunded = "portfolio.funded"
	KindFill   = "order.filled"
)

// Funded seeds a book with starting cash (once).
type Funded struct {
	Book string  `json:"book"`
	Cash float64 `json:"cash"`
}

// Fill is an executed (paper) order. Journaled as order.filled.
type Fill struct {
	Book         string  `json:"book"`
	Symbol       string  `json:"symbol"`
	Side         string  `json:"side"` // buy | sell
	Qty          int     `json:"qty"`
	Price        float64 `json:"price"` // after slippage
	QuotePrice   float64 `json:"quote_price"`
	QuoteTime    string  `json:"quote_time"`
	Fee          float64 `json:"fee"`
	SuggestionID string  `json:"suggestion_id,omitempty"`
}

// Position is a whole-share holding.
type Position struct {
	Qty     int     `json:"qty"`
	AvgCost float64 `json:"avg_cost"`
}

// Book is a replayed portfolio.
type Book struct {
	Name        string              `json:"name"`
	Funded      bool                `json:"funded"`
	Cash        float64             `json:"cash"`
	Positions   map[string]Position `json:"positions"`
	RealizedPnL float64             `json:"realized_pnl"`
	Fills       int                 `json:"fills"`
}

// Replay builds the named books from journal events (any order — they are
// sorted by id). Unknown event kinds are ignored.
func Replay(events []journal.Event, books ...string) (map[string]*Book, error) {
	sorted := append([]journal.Event(nil), events...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	out := map[string]*Book{}
	for _, b := range books {
		out[b] = &Book{Name: b, Positions: map[string]Position{}}
	}
	for _, e := range sorted {
		switch e.Kind {
		case KindFunded:
			var f Funded
			if err := json.Unmarshal(e.Payload, &f); err != nil {
				return nil, fmt.Errorf("event %d: %w", e.ID, err)
			}
			if b, ok := out[f.Book]; ok && !b.Funded {
				b.Funded, b.Cash = true, f.Cash
			}
		case KindFill:
			var f Fill
			if err := json.Unmarshal(e.Payload, &f); err != nil {
				return nil, fmt.Errorf("event %d: %w", e.ID, err)
			}
			b, ok := out[f.Book]
			if !ok {
				continue
			}
			if err := b.apply(f); err != nil {
				return nil, fmt.Errorf("event %d: %w", e.ID, err)
			}
		}
	}
	return out, nil
}

func (b *Book) apply(f Fill) error {
	p := b.Positions[f.Symbol]
	switch f.Side {
	case "buy":
		cost := float64(f.Qty)*f.Price + f.Fee
		total := float64(p.Qty)*p.AvgCost + float64(f.Qty)*f.Price
		p.Qty += f.Qty
		p.AvgCost = total / float64(p.Qty)
		b.Cash -= cost
	case "sell":
		if f.Qty > p.Qty {
			return fmt.Errorf("sell %d %s exceeds held %d (no shorting)", f.Qty, f.Symbol, p.Qty)
		}
		b.Cash += float64(f.Qty)*f.Price - f.Fee
		b.RealizedPnL += float64(f.Qty)*(f.Price-p.AvgCost) - f.Fee
		p.Qty -= f.Qty
		if p.Qty == 0 {
			p.AvgCost = 0
		}
	default:
		return fmt.Errorf("unknown side %q", f.Side)
	}
	b.Fills++
	if p.Qty == 0 {
		delete(b.Positions, f.Symbol)
	} else {
		b.Positions[f.Symbol] = p
	}
	return nil
}

// Equity is cash plus positions marked at prices (missing price → cost basis).
func (b *Book) Equity(prices map[string]float64) float64 {
	eq := b.Cash
	for sym, p := range b.Positions {
		px, ok := prices[sym]
		if !ok {
			px = p.AvgCost
		}
		eq += float64(p.Qty) * px
	}
	return math.Round(eq*100) / 100
}

// Held returns the whole-share quantity of symbol.
func (b *Book) Held(symbol string) int { return b.Positions[symbol].Qty }
