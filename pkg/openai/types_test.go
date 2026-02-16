package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatCompletionRequest_Parse(t *testing.T) {
	raw := `{
		"model": "qwen2.5-7b",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "Hello!"}
		],
		"stream": true,
		"temperature": 0.7,
		"max_tokens": 2048
	}`

	var req ChatCompletionRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("parsing request: %v", err)
	}

	if req.Model != "qwen2.5-7b" {
		t.Errorf("model: got %q, want %q", req.Model, "qwen2.5-7b")
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages count: got %d, want 2", len(req.Messages))
	}
	if req.Messages[0].Role != "system" {
		t.Errorf("first message role: got %q, want %q", req.Messages[0].Role, "system")
	}
	if !req.Stream {
		t.Error("stream should be true")
	}
	if req.Temperature == nil || *req.Temperature != 0.7 {
		t.Error("temperature should be 0.7")
	}
	if req.MaxTokens == nil || *req.MaxTokens != 2048 {
		t.Error("max_tokens should be 2048")
	}
}

func TestChatCompletionRequest_WithTools(t *testing.T) {
	raw := `{
		"messages": [{"role": "user", "content": "What is the weather?"}],
		"tools": [{
			"type": "function",
			"function": {
				"name": "get_weather",
				"description": "Get weather for a location",
				"parameters": {
					"type": "object",
					"properties": {"location": {"type": "string"}},
					"required": ["location"]
				}
			}
		}]
	}`

	var req ChatCompletionRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("parsing: %v", err)
	}

	if len(req.Tools) != 1 {
		t.Fatalf("tools count: got %d, want 1", len(req.Tools))
	}
	if req.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tool name: got %q, want %q", req.Tools[0].Function.Name, "get_weather")
	}
}

func TestChatCompletionRequest_MinimalValid(t *testing.T) {
	// The absolute minimum: just messages
	raw := `{"messages": [{"role": "user", "content": "Hi"}]}`

	var req ChatCompletionRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("parsing: %v", err)
	}

	if len(req.Messages) != 1 {
		t.Fatalf("messages: got %d, want 1", len(req.Messages))
	}
	if req.Stream {
		t.Error("stream should default to false")
	}
	if req.Temperature != nil {
		t.Error("temperature should be nil (omitted)")
	}
}

func TestNonStreamingResponse_Marshal(t *testing.T) {
	usage := &Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30}
	resp := NewNonStreamingResponse("test-model", "Hello, world!", "stop", usage)

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}

	// Verify key fields are present
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("re-parsing: %v", err)
	}

	if parsed["object"] != "chat.completion" {
		t.Errorf("object: got %v, want %q", parsed["object"], "chat.completion")
	}
	if !strings.HasPrefix(parsed["id"].(string), "chatcmpl-") {
		t.Errorf("id should start with chatcmpl-, got %q", parsed["id"])
	}

	choices := parsed["choices"].([]interface{})
	if len(choices) != 1 {
		t.Fatalf("choices count: got %d, want 1", len(choices))
	}

	choice := choices[0].(map[string]interface{})
	msg := choice["message"].(map[string]interface{})
	if msg["role"] != "assistant" {
		t.Errorf("role: got %v, want %q", msg["role"], "assistant")
	}
	if msg["content"] != "Hello, world!" {
		t.Errorf("content: got %v, want %q", msg["content"], "Hello, world!")
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason: got %v, want %q", choice["finish_reason"], "stop")
	}
}

func TestToolCallResponse_Marshal(t *testing.T) {
	calls := []ToolCall{
		{
			ID:   "call_123",
			Type: "function",
			Function: ToolCallFunction{
				Name:      "get_weather",
				Arguments: `{"location":"San Francisco"}`,
			},
		},
	}

	resp := NewToolCallResponse("test-model", calls, nil)
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("re-parsing: %v", err)
	}

	choices := parsed["choices"].([]interface{})
	choice := choices[0].(map[string]interface{})
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason: got %v, want %q", choice["finish_reason"], "tool_calls")
	}

	msg := choice["message"].(map[string]interface{})
	toolCalls := msg["tool_calls"].([]interface{})
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls count: got %d, want 1", len(toolCalls))
	}
}

