package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/auxothq/auxot/pkg/auth"
	"github.com/auxothq/auxot/pkg/gpu"
	"github.com/auxothq/auxot/pkg/protocol"
	"github.com/auxothq/auxot/pkg/queue"
)

// Token stream TTL tiers.
//
// Streams are ephemeral — they exist only to ferry tokens from the GPU worker
// to the API caller. These TTLs ensure every stream is cleaned up even if the
// happy path (job.complete → 3 min TTL) is never reached.
//
//   - streamTTLDispatched: Set when the job is first dispatched to a worker.
//     This is the safety-net — if the worker crashes before producing any
//     output and the stale-job detector hasn't fired yet, the stream will
//     still expire on its own.
//
//   - streamTTLStreaming: Reset on every token event. While a job is actively
//     generating tokens the TTL keeps getting pushed forward. If the worker
//     stalls mid-stream, the stream expires after 10 minutes of silence.
//
//   - streamTTLDone: Set after the terminal event (done, error, or stale-job
//     timeout). The API caller has a short window to finish reading, then
//     Redis reclaims the memory.
const (
	streamTTLDispatched = 30 * time.Minute
	streamTTLStreaming  = 10 * time.Minute
	streamTTLDone      = 3 * time.Minute
)

// newWorkerID generates a random hex ID for a worker.
func newWorkerID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// activeJob tracks a single in-flight job assignment.
type activeJob struct {
	entryID    string    // Redis stream entry ID (for ACKing)
	assignedAt time.Time // When the job was dispatched to the worker
}

// workerConn tracks a single connected GPU worker.
type workerConn struct {
	id     string
	conn   *websocket.Conn
	mu     sync.Mutex // Protects writes to conn (WebSocket is not thread-safe for writes)
	cancel context.CancelFunc // Cancels the job reader goroutine

	// Track in-flight jobs for this worker (used by the job reader to
	// respect max parallelism). Protected by jobsMu.
	activeJobs map[string]activeJob // jobID → activeJob
	maxSlots   int
	jobsMu     sync.Mutex
}

// sendMessage marshals and sends a server message to the worker.
func (wc *workerConn) sendMessage(msg interface{}) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	wc.mu.Lock()
	defer wc.mu.Unlock()
	return wc.conn.WriteMessage(websocket.TextMessage, data)
}

// hasCapacity returns true if the worker can accept another job.
func (wc *workerConn) hasCapacity() bool {
	wc.jobsMu.Lock()
	defer wc.jobsMu.Unlock()
	return len(wc.activeJobs) < wc.maxSlots
}

// assignResult indicates the outcome of an assignJob attempt.
type assignResult int

const (
	assignOK         assignResult = iota // Job was assigned to a slot
	assignDuplicate                      // Job is already in-flight (same jobID)
	assignAtCapacity                     // Worker has no free slots
)

// assignJob records a job as in-flight. Returns assignDuplicate if the job
// is already tracked (prevents double-dispatch from ReadPending seeing a
// message that was already dispatched but not yet ACKed), assignAtCapacity
// if all slots are full, or assignOK on success.
func (wc *workerConn) assignJob(jobID, entryID string) assignResult {
	wc.jobsMu.Lock()
	defer wc.jobsMu.Unlock()
	if _, exists := wc.activeJobs[jobID]; exists {
		return assignDuplicate
	}
	if len(wc.activeJobs) >= wc.maxSlots {
		return assignAtCapacity
	}
	wc.activeJobs[jobID] = activeJob{
		entryID:    entryID,
		assignedAt: time.Now(),
	}
	return assignOK
}

// completeJob removes a job from the in-flight set.
// Returns the stream entry ID for ACKing, or empty string if not found.
func (wc *workerConn) completeJob(jobID string) string {
	wc.jobsMu.Lock()
	defer wc.jobsMu.Unlock()
	aj, ok := wc.activeJobs[jobID]
	if ok {
		delete(wc.activeJobs, jobID)
	}
	return aj.entryID
}

