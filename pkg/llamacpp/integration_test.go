package llamacpp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/auxothq/auxot/pkg/openai"
)

// TestClientSanitizesToolsBeforeSending verifies that StreamCompletion calls
// SanitizeTools, stripping property schemas to only the keys that llama.cpp's
// minja Jinja engine can handle (type, description, enum, required).
func TestClientSanitizesToolsBeforeSending(t *testing.T) {
	// Mock llama.cpp server that validates the request was sanitized
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}

		var req openai.ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Validate all tools have been stripped to allowed keys only
		allowedKeys := map[string]bool{
			"type": true, "description": true, "enum": true, "required": true,
		}

		for _, tool := range req.Tools {
			var params map[string]any
			if err := json.Unmarshal(tool.Function.Parameters, &params); err != nil {
				http.Error(w, "Invalid tool parameters: "+err.Error(), http.StatusBadRequest)
				return
			}

			// Top level should only have type, properties, required
			for key := range params {
				if key != "type" && key != "properties" && key != "required" {
					http.Error(w, "Top-level key should have been stripped: "+key, http.StatusInternalServerError)
					return
				}
			}

			// Each property should only have allowed keys
			if props, ok := params["properties"].(map[string]any); ok {
				for propName, propDef := range props {
					if propMap, ok := propDef.(map[string]any); ok {
						for key := range propMap {
							if !allowedKeys[key] {
								http.Error(w,
									"Property '"+propName+"' has disallowed key '"+key+"' — should have been stripped",
									http.StatusInternalServerError)
								return
							}
						}
					}
				}
			}
		}

		// Return a successful SSE stream
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		chunk := `{"id":"test","object":"chat.completion.chunk","created":0,"model":"test","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`
		w.Write([]byte("data: " + chunk + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer mockServer.Close()

	client := NewClient(mockServer.URL)

	// Send tools with extra keys that should be stripped
	req := &openai.ChatCompletionRequest{
		Model: "test",
		Messages: []openai.Message{
			{Role: "user", Content: openai.MessageContentString("test")},
		},
		Tools: []openai.Tool{
			{
				Type: "function",
				Function: openai.ToolFunction{
					Name:        "TaskCreate",
					Description: "Create a task",
					Parameters: json.RawMessage(`{
						"$schema": "https://json-schema.org/draft/2020-12/schema",
						"type": "object",
						"properties": {
							"subject": {"type": "string", "description": "Title", "minLength": 1},
							"count": {"type": "integer", "minimum": 0, "maximum": 100, "default": 5},
							"url": {"type": "string", "format": "uri"},
							"tags": {"type": "array", "items": {"type": "string"}, "minItems": 1},
							"metadata": {"type": "object", "propertyNames": {"type": "string"}, "additionalProperties": {}}
						},
						"required": ["subject"],
						"additionalProperties": false
					}`),
				},
			},
		},
		Stream: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tokens, err := client.StreamCompletion(ctx, req)
	if err != nil {
		t.Fatalf("StreamCompletion failed: %v", err)
	}

	// Drain the channel — if we get here without error, the server accepted the sanitized request
	for range tokens {
	}
}
