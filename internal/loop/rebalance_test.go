package loop

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/approval"
	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/desk"
	"github.com/Blacklotus89898/Tulip/internal/journal"
	"github.com/Blacklotus89898/Tulip/internal/marketdata"
	"github.com/Blacklotus89898/Tulip/internal/strategy"
	"github.com/Blacklotus89898/Tulip/internal/thorn"
)

var syms = []string{"VFV.TO", "XEF.TO", "XIU.TO", "ZAG.TO"}

type fakeMD struct {
	mu  sync.Mutex
	px  map[string]float64
	now func() time.Time
}

func (f *fakeMD) Daily(context.Context, string) ([]marketdata.Candle, error) { return nil, nil }
func (f *fakeMD) Quote(_ context.Context, s string) (marketdata.Quote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return marketdata.Quote{Symbol: s, Price: f.px[s], Time: f.now().Add(-15 * time.Minute)}, nil
}

// fakeHist: uptrends for XIU/VFV, a downtrend for XEF, flat ZAG — 300 weekdays
// ending before the month; plus one bar INSIDE the month that must be ignored.
type fakeHist struct{ monthStart time.Time }

func (h fakeHist) HistorySince(_ context.Context, s string, _ time.Time) ([]marketdata.Candle, error) {
	var out []marketdata.Candle
	d := h.monthStart.AddDate(0, 0, -1)
	for len(out) < 300 {
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
			out = append([]marketdata.Candle{{Day: d}}, out...)
		}
		d = d.AddDate(0, 0, -1)
	}
	for i := range out {
		x := float64(i)
		px := map[string]float64{"XIU.TO": 30 + 0.05*x, "VFV.TO": 80 + 0.1*x, "XEF.TO": 45 - 0.05*x, "ZAG.TO": 15}[s]
		out[i].Open, out[i].High, out[i].Low, out[i].Close = px, px, px, px
	}
	// A crash on the 1st of the month: completed only "today" — must not count.
	out = append(out, marketdata.Candle{Day: h.monthStart, Open: 1, High: 1, Low: 1, Close: 1})
	return out, nil
}

const pass = "a long operator passphrase"

func harness(t *testing.T, now *time.Time) (*Loop, *desk.Desk) {
	t.Helper()
	j, err := journal.Open(context.Background(), filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	clock := func() time.Time { return *now }
	md := &fakeMD{px: map[string]float64{"XIU.TO": 45, "VFV.TO": 110, "XEF.TO": 30, "ZAG.TO": 15}, now: clock}
	th := thorn.DefaultConfig()
	th.Allowlist, th.MaxOrderValue, th.MaxPositionWeight, th.MaxSuggestionsPerDay = syms, 1e5, 0.4, 20
	th.MaxWeightBySymbol = map[string]float64{"ZAG.TO": 1}
	hash, err := approval.HashPassphrase(pass)
	if err != nil {
		t.Fatal(err)
	}
	d := &desk.Desk{J: j, MD: md, Cfg: desk.Config{Thorn: th}, PassphraseHash: hash, Limiter: &approval.Limiter{}, Now: clock}
	if err := d.EnsureFunded(context.Background()); err != nil {
		t.Fatal(err)
	}
	ms := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, calendar.Toronto)
	l := &Loop{Desk: d, MD: md, Symbols: syms, Now: clock,
		Allocator: strategy.TrendFilter{Risk: []string{"VFV.TO", "XEF.TO", "XIU.TO"}, Safe: "ZAG.TO"}, Hist: fakeHist{ms}}
	return l, d
}

func pendingBy(t *testing.T, d *desk.Desk) map[string]desk.Suggestion {
	st, err := d.LoadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]desk.Suggestion{}
	for _, s := range st.Pending {
		out[s.Symbol] = s
	}
	return out
}

func TestMonthlyRebalanceProposesApprovesAndCloses(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 11, 0, 0, 0, calendar.Toronto) // first trading day of November
	l, d := harness(t, &now)

	res, err := l.Cycle(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	p := pendingBy(t, d)
	// XEF is in a downtrend → its third goes to ZAG: buys for VFV, XIU, ZAG only.
	if len(p) != 3 || p["XEF.TO"].ID != "" || p["ZAG.TO"].Side != "buy" || len(res.Suggestions) != 3 {
		t.Fatalf("want buys VFV/XIU/ZAG, got %v (res %+v)", p, res)
	}
	// ~1/3 each of 10k: VFV 110 → 30 shares, XIU 45 → 74, ZAG 15 → 222.
	if p["VFV.TO"].Qty != 30 || p["XIU.TO"].Qty != 74 || p["ZAG.TO"].Qty != 222 {
		t.Errorf("sizes = VFV %d XIU %d ZAG %d", p["VFV.TO"].Qty, p["XIU.TO"].Qty, p["ZAG.TO"].Qty)
	}
	// Another cycle the same day: nothing duplicated.
	if res, _ := l.Cycle(ctx, "test"); len(res.Suggestions) != 0 {
		t.Fatalf("re-proposed while pending: %v", res.Suggestions)
	}
	// The operator approves all three.
	for _, s := range p {
		if _, err := d.Approve(ctx, s.ID, s.Hash, pass, desk.Approver{IP: "test"}); err != nil {
			t.Fatalf("approve %s: %v", s.Symbol, err)
		}
	}
	// Next cycle: on target → month closed; later cycles do nothing.
	if res, _ := l.Cycle(ctx, "test"); len(res.Suggestions) != 0 {
		t.Fatalf("on target, yet proposed %v", res.Suggestions)
	}
	if done, _ := l.monthDone(ctx, "2026-11"); !done {
		t.Error("rebalance.done not journaled once on target")
	}
}

func TestSellsGoFirstAndBuysWait(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 11, 0, 0, 0, calendar.Toronto)
	l, d := harness(t, &now)
	// Start fully in XEF via an approved one-off suggestion.
	s, v, err := d.Propose(ctx, "seed", desk.ProposeRequest{Symbol: "XEF.TO", Side: "buy", Qty: 130, Source: "test"})
	if err != nil || s == nil {
		t.Fatalf("seed: %v %v %v", s, v, err)
	}
	if _, err := d.Approve(ctx, s.ID, s.Hash, pass, desk.Approver{IP: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Cycle(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	p := pendingBy(t, d)
	if p["XEF.TO"].Side != "sell" || len(p) != 1 {
		t.Fatalf("the XEF exit must be proposed alone (buys wait for its cash), got %v", p)
	}
}

func TestOutsideTheWindowNothingHappens(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 11, 0, 0, 0, calendar.Toronto) // 9th trading day
	l, d := harness(t, &now)
	if _, err := l.Cycle(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if p := pendingBy(t, d); len(p) != 0 {
		t.Errorf("rebalanced outside the month's window: %v", p)
	}
}
