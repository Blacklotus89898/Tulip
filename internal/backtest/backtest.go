// Package backtest replays Tulip's PRODUCTION strategy and Thorn over
// dividend-adjusted daily history — the main evidence for whether
// momentum-v1 is worth acting on (live paper adds only approval behaviour and
// real fill quotes; the signals are a pure function of the same bars).
//
// Honesty rules baked in:
//   - strategy.Momentum.Evaluate and thorn.Check are called as-is, with the
//     inputs the live desk builds (cash, close-marked equity, suggestions per
//     day, nil confidence) — nothing is reimplemented;
//   - a signal on day t's completed bar fills at day t+1's OPEN with the paper
//     broker's slippage (the 10:05 cycle), after Thorn's execute re-check;
//   - Thorn's daily loss cap sees DayPnL 0 (the live 10:05 check runs minutes
//     into the session) — so it never binds here;
//   - one common start date (every symbol has enough history), stated in the
//     result; two benchmarks — buy-and-hold XIU and equal-weight buy-and-hold
//     of the same symbols (asset mix vs timing skill);
//   - a held-out final segment is reported separately; sweeps are
//     sensitivity, never "pick the best".
package backtest

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/marketdata"
	"github.com/Blacklotus89898/Tulip/internal/strategy"
	"github.com/Blacklotus89898/Tulip/internal/thorn"
)

// Config for one run.
type Config struct {
	Strategy    strategy.Momentum
	Thorn       thorn.Config
	Cash        float64 // default 10,000 CAD
	SlippageBps float64 // default 10 (= broker.Paper)
	Commission  float64 // per order, CAD (0 = commission-free ETFs)
	Benchmark   string  // default XIU.TO
	HoldoutFrac float64 // final fraction reported separately, default 0.3
}

func (c Config) withDefaults() Config {
	if c.Cash == 0 {
		c.Cash = 10000
	}
	if c.SlippageBps == 0 {
		c.SlippageBps = 10
	}
	if c.Benchmark == "" {
		c.Benchmark = "XIU.TO"
	}
	if c.HoldoutFrac == 0 {
		c.HoldoutFrac = 0.3
	}
	if c.Thorn.MaxOrderValue == 0 {
		c.Thorn = thorn.DefaultConfig()
	}
	// strategy.Momentum's own defaults, made explicit for the report.
	m := &c.Strategy
	if m.MeanDays == 0 {
		m.MeanDays = 50
	}
	if m.ReturnDays == 0 {
		m.ReturnDays = 20
	}
	if m.VolDays == 0 {
		m.VolDays = 20
	}
	if m.TargetVol == 0 {
		m.TargetVol = 0.10
	}
	if m.MaxWeight == 0 {
		m.MaxWeight = 0.20
	}
	return c
}

// Trade is one simulated fill.
type Trade struct {
	Day    string  `json:"day"` // fill day
	Signal string  `json:"signal_on"`
	Symbol string  `json:"symbol"`
	Side   string  `json:"side"`
	Qty    int     `json:"qty"`
	Price  float64 `json:"price"`
	Fee    float64 `json:"fee"`
}

// RoundTrip is an entry and its exit.
type RoundTrip struct {
	Symbol    string  `json:"symbol"`
	Entry     string  `json:"entry"`
	Exit      string  `json:"exit"`
	Days      int     `json:"days"`
	ReturnPct float64 `json:"return_pct"` // after slippage and fees
}

// Metrics of one equity curve over a period.
type Metrics struct {
	From        string  `json:"from"`
	To          string  `json:"to"`
	TotalReturn float64 `json:"total_return"`
	CAGR        float64 `json:"cagr"`
	MaxDrawdown float64 `json:"max_drawdown"`
	AnnVol      float64 `json:"ann_vol"`
	Sharpe      float64 `json:"sharpe"` // rf = 0, daily returns annualised
}

// Result of a run.
type Result struct {
	Symbols     []string           `json:"symbols"`
	Start       string             `json:"start"` // common start (all symbols warmed up)
	End         string             `json:"end"`
	Params      map[string]any     `json:"params"`
	Strategy    Metrics            `json:"strategy"`
	BenchXIU    Metrics            `json:"bench_buy_hold"`
	BenchEqual  Metrics            `json:"bench_equal_weight"`
	Holdout     map[string]Metrics `json:"holdout"`  // strategy / bench_buy_hold / bench_equal_weight
	Exposure    float64            `json:"exposure"` // mean invested fraction
	Trades      int                `json:"trades"`
	RoundTrips  []RoundTrip        `json:"round_trips"`
	WinRate     float64            `json:"win_rate"`
	Rejected    map[string]int     `json:"thorn_rejections"` // reason → count
	Years       []YearRow          `json:"years"`
	FinalEquity float64            `json:"final_equity"`
	TradeLog    []Trade            `json:"trade_log"`
}