// staleJobs returns jobs that have been in-flight longer than the given timeout.
// These jobs were dispatched to the worker but no completion/error was received.
func (wc *workerConn) staleJobs(timeout time.Duration) []struct {
	jobID   string
	entryID string
	age     time.Duration
} {
	wc.jobsMu.Lock()
	defer wc.jobsMu.Unlock()
	now := time.Now()
	var stale []struct {
		jobID   string
		entryID string
		age     time.Duration
	}
	for jobID, aj := range wc.activeJobs {
		age := now.Sub(aj.assignedAt)
		if age > timeout {
			stale = append(stale, struct {
				jobID   string
				entryID string
				age     time.Duration
			}{jobID: jobID, entryID: aj.entryID, age: age})
		}
	}
	return stale
}

// activeJobIDs returns a copy of the in-flight job IDs.
func (wc *workerConn) activeJobIDs() []string {
	wc.jobsMu.Lock()
	defer wc.jobsMu.Unlock()
	ids := make([]string, 0, len(wc.activeJobs))
	for id := range wc.activeJobs {
		ids = append(ids, id)
	}
	return ids
}

// WSHandler handles WebSocket connections from both GPU workers and tools workers.
//
// For GPU workers, it runs two goroutines per connection:
//  1. WebSocket read loop — receives tokens, completions, heartbeats
//  2. Job reader loop — XREADGROUP from Redis as this worker's consumer
//
// For tools workers, it delegates to ToolsWSHandler which manages the
// tool call dispatch and continuation pipeline.
type WSHandler struct {
	verifier    *auth.Verifier
	pool        *gpu.Pool
	jobQueue    *queue.JobQueue
	tokenStream *queue.TokenStream
	heartbeat   *queue.Heartbeat
	config      *Config
	logger      *slog.Logger
	tools       *ToolsWSHandler // nil if tools key not configured

	// Track active GPU worker connections for sending jobs
	workers   map[string]*workerConn
	workersMu sync.RWMutex

	upgrader websocket.Upgrader
}

