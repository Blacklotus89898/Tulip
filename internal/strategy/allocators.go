package strategy

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/Blacklotus89898/Tulip/internal/marketdata"
)

// Allocator is a portfolio-level strategy: from COMPLETED bars (each slice
// ends at the decision day) it returns target weights summing to ≤ 1; the
// rest is cash. Pure — no clock, no I/O — so backtests and the live loop run
// the same code. Rebalancing cadence and order generation live outside.
type Allocator interface {
	Name() string
	Targets(bars map[string][]marketdata.Candle) (map[string]float64, error)
}

// Decision is an allocator's targets with the reason for each symbol's
// weight, computed in the same pass — the justification can never disagree
// with the decision.
type Decision struct {
	Targets map[string]float64 `json:"targets"`
	Reasons map[string]string  `json:"reasons"`
}

// Explainer is an Allocator that can say why. Every allocator here is one;
// Targets is Decide without the reasons.
type Explainer interface {
	Decide(bars map[string][]marketdata.Candle) (Decision, error)
}

// Decide returns a's decision, with reasons when a can explain itself.
func Decide(a Allocator, bars map[string][]marketdata.Candle) (Decision, error) {
	if e, ok := a.(Explainer); ok {
		return e.Decide(bars)
	}
	t, err := a.Targets(bars)
	return Decision{Targets: t, Reasons: map[string]string{}}, err
}

func newDecision() Decision {
	return Decision{Targets: map[string]float64{}, Reasons: map[string]string{}}
}

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

// EqualWeight holds every symbol at 1/n — the baseline a timing strategy must
// beat after costs.
type EqualWeight struct{ Symbols []string }

func (EqualWeight) Name() string { return "equal-weight" }

func (e EqualWeight) Targets(bars map[string][]marketdata.Candle) (map[string]float64, error) {
	d, err := e.Decide(bars)
	return d.Targets, err
}

func (e EqualWeight) Decide(map[string][]marketdata.Candle) (Decision, error) {
	d := newDecision()
	w := 1 / float64(len(e.Symbols))
	for _, s := range e.Symbols {
		d.Targets[s] = w
		d.Reasons[s] = fmt.Sprintf("%s holds 1/%d (%s): equal weight, rebalanced monthly back to %s.", s, len(e.Symbols), pct(w), pct(w))
	}
	return d, nil
}

// TrendFilter gives each risk symbol 1/n while its close is above its N-day
// mean; a symbol below its mean hands its share to the Safe symbol.
type TrendFilter struct {
	Risk []string
	Safe string
	Days int // default 200
}

func (TrendFilter) Name() string { return "trend-filter-200" }

func (t TrendFilter) Targets(bars map[string][]marketdata.Candle) (map[string]float64, error) {
	d, err := t.Decide(bars)
	return d.Targets, err
}

func (t TrendFilter) Decide(bars map[string][]marketdata.Candle) (Decision, error) {
	days := t.Days
	if days == 0 {
		days = 200
	}
	d := newDecision()
	w := 1 / float64(len(t.Risk))
	var handed []string
	for _, s := range t.Risk {
		b := bars[s]
		if len(b) < days {
			return Decision{}, fmt.Errorf("%s: need %d bars", s, days)
		}
		last, mean := b[len(b)-1].Close, meanClose(b[len(b)-days:])
		gap := last/mean - 1
		if last > mean {
			d.Targets[s] += w
			d.Reasons[s] = fmt.Sprintf("%s closed at %.2f, %.1f%% above its %d-day average (%.2f): uptrend, so it holds its %s share.",
				s, last, gap*100, days, mean, pct(w))
		} else {
			d.Targets[t.Safe] += w
			handed = append(handed, s)
			d.Reasons[s] = fmt.Sprintf("%s closed at %.2f, %.1f%% below its %d-day average (%.2f): downtrend, so its %s share moves to %s.",
				s, last, -gap*100, days, mean, pct(w), t.Safe)
		}
	}
	if len(handed) > 0 {
		d.Reasons[t.Safe] = fmt.Sprintf("%s is the safe asset: it takes the share of every ETF below its %d-day average (%s) — %s in total.",
			t.Safe, days, strings.Join(handed, ", "), pct(d.Targets[t.Safe]))
	} else {
		d.Reasons[t.Safe] = fmt.Sprintf("%s is the safe asset: every equity ETF is above its %d-day average, so it holds 0%%.", t.Safe, days)
	}
	return d, nil
}

