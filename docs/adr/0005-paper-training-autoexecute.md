# ADR 0005: Paper training — the rules' suggestions fill the paper book without approval

**Status:** Accepted (2026-10-27), operator decision
**Amends:** [ADR 0002 §3](0002-strategy-brain-approvals.md) for the **paper** book only
**Relates to:** [ADR 0003](0003-questrade-copilot.md) (co-pilot), [ADR 0004](0004-shadow-books-promotion.md) (shadow books)

## Context

The operator places every real order themselves (ADR 0003: Questrade's retail
API is read-only). Approving *paper* orders adds friction without protecting
any money, and approvals are not yet enabled (no sealed passphrase), so the
live paper book would sit in cash while every suggestion expired. The operator
asked (2026-10-27): run paper as a **training phase without approval**; turn
approvals on later, when they are ready, so Tulip records and tracks what was
actually traded.

## Decision

1. **`TULIP_AUTOEXECUTE=paper`** (operator-set, in the Homelab manifest) lets
   the loop fill the paper strategy book's suggestions immediately. Any other
   non-empty value refuses to start; so does `paper` with `-mode copilot`.
2. **Only the rules' own suggestions** (`source == "rules"`, proposed by the
   loop) auto-execute. Proposals from the MCP token (`claude-code`) or the
   brain stay pending for a human, exactly as before — a proposal can never
   approve itself. There is no HTTP route or MCP tool for it (`tools_test`).
3. **Same execution path as an approval:** `Desk.AutoExecute` claims the
   suggestion single-use (`approvals_used`) and runs the shared execution
   checks — fresh quote, price band, Thorn again (kill switch, caps, daily
   loss). It refuses in co-pilot mode even if the flag were set.
4. **Journaled as `order.auto_executed` (`by: paper-training`)**, never
   `approval.accepted`; `service.started` carries `auto_execute`. The training
   period stays separable from the approved period in every later analysis.
5. **No approval page for these:** the ntfy approval link is not sent for an
   auto-filled suggestion (the monthly scorecard push still reports).
6. **Rebalance in one cycle:** after sells fill, the cycle re-plans once so
   buys are sized from the cash the sells freed.

## Consequences

- The live strategy book now trades monthly like `shadow:trend-filter-200`;
  the two differ only by Thorn's live caps (equities ≤ 40%, order value) and
  slippage — expect near-twins on the scorecard.
- The approval boundary for agents is unchanged: Claude Code still never
  reads `tulip-approval*`, calls `/approve` or `/killswitch`, and never
  enables auto-execution beyond paper or for other sources.
- **Leaving training** = remove `TULIP_AUTOEXECUTE`. Then either approvals
  (sealed passphrase) for paper, or co-pilot mode (ADR 0003) to record and
  reconcile what the operator actually traded in Questrade.
