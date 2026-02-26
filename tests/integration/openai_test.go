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

	"github.com/auxothq/auxot/pkg/openai"
)

func TestOpenAI_ChatCompletions_NonStreaming(t *testing.T) {
	env := setupTestEnv(t)

	worker := newMockWorker()
	worker.connect(t, env)
	defer worker.close()

	waitForWorker(t, env, 5e9) // 5s

	// Build request
	reqBody := openai.ChatCompletionRequest{
		Model: "test-model",
		Messages: []openai.Message{
			{Role: "user", Content: openai.MessageContentString("Say hello in exactly 5 words.")},
		},
		Stream: false,
	}
	body, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/chat/completions", bytes.NewReader(body))
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

	var result openai.ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// Verify response structure
	if result.Object != "chat.completion" {
		t.Errorf("expected object=chat.completion, got %q", result.Object)
	}
	if !strings.HasPrefix(result.ID, "chatcmpl-") {
		t.Errorf("expected ID prefix chatcmpl-, got %q", result.ID)
	}
	if len(result.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(result.Choices))
	}
	if result.Choices[0].Message == nil {
		t.Fatal("expected message in choice, got nil")
	}
	if result.Choices[0].Message.Content != "Hello from the GPU!" {
		t.Errorf("expected content %q, got %q", "Hello from the GPU!", result.Choices[0].Message.Content)
	}
	if result.Choices[0].FinishReason == nil || *result.Choices[0].FinishReason != "stop" {
		t.Errorf("expected finish_reason=stop, got %v", result.Choices[0].FinishReason)
	}
	if result.Usage == nil {
		t.Error("expected usage, got nil")
	} else {
		if result.Usage.PromptTokens != 10 {
			t.Errorf("expected prompt_tokens=10, got %d", result.Usage.PromptTokens)
		}
		if result.Usage.CompletionTokens != 5 {
			t.Errorf("expected completion_tokens=5, got %d", result.Usage.CompletionTokens)
		}
	}
}

func TestOpenAI_ChatCompletions_Streaming(t *testing.T) {
	env := setupTestEnv(t)

	worker := newMockWorker()
	worker.connect(t, env)
	defer worker.close()

	waitForWorker(t, env, 5e9)

	reqBody := openai.ChatCompletionRequest{
		Model: "test-model",
		Messages: []openai.Message{
			{Role: "user", Content: openai.MessageContentString("Count to five.")},
		},
		Stream: true,
	}
	body, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/chat/completions", bytes.NewReader(body))
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

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("expected Content-Type=text/event-stream, got %q", ct)
	}

	// Parse SSE stream
	scanner := bufio.NewScanner(resp.Body)
	var chunks []openai.ChatCompletionChunk
	var gotDone bool

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			gotDone = true
			break
		}

		var chunk openai.ChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("parsing SSE chunk: %v (data: %s)", err, data)
		}
		chunks = append(chunks, chunk)
	}

	if !gotDone {
		t.Error("never received [DONE] sentinel")
	}

	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks (role + content), got %d", len(chunks))
	}

	// First chunk should set the role
	if chunks[0].Choices[0].Delta == nil || chunks[0].Choices[0].Delta.Role != "assistant" {
		t.Error("first chunk should set role=assistant")
	}

	// Concatenate content deltas
	var content strings.Builder
	for _, c := range chunks {
		if c.Choices[0].Delta != nil {
			content.WriteString(c.Choices[0].Delta.Content)
		}
	}

	expected := "Hello from the GPU!"
	if content.String() != expected {
		t.Errorf("expected streamed content %q, got %q", expected, content.String())
	}
}

func TestOpenAI_ChatCompletions_Unauthorized(t *testing.T) {
	env := setupTestEnv(t)

	reqBody := openai.ChatCompletionRequest{
		Model:    "test-model",
		Messages: []openai.Message{{Role: "user", Content: openai.MessageContentString("hello")}},
	}
	body, _ := json.Marshal(reqBody)

	// No auth header
	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/chat/completions", bytes.NewReader(body))
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

func TestOpenAI_ChatCompletions_BadKey(t *testing.T) {
	env := setupTestEnv(t)

	reqBody := openai.ChatCompletionRequest{
		Model:    "test-model",
		Messages: []openai.Message{{Role: "user", Content: openai.MessageContentString("hello")}},
	}
	body, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer rtr_invalidkey1234567890abcdef")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func TestOpenAI_ChatCompletions_EmptyMessages(t *testing.T) {
	env := setupTestEnv(t)

	reqBody := `{"model":"test","messages":[]}`

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestOpenAI_Health(t *testing.T) {
	env := setupTestEnv(t)

	resp, err := http.Get(env.baseURL() + "/health")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var health map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&health)

	if health["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", health["status"])
	}
	if health["workers"].(float64) != 0 {
		t.Errorf("expected 0 workers, got %v", health["workers"])
	}
}

// --- Legacy Completions (POST /api/openai/completions) ---

