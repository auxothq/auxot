// Package anthropic defines Anthropic Messages API-compatible request and
// response types. Pure format translation — no I/O, no Redis, no WebSocket.
//
// The router accepts requests in Anthropic format, routes them through the
// same internal protocol as OpenAI requests, and translates the results back
// into Anthropic format for the caller.
//
// Anthropic errors use the format:
//
//	{
//	  "type": "error",
//	  "error": { "type": "not_found_error", "message": "..." },
//	  "request_id": "req_..."
//	}
//
// Message content supports both string and array-of-blocks forms:
//
//	{"role": "user", "content": "Hello"}
//	{"role": "user", "content": [{"type": "text", "text": "Hello"}]}
package anthropic

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Request types
// ---------------------------------------------------------------------------

// MessagesRequest is the Anthropic-compatible /v1/messages request body.
//
// System can be either a plain string or an array of content blocks:
//
//	"system": "You are helpful"
//	"system": [{"type": "text", "text": "You are helpful"}]
//
// Use SystemText() to extract the text regardless of form.
type MessagesRequest struct {
	Model         string          `json:"model"`
	Messages      []Message       `json:"messages"`
	MaxTokens     int             `json:"max_tokens"`
	Stream        bool            `json:"stream,omitempty"`
	System        json.RawMessage `json:"system,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	Tools         []Tool          `json:"tools,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Thinking      *ThinkingConfig `json:"thinking,omitempty"` // Controls chain-of-thought
}

// ThinkingConfig controls model thinking/reasoning behavior.
// Type "enabled" activates thinking; "disabled" turns it off.
type ThinkingConfig struct {
	Type         string `json:"type"`                    // "enabled" or "disabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"` // Max tokens for thinking (when enabled)
}

// SystemText extracts the system prompt text from the request.
// Handles both string and array-of-content-blocks forms.
func (r *MessagesRequest) SystemText() string {
	if len(r.System) == 0 {
		return ""
	}

	// Try string first
	var s string
	if err := json.Unmarshal(r.System, &s); err == nil {
		return s
	}

	// Try array of content blocks
	var blocks []InputContentBlock
	if err := json.Unmarshal(r.System, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}

	return ""
}

// Message represents a single message in the conversation.
//
// Content can be either a plain string or an array of content blocks (text,
// tool_use, tool_result, etc.). Use TextContent() to extract the text
// regardless of form. Use ContentBlocks() to access the full block structure.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []InputContentBlock
}

// NewTextMessage creates a simple text message (convenience for programmatic use).
func NewTextMessage(role, text string) Message {
	data, _ := json.Marshal(text)
	return Message{Role: role, Content: data}
}

// TextContent extracts the text content from the message.
// If content is a string, returns it directly.
// If content is an array of content blocks, concatenates all text blocks.
func (m Message) TextContent() string {
	if len(m.Content) == 0 {
		return ""
	}

	// Try string first (most common case)
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}

	// Try array of content blocks
	var blocks []InputContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}

	return ""
}

// ContentBlocks parses the content as an array of content blocks.
// Returns nil if content is a plain string or cannot be parsed.
func (m Message) ContentBlocks() []InputContentBlock {
	if len(m.Content) == 0 {
		return nil
	}

	var blocks []InputContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}
	return blocks
}

// InputContentBlock represents a content block in an input message.
//
// Anthropic supports many block types. We handle:
//   - text: plain text content
//   - tool_use: model's tool invocation (in assistant messages)
//   - tool_result: result of a tool invocation (in user messages)
//
// Image, document, thinking, and server tool blocks are parsed but their
// payloads are ignored since we cannot forward them to llama.cpp.
type InputContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`          // "text" blocks
	ID        string          `json:"id,omitempty"`            // "tool_use" blocks
	Name      string          `json:"name,omitempty"`          // "tool_use" blocks
	Input     json.RawMessage `json:"input,omitempty"`         // "tool_use" blocks
	ToolUseID string          `json:"tool_use_id,omitempty"`   // "tool_result" blocks
	Content   json.RawMessage `json:"content,omitempty"`       // "tool_result" blocks (string or array)
}

// ToolResultText extracts the text from a tool_result content block.
// The content field can be a string or an array of content blocks.
func (b InputContentBlock) ToolResultText() string {
	if len(b.Content) == 0 {
		return ""
	}

	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		return s
	}

	var blocks []InputContentBlock
	if err := json.Unmarshal(b.Content, &blocks); err == nil {
		var sb strings.Builder
		for _, sub := range blocks {
			if sub.Type == "text" {
				sb.WriteString(sub.Text)
			}
		}
		return sb.String()
	}

	return ""
}

// Tool defines a tool the model can call (Anthropic style).
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"` // JSON Schema
}

