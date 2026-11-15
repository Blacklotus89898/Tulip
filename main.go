// Command tulip is a human-gated trading co-pilot: rules propose, Claude
// reviews, Thorn disposes, the operator approves (paper) or places the order
// themselves in Questrade (co-pilot), the journal remembers (ADR 0001–0003).
// A second, optional desk trades US-listed ETFs (ADR 0006).
//
//	tulip [flags]                 run the service
//	tulip hash-passphrase         read a passphrase on stdin, print its hash
//	                              (for TULIP_APPROVAL_PASSPHRASE_HASH)
//	tulip backtest [-sweep] ...   replay momentum-v1 + Thorn on adjusted history
package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/approval"
	"github.com/Blacklotus89898/Tulip/internal/broker"
	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/desk"
	"github.com/Blacklotus89898/Tulip/internal/journal"
	"github.com/Blacklotus89898/Tulip/internal/loop"
	"github.com/Blacklotus89898/Tulip/internal/marketdata"
	"github.com/Blacklotus89898/Tulip/internal/mcp"
	"github.com/Blacklotus89898/Tulip/internal/notify"
	"github.com/Blacklotus89898/Tulip/internal/questrade"
	"github.com/Blacklotus89898/Tulip/internal/shadow"
	"github.com/Blacklotus89898/Tulip/internal/strategy"
	"github.com/Blacklotus89898/Tulip/internal/thorn"
	"github.com/Blacklotus89898/Tulip/internal/web"
)

// version is stamped at build time (-ldflags "-X main.version=<sha>").
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash-passphrase" {
		hashPassphrase()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "backtest" {
		if err := runBacktest(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "backtest:", err)
			os.Exit(1)
		}
		return
	}
	addr := flag.String("addr", ":8080", "listen address (pages, MCP, API)")
	dbPath := flag.String("db", "data/tulip.db", "journal database path")
	mode := flag.String("mode", envOr("TULIP_MODE", "paper"), "paper | copilot (read-only Questrade, ADR 0003)")
	symbols := flag.String("symbols", "XIU.TO,VFV.TO,XEF.TO,ZAG.TO", "symbols the rules scan (must be on Thorn's allowlist); the LAST is the safe asset for trend-filter-200")
	strat := flag.String("strategy", envOr("TULIP_STRATEGY", "trend-filter-200"), "a pre-registered allocator (trend-filter-200, equal-weight, trend-ensemble, dual-momentum-12m, inverse-vol-60) | momentum-v1 (retired, see docs/backtests)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if *mode != "paper" && *mode != "copilot" {
		logger.Error("mode must be paper or copilot", "mode", *mode)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	md := &marketdata.Yahoo{}
	passHash := os.Getenv("TULIP_APPROVAL_PASSPHRASE_HASH")
	ntfyURL := os.Getenv("TULIP_NTFY_URL")
	topic := envOr("TULIP_NTFY_TOPIC", "tulip-approvals")
	publicURL := envOr("TULIP_PUBLIC_URL", "http://192.168.0.108:30995")

	// The TSX desk: exactly the configuration Tulip always had (ADR 0001–0005).
	tsx, err := buildDesk(ctx, logger, md, deskSpec{
		Name: "tsx", DBPath: *dbPath, Mode: *mode, Symbols: splitList(*symbols), Strategy: *strat,
		AutoExecute: os.Getenv("TULIP_AUTOEXECUTE"), AutoExecuteEnv: "TULIP_AUTOEXECUTE",
		Thorn:    parseThorn(logger, "TULIP_THORN_", thorn.DefaultConfig()),
		Exchange: calendar.TSX, Benchmark: "XIU.TO", ChartCommission: 4.95,
		PassHash: passHash, Shadow: envOr("TULIP_SHADOW", "on") != "off",
		Pager: notify.Ntfy{URL: ntfyURL, Topic: topic, PublicURL: publicURL},
	})
	if err != nil {
		logger.Error("tsx desk", "err", err)
		os.Exit(2)
	}
	envs := []*deskEnv{tsx}

	// The US desk (ADR 0006): off unless TULIP_US=on. Its own journal, NYSE
	// calendar, USD universe and Thorn limits; synthetic paper fills until the
	// IBKR broker exists.
	switch v := os.Getenv("TULIP_US"); v {
	case "", "off":
	case "on":
		us, err := buildDesk(ctx, logger, md, usSpec(logger, *dbPath, passHash, notify.Ntfy{
			URL: ntfyURL, Topic: topic, PublicURL: strings.TrimRight(publicURL, "/") + "/us"}))
		if err != nil {
			logger.Error("us desk", "err", err)
			os.Exit(2)
		}
		envs = append(envs, us)
	default:
		logger.Error("TULIP_US must be empty, off or on", "value", v)
		os.Exit(2)
	}
	for _, e := range envs {
		defer e.J.Close()
		go e.LP.Run(ctx)
		if e.Acct != nil {
			go reconcileLoop(ctx, e.D, e.Acct, 3*time.Minute, logger)
		}
	}

	token := os.Getenv("TULIP_API_TOKEN")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":%q,"mode":%q}`+"\n", version, *mode)
	})
	mux.HandleFunc("GET /metrics", metricsHandler(envs))
	mux.Handle("POST /mcp", requireToken(token, mcp.New("tulip", version, tools(envs)...)))
	for _, e := range envs {
		if e.Prefix() == "" {
			registerPages(mux, e, md) // the TSX desk keeps the original paths
			continue
		}
		sub := http.NewServeMux()
		registerPages(sub, e, md)
		mux.Handle(e.Prefix()+"/", http.StripPrefix(e.Prefix(), sub)) // pages use relative links
	}

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	for _, e := range envs[1:] {
		logger.Info("tulip desk", "desk", e.Name, "mode", e.Mode, "strategy", e.LP.StrategyName(), "symbols", e.LP.Symbols,
			"auto_execute", e.D.AutoPaper, "exchange", e.D.Cal.String())
	}
	logger.Info("tulip listening", "addr", *addr, "db", *dbPath, "mode", *mode, "strategy", tsx.LP.StrategyName(), "version", version,
		"mcp", token != "", "approvals", passHash != "", "auto_execute", tsx.D.AutoPaper, "notify", ntfyURL != "")

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "err", err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// deskEnv is one trading desk: its own journal, desk, loop and pages
// (ADR 0006 §1). The TSX desk is the original; others live under /<name>.
type deskEnv struct {
	Name            string // "tsx" | "us"
	Mode            string // paper | copilot
	J               *journal.Journal
	D               *desk.Desk
	LP              *loop.Loop
	Acct            *questradeAccount // TSX co-pilot only
	Benchmark       string
	ChartCommission float64
}