// YearRow compares calendar-year returns.
type YearRow struct {
	Year     int     `json:"year"`
	Strategy float64 `json:"strategy"`
	BuyHold  float64 `json:"buy_hold"`
	Equal    float64 `json:"equal_weight"`
}

type pending struct {
	c      strategy.Candidate
	signal string
}

// Run simulates. bars: symbol → completed daily candles, oldest first.
func Run(bars map[string][]marketdata.Candle, symbols []string, cfg Config) (*Result, error) {
	cfg = cfg.withDefaults()
	m := cfg.Strategy
	need := warmup(m)
	if _, ok := bars[cfg.Benchmark]; !ok {
		return nil, fmt.Errorf("benchmark %s has no bars", cfg.Benchmark)
	}
	for s, b := range bars {
		if len(b) > 10 {
			gap := b[len(b)/2].Day.Sub(b[len(b)/2-1].Day).Hours() / 24
			if gap > 5 {
				return nil, fmt.Errorf("%s: bars are %.0f days apart — not daily data", s, gap)
			}
		}
	}
	// Index bars by day per symbol; common start = latest warmed-up day.
	idx := map[string]map[string]int{}
	var start time.Time
	for _, s := range append(append([]string{}, symbols...), cfg.Benchmark) {
		b := bars[s]
		if len(b) <= need {
			return nil, fmt.Errorf("%s: %d bars, need more than %d", s, len(b), need)
		}
		idx[s] = map[string]int{}
		for i, c := range b {
			idx[s][key(c.Day)] = i
		}
		if d := b[need-1].Day; d.After(start) {
			start = d
		}
	}
	// The trading calendar: benchmark days from the start.
	var days []time.Time
	for _, c := range bars[cfg.Benchmark] {
		if !c.Day.Before(start) {
			days = append(days, c.Day)
		}
	}
	if len(days) < 30 {
		return nil, errors.New("not enough common history")
	}

	cash := cfg.Cash
	held := map[string]int{}
	lastClose := map[string]float64{}
	open := map[string]Trade{} // entry trade per symbol, for round trips
	var queue []pending
	res := &Result{Symbols: symbols, Start: key(days[0]), End: key(days[len(days)-1]), Rejected: map[string]int{}}
	equity := make([]float64, len(days))
	invested := 0.0
	slip := cfg.SlippageBps / 10000

	equityAt := func() float64 {
		e := cash
		for s, q := range held {
			e += float64(q) * lastClose[s]
		}
		return e
	}

	for di, day := range days {
		dk := key(day)
		// 1. Fill yesterday's signals at today's open (Thorn execute re-check).
		eqOpen := cash
		for s, q := range held {
			if i, ok := idx[s][dk]; ok {
				eqOpen += float64(q) * bars[s][i].Open
			} else {
				eqOpen += float64(q) * lastClose[s]
			}
		}
		for _, p := range queue {
			i, ok := idx[p.c.Symbol][dk]
			if !ok {
				res.Rejected["no bar on fill day"]++
				continue
			}
			px := bars[p.c.Symbol][i].Open
			qty := p.c.Qty
			if p.c.Side == strategy.Sell {
				qty = min(qty, held[p.c.Symbol])
			}
			v := thorn.Check(cfg.Thorn, thorn.Request{Stage: thorn.Execute, Symbol: p.c.Symbol, Side: string(p.c.Side),
				Qty: qty, Price: px, Held: held[p.c.Symbol], Cash: cash, Equity: eqOpen})
			if !v.Allowed {
				for _, r := range v.Reasons {
					res.Rejected["execute: "+reasonKey(r)]++
				}
				continue
			}
			fill := px * (1 + slip)
			if p.c.Side == strategy.Sell {
				fill = px * (1 - slip)
			}
			t := Trade{Day: dk, Signal: p.signal, Symbol: p.c.Symbol, Side: string(p.c.Side), Qty: qty, Price: round4(fill), Fee: cfg.Commission}
			if p.c.Side == strategy.Buy {
				cost := float64(qty)*fill + cfg.Commission
				if cost > cash {
					res.Rejected["execute: cash after slippage/fees"]++
					continue
				}
				cash -= cost
				held[p.c.Symbol] += qty
				open[p.c.Symbol] = t
			} else {
				cash += float64(qty)*fill - cfg.Commission
				held[p.c.Symbol] -= qty
				if e, ok := open[p.c.Symbol]; ok && held[p.c.Symbol] == 0 {
					in := float64(e.Qty)*e.Price + e.Fee
					out := float64(qty)*fill - cfg.Commission
					res.RoundTrips = append(res.RoundTrips, RoundTrip{Symbol: p.c.Symbol, Entry: e.Day, Exit: dk,
						Days: daysBetween(e.Day, dk), ReturnPct: round4(out/in - 1)})
					delete(open, p.c.Symbol)
				}
				if held[p.c.Symbol] == 0 {
					delete(held, p.c.Symbol)
				}
			}
			res.TradeLog = append(res.TradeLog, t)
		}
		queue = queue[:0]

		// 2. Mark at today's close.
		for s := range idx {
			if i, ok := idx[s][dk]; ok {
				lastClose[s] = bars[s][i].Close
			}
		}
		eq := equityAt()
		equity[di] = eq
		if eq > 0 {
			invested += (eq - cash) / eq
		}

		// 3. Today's completed bar → signals for tomorrow (the live cycle).
		if di == len(days)-1 {
			break
		}
		today := 0
		for _, s := range symbols {
			i, ok := idx[s][dk]
			if !ok {
				continue
			}
			cand, err := m.Evaluate(s, bars[s][:i+1], held[s], eq)
			if err != nil || cand == nil {
				continue
			}
			v := thorn.Check(cfg.Thorn, thorn.Request{Stage: thorn.Propose, Symbol: s, Side: string(cand.Side),
				Qty: cand.Qty, Price: cand.RefPrice, Held: held[s], Cash: cash, Equity: eq, SuggestionsToday: today})
			if !v.Allowed {
				for _, r := range v.Reasons {
					res.Rejected["propose: "+reasonKey(r)]++
				}
				continue
			}
			today++
			queue = append(queue, pending{c: *cand, signal: dk})
		}
	}

	res.Trades = len(res.TradeLog)
	res.Exposure = round4(invested / float64(len(days)))
	res.FinalEquity = math.Round(equity[len(equity)-1]*100) / 100
	wins := 0
	for _, rt := range res.RoundTrips {
		if rt.ReturnPct > 0 {
			wins++
		}
	}
	if len(res.RoundTrips) > 0 {
		res.WinRate = round4(float64(wins) / float64(len(res.RoundTrips)))
	}

	bh := buyHold(bars, []string{cfg.Benchmark}, idx, days, cfg)
	ew := buyHold(bars, symbols, idx, days, cfg)
	res.Strategy, res.BenchXIU, res.BenchEqual = metrics(days, equity), metrics(days, bh), metrics(days, ew)
	h := int(float64(len(days)) * (1 - cfg.HoldoutFrac))
	res.Holdout = map[string]Metrics{
		"strategy": metrics(days[h:], equity[h:]), "bench_buy_hold": metrics(days[h:], bh[h:]),
		"bench_equal_weight": metrics(days[h:], ew[h:]),
	}
	res.Years = years(days, equity, bh, ew)
	res.Params = map[string]any{"mean_days": m.MeanDays, "return_days": m.ReturnDays, "vol_days": m.VolDays,
		"target_vol": m.TargetVol, "max_weight": m.MaxWeight, "cash": cfg.Cash, "slippage_bps": cfg.SlippageBps,
		"commission": cfg.Commission, "fill": "next day's open", "prices": "dividend-adjusted",
		"thorn_max_order": cfg.Thorn.MaxOrderValue, "thorn_max_weight": cfg.Thorn.MaxPositionWeight}
	return res, nil
}

