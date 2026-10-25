package strategy

import (
	"strings"
	"testing"

	"github.com/Blacklotus89898/Tulip/internal/marketdata"
)

var (
	riskSyms = []string{"VFV.TO", "XEF.TO", "XIU.TO"}
	allSyms  = []string{"VFV.TO", "XEF.TO", "XIU.TO", "ZAG.TO"}
)

func mixedBars() map[string][]marketdata.Candle {
	return map[string][]marketdata.Candle{
		"VFV.TO": series(300, uptrend), "XIU.TO": series(300, uptrend),
		"XEF.TO": series(300, downtrend), "ZAG.TO": series(300, func(i int) float64 { return 15 + 0.01*float64(i%7) }),
	}
}

// Every allocator explains itself, and its reasons come from the same pass
// as its targets: Targets == Decide().Targets, one reason per symbol.
func TestEveryAllocatorExplainsItsTargets(t *testing.T) {
	bars := mixedBars()
	for _, a := range []Allocator{
		EqualWeight{Symbols: allSyms},
		TrendFilter{Risk: riskSyms, Safe: "ZAG.TO"},
		TrendEnsemble{Risk: riskSyms, Safe: "ZAG.TO"},
		DualMomentum{Risk: riskSyms, Safe: "ZAG.TO"},
		InverseVol{Symbols: allSyms},
	} {
		if _, ok := a.(Explainer); !ok {
			t.Errorf("%s does not explain itself", a.Name())
			continue
		}
		d, err := Decide(a, bars)
		if err != nil {
			t.Fatalf("%s: %v", a.Name(), err)
		}
		tg, err := a.Targets(bars)
		if err != nil {
			t.Fatal(err)
		}
		sum := 0.0
		for s, w := range tg {
			sum += w
			if d.Targets[s] != w {
				t.Errorf("%s: Targets[%s]=%v but Decide says %v", a.Name(), s, w, d.Targets[s])
			}
		}
		if sum > 1+1e-9 {
			t.Errorf("%s: weights sum to %v", a.Name(), sum)
		}
		for _, s := range allSyms {
			if len(d.Reasons[s]) < 20 {
				t.Errorf("%s: no real reason for %s: %q", a.Name(), s, d.Reasons[s])
			}
		}
	}
}

func TestTrendReasonsNameTheRuleAndNumbers(t *testing.T) {
	d, err := TrendFilter{Risk: riskSyms, Safe: "ZAG.TO"}.Decide(mixedBars())
	if err != nil {
		t.Fatal(err)
	}
	if r := d.Reasons["XEF.TO"]; !strings.Contains(r, "below its 200-day average") || !strings.Contains(r, "moves to ZAG.TO") {
		t.Errorf("XEF reason = %q", r)
	}
	if r := d.Reasons["XIU.TO"]; !strings.Contains(r, "above its 200-day average") || !strings.Contains(r, "%") {
		t.Errorf("XIU reason = %q", r)
	}
	if r := d.Reasons["ZAG.TO"]; !strings.Contains(r, "XEF.TO") {
		t.Errorf("ZAG reason should name who it covers: %q", r)
	}
}

func TestTrendEnsembleSplitsByWindows(t *testing.T) {
	// Rises for 200 days, then falls for 50: above the long means, below the short ones.
	turn := series(250, func(i int) float64 {
		if i < 200 {
			return 50 + 0.1*float64(i)
		}
		return 70 - 0.15*float64(i-200)
	})
	bars := map[string][]marketdata.Candle{"A": turn, "S": series(250, func(int) float64 { return 10 })}
	d, err := TrendEnsemble{Risk: []string{"A"}, Safe: "S"}.Decide(bars)
	if err != nil {
		t.Fatal(err)
	}
	if a, s := d.Targets["A"], d.Targets["S"]; a <= 0 || a >= 1 || a+s < 0.999 || a+s > 1.001 {
		t.Errorf("want a partial split, got A=%v S=%v (%s)", a, s, d.Reasons["A"])
	}
	if !strings.Contains(d.Reasons["A"], "trend windows positive") {
		t.Errorf("reason = %q", d.Reasons["A"])
	}
}
