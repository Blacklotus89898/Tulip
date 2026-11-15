package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/journal"
	"github.com/Blacklotus89898/Tulip/internal/marketdata"
	"github.com/Blacklotus89898/Tulip/internal/notify"
	"github.com/Blacklotus89898/Tulip/internal/thorn"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// The US desk's defaults (ADR 0006 §3, operator decision 2026-11-15).
func TestUSSpecDefaults(t *testing.T) {
	sp := usSpec(quiet, "data/tulip.db", "", notify.Ntfy{})
	if strings.Join(sp.Symbols, ",") != "IVV,EFA,IEF" || sp.Strategy != "trend-filter-200" || sp.Benchmark != "IVV" {
		t.Errorf("universe/strategy/benchmark = %v %s %s", sp.Symbols, sp.Strategy, sp.Benchmark)
	}
	if sp.Mode != "paper" || sp.Exchange != calendar.NYSE || sp.DBPath != filepath.Join("data", "tulip-us.db") {
		t.Errorf("mode %s exchange %v db %s", sp.Mode, sp.Exchange, sp.DBPath)
	}
	th := sp.Thorn
	if strings.Join(th.Allowlist, ",") != "IVV,EFA,IEF" || th.MaxWeightBySymbol["IEF"] != 1 {
		t.Errorf("thorn allowlist %v by-symbol %v", th.Allowlist, th.MaxWeightBySymbol)
	}
	// trend-filter-200 holds each risk ETF at 1/2 of a 10k book: Thorn must allow it.
	if th.MaxPositionWeight < 0.5 || th.MaxOrderValue < 5000 {
		t.Errorf("caps too tight for the plan: weight %.2f order %.0f", th.MaxPositionWeight, th.MaxOrderValue)
	}
}

func testDesks(t *testing.T) []*deskEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	md := &marketdata.Yahoo{}
	tsx, err := buildDesk(ctx, quiet, md, deskSpec{Name: "tsx", DBPath: filepath.Join(dir, "tulip.db"), Mode: "paper",
		Symbols: []string{"XIU.TO", "VFV.TO", "XEF.TO", "ZAG.TO"}, Strategy: "trend-filter-200",
		Thorn: thorn.DefaultConfig(), Exchange: calendar.TSX, Benchmark: "XIU.TO", Shadow: true})
	if err != nil {
		t.Fatal(err)
	}
	us, err := buildDesk(ctx, quiet, md, usSpec(quiet, filepath.Join(dir, "tulip.db"), "", notify.Ntfy{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tsx.J.Close(); us.J.Close() })
	return []*deskEnv{tsx, us}
}

func TestBuildDeskKeepsTheTSXDeskAsItWas(t *testing.T) {
	envs := testDesks(t)
	tsx, us := envs[0], envs[1]
	if tsx.D.Cal != nil || tsx.D.Cfg.Benchmark != "" || tsx.Prefix() != "" {
		t.Errorf("TSX desk must keep its zero values: cal %v benchmark %q prefix %q", tsx.D.Cal, tsx.D.Cfg.Benchmark, tsx.Prefix())
	}
	if us.D.Cal != calendar.NYSE || us.D.Cfg.Benchmark != "IVV" || us.Prefix() != "/us" {
		t.Errorf("US desk: cal %v benchmark %q prefix %q", us.D.Cal, us.D.Cfg.Benchmark, us.Prefix())
	}
	ctx := context.Background()
	for _, e := range envs {
		evs, err := e.J.List(ctx, journal.Query{Kind: "service.started", Limit: 5})
		if err != nil || len(evs) != 1 {
			t.Fatalf("%s: service.started events %d (%v)", e.Name, len(evs), err)
		}
		var p map[string]any
		_ = json.Unmarshal(evs[0].Payload, &p)
		if _, has := p["desk"]; has != (e.Name != "tsx") {
			t.Errorf("%s: service.started desk key present=%v (the TSX payload must be unchanged)", e.Name, has)
		}
	}
}

func TestBuildDeskRefusesBadAutoExecute(t *testing.T) {
	sp := usSpec(quiet, filepath.Join(t.TempDir(), "tulip.db"), "", notify.Ntfy{})
	sp.AutoExecute = "live"
	if _, err := buildDesk(context.Background(), quiet, &marketdata.Yahoo{}, sp); err == nil {
		t.Error("TULIP_US_AUTOEXECUTE=live must refuse to start")
	}
	sp.AutoExecute, sp.Mode = "paper", "copilot"
	if _, err := buildDesk(context.Background(), quiet, &marketdata.Yahoo{}, sp); err == nil {
		t.Error("auto-execute outside paper mode must refuse to start")
	}
}

func call(t *testing.T, envs []*deskEnv, name, args string) (string, error) {
	t.Helper()
	for _, tool := range tools(envs) {
		if tool.Name == name {
			return tool.Call(context.Background(), json.RawMessage(args))
		}
	}
	t.Fatalf("no tool %s", name)
	return "", nil
}

func TestKillSwitchArmsEveryDesk(t *testing.T) {
	envs := testDesks(t)
	out, err := call(t, envs, "kill_switch_arm", `{"reason":"test","desk":"tsx"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range envs {
		st, err := e.D.LoadState(context.Background())
		if err != nil || !st.KillSwitch {
			t.Errorf("%s: kill switch not armed (%v)", e.Name, err)
		}
	}
	if !strings.Contains(out, "/us/killswitch") {
		t.Errorf("reply should name every disarm page: %s", out)
	}
}

func TestToolsTakeADeskArgument(t *testing.T) {
	envs := testDesks(t)
	for args, want := range map[string]string{`{}`: `"tsx"`, ``: `"tsx"`, `{"desk":"us"}`: `"us"`} {
		out, err := call(t, envs, "tulip_status", args)
		if err != nil || !strings.Contains(out, `"desk": `+want) {
			t.Errorf("status(%s): want desk %s, got %v %s", args, want, err, out)
		}
	}
	if _, err := call(t, envs, "portfolio", `{"desk":"lse"}`); err == nil {
		t.Error("an unknown desk must be an error, never a silent default")
	}
	for _, tool := range tools(envs) {
		props, _ := tool.InputSchema["properties"].(map[string]any)
		if _, ok := props["desk"]; !ok {
			t.Errorf("%s does not advertise the desk argument", tool.Name)
		}
	}
}