// buyHold: cash split equally across syms at the first day's open (slippage
// and one commission each), whole shares, then held.
func buyHold(bars map[string][]marketdata.Candle, syms []string, idx map[string]map[string]int, days []time.Time, cfg Config) []float64 {
	cash := cfg.Cash
	qty := map[string]int{}
	d0 := key(days[0])
	per := cfg.Cash / float64(len(syms))
	for _, s := range syms {
		px := bars[s][idx[s][d0]].Open * (1 + cfg.SlippageBps/10000)
		q := int(math.Floor((per - cfg.Commission) / px))
		if q > 0 {
			qty[s] = q
			cash -= float64(q)*px + cfg.Commission
		}
	}
	out := make([]float64, len(days))
	last := map[string]float64{}
	for i, d := range days {
		e := cash
		for s, q := range qty {
			if j, ok := idx[s][key(d)]; ok {
				last[s] = bars[s][j].Close
			}
			e += float64(q) * last[s]
		}
		out[i] = e
	}
	return out
}

func metrics(days []time.Time, eq []float64) Metrics {
	m := Metrics{From: key(days[0]), To: key(days[len(days)-1])}
	if len(eq) < 2 || eq[0] <= 0 {
		return m
	}
	m.TotalReturn = round4(eq[len(eq)-1]/eq[0] - 1)
	yrs := days[len(days)-1].Sub(days[0]).Hours() / 24 / 365.25
	if yrs > 0 && eq[len(eq)-1] > 0 {
		m.CAGR = round4(math.Pow(eq[len(eq)-1]/eq[0], 1/yrs) - 1)
	}
	peak, dd := eq[0], 0.0
	rets := make([]float64, 0, len(eq)-1)
	for i, e := range eq {
		peak = math.Max(peak, e)
		dd = math.Min(dd, e/peak-1)
		if i > 0 && eq[i-1] > 0 {
			rets = append(rets, e/eq[i-1]-1)
		}
	}
	m.MaxDrawdown = round4(dd)
	mean, sd := meanStd(rets)
	m.AnnVol = round4(sd * math.Sqrt(252))
	if sd > 0 {
		m.Sharpe = math.Round(mean/sd*math.Sqrt(252)*100) / 100
	}
	return m
}

