# Backtests

Reproduce: `go run . backtest -compare` (and `go run . backtest -sweep` for
momentum-v1). Raw outputs are checked in next to this file, dated.

Method (`internal/backtest`): production strategy code, dividend-adjusted daily
Yahoo history, signals on completed bars filled at the **next day's open** with
10 bps slippage, every order through Thorn, one common start for all
candidates, the final 30% held out. Candidates were chosen **before** seeing
results and not tuned; lookback variants are sensitivity checks only.

## 2026-10-24 — momentum-v1 vs pre-registered allocators

Universe XIU.TO, VFV.TO, XEF.TO, ZAG.TO · 2014-04 → 2026-10 · holdout from 2023-01.

| candidate | CAGR | max DD | Sharpe | holdout CAGR | trades |
|---|---|---|---|---|---|
| momentum-v1 (live) | +2.3% | −7% | 0.63 | +2.1% | 792 |
| equal-weight, monthly rebalance | +9.4% | −24% | 0.90 | +14.4% | 21 |
| **trend-filter-200** | +9.5% | −22% | 0.93 | +16.4% | 112 |
| dual-momentum-12m | +12.5% | −27% | 0.88 | +19.6% | 49 |
| inverse-vol-60 | +6.9% | −22% | 0.84 | +11.0% | 144 |
| buy & hold XIU | +11.0% | −35% | 0.78 | +18.8% | 1 |
| equal-weight hold | +10.4% | −25% | 0.93 | +16.8% | 4 |

At $4.95/order momentum-v1 falls to −2.4%/yr; the allocators barely move.

Sensitivity (commission 0):
- dual-momentum lookback 126/189/252/315 d → CAGR 6.8 / 8.9 / 12.4 / 13.2%:
  **fragile** — 12 months happens to be good; one concentrated bet, mostly
  "hold the US" in a decade the US led.
- trend-filter 100/150/200/250 d → CAGR 8.7–10.0%, Sharpe 0.91–0.97, DD −22%:
  **robust**, but no better than equal-weight hold on return; a slightly
  shallower drawdown.

### Conclusions

1. **momentum-v1 is retired** from consideration for real money: it whipsaws
   (≈30 round trips/yr, 37% winners), sits ~60% in cash, and costs kill it.
2. **No candidate beats equal-weight buy-and-hold risk-adjusted.** Dual
   momentum's higher return is not robust to its lookback.
3. Best practical rule for the operator: **equal-weight core, with the 200-day
   trend filter as an optional drawdown dampener** (robust, ~9 trades/yr).
   Tulip's value is discipline — rebalancing reminders, no panic trades — not
   alpha. Any future strategy must beat equal-weight hold on the holdout,
   after $4.95 commissions, and survive lookback sensitivity.

## 2026-10-24 — 25 years through 2008: XIU + XBB (bonds), 2001-12 → 2026-10

The 4-ETF test above never saw a real crash (VFV/XEF start 2012–13). XIU and
XBB go back through 2008 and 2020 (`-compare -symbols XIU.TO,XBB.TO`;
holdout from 2019-05).

| candidate | CAGR | max DD | Sharpe | holdout CAGR |
|---|---|---|---|---|
| **trend-filter-200** | +8.8% | **−24%** | 0.77 | +11.7% |
| buy & hold XIU | +9.3% | **−48%** | 0.62 | +14.1% |
| equal-weight hold (50/50) | +7.3% | −29% | 0.78 | +10.2% |
| momentum-v1 | +0.6% | −6% | 0.31 | +0.5% |

Trend-filter lookbacks 100/150/200/250 d → CAGR 8.8–10.5%, max DD −20 to −24%:
robust. **This is the trend filter's case: it halves the crash drawdown for
≈0.5%/yr less return than all-equity**, and beats a static 50/50 on return.
It lags in fast V-shaped recoveries (holdout includes 2020). It is the live
default (`TULIP_STRATEGY=trend-filter-200`).

Not covered: taxes, FX on VFV/XEF (CAD-hedged/unhedged mix is embedded in the
ETF prices), regime risk (one 12-year sample, mostly a bull market).

