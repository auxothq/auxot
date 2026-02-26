package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/auxothq/auxot/pkg/auth"
	"github.com/auxothq/auxot/pkg/protocol"
	"github.com/auxothq/auxot/pkg/queue"
)

// MCPHandler implements an MCP (Model Context Protocol) server over HTTP+SSE transport.
//
// The MCP specification uses JSON-RPC 2.0. The HTTP+SSE transport works as follows:
//   - GET  /mcp/sse     — Client connects; receives a "endpoint" event with a session ID,
//                         then subsequent JSON-RPC response/notification events.
//   - POST /mcp/message — Client sends JSON-RPC requests; responses arrive via SSE.
//
// This allows external MCP clients (Claude Desktop, Cursor, etc.) to use the
// tools provided by connected auxot-tools workers without any additional setup.
//
// When config.MCPExposeLLM is true, the MCP server also exposes a "generate_text"
// tool that lets MCP clients invoke the router's LLM directly.
//
// Reference: https://spec.modelcontextprotocol.io/specification/basic/transports/#http-with-sse
type MCPHandler struct {
	verifier    *auth.Verifier
	tools       *ToolsWSHandler
	jobQueue    *queue.JobQueue    // nil if LLM tool is not enabled or unavailable
	tokenStream *queue.TokenStream // nil if LLM tool is not enabled or unavailable
	logger      *slog.Logger
	config      *Config

	// Active SSE sessions: sessionID → *mcpSession
	sessions   map[string]*mcpSession
	sessionsMu sync.RWMutex
}

// mcpSession represents one MCP client's SSE connection.
type mcpSession struct {
	id      string
	eventCh chan mcpEvent
	cancel  context.CancelFunc
}

type mcpEvent struct {
	name string // SSE event name
	data []byte // SSE data field
}

// NewMCPHandler creates an MCPHandler.
//
// jobQueue and tokenStream are required when config.MCPExposeLLM is true —
// they are used to dispatch LLM jobs for the generate_text tool. Both may be
// nil when MCPExposeLLM is false (e.g., in unit tests).
func NewMCPHandler(
	verifier *auth.Verifier,
	tools *ToolsWSHandler,
	jobQueue *queue.JobQueue,
	tokenStream *queue.TokenStream,
	config *Config,
	logger *slog.Logger,
) *MCPHandler {
	return &MCPHandler{
		verifier:    verifier,
		tools:       tools,
		jobQueue:    jobQueue,
		tokenStream: tokenStream,
		config:      config,
		logger:      logger,
		sessions:    make(map[string]*mcpSession),
	}
}

// requireAuth checks the Bearer token against the configured API key.
// If no API key is configured (APIKeyHash is empty), access is always allowed —
// this supports local/development setups that skip authentication.
// Returns true if authorized, false if the request was rejected (response already written).
func (h *MCPHandler) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if h.config.APIKeyHash == "" {
		return true // No key configured — open access (dev/local mode)
	}
	apiKey := extractBearerToken(r)
	if apiKey == "" {
		writeErrorJSON(w, http.StatusUnauthorized, "authentication_error", "missing Bearer token")
		return false
	}
	valid, err := h.verifier.VerifyAPIKey(apiKey)
	if err != nil || !valid {
		writeErrorJSON(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
		return false
	}
	return true
}

// ServeHTTP routes MCP requests.
func (h *MCPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	method := r.Method

	switch {
	case method == http.MethodGet && (path == "/mcp/sse" || path == "/mcp/sse/"):
		h.handleSSE(w, r)
	case method == http.MethodPost && (path == "/mcp/message" || path == "/mcp/message/"):
		h.handleMessage(w, r)
	default:
		writeErrorJSON(w, http.StatusNotFound, "not_found", "MCP endpoint not found")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// JSON-RPC 2.0 types
// ─────────────────────────────────────────────────────────────────────────────

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // number, string, or null
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
)

func rpcError(id json.RawMessage, code int, msg string) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: msg},
	}
}

func rpcResult(id json.RawMessage, result any) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /mcp/sse — SSE stream endpoint
// ─────────────────────────────────────────────────────────────────────────────

