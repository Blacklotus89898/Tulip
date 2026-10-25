// Package shadow runs the pre-registered candidate strategies (ADR 0004) as
// paper books that nobody approves: on the same monthly schedule, data and
// order code as the live strategy, they record what each candidate WOULD
// hold, so candidates are judged on data none of them was chosen on.
//
// Shadow books live in their own journal kinds (shadow.*). They never write
// order.filled, never call the broker and never create suggestions: the live
// book's "no fill without a human approval" stays mechanically true.
package shadow

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/journal"
	"github.com/Blacklotus89898/Tulip/internal/marketdata"
	"github.com/Blacklotus89898/Tulip/internal/portfolio"
	"github.com/Blacklotus89898/Tulip/internal/strategy"
	"github.com/Blacklotus89898/Tulip/internal/thorn"
)

// Journal kinds — distinct from the real ledger's on purpose.
const (
	KindFunded     = "shadow.funded"
	KindFill       = "shadow.filled"
	KindRebalanced = "shadow.rebalanced"
	KindRejected   = "shadow.rejected"
	KindMark       = "shadow.mark"
	KindError      = "shadow.error"
)

// Prefix marks a shadow book's name.
const Prefix = "shadow:"

// BookName is the book a candidate's shadow trades in.
func BookName(a strategy.Allocator) string { return Prefix + a.Name() }

// Runner trades the shadow books.
type Runner struct {
	J          *journal.Journal
	Allocators []strategy.Allocator
	Symbols    []string
	Thorn      thorn.Config // the live limits (position-weight caps relaxed, see rebalance)
	Cash       float64      // starting cash per book (default 10000, = the live paper book)
	Slippage   float64      // fraction (default 0.001 = 10 bps, = the paper broker)
	Fee        float64      // per order (default 0; the scorecard reports fees separately)
	Band       float64      // rebalance drift band (default 0.03, = live)
}

func (r *Runner) cash() float64 {
	if r.Cash > 0 {
		return r.Cash
	}
	return 10000
}

func (r *Runner) slip() float64 {
	if r.Slippage > 0 {
		return r.Slippage
	}
	return 0.001
}

func (r *Runner) band() float64 {
	if r.Band > 0 {
		return r.Band
	}
	return 0.03
}

// Input is one cycle's view of the market, prepared by the live loop so the
// shadows see exactly what the live strategy saw.
type Input struct {
	Cycle    string
	Day      string                         // Toronto date, YYYY-MM-DD
	Month    string                         // YYYY-MM
	InWindow bool                           // within the month's rebalance window
	AsOf     string                         // last completed bar used for signals
	Bars     map[string][]marketdata.Candle // completed before the month started (nil = unavailable)
	Quotes   map[string]marketdata.Quote
	Now      time.Time
}

// Cycle funds, rebalances (once per month, in the window) and marks every
// shadow book. Errors are per book and journaled; the caller never fails on
// a shadow (they are measurement, not trading).
func (r *Runner) Cycle(ctx context.Context, in Input) {
	evs, err := r.J.ByKinds(ctx, KindFunded, KindFill, KindRebalanced, KindMark)
	if err != nil {
		_, _ = r.J.Append(ctx, KindError, in.Cycle, map[string]string{"error": err.Error()})
		return
	}
	names := make([]string, 0, len(r.Allocators))
	for _, a := range r.Allocators {
		names = append(names, BookName(a))
	}
	books, err := portfolio.ReplayKinds(evs, KindFunded, KindFill, names...)
	if err != nil {
		_, _ = r.J.Append(ctx, KindError, in.Cycle, map[string]string{"error": err.Error()})
		return
	}
	done, marked := map[string]bool{}, map[string]bool{}
	for _, e := range evs {
		var m struct{ Book, Month, Day string }
		if json.Unmarshal(e.Payload, &m) != nil {
			continue
		}
		switch e.Kind {
		case KindRebalanced:
			if m.Month == in.Month {
				done[m.Book] = true
			}
		case KindMark:
			if m.Day == in.Day {
				marked[m.Book] = true
			}
		}
	}
	for _, a := range r.Allocators {
		name := BookName(a)
		b := books[name]
		if !b.Funded {
			if _, err := r.J.Append(ctx, KindFunded, in.Cycle, portfolio.Funded{Book: name, Cash: r.cash()}); err != nil {
				continue
			}
			b.Funded, b.Cash = true, r.cash()
		}
		if in.InWindow && !done[name] && in.Bars != nil {
			if err := r.rebalance(ctx, in, a, b); err != nil {
				_, _ = r.J.Append(ctx, KindError, in.Cycle, map[string]string{"book": name, "error": err.Error()})
			}
		}
		if !marked[name] {
			px := map[string]float64{}
			for s, q := range in.Quotes {
				px[s] = q.Price
			}
			_, _ = r.J.Append(ctx, KindMark, in.Cycle, map[string]any{"book": name, "day": in.Day, "equity": b.Equity(px), "cash": round2(b.Cash)})
		}
	}
}

