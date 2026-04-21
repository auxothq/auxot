package browser

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// mcpHTTPClient for Streamable HTTP MCP requests.
// We use a shared Transport for keep-alive but rely on context deadlines for
// per-request timeouts, so no client-level Timeout is set.
var mcpHTTPClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		// ResponseHeaderTimeout is intentionally omitted so SSE response
		// streams are not cut off by a header-only deadline.
	},
}

// Session holds the MCP session state for one thread, using the Streamable
// HTTP transport (MCP 2025-03-26 spec).  Every client-to-server message is a
// POST to /mcp.  The server replies with either a plain JSON body or an SSE
// stream; postMCP handles both transparently.
type Session struct {
	threadID  string
	sidecar   *Sidecar
	sessionID string // Mcp-Session-Id assigned during initialize

	mu       sync.Mutex
	lastUsed time.Time
	nextID   int

	// inFlight counts active Call() invocations so the sweeper never evicts a
	// session with an in-progress tool call.
	// Must be 8-byte aligned — placed before pointer fields for 32-bit safety.
	inFlight int64

	done      chan struct{}
	closeOnce sync.Once
}

// close terminates the server-side MCP session (DELETE /mcp) and signals any
// waiters that the Go-side session is gone.
//
// Sending DELETE is important in non-isolated mode: the sidecar keeps the
// BrowserContext open until it receives the DELETE.  Without it a second
// thread's initialize succeeds but its first tool call races the existing
// BrowserContext and can get "Browser is already in use".
func (sess *Session) close() {
	sess.closeOnce.Do(func() {
		if sess.sessionID != "" {
			// Best-effort: fire the DELETE and ignore errors.  The sidecar
			// will eventually GC the session on its own idle timeout anyway.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
				sess.sidecar.baseURL+"/mcp", nil)
			if err == nil {
				req.Header.Set("Mcp-Session-Id", sess.sessionID)
				resp, err := mcpHTTPClient.Do(req)
				if err == nil {
					_ = resp.Body.Close()
				}
			}
		}
		close(sess.done)
	})
}

// connect performs the MCP initialize handshake and stores the session ID.
func (sess *Session) connect(logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, newSessionID, err := sess.postMCP(ctx, "", 0, map[string]any{
		"jsonrpc": "2.0",
		"id":      0,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "auxot-tools", "version": "1.0"},
		},
	})
	if err != nil {
		return fmt.Errorf("browser MCP initialize: %w", err)
	}
	sess.sessionID = newSessionID

	if raw != nil {
		var initResp struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &initResp); err == nil && initResp.Error != nil {
			return fmt.Errorf("browser MCP initialize error %d: %s", initResp.Error.Code, initResp.Error.Message)
		}
	}

	// notifications/initialized is fire-and-forget (no id, no expected response).
	_, _, err = sess.postMCP(ctx, sess.sessionID, -1, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	if err != nil {
		logger.Debug("browser MCP notifications/initialized warning", "error", err)
	}

	return nil
}

// postMCP marshals req and POSTs it to the sidecar's /mcp endpoint.
//
// wantID is the JSON-RPC id we expect in the SSE response; use -1 for
// notifications (no id means no SSE response is expected).  The function
// parses SSE events from the response stream and returns the first event
// whose "id" field matches wantID, so it terminates as soon as the answer
// arrives without waiting for the SSE stream to close.
//
// Returns (nil, sessionID, nil) for 202 Accepted (notification ack).
func (sess *Session) postMCP(ctx context.Context, sessionID string, wantID int, req map[string]any) (json.RawMessage, string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, "", fmt.Errorf("marshal MCP request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		sess.sidecar.baseURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("creating MCP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", sessionID)
	}

	resp, err := mcpHTTPClient.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("POST /mcp: %w", err)
	}
	defer resp.Body.Close()

	// Carry over the session ID from the response header (if present).
	newSessionID := resp.Header.Get("Mcp-Session-Id")
	if newSessionID == "" {
		newSessionID = sessionID
	}

	// 202 Accepted: notification acknowledged, no body to parse.
	if resp.StatusCode == http.StatusAccepted {
		return nil, newSessionID, nil
	}

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, newSessionID, fmt.Errorf("MCP server returned %d: %s", resp.StatusCode, string(b))
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		// Plain JSON response — unlikely in practice but handle defensively.
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, newSessionID, fmt.Errorf("reading MCP JSON response: %w", err)
		}
		return json.RawMessage(b), newSessionID, nil
	}

	// SSE response stream.  Read events until we find one whose id matches
	// wantID, then close the connection and return.  This avoids blocking until
	// the server closes the stream (which it may never do for long-lived sessions).
	raw, err := readSSEResponse(resp.Body, wantID)
	return raw, newSessionID, err
}

// readSSEResponse scans the SSE stream from r looking for a JSON-RPC message
// whose "id" field equals wantID.  When wantID == -1 the first event with a
// non-empty data field is returned (used for responses without a request id).
// The function returns as soon as a match is found, so the caller should close
// resp.Body after the call to terminate the SSE stream.
func readSSEResponse(r io.Reader, wantID int) (json.RawMessage, error) {
	br := bufio.NewReaderSize(r, 1<<20) // 1 MiB buffer for large screenshot responses

	var currentData strings.Builder
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")

		switch {
		case strings.HasPrefix(line, "data: "):
			currentData.WriteString(strings.TrimPrefix(line, "data: "))
		case line == "":
			if currentData.Len() > 0 {
				data := currentData.String()
				currentData.Reset()

				if wantID == -1 {
					// Caller doesn't care about ID matching (notification ack).
					return json.RawMessage(data), nil
				}

				// Match the response id.
				var msg struct {
					ID *json.Number `json:"id"`
				}
				if jsonErr := json.Unmarshal([]byte(data), &msg); jsonErr == nil && msg.ID != nil {
					gotID, parseErr := msg.ID.Int64()
					if parseErr == nil && int(gotID) == wantID {
						return json.RawMessage(data), nil
					}
				}
				// Not our response; keep reading (server may send notifications).
			}
		}

		if err != nil {
			if err == io.EOF {
				return nil, fmt.Errorf("SSE stream closed before matching response (wantID=%d)", wantID)
			}
			return nil, fmt.Errorf("reading SSE stream: %w", err)
		}
	}
}

