package router

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// noopToolVerifier satisfies the VerifyToolKey interface required by
// NewToolsWSHandler. MCP unit tests never connect an actual tools worker,
// so this method is never called.
type noopToolVerifier struct{}

func (noopToolVerifier) VerifyToolKey(string) (bool, error) { return false, nil }

// newMCPTestServer creates a minimal httptest.Server containing only the MCP
// handler. No Redis or external dependencies are required:
//
//   - APIKeyHash is intentionally empty → requireAuth returns true immediately
//     (open-access mode), so no auth.Verifier is needed.
//   - jobQueue and tokenStream are nil → safe because MCP unit tests only
//     exercise initialize / ping / tools/list / unknown-method, none of which
//     touch the job queue or token stream.
func newMCPTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newMCPTestServerWithConfig(t, &Config{
		AllowedTools:      []string{"code_executor", "web_fetch"},
		JobTimeout:        5 * time.Second,
		HeartbeatInterval: 15 * time.Second,
		// MCPExposeLLM defaults to false — generate_text must NOT appear.
	})
}

// newMCPTestServerWithConfig creates a minimal MCP httptest.Server using the
// supplied Config. jobQueue and tokenStream are always nil — tests that exercise
// generate_text only check the tools/list response, not actual LLM dispatch.
func newMCPTestServerWithConfig(t *testing.T, cfg *Config) *httptest.Server {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// nil jobQueue / tokenStream: safe for tests that only call tools/list.
	toolsHandler := NewToolsWSHandler(noopToolVerifier{}, nil, nil, cfg, logger)
	// nil verifier: safe because APIKeyHash == "" (see requireAuth).
	mcpHandler := NewMCPHandler(nil, toolsHandler, nil, nil, cfg, logger)

	mux := http.NewServeMux()
	mux.Handle("/mcp/", mcpHandler)

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// openMCPSession dials GET /mcp/sse on ts, reads the mandatory "endpoint" SSE
// event, and returns:
//   - sessionPath: the URL path for POST requests (e.g. "/mcp/message?sessionId=xxx")
//   - events: a buffered reader positioned immediately after the endpoint event
//   - cleanup: closes the SSE response body (and thus the session)
func openMCPSession(t *testing.T, ts *httptest.Server) (sessionPath string, events *bufio.Reader, cleanup func()) {
	t.Helper()

	// Use a client without a timeout: the SSE response body stays open for the
	// lifetime of the test.
	resp, err := http.Get(ts.URL + "/mcp/sse") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /mcp/sse: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET /mcp/sse: expected 200, got %d", resp.StatusCode)
	}

	events = bufio.NewReader(resp.Body)
	sessionPath = readMCPEndpointEvent(t, events)
	cleanup = func() { resp.Body.Close() }
	return sessionPath, events, cleanup
}

// readMCPEndpointEvent reads SSE lines from r until it receives the "endpoint"
// event and returns the session path string it carries.
//
// The server sends:  event: endpoint\ndata: "/mcp/message?sessionId=…"\n\n
// The data value is a JSON-quoted string, so we unmarshal it before returning.
func readMCPEndpointEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()

	var eventName, dataLine string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SSE (waiting for endpoint event): %v", err)
		}
		line = strings.TrimRight(line, "\r\n")

		switch {
		case strings.HasPrefix(line, "event: "):
			eventName = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			dataLine = strings.TrimPrefix(line, "data: ")
		case line == "" && eventName == "endpoint":
			// The data field is a Go-quoted string (fmt.Fprintf with %q),
			// which uses the same escape sequences as JSON.
			var url string
			if err := json.Unmarshal([]byte(dataLine), &url); err != nil {
				t.Fatalf("parsing MCP endpoint URL %q: %v", dataLine, err)
			}
			return url
		case line == "":
			// Blank line without a matching event name — reset and keep reading
			// (handles SSE comments like ": keepalive").
			eventName, dataLine = "", ""
		}
	}
}

// postMCPMessage POSTs body (a JSON-RPC string) to the session URL and asserts
// that the server responds with HTTP 202 Accepted. The actual JSON-RPC response
// arrives asynchronously on the SSE stream.
func postMCPMessage(t *testing.T, ts *httptest.Server, sessionPath, body string) {
	t.Helper()

	resp, err := http.Post(ts.URL+sessionPath, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", sessionPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST %s: expected 202, got %d", sessionPath, resp.StatusCode)
	}
}

