package desk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/journal"
	"github.com/Blacklotus89898/Tulip/internal/portfolio"
)

// Co-pilot mode (ADR 0003): Tulip reads ONE real Questrade account and the
// operator places every order. Sizing and Thorn use the live account; fills
// come from reconciled broker executions, never from Tulip.

// Event kinds for co-pilot mode.
const (
	KindBrokerUnavailable = "broker.unavailable"
	KindBrokerExecution   = "broker.execution"    // one order's aggregated fills
	KindSuggestionActed   = "suggestion.acted"    // the operator placed it
	KindManualTrade       = "broker.manual_trade" // an order matching no suggestion
	KindBrokerMark        = "broker.mark"         // the account's first equity of the day
)

// Snapshot is the live account, in CAD, as the broker reports it.
type Snapshot struct {
	Cash   float64        `json:"cash"`
	Equity float64        `json:"equity"`
	Held   map[string]int `json:"held"` // whole shares by symbol
	At     time.Time      `json:"at"`
}

// AccountSource reads the real account (read-only).
type AccountSource interface {
	Snapshot(ctx context.Context) (*Snapshot, error)
}

// CoPilot reports whether the desk runs against a real account.
func (d *Desk) CoPilot() bool { return d.Account != nil }

// ErrCoPilot: in co-pilot mode Tulip never fills — the operator trades in Questrade.
var ErrCoPilot = errors.New("co-pilot mode: place this order yourself in Questrade — Tulip only reads the account")

// sizingBook is the book Thorn and the strategy see, with its equity: the
// live account in co-pilot mode (fail closed if unreadable), the paper
// strategy book otherwise.
func (d *Desk) sizingBook(ctx context.Context, cycle string, st *State) (*portfolio.Book, float64, error) {
	if d.Account == nil {
		b := st.Books[portfolio.Strategy]
		return b, b.Equity(d.quotes(ctx, b)), nil
	}
	snap, err := d.Account.Snapshot(ctx)
	if err != nil {
		_, _ = d.J.Append(ctx, KindBrokerUnavailable, cycle, map[string]string{"error": err.Error()})
		return nil, 0, fmt.Errorf("account snapshot unavailable — proposing nothing: %w", err)
	}
	b := &portfolio.Book{Name: "questrade", Funded: true, Cash: snap.Cash, Positions: map[string]portfolio.Position{}}
	for sym, q := range snap.Held {
		if q > 0 {
			b.Positions[sym] = portfolio.Position{Qty: q}
		}
	}
	d.markBroker(ctx, snap.Equity)
	return b, snap.Equity, nil
}

// markBroker journals the account's equity once per Toronto day: the base of
// Thorn's daily loss cap in co-pilot mode. (A same-day deposit reads as gain —
// the cap errs late, never early; the cap is sized for that.)
func (d *Desk) markBroker(ctx context.Context, equity float64) {
	today := d.now().In(calendar.Toronto).Format("2006-01-02")
	if _, ok := d.brokerMark(ctx, today); !ok {
		_, _ = d.J.Append(ctx, KindBrokerMark, "", map[string]any{"day": today, "equity": equity})
	}
}

func (d *Desk) brokerMark(ctx context.Context, day string) (float64, bool) {
	evs, err := d.J.ByKinds(ctx, KindBrokerMark)
	if err != nil {
		return 0, false
	}
	for _, e := range evs {
		var m struct {
			Day    string  `json:"day"`
			Equity float64 `json:"equity"`
		}
		if json.Unmarshal(e.Payload, &m) == nil && m.Day == day {
			return m.Equity, true
		}
	}
	return 0, false
}

func (d *Desk) brokerDayPnL(ctx context.Context, equity float64) float64 {
	if base, ok := d.brokerMark(ctx, d.now().In(calendar.Toronto).Format("2006-01-02")); ok {
		return equity - base
	}
	return 0
}

// SizingBook exposes the sizing book and its equity to the loop.
func (d *Desk) SizingBook(ctx context.Context, cycle string) (*portfolio.Book, float64, error) {
	st, err := d.LoadState(ctx)
	if err != nil {
		return nil, 0, err
	}
	return d.sizingBook(ctx, cycle, st)
}

// ---- reconciliation ----

// Execution is a broker fill, broker-agnostic.
type Execution struct {
	ID      string
	OrderID string
	Symbol  string
	Side    string // buy | sell (other sides are journaled as manual trades)
	Qty     float64
	Price   float64
	Fee     float64
	At      time.Time
}