// Registry manages Sessions keyed by thread_id with a 30-minute idle TTL.
type Registry struct {
	sidecar  *Sidecar
	mu       sync.RWMutex
	sessions map[string]*Session
	now      func() time.Time // injectable for tests
	logger   *slog.Logger
}

// NewRegistry creates a Registry and starts its background sweeper goroutine.
// The sweeper runs until ctx is cancelled.
func NewRegistry(ctx context.Context, sidecar *Sidecar, logger *slog.Logger) *Registry {
	r := &Registry{
		sidecar:  sidecar,
		sessions: make(map[string]*Session),
		now:      time.Now,
		logger:   logger,
	}
	go r.sweeper(ctx)
	return r
}

// GetOrCreate returns the existing Session for threadID, or creates and
// connects a new one.  It touches lastUsed on every call.
func (r *Registry) GetOrCreate(threadID string) (*Session, error) {
	r.mu.RLock()
	sess, ok := r.sessions[threadID]
	r.mu.RUnlock()
	if ok {
		sess.mu.Lock()
		sess.lastUsed = r.now()
		sess.mu.Unlock()
		return sess, nil
	}

	r.mu.Lock()
	if sess, ok = r.sessions[threadID]; ok {
		r.mu.Unlock()
		sess.mu.Lock()
		sess.lastUsed = r.now()
		sess.mu.Unlock()
		return sess, nil
	}
	sess = &Session{
		threadID: threadID,
		sidecar:  r.sidecar,
		lastUsed: r.now(),
		done:     make(chan struct{}),
	}
	r.sessions[threadID] = sess
	r.mu.Unlock()

	if err := sess.connect(r.logger); err != nil {
		r.mu.Lock()
		delete(r.sessions, threadID)
		r.mu.Unlock()
		go sess.close()
		return nil, fmt.Errorf("browser session for thread %q: %w", threadID, err)
	}
	return sess, nil
}

// Call sends a single MCP JSON-RPC request via the session and returns the
// result JSON.  The caller's ctx controls the per-call deadline so slow
// browser operations (heavy pages, screenshots) respect the job timeout.
func (r *Registry) Call(ctx context.Context, sess *Session, method string, params any) (json.RawMessage, error) {
	raw, err := r.doCall(ctx, sess, method, params)
	if err != nil && isSessionNotFound(err) {
		// The playwright-mcp server lost this session (sidecar restart, proxy
		// timeout dropping the idle connection, etc.).  Evict the dead session
		// and reconnect transparently so the caller never has to handle this.
		r.mu.Lock()
		delete(r.sessions, sess.threadID)
		r.mu.Unlock()
		go sess.close()

		r.logger.Info("browser session lost; reconnecting",
			"thread_id", sess.threadID, "cause", err)

		newSess, reconnErr := r.GetOrCreate(sess.threadID)
		if reconnErr != nil {
			return nil, fmt.Errorf("browser session reconnect: %w", reconnErr)
		}
		return r.doCall(ctx, newSess, method, params)
	}
	return raw, err
}

// isSessionNotFound reports whether err indicates that the playwright-mcp
// server returned 404 "Session not found" for a stale Mcp-Session-Id.
func isSessionNotFound(err error) bool {
	s := err.Error()
	return strings.Contains(s, "404") && strings.Contains(s, "Session not found")
}

// doCall performs a single MCP round-trip with no retry logic.
func (r *Registry) doCall(ctx context.Context, sess *Session, method string, params any) (json.RawMessage, error) {
	atomic.AddInt64(&sess.inFlight, 1)
	defer atomic.AddInt64(&sess.inFlight, -1)

	sess.mu.Lock()
	id := sess.nextID
	sess.nextID++
	sessionID := sess.sessionID
	sess.mu.Unlock()

	raw, _, err := sess.postMCP(ctx, sessionID, id, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, fmt.Errorf("browser Call: %w", err)
	}

	sess.mu.Lock()
	sess.lastUsed = r.now()
	sess.mu.Unlock()

	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parsing MCP response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

// closeSession removes threadID from the sessions map and tears down the
// session.  Must be called with r.mu write-locked.
func (r *Registry) closeSession(threadID string) {
	sess, ok := r.sessions[threadID]
	if !ok {
		return
	}
	delete(r.sessions, threadID)
	go func() {
		r.logger.Info("browser session evicted (idle TTL)", "thread_id", threadID)
		sess.close()
	}()
}

// sweepOnce evicts sessions idle for more than 30 minutes, skipping those
// with in-flight Call() goroutines.
func (r *Registry) sweepOnce() {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for threadID, sess := range r.sessions {
		if atomic.LoadInt64(&sess.inFlight) > 0 {
			continue
		}
		sess.mu.Lock()
		idle := now.Sub(sess.lastUsed)
		sess.mu.Unlock()
		if idle > 30*time.Minute {
			r.closeSession(threadID)
		}
	}
}

// sweeper runs every 5 minutes, evicting sessions idle for more than 30 minutes.
func (r *Registry) sweeper(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.sweepOnce()
		case <-ctx.Done():
			return
		}
	}
}
