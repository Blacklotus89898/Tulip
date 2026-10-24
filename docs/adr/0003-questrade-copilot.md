# ADR 0003: Questrade co-pilot — read-only API, the operator places every order

**Status:** Accepted (2026-10-24)
**Supersedes:** the broker staging of [ADR 0001](0001-architecture.md) §4/§8 (v2 "Questrade practice", v3 "Questrade live" with Tulip executing)

## Context

ADR 0001 planned Tulip → Questrade practice → Questrade live, with Tulip
executing approved orders. Two facts changed that:

1. **Questrade's API does not let retail clients place orders.** Its own
   documentation: the API "enables you to develop live or practice applications
   that can access your account data and retrieve market data. If you are a
   Questrade partner developer, you can also use the API to place trades." The
   API access agreement likewise rules out automated order sending outside
   partner applications.
2. **The free practice account is no longer offered**, so there is no
   risk-free Questrade stage.

The operator chose to stay with Questrade on a real account, accepting that
any loss is theirs.

## Decision

1. **Tulip is a co-pilot on Questrade.** It observes, proposes (rules + Claude's
   review in chat, ADR 0002), gates with Thorn, notifies — and **the operator
   places every order themselves** in Questrade, under their own login and 2FA.
   Tulip's Questrade access is **read-only**: accounts, balances, positions,
   executions. No order endpoint is called — compliant with the API agreement,
   and the approval gate is now the strongest possible one: Tulip has no
   capability to trade.
2. **One dedicated account.** Tulip reads exactly one account number
   (`TULIP_QUESTRADE_ACCOUNT`), ideally a small account opened for this. Other
   accounts the token can see are ignored and never journaled.
3. **Sizing comes from the real account.** When the broker is connected, the
   strategy's held quantity, cash and equity — and Thorn's checks — come from a
   fresh account snapshot. **If the snapshot can't be fetched, Tulip proposes
   nothing** (stale cash = wrong sizes). Thorn's caps are re-tuned to the real
   account's size. The passphrase/paper path (ADR 0002 §3) stays for paper mode
   only; in co-pilot mode suggestions are acted on in Questrade, not approved
   in Tulip.
4. **Reconciliation is the truth.** Tulip polls executions, journals each one
   once (`executions_seen` primary key — idempotent), aggregates partial fills
   by order, and matches them to suggestions: same symbol and side, until the
   session closes (manual placement is slower than a 30-minute TTL). Late or
   partial follow-through is journaled as it happened; executions matching no
   suggestion are journaled as **manual trades** and excluded from the
   strategy's record. Timestamps are parsed with their UTC offset.
5. **Measured honestly.** Deposits and withdrawals move account equity, so the
   strategy is scored on **Tulip-attributed trades** (P&L of matched
   executions) and compared with buy-and-hold **in percentage terms**, not
   dollars.
6. **Token custody (the refresh token is single-use).**
   - Refreshes are serialised behind one mutex.
   - The new refresh token is persisted **before** it is used: temp file, fsync,
     rename, mode 0600, under `/data`. If persisting fails, Tulip stops using
     the broker and alerts — the old token is already spent.
   - Tokens and the API server URL never reach the journal, logs, metrics or
     ntfy (the journal is readable over MCP).
   - The sealed secret only bootstraps: at startup Tulip uses the persisted
     token, unless the sealed value changed (hash compared), meaning the
     operator re-bootstrapped.
   - **No agent ever uses the token outside Tulip** — a single stray refresh
     spends it and silently breaks Tulip's chain (Homelab `CLAUDE.md`).
   - Broker auth health is a gauge (`tulip_broker_auth_ok`) with an alert; an
     unused refresh token expires after about 7 days.
7. **Market data:** daily bars stay on Yahoo for now (Questrade quote
   entitlements for retail are unclear); the account snapshot is Questrade's.

## Consequences

- **Positive:** no capability to trade means no automation risk on real money;
  real fills and real slippage enter the journal from day one; the operator's
  own Questrade login is the approval.
- **Cost:** every trade needs the operator in the Questrade app; reconciliation
  logic; a single-use token chain that breaks if mishandled or idle ~7 days.
- **Not covered:** fully automatic execution. That requires a broker whose API
  allows retail orders (Interactive Brokers) — a broker-interface swap, not a
  rewrite.
- **Signal this was wrong:** the operator stops acting on suggestions
  (approval fatigue — the journal shows it), or reconciliation misattributes
  trades.
