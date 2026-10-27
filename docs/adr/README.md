# Tulip — Architecture Decision Records

The decision log, newest last. Each ADR is the why behind a shape you'll find
in the code; when code and ADR disagree, one of them is a bug.

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-architecture.md) | Tulip is a human-approved trading agent (service + loop, Thorn gate, staged brokers, append-only journal) | Accepted 2026-10-24 |
| [0002](0002-strategy-brain-approvals.md) | Baseline rules first, Claude as reviewer, measured against a benchmark; approvals code-enforced, single-use, human-channel only (no MCP approve) | Accepted 2026-10-24 |
| [0003](0003-questrade-copilot.md) | Questrade co-pilot: read-only API (retail can't place orders), operator places every order, real-account sizing, reconciliation, single-use token custody | Accepted 2026-10-24 |
| [0004](0004-shadow-books-promotion.md) | Pre-registered candidates run as unapproved shadow books on the live schedule; scorecard from a common start; promotion needs 12 months + the backtest gate, decided by the operator | Accepted 2026-10-25 |
| [0005](0005-paper-training-autoexecute.md) | Paper training: with `TULIP_AUTOEXECUTE=paper` (operator-set) the rules' own suggestions fill the paper book without approval; same execution checks, journaled as `order.auto_executed`; never co-pilot, never MCP/brain proposals | Accepted 2026-10-27 |
