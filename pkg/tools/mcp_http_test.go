package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mcpTestServer creates an httptest.Server that handles Streamable HTTP MCP
// requests for the happy path: initialize, tools/list, and tools/call.
//
// It responds with plain JSON (not SSE) for simplicity, which is valid per spec.
// The handler records which auth header value it received so tests can assert it.
func mcpTestServer(t *testing.T, toolsResp json.RawMessage, callResult json.RawMessage) (*httptest.Server, *[]string) {
	t.Helper()
	var receivedAuth []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		authVal := r.Header.Get("Authorization")
		if authVal != "" {
			receivedAuth = append(receivedAuth, authVal)
		}

		var req struct {
			JSONRPC string      `json:"jsonrpc"`
			ID      json.Number `json:"id"`
			Method  string      `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		switch req.Method {
		case "initialize":
			// Return a minimal initialize result.
			w.Header().Set("Mcp-Session-Id", "test-session-123")
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": mcpProtocolVersion,
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "test-mcp", "version": "0.1"},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)

		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)

		case "tools/list":
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  json.RawMessage(toolsResp),
			}
			_ = json.NewEncoder(w).Encode(resp)

		case "tools/call":
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  json.RawMessage(callResult),
			}
			_ = json.NewEncoder(w).Encode(resp)

		default:
			http.Error(w, fmt.Sprintf("unknown method: %s", req.Method), http.StatusBadRequest)
		}
	}))

	t.Cleanup(srv.Close)
	return srv, &receivedAuth
}

// sseTestServer creates an httptest.Server that responds with SSE-encoded
// JSON-RPC for tools/list and tools/call. Used to verify SSE parsing path.
func sseTestServer(t *testing.T, toolsResp json.RawMessage, callResult json.RawMessage) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			JSONRPC string      `json:"jsonrpc"`
			ID      json.Number `json:"id"`
			Method  string      `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "sse-session-456")
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": mcpProtocolVersion,
					"capabilities":    map[string]any{},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)

		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)

		case "tools/list", "tools/call":
			var payload json.RawMessage
			if req.Method == "tools/list" {
				payload, _ = json.Marshal(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  json.RawMessage(toolsResp),
				})
			} else {
				payload, _ = json.Marshal(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  json.RawMessage(callResult),
				})
			}
			w.Header().Set("Content-Type", "text/event-stream")
			// Write two events: a notification (should be skipped) then the real response.
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/something\"}\n\n")
			fmt.Fprintf(w, "data: %s\n\n", payload)
			// Flush so the reader receives both events before EOF.
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}))

	t.Cleanup(srv.Close)
	return srv
}

// --- Tests ---

func TestMcpHttpIntrospect_PlainJSON(t *testing.T) {
	toolsJSON := json.RawMessage(`{"tools":[
		{"name":"get_file","description":"Get a file","inputSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}},
		{"name":"list_files","description":"List files","inputSchema":{"type":"object","properties":{}}}
	]}`)

	srv, _ := mcpTestServer(t, toolsJSON, nil)

	defs, err := McpHttpIntrospect(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("McpHttpIntrospect: %v", err)
	}

	if len(defs) != 2 {
		t.Fatalf("expected 2 tool defs, got %d", len(defs))
	}
	if defs[0].Name != "get_file" {
		t.Errorf("defs[0].Name = %q, want %q", defs[0].Name, "get_file")
	}
	if len(defs[0].ParamNames) != 1 || defs[0].ParamNames[0] != "path" {
		t.Errorf("defs[0].ParamNames = %v, want [path]", defs[0].ParamNames)
	}
	if defs[1].Name != "list_files" {
		t.Errorf("defs[1].Name = %q, want %q", defs[1].Name, "list_files")
	}
}

func TestMcpHttpIntrospect_InjectsAuthHeader(t *testing.T) {
	toolsJSON := json.RawMessage(`{"tools":[{"name":"whoami","description":"Who am I"}]}`)
	srv, receivedAuth := mcpTestServer(t, toolsJSON, nil)

	headers := map[string]string{"Authorization": "Bearer test-token-123"}
	_, err := McpHttpIntrospect(context.Background(), srv.URL, headers)
	if err != nil {
		t.Fatalf("McpHttpIntrospect: %v", err)
	}

	// The initialize call should have carried the auth header.
	if len(*receivedAuth) == 0 {
		t.Fatal("expected auth header to be received by server, got none")
	}
	for _, v := range *receivedAuth {
		if v != "Bearer test-token-123" {
			t.Errorf("received auth = %q, want %q", v, "Bearer test-token-123")
		}
	}
}

func TestMcpHttpExecute_PlainJSON(t *testing.T) {
	callResult := json.RawMessage(`{"content":[{"type":"text","text":"hello from tool"}],"isError":false}`)
	srv, _ := mcpTestServer(t, json.RawMessage(`{"tools":[]}`), callResult)

	result, err := McpHttpExecute(context.Background(), srv.URL, nil, "my_tool", json.RawMessage(`{"key":"value"}`))
	if err != nil {
		t.Fatalf("McpHttpExecute: %v", err)
	}
	if result.Output == nil {
		t.Fatal("result.Output is nil")
	}
	if !strings.Contains(string(result.Output), "hello from tool") {
		t.Errorf("result.Output = %s, want it to contain 'hello from tool'", result.Output)
	}
}

