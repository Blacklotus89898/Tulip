// Package loop is Tulip's autonomy (ADR 0001 §1): wake on schedule during TSX
// sessions, observe, let the rules (and later the brain) propose, gate, and
// suggest. It never executes — execution only follows a human approval.
package loop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/desk"
	"github.com/Blacklotus89898/Tulip/internal/marketdata"
	"github.com/Blacklotus89898/Tulip/internal/portfolio"
	"github.com/Blacklotus89898/Tulip/internal/shadow"
	"github.com/Blacklotus89898/Tulip/internal/strategy"
)

// Reviewer is the brain's seam (ADR 0002 §1.2): it sees a candidate and may
// approve, veto or resize it, with a confidence. nil = rules-only.
type Reviewer interface {
	Review(ctx context.Context, cycle string, c strategy.Candidate, book *portfolio.Book) (Verdict, error)
}

// Verdict is the brain's answer for one candidate.
type Verdict struct {
	Decision   string  `json:"decision"` // approve | veto | resize
	Qty        int     `json:"qty"`
	Confidence float64 `json:"confidence"`
	Rationale  string  `json:"rationale"`
}

// Loop wires the pieces.
type Loop struct {
	Desk     *desk.Desk
	MD       marketdata.Source
	Strategy strategy.Momentum
	Symbols  []string // what the rules scan (subset of Thorn's allowlist)
	Brain    Reviewer // optional
	// Allocator, when set, replaces the per-symbol strategy: a monthly
	// rebalance towards its targets (backtested: Tulip docs/backtests).
	Allocator strategy.Allocator
	Hist      HistorySource // adjusted history for the allocator
	Band      float64       // rebalance drift band (default 0.03)
	// Shadow, when set, runs the pre-registered candidates as unapproved
	// paper books on the same schedule (ADR 0004).
	Shadow *shadow.Runner
	// Report sends the monthly scorecard push (title, body); nil = off.
	Report func(ctx context.Context, title, body string) error

	cycleMu sync.Mutex // the schedule and MCP cycle_run must not interleave
	Log     *slog.Logger
	Now     func() time.Time
	// RunAt are the Toronto wall-clock cycle times on trading days.
	RunAt []string // default 10:05, 15:00
}

func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func newCycleID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "cyc_" + hex.EncodeToString(b)
}

// Result summarises one cycle.
type Result struct {
	Cycle       string   `json:"cycle"`
	MarketOpen  bool     `json:"market_open"`
	Candidates  int      `json:"candidates"`
	Suggestions []string `json:"suggestions"`
	Rejected    int      `json:"rejected"`
	Unavailable []string `json:"unavailable,omitempty"`
}

