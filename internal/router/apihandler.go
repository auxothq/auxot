package router

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/auxothq/auxot/pkg/auth"
	"github.com/auxothq/auxot/pkg/gpu"
	"github.com/auxothq/auxot/pkg/openai"
	"github.com/auxothq/auxot/pkg/protocol"
	"github.com/auxothq/auxot/pkg/queue"
)

// APIHandler handles OpenAI-compatible API requests under /api/openai/.
//
// All endpoints require Bearer token authentication via AUXOT_API_KEY.
// The handler translates OpenAI-format requests into internal protocol
// messages, enqueues them to Redis, and translates the results back.
type APIHandler struct {
	verifier    *auth.Verifier
	pool        *gpu.Pool // Used by /health to report capacity
	jobQueue    *queue.JobQueue
	tokenStream *queue.TokenStream
	tools       *ToolsWSHandler // nil if tools not configured
	config      *Config
	logger      *slog.Logger
}

// NewAPIHandler creates an API handler.
func NewAPIHandler(
	verifier *auth.Verifier,
	pool *gpu.Pool,
	jobQueue *queue.JobQueue,
	tokenStream *queue.TokenStream,
	tools *ToolsWSHandler,
	config *Config,
	logger *slog.Logger,
) *APIHandler {
	return &APIHandler{
		verifier:    verifier,
		pool:        pool,
		jobQueue:    jobQueue,
		tokenStream: tokenStream,
		tools:       tools,
		config:      config,
		logger:      logger,
	}
}

// ServeHTTP routes incoming requests to the appropriate handler.
//
// OpenAI-compatible routes live under /api/openai/:
//
//	POST /api/openai/chat/completions   - Chat completion (streaming + non-streaming)
//	GET  /api/openai/chat/completions   - List stored completions (stub: empty list)
//	GET  /api/openai/chat/completions/{id}  - Get stored completion (404: we don't store)
//	DELETE /api/openai/chat/completions/{id} - Delete completion (400: not supported)
//	GET  /api/openai/chat/completions/{id}/messages - Get messages (404: we don't store)
//	POST /api/openai/completions        - Legacy text completion
//	POST /api/openai/embeddings         - Embedding generation (501: needs worker support)
//	GET  /api/openai/models             - List available models
//	GET  /api/openai/models/{id}        - Get model details
//
// Health check lives at /health (no auth required).
func (h *APIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	method := r.Method

	switch {
	// --- Health (no auth) ---
	case method == http.MethodGet && path == "/health":
		h.handleHealth(w, r)

	// --- Chat Completions ---
	case method == http.MethodPost && path == "/api/openai/chat/completions":
		h.handleChatCompletion(w, r)

	case method == http.MethodGet && path == "/api/openai/chat/completions":
		h.handleListChatCompletions(w, r)

	case strings.HasPrefix(path, "/api/openai/chat/completions/"):
		rest := strings.TrimPrefix(path, "/api/openai/chat/completions/")
		switch {
		case method == http.MethodGet && strings.HasSuffix(rest, "/messages"):
			h.handleGetCompletionMessages(w, r)
		case method == http.MethodGet && !strings.Contains(rest, "/"):
			h.handleGetChatCompletion(w, r)
		case method == http.MethodDelete && !strings.Contains(rest, "/"):
			h.handleDeleteChatCompletion(w, r)
		default:
			writeErrorJSON(w, http.StatusNotFound, "not_found", "endpoint not found")
		}

	// --- Legacy Completions ---
	case method == http.MethodPost && path == "/api/openai/completions":
		h.handleLegacyCompletion(w, r)

	// --- Embeddings ---
	case method == http.MethodPost && path == "/api/openai/embeddings":
		h.handleEmbeddings(w, r)

	// --- Models ---
	case method == http.MethodGet && path == "/api/openai/models":
		h.handleListModels(w, r)

	case method == http.MethodGet && strings.HasPrefix(path, "/api/openai/models/"):
		h.handleGetModel(w, r)

	// --- Catch-all for /api/openai/* ---
	case strings.HasPrefix(path, "/api/openai/"):
		h.logger.Info("501 not implemented",
			"method", method,
			"path", path,
			"remote", r.RemoteAddr,
		)
		writeErrorJSON(w, http.StatusNotImplemented, "not_implemented", "endpoint not implemented")

	default:
		h.logger.Info("404 not found",
			"method", method,
			"path", path,
			"remote", r.RemoteAddr,
		)
		writeErrorJSON(w, http.StatusNotFound, "not_found", "endpoint not found")
	}
}

