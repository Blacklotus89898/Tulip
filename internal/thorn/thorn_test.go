package thorn

import (
	"strings"
	"testing"
)

func ok() Request {
	return Request{Stage: Propose, Symbol: "XIU.TO", Side: "buy", Qty: 10, Price: 50,
		Cash: 10000, Equity: 10000}
}

func conf(v float64) *float64 { return &v }

func TestThorn(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		name   string
		mutate func(*Request)
		allow  bool
		reason string
	}{
		{"clean buy", func(*Request) {}, true, ""},
		{"not allowlisted", func(r *Request) { r.Symbol = "GME" }, false, "allowlist"},
		{"order too big", func(r *Request) { r.Qty = 60 }, false, "per-order cap"},
		{"not enough cash", func(r *Request) { r.Cash = 100 }, false, "exceeds cash"},
		{"position too heavy", func(r *Request) { r.Held = 45 }, false, "of equity"},
		{"daily loss cap blocks buys", func(r *Request) { r.DayPnL = -300 }, false, "buys are blocked"},
		{"daily loss cap allows sells", func(r *Request) { r.DayPnL = -300; r.Side = "sell"; r.Held = 10 }, true, ""},
		{"no shorting", func(r *Request) { r.Side = "sell"; r.Held = 3 }, false, "no shorting"},
		{"low confidence", func(r *Request) { r.Confidence = conf(0.4) }, false, "below the floor"},
		{"rules-only skips the floor", func(r *Request) { r.Confidence = nil }, true, ""},
		{"suggestion cap at propose", func(r *Request) { r.SuggestionsToday = 3 }, false, "suggestions today"},
		{"suggestion cap not at execute", func(r *Request) { r.SuggestionsToday = 3; r.Stage = Execute }, true, ""},
		{"zero qty", func(r *Request) { r.Qty = 0 }, false, "whole share"},
		{"no price", func(r *Request) { r.Price = 0 }, false, "no valid price"},
		{"kill switch", func(r *Request) { r.KillSwitch = true; r.Stage = Execute }, false, "kill switch"},
	}
	for _, c := range cases {
		r := ok()
		c.mutate(&r)
		v := Check(cfg, r)
		if v.Allowed != c.allow {
			t.Errorf("%s: allowed=%v reasons=%v", c.name, v.Allowed, v.Reasons)
			continue
		}
		if c.reason != "" && !strings.Contains(strings.Join(v.Reasons, "; "), c.reason) {
			t.Errorf("%s: reasons %v should mention %q", c.name, v.Reasons, c.reason)
		}
	}
}

// Every failing rule is reported, and the kill switch always comes last.
func TestThornReportsEveryReason(t *testing.T) {
	r := ok()
	r.Symbol, r.Qty, r.KillSwitch = "GME", 0, true
	v := Check(DefaultConfig(), r)
	if len(v.Reasons) < 3 || !strings.Contains(v.Reasons[len(v.Reasons)-1], "kill switch") {
		t.Errorf("reasons = %v", v.Reasons)
	}
}

func TestPerSymbolWeightOverride(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxOrderValue = 1e6
	cfg.MaxWeightBySymbol = map[string]float64{"ZAG.TO": 1}
	req := Request{Stage: Execute, Symbol: "ZAG.TO", Side: "buy", Qty: 600, Price: 15, Cash: 10000, Equity: 10000}
	if v := Check(cfg, req); !v.Allowed {
		t.Errorf("ZAG at 90%% must pass with a 100%% override: %v", v.Reasons)
	}
	req.Symbol, req.Qty, req.Price = "XIU.TO", 200, 45
	if v := Check(cfg, req); v.Allowed {
		t.Error("XIU at 90% must still hit the default 25% cap")
	}
}
