package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// mockSidecar simulates the Playwright MCP sidecar's SSE + messages endpoints.
// GET /sse        → streams endpoint event, then relays responses from handlers.
// POST /messages  → parses JSON-RPC, builds response, sends over SSE channel.
type mockSidecar struct {
	srv      *httptest.Server
	mu       sync.Mutex
	sseChans map[string]chan []byte // sessionID → buffered channel for SSE lines
	sessCtr  int
}

func newMockSidecar(t *testing.T) *mockSidecar {
	t.Helper()
	m := &mockSidecar{sseChans: make(map[string]chan []byte)}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", m.handleSSE)
	mux.HandleFunc("/messages", m.handleMessages)
	m.srv = httptest.NewServer(mux)
	return m
}

func (m *mockSidecar) handleSSE(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.sessCtr++
	sessionID := fmt.Sprintf("sess%d", m.sessCtr)
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

	// Send the MCP endpoint event.
	fmt.Fprintf(w, "event: endpoint\ndata: /messages?sessionId=%s\n\n", sessionID)
	flusher.Flush()

	// Relay JSON-RPC responses until the connection is closed.
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

func (m *mockSidecar) handleMessages(w http.ResponseWriter, r *http.Request) {
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

	// Acknowledge immediately — response arrives on the SSE stream.
	w.WriteHeader(http.StatusAccepted)

	// Notifications (no id) require no response.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return
	}

	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "mock-playwright-mcp", "version": "1.0"},
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

func (m *mockSidecar) close() { m.srv.Close() }

// newTestRegistry builds a Registry backed by the mock without starting the
// background sweeper goroutine, so tests can call sweepOnce() directly.
func newTestRegistry(t *testing.T, mock *mockSidecar, nowFn func() time.Time) *Registry {
	t.Helper()
	sc := &Sidecar{
		port:    0, // not used in tests — baseURL points at the mock
		baseURL: mock.srv.URL,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return &Registry{
		sidecar:  sc,
		sessions: make(map[string]*Session),
		now:      nowFn,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestRegistry_TTL verifies that a session idle for > 30 minutes is evicted.
func TestRegistry_TTL(t *testing.T) {
	mock := newMockSidecar(t)
	defer mock.close()

	now := time.Now()
	reg := newTestRegistry(t, mock, func() time.Time { return now })

	// Create a session.
	sess, err := reg.GetOrCreate("thread-ttl")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	_ = sess

	// Advance clock past the 30-minute TTL.
	now = now.Add(31 * time.Minute)

	// Sweep directly (no background goroutine in test).
	reg.sweepOnce()

	// The session must have been removed from the map.
	reg.mu.RLock()
	_, exists := reg.sessions["thread-ttl"]
	reg.mu.RUnlock()

	if exists {
		t.Error("expected session to be evicted after 31 minutes of idle time")
	}

	// GetOrCreate would create a fresh session (not tested for equality here,
	// but verifying it doesn't error proves the map entry is gone).
	sess2, err := reg.GetOrCreate("thread-ttl")
	if err != nil {
		t.Fatalf("GetOrCreate after eviction: %v", err)
	}
	if sess2 == sess {
		t.Error("expected a new session object after eviction, got the same pointer")
	}
	// Cleanup.
	reg.mu.Lock()
	reg.closeSession("thread-ttl")
	reg.mu.Unlock()
}

// TestRegistry_Touch verifies that calling Call refreshes lastUsed, preventing
// premature eviction even when total elapsed time exceeds 30 minutes.
func TestRegistry_Touch(t *testing.T) {
	mock := newMockSidecar(t)
	defer mock.close()

	now := time.Now()
	reg := newTestRegistry(t, mock, func() time.Time { return now })

	// T=0 — create session.
	sess, err := reg.GetOrCreate("thread-touch")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	// T=+20min — advance clock then make a Call (which touches lastUsed to T+20min).
	now = now.Add(20 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := reg.Call(ctx, sess, "tools/list", nil); err != nil {
		t.Fatalf("Call failed: %v", err)
	}

	// T=+40min — advance clock again.  lastUsed is T+20min, idle = 20min < 30min.
	now = now.Add(20 * time.Minute)

	reg.sweepOnce()

	reg.mu.RLock()
	_, exists := reg.sessions["thread-touch"]
	reg.mu.RUnlock()

	if !exists {
		t.Error("expected session to survive: lastUsed was refreshed by the Call")
	}

	// Cleanup.
	reg.mu.Lock()
	reg.closeSession("thread-touch")
	reg.mu.Unlock()
}
