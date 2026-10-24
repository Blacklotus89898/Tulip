package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/backtest"
	"github.com/Blacklotus89898/Tulip/internal/marketdata"
	"github.com/Blacklotus89898/Tulip/internal/thorn"
)

// runBacktest is `tulip backtest`: replay momentum-v1 + Thorn over Yahoo's
// dividend-adjusted history. Output stays OUT of the live journal (it feeds
// metrics and the scorecard) — a table on stdout, optional JSON file.
func runBacktest(args []string) error {
	fs := flag.NewFlagSet("backtest", flag.ExitOnError)
	symbols := fs.String("symbols", "XIU.TO,VFV.TO,XEF.TO,ZAG.TO", "symbols the strategy trades")
	cash := fs.Float64("cash", 10000, "starting cash (CAD)")
	commission := fs.Float64("commission", 0, "per-order commission (CAD); also try 4.95/9.95")
	slippage := fs.Float64("slippage-bps", 10, "slippage per fill, basis points")
	maxOrder := fs.Float64("max-order", 0, "Thorn per-order cap (0 = default 2500)")
	sweep := fs.Bool("sweep", false, "also run neighbouring parameters (sensitivity only)")
	cacheDir := fs.String("cache", "data/history", "on-disk bar cache (refetched after 24h)")
	jsonOut := fs.String("json", "", "write the full result (trade log included) to this file")
	_ = fs.Parse(args)

	syms := splitList(*symbols)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	bars := map[string][]marketdata.Candle{}
	for _, s := range append(append([]string{}, syms...), "XIU.TO") {
		if _, ok := bars[s]; ok {
			continue
		}
		b, err := cachedHistory(ctx, *cacheDir, s)
		if err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
		bars[s] = b
	}
	th := thorn.DefaultConfig()
	th.Allowlist = syms
	if *maxOrder > 0 {
		th.MaxOrderValue = *maxOrder
	}
	cfg := backtest.Config{Thorn: th, Cash: *cash, SlippageBps: *slippage, Commission: *commission}
	res, err := backtest.Run(bars, syms, cfg)
	if err != nil {
		return err
	}
	printResult(res)
	if *sweep {
		grid, err := backtest.Sweep(bars, syms, cfg, []int{20, 50, 100, 200}, []int{10, 20, 60})
		if err != nil {
			return err
		}
		printSweep(grid)
	}
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(res, "", "  ")
		if err := os.WriteFile(*jsonOut, b, 0o644); err != nil {
			return err
		}
		fmt.Println("\nfull result:", *jsonOut)
	}
	return nil
}

func cachedHistory(ctx context.Context, dir, sym string) ([]marketdata.Candle, error) {
	path := filepath.Join(dir, strings.ReplaceAll(sym, "/", "_")+".json")
	if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) < 24*time.Hour {
		var b []marketdata.Candle
		raw, _ := os.ReadFile(path)
		if json.Unmarshal(raw, &b) == nil && len(b) > 0 {
			return b, nil
		}
	}
	b, err := (&marketdata.Yahoo{}).History(ctx, sym)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err == nil {
		raw, _ := json.Marshal(b)
		_ = os.WriteFile(path, raw, 0o644)
	}
	return b, nil
}

func pct(x float64) string { return fmt.Sprintf("%+.1f%%", x*100) }

func printResult(r *backtest.Result) {
	fmt.Printf("momentum-v1 backtest  %s → %s  (%s; common start = every symbol warmed up)\n", r.Start, r.End, strings.Join(r.Symbols, " "))
	fmt.Printf("params: %v\n\n", r.Params)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "\tTotal\tCAGR\tMax DD\tVol\tSharpe\t")
	row := func(name string, m backtest.Metrics) {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%.0f%%\t%.2f\t\n", name, pct(m.TotalReturn), pct(m.CAGR), pct(m.MaxDrawdown), m.AnnVol*100, m.Sharpe)
	}
	row("strategy", r.Strategy)
	row("buy & hold XIU", r.BenchXIU)
	row("equal-weight hold", r.BenchEqual)
	h := r.Holdout
	fmt.Fprintf(w, "— held-out %s → %s —\t\t\t\t\t\t\n", h["strategy"].From, h["strategy"].To)
	row("strategy", h["strategy"])
	row("buy & hold XIU", h["bench_buy_hold"])
	row("equal-weight hold", h["bench_equal_weight"])
	w.Flush()

	fmt.Printf("\ntrades %d · round trips %d · win rate %.0f%% · exposure %.0f%% · final equity %.2f\n",
		r.Trades, len(r.RoundTrips), r.WinRate*100, r.Exposure*100, r.FinalEquity)
	if len(r.RoundTrips) < 30 {
		fmt.Printf("⚠ only %d round trips — too few to call the edge statistically real\n", len(r.RoundTrips))
	}
	if len(r.Rejected) > 0 {
		fmt.Printf("Thorn rejections: %v\n", r.Rejected)
	}
	fmt.Println("\nyear   strategy   buy&hold   equal-wt")
	for _, y := range r.Years {
		fmt.Printf("%d  %9s  %9s  %9s\n", y.Year, pct(y.Strategy), pct(y.BuyHold), pct(y.Equal))
	}
}

func printSweep(grid []*backtest.Result) {
	fmt.Println("\nsensitivity (NOT for picking the best — a robust edge degrades gracefully around the default 50/20):")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "mean\treturn\tCAGR\tMax DD\tSharpe\tholdout CAGR\ttrades\t")
	for _, r := range grid {
		mark := ""
		if r.Params["mean_days"] == 50 && r.Params["return_days"] == 20 {
			mark = " ← default"
		}
		fmt.Fprintf(w, "%v\t%v\t%s\t%s\t%.2f\t%s\t%d\t%s\n", r.Params["mean_days"], r.Params["return_days"],
			pct(r.Strategy.CAGR), pct(r.Strategy.MaxDrawdown), r.Strategy.Sharpe, pct(r.Holdout["strategy"].CAGR), r.Trades, mark)
	}
	w.Flush()
}
