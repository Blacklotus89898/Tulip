package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/backtest"
	"github.com/Blacklotus89898/Tulip/internal/shadow"
	"github.com/Blacklotus89898/Tulip/internal/strategy"
)

// The charts page (GET /charts) draws what the scorecard tabulates: live and
// shadow book values over time, the pre-registered backtests, a flow diagram
// of each strategy's rule with today's numbers, and the decision pipeline
// with how many suggestions reached each step. Read-only like /scorecard: no
// form, nothing to approve, no secret on it.

//go:embed charts.html
var chartsPage []byte

// LiveCharts is the journal side: book values and today's decisions.
type LiveCharts struct {
	Live      string                       `json:"live"`
	Symbols   []string                     `json:"symbols"` // risk symbols, then the safe one (last)
	Since     string                       `json:"since"`
	Books     []shadow.BookSeries          `json:"books"`
	Decisions map[string]strategy.Decision `json:"-"`
	AsOf      string                       `json:"as_of"`
	Counts    map[string]int64             `json:"-"`
}

// BacktestRun is one strategy's backtest, ready to chart.
type BacktestRun struct {
	Name        string           `json:"name"`
	Live        bool             `json:"live,omitempty"`
	CAGR        float64          `json:"cagr"`
	MaxDD       float64          `json:"max_drawdown"`
	Sharpe      float64          `json:"sharpe"`
	HoldoutCAGR float64          `json:"holdout_cagr"`
	Trades      int              `json:"trades"`
	Curve       []backtest.Point `json:"curve"`
}

// BacktestSet is every pre-registered candidate on one common start.
type BacktestSet struct {
	Universe     []string      `json:"universe"`
	Start        string        `json:"start"`
	End          string        `json:"end"`
	HoldoutStart string        `json:"holdout_start"`
	Commission   float64       `json:"commission"`
	SlippageBps  float64       `json:"slippage_bps"`
	Bench        BacktestRun   `json:"bench"`
	Runs         []BacktestRun `json:"runs"`
}

// ChartsSources feeds the page (main wires the journal, loop and backtests).
type ChartsSources struct {
	Live     func(ctx context.Context) (*LiveCharts, error)
	Backtest func(ctx context.Context) (*BacktestSet, error)
}

// Cache lifetimes: live data like the scorecard; backtests run on full
// history (a Yahoo fetch per symbol), so once a day is plenty.
const (
	liveChartsTTL = 10 * time.Minute
	backtestTTL   = 24 * time.Hour
)

// RegisterCharts mounts GET /charts and GET /charts.json.
func RegisterCharts(mux *http.ServeMux, src ChartsSources) {
	h := &chartsHandler{src: src}
	mux.HandleFunc("GET /charts", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
		_, _ = w.Write(chartsPage)
	})
	mux.HandleFunc("GET /charts.json", h.json)
}

type chartsHandler struct {
	src ChartsSources

	liveMu sync.Mutex
	live   *LiveCharts
	liveAt time.Time

	btMu  sync.Mutex
	bt    *BacktestSet
	btAt  time.Time
	btErr string
}

func (h *chartsHandler) liveData(ctx context.Context) (*LiveCharts, error) {
	h.liveMu.Lock()
	defer h.liveMu.Unlock()
	if h.live != nil && time.Since(h.liveAt) < liveChartsTTL {
		return h.live, nil
	}
	d, err := h.src.Live(ctx)
	if err != nil {
		return nil, err
	}
	h.live, h.liveAt = d, time.Now()
	return d, nil
}

// backtestData never fails the page: a Yahoo outage shows as a note on the
// backtest section and is retried after an hour, not on every view.
func (h *chartsHandler) backtestData(ctx context.Context) (*BacktestSet, string) {
	h.btMu.Lock()
	defer h.btMu.Unlock()
	fresh := h.bt != nil && time.Since(h.btAt) < backtestTTL
	retryLater := h.bt == nil && h.btErr != "" && time.Since(h.btAt) < time.Hour
	if fresh || retryLater || h.src.Backtest == nil {
		return h.bt, h.btErr
	}
	bctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	d, err := h.src.Backtest(bctx)
	h.btAt = time.Now()
	if err != nil {
		h.btErr = "backtest unavailable: " + err.Error()
		return h.bt, h.btErr // keep yesterday's set if there is one
	}
	h.bt, h.btErr = d, ""
	return d, ""
}

// Strategy is one allocator's rule as a diagram: the rule, then each
// symbol's outcome today.
type Strategy struct {
	Name    string    `json:"name"`
	Live    bool      `json:"live,omitempty"`
	Rule    string    `json:"rule"`
	Test    string    `json:"test"` // the question asked of each risk symbol
	Symbols []Outcome `json:"symbols"`
}

