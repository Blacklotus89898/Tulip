// Command tulip is a human-gated trading co-pilot: rules propose, Claude
// reviews, Thorn disposes, the operator approves (paper) or places the order
// themselves in Questrade (co-pilot), the journal remembers (ADR 0001–0003).
//
//	tulip [flags]                 run the service
//	tulip hash-passphrase         read a passphrase on stdin, print its hash
//	                              (for TULIP_APPROVAL_PASSPHRASE_HASH)
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
	"github.com/Blacklotus89898/Tulip/internal/desk"
	"github.com/Blacklotus89898/Tulip/internal/journal"
	"github.com/Blacklotus89898/Tulip/internal/loop"
	"github.com/Blacklotus89898/Tulip/internal/marketdata"
	"github.com/Blacklotus89898/Tulip/internal/mcp"
	"github.com/Blacklotus89898/Tulip/internal/notify"
	"github.com/Blacklotus89898/Tulip/internal/questrade"
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
	addr := flag.String("addr", ":8080", "listen address (pages, MCP, API)")
	dbPath := flag.String("db", "data/tulip.db", "journal database path")
	mode := flag.String("mode", envOr("TULIP_MODE", "paper"), "paper | copilot (read-only Questrade, ADR 0003)")
	symbols := flag.String("symbols", "XIU.TO,VFV.TO,XEF.TO,ZAG.TO", "symbols the rules scan (must be on Thorn's allowlist)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if *mode != "paper" && *mode != "copilot" {
		logger.Error("mode must be paper or copilot", "mode", *mode)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		logger.Error("create data dir", "err", err)
		os.Exit(1)
	}
	j, err := journal.Open(ctx, *dbPath)
	if err != nil {
		logger.Error("open journal", "err", err)
		os.Exit(1)
	}
	defer j.Close()

	md := &marketdata.Yahoo{}
	passHash := os.Getenv("TULIP_APPROVAL_PASSPHRASE_HASH")
	thornCfg := thorn.DefaultConfig()
	envFloat(logger, "TULIP_THORN_MAX_ORDER_VALUE", &thornCfg.MaxOrderValue)
	envFloat(logger, "TULIP_THORN_DAILY_LOSS_CAP", &thornCfg.DailyLossCap)
	envFloat(logger, "TULIP_THORN_MAX_POSITION_WEIGHT", &thornCfg.MaxPositionWeight)
	d := &desk.Desk{
		J: j, MD: md, Broker: broker.Paper{SlippageBps: 10},
		Cfg:            desk.Config{Thorn: thornCfg},
		PassphraseHash: passHash, Limiter: &approval.Limiter{},
	}
	var acct *questradeAccount
	if *mode == "copilot" {
		number := os.Getenv("TULIP_QUESTRADE_ACCOUNT")
		if number == "" {
			logger.Error("copilot mode needs TULIP_QUESTRADE_ACCOUNT (the one account Tulip reads)")
			os.Exit(2)
		}
		// The sealed token only bootstraps; the rotated one lives next to the
		// journal (ADR 0003 §6). Never logged.
		qc := &questrade.Client{StateDir: filepath.Dir(*dbPath), Bootstrap: strings.TrimSpace(os.Getenv("QUESTRADE_REFRESH_TOKEN"))}
		acct = newQuestradeAccount(qc, number, j)
		acct.log = logger
		d.Account = acct
	}
	pager := notify.Ntfy{
		URL: os.Getenv("TULIP_NTFY_URL"), Topic: envOr("TULIP_NTFY_TOPIC", "tulip-approvals"),
		PublicURL: envOr("TULIP_PUBLIC_URL", "http://192.168.0.108:30995"),
	}
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
		logger.Error("fund books", "err", err)
		os.Exit(1)
	}
	lp := &loop.Loop{Desk: d, MD: md, Strategy: strategy.Momentum{}, Symbols: splitList(*symbols), Log: logger}
	_, _ = j.Append(ctx, "service.started", "", map[string]any{
		"version": version, "mode": *mode, "symbols": lp.Symbols, "thorn": thornCfg,
		"approvals_enabled": passHash != "", "notify": pager.URL != "", "brain": lp.Brain != nil,
	})
	go lp.Run(ctx)
	if acct != nil {
		go reconcileLoop(ctx, d, acct, 3*time.Minute, logger)
	}

	token := os.Getenv("TULIP_API_TOKEN")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":%q,"mode":%q}`+"\n", version, *mode)
	})
	mux.HandleFunc("GET /metrics", metricsHandler(j, d, acct))
	mux.Handle("POST /mcp", requireToken(token, mcp.New("tulip", version, tools(d, lp, j)...)))
	web.Register(mux, d) // the operator's approval page (passphrase-gated)

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info("tulip listening", "addr", *addr, "db", *dbPath, "mode", *mode, "version", version,
		"mcp", token != "", "approvals", passHash != "", "notify", pager.URL != "")

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

func metricsHandler(j *journal.Journal, d *desk.Desk, acct *questradeAccount) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		counts, err := j.Counts(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		kinds := make([]string, 0, len(counts))
		for k := range counts {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		var b strings.Builder
		b.WriteString("# HELP tulip_journal_events_total Journal events by kind.\n# TYPE tulip_journal_events_total counter\n")
		for _, k := range kinds {
			fmt.Fprintf(&b, "tulip_journal_events_total{kind=\"%s\"} %d\n", strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(k), counts[k])
		}
		fmt.Fprintf(&b, "# HELP tulip_build_info Running build.\n# TYPE tulip_build_info gauge\ntulip_build_info{version=\"%s\"} 1\n", version)
		if st, err := d.LoadState(r.Context()); err == nil {
			kill := 0
			if st.KillSwitch {
				kill = 1
			}
			fmt.Fprintf(&b, "# HELP tulip_kill_switch 1 while the kill switch is armed.\n# TYPE tulip_kill_switch gauge\ntulip_kill_switch %d\n", kill)
			fmt.Fprintf(&b, "# HELP tulip_suggestions_pending Suggestions awaiting the operator.\n# TYPE tulip_suggestions_pending gauge\ntulip_suggestions_pending %d\n", len(st.Pending))
		}
		if acct != nil {
			fmt.Fprintf(&b, "# HELP tulip_broker_auth_ok Questrade auth: 1 ok, 0 failing, -1 not tried yet.\n# TYPE tulip_broker_auth_ok gauge\ntulip_broker_auth_ok %d\n", acct.AuthGauge())
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	}
}
