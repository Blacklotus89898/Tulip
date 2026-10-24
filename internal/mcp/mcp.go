// Package mcp is Tulip's supervision surface (ADR 0001 §1): an MCP server over
// streamable HTTP (POST /mcp, JSON responses) for Claude Code and other
// clients. Tools are registered by the service; by design (ADR 0002 §3) no tool
// can approve an order — approval is a human-channel action only.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Tool is one MCP tool.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Call returns text content for the model, or an error (reported to the
	// client as a tool error, not a protocol error).
	Call func(ctx context.Context, args json.RawMessage) (string, error)
}

// Server dispatches JSON-RPC requests to registered tools.
type Server struct {
	name, version string
	tools         []Tool
	byName        map[string]Tool
}

// New creates a server.
func New(name, version string, tools ...Tool) *Server {
	s := &Server{name: name, version: version, byName: map[string]Tool{}}
	for _, t := range tools {
		s.tools = append(s.tools, t)
		s.byName[t.Name] = t
	}
	return s
}

var supportedVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// ServeHTTP handles POST /mcp. Notifications (no id) get 202 and no body.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "MCP over streamable HTTP: POST JSON-RPC to this endpoint", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted) // notification
		return
	}
	writeRPC(w, s.handle(r.Context(), req))
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handle(ctx context.Context, req rpcRequest) rpcResponse {
	ok := func(result any) rpcResponse { return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result} }
	fail := func(code int, msg string) rpcResponse {
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: msg}}
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := supportedVersions[len(supportedVersions)-1]
		for _, sv := range supportedVersions {
			if sv == p.ProtocolVersion {
				v = sv
			}
		}
		return ok(map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.name, "version": s.version},
			"instructions": "Tulip is a human-approved paper-trading agent. You can read the journal, " +
				"propose, decline and arm the kill switch; you can NOT approve orders — approval is the " +
				"operator's, through their own channel (ADR 0002 §3).",
		})
	case "ping":
		return ok(map[string]any{})
	case "tools/list":
		list := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			list = append(list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": schema})
		}
		return ok(map[string]any{"tools": list})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fail(-32602, "invalid params")
		}
		t, found := s.byName[p.Name]
		if !found {
			return fail(-32602, fmt.Sprintf("unknown tool %q", p.Name))
		}
		if len(p.Arguments) == 0 {
			p.Arguments = json.RawMessage("{}")
		}
		text, err := t.Call(ctx, p.Arguments)
		if err != nil {
			return ok(map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": err.Error()}}})
		}
		return ok(map[string]any{"content": []map[string]any{{"type": "text", "text": text}}})
	case "resources/list":
		return ok(map[string]any{"resources": []any{}})
	case "prompts/list":
		return ok(map[string]any{"prompts": []any{}})
	default:
		return fail(-32601, "method not found: "+req.Method)
	}
}