// readSSEEvent reads SSE lines from r until a complete event (name + data +
// blank-line terminator) arrives. It skips comments (": …") and incomplete
// events (blank line before a data line). Returns the event name and raw data
// string.
func readSSEEvent(t *testing.T, r *bufio.Reader) (eventName, data string) {
	t.Helper()

	var name, dataLine string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SSE event: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")

		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			dataLine = strings.TrimPrefix(line, "data: ")
		case line == "" && name != "" && dataLine != "":
			return name, dataLine
		case line == "":
			// Blank terminator without accumulated content — keep reading.
			name, dataLine = "", ""
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestMCPInitialize(t *testing.T) {
	t.Parallel()

	ts := newMCPTestServer(t)
	sessionPath, events, cleanup := openMCPSession(t, ts)
	defer cleanup()

	postMCPMessage(t, ts, sessionPath, `{
		"jsonrpc": "2.0",
		"id": 1,
		"method": "initialize",
		"params": {"protocolVersion": "2024-11-05", "capabilities": {}}
	}`)

	evtName, evtData := readSSEEvent(t, events)
	if evtName != "message" {
		t.Fatalf("expected SSE event name 'message', got %q", evtName)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(evtData), &resp); err != nil {
		t.Fatalf("parsing initialize response JSON: %v", err)
	}

	if resp["jsonrpc"] != "2.0" {
		t.Errorf("expected jsonrpc=2.0, got %v", resp["jsonrpc"])
	}
	// JSON numbers decode to float64.
	if id, _ := resp["id"].(float64); id != 1 {
		t.Errorf("expected id=1, got %v", resp["id"])
	}
	if resp["error"] != nil {
		t.Fatalf("unexpected error in initialize response: %v", resp["error"])
	}

	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatal("expected result object in initialize response")
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("expected protocolVersion=2024-11-05, got %v", result["protocolVersion"])
	}
	serverInfo, _ := result["serverInfo"].(map[string]any)
	if serverInfo == nil {
		t.Fatal("expected serverInfo in initialize result")
	}
	if serverInfo["name"] != "auxot-router" {
		t.Errorf("expected serverInfo.name=auxot-router, got %v", serverInfo["name"])
	}
}

func TestMCPToolsList(t *testing.T) {
	t.Parallel()

	// No tools worker connected → AllowedToolDefs returns nil → tools list is [].
	ts := newMCPTestServer(t)
	sessionPath, events, cleanup := openMCPSession(t, ts)
	defer cleanup()

	postMCPMessage(t, ts, sessionPath, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)

	_, evtData := readSSEEvent(t, events)

	var resp map[string]any
	if err := json.Unmarshal([]byte(evtData), &resp); err != nil {
		t.Fatalf("parsing tools/list response JSON: %v", err)
	}
	if resp["error"] != nil {
		t.Fatalf("unexpected error in tools/list: %v", resp["error"])
	}

	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatal("expected result object in tools/list response")
	}

	// tools key must be present and be an empty array (not null).
	toolsRaw, ok := result["tools"]
	if !ok {
		t.Fatal("expected 'tools' key in tools/list result")
	}
	tools, _ := toolsRaw.([]any)
	if len(tools) != 0 {
		t.Errorf("expected 0 tools (no worker connected), got %d: %v", len(tools), tools)
	}
}

func TestMCPPing(t *testing.T) {
	t.Parallel()

	ts := newMCPTestServer(t)
	sessionPath, events, cleanup := openMCPSession(t, ts)
	defer cleanup()

	postMCPMessage(t, ts, sessionPath, `{"jsonrpc":"2.0","id":99,"method":"ping"}`)

	_, evtData := readSSEEvent(t, events)

	var resp map[string]any
	if err := json.Unmarshal([]byte(evtData), &resp); err != nil {
		t.Fatalf("parsing ping response JSON: %v", err)
	}

	if resp["jsonrpc"] != "2.0" {
		t.Errorf("expected jsonrpc=2.0, got %v", resp["jsonrpc"])
	}
	if id, _ := resp["id"].(float64); id != 99 {
		t.Errorf("expected id=99, got %v", resp["id"])
	}
	if resp["error"] != nil {
		t.Fatalf("unexpected error in ping response: %v", resp["error"])
	}
	// Per spec, ping result is an empty object {}.
	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Error("expected result={} for ping, got nil")
	}
}