// --- Auth helper ---

// requireAuth checks the Bearer token and returns true if authorized.
// Writes an error response and returns false otherwise.
func (h *APIHandler) requireAuth(w http.ResponseWriter, r *http.Request) bool {
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

// --- Health ---

// handleHealth returns router status for load balancers and probes.
func (h *APIHandler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	snap := h.pool.Snapshot()
	totalSlots := 0
	activeJobs := 0
	for _, ws := range snap {
		totalSlots += ws.MaxSlots
		activeJobs += len(ws.ActiveJobIDs)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":      "ok",
		"workers":     len(snap),
		"total_slots": totalSlots,
		"active_jobs": activeJobs,
		"available":   totalSlots - activeJobs,
	})
}

// --- POST /api/openai/chat/completions ---

// handleChatCompletion processes an OpenAI-compatible chat completion request.
func (h *APIHandler) handleChatCompletion(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}

	var req openai.ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}

	if len(req.Messages) == 0 {
		writeErrorJSON(w, http.StatusBadRequest, "invalid_request_error", "messages array is required")
		return
	}

	// Build job
	jobID := openai.NewCompletionID()

	// Convert openai.Message → protocol.ChatMessage
	protoMessages := make([]protocol.ChatMessage, len(req.Messages))
	for i, m := range req.Messages {
		protoMessages[i] = protocol.ChatMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		if len(m.ToolCalls) > 0 {
			protoMessages[i].ToolCalls = make([]protocol.ToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				protoMessages[i].ToolCalls[j] = protocol.ToolCall{
					ID:   tc.ID,
					Type: tc.Type,
					Function: protocol.ToolFunction{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				}
			}
		}
	}

	// Convert openai.Tool → protocol.Tool (preserve full definition including parameters schema)
	var protoTools []protocol.Tool
	for _, t := range req.Tools {
		protoTools = append(protoTools, protocol.Tool{
			Type: t.Type,
			Function: protocol.ToolDefinition{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			},
		})
	}

	jobMsg := &protocol.JobMessage{
		Type:            protocol.TypeJob,
		JobID:           jobID,
		Messages:        protoMessages,
		Tools:           protoTools,
		Temperature:     req.Temperature,
		MaxTokens:       req.MaxTokens,
		ReasoningEffort: req.ReasoningEffort,
	}

	if err := enqueueJob(h.jobQueue, jobMsg, h.logger); err != nil {
		writeErrorJSON(w, http.StatusServiceUnavailable, "server_error", err.Error())
		return
	}

	h.logger.Info("job accepted",
		"job_id", jobID,
		"model", req.Model,
		"messages", len(req.Messages),
		"stream", req.Stream,
	)

	if req.Stream {
		h.streamChatResponse(w, r, jobID, req.Model)
	} else {
		h.blockingChatResponse(w, r, jobID, req.Model)
	}
}

// --- POST /api/openai/completions (legacy) ---

// handleLegacyCompletion translates a legacy /completions request into a chat
// completion. The prompt becomes a single user message with no system prompt.
func (h *APIHandler) handleLegacyCompletion(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}

	var req openai.CompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}

	prompt := req.PromptString()
	if prompt == "" {
		writeErrorJSON(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}

	// Translate prompt → single user message (no system prompt)
	jobID := openai.NewCompletionID()
	jobMsg := &protocol.JobMessage{
		Type:  protocol.TypeJob,
		JobID: jobID,
		Messages: []protocol.ChatMessage{
			{Role: "user", Content: prompt},
		},
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}

	if err := enqueueJob(h.jobQueue, jobMsg, h.logger); err != nil {
		writeErrorJSON(w, http.StatusServiceUnavailable, "server_error", err.Error())
		return
	}

	h.logger.Info("legacy completion accepted",
		"job_id", jobID,
		"model", req.Model,
		"prompt_len", len(prompt),
		"stream", req.Stream,
	)

	if req.Stream {
		h.streamLegacyResponse(w, r, jobID, req.Model)
	} else {
		h.blockingLegacyResponse(w, r, jobID, req.Model)
	}
}

// --- GET /api/openai/chat/completions (list stub) ---

func (h *APIHandler) handleListChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(openai.EmptyListResponse())
}

// --- GET /api/openai/chat/completions/{id} ---

func (h *APIHandler) handleGetChatCompletion(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	writeErrorJSON(w, http.StatusNotFound, "not_found_error",
		"No chat completion found. Auxot does not store completions.")
}