func TestOpenAI_Completions_NonStreaming(t *testing.T) {
	env := setupTestEnv(t)

	worker := newMockWorker()
	worker.connect(t, env)
	defer worker.close()

	waitForWorker(t, env, 5e9)

	// Legacy completions use "prompt" instead of "messages"
	reqBody := `{"model":"test-model","prompt":"Say this is a test","max_tokens":50}`

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/completions", strings.NewReader(reqBody))
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

	var result openai.CompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// Verify legacy response structure
	if result.Object != "text_completion" {
		t.Errorf("expected object=text_completion, got %q", result.Object)
	}
	if !strings.HasPrefix(result.ID, "cmpl-") {
		t.Errorf("expected ID prefix cmpl-, got %q", result.ID)
	}
	if len(result.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(result.Choices))
	}
	if result.Choices[0].Text != "Hello from the GPU!" {
		t.Errorf("expected text %q, got %q", "Hello from the GPU!", result.Choices[0].Text)
	}
	if result.Choices[0].FinishReason != "stop" {
		t.Errorf("expected finish_reason=stop, got %q", result.Choices[0].FinishReason)
	}
	if result.Usage == nil {
		t.Error("expected usage, got nil")
	} else {
		if result.Usage.PromptTokens != 10 {
			t.Errorf("expected prompt_tokens=10, got %d", result.Usage.PromptTokens)
		}
	}
}

func TestOpenAI_Completions_ArrayPrompt(t *testing.T) {
	env := setupTestEnv(t)

	worker := newMockWorker()
	worker.connect(t, env)
	defer worker.close()

	waitForWorker(t, env, 5e9)

	// Prompt as an array of strings — should use first element
	reqBody := `{"model":"test-model","prompt":["First prompt","Second prompt"]}`

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/completions", strings.NewReader(reqBody))
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

	var result openai.CompletionResponse
	json.NewDecoder(resp.Body).Decode(&result)

	if result.Object != "text_completion" {
		t.Errorf("expected object=text_completion, got %q", result.Object)
	}
}

func TestOpenAI_Completions_EmptyPrompt(t *testing.T) {
	env := setupTestEnv(t)

	reqBody := `{"model":"test-model","prompt":""}`

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for empty prompt, got %d", resp.StatusCode)
	}
}

// --- Models ---

func TestOpenAI_Models_List(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest("GET", env.baseURL()+"/api/openai/models", nil)
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

	var result openai.ModelListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if result.Object != "list" {
		t.Errorf("expected object=list, got %q", result.Object)
	}
	if len(result.Data) != 1 {
		t.Fatalf("expected 1 model, got %d", len(result.Data))
	}
	if result.Data[0].ID != "test-model" {
		t.Errorf("expected model ID=test-model, got %q", result.Data[0].ID)
	}
	if result.Data[0].Object != "model" {
		t.Errorf("expected object=model, got %q", result.Data[0].Object)
	}
	if result.Data[0].OwnedBy != "auxot" {
		t.Errorf("expected owned_by=auxot, got %q", result.Data[0].OwnedBy)
	}
}

func TestOpenAI_Models_Get(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest("GET", env.baseURL()+"/api/openai/models/test-model", nil)
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

	var result openai.ModelObject
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if result.ID != "test-model" {
		t.Errorf("expected ID=test-model, got %q", result.ID)
	}
	if result.Object != "model" {
		t.Errorf("expected object=model, got %q", result.Object)
	}
}

func TestOpenAI_Models_GetNotFound(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest("GET", env.baseURL()+"/api/openai/models/nonexistent-model", nil)
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}

	var errResp openai.ErrorResponse
	json.NewDecoder(resp.Body).Decode(&errResp)

	if errResp.Error.Type != "not_found_error" {
		t.Errorf("expected error type=not_found_error, got %q", errResp.Error.Type)
	}
}

func TestOpenAI_Models_Unauthorized(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest("GET", env.baseURL()+"/api/openai/models", nil)
	// No auth header

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

// --- Stored completions stubs ---

func TestOpenAI_ListChatCompletions_EmptyList(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest("GET", env.baseURL()+"/api/openai/chat/completions", nil)
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

	var result openai.ListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if result.Object != "list" {
		t.Errorf("expected object=list, got %q", result.Object)
	}
	if len(result.Data) != 0 {
		t.Errorf("expected 0 data items, got %d", len(result.Data))
	}
	if result.HasMore {
		t.Error("expected has_more=false")
	}
	if result.FirstID != nil {
		t.Errorf("expected first_id=null, got %v", result.FirstID)
	}
	if result.LastID != nil {
		t.Errorf("expected last_id=null, got %v", result.LastID)
	}
}

func TestOpenAI_GetChatCompletion_NotFound(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest("GET", env.baseURL()+"/api/openai/chat/completions/chatcmpl-abc123", nil)
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestOpenAI_DeleteChatCompletion_BadRequest(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest("DELETE", env.baseURL()+"/api/openai/chat/completions/chatcmpl-abc123", nil)
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestOpenAI_GetChatCompletionMessages_NotFound(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest("GET", env.baseURL()+"/api/openai/chat/completions/chatcmpl-abc123/messages", nil)
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// --- Embeddings (stub) ---

func TestOpenAI_Embeddings_NotImplemented(t *testing.T) {
	env := setupTestEnv(t)

	reqBody := `{"input":"Hello world","model":"text-embedding-ada-002"}`

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/embeddings", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("expected 501, got %d", resp.StatusCode)
	}

	var errResp openai.ErrorResponse
	json.NewDecoder(resp.Body).Decode(&errResp)

	if errResp.Error.Type != "not_implemented" {
		t.Errorf("expected error type=not_implemented, got %q", errResp.Error.Type)
	}
}

// --- Catch-all ---

func TestOpenAI_CatchAll_NotImplemented(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest("GET", env.baseURL()+"/api/openai/some/unknown/endpoint", nil)
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("expected 501, got %d", resp.StatusCode)
	}
}
