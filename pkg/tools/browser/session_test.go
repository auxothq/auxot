package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// mcpMock simulates the Playwright MCP sidecar using the Streamable HTTP
// transport (MCP 2025-03-26 spec).  Every client message is a POST to /mcp;
// responses are SSE streams or 202 Accepted for notifications.
//
// Heartbeat simulation fields (optional):
//   - Set pingIntervalNs (via Store) before GetOrCreate to enable SSE pings.
//   - pingDelayNs controls the initial delay before the first ping;
//     defaults to pingIntervalNs when zero.
//   - pingResponses counts ping JSON-RPC responses received from the client.
type mcpMock struct {
	srv            *httptest.Server
	sessCtr        atomic.Int64
	pingIntervalNs atomic.Int64 // nanoseconds; 0 = no pings (default)
	pingDelayNs    atomic.Int64 // nanoseconds; 0 = use pingIntervalNs
	pingResponses  atomic.Int64 // ping responses received via POST
}

func newMCPMock(t *testing.T) *mcpMock {
	t.Helper()
	m := &mcpMock{}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", m.handleMCP)
	// GET /sse is probed by the sidecar readiness check — return 200 so tests
	// that construct a Sidecar struct pointing at the mock pass the probe.
	mux.HandleFunc("/sse", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	m.srv = httptest.NewServer(mux)
	return m
}

func (m *mcpMock) handleMCP(w http.ResponseWriter, r *http.Request) {
	// GET /mcp: the SSE server-to-client channel (heartbeat pings).
	if r.Method == http.MethodGet {
		m.handleSSE(w, r)
		return
	}

	// DELETE /mcp: session termination — acknowledge and return.
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusOK)
		return
	}

	body, _ := io.ReadAll(r.Body)

	var req struct {
		ID     *json.RawMessage `json:"id"`
		Method string           `json:"method"`
		Result *json.RawMessage `json:"result"`
	}
	_ = json.Unmarshal(body, &req)

	// Notifications have no id — acknowledge with 202 and no body.
	if req.ID == nil || string(*req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// JSON-RPC responses (ping replies) have a result field but no method.
	// The listen loop sends these back when it receives a ping over SSE.
	if req.Result != nil && req.Method == "" {
		m.pingResponses.Add(1)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Assign or carry forward the session ID.
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		sessionID = fmt.Sprintf("mock-sess-%d", m.sessCtr.Add(1))
	}
	w.Header().Set("Mcp-Session-Id", sessionID)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "mock-playwright-mcp", "version": "1.0"},
		}
	case "tools/list":
		result = map[string]any{
			"tools": []map[string]any{
				{"name": "browser_navigate", "description": "Navigate", "inputSchema": map[string]any{}},
			},
		}
	default:
		result = map[string]any{"ok": true}
	}

	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
		"result":  result,
	})
	fmt.Fprintf(w, "data: %s\n\n", resp)
}

// handleSSE serves GET /mcp as an SSE stream for server-to-client messages.
// When pingIntervalNs is non-zero the mock emits JSON-RPC "ping" events to
// exercise the listen-loop heartbeat path.  Otherwise the connection is kept
// open but silent, which is safe for tests that do not care about pings.
func (m *mcpMock) handleSSE(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	interval := time.Duration(m.pingIntervalNs.Load())
	if interval == 0 {
		// No pings configured — keep the channel open until the client
		// disconnects (context cancelled by server or session close).
		<-r.Context().Done()
		return
	}

	delay := time.Duration(m.pingDelayNs.Load())
	if delay == 0 {
		delay = interval
	}

	// Initial delay before the first ping (mirrors playwright-mcp firing
	// startHeartbeat only after the first tool call is processed).
	select {
	case <-r.Context().Done():
		return
	case <-time.After(delay):
	}

	var pingID int64 = 1
	for {
		msg, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      pingID,
			"method":  "ping",
		})
		fmt.Fprintf(w, "data: %s\n\n", msg)
		fl.Flush()
		pingID++

		select {
		case <-r.Context().Done():
			return
		case <-time.After(interval):
		}
	}
}

func (m *mcpMock) close() { m.srv.Close() }