// --- DELETE /api/openai/chat/completions/{id} ---

func (h *APIHandler) handleDeleteChatCompletion(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	writeErrorJSON(w, http.StatusBadRequest, "invalid_request_error",
		"Deleting completions is not supported. Auxot does not store completions.")
}

// --- GET /api/openai/chat/completions/{id}/messages ---

func (h *APIHandler) handleGetCompletionMessages(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	writeErrorJSON(w, http.StatusNotFound, "not_found_error",
		"No chat completion found. Auxot does not store completions.")
}

// --- POST /api/openai/embeddings ---

func (h *APIHandler) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	writeErrorJSON(w, http.StatusNotImplemented, "not_implemented",
		"Embedding support is not yet available. "+
			"This endpoint requires a worker with embedding capability, which is planned for a future release.")
}

// --- GET /api/openai/models ---

func (h *APIHandler) handleListModels(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}

	model := openai.ModelObject{
		ID:      h.config.ModelName,
		Object:  "model",
		Created: time.Now().Unix(),
		OwnedBy: "auxot",
	}

	resp := openai.ModelListResponse{
		Object: "list",
		Data:   []openai.ModelObject{model},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// --- GET /api/openai/models/{id} ---

func (h *APIHandler) handleGetModel(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}

	// Extract model ID from path: /api/openai/models/{id}
	modelID := strings.TrimPrefix(r.URL.Path, "/api/openai/models/")
	if modelID == "" {
		writeErrorJSON(w, http.StatusNotFound, "not_found_error", "model ID required")
		return
	}

	// We only serve the one model configured in the router
	if modelID != h.config.ModelName {
		writeErrorJSON(w, http.StatusNotFound, "not_found_error",
			fmt.Sprintf("The model %q does not exist", modelID))
		return
	}

	model := openai.ModelObject{
		ID:      h.config.ModelName,
		Object:  "model",
		Created: time.Now().Unix(),
		OwnedBy: "auxot",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(model)
}

// --- Streaming helpers ---

// streamChatResponse streams SSE chunks to the caller as tokens arrive.
func (h *APIHandler) streamChatResponse(w http.ResponseWriter, r *http.Request, jobID, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErrorJSON(w, http.StatusInternalServerError, "server_error", "streaming not supported")
		return
	}

	// Dedicated Redis connection for this request's blocking token reads.
	reader, cleanup := h.tokenStream.NewBlockingReader()
	defer cleanup()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	completionID := jobID

	// Send initial role chunk
	roleChunk := openai.NewStreamingRoleChunk(completionID, model)
	if data, err := openai.FormatSSE(roleChunk); err == nil {
		w.Write(data)
		flusher.Flush()
	}

	ctx := r.Context()
	lastID := "0-0"
	deadline := time.Now().Add(h.config.JobTimeout)

	for {
		if ctx.Err() != nil {
			h.logger.Info("client disconnected during stream", "job_id", jobID)
			return
		}

		if time.Now().After(deadline) {
			h.logger.Warn("streaming timeout", "job_id", jobID)
			errChunk := openai.NewStreamingDoneChunk(completionID, model, "stop", nil)
			if data, err := openai.FormatSSE(errChunk); err == nil {
				w.Write(data)
			}
			w.Write(openai.FormatSSEDone())
			flusher.Flush()
			return
		}

		entries, err := reader.Read(ctx, jobID, lastID, 100, 1*time.Second)
		if err != nil {
			h.logger.Error("reading token stream", "job_id", jobID, "error", err)
			return
		}

		for _, entry := range entries {
			lastID = entry.ID

			switch entry.Event.Type {
			case "token":
				var token string
				if err := json.Unmarshal(entry.Event.Data, &token); err != nil {
					h.logger.Warn("invalid token data", "job_id", jobID, "error", err)
					continue
				}
				chunk := openai.NewStreamingChunk(completionID, model, token)
				if data, err := openai.FormatSSE(chunk); err == nil {
					w.Write(data)
					flusher.Flush()
				}

			case "reasoning_token":
				var token string
				if err := json.Unmarshal(entry.Event.Data, &token); err != nil {
					h.logger.Warn("invalid reasoning token data", "job_id", jobID, "error", err)
					continue
				}
				chunk := openai.NewStreamingReasoningChunk(completionID, model, token)
				if data, err := openai.FormatSSE(chunk); err == nil {
					w.Write(data)
					flusher.Flush()
				}

		case "done":
			var complete protocol.CompleteMessage
			json.Unmarshal(entry.Event.Data, &complete)

			var usage *openai.Usage
			if complete.InputTokens > 0 || complete.OutputTokens > 0 || complete.CacheTokens > 0 {
				usage = &openai.Usage{
					CacheTokens:      complete.CacheTokens,
					PromptTokens:     complete.InputTokens,
					CompletionTokens: complete.OutputTokens,
					TotalTokens:      complete.InputTokens + complete.OutputTokens,
				}
			}

			finishReason := "stop"
			if len(complete.ToolCalls) > 0 {
				finishReason = "tool_calls"
			}

			doneChunk := openai.NewStreamingDoneChunk(completionID, model, finishReason, usage)
			if data, err := openai.FormatSSE(doneChunk); err == nil {
				w.Write(data)
			}
			w.Write(openai.FormatSSEDone())
			flusher.Flush()
			return

			case "error":
				var errData map[string]string
				json.Unmarshal(entry.Event.Data, &errData)
				errMsg := errData["error"]
				if errMsg == "" {
					errMsg = "internal error"
				}
				errJSON, _ := json.Marshal(openai.NewErrorResponse(errMsg, "server_error", ""))
				fmt.Fprintf(w, "data: %s\n\n", errJSON)
				w.Write(openai.FormatSSEDone())
				flusher.Flush()
				return
			}
		}
	}
}