// Cycle runs observe → propose once. Safe to call any time: outside market
// hours it only expires and marks nothing new.
func (l *Loop) Cycle(ctx context.Context, trigger string) (Result, error) {
	l.cycleMu.Lock()
	defer l.cycleMu.Unlock()
	cycle := newCycleID()
	j := l.Desk.J
	open, calErr := calendar.IsOpen(l.now())
	res := Result{Cycle: cycle, MarketOpen: open}
	_, _ = j.Append(ctx, "cycle.started", cycle, map[string]any{"trigger": trigger, "market_open": open})
	defer func() { _, _ = j.Append(ctx, "cycle.finished", cycle, res) }()

	if err := l.Desk.EnsureFunded(ctx); err != nil {
		return res, err
	}
	if _, err := l.Desk.ExpireDue(ctx); err != nil {
		return res, err
	}
	if calErr != nil {
		_, _ = j.Append(ctx, "calendar.unknown", cycle, map[string]string{"error": calErr.Error()})
	}
	if !open {
		return res, nil
	}
	if _, err := l.Desk.InitBenchmark(ctx); err != nil {
		_, _ = j.Append(ctx, "data.unavailable", cycle, map[string]string{"symbol": "benchmark", "error": err.Error()})
	}
	_ = l.Desk.Mark(ctx)
	hist := newMemoHist(l.Hist)
	if l.Shadow != nil {
		// After the live strategy (deferred), sharing its history fetches.
		defer l.shadowCycle(ctx, cycle, hist)
	}
	if l.Allocator != nil {
		return res, l.rebalance(ctx, cycle, hist, &res)
	}

	// Sizing comes from the real account in co-pilot mode (ADR 0003): if it
	// can't be read, propose nothing.
	book, equity, err := l.Desk.SizingBook(ctx, cycle)
	if err != nil {
		res.Unavailable = append(res.Unavailable, "account")
		return res, nil
	}
	for _, sym := range l.Symbols {
		bars, err := l.MD.Daily(ctx, sym)
		if err != nil {
			res.Unavailable = append(res.Unavailable, sym)
			_, _ = j.Append(ctx, "data.unavailable", cycle, map[string]string{"symbol": sym, "error": err.Error()})
			continue
		}
		cand, err := l.Strategy.Evaluate(sym, bars, book.Held(sym), equity)
		if err != nil {
			_, _ = j.Append(ctx, "strategy.skipped", cycle, map[string]string{"symbol": sym, "error": err.Error()})
			continue
		}
		if cand == nil {
			continue
		}
		res.Candidates++
		_, _ = j.Append(ctx, "candidate", cycle, cand)

		req := desk.ProposeRequest{Symbol: cand.Symbol, Side: string(cand.Side), Qty: cand.Qty,
			Source: "rules", Rationale: cand.Reason, Candidate: cand}
		if l.Brain != nil {
			v, err := l.Brain.Review(ctx, cycle, *cand, book)
			if err != nil {
				// Fail closed: a broken brain yields no proposal, never a blind one.
				_, _ = j.Append(ctx, "brain.error", cycle, map[string]string{"symbol": sym, "error": err.Error()})
				continue
			}
			_, _ = j.Append(ctx, "brain.verdict", cycle, map[string]any{"symbol": sym, "verdict": v, "candidate_qty": cand.Qty})
			if v.Decision == "veto" {
				continue
			}
			if v.Decision == "resize" && v.Qty > 0 {
				req.Qty = v.Qty
			}
			conf := v.Confidence
			req.Confidence, req.Source, req.Rationale = &conf, "brain", cand.Reason+" — brain: "+v.Rationale
		}
		s, _, err := l.Desk.Propose(ctx, cycle, req)
		switch {
		case err != nil:
			_, _ = j.Append(ctx, "propose.error", cycle, map[string]string{"symbol": sym, "error": err.Error()})
		case s == nil:
			res.Rejected++
		default:
			res.Suggestions = append(res.Suggestions, s.ID)
		}
	}
	return res, nil
}

// Run blocks until ctx ends: every minute it expires suggestions, and at the
// RunAt times on trading days it runs a cycle (once per slot per day).
func (l *Loop) Run(ctx context.Context) {
	runAt := l.RunAt
	if len(runAt) == 0 {
		runAt = []string{"10:05", "15:00"}
	}
	done := map[string]bool{}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_, _ = l.Desk.ExpireDue(ctx)
		now := l.now().In(calendar.Toronto)
		slot := now.Format("15:04")
		for _, at := range runAt {
			key := now.Format("2006-01-02") + " " + at
			if slot != at || done[key] {
				continue
			}
			done[key] = true
			if ok, err := calendar.IsTradingDay(now); err != nil || !ok {
				continue
			}
			res, err := l.Cycle(ctx, "schedule "+at)
			if err != nil && !errors.Is(err, context.Canceled) && l.Log != nil {
				l.Log.Error("cycle failed", "cycle", res.Cycle, "err", err)
			} else if l.Log != nil {
				l.Log.Info("cycle", "cycle", res.Cycle, "candidates", res.Candidates, "suggestions", len(res.Suggestions))
			}
		}
	}
}