// Prefix is the desk's URL prefix: "" for TSX (the original paths), "/us".
func (e *deskEnv) Prefix() string {
	if e.Name == "tsx" {
		return ""
	}
	return "/" + e.Name
}

type deskSpec struct {
	Name, DBPath, Mode, Strategy string
	Symbols                      []string
	AutoExecute, AutoExecuteEnv  string
	Thorn                        thorn.Config
	Exchange                     *calendar.Exchange
	Benchmark                    string
	ChartCommission              float64
	PassHash                     string
	Shadow                       bool
	Pager                        notify.Ntfy
}

// usSpec is the US desk (ADR 0006): IVV, EFA, IEF (safe last), trend-filter-200,
// buy-and-hold IVV, NYSE calendar, paper only. Every value can be overridden
// with TULIP_US_* variables.
func usSpec(logger *slog.Logger, tsxDB, passHash string, pager notify.Ntfy) deskSpec {
	syms := splitList(envOr("TULIP_US_SYMBOLS", "IVV,EFA,IEF"))
	th := thorn.DefaultConfig()
	th.Allowlist = syms
	// Two risk ETFs at 1/2 each: the TSX desk's 0.4 cap would refuse the plan,
	// and the safe asset may hold 100% when every trend is down.
	th.MaxOrderValue, th.MaxPositionWeight, th.MaxSuggestionsPerDay = 10000, 0.55, 8
	if len(syms) > 0 {
		th.MaxWeightBySymbol = map[string]float64{syms[len(syms)-1]: 1}
	}
	return deskSpec{
		Name: "us", DBPath: envOr("TULIP_US_DB", filepath.Join(filepath.Dir(tsxDB), "tulip-us.db")),
		Mode: "paper", Symbols: syms, Strategy: envOr("TULIP_US_STRATEGY", "trend-filter-200"),
		AutoExecute: os.Getenv("TULIP_US_AUTOEXECUTE"), AutoExecuteEnv: "TULIP_US_AUTOEXECUTE",
		Thorn:    parseThorn(logger, "TULIP_US_THORN_", th),
		Exchange: calendar.NYSE, Benchmark: envOr("TULIP_US_BENCHMARK", "IVV"), ChartCommission: 1.00,
		PassHash: passHash, Shadow: envOr("TULIP_US_SHADOW", "on") != "off", Pager: pager,
	}
}