// newTestRegistry builds a Registry backed by the mock without starting the
// background sweeper goroutine, so tests can call sweepOnce() directly.
func newTestRegistry(t *testing.T, mock *mcpMock, nowFn func() time.Time) *Registry {
	t.Helper()
	sc := &Sidecar{
		port:    0,
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
	mock := newMCPMock(t)
	defer mock.close()

	now := time.Now()
	reg := newTestRegistry(t, mock, func() time.Time { return now })

	sess, err := reg.GetOrCreate("thread-ttl")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	_ = sess

	// Advance clock past the 30-minute TTL.
	now = now.Add(31 * time.Minute)
	reg.sweepOnce()

	reg.mu.RLock()
	_, exists := reg.sessions["thread-ttl"]
	reg.mu.RUnlock()

	if exists {
		t.Error("expected session to be evicted after 31 minutes of idle time")
	}

	// GetOrCreate must create a fresh session — no error means the map slot is clear.
	sess2, err := reg.GetOrCreate("thread-ttl")
	if err != nil {
		t.Fatalf("GetOrCreate after eviction: %v", err)
	}
	if sess2 == sess {
		t.Error("expected a new session object after eviction, got the same pointer")
	}

	reg.mu.Lock()
	reg.closeSession("thread-ttl")
	reg.mu.Unlock()
}

// TestRegistry_Touch verifies that a Call refreshes lastUsed so an active
// session is never evicted even when total elapsed time exceeds 30 minutes.
func TestRegistry_Touch(t *testing.T) {
	mock := newMCPMock(t)
	defer mock.close()

	now := time.Now()
	reg := newTestRegistry(t, mock, func() time.Time { return now })

	sess, err := reg.GetOrCreate("thread-touch")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	// T=+20min — Call touches lastUsed.
	now = now.Add(20 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := reg.Call(ctx, sess, "tools/list", nil); err != nil {
		t.Fatalf("Call failed: %v", err)
	}

	// T=+40min total — idle since last Call is only 20min, below the 30min TTL.
	now = now.Add(20 * time.Minute)
	reg.sweepOnce()

	reg.mu.RLock()
	_, exists := reg.sessions["thread-touch"]
	reg.mu.RUnlock()

	if !exists {
		t.Error("expected session to survive: lastUsed was refreshed by the Call")
	}

	reg.mu.Lock()
	reg.closeSession("thread-touch")
	reg.mu.Unlock()
}

// TestRegistry_HeartbeatKeepsSessionAlive verifies that the listen loop
// (startListenLoop / runListenOnce) correctly responds to SSE ping requests
// sent by the server-side heartbeat mechanism.
//
// The mock is configured to send a JSON-RPC "ping" via SSE after an initial
// delay, then every 800 ms.  The test asserts:
//  1. A second Call() after a multi-second delay succeeds (session still alive).
//  2. The mock received at least one ping response POST from the client.
func TestRegistry_HeartbeatKeepsSessionAlive(t *testing.T) {
	t.Parallel()

	mock := newMCPMock(t)
	defer mock.close()

	// Configure heartbeat simulation: first ping after 1 s, then every 800 ms.
	mock.pingIntervalNs.Store(int64(800 * time.Millisecond))
	mock.pingDelayNs.Store(int64(time.Second))

	reg := newTestRegistry(t, mock, time.Now)

	sess, err := reg.GetOrCreate("thread-heartbeat")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	// Give the listen loop time to open GET /mcp before any pings fire.
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// First tool call — exercises the normal request path.
	if _, err := reg.Call(ctx, sess, "tools/list", nil); err != nil {
		t.Fatalf("first Call (tools/list): %v", err)
	}

	// Wait long enough for several heartbeat ping/response cycles to complete.
	// With 1 s initial delay + 800 ms interval, 5 s gives ~5 pings.
	time.Sleep(5 * time.Second)

	// The session must still be alive — listen loop kept responding to pings.
	if _, err := reg.Call(ctx, sess, "tools/list", nil); err != nil {
		t.Fatalf("second Call (tools/list) after heartbeat delay: %v — session may have been killed by missed pings", err)
	}

	// Confirm the listen loop actually sent ping responses back to the mock.
	if got := mock.pingResponses.Load(); got < 1 {
		t.Errorf("expected at least 1 ping response, got %d — listen loop may not be responding to pings", got)
	}
	t.Logf("ping responses received by mock: %d", mock.pingResponses.Load())

	reg.mu.Lock()
	reg.closeSession("thread-heartbeat")
	reg.mu.Unlock()
}
