package desk

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/approval"
	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/journal"
	"github.com/Blacklotus89898/Tulip/internal/marketdata"
	"github.com/Blacklotus89898/Tulip/internal/portfolio"
	"github.com/Blacklotus89898/Tulip/internal/strategy"
)

const pass = "a long operator passphrase"

var (
	hashOnce sync.Once
	passHash string
)

func testHash(t *testing.T) string {
	hashOnce.Do(func() {
		h, err := approval.HashPassphrase(pass)
		if err != nil {
			t.Fatal(err)
		}
		passHash = h
	})
	return passHash
}

type fakeMD struct {
	mu     sync.Mutex
	quotes map[string]marketdata.Quote
}

func (f *fakeMD) Daily(context.Context, string) ([]marketdata.Candle, error) { return nil, nil }
func (f *fakeMD) Quote(_ context.Context, sym string) (marketdata.Quote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q, ok := f.quotes[sym]
	if !ok {
		return q, marketdata.ErrUnavailable
	}
	return q, nil
}
func (f *fakeMD) set(sym string, px float64, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quotes[sym] = marketdata.Quote{Symbol: sym, Price: px, Time: at}
}

// Monday 2026-10-26 11:00 Toronto — the session is open.
var monday = time.Date(2026, 10, 26, 11, 0, 0, 0, calendar.Toronto)

type harness struct {
	d   *Desk
	md  *fakeMD
	now *time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	j, err := journal.Open(context.Background(), filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	now := monday
	md := &fakeMD{quotes: map[string]marketdata.Quote{}}
	md.set("XIU.TO", 50, now.Add(-15*time.Minute)) // ~15 min delayed, like Yahoo
	d := &Desk{J: j, MD: md, PassphraseHash: testHash(t), Limiter: &approval.Limiter{},
		Now: func() time.Time { return now }}
	if err := d.EnsureFunded(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &harness{d: d, md: md, now: &now}
}

func (h *harness) proposeBuy(t *testing.T, qty int) *Suggestion {
	t.Helper()
	cand := &strategy.Candidate{Strategy: "momentum-v1", Symbol: "XIU.TO", Side: strategy.Buy, Qty: qty, RefPrice: 50, SignalOn: "2026-10-23"}
	s, v, err := h.d.Propose(context.Background(), "c1", ProposeRequest{Rationale: testWhy, Symbol: "XIU.TO", Side: "buy", Qty: qty, Source: "rules", Candidate: cand})
	if err != nil || s == nil {
		t.Fatalf("propose: (%v, %+v, %v)", s, v, err)
	}
	return s
}

var who = Approver{IP: "192.168.0.50", UserAgent: "test"}

func TestApproveHappyPathFillsOnceAndOnlyOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.proposeBuy(t, 10)

	fill, err := h.d.Approve(ctx, s.ID, s.Hash, pass, who)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if fill.Qty != 10 || fill.Price <= 50 || fill.SuggestionID != s.ID {
		t.Errorf("fill = %+v (buys pay slippage)", fill)
	}
	st, _ := h.d.LoadState(ctx)
	if st.Books[portfolio.Strategy].Held("XIU.TO") != 10 || len(st.Pending) != 0 {
		t.Errorf("after approval: held=%d pending=%d", st.Books[portfolio.Strategy].Held("XIU.TO"), len(st.Pending))
	}
	if _, err := h.d.Approve(ctx, s.ID, s.Hash, pass, who); !errors.Is(err, ErrNotPending) {
		t.Errorf("replay must fail, got %v", err)
	}
}

func TestApproveRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("wrong passphrase keeps it pending", func(t *testing.T) {
		h := newHarness(t)
		s := h.proposeBuy(t, 5)
		if _, err := h.d.Approve(ctx, s.ID, s.Hash, "not the passphrase!!", who); !errors.Is(err, ErrBadPassphrase) {
			t.Fatalf("got %v", err)
		}
		if _, err := h.d.Approve(ctx, s.ID, s.Hash, pass, who); err != nil {
			t.Errorf("the right passphrase still works afterwards: %v", err)
		}
	})
	t.Run("displayed terms changed", func(t *testing.T) {
		h := newHarness(t)
		s := h.proposeBuy(t, 5)
		if _, err := h.d.Approve(ctx, s.ID, "deadbeef", pass, who); !errors.Is(err, ErrChanged) {
			t.Errorf("got %v", err)
		}
	})
	t.Run("market closed keeps it pending", func(t *testing.T) {
		h := newHarness(t)
		h.d.Cfg.TTL = 24 * time.Hour // isolate the market-hours rule from expiry
		s := h.proposeBuy(t, 5)
		*h.now = monday.Add(7 * time.Hour) // 18:00, after the close
		if _, err := h.d.Approve(ctx, s.ID, s.Hash, pass, who); !errors.Is(err, ErrMarketClosed) {
			t.Fatalf("got %v", err)
		}
		st, _ := h.d.LoadState(ctx)
		if len(st.Pending) != 1 {
			t.Error("a market-closed refusal must not consume the suggestion")
		}
	})
	t.Run("expired", func(t *testing.T) {
		h := newHarness(t)
		s := h.proposeBuy(t, 5)
		*h.now = monday.Add(31 * time.Minute)
		if _, err := h.d.Approve(ctx, s.ID, s.Hash, pass, who); !errors.Is(err, ErrExpired) {
			t.Errorf("got %v", err)
		}
	})
	t.Run("approvals disabled without a hash", func(t *testing.T) {
		h := newHarness(t)
		s := h.proposeBuy(t, 5)
		h.d.PassphraseHash = ""
		if _, err := h.d.Approve(ctx, s.ID, s.Hash, pass, who); !errors.Is(err, ErrApprovalsDisabled) {
			t.Errorf("got %v", err)
		}
	})
}

