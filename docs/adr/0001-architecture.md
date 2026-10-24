# ADR 0001: Tulip is a human-approved trading agent

**Status:** Accepted (2026-10-24)
**Context repo:** [Homelab](https://github.com/Blacklotus89898/Homelab) (deploy), [Petal](https://github.com/Blacklotus89898/Petal) (sibling pattern)

## Context

A personal, autonomous trading agent powered by Claude, built and operated on the
Homelab platform (k3s + ArgoCD GitOps, Backstage portal, Petal pipelines). The
operator is in Canada, which rules out some brokers (Alpaca has no Canadian
live accounts; Wealthsimple has no official API) and makes Questrade / IBKR /
Kraken the viable API surfaces.

The name is deliberate: the tulip mania of 1637 was the first recorded
speculative bubble. Naming a trading agent after history's cautionary tale is
baked-in humility — the whole design exists to prevent tulip mania, one human
at a time. Sibling to Petal (the platform's pipeline orchestrator), the garden
grows: Petal builds, Tulip trades.

## Decision

1. **A service with a loop, not a bare MCP server.** MCP is an interface, not
   a runtime: an MCP server only acts when a client calls it, which is zero
   autonomy. Autonomy means owning the wake schedule
   (`observe → think → gate → suggest → execute → journal`), so Tulip is a
   long-running Go service. MCP is its **supervision surface** — the window a
   human (or Claude Code) uses to inspect, interrogate, and override.

2. **Claude proposes; Thorn disposes.** The brain (Anthropic API) returns
   structured proposals `{action, symbol, qty, confidence, rationale}`. It
   never touches a broker. `internal/thorn` is a deterministic Go gate that
   enforces, before anything moves: symbol allowlist, per-order and per-position
   size caps, daily loss cap, confidence floor, and a kill switch. A
   hallucinated order dies at the gate.

3. **A human approves every order.** Surviving proposals become pending
   suggestions (journaled, with a TTL) and page the operator through the
   existing ntfy topic with Approve/Decline action buttons hitting a
   token-gated endpoint. Desk-side fallback: the same decisions as MCP tools.
   Approval fatigue is the known failure mode, so the caps are config:
   max suggestions/day (start 3), batched delivery, TTL 15 min
   (expired = auto-declined and journaled as a miss), confidence below floor
   journaled as observation-only and never paged.

4. **The broker is an interface with a staged rollout.**

   | Stage | Implementation | What it proves |
   |---|---|---|
   | v1 | `PaperBroker` — synthetic fills at quote + slippage bps | the loop, Thorn, approval UX, journal; Claude's paper hit-rate |
   | v2 | Questrade practice (`isPractice`) | OAuth handling, real order types, real TSX quotes — still no money |
   | v3 | Questrade live, approval **still on**, small size caps | real fills, real slippage |
   | v4? | autonomy inside Thorn's limits | only if the journal earns it over months — never a default |

5. **The journal is the product.** Append-only SQLite (WAL, single writer —
   the Petal registry pattern): every observation, proposal, Thorn verdict,
   human decision, fill, and TTL expiry. The edge is not Claude's picks; it is
   the discipline encoded in Thorn plus the review loop the journal enables.

6. **Market data is behind an interface too.** v1 uses free quotes/candles
   (Yahoo/Twelve Data class); Questrade/IBKR subscriptions can replace it
   without touching the brain or Thorn.

7. **Deployment follows the Homelab golden path.** Single Go binary (static,
   distroless, ~20MB — the Petal image pattern), GitHub Actions builds to
   `ghcr.io/blacklotus89898/tulip:<sha>` (Actions holds registry credentials;
   Petal cannot push to authenticated registries), Backstage scaffolder PR adds
   `apps/tulip` + `services/tulip` to Homelab, ArgoCD deploys, route at
   `https://tulip.lab.home`. Secrets (`ANTHROPIC_API_KEY`, broker keys) exist
   only as SealedSecrets in Homelab — never in this public repo, never in
   history, never in commit messages.

8. **Paper-first means no live path exists until v3.** The v1 codebase has no
   ability to place a real order; live capability arrives with the Questrade
   adapter, behind Thorn and human approval from its first commit.

## Consequences

- **Positive:** full end-to-end loop from day one at zero financial risk; the
  journal accumulates calibration data before money moves; every stage
  promotion is an interface swap plus config, not a rewrite; the platform's
  existing paging (ntfy) and GitOps discipline carry the operational load.
- **Costs:** human latency bounds strategy class (no sub-minute anything —
  fine); free market data is delayed/unofficial and will diverge from real
  fills; SQLite on a PVC is single-node by design.
- **Trust:** the operator's phone is in the approval loop; a compromised
  cluster node could approve its own suggestions — LAN-only exposure and the
  Homelab trust boundary are the mitigation, as everywhere else on the platform.
- **Signal this was wrong:** approval fatigue within two weeks (caps too high,
  or the wrong strategy class for HITL); paper→live divergence so large the
  journal's paper hit-rate was meaningless; or Claude's proposals so consistently
  gated that the LLM adds nothing over plain rules — then simplify to rules and
  keep the loop.
