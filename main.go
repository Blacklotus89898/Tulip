// Command tulip is a human-approved trading agent: Claude proposes, Thorn
// disposes, the journal remembers (ADR 0001). This skeleton wires flags and
// lifecycle only — the loop, Thorn, and the journal land next.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address (MCP + API)")
	dbPath := flag.String("db", "data/tulip.db", "journal database path")
	mode := flag.String("mode", "paper", "broker mode: paper (the only v1 mode)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// v1 stub: until the loop lands, tulip starts, says who it is, and waits
	// for shutdown. Everything after this line is increment one.
	logger.Info("tulip starting", "addr", *addr, "db", *dbPath, "mode", *mode,
		"approval", "human", "thorn", "armed-not-yet-wired")

	<-ctx.Done()
	logger.Info("tulip shutting down")
}