// CountTokensRequest is the /v1/messages/count_tokens request body.
//
// System can be either a plain string or an array of content blocks (same as
// MessagesRequest). Use SystemText() to extract the text.
type CountTokensRequest struct {
	Model    string          `json:"model"`
	Messages []Message       `json:"messages"`
	System   json.RawMessage `json:"system,omitempty"`
	Tools    []Tool          `json:"tools,omitempty"`
}

// SystemText extracts the system prompt text from the request.
func (r *CountTokensRequest) SystemText() string {
	if len(r.System) == 0 {
		return ""
	}

	var s string
	if err := json.Unmarshal(r.System, &s); err == nil {
		return s
	}

	var blocks []InputContentBlock
	if err := json.Unmarshal(r.System, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}

	return ""
}

// CountTokensResponse is returned from /v1/messages/count_tokens.
type CountTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// ---------------------------------------------------------------------------
// Non-streaming response
// ---------------------------------------------------------------------------

// MessagesResponse is the Anthropic-compatible non-streaming response.
type MessagesResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"` // "message"
	Role         string         `json:"role"` // "assistant"
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   *string        `json:"stop_reason"`   // "end_turn", "max_tokens", "tool_use"
	StopSequence *string        `json:"stop_sequence"`
	Usage        Usage          `json:"usage"`
}

// ContentBlock is a single content block in the response.
// Supports "thinking", "text", and "tool_use" block types.
type ContentBlock struct {
	Type     string          `json:"type"`              // "thinking", "text", or "tool_use"
	Thinking string          `json:"thinking,omitempty"` // for "thinking" blocks
	Text     string          `json:"text,omitempty"`    // for "text" blocks
	ID       string          `json:"id,omitempty"`      // for "tool_use" blocks
	Name     string          `json:"name,omitempty"`    // for "tool_use" blocks
	Input    json.RawMessage `json:"input,omitempty"`   // for "tool_use" blocks
}

// Usage reports token counts.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ---------------------------------------------------------------------------
// Streaming event types
// ---------------------------------------------------------------------------

// MessageStartEvent is the first SSE event in a streaming response.
type MessageStartEvent struct {
	Type    string           `json:"type"` // "message_start"
	Message MessagesResponse `json:"message"`
}

// ContentBlockStartEvent marks the beginning of a content block.
type ContentBlockStartEvent struct {
	Type         string       `json:"type"` // "content_block_start"
	Index        int          `json:"index"`
	ContentBlock ContentBlock `json:"content_block"`
}

// ContentBlockDeltaEvent carries a chunk of content.
type ContentBlockDeltaEvent struct {
	Type  string `json:"type"` // "content_block_delta"
	Index int    `json:"index"`
	Delta Delta  `json:"delta"`
}

// Delta is the inner delta payload in a content_block_delta event.
type Delta struct {
	Type        string `json:"type"`                    // "thinking_delta", "text_delta", or "input_json_delta"
	Thinking    string `json:"thinking,omitempty"`      // for thinking_delta
	Text        string `json:"text,omitempty"`          // for text_delta
	PartialJSON string `json:"partial_json,omitempty"`  // for input_json_delta
}

// ContentBlockStopEvent marks the end of a content block.
type ContentBlockStopEvent struct {
	Type  string `json:"type"` // "content_block_stop"
	Index int    `json:"index"`
}

// MessageDeltaEvent carries end-of-message metadata (stop_reason, final usage).
type MessageDeltaEvent struct {
	Type  string       `json:"type"` // "message_delta"
	Delta MessageDelta `json:"delta"`
	Usage DeltaUsage   `json:"usage"`
}

