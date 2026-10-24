// Package broker executes orders behind an interface (ADR 0001 §4). v1 has
// only the paper broker: a synthetic fill at the observed quote plus slippage.
// There is deliberately no live implementation in v1 (ADR 0001 §8).
package broker

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/marketdata"
	"github.com/Blacklotus89898/Tulip/internal/portfolio"
)

// Paper fills orders synthetically.
type Paper struct {
	SlippageBps float64 // default 10 (0.10%)
	FeePerOrder float64 // flat, e.g. 0 for paper (Questrade ETFs buy free)
}

// ErrInvalidOrder rejects malformed orders before any fill.
var ErrInvalidOrder = errors.New("invalid order")

// Fill prices an order at the quote, worse by the slippage: buys pay up,
// sells receive less. The quote's own time is recorded on the fill.
func (p Paper) Fill(book, side, symbol string, qty int, q marketdata.Quote, suggestionID string) (portfolio.Fill, error) {
	if qty < 1 || q.Price <= 0 || (side != "buy" && side != "sell") || q.Symbol != symbol {
		return portfolio.Fill{}, fmt.Errorf("%w: %s %d %s at %.4f", ErrInvalidOrder, side, qty, symbol, q.Price)
	}
	bps := p.SlippageBps
	if bps == 0 {
		bps = 10
	}
	adj := q.Price * bps / 10000
	price := q.Price + adj
	if side == "sell" {
		price = q.Price - adj
	}
	return portfolio.Fill{
		Book: book, Symbol: symbol, Side: side, Qty: qty,
		Price: math.Round(price*10000) / 10000, QuotePrice: q.Price,
		QuoteTime: q.Time.UTC().Format(time.RFC3339), Fee: p.FeePerOrder, SuggestionID: suggestionID,
	}, nil
}
