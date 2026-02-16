// Package llamacpp provides an HTTP client for the llama.cpp server's
// OpenAI-compatible /v1/chat/completions endpoint.
//
// The worker binary uses this to forward jobs from the router to llama.cpp
// and stream back tokens. This package handles:
//   - Building the HTTP request
//   - Parsing the SSE stream
//   - Extracting tokens and tool calls from streaming chunks
//   - Cancellation via context
package llamacpp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/auxothq/auxot/pkg/openai"
)

// Client talks to a llama.cpp server over HTTP.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a Client pointing at the given llama.cpp base URL
// (e.g., "http://127.0.0.1:8080").
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{},
	}
}

// StreamToken is a single token event from the SSE stream.
type StreamToken struct {
	Content      string           // Text content (may be empty for tool call chunks)
	ToolCalls    []openai.ToolCall // Tool calls (streamed incrementally)
	FinishReason string           // "stop", "tool_calls", or "" if not done
	Timings      *Timings         // Only present in the final chunk
}

// Timings contains llama.cpp performance metrics from the last chunk.
type Timings struct {
	CacheTokens      int     `json:"cache_n"`
	PromptTokens     int     `json:"prompt_n"`
	PromptMS         float64 `json:"prompt_ms"`
	PredictedTokens  int     `json:"predicted_n"`
	PredictedMS      float64 `json:"predicted_ms"`
	TokensPerSecond  float64 `json:"predicted_per_second"`
}

// CompletionResult is the final result after the entire stream is consumed.
type CompletionResult struct {
	FullResponse  string
	ToolCalls     []openai.ToolCall
	FinishReason  string
	CacheTokens   int
	InputTokens   int
	OutputTokens  int
	DurationMS    float64
}

// StreamCompletion sends a chat completion request to llama.cpp and returns
// a channel of StreamToken events. The channel is closed when the stream ends
// or an error occurs.
//
// The caller MUST drain the channel or cancel the context to avoid goroutine leaks.
// Errors are delivered as a StreamToken with a non-empty FinishReason of "error"
// and the error message in Content.
func (c *Client) StreamCompletion(ctx context.Context, req *openai.ChatCompletionRequest) (<-chan StreamToken, error) {
	// Force streaming on
	reqCopy := *req
	reqCopy.Stream = true

	// CRITICAL: Sanitize tool schemas before sending to llama.cpp.
	// The minja Jinja engine in llama.cpp lacks the `replace` filter.
	// The Unsloth chat template uses `replace` for any property key not in
	// ['type', 'description', 'enum', 'required'], so extra JSON Schema keywords
	// (default, minimum, format, items, additionalProperties, etc.) crash it with
	// "Value is not callable: null". We strip properties to only allowed keys.
	if len(reqCopy.Tools) > 0 {
		sanitized, err := SanitizeTools(reqCopy.Tools)
		if err != nil {
			return nil, fmt.Errorf("sanitizing tools: %w", err)
		}
		reqCopy.Tools = sanitized
	}

	body, err := json.Marshal(reqCopy)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		c.baseURL+"/v1/chat/completions",
		strings.NewReader(string(body)),
	)
	if err != nil {
		return nil, fmt.Errorf("creating HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request to llama.cpp: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("llama.cpp returned %d: %s", resp.StatusCode, string(errBody))
	}

	tokens := make(chan StreamToken, 32) // Buffered to avoid blocking the HTTP read

	go func() {
		defer close(tokens)
		defer resp.Body.Close()
		parseSSEStream(ctx, resp.Body, tokens)
	}()

	return tokens, nil
}

// parseSSEStream reads the SSE stream line by line and sends tokens to the channel.
func parseSSEStream(ctx context.Context, body io.Reader, tokens chan<- StreamToken) {
	scanner := bufio.NewScanner(body)
	// Increase scanner buffer for large chunks (tool call arguments can be big)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)

	for scanner.Scan() {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := scanner.Text()

		// SSE format: "data: {json}" or "data: [DONE]"
		if !strings.HasPrefix(line, "data: ") {
			continue // Skip empty lines, comments, event types
		}

		data := strings.TrimPrefix(line, "data: ")

		// Check for stream termination
		if data == "[DONE]" {
			return
		}

		// Parse the JSON chunk
		var chunk llamaCppChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			tokens <- StreamToken{
				Content:      fmt.Sprintf("error parsing SSE chunk: %v", err),
				FinishReason: "error",
			}
			return
		}

		token := chunkToToken(&chunk)
		select {
		case tokens <- token:
		case <-ctx.Done():
			return
		}
	}

	if err := scanner.Err(); err != nil {
		select {
		case tokens <- StreamToken{
			Content:      fmt.Sprintf("error reading stream: %v", err),
			FinishReason: "error",
		}:
		case <-ctx.Done():
		}
	}
}