func TestMcpHttpExecute_IsErrorFalse(t *testing.T) {
	callResult := json.RawMessage(`{"content":[{"type":"text","text":"success"}],"isError":false}`)
	srv, _ := mcpTestServer(t, json.RawMessage(`{"tools":[]}`), callResult)

	_, err := McpHttpExecute(context.Background(), srv.URL, nil, "tool", nil)
	if err != nil {
		t.Fatalf("expected no error for isError=false, got: %v", err)
	}
}

func TestMcpHttpExecute_IsErrorTrue(t *testing.T) {
	callResult := json.RawMessage(`{"content":[{"type":"text","text":"access denied"}],"isError":true}`)
	srv, _ := mcpTestServer(t, json.RawMessage(`{"tools":[]}`), callResult)

	_, err := McpHttpExecute(context.Background(), srv.URL, nil, "protected_tool", nil)
	if err == nil {
		t.Fatal("expected error for isError=true")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("error = %v, want it to contain 'access denied'", err)
	}
}

func TestMcpHttpExecute_NullArgs(t *testing.T) {
	callResult := json.RawMessage(`{"content":[{"type":"text","text":"ok"}],"isError":false}`)
	srv, _ := mcpTestServer(t, json.RawMessage(`{"tools":[]}`), callResult)

	// nil args should be normalized to {} without error.
	_, err := McpHttpExecute(context.Background(), srv.URL, nil, "no_args_tool", nil)
	if err != nil {
		t.Fatalf("McpHttpExecute with nil args: %v", err)
	}
}

func TestMcpHttpIntrospect_SSE(t *testing.T) {
	toolsJSON := json.RawMessage(`{"tools":[{"name":"sse_tool","description":"A tool via SSE"}]}`)
	srv := sseTestServer(t, toolsJSON, nil)

	defs, err := McpHttpIntrospect(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("McpHttpIntrospect (SSE): %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected 1 def, got %d", len(defs))
	}
	if defs[0].Name != "sse_tool" {
		t.Errorf("defs[0].Name = %q, want sse_tool", defs[0].Name)
	}
}

func TestMcpHttpExecute_SSE(t *testing.T) {
	callResult := json.RawMessage(`{"content":[{"type":"text","text":"sse result"}],"isError":false}`)
	srv := sseTestServer(t, json.RawMessage(`{"tools":[]}`), callResult)

	result, err := McpHttpExecute(context.Background(), srv.URL, nil, "sse_tool", nil)
	if err != nil {
		t.Fatalf("McpHttpExecute (SSE): %v", err)
	}
	if !strings.Contains(string(result.Output), "sse result") {
		t.Errorf("output = %s, want 'sse result'", result.Output)
	}
}

func TestMcpHttpIntrospect_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := McpHttpIntrospect(context.Background(), srv.URL, nil)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %v, want it to mention 500", err)
	}
}

func TestMcpHttpIntrospect_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a slow server — the cancelled context should interrupt the request.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := McpHttpIntrospect(ctx, srv.URL, nil)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

// --- Slug tests ---

func TestMcpHttpSlug_WithID(t *testing.T) {
	got := McpHttpSlug("my-server", "https://example.com/mcp")
	if got != "my_server" {
		t.Errorf("McpHttpSlug with id = %q, want %q", got, "my_server")
	}
}

func TestMcpHttpSlug_UUIDUsesHostnameNotUUID(t *testing.T) {
	id := "ddfee03b-58a1-4558-98d6-e028a99fb606"
	url := "https://api.githubcopilot.com/mcp/"
	got := McpHttpSlug(id, url)
	if got != "githubcopilot" {
		t.Errorf("McpHttpSlug(uuid, copilot URL) = %q, want githubcopilot (hostname, not uuid)", got)
	}
}

func TestMcpHttpSlug_FromURL(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://api.githubcopilot.com/mcp/", "githubcopilot"},
		{"https://mcp.example.com/", "example"},
		{"https://api.example.org:8080/mcp", "example"},
	}
	for _, tc := range cases {
		got := McpHttpSlug("", tc.url)
		if got != tc.want {
			t.Errorf("McpHttpSlug(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// --- readSSEResponse unit test ---

func TestMcpReadSSEResponse_SkipsNotifications(t *testing.T) {
	// Stream contains a notification without id (should be skipped) then our response.
	stream := "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/x\"}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"content\":[]}}\n\n"

	raw, err := mcpReadSSEResponse(strings.NewReader(stream), 2)
	if err != nil {
		t.Fatalf("mcpReadSSEResponse: %v", err)
	}
	var msg struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.ID != 2 {
		t.Errorf("id = %d, want 2", msg.ID)
	}
}

func TestMcpReadSSEResponse_WantIDMinusOne(t *testing.T) {
	stream := "data: {\"jsonrpc\":\"2.0\",\"method\":\"notification\"}\n\n"
	raw, err := mcpReadSSEResponse(strings.NewReader(stream), -1)
	if err != nil {
		t.Fatalf("mcpReadSSEResponse wantID=-1: %v", err)
	}
	if !strings.Contains(string(raw), "notification") {
		t.Errorf("raw = %s, want it to contain 'notification'", raw)
	}
}