## 2026-10-25 — trend-ensemble added (ADR 0004 candidate set)

Same method, both universes (`2026-10-25-compare-candidates.txt`,
`2026-10-25-xiu-xbb-candidates.txt`), $4.95/order:

| candidate | 4-ETF CAGR / max DD / trades | XIU+XBB 25y CAGR / max DD / trades |
|---|---|---|
| trend-filter-200 (live) | +9.2% / −22.6% / 115 | +8.7% / −23.8% / 77 |
| trend-ensemble | +8.8% / −22.5% / 223 | **+9.6% / −19.8%** / 160 |
| equal-weight hold | +10.4% / −24.9% / 4 | +7.3% / −29.3% / 4 |

The ensemble is a tie on the 4-ETF universe and better over 25 years, but it
trades about twice as often. In a cash account each sell is a taxable event,
so its edge has to survive that (ADR 0004 §4). It was added after the
lookback sensitivity was seen (mild look-ahead), so it is judged by the
shadow books, not by this table.

## 2026-11-15: US universes for the IBKR desk (ADR 0006)

Reproduce: `go run . backtest -compare -symbols <U> -benchmark IVV -commission 0 -commission-alt 1.00`
(the `-benchmark` flag is new; its default, XIU.TO, keeps the TSX runs unchanged).
The three universes were **fixed before any result was seen**. Prices are USD
and dividend-adjusted, with **no 15% US withholding tax and no CAD→USD FX
cost**. $1.00 is IBKR Pro's per-order minimum. Raw output:
`2026-11-15-us-*.txt`.

| universe (safe last) | period | trend-filter-200 | equal-weight | dual-mom-12m | buy & hold IVV | equal-wt hold |
|---|---|---|---|---|---|---|
| IVV, EFA, IEF | 2003-08 → 2026-11 | +8.6% / −26% / 0.72 | +7.7% / −39% / 0.68 | +8.5% / −29% / 0.64 | **+11.2%** / −55% / 0.67 | +8.3% / −41% / 0.67 |
| IVV, EFA, EEM, IEF | 2004-04 → 2026-11 | +8.0% / −23% / 0.64 | +7.6% / −45% / 0.57 | +9.8% / −29% / 0.60 | **+10.7%** / −55% / 0.64 | +7.6% / −48% / 0.53 |
| IVV, VXUS, BND | 2012-02 → 2026-11 | +7.0% / −24% / 0.65 | +7.8% / −23% / 0.75 | +6.8% / −33% / 0.54 | **+14.3%** / −34% / 0.89 | +9.3% / −26% / 0.77 |

(CAGR / max drawdown / Sharpe; holdout = the final 30%, in the raw files.)

### Conclusions

1. **Same story as the TSX desk: no rule beats buy-and-hold on return.**
   Holding IVV alone won every window. These were the decades the US led,
   and the result is not evidence that this will continue.
2. **The trend filter's case is drawdown, not return.** Over 2003+, which
   includes 2008, it cut the worst loss from **−55% to −26%** for about 2.6
   points of CAGR. It is robust across lookbacks: 100–250 days gives 7.5–9.8%
   CAGR and −21 to −28% drawdown. Dual momentum is again fragile: 189 days
   gives 6.2% and 315 days gives 9.9%.
3. **Costs don't matter at $1/order:** trend-filter-200 is 8.6% at both $0
   and $1. momentum-v1 stays retired (+0.8%, about 1,600 trades).
4. **Proposed for the US desk:** universe **IVV, EFA, IEF** (the longest
   history, it covers 2008, and it mirrors the TSX desk's shape), with
   **trend-filter-200** live, matching the TSX desk. The other ADR 0004
   candidates run as shadows, and the benchmark is buy-and-hold IVV. The
   operator decides. IEFA (0.07%) is a cheaper twin of EFA (0.32%), but it
   only has history from 2012. Trading the fund that was backtested keeps the
   record honest; switching to IEFA would be a separate decision.
