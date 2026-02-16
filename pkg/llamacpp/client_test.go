package llamacpp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/auxothq/auxot/pkg/openai"
)

// sseResponse builds an SSE body from data lines.
func sseResponse(lines ...string) string {
	var out string
	for _, line := range lines {
		out += "data: " + line + "\n\n"
	}
	return out
}

func TestStreamCompletion_BasicTokens(t *testing.T) {
	// Fake llama.cpp server that returns 3 tokens then [DONE]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("missing content-type header")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)

		chunks := []string{
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"timings":{"prompt_n":5,"prompt_ms":10,"predicted_n":2,"predicted_ms":20,"predicted_per_second":100}}`,
			"[DONE]",
		}

		body := sseResponse(chunks...)
		fmt.Fprint(w, body)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "user", Content: "Hi"},
		},
	}

	tokens, err := client.StreamCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("streaming: %v", err)
	}

	result := CollectStream(tokens)

	if result.FullResponse != "Hello world" {
		t.Errorf("full response: got %q, want %q", result.FullResponse, "Hello world")
	}
	if result.FinishReason != "stop" {
		t.Errorf("finish reason: got %q, want %q", result.FinishReason, "stop")
	}
	if result.InputTokens != 5 {
		t.Errorf("input tokens: got %d, want 5", result.InputTokens)
	}
	if result.OutputTokens != 2 {
		t.Errorf("output tokens: got %d, want 2", result.OutputTokens)
	}
}

func TestStreamCompletion_ToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)

		chunks := []string{
			// Role chunk
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			// Tool call chunk
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"location\":"}}]},"finish_reason":null}]}`,
			// More arguments
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"SF\"}"}}]},"finish_reason":null}]}`,
			// Done
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			"[DONE]",
		}

		body := sseResponse(chunks...)
		fmt.Fprint(w, body)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{{Role: "user", Content: "Weather?"}},
	}

	tokens, err := client.StreamCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("streaming: %v", err)
	}

	result := CollectStream(tokens)

	if result.FinishReason != "tool_calls" {
		t.Errorf("finish reason: got %q, want %q", result.FinishReason, "tool_calls")
	}
	if len(result.ToolCalls) == 0 {
		t.Fatal("should have tool calls")
	}
	if result.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("tool name: got %q, want %q", result.ToolCalls[0].Function.Name, "get_weather")
	}
}

func TestStreamCompletion_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":"model not loaded"}`)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{{Role: "user", Content: "Hi"}},
	}

	_, err := client.StreamCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestStreamCompletion_Cancellation(t *testing.T) {
	// Server that streams slowly
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("server doesn't support flushing")
		}

		// Send one token
		fmt.Fprintf(w, "data: %s\n\n",
			`{"id":"1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`)
		flusher.Flush()

		// Wait for client to cancel (context will be done)
		<-r.Context().Done()
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx, cancel := context.WithCancel(context.Background())

	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{{Role: "user", Content: "Hi"}},
	}

	tokens, err := client.StreamCompletion(ctx, req)
	if err != nil {
		t.Fatalf("starting stream: %v", err)
	}

	// Read first token
	token, ok := <-tokens
	if !ok {
		t.Fatal("expected at least one token")
	}
	if token.Content != "Hi" {
		t.Errorf("first token: got %q, want %q", token.Content, "Hi")
	}

	// Cancel and drain
	cancel()
	for range tokens {
		// drain remaining
	}
}

func TestStreamCompletion_EmptyStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := NewClient(server.URL)
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{{Role: "user", Content: "Hi"}},
	}

	tokens, err := client.StreamCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("streaming: %v", err)
	}

	result := CollectStream(tokens)
	if result.FullResponse != "" {
		t.Errorf("expected empty response, got %q", result.FullResponse)
	}
}

func TestCollectStream_ErrorToken(t *testing.T) {
	ch := make(chan StreamToken, 1)
	ch <- StreamToken{Content: "something went wrong", FinishReason: "error"}
	close(ch)

	result := CollectStream(ch)
	if result.FinishReason != "error" {
		t.Errorf("finish reason: got %q, want %q", result.FinishReason, "error")
	}
	if result.FullResponse != "something went wrong" {
		t.Errorf("response: got %q", result.FullResponse)
	}
}

func TestNewClient_TrailingSlash(t *testing.T) {
	c := NewClient("http://localhost:8080/")
	if c.baseURL != "http://localhost:8080" {
		t.Errorf("trailing slash should be stripped: got %q", c.baseURL)
	}
}
