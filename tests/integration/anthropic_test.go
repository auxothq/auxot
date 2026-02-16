//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/auxothq/auxot/pkg/anthropic"
)

// ---------------------------------------------------------------------------
// POST /api/anthropic/v1/messages — non-streaming
// ---------------------------------------------------------------------------

func TestAnthropic_Messages_NonStreaming(t *testing.T) {
	env := setupTestEnv(t)

	worker := newMockWorker()
	worker.connect(t, env)
	defer worker.close()

	waitForWorker(t, env, 5e9)

	reqBody := anthropic.MessagesRequest{
		Model:     "test-model",
		MaxTokens: 100,
		Messages: []anthropic.Message{
			anthropic.NewTextMessage("user", "Say hello in exactly 5 words."),
		},
	}
	body, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	var result anthropic.MessagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// Verify Anthropic response structure
	if result.Type != "message" {
		t.Errorf("expected type=message, got %q", result.Type)
	}
	if result.Role != "assistant" {
		t.Errorf("expected role=assistant, got %q", result.Role)
	}
	if !strings.HasPrefix(result.ID, "msg_") {
		t.Errorf("expected ID prefix msg_, got %q", result.ID)
	}
	if len(result.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(result.Content))
	}
	if result.Content[0].Type != "text" {
		t.Errorf("expected content type=text, got %q", result.Content[0].Type)
	}
	if result.Content[0].Text != "Hello from the GPU!" {
		t.Errorf("expected text %q, got %q", "Hello from the GPU!", result.Content[0].Text)
	}
	if result.StopReason == nil || *result.StopReason != "end_turn" {
		t.Errorf("expected stop_reason=end_turn, got %v", result.StopReason)
	}
	if result.Usage.InputTokens != 10 {
		t.Errorf("expected input_tokens=10, got %d", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 5 {
		t.Errorf("expected output_tokens=5, got %d", result.Usage.OutputTokens)
	}
}

// ---------------------------------------------------------------------------
// POST /api/anthropic/v1/messages — streaming
// ---------------------------------------------------------------------------

func TestAnthropic_Messages_Streaming(t *testing.T) {
	env := setupTestEnv(t)

	worker := newMockWorker()
	worker.connect(t, env)
	defer worker.close()

	waitForWorker(t, env, 5e9)

	reqBody := anthropic.MessagesRequest{
		Model:     "test-model",
		MaxTokens: 100,
		Messages: []anthropic.Message{
			anthropic.NewTextMessage("user", "Count to five."),
		},
		Stream: true,
	}
	body, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("expected Content-Type=text/event-stream, got %q", ct)
	}

	// Parse Anthropic SSE stream
	scanner := bufio.NewScanner(resp.Body)
	var eventTypes []string
	var content strings.Builder
	var gotMessageStop bool

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			eventType := strings.TrimPrefix(line, "event: ")
			eventTypes = append(eventTypes, eventType)

			if eventType == "message_stop" {
				gotMessageStop = true
			}
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var event map[string]interface{}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}

			// Extract content from content_block_delta events
			if event["type"] == "content_block_delta" {
				if delta, ok := event["delta"].(map[string]interface{}); ok {
					if text, ok := delta["text"].(string); ok {
						content.WriteString(text)
					}
				}
			}
		}
	}

	if !gotMessageStop {
		t.Error("never received message_stop event")
	}

	// Verify event sequence
	// content_block_start is now lazy — sent on first token, after ping
	expectedEvents := []string{
		"message_start",
		"ping",
		"content_block_start",
	}
	for i, expected := range expectedEvents {
		if i >= len(eventTypes) {
			t.Fatalf("missing event at index %d: expected %q", i, expected)
		}
		if eventTypes[i] != expected {
			t.Errorf("event[%d]: expected %q, got %q", i, expected, eventTypes[i])
		}
	}

	// Verify we got content_block_delta events
	hasDelta := false
	for _, et := range eventTypes {
		if et == "content_block_delta" {
			hasDelta = true
			break
		}
	}
	if !hasDelta {
		t.Error("no content_block_delta events received")
	}

	// Verify content
	expected := "Hello from the GPU!"
	if content.String() != expected {
		t.Errorf("expected streamed content %q, got %q", expected, content.String())
	}
}