// llamaCppChunk is the raw JSON structure from llama.cpp's SSE stream.
// Matches the OpenAI streaming format with llama.cpp extensions (timings).
type llamaCppChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content   string `json:"content,omitempty"`
			Role      string `json:"role,omitempty"`
			ToolCalls []struct {
				Index    *int   `json:"index,omitempty"`
				ID       string `json:"id,omitempty"`
				Type     string `json:"type,omitempty"`
				Function struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				} `json:"function,omitempty"`
			} `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Timings *Timings `json:"timings,omitempty"`
}

// chunkToToken converts a raw llama.cpp chunk to our StreamToken type.
func chunkToToken(chunk *llamaCppChunk) StreamToken {
	token := StreamToken{
		Timings: chunk.Timings,
	}

	if len(chunk.Choices) == 0 {
		return token
	}

	choice := chunk.Choices[0]
	token.Content = choice.Delta.Content

	if choice.FinishReason != nil {
		token.FinishReason = *choice.FinishReason
	}

	// Convert tool calls — preserve the streaming index for delta merging.
	for _, tc := range choice.Delta.ToolCalls {
		idx := 0
		if tc.Index != nil {
			idx = *tc.Index
		}
		token.ToolCalls = append(token.ToolCalls, openai.ToolCall{
			Index: idx,
			ID:    tc.ID,
			Type:  tc.Type,
			Function: openai.ToolCallFunction{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}

	return token
}

// CollectStream reads all tokens from a stream channel and returns the
// accumulated result. This is useful for non-streaming API responses where
// the caller wants the complete text.
func CollectStream(tokens <-chan StreamToken) *CompletionResult {
	result := &CompletionResult{}

	// Merge tool call deltas by streaming index.
	// SSE chunks carry incremental fragments: only the first chunk for a given
	// index has id/type/name, subsequent chunks append to arguments.
	tcMap := make(map[int]*openai.ToolCall)

	for token := range tokens {
		if token.FinishReason == "error" {
			result.FinishReason = "error"
			result.FullResponse = token.Content
			return result
		}

		result.FullResponse += token.Content

		// Merge tool call deltas by index
		for _, tc := range token.ToolCalls {
			mergeToolCallDelta(tcMap, tc)
		}

		if token.FinishReason != "" {
			result.FinishReason = token.FinishReason
		}

		if token.Timings != nil {
			result.CacheTokens = token.Timings.CacheTokens
			result.InputTokens = token.Timings.PromptTokens
			result.OutputTokens = token.Timings.PredictedTokens
			result.DurationMS = token.Timings.PredictedMS + token.Timings.PromptMS
		}
	}

	result.ToolCalls = sortedToolCalls(tcMap)
	return result
}

// mergeToolCallDelta merges a streaming tool call delta into the accumulator map.
// The first delta for a given index carries id/type/name; subsequent deltas
// only carry argument fragments that must be concatenated.
func mergeToolCallDelta(m map[int]*openai.ToolCall, delta openai.ToolCall) {
	existing, ok := m[delta.Index]
	if !ok {
		copy := delta
		m[delta.Index] = &copy
		return
	}
	if delta.ID != "" {
		existing.ID = delta.ID
	}
	if delta.Type != "" {
		existing.Type = delta.Type
	}
	if delta.Function.Name != "" {
		existing.Function.Name = delta.Function.Name
	}
	existing.Function.Arguments += delta.Function.Arguments
}

// sortedToolCalls converts the index→ToolCall map to a slice sorted by index.
func sortedToolCalls(m map[int]*openai.ToolCall) []openai.ToolCall {
	if len(m) == 0 {
		return nil
	}
	sorted := make([]int, 0, len(m))
	for idx := range m {
		sorted = append(sorted, idx)
	}
	sort.Ints(sorted)
	out := make([]openai.ToolCall, 0, len(sorted))
	for _, idx := range sorted {
		out = append(out, *m[idx])
	}
	return out
}
