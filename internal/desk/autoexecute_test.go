package desk

import (
	"context"
	"errors"
	"testing"

	"github.com/Blacklotus89898/Tulip/internal/portfolio"
)

// Paper training (ADR 0005): the rules' own suggestions fill without an
// approval; nothing else does.

func TestAutoExecuteFillsRulesSuggestionsOnce(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.d.AutoPaper = true
	notified := 0
	h.d.Notify = func(Suggestion) { notified++ }
	s := h.proposeBuy(t, 10)
	if notified != 0 {
		t.Errorf("an auto-filled suggestion must not page an approval link (notified %d)", notified)
	}

	fill, err := h.d.AutoExecute(ctx, s.ID)
	if err != nil {
		t.Fatalf("auto-execute: %v", err)
	}
	if fill.Qty != 10 || fill.SuggestionID != s.ID {
		t.Errorf("fill = %+v", fill)
	}
	if st, _ := h.d.LoadState(ctx); st.Books[portfolio.Strategy].Held("XIU.TO") != 10 || len(st.Pending) != 0 {
		t.Errorf("held=%d pending=%d", st.Books[portfolio.Strategy].Held("XIU.TO"), len(st.Pending))
	}
	// The journal keeps training fills apart from human approvals.
	if n := len(kinds(t, h, KindAutoExecuted)); n != 1 {
		t.Errorf("%s journaled %d times, want 1", KindAutoExecuted, n)
	}
	if n := len(kinds(t, h, KindApprovalAccepted)); n != 0 {
		t.Errorf("auto-execution must never journal %s (got %d)", KindApprovalAccepted, n)
	}
	if _, err := h.d.AutoExecute(ctx, s.ID); !errors.Is(err, ErrNotPending) {
		t.Errorf("a second auto-execute must fail, got %v", err)
	}
}

func TestAutoExecuteRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("off by default", func(t *testing.T) {
		h := newHarness(t)
		s := h.proposeBuy(t, 5)
		if _, err := h.d.AutoExecute(ctx, s.ID); !errors.Is(err, ErrAutoExecute) {
			t.Fatalf("got %v", err)
		}
		if st, _ := h.d.LoadState(ctx); len(st.Pending) != 1 {
			t.Errorf("must stay pending, pending=%d", len(st.Pending))
		}
	})
	// A proposal from the MCP token (Claude Code) or the brain must never
	// approve itself — it waits for the operator like before.
	for _, src := range []string{"claude-code", "brain"} {
		t.Run("source "+src, func(t *testing.T) {
			h := newHarness(t)
			h.d.AutoPaper = true
			s, _, err := h.d.Propose(ctx, "c1", ProposeRequest{Rationale: testWhy, Symbol: "XIU.TO", Side: "buy", Qty: 5, Source: src})
			if err != nil || s == nil {
				t.Fatalf("propose: %v", err)
			}
			if _, err := h.d.AutoExecute(ctx, s.ID); !errors.Is(err, ErrAutoExecute) {
				t.Fatalf("got %v", err)
			}
			if st, _ := h.d.LoadState(ctx); len(st.Pending) != 1 || st.Books[portfolio.Strategy].Held("XIU.TO") != 0 {
				t.Errorf("must stay pending and unfilled")
			}
		})
	}
	t.Run("never in co-pilot mode", func(t *testing.T) {
		h := newHarness(t)
		s := h.proposeBuy(t, 5)
		h.d.AutoPaper = true
		h.d.Account = &fakeAccount{}
		if _, err := h.d.AutoExecute(ctx, s.ID); !errors.Is(err, ErrAutoExecute) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("kill switch still stops it", func(t *testing.T) {
		h := newHarness(t)
		h.d.AutoPaper = true
		s := h.proposeBuy(t, 5)
		if err := h.d.ArmKillSwitch(ctx, "test", "stop"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.d.AutoExecute(ctx, s.ID); err == nil {
			t.Fatal("Thorn must refuse at execution with the kill switch armed")
		}
		if st, _ := h.d.LoadState(ctx); st.Books[portfolio.Strategy].Held("XIU.TO") != 0 {
			t.Error("filled despite the kill switch")
		}
	})
}
