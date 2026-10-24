package desk

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/portfolio"
)

type fakeAccount struct {
	snap *Snapshot
	err  error
}

func (f *fakeAccount) Snapshot(context.Context) (*Snapshot, error) { return f.snap, f.err }

func copilotHarness(t *testing.T, acct *fakeAccount) *harness {
	h := newHarness(t)
	h.d.Account = acct
	return h
}

func kinds(t *testing.T, h *harness, kind string) []json.RawMessage {
	t.Helper()
	evs, err := h.d.J.ByKinds(context.Background(), kind)
	if err != nil {
		t.Fatal(err)
	}
	out := []json.RawMessage{}
	for _, e := range evs {
		out = append(out, e.Payload)
	}
	return out
}

func TestCoPilotFailsClosedWithoutTheAccount(t *testing.T) {
	h := copilotHarness(t, &fakeAccount{err: errors.New("questrade: authentication failed")})
	s, _, err := h.d.Propose(context.Background(), "c1", ProposeRequest{Symbol: "XIU.TO", Side: "buy", Qty: 1, Source: "rules"})
	if err == nil || s != nil {
		t.Fatalf("no account snapshot must mean no suggestion, got (%v, %v)", s, err)
	}
	if n := len(kinds(t, h, KindBrokerUnavailable)); n != 1 {
		t.Errorf("broker.unavailable journaled %d times, want 1", n)
	}
	if st, _ := h.d.LoadState(context.Background()); len(st.Pending) != 0 {
		t.Errorf("pending = %d", len(st.Pending))
	}
}

func TestCoPilotSizesFromTheAccountAndNeverFills(t *testing.T) {
	ctx := context.Background()
	h := copilotHarness(t, &fakeAccount{snap: &Snapshot{Cash: 100, Equity: 100, Held: map[string]int{}}})
	// 10 × 50 = 500 > the real account's 100 cash: Thorn must refuse, even
	// though the paper book has plenty.
	if s, v, err := h.d.Propose(ctx, "c1", ProposeRequest{Symbol: "XIU.TO", Side: "buy", Qty: 10, Source: "rules"}); err != nil || s != nil {
		t.Fatalf("an order beyond the real cash must be rejected, got (%v, %+v, %v)", s, v, err)
	}

	h.d.Account = &fakeAccount{snap: &Snapshot{Cash: 5000, Equity: 5000, Held: map[string]int{}}}
	s := h.proposeBuy(t, 2)
	_, close := calendar.SessionBounds(*h.now)
	if !s.Expires.Equal(close) {
		t.Errorf("co-pilot suggestion expires %v, want the session close %v", s.Expires, close)
	}
	if _, err := h.d.Approve(ctx, s.ID, s.Hash, pass, who); !errors.Is(err, ErrCoPilot) {
		t.Fatalf("Approve in co-pilot mode = %v, want ErrCoPilot", err)
	}
	if n := len(kinds(t, h, portfolio.KindFill)); n != 0 {
		t.Errorf("co-pilot mode produced %d fills", n)
	}
}

func TestReconcileIsIdempotentAggregatesAndAttributes(t *testing.T) {
	ctx := context.Background()
	h := copilotHarness(t, &fakeAccount{snap: &Snapshot{Cash: 5000, Equity: 5000, Held: map[string]int{}}})
	s := h.proposeBuy(t, 4)
	at := h.now.Add(20 * time.Minute)
	execs := []Execution{
		{ID: "e1", OrderID: "o1", Symbol: "XIU.TO", Side: "buy", Qty: 1, Price: 50.10, At: at},
		{ID: "e2", OrderID: "o1", Symbol: "XIU.TO", Side: "buy", Qty: 3, Price: 50.30, At: at.Add(time.Second)},
		{ID: "e3", OrderID: "o2", Symbol: "VFV.TO", Side: "buy", Qty: 5, Price: 140, At: at}, // the operator's own trade
	}
	n, err := h.d.Reconcile(ctx, execs)
	if err != nil || n != 2 {
		t.Fatalf("reconcile = (%d, %v), want 2 orders", n, err)
	}
	if n, err := h.d.Reconcile(ctx, execs); err != nil || n != 0 {
		t.Fatalf("re-reconciling the same executions = (%d, %v), want 0", n, err)
	}

	acted := kinds(t, h, KindSuggestionActed)
	if len(acted) != 1 {
		t.Fatalf("suggestion.acted = %d, want 1", len(acted))
	}
	var a map[string]any
	_ = json.Unmarshal(acted[0], &a)
	if a["id"] != s.ID || a["filled_qty"].(float64) != 4 || a["avg_price"].(float64) != 50.25 {
		t.Errorf("acted = %v (want id %s, qty 4, avg 50.25)", a, s.ID)
	}
	if len(kinds(t, h, KindManualTrade)) != 1 {
		t.Error("the unmatched VFV order must be a manual trade")
	}
	if st, _ := h.d.LoadState(ctx); len(st.Pending) != 0 {
		t.Errorf("an acted-on suggestion must leave pending, got %d", len(st.Pending))
	}
}