// ---------------------------------------------------------------------------
// Auth: Bearer token also works (convenience)
// ---------------------------------------------------------------------------

func TestAnthropic_Messages_BearerAuth(t *testing.T) {
	env := setupTestEnv(t)

	worker := newMockWorker()
	worker.connect(t, env)
	defer worker.close()

	waitForWorker(t, env, 5e9)

	reqBody := anthropic.MessagesRequest{
		Model:     "test-model",
		MaxTokens: 100,
		Messages: []anthropic.Message{
			anthropic.NewTextMessage("user", "Hello"),
		},
	}
	body, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}
}

// ---------------------------------------------------------------------------
// System message
// ---------------------------------------------------------------------------

func TestAnthropic_Messages_SystemMessage(t *testing.T) {
	env := setupTestEnv(t)

	worker := newMockWorker()
	worker.connect(t, env)
	defer worker.close()

	waitForWorker(t, env, 5e9)

	reqBody := anthropic.MessagesRequest{
		Model:     "test-model",
		MaxTokens: 100,
		System:    json.RawMessage(`"You are a pirate."`),
		Messages: []anthropic.Message{
			anthropic.NewTextMessage("user", "Hello"),
		},
	}
	body, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	var result anthropic.MessagesResponse
	json.NewDecoder(resp.Body).Decode(&result)

	if result.Type != "message" {
		t.Errorf("expected type=message, got %q", result.Type)
	}
}

// ---------------------------------------------------------------------------
// Validation errors
// ---------------------------------------------------------------------------

func TestAnthropic_Messages_MissingMaxTokens(t *testing.T) {
	env := setupTestEnv(t)

	reqBody := `{"model":"test","messages":[{"role":"user","content":"hello"}]}`

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/anthropic/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing max_tokens, got %d", resp.StatusCode)
	}
}

func TestAnthropic_Messages_MissingModel(t *testing.T) {
	env := setupTestEnv(t)

	reqBody := `{"max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/anthropic/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing model, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Auth: Unauthorized
// ---------------------------------------------------------------------------

func TestAnthropic_Messages_Unauthorized(t *testing.T) {
	env := setupTestEnv(t)

	reqBody := `{"model":"test","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/anthropic/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	// No auth header

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}

	var errResp anthropic.ErrorResponse
	json.NewDecoder(resp.Body).Decode(&errResp)

	if errResp.Type != "error" {
		t.Errorf("expected type=error, got %q", errResp.Type)
	}
	if errResp.Error.Type != "authentication_error" {
		t.Errorf("expected error type=authentication_error, got %q", errResp.Error.Type)
	}
	if errResp.RequestID == "" {
		t.Error("expected request_id to be set")
	}
	if !strings.HasPrefix(errResp.RequestID, "req_") {
		t.Errorf("expected request_id prefix req_, got %q", errResp.RequestID)
	}
}

// ---------------------------------------------------------------------------
// Error response format
// ---------------------------------------------------------------------------

