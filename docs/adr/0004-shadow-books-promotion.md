# ADR 0004: Shadow books, a pre-registered candidate set, and a promotion rule

**Status:** Accepted (2026-10-25), written before any shadow data exists
**Builds on:** [ADR 0002](0002-strategy-brain-approvals.md) (measured against a benchmark), `docs/backtests/`

## Context

The operator wants Tulip to "assess multiple strategies and ultimately get
better performance". The trap is overfitting: try enough variants on one
25-year history and one will win by luck. The backtests so far say no
candidate beats equal-weight hold risk-adjusted on the 4-ETF universe, and
the trend filter's case is drawdown, not return. More backtesting of the same
data can't settle what's "better"; only data no candidate was chosen on can.

The operator trades a **cash (non-registered) account**: every sell realizes
a taxable gain or loss, so turnover has a cost the backtests don't model.

## Decision

### 1. A fixed candidate set (`strategy.Candidates`)
For a universe whose last symbol is the safe asset (live: XIU, VFV, XEF; safe
ZAG):

| candidate | rule |
|---|---|
| `trend-filter-200` (live) | each equity 1/n while above its 200-day mean, else its share → safe |
| `equal-weight` | 1/n each, monthly rebalance |
| `trend-ensemble` | trend filter over 100/150/200/250-day means; each equity keeps the fraction of windows it's above |
| `dual-momentum-12m` | 100% in the best 12-month equity if it beats the safe asset, else 100% safe |
| `inverse-vol-60` | weights ∝ 1/(60-day volatility) |

The live XIU buy-and-hold benchmark is scored alongside. Candidates are not
tuned. **Honest caveat:** `trend-ensemble` was added after the 2026-10-24
sensitivity runs showed every window from 100 to 250 days works, which is mild
look-ahead; its first-day backtest is recorded in
`docs/backtests/2026-10-25-*`. Adding a candidate is a change to this ADR,
and its shadow clock starts at zero.

### 2. Shadow books
Every candidate, the live one included, trades a 10k paper book that nobody
approves. It runs on the live schedule (first 5 trading days of each month),
on bars completed before the month started, at that cycle's quotes with the
paper broker's 10 bps slippage, using the shared order code
(`strategy.Rebalance`). Each order passes Thorn with the live limits **except
the position-weight caps**: those are sized for the live strategy and would
be re-tuned if a candidate were promoted (otherwise dual momentum, 100% in one
ETF, would sit in cash and measure nothing). Allowlist, cash, no-shorting and
order-value rules all stand. Rejections are journaled.

Shadows write only `shadow.*` journal kinds and never call the broker or the
desk (pinned by `tools_test.go`). The live book's "no fill without a human
approval" is unchanged. The shadow of the live strategy measures the cost of
approval timing.

### 3. Scorecard
The `scorecard` MCP tool (read-only) scores every book from a **common start**
(the first shadow rebalance):
- return and max drawdown, from daily marks;
- trades and **sells** (taxable events);
- fees and realized P&L;
- each candidate's current targets and reasons.

### 4. Promotion rule (decided now, applied later)
A candidate may replace the live strategy only if **all** of these hold:
1. **≥ 12 months** of shadow data from the common start.
2. Over that period it beats the live strategy's shadow on return, with a max
   drawdown no worse than the live one's +5 points, **or** it has a clearly
   better return/drawdown ratio at a similar return.
3. It still beats the live strategy on the backtest holdout after $4.95
   commissions, and it is robust to its own parameter sensitivity.
4. Its sell count does not make the after-tax result worse. In a cash
   account, a small edge that needs twice the sells is not an edge.

Five candidates are being compared, so a narrow win is expected by chance. A
margin that would not survive picking the best of five is not a promotion.

**Tulip never switches strategy itself.** Promotion is the operator changing
`TULIP_STRATEGY` in git (Homelab `services/tulip/deployment.yaml`), with the
scorecard and backtest attached to the commit.

## Consequences
- In the first months the scorecard is mostly noise; it says so.
- Cost: a few journal events per book per trading day; history fetches are
  shared with the live rebalance (one per symbol per cycle).
- `TULIP_SHADOW=off` disables shadow books.