// buildDesk opens the desk's journal and wires its desk and loop. For the
// TSX desk every value and journal payload is what Tulip always wrote.
func buildDesk(ctx context.Context, logger *slog.Logger, md *marketdata.Yahoo, sp deskSpec) (*deskEnv, error) {
	// Paper training (ADR 0005): the operator lets the rules' suggestions fill
	// the paper book without approval. Only the exact value "paper" enables
	// it, and never outside paper mode.
	autoPaper := false
	switch sp.AutoExecute {
	case "", "off":
	case "paper":
		if sp.Mode != "paper" {
			return nil, fmt.Errorf("%s=paper is only allowed in paper mode (mode %s)", sp.AutoExecuteEnv, sp.Mode)
		}
		autoPaper = true
	default:
		return nil, fmt.Errorf("%s must be empty, off or paper, not %q", sp.AutoExecuteEnv, sp.AutoExecute)
	}
	if err := os.MkdirAll(filepath.Dir(sp.DBPath), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	j, err := journal.Open(ctx, sp.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	isTSX := sp.Name == "tsx"
	cfg := desk.Config{Thorn: sp.Thorn}
	d := &desk.Desk{
		J: j, MD: md, Broker: broker.Paper{SlippageBps: 10}, Cfg: cfg,
		PassphraseHash: sp.PassHash, Limiter: &approval.Limiter{}, AutoPaper: autoPaper,
	}
	if !isTSX { // the TSX desk keeps its zero values (TSX calendar, XIU benchmark)
		d.Cfg.Benchmark, d.Cal = sp.Benchmark, sp.Exchange
	}
	e := &deskEnv{Name: sp.Name, Mode: sp.Mode, J: j, D: d, Benchmark: sp.Benchmark, ChartCommission: sp.ChartCommission}
	if sp.Mode == "copilot" {
		number := os.Getenv("TULIP_QUESTRADE_ACCOUNT")
		if number == "" {
			return nil, errors.New("copilot mode needs TULIP_QUESTRADE_ACCOUNT (the one account Tulip reads)")
		}
		// The sealed token only bootstraps; the rotated one lives next to the
		// journal (ADR 0003 §6). Never logged.
		qc := &questrade.Client{StateDir: filepath.Dir(sp.DBPath), Bootstrap: strings.TrimSpace(os.Getenv("QUESTRADE_REFRESH_TOKEN"))}
		e.Acct = newQuestradeAccount(qc, number, j)
		e.Acct.log = logger
		d.Account = e.Acct
	}
	pager := sp.Pager
	d.Notify = func(s desk.Suggestion) {
		go func() { // never block the desk on the network
			nctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := pager.Suggestion(nctx, s); err != nil {
				_, _ = j.Append(nctx, "notify.error", "", map[string]string{"id": s.ID, "error": err.Error()})
				return
			}
			_, _ = j.Append(nctx, "suggestion.notified", "", map[string]string{"id": s.ID, "topic": pager.Topic})
		}()
	}
	if err := d.EnsureFunded(ctx); err != nil {
		return nil, fmt.Errorf("fund books: %w", err)
	}
	log := logger
	if !isTSX {
		log = logger.With("desk", sp.Name)
	}
	lp := &loop.Loop{Desk: d, MD: md, Strategy: strategy.Momentum{}, Symbols: sp.Symbols, Log: log, Hist: md}
	if sp.Strategy != "momentum-v1" {
		a, ok := strategy.ByName(lp.Symbols, sp.Strategy)
		if !ok {
			return nil, fmt.Errorf("unknown strategy %q (pre-registered: trend-filter-200, equal-weight, trend-ensemble, dual-momentum-12m, inverse-vol-60; universe = risk symbols then the safe one)", sp.Strategy)
		}
		lp.Allocator = a
	}
	// Shadow books (ADR 0004): every pre-registered candidate, unapproved, on
	// the live schedule.
	if sp.Shadow {
		lp.Shadow = &shadow.Runner{J: j, Allocators: strategy.Candidates(lp.Symbols), Symbols: lp.Symbols, Thorn: sp.Thorn}
	}
	// Monthly scorecard push (after each month's shadow rebalance), linking
	// to the read-only scorecard page.
	tag := ""
	if !isTSX {
		tag = "[" + strings.ToUpper(sp.Name) + "] "
	}
	lp.Report = func(ctx context.Context, title, body string) error {
		return pager.Message(ctx, tag+title, body, "/scorecard")
	}
	e.LP = lp
	started := map[string]any{
		"version": version, "mode": sp.Mode, "symbols": lp.Symbols, "thorn": sp.Thorn, "strategy": lp.StrategyName(),
		"approvals_enabled": sp.PassHash != "", "notify": pager.URL != "", "brain": lp.Brain != nil,
		"auto_execute": autoPaper,
	}
	if !isTSX {
		started["desk"], started["exchange"], started["benchmark"] = sp.Name, sp.Exchange.Name, sp.Benchmark
	}
	_, _ = j.Append(ctx, "service.started", "", started)
	return e, nil
}

// registerPages mounts one desk's approval, kill-switch, scorecard and chart
// pages on mux.
func registerPages(mux *http.ServeMux, e *deskEnv, md *marketdata.Yahoo) {
	j, lp := e.J, e.LP
	web.Register(mux, e.D)                                                             // the operator's approval page (passphrase-gated)
	web.RegisterScorecard(mux, func(ctx context.Context) (*web.ScorecardData, error) { // read-only (ADR 0004)
		card, err := shadow.Build(ctx, j)
		if err != nil {
			return nil, err
		}
		dec, asOf, unavailable, err := lp.CandidateDecisions(ctx)
		if err != nil {
			return nil, err
		}
		return &web.ScorecardData{Live: lp.StrategyName(), Card: card, Candidates: dec, AsOf: asOf, Unavailable: unavailable}, nil
	})
	web.RegisterCharts(mux, web.ChartsSources{ // read-only: the scorecard drawn as charts and diagrams
		Live: func(ctx context.Context) (*web.LiveCharts, error) {
			since, books, err := shadow.Series(ctx, j)
			if err != nil {
				return nil, err
			}
			dec, asOf, _, err := lp.CandidateDecisions(ctx)
			if err != nil {
				return nil, err
			}
			counts, err := j.Counts(ctx)
			if err != nil {
				return nil, err
			}
			return &web.LiveCharts{Live: lp.StrategyName(), Symbols: lp.Symbols, Since: since, Books: books,
				Decisions: dec, AsOf: asOf, Counts: counts}, nil
		},
		Backtest: func(ctx context.Context) (*web.BacktestSet, error) {
			return chartBacktest(ctx, md, lp.Symbols, e.Benchmark, lp.StrategyName(), e.ChartCommission)
		},
	})
}

// parseThorn applies the operator's overrides (env vars starting with prefix)
// to base: MAX_ORDER_VALUE, DAILY_LOSS_CAP, MAX_POSITION_WEIGHT,
// MAX_SUGGESTIONS_PER_DAY, MAX_WEIGHT_BY_SYMBOL ("ZAG.TO=1,XIU.TO=0.4").
func parseThorn(logger *slog.Logger, prefix string, base thorn.Config) thorn.Config {
	c := base
	envFloat(logger, prefix+"MAX_ORDER_VALUE", &c.MaxOrderValue)
	envFloat(logger, prefix+"DAILY_LOSS_CAP", &c.DailyLossCap)
	envFloat(logger, prefix+"MAX_POSITION_WEIGHT", &c.MaxPositionWeight)
	if v := os.Getenv(prefix + "MAX_SUGGESTIONS_PER_DAY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.MaxSuggestionsPerDay = n
		}
	}
	// Per-symbol position caps (the trend filter's safe asset may hold up to 100%).
	for _, kv := range splitList(os.Getenv(prefix + "MAX_WEIGHT_BY_SYMBOL")) {
		k, v, ok := strings.Cut(kv, "=")
		f, err := strconv.ParseFloat(v, 64)
		if !ok || err != nil || f <= 0 || f > 1 {
			logger.Error("ignoring invalid "+prefix+"MAX_WEIGHT_BY_SYMBOL entry", "entry", kv)
			continue
		}
		m := map[string]float64{}
		for s, w := range c.MaxWeightBySymbol {
			m[s] = w
		}
		m[strings.TrimSpace(k)] = f
		c.MaxWeightBySymbol = m
	}
	return c
}

// hashPassphrase reads one line from stdin and prints its PBKDF2 hash. Run it
// on your own machine; only the hash goes into the cluster.
func hashPassphrase() {
	fmt.Fprintln(os.Stderr, "Passphrase (12+ chars), then Enter:")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(os.Stderr, "no input")
		os.Exit(1)
	}
	h, err := approval.HashPassphrase(strings.TrimRight(line, "\r\n"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(h)
}

// envFloat overrides *dst from a positive number in env k.
func envFloat(log *slog.Logger, k string, dst *float64) {
	v := os.Getenv(k)
	if v == "" {
		return
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		log.Error("ignoring invalid number", "env", k)
		return
	}
	*dst = f
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// requireToken guards a handler with a bearer token; no token configured →
// 503 (an open port never means open access — the Petal rule).
func requireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			http.Error(w, "TULIP_API_TOKEN not configured", http.StatusServiceUnavailable)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tulip"`)
			http.Error(w, "missing or invalid bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// metricsHandler: the TSX desk's series keep their original unlabelled names,
// so the existing alert rules keep working; other desks add desk="<name>".
func metricsHandler(envs []*deskEnv) http.HandlerFunc {
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	labels := func(e *deskEnv, extra string) string {
		var parts []string
		if e.Name != "tsx" {
			parts = append(parts, `desk="`+esc.Replace(e.Name)+`"`)
		}
		if extra != "" {
			parts = append(parts, extra)
		}
		if len(parts) == 0 {
			return ""
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("# HELP tulip_journal_events_total Journal events by kind.\n# TYPE tulip_journal_events_total counter\n")
		for _, e := range envs {
			counts, err := e.J.Counts(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			kinds := make([]string, 0, len(counts))
			for k := range counts {
				kinds = append(kinds, k)
			}
			sort.Strings(kinds)
			for _, k := range kinds {
				fmt.Fprintf(&b, "tulip_journal_events_total%s %d\n", labels(e, `kind="`+esc.Replace(k)+`"`), counts[k])
			}
		}
		fmt.Fprintf(&b, "# HELP tulip_build_info Running build.\n# TYPE tulip_build_info gauge\ntulip_build_info{version=\"%s\"} 1\n", version)
		var kills, pending strings.Builder
		for _, e := range envs {
			st, err := e.D.LoadState(r.Context())
			if err != nil {
				continue
			}
			kill := 0
			if st.KillSwitch {
				kill = 1
			}
			fmt.Fprintf(&kills, "tulip_kill_switch%s %d\n", labels(e, ""), kill)
			fmt.Fprintf(&pending, "tulip_suggestions_pending%s %d\n", labels(e, ""), len(st.Pending))
		}
		if kills.Len() > 0 {
			b.WriteString("# HELP tulip_kill_switch 1 while the kill switch is armed.\n# TYPE tulip_kill_switch gauge\n" + kills.String())
			b.WriteString("# HELP tulip_suggestions_pending Suggestions awaiting the operator.\n# TYPE tulip_suggestions_pending gauge\n" + pending.String())
		}
		for _, e := range envs {
			if e.Acct != nil {
				fmt.Fprintf(&b, "# HELP tulip_broker_auth_ok Questrade auth: 1 ok, 0 failing, -1 not tried yet.\n# TYPE tulip_broker_auth_ok gauge\ntulip_broker_auth_ok %d\n", e.Acct.AuthGauge())
			}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	}
}