// TrendEnsemble is the trend filter over several lookbacks at once: each risk
// symbol keeps the fraction of its 1/n share equal to the fraction of windows
// in which it is above its mean; the rest goes to Safe. It removes the bet on
// one window (pre-registered in ADR 0004; the 2026-10-24 sensitivity runs
// showed every window in 100–250 d works, which is mild look-ahead).
type TrendEnsemble struct {
	Risk []string
	Safe string
	Days []int // default 100, 150, 200, 250
}

func (TrendEnsemble) Name() string { return "trend-ensemble" }

func (t TrendEnsemble) Targets(bars map[string][]marketdata.Candle) (map[string]float64, error) {
	d, err := t.Decide(bars)
	return d.Targets, err
}

func (t TrendEnsemble) Decide(bars map[string][]marketdata.Candle) (Decision, error) {
	windows := t.Days
	if len(windows) == 0 {
		windows = []int{100, 150, 200, 250}
	}
	longest := 0
	for _, n := range windows {
		longest = max(longest, n)
	}
	d := newDecision()
	w := 1 / float64(len(t.Risk))
	var handed []string
	for _, s := range t.Risk {
		b := bars[s]
		if len(b) < longest {
			return Decision{}, fmt.Errorf("%s: need %d bars", s, longest)
		}
		last := b[len(b)-1].Close
		var up, down []string
		for _, n := range windows {
			if last > meanClose(b[len(b)-n:]) {
				up = append(up, fmt.Sprint(n))
			} else {
				down = append(down, fmt.Sprint(n))
			}
		}
		frac := float64(len(up)) / float64(len(windows))
		d.Targets[s] += w * frac
		if frac < 1 {
			d.Targets[t.Safe] += w * (1 - frac)
			handed = append(handed, s)
		}
		switch {
		case len(down) == 0:
			d.Reasons[s] = fmt.Sprintf("%s (%.2f) is above all its trend averages (%s days): holds its full %s share.",
				s, last, strings.Join(up, "/"), pct(w))
		case len(up) == 0:
			d.Reasons[s] = fmt.Sprintf("%s (%.2f) is below all its trend averages (%s days): its %s share moves to %s.",
				s, last, strings.Join(down, "/"), pct(w), t.Safe)
		default:
			d.Reasons[s] = fmt.Sprintf("%s (%.2f) is above its %s-day averages but below its %s-day: %d of %d trend windows positive, so it holds %s of its %s share and %s goes to %s.",
				s, last, strings.Join(up, "/"), strings.Join(down, "/"), len(up), len(windows), pct(w*frac), pct(w), pct(w*(1-frac)), t.Safe)
		}
	}
	if len(handed) > 0 {
		d.Reasons[t.Safe] = fmt.Sprintf("%s is the safe asset: it takes the trend-negative part of %s — %s in total.",
			t.Safe, strings.Join(handed, ", "), pct(d.Targets[t.Safe]))
	} else {
		d.Reasons[t.Safe] = fmt.Sprintf("%s is the safe asset: every equity ETF is above all its trend averages, so it holds 0%%.", t.Safe)
	}
	return d, nil
}

// DualMomentum holds 100% of the risk symbol with the best lookback return if
// that return beats the safe symbol's; otherwise 100% safe.
type DualMomentum struct {
	Risk     []string
	Safe     string
	Lookback int // trading days, default 252 (~12 months)
}

func (DualMomentum) Name() string { return "dual-momentum-12m" }

func (m DualMomentum) Targets(bars map[string][]marketdata.Candle) (map[string]float64, error) {
	d, err := m.Decide(bars)
	return d.Targets, err
}