// MessageDelta holds the final stop_reason.
type MessageDelta struct {
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

// DeltaUsage holds the final output token count.
type DeltaUsage struct {
	OutputTokens int `json:"output_tokens"`
}

// MessageStopEvent is the final SSE event.
type MessageStopEvent struct {
	Type string `json:"type"` // "message_stop"
}

// PingEvent is a keepalive event sent during streaming.
type PingEvent struct {
	Type string `json:"type"` // "ping"
}

// ---------------------------------------------------------------------------
// Model types (for GET /v1/models)
// ---------------------------------------------------------------------------

// ModelObject represents a single model in the Anthropic model list.
type ModelObject struct {
	ID          string `json:"id"`
	CreatedAt   string `json:"created_at"`   // ISO 8601 timestamp
	DisplayName string `json:"display_name"`
	Type        string `json:"type"` // "model"
}

// ModelListResponse is the response for GET /v1/models.
type ModelListResponse struct {
	Data    []ModelObject `json:"data"`
	FirstID *string       `json:"first_id"`
	HasMore bool          `json:"has_more"`
	LastID  *string       `json:"last_id"`
}

// ---------------------------------------------------------------------------
// Error response
// ---------------------------------------------------------------------------

// ErrorResponse is an Anthropic-compatible error response body.
//
//	{
//	  "type": "error",
//	  "error": { "type": "not_found_error", "message": "..." },
//	  "request_id": "req_011CSHoEeqs5C35K2UUqR7Fy"
//	}
type ErrorResponse struct {
	Type      string      `json:"type"`       // "error"
	Error     ErrorDetail `json:"error"`
	RequestID string      `json:"request_id"` // "req_..."
}

// ErrorDetail holds the error information.
type ErrorDetail struct {
	Type    string `json:"type"` // "not_found_error", "authentication_error", etc.
	Message string `json:"message"`
}

// NewErrorResponse creates an Anthropic-compatible error response.
func NewErrorResponse(errType, message string) *ErrorResponse {
	return &ErrorResponse{
		Type: "error",
		Error: ErrorDetail{
			Type:    errType,
			Message: message,
		},
		RequestID: NewRequestID(),
	}
}

// ---------------------------------------------------------------------------
// Builder functions
// ---------------------------------------------------------------------------

// NewMessageID generates a unique message ID (msg_xxx format).
func NewMessageID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "msg_" + hex.EncodeToString(b)
}

// NewRequestID generates a unique request ID (req_xxx format).
func NewRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

// NewNonStreamingResponse builds a complete non-streaming response.
func NewNonStreamingResponse(model, content string, stopReason string, usage Usage) *MessagesResponse {
	sr := stopReason
	return &MessagesResponse{
		ID:         NewMessageID(),
		Type:       "message",
		Role:       "assistant",
		Content:    []ContentBlock{{Type: "text", Text: content}},
		Model:      model,
		StopReason: &sr,
		Usage:      usage,
	}
}

// NewNonStreamingResponseWithThinking builds a non-streaming response that includes a thinking block before the text.
func NewNonStreamingResponseWithThinking(model, thinking, content string, stopReason string, usage Usage) *MessagesResponse {
	sr := stopReason
	var blocks []ContentBlock
	if thinking != "" {
		blocks = append(blocks, ContentBlock{Type: "thinking", Thinking: thinking})
	}
	blocks = append(blocks, ContentBlock{Type: "text", Text: content})
	return &MessagesResponse{
		ID:         NewMessageID(),
		Type:       "message",
		Role:       "assistant",
		Content:    blocks,
		Model:      model,
		StopReason: &sr,
		Usage:      usage,
	}
}

// NewToolUseResponse builds a non-streaming response with tool use blocks.
func NewToolUseResponse(model string, toolCalls []ContentBlock, usage Usage) *MessagesResponse {
	sr := "tool_use"
	return &MessagesResponse{
		ID:         NewMessageID(),
		Type:       "message",
		Role:       "assistant",
		Content:    toolCalls,
		Model:      model,
		StopReason: &sr,
		Usage:      usage,
	}
}

// ---------------------------------------------------------------------------
// SSE formatting
// ---------------------------------------------------------------------------

// FormatSSE formats a named SSE event. Anthropic uses "event: type\ndata: json\n\n".
func FormatSSE(eventType string, data any) ([]byte, error) {
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	result := fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, string(jsonBytes))
	return []byte(result), nil
}

// ---------------------------------------------------------------------------
// Token estimation
// ---------------------------------------------------------------------------

// EstimateTokens gives a rough token count estimate for text content.
// Uses the ~4 chars/token heuristic common for English text.
// In production, route to llama.cpp /tokenize for exact counts.
func EstimateTokens(texts ...string) int {
	total := 0
	for _, t := range texts {
		total += len(t)
	}
	// Rough estimate: 4 characters per token, minimum 1
	tokens := total / 4
	if tokens < 1 && total > 0 {
		tokens = 1
	}
	// Add overhead for message framing (~4 tokens per message)
	return tokens
}

// ---------------------------------------------------------------------------
// Timestamp helper
// ---------------------------------------------------------------------------

// Now returns the current Unix timestamp.
func Now() int64 {
	return time.Now().Unix()
}

// FormatTimestamp returns an ISO 8601 formatted timestamp string.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