// NewWSHandler creates a WebSocket handler for GPU worker connections.
func NewWSHandler(
	verifier *auth.Verifier,
	pool *gpu.Pool,
	jobQueue *queue.JobQueue,
	tokenStream *queue.TokenStream,
	heartbeat *queue.Heartbeat,
	config *Config,
	logger *slog.Logger,
	toolsHandler *ToolsWSHandler,
) *WSHandler {
	return &WSHandler{
		verifier:    verifier,
		pool:        pool,
		jobQueue:    jobQueue,
		tokenStream: tokenStream,
		heartbeat:   heartbeat,
		config:      config,
		logger:      logger,
		tools:       toolsHandler,
		workers:     make(map[string]*workerConn),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

// ServeHTTP upgrades the connection to WebSocket and enters the worker message loop.
func (h *WSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Error("websocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	// First message must be a hello
	_, msgData, err := conn.ReadMessage()
	if err != nil {
		h.logger.Error("reading hello message", "error", err)
		return
	}

	msg, err := protocol.ParseMessage(msgData)
	if err != nil {
		h.logger.Error("parsing hello message", "error", err)
		return
	}

	hello, ok := msg.(protocol.HelloMessage)
	if !ok {
		h.logger.Error("first message must be hello", "got_type", fmt.Sprintf("%T", msg))
		return
	}

	// Route tools workers to the tools handler.
	if hello.WorkerType == protocol.WorkerTypeTools {
		if h.tools == nil {
			h.logger.Warn("tools worker connection refused — AUXOT_TOOLS_KEY_HASH not configured")
			errMsg := protocol.ErrorMessage{
				Type:  protocol.TypeError,
				Error: "tools workers not enabled on this router (AUXOT_TOOLS_KEY_HASH not set)",
			}
			data, _ := json.Marshal(errMsg)
			conn.WriteMessage(websocket.TextMessage, data) //nolint:errcheck
			return
		}
		h.tools.HandleToolsWorker(conn, hello)
		return
	}

	// GPU worker path — verify admin key.
	valid, err := h.verifier.VerifyAdminKey(hello.GPUKey)
	if err != nil || !valid {
		h.logger.Warn("invalid GPU key", "error", err)
		errMsg := protocol.ErrorMessage{
			Type:    protocol.TypeError,
			Error:   "invalid GPU key",
			Details: "Authentication failed. Check your admin key.",
		}
		data, _ := json.Marshal(errMsg)
		conn.WriteMessage(websocket.TextMessage, data)
		return
	}

	// Generate worker ID and add to pool
	workerID := newWorkerID()
	maxSlots := h.config.MaxParallel
	if hello.Capabilities.TotalSlots > 0 {
		maxSlots = hello.Capabilities.TotalSlots
	}

	caps := gpu.Capabilities{
		Backend:    hello.Capabilities.Backend,
		Model:      hello.Capabilities.Model,
		CtxSize:    hello.Capabilities.CtxSize,
		VRAMGB:     hello.Capabilities.VRAMGB,
		Parameters: hello.Capabilities.Parameters,
		TotalSlots: maxSlots,
	}

	if err := h.pool.Add(workerID, caps, maxSlots); err != nil {
		h.logger.Error("adding worker to pool", "error", err)
		return
	}

	// Create cancellable context for this worker's goroutines
	workerCtx, workerCancel := context.WithCancel(context.Background())

	// Track the connection
	wc := &workerConn{
		id:         workerID,
		conn:       conn,
		cancel:     workerCancel,
		activeJobs: make(map[string]activeJob),
		maxSlots:   maxSlots,
	}
	h.workersMu.Lock()
	h.workers[workerID] = wc
	h.workersMu.Unlock()

	// Infer capabilities from model name
	policyCaps := inferCapabilities(h.config.ModelName)

	// Send hello_ack with policy so worker knows what model to run
	ack := protocol.HelloAckMessage{
		Type:    protocol.TypeHelloAck,
		Success: true,
		GPUID:   workerID,
		Policy: &protocol.Policy{
			ModelName:      h.config.ModelName,
			Quantization:   h.config.Quantization,
			ContextSize:    h.config.ContextSize,
			MaxParallelism: h.config.MaxParallel,
			Capabilities:   policyCaps,
		},
	}
	if err := wc.sendMessage(ack); err != nil {
		h.logger.Error("sending hello_ack", "worker_id", workerID, "error", err)
		h.cleanup(wc)
		return
	}

	h.logger.Info("worker connected",
		"worker_id", workerID,
		"model", caps.Model,
		"backend", caps.Backend,
		"max_slots", maxSlots,
		"pool_size", h.pool.Size(),
	)

	// Start the job reader goroutine — pulls jobs from Redis for this consumer
	go h.jobReaderLoop(workerCtx, wc)

	// Start the heartbeat ticker — keeps this consumer's heartbeat alive
	go h.heartbeatLoop(workerCtx, wc)

	// Enter WebSocket message loop (blocks until disconnect)
	h.messageLoop(wc)
}

// jobReaderLoop reads jobs from the Redis stream as this worker's consumer.
//
// Connection strategy:
//   - Blocking XREADGROUP uses a DEDICATED Redis connection (pool size 1)
//     so it doesn't hold a connection from the shared pool while waiting.
//   - Non-blocking operations (ReadPending, Ack) use the SHARED pool via
//     h.jobQueue — these are fast and release the connection immediately.
//   - Heartbeats run in a separate goroutine on the shared pool.
//
// This means 100 workers = 100 dedicated connections for blocking reads
// + 1 shared pool (~10-20 connections) for everything else.
// Redis handles 10,000+ connections easily.
func (h *WSHandler) jobReaderLoop(ctx context.Context, wc *workerConn) {
	logger := h.logger.With("worker_id", wc.id, "loop", "job_reader")

	// Create a dedicated Redis connection for this worker's blocking reads.
	// This connection does nothing but XREADGROUP BLOCK — it won't interfere
	// with heartbeats, acks, enqueues, or token stream operations on the shared pool.
	blockingReader, cleanup := h.jobQueue.NewBlockingReader()
	defer cleanup()

	logger.Debug("job reader started", "dedicated_conn", true)

	for {
		select {
		case <-ctx.Done():
			logger.Debug("job reader stopping")
			return
		default:
		}

		// Wait for capacity
		if !wc.hasCapacity() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}

		// Expire stale jobs — if a job has been in-flight longer than
		// JobTimeout without the worker sending completion/error, forcibly
		// clean it up. This prevents a single stuck job from permanently
		// consuming a slot and blocking the PEL.
		h.expireStaleJobs(ctx, wc)

		// First, check for pending entries (jobs assigned to this consumer
		// that weren't ACKed — e.g., from a previous connection).
		// Uses SHARED pool — ReadPending is non-blocking (Block: -1).
		pending, err := h.jobQueue.ReadPending(ctx, wc.id)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Error("reading pending entries", "error", err)
			time.Sleep(1 * time.Second)
			continue
		}

		if len(pending) > 0 {
			anyDispatched := false
			for _, entry := range pending {
				if ctx.Err() != nil {
					return
				}
				result := h.dispatchToWorker(ctx, wc, entry)
				switch result {
				case dispatched:
					anyDispatched = true
				case workerFull:
					break // No point trying more entries
				case alreadyAssigned:
					// Skip — job is already in-flight, will be ACKed on completion
				case dispatchFailed:
					// Already logged/ACKed inside dispatchToWorker
				}
			}
			if !anyDispatched {
				// All pending entries are already in-flight — backoff to avoid
				// a tight spin loop. The entries will be ACKed when the worker
				// sends completion messages.
				logger.Debug("all pending entries already in-flight, backing off",
					"pending_count", len(pending),
					"active_jobs", len(wc.activeJobIDs()),
				)
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
			}
			continue // Re-check pending before reading new entries
		}

		// No pending — block for new jobs on the DEDICATED connection.
		// 30 second block timeout. The connection is dedicated so this is cheap —
		// it just sits there waiting. Context cancellation (worker disconnect)
		// interrupts the block immediately.
		entries, err := blockingReader.Read(ctx, wc.id, 30*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Error("reading from job queue", "error", err)
			time.Sleep(1 * time.Second)
			continue
		}

		for _, entry := range entries {
			if ctx.Err() != nil {
				return
			}
			h.dispatchToWorker(ctx, wc, entry)
		}
	}
}

// dispatchResult indicates what happened when we tried to dispatch a job.
type dispatchResult int

const (
	dispatched      dispatchResult = iota // Job was sent to the worker
	alreadyAssigned                       // Job is already in-flight (skip)
	workerFull                            // Worker has no free slots
	dispatchFailed                        // Parse or send error
)

// dispatchToWorker sends a single job to the worker via WebSocket.
func (h *WSHandler) dispatchToWorker(ctx context.Context, wc *workerConn, entry queue.StreamEntry) dispatchResult {
	var jobMsg protocol.JobMessage
	if err := json.Unmarshal(entry.Payload.Data, &jobMsg); err != nil {
		h.logger.Error("parsing job data",
			"worker_id", wc.id,
			"job_id", entry.Payload.JobID,
			"error", err,
		)
		// ACK corrupt entries so they don't block the queue
		_ = h.jobQueue.Ack(ctx, entry.ID)
		return dispatchFailed
	}

	jobMsg.Type = protocol.TypeJob
	if jobMsg.JobID == "" {
		jobMsg.JobID = entry.Payload.JobID
	}

	// Inject allowed tool definitions from connected tools workers.
	// Only inject for jobs that don't already have these tools (avoids duplicates
	// in continuation rounds where we already included them in the job).
	if h.tools != nil {
		if defs := h.tools.AllowedToolDefs(); len(defs) > 0 {
			existing := make(map[string]bool, len(jobMsg.Tools))
			for _, t := range jobMsg.Tools {
				existing[t.Function.Name] = true
			}
			for _, def := range defs {
				if !existing[def.Function.Name] {
					jobMsg.Tools = append(jobMsg.Tools, def)
				}
			}
		}
		// Save job context for potential continuation.
		h.tools.SaveJobContext(jobMsg.JobID, &jobMsg)
	}

	// Track this job as in-flight
	result := wc.assignJob(jobMsg.JobID, entry.ID)
	switch result {
	case assignDuplicate:
		// Already dispatched and in-flight — skip silently.
		// The pending entry will be ACKed when the worker completes the job.
		return alreadyAssigned
	case assignAtCapacity:
		// Genuinely at capacity — shouldn't happen because we check before
		// reading, but a race is possible. Leave the entry pending for retry.
		h.logger.Warn("worker at capacity, leaving job pending",
			"worker_id", wc.id,
			"job_id", jobMsg.JobID,
		)
		return workerFull
	}

	// Track in pool (for /health endpoint visibility)
	_ = h.pool.AssignJob(wc.id, jobMsg.JobID)

	// Send job via WebSocket
	if err := wc.sendMessage(&jobMsg); err != nil {
		wc.completeJob(jobMsg.JobID)             // Rollback local tracking
		h.pool.CompleteJob(wc.id, jobMsg.JobID)  // Rollback pool tracking
		h.logger.Error("sending job to worker",
			"worker_id", wc.id,
			"job_id", jobMsg.JobID,
			"error", err,
		)
		// Leave the entry pending in the PEL — sweeper or reconnect will handle it.
		return dispatchFailed
	}

	h.logger.Info("job dispatched",
		"worker_id", wc.id,
		"job_id", jobMsg.JobID,
	)

	// Create the token stream early and set a generous safety-net TTL.
	// The stream is created by publishing a sentinel event the API reader
	// will silently skip (it only handles "token", "done", "error").
	// If the worker crashes before producing any output, the stream still
	// expires after 30 minutes instead of living forever.
	sentinel := queue.TokenEvent{Type: "job_dispatched"}
	if _, err := h.tokenStream.Publish(ctx, jobMsg.JobID, sentinel); err != nil {
		h.logger.Error("publishing job_dispatched sentinel", "job_id", jobMsg.JobID, "error", err)
	}
	if err := h.tokenStream.SetTTL(ctx, jobMsg.JobID, streamTTLDispatched); err != nil {
		h.logger.Error("setting initial token stream TTL", "job_id", jobMsg.JobID, "error", err)
	}

	return dispatched
}

// expireStaleJobs checks for jobs that have been in-flight longer than
// JobTimeout and forcibly cleans them up. This is a safety net for when
// the worker receives a job but never sends back a completion or error
// (e.g., job dispatched right before a crash, or worker silently dropped it).
//
// For each expired job, we:
//  1. Remove it from activeJobs (frees the slot)
//  2. ACK it in Redis (removes from PEL so it stops being re-delivered)
//  3. Publish an error to the token stream (so the waiting API caller gets a response)
func (h *WSHandler) expireStaleJobs(ctx context.Context, wc *workerConn) {
	stale := wc.staleJobs(h.config.JobTimeout)
	if len(stale) == 0 {
		return
	}

	for _, s := range stale {
		h.logger.Warn("expiring stale job — worker never completed it",
			"worker_id", wc.id,
			"job_id", s.jobID,
			"age", s.age.Round(time.Second).String(),
			"timeout", h.config.JobTimeout.String(),
		)

		// Remove from activeJobs
		wc.completeJob(s.jobID)

		// ACK the PEL entry so it stops being re-delivered
		if s.entryID != "" {
			if err := h.jobQueue.Ack(ctx, s.entryID); err != nil {
				h.logger.Error("acking expired job", "job_id", s.jobID, "error", err)
			}
		}

		// Publish error to token stream so the waiting client gets a response
		errData, _ := json.Marshal(map[string]string{
			"error":   "job_timeout",
			"details": fmt.Sprintf("Job timed out after %s without completion from worker", h.config.JobTimeout),
		})
		event := queue.TokenEvent{
			Type: "error",
			Data: errData,
		}
		if _, err := h.tokenStream.Publish(ctx, s.jobID, event); err != nil {
			h.logger.Error("publishing timeout error event", "job_id", s.jobID, "error", err)
		}

		// Set short TTL so the timed-out stream is cleaned up.
		// Previously this path never set TTL — streams leaked forever.
		if err := h.tokenStream.SetTTL(ctx, s.jobID, streamTTLDone); err != nil {
			h.logger.Error("setting token stream TTL after timeout", "job_id", s.jobID, "error", err)
		}

		// Remove from pool tracking
		h.pool.CompleteJob(wc.id, s.jobID)
	}
}

// heartbeatLoop refreshes the worker's heartbeat key in Redis.
func (h *WSHandler) heartbeatLoop(ctx context.Context, wc *workerConn) {
	ticker := time.NewTicker(h.config.HeartbeatInterval)
	defer ticker.Stop()

	// Initial heartbeat immediately
	if err := h.heartbeat.Ping(ctx, wc.id); err != nil {
		h.logger.Error("initial heartbeat", "worker_id", wc.id, "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := h.heartbeat.Ping(ctx, wc.id); err != nil {
				h.logger.Error("heartbeat ping", "worker_id", wc.id, "error", err)
			}
		}
	}
}