// blockingChatResponse waits for the full completion and returns it as JSON.
func (h *APIHandler) blockingChatResponse(w http.ResponseWriter, r *http.Request, jobID, model string) {
	reader, cleanup := h.tokenStream.NewBlockingReader()
	defer cleanup()

	ctx := r.Context()
	lastID := "0-0"
	deadline := time.Now().Add(h.config.JobTimeout)

	var fullContent strings.Builder
	var reasoningContent strings.Builder
	var toolCalls []openai.ToolCall
	var inputTokens, outputTokens int

	for {
		if ctx.Err() != nil {
			writeErrorJSON(w, http.StatusGatewayTimeout, "server_error", "client disconnected")
			return
		}

		if time.Now().After(deadline) {
			writeErrorJSON(w, http.StatusGatewayTimeout, "server_error", "job timed out")
			return
		}

		entries, err := reader.Read(ctx, jobID, lastID, 100, 1*time.Second)
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, "server_error",
				"failed to read response: "+err.Error())
			return
		}

		for _, entry := range entries {
			lastID = entry.ID

			switch entry.Event.Type {
			case "token":
				var token string
				if err := json.Unmarshal(entry.Event.Data, &token); err == nil {
					fullContent.WriteString(token)
				}

			case "reasoning_token":
				var token string
				if err := json.Unmarshal(entry.Event.Data, &token); err == nil {
					reasoningContent.WriteString(token)
				}

		case "done":
			var complete protocol.CompleteMessage
			if err := json.Unmarshal(entry.Event.Data, &complete); err == nil {
				inputTokens = complete.InputTokens
				outputTokens = complete.OutputTokens

				if complete.FullResponse != "" {
					fullContent.Reset()
					fullContent.WriteString(complete.FullResponse)
				}
				if complete.ReasoningContent != "" {
					reasoningContent.Reset()
					reasoningContent.WriteString(complete.ReasoningContent)
				}

				for _, tc := range complete.ToolCalls {
					toolCalls = append(toolCalls, openai.ToolCall{
						ID:   tc.ID,
						Type: tc.Type,
						Function: openai.ToolCallFunction{
							Name:      tc.Function.Name,
							Arguments: tc.Function.Arguments,
						},
					})
				}
			}

			usage := &openai.Usage{
				CacheTokens:      complete.CacheTokens,
				PromptTokens:     inputTokens,
				CompletionTokens: outputTokens,
				TotalTokens:      inputTokens + outputTokens,
			}

			var resp *openai.ChatCompletionResponse
			if len(toolCalls) > 0 {
				resp = openai.NewToolCallResponse(model, toolCalls, usage)
			} else if reasoningContent.Len() > 0 {
				resp = openai.NewNonStreamingResponseWithReasoning(model, fullContent.String(), reasoningContent.String(), "stop", usage)
			} else {
				resp = openai.NewNonStreamingResponse(model, fullContent.String(), "stop", usage)
			}

				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
				return

			case "error":
				var errData map[string]string
				json.Unmarshal(entry.Event.Data, &errData)
				errMsg := errData["error"]
				if errMsg == "" {
					errMsg = "internal error"
				}
				writeErrorJSON(w, http.StatusInternalServerError, "server_error", errMsg)
				return
			}
		}
	}
}

