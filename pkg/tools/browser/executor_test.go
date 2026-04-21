package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/auxothq/auxot/pkg/tools"
)

// execMockSidecar simulates the Playwright MCP sidecar's Streamable HTTP
// transport (MCP 2025-03-26): every client request is a POST to /mcp, and the
// server replies with either a 202 Accepted (for notifications) or an inline
// SSE stream containing the JSON-RPC response.
type execMockSidecar struct {
	srv *httptest.Server
	mu  sync.Mutex

	sessCtr int

	// toolsCallResult, when non-nil, is returned as the result for tools/call.
	toolsCallResult func() any
}

func newExecMockSidecar(t *testing.T, toolsCallResult func() any) *execMockSidecar {
	t.Helper()
	m := &execMockSidecar{toolsCallResult: toolsCallResult}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", m.handleMCP)
	m.srv = httptest.NewServer(mux)
	return m
}

// handleMCP handles POST /mcp — the single endpoint of the Streamable HTTP
// MCP transport.  It parses the JSON-RPC method and returns:
//   - 202 Accepted for notifications (no id, no body needed)
//   - 200 with SSE body for request/response round-trips
func (m *execMockSidecar) handleMCP(w http.ResponseWriter, r *http.Request) {
	// DELETE /mcp: session termination — just acknowledge.
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &req)

	// Notifications have no id — acknowledge with 202, no body.
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"
	if isNotification {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Determine result based on method.
	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "mock-playwright-mcp", "version": "1.0"},
		}
	case "tools/call":
		if m.toolsCallResult != nil {
			result = m.toolsCallResult()
		} else {
			result = map[string]any{"content": []any{}, "isError": false}
		}
	default:
		result = map[string]any{"ok": true}
	}

	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
		"result":  result,
	})

	// Assign or carry forward the session ID via header.
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		m.mu.Lock()
		m.sessCtr++
		sessionID = fmt.Sprintf("exec-sess%d", m.sessCtr)
		m.mu.Unlock()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Mcp-Session-Id", sessionID)
	w.WriteHeader(http.StatusOK)

	fmt.Fprintf(w, "data: %s\n\n", resp)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// close shuts down the mock server.
func (m *execMockSidecar) close() {
	m.srv.CloseClientConnections()
	m.srv.Close()
}

// newExecTestRegistry creates a Registry backed by the exec mock (no sweeper goroutine).
func newExecTestRegistry(mock *execMockSidecar) *Registry {
	sc := &Sidecar{
		port:    0,
		baseURL: mock.srv.URL,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return &Registry{
		sidecar:  sc,
		sessions: make(map[string]*Session),
		now:      time.Now,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestPerToolExecutor_TextResult verifies that a text-only Playwright MCP response
// is correctly mapped to tools.Result.Output as a JSON string.
func TestPerToolExecutor_TextResult(t *testing.T) {
	mock := newExecMockSidecar(t, func() any {
		return map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "hello"},
			},
			"isError": false,
		}
	})
	defer mock.close()

	reg := newExecTestRegistry(mock)
	exec := NewPerToolExecutor("browser_navigate", reg)

	args, _ := json.Marshal(map[string]any{"url": "https://example.com"})

	ctx := tools.WithThreadID(context.Background(), "test-thread-text")
	result, err := exec.Execute(ctx, args)
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}

	// Output must be a JSON string equal to "hello".
	var got string
	if err := json.Unmarshal(result.Output, &got); err != nil {
		t.Fatalf("Output is not a JSON string: %v (raw=%s)", err, result.Output)
	}
	if got != "hello" {
		t.Errorf("Output = %q, want %q", got, "hello")
	}

	// No image parts expected for a text-only response.
	if len(result.ImageParts) != 0 {
		t.Errorf("ImageParts len = %d, want 0", len(result.ImageParts))
	}
}