// messageLoop reads messages from a connected worker until disconnect.
func (h *WSHandler) messageLoop(wc *workerConn) {
	defer h.cleanup(wc)

	for {
		_, msgData, err := wc.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				h.logger.Warn("worker disconnected unexpectedly", "worker_id", wc.id, "error", err)
			} else {
				h.logger.Info("worker disconnected", "worker_id", wc.id)
			}
			return
		}

		msg, err := protocol.ParseMessage(msgData)
		if err != nil {
			h.logger.Warn("invalid message from worker", "worker_id", wc.id, "error", err)
			continue
		}

		switch m := msg.(type) {
		case protocol.HeartbeatMessage:
			h.handleHeartbeat(wc, &m)
		case protocol.ConfigMessage:
			h.handleConfig(wc, &m)
		case protocol.TokenMessage:
			h.handleToken(wc, &m)
		case protocol.ToolGeneratingMessage:
			h.handleToolGenerating(wc, &m)
		case protocol.CompleteMessage:
			h.handleComplete(wc, &m)
		case protocol.ErrorMessage:
			h.handleWorkerError(wc, &m)
		default:
			h.logger.Warn("unexpected message type from worker", "worker_id", wc.id, "type", fmt.Sprintf("%T", m))
		}
	}
}

func (h *WSHandler) handleConfig(wc *workerConn, msg *protocol.ConfigMessage) {
	caps := gpu.Capabilities{
		Backend:    msg.Capabilities.Backend,
		Model:      msg.Capabilities.Model,
		CtxSize:    msg.Capabilities.CtxSize,
		VRAMGB:     msg.Capabilities.VRAMGB,
		Parameters: msg.Capabilities.Parameters,
		TotalSlots: msg.Capabilities.TotalSlots,
	}

	h.pool.UpdateCapabilities(wc.id, caps)

	// Update max slots if capabilities report different
	if caps.TotalSlots > 0 {
		wc.jobsMu.Lock()
		wc.maxSlots = caps.TotalSlots
		wc.jobsMu.Unlock()
	}

	h.logger.Info("worker config updated",
		"worker_id", wc.id,
		"model", caps.Model,
		"backend", caps.Backend,
		"ctx_size", caps.CtxSize,
		"total_slots", caps.TotalSlots,
	)

	ack := protocol.ConfigAckMessage{
		Type:    protocol.TypeConfigAck,
		Success: true,
	}
	if err := wc.sendMessage(ack); err != nil {
		h.logger.Error("sending config_ack", "worker_id", wc.id, "error", err)
	}
}

