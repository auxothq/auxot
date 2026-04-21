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

// sseHTTPClient is used for long-lived SSE GET requests.  We avoid a full
// Timeout (which would cut the streaming body) but set ResponseHeaderTimeout
// so a hung sidecar doesn't block forever before headers arrive.
var sseHTTPClient = &http.Client{
	Transport: &http.Transport{
		ResponseHeaderTimeout: 10 * time.Second,
	},
}

// rpcHTTPClient is used for JSON-RPC POST requests that expect a quick 202 Accepted.
var rpcHTTPClient = &http.Client{Timeout: 10 * time.Second}

// Session holds a persistent SSE connection to the Playwright MCP sidecar for
// one thread.  All exported state is accessed under the appropriate mutex.
type Session struct {
	threadID  string
	sidecar   *Sidecar
	sessionID string // assigned by the sidecar on SSE connect
	msgPath   string // e.g. /messages?sessionId=<id>

	mu       sync.Mutex
	lastUsed time.Time
	nextID   int

	// inFlight counts active Call() goroutines on this session.  Accessed with
	// sync/atomic — kept at a predictable offset by placing it before pointer
	// fields so it is always 8-byte aligned even on 32-bit builds.
	inFlight int64

	sseBody   io.ReadCloser
	closeOnce sync.Once
	done      chan struct{} // closed when the session is evicted or closed

	pendingMu sync.Mutex
	pending   map[int]chan json.RawMessage // request ID → buffered response channel
}

// close shuts down the session exactly once: it signals any in-flight Calls
// and tears down the SSE connection.
func (sess *Session) close() {
	sess.closeOnce.Do(func() {
		close(sess.done)
		if sess.sseBody != nil {
			_ = sess.sseBody.Close()
		}
	})
}

// connect opens the SSE connection to the sidecar, parses the endpoint event,
// starts the SSE reader goroutine, and performs the MCP initialize handshake.
func (sess *Session) connect(logger *slog.Logger) error {
	resp, err := sseHTTPClient.Get(sess.sidecar.baseURL + "/sse")
	if err != nil {
		return fmt.Errorf("connecting to browser sidecar SSE: %w", err)
	}
	sess.sseBody = resp.Body

	br := bufio.NewReader(resp.Body)

	// Read the initial endpoint event synchronously before starting the goroutine.
	msgPath, err := parseEndpointEvent(br)
	if err != nil {
		_ = resp.Body.Close()
		return fmt.Errorf("reading sidecar endpoint event: %w", err)
	}
	sess.msgPath = msgPath

	// Extract the sessionId fragment from /messages?sessionId=<id>[&...].
	if idx := strings.Index(msgPath, "sessionId="); idx >= 0 {
		tail := msgPath[idx+len("sessionId="):]
		if amp := strings.IndexByte(tail, '&'); amp >= 0 {
			sess.sessionID = tail[:amp]
		} else {
			sess.sessionID = tail
		}
	}

	// The goroutine owns br from this point onward; connect() no longer reads it.
	go sess.readSSE(br, logger)

	if err := sess.initialize(logger); err != nil {
		return fmt.Errorf("MCP initialize handshake: %w", err)
	}
	return nil
}

// parseEndpointEvent reads SSE lines from br until it finds and returns the data
// from an "endpoint" event.  It consumes exactly the bytes up to and including
// the blank line that terminates the event, leaving the rest of br intact for
// the readSSE goroutine.
func parseEndpointEvent(br *bufio.Reader) (string, error) {
	var eventType, dataLine string
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			eventType = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			dataLine = strings.TrimPrefix(line, "data: ")
		case line == "":
			if eventType == "endpoint" && dataLine != "" {
				return dataLine, nil
			}
			eventType = ""
			dataLine = ""
		}
		if err != nil {
			if err == io.EOF {
				return "", fmt.Errorf("SSE stream closed before endpoint event")
			}
			return "", fmt.Errorf("reading SSE stream: %w", err)
		}
	}
}

// readSSE is the long-running goroutine that parses SSE events from br and
// dispatches JSON-RPC responses to the appropriate pending channel.
func (sess *Session) readSSE(br *bufio.Reader, logger *slog.Logger) {
	var currentEvent, currentData string
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")

		switch {
		case strings.HasPrefix(line, "event: "):
			currentEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			currentData = strings.TrimPrefix(line, "data: ")
		case line == "":
			if currentEvent == "message" && currentData != "" {
				sess.dispatchMessage([]byte(currentData), logger)
			}
			currentEvent = ""
			currentData = ""
		}

		if err != nil {
			select {
			case <-sess.done:
				// Expected: session was closed.
			default:
				logger.Debug("browser SSE stream closed unexpectedly",
					"thread_id", sess.threadID, "error", err)
			}
			return
		}
	}
}

// dispatchMessage parses a JSON-RPC response line from the SSE stream and
// delivers it to the matching pending channel (if any).
func (sess *Session) dispatchMessage(data []byte, logger *slog.Logger) {
	var msg struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		logger.Debug("browser SSE: failed to unmarshal message", "error", err, "raw", string(data))
		return
	}
	id, ok := extractIntID(msg.ID)
	if !ok {
		return // notification (no id) or unparseable
	}

	sess.pendingMu.Lock()
	ch, exists := sess.pending[id]
	if exists {
		delete(sess.pending, id)
	}
	sess.pendingMu.Unlock()

	if exists {
		select {
		case ch <- json.RawMessage(data):
		default:
			logger.Debug("browser SSE: dropped duplicate response", "id", id)
		}
	}
}

