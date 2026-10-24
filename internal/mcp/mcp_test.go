package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func call(t *testing.T, s *Server, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)))
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("bad JSON %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, out
}

func TestMCPHandshakeAndTools(t *testing.T) {
	s := New("tulip", "test",
		Tool{Name: "echo", Description: "echo", Call: func(_ context.Context, a json.RawMessage) (string, error) { return string(a), nil }},
		Tool{Name: "boom", Call: func(context.Context, json.RawMessage) (string, error) { return "", errors.New("nope") }},
	)

	_, init := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	res := init["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" || !strings.Contains(res["instructions"].(string), "can NOT approve") {
		t.Errorf("initialize = %v", res)
	}
	if code, _ := call(t, s, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Errorf("notification: %d", code)
	}
	_, list := call(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if n := len(list["result"].(map[string]any)["tools"].([]any)); n != 2 {
		t.Errorf("tools/list: %d tools", n)
	}
	_, echo := call(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"x":1}}}`)
	if txt := echo["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; txt != `{"x":1}` {
		t.Errorf("echo = %v", txt)
	}
	_, boom := call(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"boom"}}`)
	if boom["result"].(map[string]any)["isError"] != true {
		t.Errorf("tool errors are results with isError: %v", boom)
	}
	_, unknown := call(t, s, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"approve"}}`)
	if unknown["error"] == nil {
		t.Errorf("unknown tool must be a protocol error: %v", unknown)
	}
}