func (h *WSHandler) handleHeartbeat(wc *workerConn, _ *protocol.HeartbeatMessage) {
	h.pool.Heartbeat(wc.id)

	ack := protocol.HeartbeatAckMessage{
		Type: protocol.TypeHeartbeatAck,
	}
	if err := wc.sendMessage(ack); err != nil {
		h.logger.Error("sending heartbeat_ack", "worker_id", wc.id, "error", err)
	}
}

func (h *WSHandler) handleToolGenerating(wc *workerConn, msg *protocol.ToolGeneratingMessage) {
	ctx := context.Background()
	event := queue.TokenEvent{
		Type: "tool_generating",
		Data: json.RawMessage(`{}`),
	}
	if _, err := h.tokenStream.Publish(ctx, msg.JobID, event); err != nil {
		h.logger.Warn("publish tool_generating failed", "job_id", msg.JobID, "error", err)
	}
}

func (h *WSHandler) handleToken(wc *workerConn, msg *protocol.TokenMessage) {
	ctx := context.Background()

	// Choose event type based on whether this is a reasoning/thinking token
	eventType := "token"
	if msg.Reasoning {
		eventType = "reasoning_token"
	}

	event := queue.TokenEvent{
		Type: eventType,
		Data: json.RawMessage(`"` + escapeJSON(msg.Token) + `"`),
	}
	if _, err := h.tokenStream.Publish(ctx, msg.JobID, event); err != nil {
		h.logger.Error("publishing token", "worker_id", wc.id, "job_id", msg.JobID, "error", err, "reasoning", msg.Reasoning)
	}

	// Keep pushing the TTL forward while the worker is actively streaming.
	// Each token resets the clock to 10 minutes. If the worker stalls
	// mid-stream (no more tokens arrive), the stream expires after 10 min
	// of silence rather than living forever.
	if err := h.tokenStream.SetTTL(ctx, msg.JobID, streamTTLStreaming); err != nil {
		h.logger.Error("refreshing token stream TTL", "job_id", msg.JobID, "error", err)
	}
}

