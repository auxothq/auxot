package router

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/auxothq/auxot/pkg/protocol"
	"github.com/auxothq/auxot/pkg/queue"
	pkgtools "github.com/auxothq/auxot/pkg/tools"
	"github.com/auxothq/auxot/pkg/tools/browser"
)

// ─────────────────────────────────────────────────────────────────────────────
// Tools worker connection
// ─────────────────────────────────────────────────────────────────────────────

// toolsConn tracks a single connected tools worker.
type toolsConn struct {
	id     string
	conn   *websocket.Conn
	mu     sync.Mutex
	tools  []string // tool names this worker advertises
	cancel context.CancelFunc
}

func (tc *toolsConn) sendMessage(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.conn.WriteMessage(websocket.TextMessage, data)
}

// ─────────────────────────────────────────────────────────────────────────────
// Continuation pipeline data structures
// ─────────────────────────────────────────────────────────────────────────────

// jobContext saves the parameters of an in-flight GPU job so the router can
// rebuild the message history for continuation rounds after tool results arrive.
type jobContext struct {
	messages        []protocol.ChatMessage
	tools           []protocol.Tool
	temperature     *float64
	maxTokens       *int
	reasoningEffort string
	// threadID is the conversation thread identifier (populated from
	// JobMessage.ReferenceID). Used for browser context isolation and
	// sticky worker routing in later phases.
	threadID string
}

// toolCallResult holds the result (or error) of a single tool invocation.
type toolCallResult struct {
	toolName   string
	result     json.RawMessage    // nil if errMsg is set
	errMsg     string
	imageParts []protocol.ImagePart // populated when tool result carries binary images
}

// pendingContinuation tracks an in-flight multi-tool-call round.
// It is created in handleComplete when the GPU worker returns tool calls,
// and resolved when all tool results arrive.
type pendingContinuation struct {
	jobID        string
	ctx          *jobContext                   // nil for direct (non-LLM) tool calls
	assistantMsg protocol.ChatMessage          // the assistant turn that triggered tool calls
	results      map[string]*toolCallResult    // tool_call_id → result (nil until filled)
	remaining    int
	isDirect     bool   // true for DispatchDirectToolCall — skips continueLLMRound
	mu           sync.Mutex
}

// toolJobRef maps a tool-invocation job ID back to its parent continuation.
type toolJobRef struct {
	parentJobID string
	toolCallID  string
}

// ─────────────────────────────────────────────────────────────────────────────
// ToolsWSHandler — manages tools workers and the continuation pipeline
// ─────────────────────────────────────────────────────────────────────────────

// ToolsWSHandler manages WebSocket connections from tools workers and
// orchestrates the tool call → result → continuation pipeline.
//
// It is embedded in WSHandler so that handleComplete (GPU side) can
// trigger tool dispatch directly.
type ToolsWSHandler struct {
	verifier    interface{ VerifyToolKey(string) (bool, error) }
	jobQueue    *queue.JobQueue
	tokenStream *queue.TokenStream
	config      *Config
	logger      *slog.Logger

	// Connected tools workers.
	toolsWorkers   map[string]*toolsConn
	toolsWorkersMu sync.RWMutex

	// Built-in tool definitions, keyed by tool name.
	// Populated at construction from pkg/tools.BuiltinDefinitions().
	builtinDefs map[string]protocol.Tool

	// In-flight GPU job contexts — needed for continuation rebuilding.
	// Keyed by GPU jobID, populated when the job is dispatched to the GPU worker.
	jobContexts   map[string]*jobContext
	jobContextsMu sync.Mutex

	// Pending continuations — one per GPU job that is waiting for tool results.
	// Keyed by GPU jobID.
	pendingConts   map[string]*pendingContinuation
	pendingContsMu sync.Mutex

	// Maps tool-invocation job ID → (parentJobID, toolCallID).
	// Lets handleToolResult look up which continuation to update.
	toolJobRefs   map[string]toolJobRef
	toolJobRefsMu sync.Mutex
}