// Outcome is one symbol's weight and why.
type Outcome struct {
	Symbol string  `json:"symbol"`
	Safe   bool    `json:"safe,omitempty"`
	Weight float64 `json:"weight"`
	Reason string  `json:"reason"`
}

// rules describes each pre-registered allocator in one line, plus the test it
// applies to each risk symbol (the diagram's middle box).
var rules = map[string][2]string{
	"trend-filter-200":  {"Each equity ETF gets an equal share while it trades above its 200-day average; a share below it moves to the safe asset.", "close > 200-day average?"},
	"trend-ensemble":    {"Each equity ETF keeps the fraction of its equal share equal to how many of its 100/150/200/250-day averages it is above; the rest moves to the safe asset.", "above how many of 100/150/200/250-day averages?"},
	"dual-momentum-12m": {"The equity ETF with the best 12-month return takes 100% if it also beats the safe asset over 12 months; otherwise the safe asset takes 100%.", "best 12-month return, and beats the safe asset?"},
	"equal-weight":      {"Every ETF, the safe one included, holds an equal share, rebalanced monthly.", "always its equal share"},
	"inverse-vol-60":    {"Each ETF is weighted by 1 / its 60-day volatility, so calmer ETFs hold more.", "60-day volatility → weight ∝ 1/vol"},
}

// Step is one stage of the decision pipeline with its journal count.
type Step struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Count int64  `json:"count"`
	Note  string `json:"note,omitempty"`
}

func pipeline(c map[string]int64) []Step {
	return []Step{
		{"rules", "Rules decide targets", c["allocator.targets"], "deterministic strategy code, monthly"},
		{"proposed", "Suggestions proposed", c["suggestion.created"] + c["suggestion.rejected"], "orders to reach the targets"},
		{"thorn-propose", "Thorn said no (propose)", c["suggestion.rejected"], "risk limits before you see it"},
		{"review", "Claude reviewed", c["claude.review"], "advice only"},
		{"approved", "You approved", c["approval.accepted"], "passphrase on the approval page"},
		{"auto", "Paper training filled", c["order.auto_executed"], "rules suggestions only, paper book (ADR 0005)"},
		{"declined", "Declined", c["suggestion.declined"], ""},
		{"expired", "Expired unanswered", c["suggestion.expired"], "15-minute TTL"},
		{"thorn-execute", "Thorn said no (execute)", c["order.rejected"], "re-checked at fill time"},
		{"filled", "Paper fills", c["order.filled"], "journaled"},
	}
}

type chartsView struct {
	Generated  time.Time           `json:"generated"`
	Live       string              `json:"live"`
	Since      string              `json:"since"`
	AsOf       string              `json:"as_of"`
	Books      []shadow.BookSeries `json:"books"`
	Backtest   *BacktestSet        `json:"backtest,omitempty"`
	BTNote     string              `json:"backtest_note,omitempty"`
	Strategies []Strategy          `json:"strategies"`
	Pipeline   []Step              `json:"pipeline"`
	Note       string              `json:"note,omitempty"`
}

func (h *chartsHandler) json(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	live, err := h.liveData(r.Context())
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "charts unavailable: " + err.Error()})
		return
	}
	bt, btNote := h.backtestData(r.Context())
	v := chartsView{Generated: time.Now().UTC(), Live: live.Live, Since: live.Since, AsOf: live.AsOf,
		Books: live.Books, Backtest: bt, BTNote: btNote, Pipeline: pipeline(live.Counts)}
	if live.Since == "" {
		v.Note = "No shadow rebalance yet — the book charts start at the first rebalance window."
	}
	v.Strategies = strategies(live)
	_ = json.NewEncoder(w).Encode(v)
}

func strategies(live *LiveCharts) []Strategy {
	names := make([]string, 0, len(live.Decisions))
	for n := range live.Decisions {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { // live strategy first, then by name
		if (names[i] == live.Live) != (names[j] == live.Live) {
			return names[i] == live.Live
		}
		return names[i] < names[j]
	})
	safe := ""
	if n := len(live.Symbols); n > 0 {
		safe = live.Symbols[n-1]
	}
	out := make([]Strategy, 0, len(names))
	for _, n := range names {
		d := live.Decisions[n]
		r, ok := rules[n]
		if !ok {
			r = [2]string{"Pre-registered allocator.", "weight"}
		}
		s := Strategy{Name: n, Live: n == live.Live, Rule: r[0], Test: r[1]}
		syms := live.Symbols
		if len(syms) == 0 {
			for sym := range d.Targets {
				syms = append(syms, sym)
			}
			sort.Strings(syms)
		}
		for _, sym := range syms {
			s.Symbols = append(s.Symbols, Outcome{Symbol: sym, Safe: sym == safe, Weight: d.Targets[sym],
				Reason: strings.TrimSpace(d.Reasons[sym])})
		}
		out = append(out, s)
	}
	return out
}