func (h *WSHandler) handleComplete(wc *workerConn, msg *protocol.CompleteMessage) {
	ctx := context.Background()

	// ACK the job in Redis — this removes it from the PEL regardless of tool calls.
	// If we're starting a continuation round, the new job gets its own Redis entry.
	entryID := wc.completeJob(msg.JobID)
	if entryID != "" {
		if err := h.jobQueue.Ack(ctx, entryID); err != nil {
			h.logger.Error("acking completed job", "worker_id", wc.id, "job_id", msg.JobID, "error", err)
		}
	}

	// Remove job from pool's tracking (for /health endpoint).
	h.pool.CompleteJob(wc.id, msg.JobID)

	// If the GPU returned tool calls AND we have a tools worker, start the
	// continuation pipeline. The API caller keeps reading from the same token
	// stream — we don't publish "done" yet.
	if h.tools != nil && len(msg.ToolCalls) > 0 {
		if h.tools.StartContinuation(ctx, msg) {
			h.logger.Info("tool call continuation started",
				"worker_id", wc.id,
				"job_id", msg.JobID,
				"tool_calls", len(msg.ToolCalls),
			)
			// Don't publish "done" — the continuation will re-enqueue the job
			// and the token stream will keep flowing for the next round.
			return
		}
		// StartContinuation returned false (no tools worker or no context).
		// Fall through and publish the completion as-is with finish_reason=tool_calls.
		h.logger.Info("tool calls not handled by tools worker — passing through to API caller",
			"job_id", msg.JobID,
			"tool_calls", len(msg.ToolCalls),
		)
	}

	// Final completion — publish "done" to the token stream.
	data, _ := json.Marshal(msg)
	event := queue.TokenEvent{
		Type: "done",
		Data: data,
	}
	if _, err := h.tokenStream.Publish(ctx, msg.JobID, event); err != nil {
		h.logger.Error("publishing completion", "worker_id", wc.id, "job_id", msg.JobID, "error", err)
	}

	// Shorten TTL now that the job is done.
	if err := h.tokenStream.SetTTL(ctx, msg.JobID, streamTTLDone); err != nil {
		h.logger.Error("setting token stream TTL", "job_id", msg.JobID, "error", err)
	}

	// Clean up saved job context.
	if h.tools != nil {
		h.tools.DeleteJobContext(msg.JobID)
	}

	h.logger.Info("job completed",
		"worker_id", wc.id,
		"job_id", msg.JobID,
		"input_tokens", msg.InputTokens,
		"output_tokens", msg.OutputTokens,
		"tool_calls", len(msg.ToolCalls),
	)
}