// TestPerToolExecutor_ScreenshotResult verifies that an image content item from
// Playwright MCP is decoded from base64 and placed in tools.Result.ImageParts.
func TestPerToolExecutor_ScreenshotResult(t *testing.T) {
	// A 1×1 transparent PNG in base64 (minimal valid PNG, ~68 bytes).
	pngBytes := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, // PNG signature
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52, // IHDR chunk length + type
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, // 1×1
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, // bit depth + color type + CRC...
		0xDE, 0x00, 0x00, 0x00, 0x0C, 0x49, 0x44, 0x41,
		0x54, 0x08, 0xD7, 0x63, 0xF8, 0xFF, 0xFF, 0x3F,
		0x00, 0x05, 0xFE, 0x02, 0xFE, 0xDC, 0xCC, 0x59,
		0xE7, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E,
		0x44, 0xAE, 0x42, 0x60, 0x82,
	}
	encoded := base64.StdEncoding.EncodeToString(pngBytes)

	mock := newExecMockSidecar(t, func() any {
		return map[string]any{
			"content": []any{
				map[string]any{
					"type":     "image",
					"data":     encoded,
					"mimeType": "image/png",
				},
			},
			"isError": false,
		}
	})
	defer mock.close()

	reg := newExecTestRegistry(mock)
	exec := NewPerToolExecutor("browser_take_screenshot", reg)

	args := json.RawMessage("{}")

	ctx := tools.WithThreadID(context.Background(), "test-thread-screenshot")
	result, err := exec.Execute(ctx, args)
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}

	if len(result.ImageParts) != 1 {
		t.Fatalf("ImageParts len = %d, want 1", len(result.ImageParts))
	}
	ip := result.ImageParts[0]
	if ip.MIMEType != "image/png" {
		t.Errorf("ImageParts[0].MIMEType = %q, want %q", ip.MIMEType, "image/png")
	}
	if len(ip.Data) != len(pngBytes) {
		t.Errorf("ImageParts[0].Data len = %d, want %d", len(ip.Data), len(pngBytes))
	}
	for i := range pngBytes {
		if ip.Data[i] != pngBytes[i] {
			t.Errorf("ImageParts[0].Data[%d] = %d, want %d", i, ip.Data[i], pngBytes[i])
			break
		}
	}
}

// TestAllowedToolNames_ExcludesLifecycleTools verifies that context-lifecycle tools
// such as browser_close are not in AllowedToolNames, and that the expected set of
// individual Playwright tools is present and sorted.
func TestAllowedToolNames_ExcludesLifecycleTools(t *testing.T) {
	names := AllowedToolNames()
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}

	// Lifecycle tools must never be exposed.
	for _, blocked := range []string{"browser_close", "browser_new_context", "browser_new_page"} {
		if nameSet[blocked] {
			t.Errorf("AllowedToolNames() must not include lifecycle tool %q", blocked)
		}
	}

	// Core tools must be present.
	for _, required := range []string{
		"browser_navigate", "browser_click", "browser_type",
		"browser_take_screenshot", "browser_snapshot", "browser_evaluate",
	} {
		if !nameSet[required] {
			t.Errorf("AllowedToolNames() is missing required tool %q", required)
		}
	}

	// Names must be sorted.
	for i := 1; i < len(names); i++ {
		if names[i] < names[i-1] {
			t.Errorf("AllowedToolNames() not sorted at index %d: %q > %q", i, names[i-1], names[i])
		}
	}
}

// TestPerToolExecutor_EmptyThreadID verifies that Execute returns an error when no
// thread_id is present in context, preventing cross-tenant session sharing.
func TestPerToolExecutor_EmptyThreadID(t *testing.T) {
	mock := newExecMockSidecar(t, func() any {
		return map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "should not be reached"},
			},
			"isError": false,
		}
	})
	defer mock.close()

	reg := newExecTestRegistry(mock)
	exec := NewPerToolExecutor("browser_navigate", reg)

	args, _ := json.Marshal(map[string]any{"url": "https://example.com"})

	// Intentionally do NOT set a thread_id in context — must return an error.
	ctx := context.Background()
	_, err := exec.Execute(ctx, args)
	if err == nil {
		t.Fatal("Execute with empty thread_id should return an error, got nil")
	}
	if !strings.Contains(err.Error(), "thread_id") {
		t.Errorf("error should mention thread_id, got: %v", err)
	}
}