// --- Legacy completion response helpers ---

// streamLegacyResponse streams legacy completion SSE chunks.
// Legacy format uses the same structure but object is "text_completion".
func (h *APIHandler) streamLegacyResponse(w http.ResponseWriter, r *http.Request, jobID, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErrorJSON(w, http.StatusInternalServerError, "server_error", "streaming not supported")
		return
	}

	reader, cleanup := h.tokenStream.NewBlockingReader()
	defer cleanup()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	lastID := "0-0"
	deadline := time.Now().Add(h.config.JobTimeout)

	completionID := openai.NewLegacyCompletionID()

	for {
		if ctx.Err() != nil {
			return
		}
		if time.Now().After(deadline) {
			return
		}

		entries, err := reader.Read(ctx, jobID, lastID, 100, 1*time.Second)
		if err != nil {
			return
		}

		for _, entry := range entries {
			lastID = entry.ID

			switch entry.Event.Type {
			case "token":
				var token string
				if err := json.Unmarshal(entry.Event.Data, &token); err != nil {
					continue
				}
				// Legacy streaming uses the same envelope as non-streaming
				// but with partial text in each chunk
				chunk := openai.CompletionResponse{
					ID:      completionID,
					Object:  "text_completion",
					Created: time.Now().Unix(),
					Model:   model,
					Choices: []openai.CompletionChoice{
						{Text: token, Index: 0},
					},
				}
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()

			case "done":
				// Final chunk
				chunk := openai.CompletionResponse{
					ID:      completionID,
					Object:  "text_completion",
					Created: time.Now().Unix(),
					Model:   model,
					Choices: []openai.CompletionChoice{
						{Text: "", Index: 0, FinishReason: "stop"},
					},
				}
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				w.Write(openai.FormatSSEDone())
				flusher.Flush()
				return

			case "error":
				w.Write(openai.FormatSSEDone())
				flusher.Flush()
				return
			}
		}
	}
}

// blockingLegacyResponse waits for the full completion and returns it.
func (h *APIHandler) blockingLegacyResponse(w http.ResponseWriter, r *http.Request, jobID, model string) {
	reader, cleanup := h.tokenStream.NewBlockingReader()
	defer cleanup()

	ctx := r.Context()
	lastID := "0-0"
	deadline := time.Now().Add(h.config.JobTimeout)

	var fullContent strings.Builder
	var inputTokens, outputTokens int

	for {
		if ctx.Err() != nil {
			writeErrorJSON(w, http.StatusGatewayTimeout, "server_error", "client disconnected")
			return
		}
		if time.Now().After(deadline) {
			writeErrorJSON(w, http.StatusGatewayTimeout, "server_error", "job timed out")
			return
		}

		entries, err := reader.Read(ctx, jobID, lastID, 100, 1*time.Second)
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, "server_error",
				"failed to read response: "+err.Error())
			return
		}

		for _, entry := range entries {
			lastID = entry.ID

			switch entry.Event.Type {
			case "token":
				var token string
				if err := json.Unmarshal(entry.Event.Data, &token); err == nil {
					fullContent.WriteString(token)
				}

		case "done":
			var complete protocol.CompleteMessage
			if err := json.Unmarshal(entry.Event.Data, &complete); err == nil {
				inputTokens = complete.InputTokens
				outputTokens = complete.OutputTokens
				if complete.FullResponse != "" {
					fullContent.Reset()
					fullContent.WriteString(complete.FullResponse)
				}
			}

			usage := &openai.Usage{
				CacheTokens:      complete.CacheTokens,
				PromptTokens:     inputTokens,
				CompletionTokens: outputTokens,
				TotalTokens:      inputTokens + outputTokens,
			}

			resp := openai.NewCompletionResponse(model, fullContent.String(), "stop", usage)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
				return

			case "error":
				var errData map[string]string
				json.Unmarshal(entry.Event.Data, &errData)
				errMsg := errData["error"]
				if errMsg == "" {
					errMsg = "internal error"
				}
				writeErrorJSON(w, http.StatusInternalServerError, "server_error", errMsg)
				return
			}
		}
	}
}

// --- Helpers ---

// extractBearerToken extracts the token from "Authorization: Bearer xxx".
func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	return strings.TrimSpace(auth[len(prefix):])
}

// writeErrorJSON writes an OpenAI-compatible error response.
func writeErrorJSON(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(openai.NewErrorResponse(message, errType, ""))
}