func (h *WSHandler) handleWorkerError(wc *workerConn, msg *protocol.ErrorMessage) {
	h.logger.Error("worker reported error",
		"worker_id", wc.id,
		"job_id", msg.JobID,
		"error", msg.Error,
		"details", msg.Details,
	)

	if msg.JobID != "" {
		ctx := context.Background()

		// Publish error to token stream
		data, _ := json.Marshal(map[string]string{"error": msg.Error, "details": msg.Details})
		event := queue.TokenEvent{
			Type: "error",
			Data: data,
		}
		if _, err := h.tokenStream.Publish(ctx, msg.JobID, event); err != nil {
			h.logger.Error("publishing error event", "job_id", msg.JobID, "error", err)
		}

		// Set short TTL so the error stream is cleaned up.
		// Previously this path never set TTL — streams leaked forever.
		if err := h.tokenStream.SetTTL(ctx, msg.JobID, streamTTLDone); err != nil {
			h.logger.Error("setting token stream TTL after error", "job_id", msg.JobID, "error", err)
		}

		// ACK the failed job so it doesn't get re-dispatched
		entryID := wc.completeJob(msg.JobID)
		if entryID != "" {
			if err := h.jobQueue.Ack(ctx, entryID); err != nil {
				h.logger.Error("acking errored job", "job_id", msg.JobID, "error", err)
			}
		}

		h.pool.CompleteJob(wc.id, msg.JobID)
	}
}

