# Tulip 🌷

Human-approved trading agent. **Claude proposes; Thorn disposes; the journal
remembers.**

Named after the [tulip mania](https://en.wikipedia.org/wiki/Tulip_mania) of
1637 — history's first speculative bubble — as permanent humility for a
money-touching service. Sibling to [Petal](https://github.com/Blacklotus89898/Petal)
(the platform's pipeline orchestrator): Petal builds, Tulip trades.

Autonomous in observation and judgment, human in execution: Tulip runs the
loop itself (watch markets → ask Claude → gate proposals), but **no order
moves without a thumbs-up** delivered to your phone.

## Architecture

```
                 ┌──────────────────── Tulip (one Go service) ────────────────────┐
   ticker ──────►│ loop      wake on schedule (market hours)                      │
                 │ observe   portfolio + watchlist via marketdata interface       │
                 │ think     Claude (Anthropic API) → structured proposals        │
                 │ thorn     deterministic gate: allowlist · caps · loss limit ·  │
                 │           confidence floor · kill switch      (internal/thorn)│
                 │ suggest   journal (pending, TTL) ──► ntfy [Approve] [Decline] │
                 │ execute   human-approved orders via broker interface           │
                 │ journal   append-only SQLite — every step of every cycle       │
                 │ mcp       supervision surface: portfolio, journal, decisions,  │
                 │           kill switch — the desk-side window in                │
                 └────────────────────────────────────────────────────────────────┘
```

The model **never** touches a broker: Claude returns proposals, Thorn (plain
Go) enforces the risk rules, a human approves, the broker interface executes.
Full reasoning in [ADR 0001](docs/adr/0001-architecture.md).

## Operating it (v1, paper)

- **Strategy:** `trend-filter-200` (default, `-strategy`/`TULIP_STRATEGY`):
  equal thirds in XIU/VFV/XEF, each one's share moved to ZAG while it trades
  below its 200-day mean, rebalanced **monthly**. Chosen by backtest
  ([docs/backtests](docs/backtests/README.md)): it matches equal-weight
  buy-and-hold with a slightly shallower drawdown and is robust to its
  lookback; `momentum-v1` was retired (whipsaw, ~2%/yr). `equal-weight` is
  the plain alternative. Reproduce: `go run . backtest -compare`.
- **Cycles** run at 10:05 and 15:00 Toronto on TSX trading days (or on demand
  via the `cycle_run` MCP tool). During the **first 5 trading days of a
  month** the allocator computes targets from bars completed by month-end
  (dividend-adjusted, as backtested) and the shared `strategy.Rebalance`
  turns them into orders: **sells first, buys once no sell is pending**, a 3%
  drift band, never two suggestions for one symbol. Each passes Thorn and
  becomes a pending **suggestion** paged to ntfy topic `tulip-approvals`. On
  target → `rebalance.done` for the month.
- **Thorn knobs** (env): `TULIP_THORN_MAX_ORDER_VALUE`,
  `TULIP_THORN_DAILY_LOSS_CAP`, `TULIP_THORN_MAX_POSITION_WEIGHT`,
  `TULIP_THORN_MAX_WEIGHT_BY_SYMBOL` (e.g. `ZAG.TO=1`),
  `TULIP_THORN_MAX_SUGGESTIONS_PER_DAY`.
- **Approve** on the page the notification links to (`/approve/<id>`), with
  **your passphrase**. It executes at once — only during TSX hours, at a fresh
  quote newer than the signal bar, inside the price band, after Thorn re-checks.
  Unapproved suggestions expire after 30 minutes.
- **Set the passphrase** (once, on your own machine — only the hash goes in
  the cluster):
  ```bash
  go run . hash-passphrase        # type a 12+ character passphrase
  # seal the printed hash as TULIP_APPROVAL_PASSPHRASE_HASH (Homelab services/tulip)
  ```
  Without it, approvals are disabled (fail closed) and suggestions just expire.
- **Benchmark:** a second book buys XIU.TO once with the same 10,000 CAD and
  holds it; both books are marked daily. A strategy that doesn't beat it isn't
  promoted.
- **Claude Code (MCP):** `tulip_status`, `journal_list`, `portfolio`,
  `suggestions_pending`, `propose`, `review_suggestion`, `decline`,
  `kill_switch_arm`, `cycle_run`, `broker_account` (read-only).
  No tool approves, executes or disarms — `tools_test.go` enforces the list.
  **An agent must never read the approval secret, call `/approve` or
  `/killswitch`, or approve on the operator's behalf** (ADR 0002 §3).

## Justifications, shadow books, scorecard (ADR 0004)

- **Every suggestion carries a justification**; `Desk.Propose` refuses one
  without (all sources: rules, brain, Claude Code). Rebalance suggestions
  give:
  - the rule with its numbers (e.g. "XEF.TO closed at 50.44, 1.0% above its
    200-day average");
  - what the order does to the book, and the signal date;
  - for a sell, the estimated realized gain or loss (cash account).

  The review page shows it all; the push shows the first line.
- **Shadow books:** every pre-registered candidate (`strategy.Candidates`:
  trend-filter-200, equal-weight, trend-ensemble, dual-momentum-12m,
  inverse-vol-60) trades an unapproved 10k paper book on the live schedule,
  in `shadow.*` journal kinds only. `TULIP_SHADOW=off` disables them.
- **`scorecard` (MCP, read-only):** every book from a common start: return,
  max drawdown, trades, sells, fees, realized P&L, plus what each candidate
  would hold today and why. The promotion rule (12 months + backtest gate,
  operator decides) is in ADR 0004.

## Co-pilot mode (Questrade, real account — ADR 0003)

Questrade's API is **read-only for retail clients**, so Tulip never trades:
`-mode copilot` (or `TULIP_MODE=copilot`) reads **one** account
(`TULIP_QUESTRADE_ACCOUNT`) and **you place every order yourself** in Questrade.

- **Sizing** (the strategy's cash, equity, holdings, and Thorn's checks) comes
  from a fresh account snapshot. If the account can't be read, **nothing is
  proposed** (`broker.unavailable`).
- **Suggestions** stay open until the session closes; the page tells you what
  to place. Approving in Tulip is disabled (`ErrCoPilot`) — Decline still works.
- **Reconciliation** polls today's executions every 3 minutes from the open to
  an hour after the close. Each execution is journaled once (`executions_seen`),
  partial fills are aggregated per order, and an order matching a suggestion
  (same symbol and side, same day, placed before it closed) records
  `suggestion.acted` with slippage; anything else is a `broker.manual_trade`,
  excluded from the strategy's record.
- **Daily loss cap** uses the account's first equity of the day (`broker.mark`).
  Caps are tunable: `TULIP_THORN_MAX_ORDER_VALUE`, `TULIP_THORN_DAILY_LOSS_CAP`,
  `TULIP_THORN_MAX_POSITION_WEIGHT`.
- **Token:** a Questrade manual-authorization refresh token, sealed as
  `QUESTRADE_REFRESH_TOKEN`, only **bootstraps**: tokens are single-use, so the
  rotated one is persisted (0600, atomic) next to the journal before use. A
  re-sealed value is detected by hash and re-bootstraps. Health:
  `tulip_broker_auth_ok`, `broker.auth_ok` / `broker.auth_failed` events.
  **Never use the token anywhere else** — one stray refresh spends it and
  breaks Tulip's chain.

## Stages

| Stage | Broker | Risk |
|---|---|---|
| **v1** | `PaperBroker` — synthetic fills at quote + slippage | none; proves the loop and calibrates Claude on paper |
| **v2** | Questrade **co-pilot** — read-only API, the operator places every order (ADR 0003; the practice stage no longer exists) | real, capped by Thorn and by you |
| **v3** | automatic execution — needs a broker whose API allows retail orders | real |
| **v4?** | autonomy inside Thorn's limits | only if the journal earns it over months |

(Canada constraints baked in: no Alpaca live, no Wealthsimple API — see ADR 0001.)

## Layout (as it lands)

```
main.go                 # entrypoint: flags, lifecycle
internal/loop/          # the autonomy: wake → observe → think → gate → suggest
internal/brain/         # Anthropic API client; proposals in, never out to brokers
internal/thorn/         # the deterministic risk gate
internal/broker/        # paper fills
internal/questrade/     # read-only Questrade client (accounts, balances, positions, executions)
internal/desk/          # suggest → approve (paper) / reconcile (co-pilot)
internal/marketdata/    # interface: free quotes/candles today, subscriptions later
internal/journal/       # append-only SQLite registry (the product)
internal/notify/        # ntfy suggestion pages with action buttons
internal/mcp/           # supervision surface (streamable HTTP)
docs/adr/               # decision log — read 0001 before the code
```

## Deployment

Built and operated through the Homelab platform golden path: GitHub Actions
→ `ghcr.io/blacklotus89898/tulip:<sha>` → Backstage scaffolder PR → ArgoCD →
`https://tulip.lab.home`. Secrets (`ANTHROPIC_API_KEY`, broker keys) live only
as SealedSecrets in the Homelab repo — nothing secret-shaped ever lands in
this public repo.
