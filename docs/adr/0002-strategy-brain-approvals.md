# ADR 0002: Baseline rules first, Claude as reviewer, measured; approvals are code-enforced and single-use

**Status:** Accepted (2026-10-24)
**Builds on:** [ADR 0001](0001-architecture.md)

## Context

ADR 0001 fixed the shape (loop, Thorn, human approval, journal, staged broker).
Two questions were left open: *what makes Tulip "gradually smarter"*, and *how
"Claude as the brain" coexists with "every action gated by the operator"* —
including the operator's wish that Claude (via its API and via Claude Code)
performs the transactions.

Both have a failure mode that would make the journal meaningless or the gate
fictional: an LLM picking trades with nothing to compare against can't be
evaluated; and a gate that lives in a prompt ("only execute if approved") is
not a gate.

## Decision

### 1. Layers: rules propose, Claude reviews, evidence decides

1. **Baseline strategies are deterministic Go** — pure functions of daily
   candles (`internal/strategy`): no network, no clock, unit-tested on fixture
   candles, so the *same code* runs live, in backtests, and later as Petal
   pipelines. v1 ships one: **trend-following momentum** (close above its
   50-day mean and 20-day return > 0 → candidate buy; below → candidate exit),
   sized by volatility.
2. **Claude reviews candidates** — it never invents orders from nothing. For
   each candidate it sees the numbers (signals, volatility, position, cash,
   recent journal outcomes) and returns strict JSON:
   `{verdict: approve|veto|resize, qty, confidence 0..1, rationale}`. It is only
   called when the baseline produced candidates (cost control), with a daily
   call cap and a configurable model (default a cheaper one). Malformed JSON,
   schema miss, or API error → **no proposal** (fail closed). No API key →
   brain disabled, rules-only.
3. **Everything is journaled with its counterfactual:** the rule's candidate,
   Claude's verdict, the human decision, and the paper fill — so the journal can
   answer "does Claude add anything over the plain rules?" (ADR 0001's signal to
   simplify) and "is Claude's confidence calibrated?".
4. **A benchmark from day one:** a buy-and-hold paper portfolio of the
   benchmark ETF (XIU.TO), started with the same capital, journaled daily.
   A strategy that doesn't beat it after costs is not promoted.
5. **"Gradually smarter" = evidence-driven promotion,** in later increments:
   calibration of confidence, walk-forward evaluation, more strategies competing
   on the same journal, strategies promoted or retired by their record. ML only
   once months of data exist (it overfits before that). None of this is v1.

### 2. Measurement rules (or the journal lies)
- **No lookahead:** decisions use data up to bar *t*; paper fills happen at the
  **next** observable price (+ slippage bps), never the price that made the
  signal.
- TSX hours/holidays in America/Toronto; `.TO` symbols; CAD.
- Market data (Yahoo chart endpoint — unofficial): User-Agent sent, daily
  candles cached, `429`/empty → journal "data unavailable" and propose nothing.
- The journal is **append-only**: events are inserted, never updated or deleted;
  state (positions, cash, pending suggestions) is derived from events.

### 3. Approvals are enforced in code, and credentials are split by direction
- **The MCP / API token (Claude Code, the brain's caller) can:** read
  everything, propose, decline, arm the kill switch. Anything that *reduces* risk
  is cheap to reach.
- **It cannot approve.** There is no approve tool on the MCP surface. If the
  credential Claude holds could approve, Claude could approve its own
  proposals — the human gate would not exist.
- **Approval is a human-channel action, gated by a passphrase only the
  operator knows** (implemented 2026-10-24, replacing the single-use HMAC link
  first sketched here). The operator generates it on their own machine
  (`tulip hash-passphrase`) and the cluster stores only its PBKDF2-SHA256 hash
  (600k iterations, `TULIP_APPROVAL_PASSPHRASE_HASH`). Reading the secret does
  not grant approval; wrong attempts are rate-limited and journaled.
- **ntfy messages carry no credential:** only the terms and a link to the
  approval page. The page shows the exact terms; the form posts back a
  proposal hash (symbol, side, qty, ref price, band, expiry), so an approval
  binds to what was displayed. **Single-use is enforced by the database**
  (`approvals_used` primary key — concurrent approvals can't both win). The
  approver's IP and User-Agent are journaled. Declining needs no passphrase
  (it only reduces risk); disarming the kill switch does.
- **What v1's gate holds against, stated plainly:** the in-loop brain and the
  MCP token cannot approve. An agent session with cluster-admin (Claude Code
  in the Homelab repo) can read secrets — it still cannot approve without the
  passphrase, but nothing *cryptographically* stops it from trying; the rule
  that it never reads `tulip-approval-*` secrets, never calls `/approve` or
  `/killswitch`, and never approves on the operator's behalf is **policy**,
  written into Homelab `CLAUDE.md`.
- **"Claude performs the transaction" is satisfied by the server, not the
  prompt:** once a valid human approval exists, Tulip executes it through the
  broker interface (Claude Code can also *trigger* execution of an
  already-approved suggestion — the server checks the approval record).
- **At execution time Thorn re-checks** (caps, loss limit, allowlist, price
  still inside the approved band); a stale approval is journaled as a reject.
  The **kill switch** is persisted and checked last.

### 4. v3 gate (live money) — written now so it can't be forgotten
The v1 ntfy channel is unauthenticated on the LAN: anything in a message
(including an approval link) is readable by any LAN client. Acceptable for
**paper**. Before any live broker (v3):
- **asymmetric approval** — a WebAuthn passkey (or Ed25519 key) whose private
  half lives only on the operator's device, the cluster holding only the
  public key, so no cluster-side secret (and no agent with cluster-admin) can
  approve;
- an authenticated notification channel (ntfy access control + a phone
  subscriber token);
- TLS to the approval page from the phone (lab CA trusted there) — v1 serves
  it on the LAN NodePort over plain HTTP, so the passphrase crosses the LAN
  unencrypted;
- the Homelab sealed-secrets keyring re-exported after each rotation.

## Consequences

- **Positive:** the brain's value is measurable instead of assumed; the human
  gate is a property of the code; strategy code is reusable for backtests.
- **Cost:** v1 is deliberately boring (one rule strategy) — the point is a loop
  that tells the truth, not returns.
- **Signal this was wrong:** after a few months of paper, Claude's reviewed
  proposals do no better than the raw baseline (simplify to rules), or neither
  beats the benchmark (the strategy class is wrong, not the plumbing).