// SendJob sends a job message to a specific worker.
// Used by tests and admin tools; normal dispatch goes through the job reader goroutine.
func (h *WSHandler) SendJob(workerID string, job *protocol.JobMessage) error {
	h.workersMu.RLock()
	wc, ok := h.workers[workerID]
	h.workersMu.RUnlock()
	if !ok {
		return fmt.Errorf("worker %q not connected", workerID)
	}
	return wc.sendMessage(job)
}

// cleanup removes a worker from all tracking and publishes errors for orphaned jobs.
func (h *WSHandler) cleanup(wc *workerConn) {
	// Cancel the job reader and heartbeat goroutines
	wc.cancel()

	// Remove heartbeat from Redis
	if err := h.heartbeat.Remove(context.Background(), wc.id); err != nil {
		h.logger.Error("removing heartbeat", "worker_id", wc.id, "error", err)
	}

	// Get orphaned jobs BEFORE removing from pool
	orphanedJobs := wc.activeJobIDs()
	h.pool.Remove(wc.id)

	h.workersMu.Lock()
	delete(h.workers, wc.id)
	h.workersMu.Unlock()

	// Note: we intentionally do NOT publish error events for orphaned jobs here.
	// The sweeper will detect the dead consumer (heartbeat expired) and re-enqueue
	// the pending messages for another worker to pick up. The jobs get retried,
	// not failed.
	//
	// If there are no other workers, the jobs stay in the PEL until one connects.

	if len(orphanedJobs) > 0 {
		h.logger.Warn("worker disconnected with active jobs",
			"worker_id", wc.id,
			"orphaned_jobs", len(orphanedJobs),
			"note", "sweeper will re-enqueue pending jobs for other workers",
		)
	}
}

// inferCapabilities derives model capabilities from the model name.
// This matches the logic in the Node.js websocket server and model registry.
func inferCapabilities(modelName string) []string {
	name := strings.ToLower(modelName)
	var caps []string

	if strings.Contains(name, "vision") || strings.Contains(name, "multimodal") || strings.Contains(name, "vl-") {
		caps = append(caps, "vision")
	}
	if strings.Contains(name, "code") || strings.Contains(name, "coder") || strings.Contains(name, "starcoder") {
		caps = append(caps, "code")
	}
	if strings.Contains(name, "embed") || strings.Contains(name, "embedding") {
		caps = append(caps, "embedding")
	}
	// Reasoning-capable models: DeepSeek-R1, Kimi K2.5, QwQ, etc.
	if strings.Contains(name, "deepseek-r1") || strings.Contains(name, "kimi") || strings.Contains(name, "qwq") {
		caps = append(caps, "reasoning")
	}
	if len(caps) == 0 {
		caps = append(caps, "chat")
	}
	return caps
}

// escapeJSON escapes a string for safe embedding in a JSON string value.
func escapeJSON(s string) string {
	b, _ := json.Marshal(s)
	// json.Marshal wraps in quotes, strip them
	if len(b) >= 2 {
		return string(b[1 : len(b)-1])
	}
	return s
}
