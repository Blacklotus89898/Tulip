// Command tulip is a human-approved trading agent: Claude proposes, Thorn
// disposes, the journal remembers (ADR 0001, 0002). This is the walking
// skeleton: journal, health, metrics and the read-only MCP supervision surface
// — deployed through the Homelab golden path before any trading logic lands.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/journal"
	"github.com/Blacklotus89898/Tulip/internal/mcp"
)

// version is stamped at build time (-ldflags "-X main.version=<sha>").
var version = "dev"

func main() {
	addr := flag.String("addr", ":8080", "listen address (MCP + API)")
	dbPath := flag.String("db", "data/tulip.db", "journal database path")
	mode := flag.String("mode", "paper", "broker mode: paper (the only v1 mode)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if *mode != "paper" {
		logger.Error("only paper mode exists in v1 (ADR 0001 §8)", "mode", *mode)
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
	_, _ = j.Append(ctx, "service.started", "", map[string]string{"version": version, "mode": *mode})

	token := os.Getenv("TULIP_API_TOKEN")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"status": "ok", "version": version, "mode": *mode})
	})
	mux.HandleFunc("GET /metrics", metricsHandler(j))
	mux.Handle("POST /mcp", requireToken(token, mcp.New("tulip", version, tools(j, *mode)...)))

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info("tulip listening", "addr", *addr, "db", *dbPath, "mode", *mode, "version", version,
		"mcp", token != "", "approval", "human-only (no MCP approve tool)")

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

// tools are the skeleton's read-only MCP tools. Propose / decline / kill switch
// arrive with the loop; an approve tool never does (ADR 0002 §3).
func tools(j *journal.Journal, mode string) []mcp.Tool {
	return []mcp.Tool{
		{
			Name:        "tulip_status",
			Description: "Tulip's mode, version and journal event counts by kind.",
			Call: func(ctx context.Context, _ json.RawMessage) (string, error) {
				counts, err := j.Counts(ctx)
				if err != nil {
					return "", err
				}
				b, _ := json.MarshalIndent(map[string]any{
					"version": version, "mode": mode, "approval": "human-only",
					"journal_events": counts,
				}, "", "  ")
				return string(b), nil
			},
		},
		{
			Name:        "journal_list",
			Description: "Read journal events, newest first. Optional filters: kind, cycle; limit (default 50, max 500); after_id to tail oldest-first.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind":     map[string]any{"type": "string"},
					"cycle":    map[string]any{"type": "string"},
					"limit":    map[string]any{"type": "integer"},
					"after_id": map[string]any{"type": "integer"},
				},
			},
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Kind    string `json:"kind"`
					Cycle   string `json:"cycle"`
					Limit   int    `json:"limit"`
					AfterID int64  `json:"after_id"`
				}
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("bad arguments: %w", err)
				}
				evs, err := j.List(ctx, journal.Query{Kind: a.Kind, Cycle: a.Cycle, Limit: a.Limit, AfterID: a.AfterID})
				if err != nil {
					return "", err
				}
				b, _ := json.MarshalIndent(evs, "", "  ")
				return string(b), nil
			},
		},
	}
}

func metricsHandler(j *journal.Journal) http.HandlerFunc {
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
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
