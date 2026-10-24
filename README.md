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

- **Cycles** run at 10:05 and 15:00 Toronto on TSX trading days (or on demand
  via the `cycle_run` MCP tool): completed daily bars → `momentum-v1` → Thorn →
  a pending **suggestion**, paged to ntfy topic `tulip-approvals`.
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
  `suggestions_pending`, `propose`, `decline`, `kill_switch_arm`, `cycle_run`.
  No tool approves, executes or disarms — `tools_test.go` enforces the list.
  **An agent must never read the approval secret, call `/approve` or
  `/killswitch`, or approve on the operator's behalf** (ADR 0002 §3).

## Stages

| Stage | Broker | Risk |
|---|---|---|
| **v1** | `PaperBroker` — synthetic fills at quote + slippage | none; proves the loop and calibrates Claude on paper |
| **v2** | Questrade practice (`isPractice`) | none; real API, real quotes |
| **v3** | Questrade live — approval still on, small caps | real, capped |
| **v4?** | autonomy inside Thorn's limits | only if the journal earns it over months |

(Canada constraints baked in: no Alpaca live, no Wealthsimple API — see ADR 0001.)

## Layout (as it lands)

```
main.go                 # entrypoint: flags, lifecycle
internal/loop/          # the autonomy: wake → observe → think → gate → suggest
internal/brain/         # Anthropic API client; proposals in, never out to brokers
internal/thorn/         # the deterministic risk gate
internal/broker/        # interface: paper | questrade-practice | questrade-live
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