// OrderFill is one order's aggregated, newly seen fills.
type OrderFill struct {
	OrderID  string    `json:"order_id"`
	Symbol   string    `json:"symbol"`
	Side     string    `json:"side"`
	Qty      float64   `json:"qty"`
	AvgPrice float64   `json:"avg_price"`
	Fees     float64   `json:"fees"`
	First    time.Time `json:"first"`
	Fills    int       `json:"fills"`
}

// Reconcile journals new executions once each (idempotent), aggregates them
// by order, and matches each order to a pending suggestion — same symbol and
// side, created the same Toronto day (a suggestion that expired at the close
// still matches an order placed before it); it is closed as acted on.
// Orders matching nothing are manual trades (excluded from the strategy's
// record). Returns how many orders were newly journaled.
func (d *Desk) Reconcile(ctx context.Context, execs []Execution) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	orders := map[string]*OrderFill{}
	for _, e := range execs {
		if err := d.J.ClaimExecution(ctx, e.ID); errors.Is(err, journal.ErrAlreadyUsed) {
			continue
		} else if err != nil {
			return 0, err
		}
		o, ok := orders[e.OrderID]
		if !ok {
			o = &OrderFill{OrderID: e.OrderID, Symbol: e.Symbol, Side: e.Side, First: e.At}
			orders[e.OrderID] = o
		}
		o.AvgPrice = (o.AvgPrice*o.Qty + e.Price*e.Qty) / (o.Qty + e.Qty)
		o.Qty += e.Qty
		o.Fees += e.Fee
		o.Fills++
		if e.At.Before(o.First) {
			o.First = e.At
		}
	}
	if len(orders) == 0 {
		return 0, nil
	}
	st, err := d.LoadState(ctx)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(orders))
	for id := range orders {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		o := orders[id]
		o.AvgPrice = math.Round(o.AvgPrice*10000) / 10000
		if _, err := d.J.Append(ctx, KindBrokerExecution, "", o); err != nil {
			return 0, err
		}
		s := matchSuggestion(st.Pending, o)
		if s == nil {
			// Placed before the session closed but polled after the
			// suggestion expired: still the strategy's trade.
			if e := matchSuggestion(st.Expired, o); e != nil && !o.First.After(e.Expires) {
				s = e
			}
		}
		if s != nil {
			slip := 0.0
			if s.RefPrice > 0 {
				slip = (o.AvgPrice - s.RefPrice) / s.RefPrice
				if o.Side == "sell" {
					slip = -slip
				}
			}
			if _, err := d.J.Append(ctx, KindSuggestionActed, "", map[string]any{
				"id": s.ID, "order_id": o.OrderID, "suggested_qty": s.Qty, "filled_qty": o.Qty,
				"ref_price": s.RefPrice, "avg_price": o.AvgPrice, "slippage": math.Round(slip*1e5) / 1e5,
			}); err != nil {
				return 0, err
			}
			sid := s.ID
			st.Pending, st.Expired = removeID(st.Pending, sid), removeID(st.Expired, sid)
			continue
		}
		if _, err := d.J.Append(ctx, KindManualTrade, "", o); err != nil {
			return 0, err
		}
	}
	return len(orders), nil
}

func matchSuggestion(pending []Suggestion, o *OrderFill) *Suggestion {
	day := o.First.In(calendar.Toronto).Format("2006-01-02")
	for i := range pending {
		s := &pending[i]
		if strings.EqualFold(s.Symbol, o.Symbol) && s.Side == o.Side &&
			s.Created.In(calendar.Toronto).Format("2006-01-02") == day {
			return s
		}
	}
	return nil
}

func removeID(list []Suggestion, id string) []Suggestion {
	out := list[:0]
	for _, s := range list {
		if s.ID != id {
			out = append(out, s)
		}
	}
	return out
}

// sessionClose is today's TSX close if it's still ahead, else now+ttl.
func sessionClose(now time.Time, ttl time.Duration) time.Time {
	_, close := calendar.SessionBounds(now)
	if now.Before(close) {
		return close
	}
	return now.Add(ttl)
}

// Attributed sums the strategy's real record in co-pilot mode: matched orders
// only (manual trades excluded), as P&L-free facts for the scorecard.
func (d *Desk) Attributed(ctx context.Context) ([]map[string]any, error) {
	evs, err := d.J.ByKinds(ctx, KindSuggestionActed)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, e := range evs {
		var m map[string]any
		if json.Unmarshal(e.Payload, &m) == nil {
			m["at"] = e.TS
			out = append(out, m)
		}
	}
	return out, nil
}