// NewToolsWSHandler creates a ToolsWSHandler.
func NewToolsWSHandler(
	verifier interface{ VerifyToolKey(string) (bool, error) },
	jobQueue *queue.JobQueue,
	tokenStream *queue.TokenStream,
	config *Config,
	logger *slog.Logger,
) *ToolsWSHandler {
	// Index built-in tool definitions by name for fast lookup.
	builtinDefs := make(map[string]protocol.Tool)
	for _, def := range pkgtools.BuiltinDefinitions() {
		builtinDefs[def.Name] = protocol.Tool{
			Type: "function",
			Function: protocol.ToolDefinition{
				Name:        def.Name,
				Description: def.Description,
				Parameters:  def.Parameters,
			},
		}
	}
	builtinDefs["browser"] = protocol.Tool{
		Type: "function",
		Function: protocol.ToolDefinition{
			Name:        browser.Definition.Name,
			Description: browser.Definition.Description,
			Parameters:  browser.Definition.Parameters,
		},
	}

	return &ToolsWSHandler{
		verifier:     verifier,
		jobQueue:     jobQueue,
		tokenStream:  tokenStream,
		config:       config,
		logger:       logger,
		toolsWorkers: make(map[string]*toolsConn),
		builtinDefs:  builtinDefs,
		jobContexts:  make(map[string]*jobContext),
		pendingConts: make(map[string]*pendingContinuation),
		toolJobRefs:  make(map[string]toolJobRef),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Tools worker connection lifecycle
// ─────────────────────────────────────────────────────────────────────────────

// HandleToolsWorker runs the message loop for a connected tools worker.
// It is called from WSHandler.ServeHTTP after the hello handshake confirms
// the worker_type is "tools".
func (h *ToolsWSHandler) HandleToolsWorker(conn *websocket.Conn, hello protocol.HelloMessage) {
	tc := &toolsConn{
		conn: conn,
	}

	// Verify tool connector key.
	valid, err := h.verifier.VerifyToolKey(hello.GPUKey)
	if err != nil || !valid {
		msg := "invalid tool connector key"
		if err != nil {
			msg = fmt.Sprintf("tool key verification error: %s", err)
		}
		h.logger.Warn("tools worker authentication failed", "error", msg)
		errMsg := protocol.ErrorMessage{
			Type:  protocol.TypeError,
			Error: "authentication failed",
		}
		data, _ := json.Marshal(errMsg)
		conn.WriteMessage(websocket.TextMessage, data) //nolint:errcheck
		conn.Close()
		return
	}

	// Assign ID and register.
	tc.id = newToolsWorkerID()
	if hello.ToolsCapabilities != nil {
		tc.tools = hello.ToolsCapabilities.Tools
	}

	workerCtx, cancel := context.WithCancel(context.Background())
	tc.cancel = cancel

	h.toolsWorkersMu.Lock()
	h.toolsWorkers[tc.id] = tc
	h.toolsWorkersMu.Unlock()

	h.logger.Info("tools worker connected",
		"tool_id", tc.id,
		"tools", tc.tools,
		"pool_size", h.toolsPoolSize(),
	)

	// Acknowledge the connection.
	ack := protocol.HelloAckMessage{
		Type:    protocol.TypeHelloAck,
		Success: true,
		GPUID:   tc.id,
	}
	if err := tc.sendMessage(ack); err != nil {
		h.logger.Error("sending hello_ack to tools worker", "tool_id", tc.id, "error", err)
		h.cleanupToolsWorker(tc)
		cancel()
		return
	}

	// Start heartbeat goroutine.
	go h.toolsHeartbeatLoop(workerCtx, tc)

	// Enter message loop (blocks until disconnect).
	h.toolsMessageLoop(tc)
	cancel()
}

// toolsMessageLoop reads messages from a tools worker until it disconnects.
func (h *ToolsWSHandler) toolsMessageLoop(tc *toolsConn) {
	defer h.cleanupToolsWorker(tc)

	for {
		_, msgData, err := tc.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				h.logger.Warn("tools worker disconnected unexpectedly", "tool_id", tc.id, "error", err)
			} else {
				h.logger.Info("tools worker disconnected", "tool_id", tc.id)
			}
			return
		}

		msg, err := protocol.ParseMessage(msgData)
		if err != nil {
			h.logger.Warn("invalid message from tools worker", "tool_id", tc.id, "error", err)
			continue
		}

		switch m := msg.(type) {
		case protocol.ToolResultMessage:
			h.handleToolResult(tc, &m)
		case protocol.HeartbeatMessage:
			ack := protocol.HeartbeatAckMessage{Type: protocol.TypeHeartbeatAck}
			if err := tc.sendMessage(ack); err != nil {
				h.logger.Error("sending heartbeat_ack to tools worker", "tool_id", tc.id, "error", err)
			}
		case protocol.ErrorMessage:
			h.logger.Error("tools worker reported error",
				"tool_id", tc.id,
				"job_id", m.JobID,
				"error", m.Error,
			)
			if m.JobID != "" {
				// Treat an error message as a failed tool result.
				h.handleToolError(m.JobID, m.Error)
			}
		default:
			h.logger.Warn("unexpected message type from tools worker",
				"tool_id", tc.id,
				"type", fmt.Sprintf("%T", m),
			)
		}
	}
}

