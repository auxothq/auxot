// Package openai defines the OpenAI and Anthropic API-compatible request
// and response types. This package does pure format translation — no I/O,
// no Redis, no WebSocket.
//
// The router accepts requests in OpenAI format, routes them to llama.cpp
// via GPU workers, and translates the results back into OpenAI (or Anthropic)
// format for the caller.
package openai

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// --- Request types ---

// ChatCompletionRequest is the OpenAI-compatible /v1/chat/completions request body.
type ChatCompletionRequest struct {
	Model       string    `json:"model,omitempty"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	Stop        any       `json:"stop,omitempty"` // string or []string
}

// Message represents a single message in the conversation.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// Tool defines a function the model can call.
type Tool struct {
	Type     string       `json:"type"` // always "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction describes the function name, description, and parameters schema.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema
}

// ToolCall represents a tool call returned by the model.
type ToolCall struct {
	Index    int              `json:"-"` // Streaming only: used to merge SSE deltas by position
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction has the function name and serialized arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// --- Non-streaming response ---

// ChatCompletionResponse is the OpenAI-compatible non-streaming response.
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"` // "chat.completion"
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice is a single completion choice.
type Choice struct {
	Index        int      `json:"index"`
	Message      *Message `json:"message,omitempty"`       // non-streaming
	Delta        *Message `json:"delta,omitempty"`          // streaming
	FinishReason *string  `json:"finish_reason"`           // "stop", "tool_calls", null
}

// Usage reports token counts.
type Usage struct {
	CacheTokens      int `json:"cache_tokens,omitempty"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// --- Streaming response (SSE chunks) ---

// ChatCompletionChunk is a single SSE chunk in the streaming response.
type ChatCompletionChunk struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"` // "chat.completion.chunk"
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// --- Builder functions ---

// NewCompletionID generates a unique completion ID (chatcmpl-xxx format).
func NewCompletionID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b) // crypto/rand never errors on supported platforms
	return "chatcmpl-" + hex.EncodeToString(b)
}

// NewNonStreamingResponse builds a complete non-streaming response.
func NewNonStreamingResponse(model, content string, finishReason string, usage *Usage) *ChatCompletionResponse {
	fr := finishReason
	return &ChatCompletionResponse{
		ID:      NewCompletionID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{
			{
				Index:        0,
				Message:      &Message{Role: "assistant", Content: content},
				FinishReason: &fr,
			},
		},
		Usage: usage,
	}
}

// NewToolCallResponse builds a non-streaming response with tool calls.
func NewToolCallResponse(model string, toolCalls []ToolCall, usage *Usage) *ChatCompletionResponse {
	fr := "tool_calls"
	return &ChatCompletionResponse{
		ID:      NewCompletionID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{
			{
				Index: 0,
				Message: &Message{
					Role:      "assistant",
					ToolCalls: toolCalls,
				},
				FinishReason: &fr,
			},
		},
		Usage: usage,
	}
}

// NewStreamingChunk builds a single SSE chunk with a content delta.
func NewStreamingChunk(completionID, model, content string) *ChatCompletionChunk {
	return &ChatCompletionChunk{
		ID:      completionID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{
			{
				Index:        0,
				Delta:        &Message{Content: content},
				FinishReason: nil,
			},
		},
	}
}

// NewStreamingRoleChunk builds the initial SSE chunk that sets the role.
func NewStreamingRoleChunk(completionID, model string) *ChatCompletionChunk {
	return &ChatCompletionChunk{
		ID:      completionID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{
			{
				Index:        0,
				Delta:        &Message{Role: "assistant", Content: ""},
				FinishReason: nil,
			},
		},
	}
}

// NewStreamingDoneChunk builds the final SSE chunk with a finish reason and optional usage.
func NewStreamingDoneChunk(completionID, model, finishReason string, usage *Usage) *ChatCompletionChunk {
	fr := finishReason
	return &ChatCompletionChunk{
		ID:      completionID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{
			{
				Index:        0,
				Delta:        &Message{},
				FinishReason: &fr,
			},
		},
		Usage: usage,
	}
}

// FormatSSE formats a chunk as a Server-Sent Event data line.
// Returns "data: {json}\n\n"
func FormatSSE(chunk *ChatCompletionChunk) ([]byte, error) {
	data, err := json.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	// "data: " + json + "\n\n"
	result := make([]byte, 0, 6+len(data)+2)
	result = append(result, "data: "...)
	result = append(result, data...)
	result = append(result, '\n', '\n')
	return result, nil
}

// FormatSSEDone returns the final SSE sentinel "[DONE]".
func FormatSSEDone() []byte {
	return []byte("data: [DONE]\n\n")
}