// extractIntID parses a JSON-RPC id field (number or quoted-number string) to int.
func extractIntID(raw json.RawMessage) (int, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int(f), true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

// initialize performs the MCP initialization handshake: sends an "initialize"
// request, waits for its response, then sends "notifications/initialized".
func (sess *Session) initialize(logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sess.mu.Lock()
	id := sess.nextID
	sess.nextID++
	sess.mu.Unlock()

	ch := make(chan json.RawMessage, 1)
	sess.pendingMu.Lock()
	sess.pending[id] = ch
	sess.pendingMu.Unlock()
	defer func() {
		sess.pendingMu.Lock()
		delete(sess.pending, id)
		sess.pendingMu.Unlock()
	}()

	if err := sess.postRPC(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "auxot-tools", "version": "1.0"},
		},
	}); err != nil {
		return fmt.Errorf("sending initialize: %w", err)
	}

	select {
	case raw := <-ch:
		var resp struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &resp); err == nil && resp.Error != nil {
			return fmt.Errorf("initialize error %d: %s", resp.Error.Code, resp.Error.Message)
		}
	case <-ctx.Done():
		return fmt.Errorf("timeout waiting for initialize response: %w", ctx.Err())
	case <-sess.done:
		return fmt.Errorf("session closed during initialize")
	}

	// notifications/initialized has no id — it is a notification, no response expected.
	return sess.postRPC(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
}

// postRPC marshals req to JSON and POSTs it to the sidecar's message endpoint.
// For the SSE transport, the sidecar responds with 202 Accepted immediately;
// the actual JSON-RPC response arrives asynchronously on the SSE stream.
func (sess *Session) postRPC(req any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal RPC request: %w", err)
	}
	resp, err := rpcHTTPClient.Post(
		sess.sidecar.baseURL+sess.msgPath,
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("POST RPC request: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("RPC POST: server returned %d", resp.StatusCode)
	}
	return nil
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

// GetOrCreate returns the existing Session for threadID, or creates and connects
// a new one.  It touches lastUsed on every call.
func (r *Registry) GetOrCreate(threadID string) (*Session, error) {
	// Fast path: session already exists.
	r.mu.RLock()
	sess, ok := r.sessions[threadID]
	r.mu.RUnlock()
	if ok {
		sess.mu.Lock()
		sess.lastUsed = r.now()
		sess.mu.Unlock()
		return sess, nil
	}

	// Slow path: create under write lock, then connect outside the lock.
	r.mu.Lock()
	// Double-check: another goroutine may have created the session while we waited.
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
		pending:  make(map[int]chan json.RawMessage),
		done:     make(chan struct{}),
	}
	r.sessions[threadID] = sess
	r.mu.Unlock()

	// Connect outside the registry lock so we don't block other goroutines.
	if err := sess.connect(r.logger); err != nil {
		r.mu.Lock()
		delete(r.sessions, threadID)
		r.mu.Unlock()
		go sess.close() // release resources asynchronously
		return nil, fmt.Errorf("browser session for thread %q: %w", threadID, err)
	}
	return sess, nil
}

// Call sends a single MCP JSON-RPC request on sess and returns the result JSON.
// It touches the session's lastUsed timestamp after the POST is accepted.
func (r *Registry) Call(ctx context.Context, sess *Session, method string, params any) (json.RawMessage, error) {
	// Increment in-flight counter so the sweeper never evicts an active session.
	atomic.AddInt64(&sess.inFlight, 1)
	defer atomic.AddInt64(&sess.inFlight, -1)

	sess.mu.Lock()
	id := sess.nextID
	sess.nextID++
	sess.mu.Unlock()

	ch := make(chan json.RawMessage, 1)
	sess.pendingMu.Lock()
	sess.pending[id] = ch
	sess.pendingMu.Unlock()
	defer func() {
		sess.pendingMu.Lock()
		delete(sess.pending, id)
		sess.pendingMu.Unlock()
	}()

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	if err := sess.postRPC(req); err != nil {
		return nil, fmt.Errorf("browser Call POST: %w", err)
	}

	// Touch lastUsed once the POST is accepted (fire-and-forget on the wire).
	sess.mu.Lock()
	sess.lastUsed = r.now()
	sess.mu.Unlock()

	select {
	case raw := <-ch:
		var resp struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("parsing Call response: %w", err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("browser Call context: %w", ctx.Err())
	case <-sess.done:
		return nil, fmt.Errorf("browser session closed during Call")
	}
}

// closeSession removes threadID from the sessions map and tears down the session.
// Must be called with r.mu write-locked.
func (r *Registry) closeSession(threadID string) {
	sess, ok := r.sessions[threadID]
	if !ok {
		return
	}
	delete(r.sessions, threadID)
	// I/O in a goroutine so we don't hold the registry lock during close.
	go func() {
		r.logger.Info("browser session evicted (idle TTL)", "thread_id", threadID)
		sess.close()
	}()
}

// sweepOnce closes all sessions that have been idle for more than 30 minutes.
// Sessions with in-flight Call() goroutines are never evicted regardless of
// idle time, so a slow tool call (e.g. long screenshot) is never torn down mid-flight.
func (r *Registry) sweepOnce() {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for threadID, sess := range r.sessions {
		if atomic.LoadInt64(&sess.inFlight) > 0 {
			continue // active call — never evict
		}
		sess.mu.Lock()
		idle := now.Sub(sess.lastUsed)
		sess.mu.Unlock()
		if idle > 30*time.Minute {
			r.closeSession(threadID)
		}
	}
}

// sweeper runs every 5 minutes, closing sessions idle for more than 30 minutes.
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