func (m DualMomentum) Decide(bars map[string][]marketdata.Candle) (Decision, error) {
	lb := m.Lookback
	if lb == 0 {
		lb = 252
	}
	ret := func(s string) (float64, error) {
		b := bars[s]
		if len(b) <= lb {
			return 0, fmt.Errorf("%s: need %d bars", s, lb+1)
		}
		return b[len(b)-1].Close/b[len(b)-1-lb].Close - 1, nil
	}
	rets := map[string]float64{}
	best, bestRet := "", math.Inf(-1)
	for _, s := range m.Risk {
		r, err := ret(s)
		if err != nil {
			return Decision{}, err
		}
		rets[s] = r
		if r > bestRet {
			best, bestRet = s, r
		}
	}
	safeRet, err := ret(m.Safe)
	if err != nil {
		return Decision{}, err
	}
	d := newDecision()
	period := fmt.Sprintf("%d-day", lb)
	if lb == 252 {
		period = "12-month"
	}
	for _, s := range m.Risk {
		switch {
		case s == best && bestRet > safeRet:
			d.Targets[s] = 1
			d.Reasons[s] = fmt.Sprintf("%s had the best %s return of the equity ETFs (%+.1f%%) and beat %s (%+.1f%%): it holds 100%%.",
				s, period, rets[s]*100, m.Safe, safeRet*100)
		case s == best:
			d.Reasons[s] = fmt.Sprintf("%s had the best %s equity return (%+.1f%%) but did not beat %s (%+.1f%%): 0%%.",
				s, period, rets[s]*100, m.Safe, safeRet*100)
		default:
			d.Reasons[s] = fmt.Sprintf("%s's %s return (%+.1f%%) trails %s (%+.1f%%): 0%%.", s, period, rets[s]*100, best, bestRet*100)
		}
	}
	if bestRet > safeRet {
		d.Reasons[m.Safe] = fmt.Sprintf("%s (%+.1f%% over %s) is beaten by %s: 0%%.", m.Safe, safeRet*100, period, best)
	} else {
		d.Targets[m.Safe] = 1
		d.Reasons[m.Safe] = fmt.Sprintf("No equity ETF beat %s's %s return (%+.1f%%; best was %s %+.1f%%): 100%% safe.",
			m.Safe, period, safeRet*100, best, bestRet*100)
	}
	return d, nil
}

// InverseVol weights symbols by 1/volatility (Days-day daily returns).
type InverseVol struct {
	Symbols []string
	Days    int // default 60
}

func (InverseVol) Name() string { return "inverse-vol-60" }

func (v InverseVol) Targets(bars map[string][]marketdata.Candle) (map[string]float64, error) {
	d, err := v.Decide(bars)
	return d.Targets, err
}

func (v InverseVol) Decide(bars map[string][]marketdata.Candle) (Decision, error) {
	days := v.Days
	if days == 0 {
		days = 60
	}
	inv := map[string]float64{}
	vols := map[string]float64{}
	total := 0.0
	for _, s := range v.Symbols {
		b := bars[s]
		if len(b) <= days {
			return Decision{}, fmt.Errorf("%s: need %d bars", s, days+1)
		}
		closes := make([]float64, 0, days+1)
		for _, c := range b[len(b)-1-days:] {
			closes = append(closes, c.Close)
		}
		vol := dailyVol(closes)
		if vol <= 0 {
			return Decision{}, fmt.Errorf("%s: zero volatility", s)
		}
		vols[s] = vol
		inv[s] = 1 / vol
		total += 1 / vol
	}
	d := newDecision()
	syms := append([]string{}, v.Symbols...)
	sort.Strings(syms)
	for _, s := range syms {
		d.Targets[s] = inv[s] / total
		d.Reasons[s] = fmt.Sprintf("%s's %d-day volatility is %.1f%%/yr; weights are proportional to 1/volatility, so it holds %s.",
			s, days, vols[s]*math.Sqrt(252)*100, pct(d.Targets[s]))
	}
	return d, nil
}

func meanClose(b []marketdata.Candle) float64 {
	s := 0.0
	for _, c := range b {
		s += c.Close
	}
	return s / float64(len(b))
}