func TestReconcileAfterTheCloseStillAttributes(t *testing.T) {
	ctx := context.Background()
	h := copilotHarness(t, &fakeAccount{snap: &Snapshot{Cash: 5000, Equity: 5000, Held: map[string]int{}}})
	s := h.proposeBuy(t, 2)
	placed := s.Expires.Add(-time.Minute) // 15:59
	*h.now = s.Expires.Add(5 * time.Minute)
	if n, _ := h.d.ExpireDue(ctx); n != 1 {
		t.Fatal("the suggestion should have expired at the close")
	}
	if _, err := h.d.Reconcile(ctx, []Execution{{ID: "e1", OrderID: "o1", Symbol: "XIU.TO", Side: "buy", Qty: 2, Price: 50, At: placed}}); err != nil {
		t.Fatal(err)
	}
	if len(kinds(t, h, KindSuggestionActed)) != 1 || len(kinds(t, h, KindManualTrade)) != 0 {
		t.Error("an order placed before the close must be attributed even if polled after expiry")
	}
	// The same symbol after the close is the operator's own trade.
	if _, err := h.d.Reconcile(ctx, []Execution{{ID: "e2", OrderID: "o2", Symbol: "XIU.TO", Side: "buy", Qty: 1, Price: 50, At: *h.now}}); err != nil {
		t.Fatal(err)
	}
	if len(kinds(t, h, KindManualTrade)) != 1 {
		t.Error("an order after the suggestion closed must be a manual trade")
	}
}

func TestCoPilotDailyLossCapUsesTheRealAccount(t *testing.T) {
	ctx := context.Background()
	acct := &fakeAccount{snap: &Snapshot{Cash: 5000, Equity: 5000, Held: map[string]int{}}}
	h := copilotHarness(t, acct)
	h.proposeBuy(t, 1)                                                      // marks the day's base equity at 5000
	acct.snap = &Snapshot{Cash: 4000, Equity: 4000, Held: map[string]int{}} // down 1000 > the 250 cap
	if s, _, err := h.d.Propose(ctx, "c2", ProposeRequest{Symbol: "XIU.TO", Side: "buy", Qty: 1, Source: "rules"}); err != nil || s != nil {
		t.Fatalf("a real-account loss past the cap must block buys, got (%v, %v)", s, err)
	}
	if n := len(kinds(t, h, KindBrokerMark)); n != 1 {
		t.Errorf("broker.mark journaled %d times, want once per day", n)
	}
}

func TestPartialFillsAcrossPollsStayAttributed(t *testing.T) {
	ctx := context.Background()
	h := copilotHarness(t, &fakeAccount{snap: &Snapshot{Cash: 5000, Equity: 5000, Held: map[string]int{}}})
	s := h.proposeBuy(t, 4)
	at := h.now.Add(10 * time.Minute)
	if _, err := h.d.Reconcile(ctx, []Execution{{ID: "e1", OrderID: "o1", Symbol: "XIU.TO", Side: "buy", Qty: 1, Price: 50, At: at}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Reconcile(ctx, []Execution{
		{ID: "e1", OrderID: "o1", Symbol: "XIU.TO", Side: "buy", Qty: 1, Price: 50, At: at}, // seen again
		{ID: "e2", OrderID: "o1", Symbol: "XIU.TO", Side: "buy", Qty: 3, Price: 50.4, At: at.Add(3 * time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(kinds(t, h, KindManualTrade)); n != 0 {
		t.Fatalf("a later partial of an attributed order became %d manual trade(s)", n)
	}
	rows, err := h.d.Attributed(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("attributed = (%v, %v), want one row per order", rows, err)
	}
	if r := rows[0]; r["id"] != s.ID || r["filled_qty"].(float64) != 4 || r["avg_price"].(float64) != 50.3 || r["suggested_qty"].(int) != 4 {
		t.Errorf("attributed row = %v (want qty 4, avg 50.3)", r)
	}
}