// rebalance moves one book to its candidate's targets at this cycle's
// quotes: sells first, then buys with the freed cash — instantly, since no
// human is in the loop. Every order passes the live Thorn limits.
func (r *Runner) rebalance(ctx context.Context, in Input, a strategy.Allocator, b *portfolio.Book) error {
	name := BookName(a)
	dec, err := strategy.Decide(a, in.Bars)
	if err != nil {
		return err
	}
	px, held := map[string]float64{}, map[string]int{}
	for _, s := range r.Symbols {
		q, ok := in.Quotes[s]
		if !ok || q.Price <= 0 {
			return fmt.Errorf("no quote for %s", s)
		}
		px[s], held[s] = q.Price, b.Held(s)
	}
	equity := b.Equity(px)
	sells, buys := strategy.Rebalance(r.Symbols, dec.Targets, held, px, equity, r.band(), r.slip())
	// Position-weight caps are sized for the LIVE strategy and would be
	// re-tuned on promotion; applied here they'd leave e.g. dual momentum (one
	// ETF at 100%) in cash, measuring nothing. Every other rule stands:
	// allowlist, cash, no shorting, order value (ADR 0004).
	cfg := r.Thorn
	cfg.MaxPositionWeight, cfg.MaxWeightBySymbol = 1, nil
	fills, rejected := 0, 0
	for _, o := range append(sells, buys...) {
		side, qty := "buy", o.Qty
		if qty < 0 {
			side, qty = "sell", -qty
		}
		v := thorn.Check(cfg, thorn.Request{Stage: thorn.Propose, Symbol: o.Symbol, Side: side, Qty: qty,
			Price: px[o.Symbol], Held: b.Held(o.Symbol), Cash: b.Cash, Equity: b.Equity(px)})
		if !v.Allowed {
			rejected++
			_, _ = r.J.Append(ctx, KindRejected, in.Cycle, map[string]any{"book": name, "month": in.Month,
				"symbol": o.Symbol, "side": side, "qty": qty, "reasons": v.Reasons})
			continue
		}
		q := in.Quotes[o.Symbol]
		price := q.Price * (1 + r.slip())
		if side == "sell" {
			price = q.Price * (1 - r.slip())
		}
		f := portfolio.Fill{Book: name, Symbol: o.Symbol, Side: side, Qty: qty, Price: math.Round(price*10000) / 10000,
			QuotePrice: q.Price, QuoteTime: q.Time.UTC().Format(time.RFC3339), Fee: r.Fee}
		if err := b.Apply(f); err != nil {
			return err
		}
		if _, err := r.J.Append(ctx, KindFill, in.Cycle, f); err != nil {
			return err
		}
		fills++
	}
	_, err = r.J.Append(ctx, KindRebalanced, in.Cycle, map[string]any{"book": name, "strategy": a.Name(), "month": in.Month,
		"as_of": in.AsOf, "targets": dec.Targets, "reasons": dec.Reasons, "fills": fills, "rejected": rejected})
	return err
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }
