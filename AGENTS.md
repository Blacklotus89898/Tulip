# AGENTS.md — Tulip

Guide for any coding agent (Claude Code, Codex, Cursor, Copilot, Gemini…) and human contributor.

## What this is

Human-approved trading agent. **Claude proposes; Thorn disposes; the journal remembers.**
The loop observes markets, asks Claude for proposals, gates them through Thorn (deterministic
risk rules) and pages a suggestion to the operator's phone. Nothing executes without the
operator's approval (paper training mode is the one, operator-set exception — ADR 0005).

Read first: `docs/adr/0001-architecture.md`, then 0002 (approvals), 0003 (Questrade co-pilot),
0004 (shadow books), 0005 (paper auto-execute), 0006 (IBKR US desk). README "Operating it".

## Commands

```bash
gofmt -l .                 # must print nothing
go vet ./...
go test ./...
go run . backtest -compare # reproduce the strategy choice (docs/backtests)
go run .                   # local run; config by flags/env (README)
```

CI: `.github/workflows/ci.yaml` (vet/build/test); `build-image.yaml` publishes
`ghcr.io/blacklotus89898/tulip:<sha>` from `main`. `.petal/pipelines/` holds the monthly backtest run.

## Layout

```
main.go              entrypoint, flags, lifecycle, Anthropic client (proposals in, never out to brokers)
tools.go, copilot.go, backtest.go   MCP tool table, Questrade co-pilot, backtest CLI
internal/loop        wake → observe → think → gate → suggest
internal/thorn       the deterministic risk gate
internal/approval    approve page + passphrase check
internal/desk        suggest → approve (paper) / reconcile (co-pilot); one desk per exchange
internal/broker      broker interface + paper fills
internal/questrade   READ-ONLY Questrade client
internal/calendar    exchange calendars (TSX, NYSE)
internal/marketdata  quotes/candles
internal/strategy    allocators + shared Rebalance
internal/backtest    backtester
internal/portfolio, internal/shadow   positions, shadow books (ADR 0004)
internal/journal     append-only SQLite (the product)
internal/notify      ntfy pages with action buttons
internal/mcp         supervision surface (streamable HTTP)
internal/web         approve/charts pages
docs/adr/, docs/backtests/
```

## Non-negotiables (money-touching code)

- **The model never touches a broker.** The Anthropic client returns proposals only; only the desk,
  after Thorn and human approval, calls the broker interface.
- **Thorn is plain, tested Go.** Never loosen a Thorn check, cap or the kill switch as a side
  effect of another change; any change to a limit needs its own PR and tests.
- **Agents never approve.** Do not call `/approve/…` or `/killswitch`, read approval secrets or
  passphrase hashes, or extend auto-execute beyond paper mode or to MCP/brain proposals.
- **Questrade is read-only** here; refresh tokens are single-use — never use them outside Tulip.
- The journal is append-only. No UPDATE/DELETE of history.
- Public repo: secrets (`ANTHROPIC_API_KEY`, broker keys) exist only as SealedSecrets in Homelab.
- Strategy changes must be justified by a reproducible backtest committed under `docs/backtests/`.

## Adding a feature

1. Branch `feat/<topic>`; decisions that change behaviour → new ADR (status Proposed) first.
2. New strategy: implement in `internal/strategy`, backtest it with `internal/backtest`, add a pre-registered universe, run
   `backtest -compare`, commit the result, then wire it behind a flag — never as the default in the same PR.
3. New MCP tool: add to the tool table, keep it read-only unless the ADR says otherwise, and test it.
4. gofmt/vet/test green → PR, conventional title citing the ADR: `feat: … (ADR 0006) (#8)`.

## Shipping

Merge → image `:<sha>` → bump SHA in Homelab `services/tulip/` (PR) → ArgoCD → `https://tulip.lab.home`.

## Knowledge base

If the `knowledge` MCP is connected, search it before debugging and write back non-obvious
fixes (rules: DaVinci `AGENTS.md`).