func TestExecutionChecksRejectAfterApproval(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		setup  func(h *harness)
		reason string
	}{
		"price left the band": {func(h *harness) { h.md.set("XIU.TO", 53, monday.Add(-5*time.Minute)) }, "price moved"},
		"stale quote":         {func(h *harness) { h.md.set("XIU.TO", 50, monday.Add(-2*time.Hour)) }, "old"},
		"quote not newer than the signal bar": {func(h *harness) {
			// a quote from Friday 15:30 — before the signal bar's 16:00 close
			h.md.set("XIU.TO", 50, time.Date(2026, 10, 23, 15, 30, 0, 0, calendar.Toronto))
			h.d.Cfg.MaxQuoteAge = 100 * time.Hour
		}, "signal bar"},
		"kill switch armed after proposal": {func(h *harness) {
			_ = h.d.ArmKillSwitch(context.Background(), "test", "panic button")
		}, "kill switch"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			s := h.proposeBuy(t, 5)
			c.setup(h)
			_, err := h.d.Approve(ctx, s.ID, s.Hash, pass, who)
			if err == nil || !strings.Contains(err.Error(), c.reason) {
				t.Fatalf("expected a rejection mentioning %q, got %v", c.reason, err)
			}
			st, _ := h.d.LoadState(ctx)
			if st.Books[portfolio.Strategy].Fills != 0 || len(st.Pending) != 0 {
				t.Errorf("rejected at execution: no fill, not pending (fills=%d pending=%d)", st.Books[portfolio.Strategy].Fills, len(st.Pending))
			}
		})
	}
}

func TestProposeThornRejectionIsJournaled(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.md.set("GME", 20, monday)
	s, v, err := h.d.Propose(ctx, "c1", ProposeRequest{Rationale: testWhy, Symbol: "GME", Side: "buy", Qty: 1, Source: "claude-code"})
	if err != nil || s != nil || v.Allowed {
		t.Fatalf("GME must be rejected by Thorn: (%v, %+v, %v)", s, v, err)
	}
	evs, _ := h.d.J.List(ctx, journal.Query{Kind: KindSuggestionRejected})
	if len(evs) != 1 {
		t.Errorf("Thorn rejections are journaled, got %d", len(evs))
	}
}