func TestMCPUnknownMethod(t *testing.T) {
	t.Parallel()

	ts := newMCPTestServer(t)
	sessionPath, events, cleanup := openMCPSession(t, ts)
	defer cleanup()

	postMCPMessage(t, ts, sessionPath, `{"jsonrpc":"2.0","id":5,"method":"nonexistent/method"}`)

	_, evtData := readSSEEvent(t, events)

	var resp map[string]any
	if err := json.Unmarshal([]byte(evtData), &resp); err != nil {
		t.Fatalf("parsing unknown-method response JSON: %v", err)
	}

	if resp["error"] == nil {
		t.Fatal("expected JSON-RPC error for unknown method, got none")
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("expected error to be an object, got %T", resp["error"])
	}
	// JSON-RPC error code -32601 = Method not found.
	code, _ := errObj["code"].(float64)
	if code != -32601 {
		t.Errorf("expected error code -32601 (method not found), got %v", code)
	}
}

// TestMCPGenerateTextInToolsList verifies that "generate_text" appears in the
// tools/list response when MCPExposeLLM is true.
func TestMCPGenerateTextInToolsList(t *testing.T) {
	t.Parallel()

	ts := newMCPTestServerWithConfig(t, &Config{
		AllowedTools:      []string{"code_executor"},
		JobTimeout:        5 * time.Second,
		HeartbeatInterval: 15 * time.Second,
		MCPExposeLLM:      true, // feature flag ON
	})
	sessionPath, events, cleanup := openMCPSession(t, ts)
	defer cleanup()

	postMCPMessage(t, ts, sessionPath, `{"jsonrpc":"2.0","id":10,"method":"tools/list"}`)

	_, evtData := readSSEEvent(t, events)

	var resp map[string]any
	if err := json.Unmarshal([]byte(evtData), &resp); err != nil {
		t.Fatalf("parsing tools/list response JSON: %v", err)
	}
	if resp["error"] != nil {
		t.Fatalf("unexpected error in tools/list: %v", resp["error"])
	}

	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatal("expected result object in tools/list response")
	}

	toolsRaw, ok := result["tools"]
	if !ok {
		t.Fatal("expected 'tools' key in tools/list result")
	}
	tools, _ := toolsRaw.([]any)

	found := false
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		if tool["name"] == "generate_text" {
			found = true
			// Verify a description is present.
			if desc, _ := tool["description"].(string); desc == "" {
				t.Error("generate_text tool has no description")
			}
			// Verify the input schema is present.
			if tool["inputSchema"] == nil {
				t.Error("generate_text tool has no inputSchema")
			}
		}
	}
	if !found {
		t.Errorf("generate_text not found in tools/list with MCPExposeLLM=true; got: %v", tools)
	}
}

// TestMCPGenerateTextNotInListByDefault verifies that "generate_text" does NOT
// appear in tools/list when MCPExposeLLM is false (the default).
func TestMCPGenerateTextNotInListByDefault(t *testing.T) {
	t.Parallel()

	// Default server: MCPExposeLLM is false.
	ts := newMCPTestServer(t)
	sessionPath, events, cleanup := openMCPSession(t, ts)
	defer cleanup()

	postMCPMessage(t, ts, sessionPath, `{"jsonrpc":"2.0","id":11,"method":"tools/list"}`)

	_, evtData := readSSEEvent(t, events)

	var resp map[string]any
	if err := json.Unmarshal([]byte(evtData), &resp); err != nil {
		t.Fatalf("parsing tools/list response JSON: %v", err)
	}
	if resp["error"] != nil {
		t.Fatalf("unexpected error in tools/list: %v", resp["error"])
	}

	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatal("expected result object in tools/list response")
	}

	toolsRaw, _ := result["tools"]
	tools, _ := toolsRaw.([]any)

	for _, item := range tools {
		tool, _ := item.(map[string]any)
		if tool["name"] == "generate_text" {
			t.Error("generate_text appeared in tools/list but MCPExposeLLM is false")
		}
	}
}