func TestAnthropic_ErrorFormat(t *testing.T) {
	// Verify all Anthropic error responses match the spec:
	// { "type": "error", "error": { "type": "...", "message": "..." }, "request_id": "req_..." }
	env := setupTestEnv(t)

	cases := []struct {
		name           string
		method         string
		path           string
		body           string
		auth           bool
		expectedStatus int
		expectedErrType string
	}{
		{"unauthed", "POST", "/api/anthropic/v1/messages",
			`{"model":"test","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`,
			false, 401, "authentication_error"},
		{"missing messages", "POST", "/api/anthropic/v1/messages",
			`{"model":"test","max_tokens":100,"messages":[]}`,
			true, 400, "invalid_request_error"},
		{"missing max_tokens", "POST", "/api/anthropic/v1/messages",
			`{"model":"test","messages":[{"role":"user","content":"hi"}]}`,
			true, 400, "invalid_request_error"},
		{"batch stub", "POST", "/api/anthropic/v1/messages/batches",
			`{}`, true, 501, "not_implemented_error"},
		{"unknown endpoint", "GET", "/api/anthropic/v1/files",
			"", true, 501, "not_implemented_error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			req, _ := http.NewRequest(tc.method, env.baseURL()+tc.path, body)
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			if tc.auth {
				req.Header.Set("x-api-key", env.apiKey)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.expectedStatus {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("expected %d, got %d: %s", tc.expectedStatus, resp.StatusCode, string(b))
			}

			// Verify error response shape
			var errResp anthropic.ErrorResponse
			b, _ := io.ReadAll(resp.Body)
			if err := json.Unmarshal(b, &errResp); err != nil {
				t.Fatalf("error response is not valid JSON: %v\nBody: %s", err, string(b))
			}

			if errResp.Type != "error" {
				t.Errorf("expected type=error, got %q", errResp.Type)
			}
			if errResp.Error.Type != tc.expectedErrType {
				t.Errorf("expected error.type=%q, got %q", tc.expectedErrType, errResp.Error.Type)
			}
			if errResp.Error.Message == "" {
				t.Error("expected non-empty error.message")
			}
			if errResp.RequestID == "" {
				t.Error("expected non-empty request_id")
			}
			if !strings.HasPrefix(errResp.RequestID, "req_") {
				t.Errorf("expected request_id prefix req_, got %q", errResp.RequestID)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// POST /api/anthropic/v1/messages/count_tokens
// ---------------------------------------------------------------------------

func TestAnthropic_CountTokens(t *testing.T) {
	env := setupTestEnv(t)

	reqBody := anthropic.CountTokensRequest{
		Model: "test-model",
		Messages: []anthropic.Message{
			anthropic.NewTextMessage("user", "Hello, how are you doing today?"),
		},
	}
	body, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/anthropic/v1/messages/count_tokens", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	var result anthropic.CountTokensResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// We use a heuristic (chars/4 + per-message overhead), so just verify it's
	// positive and roughly in the right range.
	if result.InputTokens < 1 {
		t.Errorf("expected input_tokens > 0, got %d", result.InputTokens)
	}
	if result.InputTokens > 100 {
		t.Errorf("expected input_tokens < 100 for short message, got %d", result.InputTokens)
	}

	t.Logf("count_tokens estimate: %d tokens for %q", result.InputTokens, "Hello, how are you doing today?")
}

func TestAnthropic_CountTokens_WithSystem(t *testing.T) {
	env := setupTestEnv(t)

	reqBody := anthropic.CountTokensRequest{
		Model:  "test-model",
		System: json.RawMessage(`"You are a helpful assistant."`),
		Messages: []anthropic.Message{
			anthropic.NewTextMessage("user", "Hello"),
		},
	}
	body, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/anthropic/v1/messages/count_tokens", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	var result anthropic.CountTokensResponse
	json.NewDecoder(resp.Body).Decode(&result)

	// Should be higher than without system message
	if result.InputTokens < 5 {
		t.Errorf("expected input_tokens >= 5 with system message, got %d", result.InputTokens)
	}
}

func TestAnthropic_CountTokens_Unauthorized(t *testing.T) {
	env := setupTestEnv(t)

	reqBody := `{"model":"test","messages":[{"role":"user","content":"hello"}]}`

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/anthropic/v1/messages/count_tokens", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// GET /api/anthropic/v1/models — list models
// ---------------------------------------------------------------------------

func TestAnthropic_ListModels(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest("GET", env.baseURL()+"/api/anthropic/v1/models", nil)
	req.Header.Set("x-api-key", env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	var result anthropic.ModelListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(result.Data) != 1 {
		t.Fatalf("expected 1 model, got %d", len(result.Data))
	}

	model := result.Data[0]
	if model.ID != "test-model" {
		t.Errorf("expected model ID=test-model, got %q", model.ID)
	}
	if model.Type != "model" {
		t.Errorf("expected type=model, got %q", model.Type)
	}
	if model.DisplayName == "" {
		t.Error("expected non-empty display_name")
	}
	if model.CreatedAt == "" {
		t.Error("expected non-empty created_at")
	}
	if result.HasMore {
		t.Error("expected has_more=false")
	}
	if result.FirstID == nil || *result.FirstID != "test-model" {
		t.Errorf("expected first_id=test-model, got %v", result.FirstID)
	}
	if result.LastID == nil || *result.LastID != "test-model" {
		t.Errorf("expected last_id=test-model, got %v", result.LastID)
	}
}

// ---------------------------------------------------------------------------
// GET /api/anthropic/v1/models/{id} — get model
// ---------------------------------------------------------------------------

func TestAnthropic_GetModel(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest("GET", env.baseURL()+"/api/anthropic/v1/models/test-model", nil)
	req.Header.Set("x-api-key", env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	var model anthropic.ModelObject
	if err := json.NewDecoder(resp.Body).Decode(&model); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if model.ID != "test-model" {
		t.Errorf("expected id=test-model, got %q", model.ID)
	}
	if model.Type != "model" {
		t.Errorf("expected type=model, got %q", model.Type)
	}
}

func TestAnthropic_GetModel_NotFound(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest("GET", env.baseURL()+"/api/anthropic/v1/models/nonexistent", nil)
	req.Header.Set("x-api-key", env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}

	var errResp anthropic.ErrorResponse
	json.NewDecoder(resp.Body).Decode(&errResp)

	if errResp.Type != "error" {
		t.Errorf("expected type=error, got %q", errResp.Type)
	}
	if errResp.Error.Type != "not_found_error" {
		t.Errorf("expected error.type=not_found_error, got %q", errResp.Error.Type)
	}
}

// ---------------------------------------------------------------------------
// Batch endpoints — all return 501
// ---------------------------------------------------------------------------

func TestAnthropic_Batches_NotImplemented(t *testing.T) {
	env := setupTestEnv(t)

	cases := []struct {
		method string
		path   string
	}{
		{"POST", "/api/anthropic/v1/messages/batches"},
		{"GET", "/api/anthropic/v1/messages/batches"},
		{"GET", "/api/anthropic/v1/messages/batches/batch_123"},
		{"POST", "/api/anthropic/v1/messages/batches/batch_123/cancel"},
		{"DELETE", "/api/anthropic/v1/messages/batches/batch_123"},
		{"GET", "/api/anthropic/v1/messages/batches/batch_123/results"},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var body io.Reader
			if tc.method == "POST" {
				body = strings.NewReader(`{}`)
			}
			req, _ := http.NewRequest(tc.method, env.baseURL()+tc.path, body)
			if tc.method == "POST" {
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("x-api-key", env.apiKey)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusNotImplemented {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("expected 501, got %d: %s", resp.StatusCode, string(b))
			}

			var errResp anthropic.ErrorResponse
			json.NewDecoder(resp.Body).Decode(&errResp)

			if errResp.Error.Type != "not_implemented_error" {
				t.Errorf("expected error.type=not_implemented_error, got %q", errResp.Error.Type)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Catch-all — files, skills, etc.
// ---------------------------------------------------------------------------

func TestAnthropic_CatchAll_NotImplemented(t *testing.T) {
	env := setupTestEnv(t)

	cases := []struct {
		method string
		path   string
	}{
		{"GET", "/api/anthropic/v1/files"},
		{"POST", "/api/anthropic/v1/files"},
		{"GET", "/api/anthropic/v1/skills"},
		{"GET", "/api/anthropic/v1/something/unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var body io.Reader
			if tc.method == "POST" {
				body = strings.NewReader(`{}`)
			}
			req, _ := http.NewRequest(tc.method, env.baseURL()+tc.path, body)
			if tc.method == "POST" {
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("x-api-key", env.apiKey)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusNotImplemented {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("expected 501, got %d: %s", resp.StatusCode, string(b))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Content blocks: array-form message content
// ---------------------------------------------------------------------------

func TestAnthropic_Messages_ContentBlocksArray(t *testing.T) {
	// Verify that content as an array of text blocks is handled correctly
	env := setupTestEnv(t)

	worker := newMockWorker()
	worker.connect(t, env)
	defer worker.close()

	waitForWorker(t, env, 5e9)

	// Send content as array of text blocks (instead of plain string)
	reqBody := `{
		"model": "test-model",
		"max_tokens": 100,
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "Hello, "},
					{"type": "text", "text": "world!"}
				]
			}
		]
	}`

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/anthropic/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	var result anthropic.MessagesResponse
	json.NewDecoder(resp.Body).Decode(&result)

	if result.Type != "message" {
		t.Errorf("expected type=message, got %q", result.Type)
	}
}

// ---------------------------------------------------------------------------
// Models auth
// ---------------------------------------------------------------------------

func TestAnthropic_ListModels_Unauthorized(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest("GET", env.baseURL()+"/api/anthropic/v1/models", nil)
	// No auth

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}
