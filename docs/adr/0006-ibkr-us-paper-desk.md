# ADR 0006: An IBKR desk for US-listed ETFs, beside the Questrade co-pilot

**Status:** Proposed (2026-11-14). It needs the operator's confirmation, and the spike (§8) can't start until an IBKR account exists.
**Builds on:** [ADR 0001 §4](0001-architecture.md) (the broker interface), [ADR 0003](0003-questrade-copilot.md) (reconciliation, token custody), [ADR 0004](0004-shadow-books-promotion.md) (books, scorecard), [ADR 0005](0005-paper-training-autoexecute.md) (paper auto-execute)
**Does not supersede:** ADR 0003. The TSX co-pilot stays as it is.

## Context

The operator wants Tulip to place orders through a broker API, practising on
paper first. Questrade can't do this, because its retail API is read-only
(ADR 0003). Interactive Brokers can. Two facts from IBKR's official
[TWS API limitations page](https://www.interactivebrokers.com/docs/tws-api/doc/notes-limitations/tws-api-limitations)
(checked 2026-11-14) constrain the design:

1. **Canadian residents may not send API orders for products listed in
   Canada.** IBKR Canada applies CIRO rule DMR 3200 A.1(b)(i), and it applies
   to third-party tools too. Tulip's universe (XIU, VFV, XEF, ZAG) is all on
   the TSX, so it can never be traded through IBKR's API. We don't know whether
   the paper account enforces this, but a paper desk that could never go live
   would be practising the wrong thing.
2. **A paper account can be opened only from an approved and funded live
   account.** The paper account inherits that account's trading permissions
   and market-data subscriptions.

On 2026-11-14 the operator chose to **keep the Canadian strategy and add a
separate IBKR section for US-listed ETFs**, rather than move everything to
USD.

## Decision

### 1. Two desks, each with its own broker and mode
Today `TULIP_MODE` is global (`paper | copilot`, checked in `main.go`). It
becomes a setting of each **desk**:

| desk | universe | broker | mode | who places orders |
|---|---|---|---|---|
| `tsx` (existing) | XIU, VFV, XEF, safe ZAG (CAD) | Questrade, read-only | `paper` or `copilot`, as today | the operator, in Questrade (ADR 0003) |
| `us` (new) | IVV, EFA, safe IEF (USD) (§3) | `paper` now (synthetic fills); `ibkr-paper` after the spike | paper | Tulip (synthetic, then through the API) |

- **One journal per desk** (amended 2026-11-15, before any code). The TSX desk
  keeps `data/tulip.db` and the US desk writes `data/tulip-us.db`. Each desk has
  its own strategy book, benchmark book, shadow books, Thorn limits and
  calendar (`calendar.NYSE`), so no replay code changes and single-use claims
  (`approvals_used`, `executions_seen`) stay per desk. The cost: there is no
  single query across desks, and the kill switch is a fan-out (MCP
  `kill_switch_arm` arms **every** desk and fails closed if any one fails).
  The two desks never net across currencies or books.
- **Off by default.** `TULIP_US=on` adds the US desk; every other `TULIP_US_*`
  variable has a default (symbols, strategy, benchmark, auto-execute, Thorn
  caps). With it unset, Tulip runs exactly as before: a nil calendar is TSX,
  and the TSX desk's journal payloads, metric names and page paths are
  unchanged. The US pages live under `/us/`, and MCP tools take an optional
  `desk` argument (default `tsx`; an unknown desk is an error).
- **US Thorn defaults:** order cap 10,000, position cap 0.55 (two risk ETFs at
  1/2 each; the TSX desk's 0.4 would refuse the plan), safe asset up to 100%,
  8 suggestions a day. Override with `TULIP_US_THORN_*`.
- There is no `ibkr-live` mode in this ADR. Live IBKR trading needs its own
  ADR, with human approval kept on (§7).

### 2. Paper training on the `us` desk follows ADR 0005
`TULIP_AUTOEXECUTE` becomes per desk. On the `us` desk, the `paper` value
lets the rules' own suggestions be sent to the **IBKR paper account** without
approval. Every ADR 0005 rule still applies: only `source == "rules"` orders
qualify; each claim is single-use; the order passes the same execution checks
and Thorn again; it is journaled as `order.auto_executed`; and nothing can
trigger it over MCP or HTTP. Because IBKR fills orders asynchronously, an
`order.submitted` event is journaled first, and the fill (§5) settles it later.

### 3. The US universe and its benchmark
- **Operator decision 2026-11-15: IVV · EFA · safe IEF, with trend-filter-200
  live.** Three universes were fixed before any result was seen and
  backtested (`docs/backtests/`, 2026-11-15). This one has the longest history
  (2003+, so it includes 2008) and mirrors the TSX desk's shape. EFA was chosen
  over its cheaper twin IEFA because it is the fund that was backtested.
- The benchmark is a USD buy-and-hold of the US equity ETF (IVV). Scorecards
  compare percentages and never mix CAD and USD.
- The ADR 0004 candidate set runs as shadow books on this universe from day
  one. Its promotion clock is separate from the TSX desk's.
- **Not modelled:** FX conversion cost (CAD→USD at funding), the 15% US
  withholding tax on dividends in a non-registered account, and US ETF estate
  tax exposure. The scorecard states this; backtests do not pretend otherwise.

### 4. A Canadian-listing guard in code, not config
Before any `placeOrder`, the IBKR broker resolves the contract (conId) and
**refuses** the order if any of these are true:
- its exchange or primary exchange is a Canadian venue (TSE, VENTURE, CSE,
  NEO/Cboe Canada, any `*.TO` / `*.V` / `*.NE` / `*.CN` symbol);
- its currency is not USD;
- its conId is not on the `us` desk's allowlist.

The refusal is journaled as `thorn.rejected` (reason `ca-listing`), and a test
pins it. The rule comes from a regulator, so no environment variable can turn
it off.

### 5. The IBKR broker: an asynchronous order lifecycle behind the interface
- **Library:** [scmhub/ibapi](https://github.com/scmhub/ibapi), an unofficial
  Go client that mirrors the official TWS API. It is MIT-licensed, at v0.10.50
  (tracking API 10.48), with commits as recent as 2026-09. It needs **Go
  1.26**, so Tulip's go.mod and Dockerfile go from 1.25 to 1.26 in the same
  PR. The module is pinned by version and checksummed.
  - *Why Go and not a Rust sidecar:* Thorn, the journal and single-use
    approvals stay in one process, so there is no internal hop where an order
    can be duplicated or lost. A Rust service (the `ibapi` crate, 2.x) is the
    fallback if the Go client fails the spike.
- **The subset Tulip uses.** This list is also the spike's acceptance list:
  - connect, handshake, `nextValidId`, and reconnect after the daily Gateway restart
  - `reqAccountSummary`, `reqPositions`, for USD cash and holdings sizing
  - `reqContractDetails`, to resolve conIds and feed the §4 guard
  - `placeOrder`, a market or marketable-limit DAY order with `orderRef = suggestion ID`
  - `orderStatus`, `execDetails`, `commissionReport`
  - `reqExecutions`, to reconcile at startup and after a reconnect
- **Lifecycle:** submit → `order.submitted` (orderId, permId, orderRef) →
  partial fills → `broker.execution`, journaled once per **execId**
  (`executions_seen`, the same idempotency as ADR 0003) → `order.filled`,
  `order.cancelled` or `order.expired` at the close. Fills match their
  suggestion **exactly through `orderRef`**, not by symbol and side as on the
  co-pilot desk. An execution whose `orderRef` isn't Tulip's is journaled as
  `broker.manual_trade` and left out of the strategy's record.
- **Sizing comes from the account**, as in ADR 0003 §3. If Tulip can't fetch
  an account snapshot, it proposes nothing on the `us` desk.
- **No duplicate sends:** before submitting, the broker checks open orders and
  today's executions for the same `orderRef`. A suggestion that already has a
  `permId` is never sent again.

### 6. IB Gateway in the cluster
- **Image:** built by us from
  [gnzsnz/ib-gateway-docker](https://github.com/gnzsnz/ib-gateway-docker)
  (IB Gateway + IBC for unattended login), because IBKR's license forbids
  redistributing pre-built images. It is pushed to GHCR, pinned by digest, and
  rebuilt when Renovate bumps the Gateway version. Size matters because the
  worker disk is about 82% full, so we pull one image and keep no old tags.
- **Where it runs:** as a **sidecar in the Tulip pod**, with the API on
  `127.0.0.1:4002` (paper). The TWS socket is unencrypted and unauthenticated,
  so there is no Service, no NodePort and no route. IBC sets
  `TRADING_MODE=paper`. The VNC console stays off, and if it's ever needed it
  is reached only through `kubectl port-forward`.
- **Credentials:** the IBKR username and password are a SealedSecret in
  Homelab (the repo is public), mounted only into the Gateway sidecar. They
  never reach the journal, logs, metrics or ntfy.
- **Daily restart:** IBC restarts the Gateway every day at a fixed time,
  outside US market hours. Tulip treats a dropped connection as expected,
  backs off, reconnects and reconciles. If it is still down after N minutes
  during market hours, it journals `broker.unavailable` and sends one ntfy
  push.
- **Resources:** the Gateway is a JVM. It gets measured requests and limits,
  set from the spike's real usage, inside the namespace LimitRange.

### 7. Explicitly out of scope
- **Live IBKR trading.** It needs its own ADR: human approval kept on through
  the approval page, Thorn caps re-tuned for the real account, a decision on
  2FA and IBKR's re-authentication rules, and a measured paper record first.
  The ADR 0004 promotion discipline is the minimum.
- Options, margin, shorting, fractional shares and non-USD currencies other
  than the funding conversion.
- Running Claude or the brain anywhere near the broker. The LLM still never
  touches an order (ADR 0001).

### 8. Order of work
0. **Precondition:** the operator opens and funds an IBKR account and opens
   its paper account. IBC credentials are sealed into Homelab.
1. **Spike** (a branch, not deployed): build the Gateway image and run it
   locally in paper mode. Prove every call in the §5 subset against it with
   scmhub/ibapi, including a forced restart and reconnect, and the §4 guard
   rejecting a `.TO` contract. If the library fails, record why here and
   reconsider Rust or vendoring the subset.
2. Backtest the US universe (`docs/backtests/`), then fix the symbol list.
3. Desk refactor (§1): make mode, broker and books per desk, with the `tsx`
   desk behaving as before. Tests pin that. Until the IBKR broker exists, the
   US desk runs paper training (ADR 0005) on synthetic fills, so its 12-month
   shadow clock starts now.
4. IBKR broker plus reconciliation, with tests against a fake TWS server.
5. Homelab PR: the Gateway sidecar, the SealedSecret, `TULIP_DESKS`, and an
   SLO probe on Tulip's view of the broker connection.

## Consequences
- Tulip gets its first real order path, on paper only. The TSX co-pilot is
  unchanged and keeps its stronger guarantee: Tulip has no ability to trade
  there.
- There are two currencies and two scorecards. Nothing aggregates them into
  one "total", by design.
- Running the Gateway adds an operational dependency: a JVM sidecar, a daily
  restart, and credentials. The ntfy noise budget from the golden-signals work
  applies, so a dropped connection pages once, not every cycle.
- The spike can disprove the library choice cheaply, before any refactor.