func (h *MCPHandler) handleSSE(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErrorJSON(w, http.StatusInternalServerError, "server_error", "SSE not supported")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	sessionID := newToolsWorkerID()

	sess := &mcpSession{
		id:      sessionID,
		eventCh: make(chan mcpEvent, 64),
		cancel:  cancel,
	}

	h.sessionsMu.Lock()
	h.sessions[sessionID] = sess
	h.sessionsMu.Unlock()

	defer func() {
		cancel()
		h.sessionsMu.Lock()
		delete(h.sessions, sessionID)
		h.sessionsMu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Send the endpoint event — tells the client where to POST messages.
	// The client appends ?sessionId= to its POST requests.
	endpointURL := fmt.Sprintf("/mcp/message?sessionId=%s", sessionID)
	fmt.Fprintf(w, "event: endpoint\ndata: %q\n\n", endpointURL)
	flusher.Flush()

	h.logger.Info("MCP client connected", "session_id", sessionID, "remote", r.RemoteAddr)

	// Keep-alive ticker — SSE connections may time out without activity.
	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case <-ctx.Done():
			h.logger.Info("MCP client disconnected", "session_id", sessionID)
			return
		case <-keepAlive.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		case evt, ok := <-sess.eventCh:
			if !ok {
				return
			}
			if evt.name != "" {
				fmt.Fprintf(w, "event: %s\n", evt.name)
			}
			fmt.Fprintf(w, "data: %s\n\n", evt.data)
			flusher.Flush()
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /mcp/message — JSON-RPC request handler
// ─────────────────────────────────────────────────────────────────────────────

func (h *MCPHandler) handleMessage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")

	h.sessionsMu.RLock()
	sess, ok := h.sessions[sessionID]
	h.sessionsMu.RUnlock()

	if !ok {
		writeErrorJSON(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("MCP session %q not found — open GET /mcp/sse first", sessionID))
		return
	}

	var req jsonRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.sendSSEResponse(sess, rpcError(nil, rpcParseError, "invalid JSON: "+err.Error()))
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if req.JSONRPC != "2.0" {
		h.sendSSEResponse(sess, rpcError(req.ID, rpcInvalidRequest, "jsonrpc must be \"2.0\""))
		w.WriteHeader(http.StatusAccepted)
		return
	}

	h.logger.Debug("MCP request",
		"session_id", sessionID,
		"method", req.Method,
	)

	// Dispatch and send the response asynchronously via SSE.
	// Use a fresh background context so the dispatch goroutine is not bound to
	// the POST request's lifecycle — r.Context() is canceled when the handler
	// returns (after writing 202).
	go h.dispatch(context.Background(), sess, &req)

	// HTTP 202 Accepted — actual response comes via SSE.
	w.WriteHeader(http.StatusAccepted)
}

// dispatch processes a JSON-RPC request and sends the response to the SSE stream.
func (h *MCPHandler) dispatch(ctx context.Context, sess *mcpSession, req *jsonRPCRequest) {
	var resp *jsonRPCResponse

	switch req.Method {
	case "initialize":
		resp = h.handleInitialize(req)
	case "notifications/initialized":
		// Client acknowledges — no response needed for notifications (no id).
		return
	case "tools/list":
		resp = h.handleToolsList(req)
	case "tools/call":
		resp = h.handleToolsCall(ctx, req)
	case "ping":
		resp = rpcResult(req.ID, map[string]any{})
	default:
		resp = rpcError(req.ID, rpcMethodNotFound,
			fmt.Sprintf("method %q not supported", req.Method))
	}

	h.sendSSEResponse(sess, resp)
}

// sendSSEResponse marshals a JSON-RPC response and sends it to the SSE stream.
func (h *MCPHandler) sendSSEResponse(sess *mcpSession, resp *jsonRPCResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		h.logger.Error("marshaling MCP response", "error", err)
		return
	}
	select {
	case sess.eventCh <- mcpEvent{name: "message", data: data}:
	default:
		h.logger.Warn("MCP session event channel full — dropping response",
			"session_id", sess.id)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// MCP method implementations
// ─────────────────────────────────────────────────────────────────────────────

// handleInitialize responds to the MCP initialize handshake.
func (h *MCPHandler) handleInitialize(req *jsonRPCRequest) *jsonRPCResponse {
	result := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "auxot-router",
			"version": "0.1.0",
		},
	}
	return rpcResult(req.ID, result)
}

