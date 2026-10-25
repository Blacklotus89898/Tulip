package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/desk"
	"github.com/Blacklotus89898/Tulip/internal/journal"
	"github.com/Blacklotus89898/Tulip/internal/loop"
	"github.com/Blacklotus89898/Tulip/internal/mcp"
	"github.com/Blacklotus89898/Tulip/internal/shadow"
)

// tools is the MCP supervision surface for Claude Code (ADR 0002 §3). Every
// tool either reads, proposes (→ Thorn → a pending suggestion), or REDUCES
// risk (decline, arm the kill switch). None approves, executes or disarms —
// tools_test.go enforces that list.
func tools(d *desk.Desk, lp *loop.Loop, j *journal.Journal) []mcp.Tool {
	obj := func(props map[string]any, required ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	str := map[string]any{"type": "string"}
	pretty := func(v any) (string, error) {
		b, err := json.MarshalIndent(v, "", "  ")
		return string(b), err
	}
	return []mcp.Tool{
		{
			Name:        "tulip_status",
			Description: "Mode, version, market open, kill switch, pending suggestions and journal counts.",
			Call: func(ctx context.Context, _ json.RawMessage) (string, error) {
				st, err := d.LoadState(ctx)
				if err != nil {
					return "", err
				}
				counts, _ := j.Counts(ctx)
				open, calErr := calendar.IsOpen(d.Now0())
				mode, approvalHow := "paper", "human-only (passphrase on the approval page)"
				if d.CoPilot() {
					mode, approvalHow = "copilot", "the operator places every order in Questrade; Tulip only reads the account"
				}
				out := map[string]any{
					"version": version, "mode": mode, "approval": approvalHow, "strategy": lp.StrategyName(),
					"market_open": open, "kill_switch": st.KillSwitch, "kill_reason": st.KillReason,
					"pending": len(st.Pending), "approvals_enabled": d.PassphraseHash != "",
					"brain": lp.Brain != nil, "journal_events": counts,
				}
				if calErr != nil {
					out["calendar"] = calErr.Error()
				}
				return pretty(out)
			},
		},
		{
			Name:        "journal_list",
			Description: "Read journal events, newest first. Filters: kind, cycle; limit (default 50, max 500); after_id to tail oldest-first.",
			InputSchema: obj(map[string]any{"kind": str, "cycle": str,
				"limit": map[string]any{"type": "integer"}, "after_id": map[string]any{"type": "integer"}}),
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Kind, Cycle string
					Limit       int   `json:"limit"`
					AfterID     int64 `json:"after_id"`
				}
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("bad arguments: %w", err)
				}
				evs, err := j.List(ctx, journal.Query{Kind: a.Kind, Cycle: a.Cycle, Limit: a.Limit, AfterID: a.AfterID})
				if err != nil {
					return "", err
				}
				return pretty(evs)
			},
		},
		{
			Name:        "portfolio",
			Description: "Strategy and benchmark books (cash, positions, realized P&L), marked at cost — compare them to judge the strategy.",
			Call: func(ctx context.Context, _ json.RawMessage) (string, error) {
				st, err := d.LoadState(ctx)
				if err != nil {
					return "", err
				}
				out := map[string]any{}
				for name, b := range st.Books {
					out[name] = map[string]any{"book": b, "equity_at_cost": b.Equity(nil)}
				}
				return pretty(out)
			},
		},
		{
			Name: "broker_account",
			Description: "Co-pilot mode (ADR 0003): a fresh READ-ONLY snapshot of the one Questrade account (CAD cash, " +
				"equity, whole-share holdings) and the strategy's attributed trades (suggestions the operator acted on, " +
				"with slippage). Paper mode: says so. Never places or changes anything.",
			Call: func(ctx context.Context, _ json.RawMessage) (string, error) {
				if !d.CoPilot() {
					return pretty(map[string]any{"mode": "paper", "note": "no broker connected — see the portfolio tool"})
				}
				out := map[string]any{"mode": "copilot"}
				if snap, err := d.Account.Snapshot(ctx); err != nil {
					out["snapshot_error"] = err.Error()
				} else {
					out["snapshot"] = snap
				}
				attributed, err := d.Attributed(ctx)
				if err != nil {
					return "", err
				}
				out["attributed_trades"] = attributed
				return pretty(out)
			},
		},
		{
			Name: "rebalance_preview",
			Description: "READ-ONLY: what the allocator (default trend-filter-200) would target from today's completed " +
				"bars, the book's current weights, and the orders that would get there. Proposes nothing — the live " +
				"rebalance runs itself in the first 5 trading days of each month.",
			Call: func(ctx context.Context, _ json.RawMessage) (string, error) {
				p, unavailable, err := lp.Preview(ctx)
				if err != nil {
					return "", err
				}
				if p == nil {
					return pretty(map[string]any{"error": "data unavailable — failing closed", "unavailable": unavailable})
				}
				return pretty(p)
			},
		},
		{
			Name: "scorecard",
			Description: "READ-ONLY: every book compared from a common start — the live paper book, the XIU benchmark and " +
				"the shadow book of each pre-registered candidate (ADR 0004): return, max drawdown, trades, sells " +
				"(taxable in a cash account), fees, realized P&L — plus what each candidate would hold on today's " +
				"completed bars, with its reasons. Promotion is the operator's decision (12 months + backtest gate).",
			Call: func(ctx context.Context, _ json.RawMessage) (string, error) {
				card, err := shadow.Build(ctx, j)
				if err != nil {
					return "", err
				}
				decisions, asOf, unavailable, err := lp.CandidateDecisions(ctx)
				if err != nil {
					return "", err
				}
				return pretty(map[string]any{"live_strategy": lp.StrategyName(), "scorecard": card,
					"candidates_now": decisions, "signals_as_of": asOf, "unavailable": unavailable})
			},
		},
		{
			Name:        "suggestions_pending",
			Description: "Suggestions waiting for the operator (paper: to approve; co-pilot: to place in Questrade), with their page path.",
			Call: func(ctx context.Context, _ json.RawMessage) (string, error) {
				st, err := d.LoadState(ctx)
				if err != nil {
					return "", err
				}
				type row struct {
					desk.Suggestion
					ApprovePage string `json:"approve_page"`
				}
				out := []row{}
				for _, s := range st.Pending {
					out = append(out, row{s, "/approve/" + s.ID})
				}
				return pretty(out)
			},
		},
		{
			Name: "propose",
			Description: "Propose a paper trade. It goes through Thorn (allowlist, caps, cash, loss limit, kill switch); " +
				"if allowed it becomes a PENDING suggestion and the operator is paged. You cannot approve it.",
			InputSchema: obj(map[string]any{
				"symbol": str, "side": map[string]any{"type": "string", "enum": []string{"buy", "sell"}},
				"qty": map[string]any{"type": "integer", "minimum": 1}, "rationale": str,
				"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			}, "symbol", "side", "qty", "rationale"),
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Symbol, Side, Rationale string
					Qty                     int      `json:"qty"`
					Confidence              *float64 `json:"confidence"`
				}
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("bad arguments: %w", err)
				}
				s, v, err := d.Propose(ctx, "", desk.ProposeRequest{Symbol: a.Symbol, Side: a.Side, Qty: a.Qty,
					Source: "claude-code", Rationale: a.Rationale, Confidence: a.Confidence})
				if err != nil {
					return "", err
				}
				if s == nil {
					return pretty(map[string]any{"created": false, "thorn": v})
				}
				return pretty(map[string]any{"created": true, "suggestion": s, "approve_page": "/approve/" + s.ID,
					"note": "pending — only the operator can approve it"})
			},
		},
		{
			Name: "review_suggestion",
			Description: "Record your review of a pending suggestion: verdict agree|disagree|resize (with qty), confidence 0..1 " +
				"and rationale. Advice only — it is shown to the operator on the approval page and journaled for " +
				"measuring whether your reviews beat the plain rules. It never approves or changes the suggestion.",
			InputSchema: obj(map[string]any{
				"id": str, "verdict": map[string]any{"type": "string", "enum": []string{"agree", "disagree", "resize"}},
				"qty":        map[string]any{"type": "integer", "minimum": 1},
				"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1}, "rationale": str,
			}, "id", "verdict", "confidence", "rationale"),
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					ID, Verdict, Rationale string
					Qty                    int     `json:"qty"`
					Confidence             float64 `json:"confidence"`
				}
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("bad arguments: %w", err)
				}
				if err := d.AddReview(ctx, desk.Review{SuggestionID: a.ID, Verdict: a.Verdict, Qty: a.Qty,
					Confidence: a.Confidence, Rationale: a.Rationale, By: "claude-code"}); err != nil {
					return "", err
				}
				return "review recorded for " + a.ID + " — the operator sees it on the approval page", nil
			},
		},
		{
			Name:        "decline",
			Description: "Decline a pending suggestion (reduces risk; no approval needed).",
			InputSchema: obj(map[string]any{"id": str, "reason": str}, "id", "reason"),
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct{ ID, Reason string }
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("bad arguments: %w", err)
				}
				if err := d.Decline(ctx, a.ID, "claude-code", a.Reason); err != nil {
					return "", err
				}
				return "declined " + a.ID, nil
			},
		},
		{
			Name:        "kill_switch_arm",
			Description: "Arm the kill switch: nothing executes until the operator disarms it with their passphrase.",
			InputSchema: obj(map[string]any{"reason": str}, "reason"),
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct{ Reason string }
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("bad arguments: %w", err)
				}
				if err := d.ArmKillSwitch(ctx, "claude-code", a.Reason); err != nil {
					return "", err
				}
				return "kill switch armed (disarm: the operator, at /killswitch)", nil
			},
		},
		{
			Name:        "cycle_run",
			Description: "Run one observe → rules → Thorn → suggest cycle now (only creates suggestions; outside TSX hours it only expires stale ones). With an allocator (default trend-filter-200) it works on the month's rebalance during the first 5 trading days.",
			Call: func(ctx context.Context, _ json.RawMessage) (string, error) {
				res, err := lp.Cycle(ctx, "mcp")
				if err != nil {
					return "", err
				}
				return pretty(res)
			},
		},
	}
}