// --- Legacy Completions (POST /completions) ---

// CompletionRequest is the OpenAI-compatible /completions request body.
// This is the legacy text completions endpoint. We translate the prompt into a
// single user message and run it through the same chat pipeline.
type CompletionRequest struct {
	Model       string          `json:"model,omitempty"`
	Prompt      json.RawMessage `json:"prompt"` // string, []string, or token arrays — we handle string and []string
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Stop        any             `json:"stop,omitempty"`
	Suffix      string          `json:"suffix,omitempty"`
	// Ignored: best_of, echo, frequency_penalty, logprobs, logit_bias,
	// presence_penalty, seed, user, n
}

// PromptString extracts the prompt as a single string.
// Handles "prompt": "text" and "prompt": ["text", ...] (takes first element).
func (r *CompletionRequest) PromptString() string {
	if len(r.Prompt) == 0 {
		return ""
	}
	// Try as string first
	var s string
	if err := json.Unmarshal(r.Prompt, &s); err == nil {
		return s
	}
	// Try as array of strings
	var arr []string
	if err := json.Unmarshal(r.Prompt, &arr); err == nil && len(arr) > 0 {
		return arr[0]
	}
	return ""
}

// CompletionResponse is the OpenAI-compatible /completions response.
type CompletionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"` // "text_completion"
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []CompletionChoice `json:"choices"`
	Usage   *Usage             `json:"usage,omitempty"`
}

// CompletionChoice is a single choice in a legacy completion response.
type CompletionChoice struct {
	Text         string `json:"text"`
	Index        int    `json:"index"`
	Logprobs     *any   `json:"logprobs"`      // always null
	FinishReason string `json:"finish_reason"` // "stop" or "length"
}

// NewLegacyCompletionID generates a unique completion ID (cmpl-xxx format).
func NewLegacyCompletionID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "cmpl-" + hex.EncodeToString(b)
}

// NewCompletionResponse builds a legacy text completion response.
func NewCompletionResponse(model, text, finishReason string, usage *Usage) *CompletionResponse {
	return &CompletionResponse{
		ID:      NewLegacyCompletionID(),
		Object:  "text_completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []CompletionChoice{
			{
				Text:         text,
				Index:        0,
				Logprobs:     nil,
				FinishReason: finishReason,
			},
		},
		Usage: usage,
	}
}

// --- Models ---

// ModelObject represents a single model in the OpenAI models API.
type ModelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // "model"
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelListResponse is the response for GET /models.
type ModelListResponse struct {
	Object string        `json:"object"` // "list"
	Data   []ModelObject `json:"data"`
}

// --- Embeddings ---

// EmbeddingRequest is the OpenAI-compatible /embeddings request body.
type EmbeddingRequest struct {
	Input          json.RawMessage `json:"input"` // string or []string
	Model          string          `json:"model,omitempty"`
	Dimensions     *int            `json:"dimensions,omitempty"`
	EncodingFormat string          `json:"encoding_format,omitempty"` // "float" or "base64"
}

// EmbeddingResponse is the OpenAI-compatible /embeddings response.
type EmbeddingResponse struct {
	Object string            `json:"object"` // "list"
	Data   []EmbeddingObject `json:"data"`
	Model  string            `json:"model"`
	Usage  EmbeddingUsage    `json:"usage"`
}

// EmbeddingObject is a single embedding vector.
type EmbeddingObject struct {
	Object    string    `json:"object"` // "embedding"
	Embedding []float64 `json:"embedding"`
	Index     int       `json:"index"`
}

// EmbeddingUsage reports token counts for an embedding request.
type EmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// --- Stored completions list (stub) ---

// ListResponse is the OpenAI paginated list envelope.
// We don't store completions, so data is always empty.
type ListResponse struct {
	Object  string  `json:"object"` // "list"
	Data    []any   `json:"data"`
	HasMore bool    `json:"has_more"`
	FirstID *string `json:"first_id"`
	LastID  *string `json:"last_id"`
}

// EmptyListResponse returns an empty paginated list.
func EmptyListResponse() *ListResponse {
	return &ListResponse{
		Object:  "list",
		Data:    []any{},
		HasMore: false,
		FirstID: nil,
		LastID:  nil,
	}
}

// --- Error response ---

// ErrorResponse is an OpenAI-compatible error response body.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail holds the error information.
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// NewErrorResponse creates a standard error response.
func NewErrorResponse(message, errType, code string) *ErrorResponse {
	return &ErrorResponse{
		Error: ErrorDetail{
			Message: message,
			Type:    errType,
			Code:    code,
		},
	}
}
