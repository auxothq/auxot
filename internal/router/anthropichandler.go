package router

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/auxothq/auxot/pkg/anthropic"
	"github.com/auxothq/auxot/pkg/auth"
	"github.com/auxothq/auxot/pkg/openai"
	"github.com/auxothq/auxot/pkg/protocol"
	"github.com/auxothq/auxot/pkg/queue"
)

// AnthropicHandler handles Anthropic Messages API-compatible requests.
//
// Internally, it translates Anthropic requests into the same protocol messages
// used by OpenAI requests, enqueues them to Redis for worker consumers, and
// translates the results back into Anthropic response format.
//
// Routes are served under /api/anthropic/ with the full Anthropic path structure:
//
//	POST   /api/anthropic/v1/messages                           - Create a message
//	POST   /api/anthropic/v1/messages/count_tokens              - Count tokens
//	POST   /api/anthropic/v1/messages/batches                   - Create batch (stub)
//	GET    /api/anthropic/v1/messages/batches                   - List batches (stub)
//	GET    /api/anthropic/v1/messages/batches/{id}              - Get batch (stub)
//	POST   /api/anthropic/v1/messages/batches/{id}/cancel       - Cancel batch (stub)
//	DELETE /api/anthropic/v1/messages/batches/{id}              - Delete batch (stub)
//	GET    /api/anthropic/v1/messages/batches/{id}/results      - Get batch results (stub)
//	GET    /api/anthropic/v1/models                             - List models
//	GET    /api/anthropic/v1/models/{id}                        - Get model
//
// Base URL: /api/anthropic — clients add /v1/messages, etc.
type AnthropicHandler struct {
	verifier    *auth.Verifier
	jobQueue    *queue.JobQueue
	tokenStream *queue.TokenStream
	config      *Config
	logger      *slog.Logger
}

// NewAnthropicHandler creates an Anthropic API handler.
func NewAnthropicHandler(
	verifier *auth.Verifier,
	jobQueue *queue.JobQueue,
	tokenStream *queue.TokenStream,
	config *Config,
	logger *slog.Logger,
) *AnthropicHandler {
	return &AnthropicHandler{
		verifier:    verifier,
		jobQueue:    jobQueue,
		tokenStream: tokenStream,
		config:      config,
		logger:      logger,
	}
}

// ServeHTTP routes incoming requests to the appropriate handler.
//
// The /api/anthropic/ prefix is stripped by the parent mux in server.go,
// so we route on /v1/messages, /v1/models, etc.
func (h *AnthropicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	method := r.Method

	switch {
	// --- Messages ---
	case method == http.MethodPost && path == "/v1/messages":
		h.handleMessages(w, r)

	case method == http.MethodPost && path == "/v1/messages/count_tokens":
		h.handleCountTokens(w, r)

	// --- Batches (all stubs) ---
	case method == http.MethodPost && path == "/v1/messages/batches":
		h.requireAuth(w, r, h.handleBatchStub)

	case method == http.MethodGet && path == "/v1/messages/batches":
		h.requireAuth(w, r, h.handleBatchStub)

	case strings.HasPrefix(path, "/v1/messages/batches/"):
		rest := strings.TrimPrefix(path, "/v1/messages/batches/")
		switch {
		case method == http.MethodGet && strings.HasSuffix(rest, "/results"):
			h.requireAuth(w, r, h.handleBatchStub)
		case method == http.MethodPost && strings.HasSuffix(rest, "/cancel"):
			h.requireAuth(w, r, h.handleBatchStub)
		case method == http.MethodGet && !strings.Contains(rest, "/"):
			h.requireAuth(w, r, h.handleBatchStub)
		case method == http.MethodDelete && !strings.Contains(rest, "/"):
			h.requireAuth(w, r, h.handleBatchStub)
		default:
			h.requireAuth(w, r, h.handleCatchAll)
		}

	// --- Models ---
	case method == http.MethodGet && path == "/v1/models":
		h.requireAuth(w, r, h.handleListModels)

	case method == http.MethodGet && strings.HasPrefix(path, "/v1/models/"):
		h.requireAuth(w, r, h.handleGetModel)

	// --- Catch-all (files, skills, etc.) ---
	case strings.HasPrefix(path, "/v1/"):
		h.requireAuth(w, r, h.handleCatchAll)

	default:
		writeAnthropicError(w, http.StatusNotFound, "not_found_error",
			"The requested resource could not be found.")
	}
}