func TestStreamingChunk_Marshal(t *testing.T) {
	id := NewCompletionID()
	chunk := NewStreamingChunk(id, "test-model", "Hello")

	data, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("re-parsing: %v", err)
	}

	if parsed["object"] != "chat.completion.chunk" {
		t.Errorf("object: got %v, want %q", parsed["object"], "chat.completion.chunk")
	}

	choices := parsed["choices"].([]interface{})
	choice := choices[0].(map[string]interface{})
	delta := choice["delta"].(map[string]interface{})
	if delta["content"] != "Hello" {
		t.Errorf("delta content: got %v, want %q", delta["content"], "Hello")
	}

	// finish_reason should be null (JSON null)
	if choice["finish_reason"] != nil {
		t.Errorf("finish_reason should be null, got %v", choice["finish_reason"])
	}
}

func TestStreamingRoleChunk(t *testing.T) {
	chunk := NewStreamingRoleChunk("chatcmpl-test", "model")
	data, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}

	var parsed map[string]interface{}
	_ = json.Unmarshal(data, &parsed)

	choices := parsed["choices"].([]interface{})
	choice := choices[0].(map[string]interface{})
	delta := choice["delta"].(map[string]interface{})

	if delta["role"] != "assistant" {
		t.Errorf("role: got %v, want %q", delta["role"], "assistant")
	}
}

func TestStreamingDoneChunk(t *testing.T) {
	chunk := NewStreamingDoneChunk("chatcmpl-test", "model", "stop", nil)
	data, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}

	var parsed map[string]interface{}
	_ = json.Unmarshal(data, &parsed)

	choices := parsed["choices"].([]interface{})
	choice := choices[0].(map[string]interface{})

	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason: got %v, want %q", choice["finish_reason"], "stop")
	}
}

func TestFormatSSE(t *testing.T) {
	chunk := NewStreamingChunk("chatcmpl-abc", "model", "Hi")
	sse, err := FormatSSE(chunk)
	if err != nil {
		t.Fatalf("formatting SSE: %v", err)
	}

	s := string(sse)
	if !strings.HasPrefix(s, "data: ") {
		t.Errorf("SSE should start with 'data: ', got %q", s[:10])
	}
	if !strings.HasSuffix(s, "\n\n") {
		t.Error("SSE should end with double newline")
	}

	// Verify the JSON inside is valid
	jsonPart := strings.TrimPrefix(s, "data: ")
	jsonPart = strings.TrimSuffix(jsonPart, "\n\n")
	var parsed ChatCompletionChunk
	if err := json.Unmarshal([]byte(jsonPart), &parsed); err != nil {
		t.Fatalf("SSE payload is invalid JSON: %v", err)
	}
}

func TestFormatSSEDone(t *testing.T) {
	done := FormatSSEDone()
	if string(done) != "data: [DONE]\n\n" {
		t.Errorf("done event: got %q, want %q", string(done), "data: [DONE]\n\n")
	}
}

func TestNewCompletionID_Format(t *testing.T) {
	id := NewCompletionID()
	if !strings.HasPrefix(id, "chatcmpl-") {
		t.Errorf("ID should start with chatcmpl-, got %q", id)
	}
	// "chatcmpl-" (9 chars) + 24 hex chars = 33 total
	if len(id) != 33 {
		t.Errorf("ID length should be 33, got %d", len(id))
	}
}

func TestNewCompletionID_Unique(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := NewCompletionID()
		if ids[id] {
			t.Fatalf("duplicate ID generated: %q", id)
		}
		ids[id] = true
	}
}

func TestErrorResponse_Marshal(t *testing.T) {
	resp := NewErrorResponse("Invalid API key", "authentication_error", "invalid_api_key")
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}

	var parsed map[string]interface{}
	_ = json.Unmarshal(data, &parsed)

	errObj := parsed["error"].(map[string]interface{})
	if errObj["message"] != "Invalid API key" {
		t.Errorf("message: got %v", errObj["message"])
	}
	if errObj["type"] != "authentication_error" {
		t.Errorf("type: got %v", errObj["type"])
	}
}

func TestRequest_UnknownFieldsIgnored(t *testing.T) {
	// Go's encoding/json ignores unknown fields by default — extensible protocol!
	raw := `{
		"messages": [{"role": "user", "content": "Hi"}],
		"future_field": "should be ignored",
		"another_new_thing": 42
	}`

	var req ChatCompletionRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("parsing with unknown fields should succeed: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Errorf("messages count: got %d, want 1", len(req.Messages))
	}
}
