package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The MCP surface is the credential Claude Code holds. It must never grow a
// tool that approves, executes or disarms (ADR 0002 §3) — this list is the
// contract; extending it is a deliberate, reviewed change.
var allowedTools = map[string]bool{
	"tulip_status": true, "journal_list": true, "portfolio": true, "suggestions_pending": true,
	"propose": true, "review_suggestion": true, "decline": true, "kill_switch_arm": true, "cycle_run": true,
	"broker_account": true, // read-only (ADR 0003)
}

func TestMCPToolsAreAllowlisted(t *testing.T) {
	for _, tool := range tools(nil, nil, nil) {
		if !allowedTools[tool.Name] {
			t.Errorf("tool %q is not on the reviewed allowlist", tool.Name)
		}
		if regexp.MustCompile(`approve|execute|disarm|fill|order`).MatchString(tool.Name) {
			t.Errorf("tool %q looks like it moves money", tool.Name)
		}
	}
}

// broker.Fill for the strategy book is reachable only from desk.Approve:
// no other file in the service may call it.
func TestFillOnlyFromTheApprovalPath(t *testing.T) {
	for _, path := range []string{"main.go", "tools.go", "internal/loop/loop.go", "internal/web/web.go", "internal/mcp/mcp.go", "internal/notify/notify.go", "copilot.go", "internal/desk/copilot.go"} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), ".Fill(") {
			t.Errorf("%s calls .Fill( — fills must only happen in desk.Approve (and the benchmark)", path)
		}
	}
}

// The Questrade client is read-only (ADR 0003): no order endpoint, no POST.
func TestQuestradeClientIsReadOnly(t *testing.T) {
	src, err := os.ReadFile("internal/questrade/client.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"/orders", "MethodPost", "MethodPut", "MethodDelete", `"POST"`} {
		if strings.Contains(string(src), bad) {
			t.Errorf("internal/questrade/client.go contains %q — the broker client must stay read-only", bad)
		}
	}
}