// generateTextSchema is the JSON Schema for the generate_text MCP tool's input.
var generateTextSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"prompt": {
			"type": "string",
			"description": "The user prompt to send to the LLM"
		},
		"system": {
			"type": "string",
			"description": "Optional system prompt. Defaults to a neutral assistant prompt."
		},
		"max_tokens": {
			"type": "integer",
			"description": "Maximum tokens to generate. Default: 1024."
		},
		"temperature": {
			"type": "number",
			"description": "Sampling temperature 0.0-2.0. Default: 0.7."
		}
	},
	"required": ["prompt"]
}`)

// handleToolsList returns the list of tools available from connected tools workers.
// When config.MCPExposeLLM is true, "generate_text" is prepended to the list.
func (h *MCPHandler) handleToolsList(req *jsonRPCRequest) *jsonRPCResponse {
	defs := h.tools.AllowedToolDefs()

	type mcpTool struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}

	var tools []mcpTool

	// Expose the LLM as a tool when the feature flag is set.
	if h.config.MCPExposeLLM {
		tools = append(tools, mcpTool{
			Name: "generate_text",
			Description: "Generate text using the router's LLM. " +
				"Use for one-shot inference, summarization, classification, " +
				"or any task that benefits from a separate LLM call.",
			InputSchema: generateTextSchema,
		})
	}

	for _, def := range defs {
		schema := def.Function.Parameters
		if schema == nil {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, mcpTool{
			Name:        def.Function.Name,
			Description: def.Function.Description,
			InputSchema: schema,
		})
	}

	return rpcResult(req.ID, map[string]any{"tools": tools})
}

// handleToolsCall dispatches a tool/call to the appropriate executor and returns the result.
//
// "generate_text" is handled locally via the LLM job queue when MCPExposeLLM is true.
// All other tool names are forwarded to a connected tools worker.
func (h *MCPHandler) handleToolsCall(ctx context.Context, req *jsonRPCRequest) *jsonRPCResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcError(req.ID, rpcInvalidParams, "invalid params: "+err.Error())
	}
	if params.Name == "" {
		return rpcError(req.ID, rpcInvalidParams, "tool name is required")
	}
	if params.Arguments == nil {
		params.Arguments = json.RawMessage("{}")
	}

	// generate_text is served by the LLM job queue, not a tools worker.
	if params.Name == "generate_text" {
		if !h.config.MCPExposeLLM {
			return rpcError(req.ID, rpcMethodNotFound,
				"generate_text is not enabled — set AUXOT_MCP_EXPOSE_LLM=true to enable")
		}
		callCtx, cancel := context.WithTimeout(ctx, h.config.JobTimeout)
		defer cancel()
		return h.handleGenerateText(callCtx, req.ID, params.Arguments)
	}

	if !h.tools.HasToolsWorker() {
		return rpcError(req.ID, rpcInternalError,
			"no tools worker connected — start auxot-tools and connect it to this router")
	}

	// Apply job timeout to tool calls.
	callCtx, cancel := context.WithTimeout(ctx, h.config.JobTimeout)
	defer cancel()

	result, err := h.tools.DispatchDirectToolCall(callCtx, params.Name, params.Arguments)
	if err != nil {
		h.logger.Error("MCP tool call failed",
			"tool", params.Name,
			"error", err,
		)
		return rpcError(req.ID, rpcInternalError, "tool execution failed: "+err.Error())
	}

	// MCP tools/call result format:
	// { content: [{ type: "text", text: "..." }], isError: false }
	var textContent string
	var s string
	if err := json.Unmarshal(result, &s); err == nil {
		textContent = s // result was a JSON string — use as-is
	} else {
		textContent = string(result) // result is a JSON object/array — serialize as text
	}

	mcpResult := map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": textContent},
		},
		"isError": false,
	}
	return rpcResult(req.ID, mcpResult)
}

// handleGenerateText executes the "generate_text" MCP tool by enqueuing an LLM job
// and collecting the streamed response into a single string.
func (h *MCPHandler) handleGenerateText(ctx context.Context, id json.RawMessage, args json.RawMessage) *jsonRPCResponse {
	if h.jobQueue == nil || h.tokenStream == nil {
		return rpcError(id, rpcInternalError,
			"LLM dispatch not available — router was started without a job queue")
	}

	var input struct {
		Prompt      string   `json:"prompt"`
		System      string   `json:"system"`
		MaxTokens   *int     `json:"max_tokens"`
		Temperature *float64 `json:"temperature"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return rpcError(id, rpcInvalidParams, "invalid arguments: "+err.Error())
	}
	if input.Prompt == "" {
		return rpcError(id, rpcInvalidParams, "prompt is required")
	}

	// Apply defaults.
	if input.System == "" {
		input.System = "You are a helpful assistant."
	}
	if input.MaxTokens == nil {
		n := 1024
		input.MaxTokens = &n
	}
	if input.Temperature == nil {
		t := 0.7
		input.Temperature = &t
	}

	messages := []protocol.ChatMessage{
		{Role: "system", Content: protocol.ChatContentString(input.System)},
		{Role: "user", Content: protocol.ChatContentString(input.Prompt)},
	}

	text, finishReason, err := h.dispatchLLMCall(ctx, messages, input.MaxTokens, input.Temperature)
	if err != nil {
		h.logger.Error("generate_text LLM call failed", "error", err)
		return rpcError(id, rpcInternalError, "LLM call failed: "+err.Error())
	}

	// Return the result as a JSON object so callers get structured output.
	resultJSON, _ := json.Marshal(map[string]any{
		"text":          text,
		"finish_reason": finishReason,
	})

	mcpResult := map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(resultJSON)},
		},
		"isError": false,
	}
	return rpcResult(id, mcpResult)
}