func years(days []time.Time, s, b, e []float64) []YearRow {
	var out []YearRow
	startIdx := 0
	for i := 1; i <= len(days); i++ {
		if i == len(days) || days[i].Year() != days[startIdx].Year() {
			j := i - 1
			base := startIdx
			if startIdx > 0 {
				base = startIdx - 1 // previous year's last close
			}
			out = append(out, YearRow{Year: days[startIdx].Year(),
				Strategy: round4(s[j]/s[base] - 1), BuyHold: round4(b[j]/b[base] - 1), Equal: round4(e[j]/e[base] - 1)})
			startIdx = i
		}
	}
	return out
}

func meanStd(xs []float64) (float64, float64) {
	if len(xs) < 2 {
		return 0, 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	mean := s / float64(len(xs))
	ss := 0.0
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	return mean, math.Sqrt(ss / float64(len(xs)-1))
}

func warmup(m strategy.Momentum) int {
	mean, ret, vol := m.MeanDays, m.ReturnDays, m.VolDays
	if mean == 0 {
		mean = 50
	}
	if ret == 0 {
		ret = 20
	}
	if vol == 0 {
		vol = 20
	}
	return max(mean, ret+1, vol+1)
}

// reasonKey groups Thorn reasons by their rule (drops the numbers).
func reasonKey(r string) string {
	for _, k := range []string{"per-order cap", "exceeds cash", "of equity", "daily loss", "suggestions today", "exceeds held", "allowlist"} {
		if strings.Contains(r, k) {
			return k
		}
	}
	return r
}

func key(t time.Time) string { return t.Format("2006-01-02") }

func daysBetween(a, b string) int {
	ta, _ := time.Parse("2006-01-02", a)
	tb, _ := time.Parse("2006-01-02", b)
	return int(tb.Sub(ta).Hours() / 24)
}

func round4(x float64) float64 { return math.Round(x*1e4) / 1e4 }

// Sweep runs the default and neighbouring parameters — SENSITIVITY ONLY: a
// robust strategy degrades gracefully around its defaults; a spike at one
// setting is noise, never a parameter to adopt.
func Sweep(bars map[string][]marketdata.Candle, symbols []string, cfg Config, means, rets []int) ([]*Result, error) {
	var out []*Result
	for _, mn := range means {
		for _, r := range rets {
			c := cfg
			c.Strategy.MeanDays, c.Strategy.ReturnDays = mn, r
			res, err := Run(bars, symbols, c)
			if err != nil {
				return nil, err
			}
			res.TradeLog, res.RoundTrips, res.Years = nil, nil, nil
			out = append(out, res)
		}
	}
	return out, nil
}