// ---------------------------------------------------------------------------
// Auth helper
// ---------------------------------------------------------------------------

// requireAuth authenticates via x-api-key header (Anthropic convention) or
// Bearer token (convenience). Calls next only if authentication succeeds.
func (h *AnthropicHandler) requireAuth(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	apiKey := r.Header.Get("x-api-key")
	if apiKey == "" {
		apiKey = extractBearerToken(r)
	}
	if apiKey == "" {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error",
			"Missing API key. Set the x-api-key header or use Bearer token.")
		return
	}

	valid, err := h.verifier.VerifyAPIKey(apiKey)
	if err != nil || !valid {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error",
			"Invalid API key.")
		return
	}

	next(w, r)
}

// ---------------------------------------------------------------------------
// POST /v1/messages
// ---------------------------------------------------------------------------

// handleMessages processes an Anthropic-compatible /v1/messages request.
func (h *AnthropicHandler) handleMessages(w http.ResponseWriter, r *http.Request) {
	// --- Authenticate ---
	apiKey := r.Header.Get("x-api-key")
	if apiKey == "" {
		apiKey = extractBearerToken(r)
	}
	if apiKey == "" {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error",
			"Missing API key. Set the x-api-key header or use Bearer token.")
		return
	}

	valid, err := h.verifier.VerifyAPIKey(apiKey)
	if err != nil || !valid {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error",
			"Invalid API key.")
		return
	}

	// --- Parse request ---
	var req anthropic.MessagesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.logger.Warn("failed to decode request",
			"error", err.Error(),
		)
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"Invalid JSON: "+err.Error())
		return
	}

	h.logger.Info("messages request",
		"model", req.Model,
		"messages", len(req.Messages),
		"stream", req.Stream,
		"max_tokens", req.MaxTokens,
		"has_system", len(req.System) > 0,
		"has_tools", len(req.Tools) > 0,
	)

	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"messages: Field required")
		return
	}
	if req.Model == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"model: Field required")
		return
	}
	if req.MaxTokens == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"max_tokens: Field required")
		return
	}

	// --- Build internal job ---
	jobID := openai.NewCompletionID() // Reuse the same ID generator

	// Convert Anthropic messages → protocol messages
	var protoMessages []protocol.ChatMessage

	// System message (Anthropic puts it at the top level, not in messages)
	if sysText := req.SystemText(); sysText != "" {
		protoMessages = append(protoMessages, protocol.ChatMessage{
			Role:    "system",
			Content: sysText,
		})
	}

	for _, m := range req.Messages {
		// Handle content blocks for tool_use, tool_result, and text
		blocks := m.ContentBlocks()
		if blocks != nil {
			// Collect text, tool_calls, and tool_results from content blocks
			var textContent string
			var toolCalls []protocol.ToolCall
			var toolResults []protocol.ChatMessage

			for _, b := range blocks {
				switch b.Type {
				case "text":
					textContent += b.Text
				case "tool_use":
					// Model invoked a tool — convert to protocol tool call
					toolCalls = append(toolCalls, protocol.ToolCall{
						ID:   b.ID,
						Type: "function",
						Function: protocol.ToolFunction{
							Name:      b.Name,
							Arguments: string(b.Input),
						},
					})
				case "tool_result":
					// Each tool_result becomes a separate role:"tool" message
					// (OpenAI format requires one message per tool result)
					toolResults = append(toolResults, protocol.ChatMessage{
						Role:       "tool",
						ToolCallID: b.ToolUseID,
						Content:    b.ToolResultText(),
					})
				}
			}

			// Build the main message (assistant with text + tool_calls,
			// or user with text content)
			if m.Role == "assistant" {
				if textContent != "" || len(toolCalls) > 0 {
					pm := protocol.ChatMessage{
						Role:      "assistant",
						Content:   textContent,
						ToolCalls: toolCalls,
					}
					protoMessages = append(protoMessages, pm)
				}
			} else if len(toolResults) > 0 {
				// User message containing tool_result blocks →
				// emit each as a separate role:"tool" message
				for _, tr := range toolResults {
					protoMessages = append(protoMessages, tr)
				}
			} else {
				// Regular user/system message with array content
				protoMessages = append(protoMessages, protocol.ChatMessage{
					Role:    m.Role,
					Content: textContent,
				})
			}
		} else {
			// Plain string content — simple message
			protoMessages = append(protoMessages, protocol.ChatMessage{
				Role:    m.Role,
				Content: m.TextContent(),
			})
		}
	}

	// Convert Anthropic tools → protocol tools
	var protoTools []protocol.Tool
	for _, t := range req.Tools {
		protoTools = append(protoTools, protocol.Tool{
			Type: "function",
			Function: protocol.ToolDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema, // Anthropic calls it input_schema, same thing
			},
		})
	}

	maxTokens := req.MaxTokens

	// Translate Anthropic thinking config to reasoning_effort
	reasoningEffort := ""
	if req.Thinking != nil {
		if req.Thinking.Type == "disabled" {
			reasoningEffort = "none"
		} else if req.Thinking.Type == "enabled" {
			reasoningEffort = "high" // Anthropic "enabled" → full thinking
		}
	}

	jobMsg := &protocol.JobMessage{
		Type:            protocol.TypeJob,
		JobID:           jobID,
		Messages:        protoMessages,
		Tools:           protoTools,
		Temperature:     req.Temperature,
		MaxTokens:       &maxTokens,
		ReasoningEffort: reasoningEffort,
	}

	// --- Enqueue for the dispatcher ---
	if err := enqueueJob(h.jobQueue, jobMsg, h.logger); err != nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, "overloaded_error", err.Error())
		return
	}

	h.logger.Info("job accepted (anthropic)",
		"job_id", jobID,
		"model", req.Model,
		"messages", len(req.Messages),
		"stream", req.Stream,
	)

	// --- Return response ---
	if req.Stream {
		h.streamResponse(w, r, jobID, req.Model)
	} else {
		h.blockingResponse(w, r, jobID, req.Model)
	}
}