func (h *ToolsWSHandler) toolsHeartbeatLoop(ctx context.Context, tc *toolsConn) {
	ticker := time.NewTicker(h.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := tc.sendMessage(protocol.HeartbeatAckMessage{Type: protocol.TypeHeartbeatAck}); err != nil {
				h.logger.Warn("tools heartbeat failed", "tool_id", tc.id, "error", err)
			}
		}
	}
}

func (h *ToolsWSHandler) cleanupToolsWorker(tc *toolsConn) {
	h.toolsWorkersMu.Lock()
	delete(h.toolsWorkers, tc.id)
	h.toolsWorkersMu.Unlock()

	h.logger.Info("tools worker removed from pool",
		"tool_id", tc.id,
		"pool_size", h.toolsPoolSize(),
	)

	// Fail any pending tool jobs that were dispatched to this worker.
	// We find them by scanning toolJobRefs — any ref whose parentJobID has a
	// pending continuation that is still waiting.
	h.failPendingToolJobs(tc.id)
}

// failPendingToolJobs fails all tool call results that were dispatched to the
// given tools worker. Called when the worker disconnects unexpectedly.
func (h *ToolsWSHandler) failPendingToolJobs(toolID string) {
	// Collect all tool job IDs that belong to this worker.
	// We don't currently track which worker a tool job was sent to, so we
	// mark all orphaned pending continuations as failed after a brief delay
	// (the result may already be in-flight).
	//
	// A future improvement: track toolJobID → toolWorkerID for precise cleanup.
	// For now, pending continuations time out via the job timeout on the GPU side.
	h.logger.Debug("tools worker cleanup — pending tool jobs may time out",
		"tool_id", toolID,
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Tool injection into GPU jobs
// ─────────────────────────────────────────────────────────────────────────────

// SaveJobContext stores a GPU job's parameters so we can rebuild the message
// history if tool calls need a continuation. Called from dispatchToWorker
// before sending the job to the GPU.
func (h *ToolsWSHandler) SaveJobContext(jobID string, jobMsg *protocol.JobMessage) {
	if len(h.config.AllowedTools) == 0 {
		return // tools not configured, no context needed
	}
	ctx := &jobContext{
		messages:        jobMsg.Messages,
		tools:           jobMsg.Tools,
		temperature:     jobMsg.Temperature,
		maxTokens:       jobMsg.MaxTokens,
		reasoningEffort: jobMsg.ReasoningEffort,
	}
	if jobMsg.ReferenceID != "" {
		ctx.threadID = jobMsg.ReferenceID
	}
	h.jobContextsMu.Lock()
	h.jobContexts[jobID] = ctx
	h.jobContextsMu.Unlock()
}

// DeleteJobContext removes a job's saved context after the job is fully done.
func (h *ToolsWSHandler) DeleteJobContext(jobID string) {
	h.jobContextsMu.Lock()
	delete(h.jobContexts, jobID)
	h.jobContextsMu.Unlock()
}

// AllowedToolDefs returns the protocol.Tool definitions for the tools in
// AllowedTools that are both configured AND advertised by a connected tools worker.
// Returns nil if no tools worker is connected or no allowed tools are configured.
func (h *ToolsWSHandler) AllowedToolDefs() []protocol.Tool {
	if len(h.config.AllowedTools) == 0 {
		return nil
	}

	// Only inject if at least one tools worker is connected.
	h.toolsWorkersMu.RLock()
	workerCount := len(h.toolsWorkers)
	h.toolsWorkersMu.RUnlock()
	if workerCount == 0 {
		return nil
	}

	var defs []protocol.Tool
	for _, name := range h.config.AllowedTools {
		if def, ok := h.builtinDefs[name]; ok {
			defs = append(defs, def)
		}
	}
	return defs
}

// ─────────────────────────────────────────────────────────────────────────────
// Tool call dispatch (GPU side → tools workers)
// ─────────────────────────────────────────────────────────────────────────────

// StartContinuation is called from handleComplete when the GPU worker responds
// with tool calls. It creates a pending continuation and dispatches each tool
// call to a connected tools worker.
//
// Returns true if tool calls were handled (caller should NOT publish "done" to
// the token stream). Returns false if no tools worker is available — the caller
// should publish the CompleteMessage as-is (callers handle their own tools).
func (h *ToolsWSHandler) StartContinuation(ctx context.Context, msg *protocol.CompleteMessage) bool {
	if len(msg.ToolCalls) == 0 {
		return false
	}

	// Collect all distinct tool names used in this round so we can select a
	// worker that advertises every one of them.
	toolNameSet := make(map[string]struct{}, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		if tc.Function.Name != "" {
			toolNameSet[tc.Function.Name] = struct{}{}
		}
	}
	allToolNames := make([]string, 0, len(toolNameSet))
	for name := range toolNameSet {
		allToolNames = append(allToolNames, name)
	}

	// Look up the saved job context.
	h.jobContextsMu.Lock()
	jobCtx, ok := h.jobContexts[msg.JobID]
	h.jobContextsMu.Unlock()
	if !ok {
		// No saved context — can't do continuation. Fall through.
		h.logger.Warn("no job context for continuation",
			"job_id", msg.JobID,
			"note", "tool calls will be returned to caller",
		)
		return false
	}

	// Select the worker deterministically by thread_id so all browser (and other
	// stateful) tool calls for this thread land on the same worker.
	// Filter by ALL tool names in this round so a mixed round (e.g. web_fetch +
	// browser) lands on a worker that can handle every tool call.
	threadID := jobCtx.threadID
	worker := h.pickToolsWorkerAll(allToolNames, threadID)

	if worker == nil {
		// No tools worker — let the GPU complete message fall through to the
		// token stream unchanged. API callers handle their own tool_calls.
		return false
	}

	// Build the assistant message that contains the tool_calls.
	assistantMsg := protocol.ChatMessage{
		Role:      "assistant",
		Content:   protocol.ChatContentString(msg.FullResponse),
		ToolCalls: msg.ToolCalls,
	}

	// Create the continuation tracker.
	cont := &pendingContinuation{
		jobID:        msg.JobID,
		ctx:          jobCtx,
		assistantMsg: assistantMsg,
		results:      make(map[string]*toolCallResult, len(msg.ToolCalls)),
		remaining:    len(msg.ToolCalls),
	}
	for _, tc := range msg.ToolCalls {
		cont.results[tc.ID] = nil // placeholder — filled when result arrives
	}

	h.pendingContsMu.Lock()
	h.pendingConts[msg.JobID] = cont
	h.pendingContsMu.Unlock()

	h.logger.Info("dispatching tool calls",
		"job_id", msg.JobID,
		"tool_calls", len(msg.ToolCalls),
		"tool_worker", worker.id,
	)

	// Dispatch each tool call.
	for _, tc := range msg.ToolCalls {
		toolJobID := newToolsWorkerID() // unique ID for this tool invocation

		h.toolJobRefsMu.Lock()
		h.toolJobRefs[toolJobID] = toolJobRef{
			parentJobID: msg.JobID,
			toolCallID:  tc.ID,
		}
		h.toolJobRefsMu.Unlock()

		toolJob := protocol.ToolJobMessage{
			Type:        protocol.TypeToolJob,
			JobID:       toolJobID,
			ParentJobID: msg.JobID,
			ThreadID:    jobCtx.threadID,
			ToolName:    tc.Function.Name,
			ToolCallID:  tc.ID,
			Arguments:   json.RawMessage(tc.Function.Arguments),
			Credentials: h.config.ToolCredentials[tc.Function.Name],
		}

		if err := worker.sendMessage(toolJob); err != nil {
			h.logger.Error("sending tool job to tools worker",
				"tool_id", worker.id,
				"job_id", msg.JobID,
				"tool_call_id", tc.ID,
				"error", err,
			)
			// Fail this tool call immediately.
			h.recordToolResult(msg.JobID, tc.ID, tc.Function.Name, nil, nil, "failed to dispatch to tools worker")
		} else {
			h.logger.Info("tool job dispatched",
				"tool_job_id", toolJobID,
				"parent_job_id", msg.JobID,
				"tool", tc.Function.Name,
				"tool_call_id", tc.ID,
			)
		}
	}

	return true
}

// ─────────────────────────────────────────────────────────────────────────────
// Tool result handling (tools workers → GPU continuation)
// ─────────────────────────────────────────────────────────────────────────────

// handleToolResult processes a ToolResultMessage from a tools worker.
func (h *ToolsWSHandler) handleToolResult(tc *toolsConn, msg *protocol.ToolResultMessage) {
	h.logger.Info("tool result received",
		"tool_id", tc.id,
		"job_id", msg.JobID,
		"parent_job_id", msg.ParentJobID,
		"tool", msg.ToolName,
		"tool_call_id", msg.ToolCallID,
		"duration_ms", msg.DurationMS,
		"has_error", msg.Error != "",
	)

	// Clean up the job ref.
	h.toolJobRefsMu.Lock()
	delete(h.toolJobRefs, msg.JobID)
	h.toolJobRefsMu.Unlock()

	h.recordToolResult(msg.ParentJobID, msg.ToolCallID, msg.ToolName, msg.Result, msg.ImageParts, msg.Error)
}

// handleToolError processes an error message from a tools worker for a specific tool job.
func (h *ToolsWSHandler) handleToolError(toolJobID, errMsg string) {
	h.toolJobRefsMu.Lock()
	ref, ok := h.toolJobRefs[toolJobID]
	delete(h.toolJobRefs, toolJobID)
	h.toolJobRefsMu.Unlock()

	if !ok {
		return
	}
	h.recordToolResult(ref.parentJobID, ref.toolCallID, "", nil, nil, errMsg)
}

// recordToolResult stores one tool call's result and, when all results are in,
// rebuilds the message history and re-enqueues the GPU job.
func (h *ToolsWSHandler) recordToolResult(parentJobID, toolCallID, toolName string, result json.RawMessage, imageParts []protocol.ImagePart, errMsg string) {
	h.pendingContsMu.Lock()
	cont, ok := h.pendingConts[parentJobID]
	h.pendingContsMu.Unlock()

	if !ok {
		h.logger.Warn("tool result for unknown continuation",
			"parent_job_id", parentJobID,
			"tool_call_id", toolCallID,
		)
		return
	}

	cont.mu.Lock()
	defer cont.mu.Unlock()

	if _, exists := cont.results[toolCallID]; !exists {
		h.logger.Warn("tool result for unknown tool_call_id",
			"parent_job_id", parentJobID,
			"tool_call_id", toolCallID,
		)
		return
	}

	cont.results[toolCallID] = &toolCallResult{
		toolName:   toolName,
		result:     result,
		errMsg:     errMsg,
		imageParts: imageParts,
	}
	cont.remaining--

	if cont.remaining > 0 {
		return // still waiting for more results
	}

	// All tool calls are done.
	h.pendingContsMu.Lock()
	delete(h.pendingConts, parentJobID)
	h.pendingContsMu.Unlock()

	if cont.isDirect {
		// Direct tool call — the result is already set in cont.results.
		// The goroutine in DispatchDirectToolCall is polling for it; nothing else to do.
		return
	}

	go h.continueLLMRound(cont)
}

// continueLLMRound builds the next-round message history and re-enqueues
// the GPU job with tool results appended.
func (h *ToolsWSHandler) continueLLMRound(cont *pendingContinuation) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Build the updated message list:
	//   [original messages] + [assistant turn with tool_calls] + [tool result messages]
	messages := make([]protocol.ChatMessage, 0,
		len(cont.ctx.messages)+1+len(cont.results))
	messages = append(messages, cont.ctx.messages...)
	messages = append(messages, cont.assistantMsg)

	// Append tool results in the order of the assistant's tool_calls.
	// The LLM needs them matched by tool_call_id.
	for _, tc := range cont.assistantMsg.ToolCalls {
		res, ok := cont.results[tc.ID]
		if !ok || res == nil {
			// This shouldn't happen but be defensive.
			messages = append(messages, protocol.ChatMessage{
				Role:       "tool",
				Content:    protocol.ChatContentString(`{"error":"tool result missing"}`),
				ToolCallID: tc.ID,
			})
			continue
		}

		content := toolResultContent(res)
		var msgContent json.RawMessage
		if len(res.imageParts) > 0 {
			msgContent = buildMultimodalContent(content, res.imageParts)
		} else {
			msgContent = protocol.ChatContentString(content)
		}
		messages = append(messages, protocol.ChatMessage{
			Role:       "tool",
			Content:    msgContent,
			ToolCallID: tc.ID,
		})
	}

	// Build the next-round JobMessage. Same jobID → same token stream → API
	// caller continues reading without interruption.
	nextJob := &protocol.JobMessage{
		Type:            protocol.TypeJob,
		JobID:           cont.jobID,
		Messages:        messages,
		Tools:           cont.ctx.tools,
		Temperature:     cont.ctx.temperature,
		MaxTokens:       cont.ctx.maxTokens,
		ReasoningEffort: cont.ctx.reasoningEffort,
	}

	// Update the saved job context with the extended message history. Carry
	// threadID forward so subsequent continuation rounds retain sticky routing.
	h.jobContextsMu.Lock()
	h.jobContexts[cont.jobID] = &jobContext{
		messages:        messages,
		tools:           cont.ctx.tools,
		temperature:     cont.ctx.temperature,
		maxTokens:       cont.ctx.maxTokens,
		reasoningEffort: cont.ctx.reasoningEffort,
		threadID:        cont.ctx.threadID,
	}
	h.jobContextsMu.Unlock()

	// Re-enqueue the GPU job.
	if err := enqueueJob(h.jobQueue, nextJob, h.logger); err != nil {
		h.logger.Error("re-enqueuing continuation job after tool results",
			"job_id", cont.jobID,
			"error", err,
		)
		// Publish an error to the token stream so the API caller doesn't hang.
		errData, _ := json.Marshal(map[string]string{
			"error":   "continuation_failed",
			"details": fmt.Sprintf("failed to re-enqueue job after tool results: %s", err),
		})
		event := queue.TokenEvent{Type: "error", Data: errData}
		if _, pubErr := h.tokenStream.Publish(ctx, cont.jobID, event); pubErr != nil {
			h.logger.Error("publishing continuation error", "job_id", cont.jobID, "error", pubErr)
		}
		if err := h.tokenStream.SetTTL(ctx, cont.jobID, streamTTLDone); err != nil {
			h.logger.Error("setting TTL after continuation error", "job_id", cont.jobID, "error", err)
		}
		return
	}

	h.logger.Info("continuation job enqueued",
		"job_id", cont.jobID,
		"round_messages", len(messages),
		"tool_results", len(cont.results),
	)
}

// toolResultContent converts a tool result JSON value to a string suitable
// for a "tool" role message. If the result is a JSON string, it's unwrapped.
// Otherwise the raw JSON is used.
func toolResultContent(res *toolCallResult) string {
	if res.errMsg != "" {
		errJSON, _ := json.Marshal(map[string]string{"error": res.errMsg})
		return string(errJSON)
	}
	if res.result == nil {
		return ""
	}
	// If the result is already a JSON string, unwrap it.
	var s string
	if err := json.Unmarshal(res.result, &s); err == nil {
		return s
	}
	return string(res.result)
}

// ─────────────────────────────────────────────────────────────────────────────
// Worker selection
// ─────────────────────────────────────────────────────────────────────────────

// pickFromEligible selects one worker from a pre-filtered, already-sorted
// eligible slice using a deterministic hash of threadID.  When threadID is
// empty the first worker (index 0) is returned with a warning log.
// Returns nil if eligible is empty.
func (h *ToolsWSHandler) pickFromEligible(eligible []*toolsConn, toolName, threadID string) *toolsConn {
	if len(eligible) == 0 {
		return nil
	}
	// Sort by worker id so map-iteration order doesn't affect selection.
	sort.Slice(eligible, func(i, j int) bool {
		return eligible[i].id < eligible[j].id
	})
	if threadID == "" {
		h.logger.Warn("picking tools worker without thread_id — routing is non-sticky",
			"tool_name", toolName,
			"worker_id", eligible[0].id,
		)
		return eligible[0]
	}
	h64 := fnv.New64a()
	_, _ = h64.Write([]byte(threadID))
	idx := h64.Sum64() % uint64(len(eligible))
	return eligible[idx]
}

// pickToolsWorker returns the tools worker that should handle the given tool
// for the given threadID. Workers are filtered to those advertising toolName;
// among those the worker is selected deterministically by
//
//	stable_hash(threadID) % len(eligible)
//
// When threadID is empty, the first eligible worker (alphabetically by id) is
// returned with a warning log. When toolName is empty, all connected workers
// are eligible. Returns nil if no eligible worker is found.
func (h *ToolsWSHandler) pickToolsWorker(toolName, threadID string) *toolsConn {
	h.toolsWorkersMu.RLock()
	defer h.toolsWorkersMu.RUnlock()

	var eligible []*toolsConn
	for _, w := range h.toolsWorkers {
		if toolName == "" {
			eligible = append(eligible, w)
			continue
		}
		for _, t := range w.tools {
			if t == toolName {
				eligible = append(eligible, w)
				break
			}
		}
	}
	return h.pickFromEligible(eligible, toolName, threadID)
}

// pickToolsWorkerAll returns the worker that should handle a round of tool calls
// where multiple distinct tool names may be present.  It selects from workers
// that advertise ALL of the given tool names so that stateful tools (e.g.
// browser) stay on the same worker as companion tools in the same round.
//
// If no single worker covers all tools, it falls back to workers that cover at
// least one of them (preferring broader coverage via the same hash selection),
// and logs a warning about the partial coverage.  Returns nil if no worker
// advertises any of the requested tools.
func (h *ToolsWSHandler) pickToolsWorkerAll(toolNames []string, threadID string) *toolsConn {
	if len(toolNames) == 0 {
		return h.pickToolsWorker("", threadID)
	}
	if len(toolNames) == 1 {
		return h.pickToolsWorker(toolNames[0], threadID)
	}

	h.toolsWorkersMu.RLock()
	defer h.toolsWorkersMu.RUnlock()

	// Build a set of required tool names for O(1) lookup.
	required := make(map[string]struct{}, len(toolNames))
	for _, name := range toolNames {
		required[name] = struct{}{}
	}

	var allCover []*toolsConn // workers advertising every required tool
	var anyCover []*toolsConn // workers advertising at least one

	for _, w := range h.toolsWorkers {
		advertised := make(map[string]struct{}, len(w.tools))
		for _, t := range w.tools {
			advertised[t] = struct{}{}
		}
		coversAll := true
		coversAny := false
		for name := range required {
			if _, ok := advertised[name]; ok {
				coversAny = true
			} else {
				coversAll = false
			}
		}
		if coversAll {
			allCover = append(allCover, w)
		} else if coversAny {
			anyCover = append(anyCover, w)
		}
	}

	if len(allCover) > 0 {
		return h.pickFromEligible(allCover, "", threadID)
	}

	if len(anyCover) > 0 {
		h.logger.Warn("no single tools worker covers all tools in round; using best-coverage worker",
			"tools", toolNames,
		)
		return h.pickFromEligible(anyCover, "", threadID)
	}

	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Pool helpers
// ─────────────────────────────────────────────────────────────────────────────

func (h *ToolsWSHandler) toolsPoolSize() int {
	h.toolsWorkersMu.RLock()
	defer h.toolsWorkersMu.RUnlock()
	return len(h.toolsWorkers)
}

// ConnectedTools returns the deduplicated list of tool names advertised by
// all currently connected tools workers. Used by the /api/tools/v1/tools endpoint.
func (h *ToolsWSHandler) ConnectedTools() []string {
	h.toolsWorkersMu.RLock()
	defer h.toolsWorkersMu.RUnlock()

	seen := make(map[string]bool)
	var names []string
	for _, w := range h.toolsWorkers {
		for _, name := range w.tools {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	return names
}

// HasToolsWorker returns true if at least one tools worker is connected.
func (h *ToolsWSHandler) HasToolsWorker() bool {
	h.toolsWorkersMu.RLock()
	defer h.toolsWorkersMu.RUnlock()
	return len(h.toolsWorkers) > 0
}

// DispatchDirectToolCall sends a single tool call to any connected tools worker
// and waits synchronously for the result. Used by the direct tool API endpoint.
// Returns the result JSON or an error.
func (h *ToolsWSHandler) DispatchDirectToolCall(ctx context.Context, toolName string, args json.RawMessage) (json.RawMessage, error) {
	// Direct calls have no thread identity — pickToolsWorker logs a warning and
	// returns the first eligible worker (alphabetically by id).
	worker := h.pickToolsWorker(toolName, "")
	if worker == nil {
		return nil, fmt.Errorf("no tools worker connected that supports %q", toolName)
	}

	// Create a unique job ID for this direct call.
	toolJobID := newToolsWorkerID()
	callID := newToolsWorkerID() // tool_call_id

	// We need a callback when the result arrives. Use a channel.
	resultCh := make(chan *toolCallResult, 1)

	// Register a one-shot continuation listener via a sentinel pending continuation.
	// We abuse the continuation mechanism slightly: create a fake continuation with
	// one pending call, and when it resolves it sends to resultCh instead of
	// re-enqueuing a GPU job.
	//
	// This is cleaner than adding a separate callback map.
	sentinelJobID := "direct:" + toolJobID

	cont := &pendingContinuation{
		jobID:     sentinelJobID,
		remaining: 1,
		results:   map[string]*toolCallResult{callID: nil},
		isDirect:  true,
	}
	h.pendingContsMu.Lock()
	h.pendingConts[sentinelJobID] = cont
	h.pendingContsMu.Unlock()

	h.toolJobRefsMu.Lock()
	h.toolJobRefs[toolJobID] = toolJobRef{
		parentJobID: sentinelJobID,
		toolCallID:  callID,
	}
	h.toolJobRefsMu.Unlock()

	// Override continueLLMRound to send to the channel instead.
	// We do this by registering a goroutine that watches the cont.
	go func() {
		for {
			select {
			case <-ctx.Done():
				resultCh <- &toolCallResult{errMsg: ctx.Err().Error()}
				return
			case <-time.After(100 * time.Millisecond):
				cont.mu.Lock()
				res := cont.results[callID]
				cont.mu.Unlock()
				if res != nil {
					h.pendingContsMu.Lock()
					delete(h.pendingConts, sentinelJobID)
					h.pendingContsMu.Unlock()
					resultCh <- res
					return
				}
			}
		}
	}()

	toolJob := protocol.ToolJobMessage{
		Type:        protocol.TypeToolJob,
		JobID:       toolJobID,
		ParentJobID: sentinelJobID,
		ToolName:    toolName,
		ToolCallID:  callID,
		Arguments:   args,
		Credentials: h.config.ToolCredentials[toolName],
	}
	if err := worker.sendMessage(toolJob); err != nil {
		h.pendingContsMu.Lock()
		delete(h.pendingConts, sentinelJobID)
		h.pendingContsMu.Unlock()
		h.toolJobRefsMu.Lock()
		delete(h.toolJobRefs, toolJobID)
		h.toolJobRefsMu.Unlock()
		return nil, fmt.Errorf("dispatching tool job: %w", err)
	}

	select {
	case res := <-resultCh:
		if res.errMsg != "" {
			return nil, fmt.Errorf("tool execution error: %s", res.errMsg)
		}
		return res.result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("tool call timed out: %w", ctx.Err())
	}
}

// buildMultimodalContent marshals text + image parts into an OpenAI-compatible
// multimodal content array:
//
//	[{"type":"text","text":"..."},{"type":"image_url","image_url":{"url":"data:<mime>;base64,..."}},...]
//
// This format is accepted by vision-capable LLM APIs for tool result messages.
func buildMultimodalContent(text string, imgs []protocol.ImagePart) json.RawMessage {
	type textPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type imageURL struct {
		URL string `json:"url"`
	}
	type imagePart struct {
		Type     string   `json:"type"`
		ImageURL imageURL `json:"image_url"`
	}

	parts := make([]any, 0, 1+len(imgs))
	parts = append(parts, textPart{Type: "text", Text: text})
	for _, img := range imgs {
		url := fmt.Sprintf("data:%s;base64,%s",
			img.MIMEType,
			base64.StdEncoding.EncodeToString(img.Data),
		)
		parts = append(parts, imagePart{
			Type:     "image_url",
			ImageURL: imageURL{URL: url},
		})
	}

	b, err := json.Marshal(parts)
	if err != nil {
		// Fallback: return plain text only so the LLM round is not lost.
		return protocol.ChatContentString(text)
	}
	return json.RawMessage(b)
}

func newToolsWorkerID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