func TestDeclineExpireKillSwitch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.proposeBuy(t, 5)
	if err := h.d.Decline(ctx, a.ID, "claude-code", "changed my mind"); err != nil {
		t.Fatal(err)
	}
	b := h.proposeBuy(t, 5)
	*h.now = monday.Add(time.Hour)
	if n, _ := h.d.ExpireDue(ctx); n != 1 {
		t.Errorf("expired %d, want 1 (%s)", n, b.ID)
	}
	_ = h.d.ArmKillSwitch(ctx, "claude-code", "test")
	if err := h.d.DisarmKillSwitch(ctx, "wrong passphrase!!", who); !errors.Is(err, ErrBadPassphrase) {
		t.Errorf("disarm needs the passphrase: %v", err)
	}
	if err := h.d.DisarmKillSwitch(ctx, pass, who); err != nil {
		t.Fatal(err)
	}
	st, _ := h.d.LoadState(ctx)
	if st.KillSwitch || len(st.Pending) != 0 {
		t.Errorf("state = kill:%v pending:%d", st.KillSwitch, len(st.Pending))
	}
}

func TestBenchmarkAndMarks(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	f, err := h.d.InitBenchmark(ctx)
	if err != nil || f == nil || f.Book != portfolio.Benchmark || f.Qty != 199 {
		t.Fatalf("benchmark fill = (%+v, %v) — 10000 / (50×1.002) = 199 shares", f, err)
	}
	if again, _ := h.d.InitBenchmark(ctx); again != nil {
		t.Error("the benchmark buys once")
	}
	if err := h.d.Mark(ctx); err != nil {
		t.Fatal(err)
	}
	_ = h.d.Mark(ctx)
	marks, _ := h.d.J.ByKinds(ctx, KindMark)
	if len(marks) != 2 {
		t.Errorf("one mark per book per day, got %d", len(marks))
	}
}

func TestReviewIsAdviceOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.proposeBuy(t, 10)
	if err := h.d.AddReview(ctx, Review{SuggestionID: s.ID, Verdict: "resize", Qty: 5, Confidence: 0.7,
		Rationale: "trend is young", By: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]Review{
		"bad verdict":     {SuggestionID: s.ID, Verdict: "approve", Confidence: 0.5, Rationale: "x"},
		"no rationale":    {SuggestionID: s.ID, Verdict: "agree", Confidence: 0.5},
		"resize no qty":   {SuggestionID: s.ID, Verdict: "resize", Confidence: 0.5, Rationale: "x"},
		"confidence > 1":  {SuggestionID: s.ID, Verdict: "agree", Confidence: 2, Rationale: "x"},
		"unknown pending": {SuggestionID: "sug_nope", Verdict: "agree", Confidence: 0.5, Rationale: "x"},
	} {
		if err := h.d.AddReview(ctx, bad); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	rs, _ := h.d.Reviews(ctx, s.ID)
	if len(rs) != 1 || rs[0].Verdict != "resize" {
		t.Errorf("reviews = %+v", rs)
	}
	// Advice only: still pending, same qty, nothing filled.
	st, _ := h.d.LoadState(ctx)
	if len(st.Pending) != 1 || st.Pending[0].Qty != 10 || st.Books["strategy"].Fills != 0 {
		t.Errorf("a review must not change the suggestion: %+v", st.Pending)
	}
}

const testWhy = "test: a justification long enough to pass"

func TestProposeRequiresAJustification(t *testing.T) {
	h := newHarness(t)
	for _, why := range []string{"", "   ", "too short"} {
		s, _, err := h.d.Propose(context.Background(), "c1", ProposeRequest{Symbol: "XIU.TO", Side: "buy", Qty: 1, Source: "claude-code", Rationale: why})
		if !errors.Is(err, ErrNoJustification) || s != nil {
			t.Errorf("rationale %q: want ErrNoJustification, got (%v, %v)", why, s, err)
		}
	}
}