// ---------------------------------------------------------------------------
// POST /v1/messages/count_tokens
// ---------------------------------------------------------------------------

// handleCountTokens estimates the token count for a set of messages.
func (h *AnthropicHandler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	// --- Authenticate ---
	apiKey := r.Header.Get("x-api-key")
	if apiKey == "" {
		apiKey = extractBearerToken(r)
	}
	if apiKey == "" {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error",
			"Missing API key. Set the x-api-key header or use Bearer token.")
		return
	}

	valid, err := h.verifier.VerifyAPIKey(apiKey)
	if err != nil || !valid {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error",
			"Invalid API key.")
		return
	}

	// --- Parse request ---
	var req anthropic.CountTokensRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"Invalid JSON: "+err.Error())
		return
	}

	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"messages: Field required")
		return
	}

	// Collect all text for estimation
	var texts []string
	if sysText := req.SystemText(); sysText != "" {
		texts = append(texts, sysText)
	}
	for _, m := range req.Messages {
		texts = append(texts, m.TextContent())
	}
	for _, t := range req.Tools {
		texts = append(texts, t.Name, t.Description)
		if len(t.InputSchema) > 0 {
			texts = append(texts, string(t.InputSchema))
		}
	}

	estimate := anthropic.EstimateTokens(texts...)

	// Add per-message overhead (role tokens, framing)
	estimate += len(req.Messages) * 4

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(anthropic.CountTokensResponse{
		InputTokens: estimate,
	})
}

// ---------------------------------------------------------------------------
// Batch endpoints (all stubs)
// ---------------------------------------------------------------------------

// handleBatchStub returns a 501 Not Implemented for all batch endpoints.
func (h *AnthropicHandler) handleBatchStub(w http.ResponseWriter, r *http.Request) {
	writeAnthropicError(w, http.StatusNotImplemented, "not_implemented_error",
		"Message batches are not yet supported. "+
			"This feature requires persistent job storage, which is planned for a future release.")
}