// dispatchLLMCall enqueues a chat completion job and collects all streamed tokens
// into a single string, blocking until the job completes or the context expires.
func (h *MCPHandler) dispatchLLMCall(
	ctx context.Context,
	messages []protocol.ChatMessage,
	maxTokens *int,
	temperature *float64,
) (text string, finishReason string, err error) {
	jobID := newToolsWorkerID()

	jobMsg := &protocol.JobMessage{
		Type:        protocol.TypeJob,
		JobID:       jobID,
		Messages:    messages,
		Temperature: temperature,
		MaxTokens:   maxTokens,
	}

	if err := enqueueJob(h.jobQueue, jobMsg, h.logger); err != nil {
		return "", "", fmt.Errorf("enqueuing LLM job: %w", err)
	}

	h.logger.Info("generate_text job enqueued", "job_id", jobID)

	reader, cleanup := h.tokenStream.NewBlockingReader()
	defer cleanup()

	var content strings.Builder
	lastID := "0-0"

	for {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}

		entries, err := reader.Read(ctx, jobID, lastID, 100, 1*time.Second)
		if err != nil {
			return "", "", fmt.Errorf("reading token stream: %w", err)
		}

		for _, entry := range entries {
			lastID = entry.ID

			switch entry.Event.Type {
			case "token":
				var token string
				if jsonErr := json.Unmarshal(entry.Event.Data, &token); jsonErr == nil {
					content.WriteString(token)
				}

			case "done":
				var complete protocol.CompleteMessage
				json.Unmarshal(entry.Event.Data, &complete) //nolint:errcheck — zero value is safe
				if complete.FullResponse != "" {
					// Worker sent the full assembled response; use it instead of
					// accumulated streaming tokens (avoids off-by-one token gaps).
					content.Reset()
					content.WriteString(complete.FullResponse)
				}
				reason := "stop"
				if len(complete.ToolCalls) > 0 {
					reason = "tool_calls"
				}
				return content.String(), reason, nil

			case "error":
				var errData map[string]string
				json.Unmarshal(entry.Event.Data, &errData) //nolint:errcheck
				msg := errData["error"]
				if msg == "" {
					msg = "internal error"
				}
				return "", "", fmt.Errorf("LLM error: %s", msg)
			}
		}
	}
}
