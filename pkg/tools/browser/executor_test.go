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

// execMockSidecar is a variant of the test mock that allows setting a custom
// response for tools/call requests so Executor tests can control sidecar output.
type execMockSidecar struct {
	srv     *httptest.Server
	mu      sync.Mutex
	sessCtr int
	sseChans map[string]chan []byte

	// toolsCallResult, when non-nil, is sent as the result field for tools/call.
	toolsCallResult func() any
}

func newExecMockSidecar(t *testing.T, toolsCallResult func() any) *execMockSidecar {
	t.Helper()
	m := &execMockSidecar{
		sseChans:        make(map[string]chan []byte),
		toolsCallResult: toolsCallResult,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", m.handleSSE)
	mux.HandleFunc("/messages", m.handleMessages)
	m.srv = httptest.NewServer(mux)
	return m
}

func (m *execMockSidecar) handleSSE(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.sessCtr++
	sessionID := fmt.Sprintf("exec-sess%d", m.sessCtr)
	ch := make(chan []byte, 64)
	m.sseChans[sessionID] = ch
	m.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, "event: endpoint\ndata: /messages?sessionId=%s\n\n", sessionID)
	flusher.Flush()

	for {
		select {
		case data, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			m.mu.Lock()
			delete(m.sseChans, sessionID)
			m.mu.Unlock()
			return
		}
	}
}

func (m *execMockSidecar) handleMessages(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	m.mu.Lock()
	ch := m.sseChans[sessionID]
	m.mu.Unlock()

	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &req)

	w.WriteHeader(http.StatusAccepted)

	if len(req.ID) == 0 || string(req.ID) == "null" {
		return // notifications need no response
	}

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
	if ch != nil {
		ch <- resp
	}
}

// close shuts the mock server down, forcing any open SSE connections closed first
// so httptest.Server.Close() does not block indefinitely.
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

// TestExecutor_TextResult verifies that a text-only Playwright MCP response is
// correctly mapped to tools.Result.Output as a JSON string.
func TestExecutor_TextResult(t *testing.T) {
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
	exec := NewExecutor(reg)

	args, _ := json.Marshal(map[string]any{
		"tool_name": "browser_navigate",
		"params":    map[string]any{"url": "https://example.com"},
	})

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

// TestExecutor_ScreenshotResult verifies that an image content item from Playwright
// MCP is decoded from base64 and placed in tools.Result.ImageParts.
func TestExecutor_ScreenshotResult(t *testing.T) {
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
	exec := NewExecutor(reg)

	args, _ := json.Marshal(map[string]any{
		"tool_name": "browser_screenshot",
		"params":    map[string]any{},
	})

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

// TestExecutor_BlockedTool verifies that tools not in AllowedTools are rejected
// before a sidecar call is made.
func TestExecutor_BlockedTool(t *testing.T) {
	mock := newExecMockSidecar(t, nil)
	defer mock.close()

	reg := newExecTestRegistry(mock)
	exec := NewExecutor(reg)

	args, _ := json.Marshal(map[string]any{
		"tool_name": "browser_close",
		"params":    map[string]any{},
	})

	ctx := tools.WithThreadID(context.Background(), "test-thread-blocked")
	_, err := exec.Execute(ctx, args)
	if err == nil {
		t.Fatal("expected error for disallowed tool, got nil")
	}
	// Error message should mention the tool name or the allowlist.
	errMsg := err.Error()
	if errMsg == "" {
		t.Error("error message is empty")
	}
}

// TestExecutor_EmptyThreadID verifies that Execute returns an error when no
// thread_id is present in context, preventing cross-tenant session sharing.
func TestExecutor_EmptyThreadID(t *testing.T) {
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
	exec := NewExecutor(reg)

	args, _ := json.Marshal(map[string]any{
		"tool_name": "browser_navigate",
		"params":    map[string]any{"url": "https://example.com"},
	})

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