// ---------------------------------------------------------------------------
// GET /v1/models
// ---------------------------------------------------------------------------

// handleListModels lists the active model from the policy.
func (h *AnthropicHandler) handleListModels(w http.ResponseWriter, r *http.Request) {
	modelID := h.config.ModelName
	displayName := modelID // Use model ID as display name

	model := anthropic.ModelObject{
		ID:          modelID,
		CreatedAt:   anthropic.FormatTimestamp(time.Now()),
		DisplayName: displayName,
		Type:        "model",
	}

	resp := anthropic.ModelListResponse{
		Data:    []anthropic.ModelObject{model},
		FirstID: &modelID,
		HasMore: false,
		LastID:  &modelID,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ---------------------------------------------------------------------------
// GET /v1/models/{id}
// ---------------------------------------------------------------------------

// handleGetModel returns details for a specific model.
func (h *AnthropicHandler) handleGetModel(w http.ResponseWriter, r *http.Request) {
	// Extract model ID from path: /v1/models/{id}
	modelID := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	if modelID == "" {
		writeAnthropicError(w, http.StatusNotFound, "not_found_error",
			"The requested resource could not be found.")
		return
	}

	// We only serve the one model configured in the router
	if modelID != h.config.ModelName {
		writeAnthropicError(w, http.StatusNotFound, "not_found_error",
			fmt.Sprintf("The model %q does not exist or you do not have access to it.", modelID))
		return
	}

	model := anthropic.ModelObject{
		ID:          modelID,
		CreatedAt:   anthropic.FormatTimestamp(time.Now()),
		DisplayName: modelID,
		Type:        "model",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(model)
}

// ---------------------------------------------------------------------------
// Catch-all
// ---------------------------------------------------------------------------

// handleCatchAll returns a 501 Not Implemented for any unhandled Anthropic API routes.
func (h *AnthropicHandler) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	writeAnthropicError(w, http.StatusNotImplemented, "not_implemented_error",
		fmt.Sprintf("The endpoint '%s %s' is not implemented.", r.Method, r.URL.Path))
}

// ---------------------------------------------------------------------------
// Streaming response
// ---------------------------------------------------------------------------

// streamResponse streams Anthropic-style SSE events.
func (h *AnthropicHandler) streamResponse(w http.ResponseWriter, r *http.Request, jobID, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "Streaming not supported.")
		return
	}

	// Dedicated Redis connection for blocking token reads.
	reader, cleanup := h.tokenStream.NewBlockingReader()
	defer cleanup()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	msgID := anthropic.NewMessageID()

	// message_start
	startMsg := anthropic.MessagesResponse{
		ID:      msgID,
		Type:    "message",
		Role:    "assistant",
		Content: []anthropic.ContentBlock{},
		Model:   model,
		Usage:   anthropic.Usage{InputTokens: 0, OutputTokens: 0},
	}
	if data, err := anthropic.FormatSSE("message_start", anthropic.MessageStartEvent{
		Type:    "message_start",
		Message: startMsg,
	}); err == nil {
		w.Write(data)
		flusher.Flush()
	}

	// ping
	if data, err := anthropic.FormatSSE("ping", anthropic.PingEvent{Type: "ping"}); err == nil {
		w.Write(data)
		flusher.Flush()
	}

	// Read tokens from Redis stream
	ctx := r.Context()
	lastID := "0-0"
	outputTokens := 0
	currentBlockIndex := 0
	hasStartedTextBlock := false
	hasStartedThinkingBlock := false

	for {
		if ctx.Err() != nil {
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
			case "reasoning_token":
				var token string
				if err := json.Unmarshal(entry.Event.Data, &token); err != nil {
					continue
				}

				// Start thinking block on first reasoning token
				if !hasStartedThinkingBlock {
					if data, err := anthropic.FormatSSE("content_block_start", anthropic.ContentBlockStartEvent{
						Type:         "content_block_start",
						Index:        currentBlockIndex,
						ContentBlock: anthropic.ContentBlock{Type: "thinking", Thinking: ""},
					}); err == nil {
						w.Write(data)
						flusher.Flush()
					}
					hasStartedThinkingBlock = true
				}

				delta := anthropic.ContentBlockDeltaEvent{
					Type:  "content_block_delta",
					Index: currentBlockIndex,
					Delta: anthropic.Delta{Type: "thinking_delta", Thinking: token},
				}
				if data, err := anthropic.FormatSSE("content_block_delta", delta); err == nil {
					w.Write(data)
					flusher.Flush()
				}

			case "token":
				var token string
				if err := json.Unmarshal(entry.Event.Data, &token); err != nil {
					continue
				}

				// Close thinking block before starting text block (if thinking was active)
				if hasStartedThinkingBlock && !hasStartedTextBlock {
					if data, err := anthropic.FormatSSE("content_block_stop", anthropic.ContentBlockStopEvent{
						Type:  "content_block_stop",
						Index: currentBlockIndex,
					}); err == nil {
						w.Write(data)
					}
					currentBlockIndex++
					hasStartedThinkingBlock = false // Prevent double-close
				}

				// Start text block on first content token
				if !hasStartedTextBlock {
					if data, err := anthropic.FormatSSE("content_block_start", anthropic.ContentBlockStartEvent{
						Type:         "content_block_start",
						Index:        currentBlockIndex,
						ContentBlock: anthropic.ContentBlock{Type: "text", Text: ""},
					}); err == nil {
						w.Write(data)
						flusher.Flush()
					}
					hasStartedTextBlock = true
				}

				outputTokens++

				delta := anthropic.ContentBlockDeltaEvent{
					Type:  "content_block_delta",
					Index: currentBlockIndex,
					Delta: anthropic.Delta{Type: "text_delta", Text: token},
				}
				if data, err := anthropic.FormatSSE("content_block_delta", delta); err == nil {
					w.Write(data)
					flusher.Flush()
				}

			case "done":
				// Parse completion for usage and tool calls
				var complete protocol.CompleteMessage
				json.Unmarshal(entry.Event.Data, &complete)
				if complete.OutputTokens > 0 {
					outputTokens = complete.OutputTokens
				}

				// Close thinking block if still open
				if hasStartedThinkingBlock && !hasStartedTextBlock {
					if data, err := anthropic.FormatSSE("content_block_stop", anthropic.ContentBlockStopEvent{
						Type:  "content_block_stop",
						Index: currentBlockIndex,
					}); err == nil {
						w.Write(data)
					}
					currentBlockIndex++
				}

				// Close the text block if it was started
				if hasStartedTextBlock {
					if data, err := anthropic.FormatSSE("content_block_stop", anthropic.ContentBlockStopEvent{
						Type:  "content_block_stop",
						Index: currentBlockIndex,
					}); err == nil {
						w.Write(data)
					}
					currentBlockIndex++
				}

				// Emit tool_use content blocks (if any)
				for _, tc := range complete.ToolCalls {
					toolID := tc.ID
					if toolID == "" {
						toolID = "toolu_" + openai.NewCompletionID()
					}

					// content_block_start (tool_use)
					if data, err := anthropic.FormatSSE("content_block_start", anthropic.ContentBlockStartEvent{
						Type:  "content_block_start",
						Index: currentBlockIndex,
						ContentBlock: anthropic.ContentBlock{
							Type: "tool_use",
							ID:   toolID,
							Name: tc.Function.Name,
						},
					}); err == nil {
						w.Write(data)
					}

					// content_block_delta (input_json_delta)
					if tc.Function.Arguments != "" {
						if data, err := anthropic.FormatSSE("content_block_delta", anthropic.ContentBlockDeltaEvent{
							Type:  "content_block_delta",
							Index: currentBlockIndex,
							Delta: anthropic.Delta{
								Type:        "input_json_delta",
								PartialJSON: tc.Function.Arguments,
							},
						}); err == nil {
							w.Write(data)
						}
					}

					// content_block_stop
					if data, err := anthropic.FormatSSE("content_block_stop", anthropic.ContentBlockStopEvent{
						Type:  "content_block_stop",
						Index: currentBlockIndex,
					}); err == nil {
						w.Write(data)
					}

					currentBlockIndex++
				}

				stopReason := "end_turn"
				if len(complete.ToolCalls) > 0 {
					stopReason = "tool_use"
				}

				// message_delta
				sr := stopReason
				if data, err := anthropic.FormatSSE("message_delta", anthropic.MessageDeltaEvent{
					Type:  "message_delta",
					Delta: anthropic.MessageDelta{StopReason: &sr},
					Usage: anthropic.DeltaUsage{OutputTokens: outputTokens},
				}); err == nil {
					w.Write(data)
				}

				// message_stop
				if data, err := anthropic.FormatSSE("message_stop", anthropic.MessageStopEvent{
					Type: "message_stop",
				}); err == nil {
					w.Write(data)
				}

				flusher.Flush()
				return

			case "error":
				var errData map[string]string
				json.Unmarshal(entry.Event.Data, &errData)
				errMsg := errData["error"]
				if errMsg == "" {
					errMsg = "internal error"
				}
				if data, err := anthropic.FormatSSE("error", anthropic.NewErrorResponse("api_error", errMsg)); err == nil {
					w.Write(data)
				}
				flusher.Flush()
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Blocking response
// ---------------------------------------------------------------------------

// blockingResponse waits for the full completion and returns it as Anthropic JSON.
func (h *AnthropicHandler) blockingResponse(w http.ResponseWriter, r *http.Request, jobID, model string) {
	// Dedicated Redis connection for blocking token reads.
	reader, cleanup := h.tokenStream.NewBlockingReader()
	defer cleanup()

	ctx := r.Context()
	lastID := "0-0"

	var fullContent strings.Builder
	var reasoningContent strings.Builder
	var inputTokens, outputTokens int

	for {
		if ctx.Err() != nil {
			writeAnthropicError(w, http.StatusGatewayTimeout, "api_error", "Client disconnected.")
			return
		}

		entries, err := reader.Read(ctx, jobID, lastID, 100, 1*time.Second)
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error",
				"Failed to read response: "+err.Error())
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
				}

				usage := anthropic.Usage{
					InputTokens:  inputTokens,
					OutputTokens: outputTokens,
				}

				// Build content blocks
				var contentBlocks []anthropic.ContentBlock

				// Add thinking block if there's reasoning content
				thinking := reasoningContent.String()
				if thinking != "" {
					contentBlocks = append(contentBlocks, anthropic.ContentBlock{
						Type:     "thinking",
						Thinking: thinking,
					})
				}

				// Add text block if there's content
				text := fullContent.String()
				if text != "" {
					contentBlocks = append(contentBlocks, anthropic.ContentBlock{
						Type: "text",
						Text: text,
					})
				}

				// Add tool_use blocks
				for _, tc := range complete.ToolCalls {
					toolID := tc.ID
					if toolID == "" {
						toolID = "toolu_" + openai.NewCompletionID()
					}

					contentBlocks = append(contentBlocks, anthropic.ContentBlock{
						Type:  "tool_use",
						ID:    toolID,
						Name:  tc.Function.Name,
						Input: json.RawMessage(tc.Function.Arguments),
					})
				}

				// If no content blocks at all, add empty text block
				if len(contentBlocks) == 0 {
					contentBlocks = append(contentBlocks, anthropic.ContentBlock{
						Type: "text",
						Text: "",
					})
				}

				stopReason := "end_turn"
				if len(complete.ToolCalls) > 0 {
					stopReason = "tool_use"
				}

				resp := &anthropic.MessagesResponse{
					ID:         anthropic.NewMessageID(),
					Type:       "message",
					Role:       "assistant",
					Content:    contentBlocks,
					Model:      model,
					StopReason: &stopReason,
					Usage:      usage,
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
				writeAnthropicError(w, http.StatusInternalServerError, "api_error", errMsg)
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Error helper
// ---------------------------------------------------------------------------

// writeAnthropicError writes an Anthropic-compatible error response.
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(anthropic.NewErrorResponse(errType, message))
}
