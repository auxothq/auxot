//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/auxothq/auxot/pkg/openai"
	"github.com/auxothq/auxot/pkg/protocol"
)

// ==========================================================================
// Auth boundary tests — Bearer token edge cases
// ==========================================================================

func TestSecurity_EmptyBearerToken(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/chat/completions",
		strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("empty bearer token: expected 401, got %d", resp.StatusCode)
	}
}

func TestSecurity_BearerTokenCaseSensitive(t *testing.T) {
	env := setupTestEnv(t)

	// "bearer" lowercase should still work (HTTP headers are case-insensitive
	// but the Authorization scheme parsing is our responsibility)
	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/chat/completions",
		strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// We currently require "Bearer " (capital B). This is consistent with
	// OpenAI's behavior. Lowercase "bearer" should be rejected.
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("lowercase bearer: expected 401, got %d", resp.StatusCode)
	}
}

func TestSecurity_NoAuthorizationHeader(t *testing.T) {
	env := setupTestEnv(t)

	// Hit every authed endpoint with no auth header
	endpoints := []struct {
		method string
		path   string
	}{
		{"POST", "/api/openai/chat/completions"},
		{"GET", "/api/openai/chat/completions"},
		{"GET", "/api/openai/chat/completions/chatcmpl-fake123"},
		{"DELETE", "/api/openai/chat/completions/chatcmpl-fake123"},
		{"GET", "/api/openai/chat/completions/chatcmpl-fake123/messages"},
		{"POST", "/api/openai/completions"},
		{"POST", "/api/openai/embeddings"},
		{"GET", "/api/openai/models"},
		{"GET", "/api/openai/models/test-model"},
		{"POST", "/api/anthropic/v1/messages"},
		{"POST", "/api/anthropic/v1/messages/count_tokens"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			var body io.Reader
			if ep.method == "POST" {
				body = strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
			}
			req, _ := http.NewRequest(ep.method, env.baseURL()+ep.path, body)
			if ep.method == "POST" {
				req.Header.Set("Content-Type", "application/json")
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("expected 401, got %d", resp.StatusCode)
			}
		})
	}
}

func TestSecurity_VeryLongAPIKey(t *testing.T) {
	env := setupTestEnv(t)

	// 10KB API key — should be rejected, not crash
	longKey := "rtr_" + strings.Repeat("A", 10240)

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/chat/completions",
		strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+longKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("very long key: expected 401, got %d", resp.StatusCode)
	}
}

func TestSecurity_AdminKeyAsAPIKey(t *testing.T) {
	env := setupTestEnv(t)

	// Admin key (adm_ prefix) should NOT work as an API key (rtr_ prefix)
	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/chat/completions",
		strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.adminKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("admin key as API key: expected 401, got %d", resp.StatusCode)
	}
}

func TestSecurity_APIKeyAsAdminKey(t *testing.T) {
	env := setupTestEnv(t)

	// API key should NOT work for WebSocket auth (admin key required)
	conn, resp, err := websocket.DefaultDialer.Dial(env.wsURL(), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}

	// Send hello with API key instead of admin key
	hello := protocol.HelloMessage{
		Type:   protocol.TypeHello,
		GPUKey: env.apiKey, // WRONG key type
		Capabilities: protocol.Capabilities{
			Backend:    "mock",
			Model:      "test-model",
			CtxSize:    4096,
			TotalSlots: 1,
		},
	}
	data, _ := json.Marshal(hello)
	conn.WriteMessage(websocket.TextMessage, data)

	// Server should reject — either as hello_ack with success=false or as error message
	_, respData, err := conn.ReadMessage()
	if err != nil {
		// Connection closed immediately — also a valid rejection
		t.Logf("connection closed (valid rejection): %v", err)
		return
	}

	msg, err := protocol.ParseMessage(respData)
	if err != nil {
		t.Fatalf("parsing response: %v", err)
	}

	switch m := msg.(type) {
	case protocol.HelloAckMessage:
		if m.Success {
			t.Error("API key should NOT authenticate as admin/GPU key")
		}
	case protocol.ErrorMessage:
		t.Logf("correctly rejected with error: %s", m.Error)
	default:
		t.Fatalf("expected hello_ack or error, got %T", msg)
	}
}

func TestSecurity_WSNoGPUKey(t *testing.T) {
	env := setupTestEnv(t)

	conn, _, err := websocket.DefaultDialer.Dial(env.wsURL(), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send hello with empty GPU key
	hello := protocol.HelloMessage{
		Type:   protocol.TypeHello,
		GPUKey: "",
		Capabilities: protocol.Capabilities{
			Backend:    "mock",
			Model:      "test-model",
			CtxSize:    4096,
			TotalSlots: 1,
		},
	}
	data, _ := json.Marshal(hello)
	conn.WriteMessage(websocket.TextMessage, data)

	_, respData, err := conn.ReadMessage()
	if err != nil {
		// Connection closed — valid rejection
		t.Logf("connection closed (valid rejection): %v", err)
		return
	}

	msg, _ := protocol.ParseMessage(respData)
	switch m := msg.(type) {
	case protocol.HelloAckMessage:
		if m.Success {
			t.Error("empty GPU key should NOT authenticate")
		}
	case protocol.ErrorMessage:
		t.Logf("correctly rejected with error: %s", m.Error)
	default:
		t.Fatalf("expected hello_ack or error, got %T", msg)
	}
}

func TestSecurity_WSWrongGPUKey(t *testing.T) {
	env := setupTestEnv(t)

	conn, _, err := websocket.DefaultDialer.Dial(env.wsURL(), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send hello with wrong GPU key
	hello := protocol.HelloMessage{
		Type:   protocol.TypeHello,
		GPUKey: "adm_totallyinvalid1234567890",
		Capabilities: protocol.Capabilities{
			Backend:    "mock",
			Model:      "test-model",
			CtxSize:    4096,
			TotalSlots: 1,
		},
	}
	data, _ := json.Marshal(hello)
	conn.WriteMessage(websocket.TextMessage, data)

	_, respData, err := conn.ReadMessage()
	if err != nil {
		// Connection closed — valid rejection
		t.Logf("connection closed (valid rejection): %v", err)
		return
	}

	msg, _ := protocol.ParseMessage(respData)
	switch m := msg.(type) {
	case protocol.HelloAckMessage:
		if m.Success {
			t.Error("wrong GPU key should NOT authenticate")
		}
	case protocol.ErrorMessage:
		t.Logf("correctly rejected with error: %s", m.Error)
	default:
		t.Fatalf("expected hello_ack or error, got %T", msg)
	}
}

// ==========================================================================
// Malformed input tests — JSON garbage, binary, truncated
// ==========================================================================

func TestSecurity_MalformedJSON(t *testing.T) {
	env := setupTestEnv(t)

	cases := []struct {
		name string
		body string
	}{
		{"not json at all", "this is not json"},
		{"truncated json", `{"model":"test","messages":[{"role":"user","content":"h`},
		{"xml instead", `<request><model>test</model></request>`},
		{"just a number", `42`},
		{"just a string", `"hello"`},
		{"just a boolean", `true`},
		{"null", `null`},
		{"empty object", `{}`},
		{"array instead of object", `[1,2,3]`},
		{"double encoded json", `"{\"model\":\"test\"}"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/chat/completions",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+env.apiKey)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			// Should get 400 (bad request), not 500 (server error) or crash
			if resp.StatusCode == http.StatusInternalServerError {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("got 500 (should be 400): %s", string(b))
			}
			if resp.StatusCode == http.StatusOK {
				t.Error("malformed JSON should not return 200")
			}
		})
	}
}

func TestSecurity_BinaryBody(t *testing.T) {
	env := setupTestEnv(t)

	// Send raw binary garbage
	garbage := make([]byte, 1024)
	for i := range garbage {
		garbage[i] = byte(i % 256)
	}

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/chat/completions",
		bytes.NewReader(garbage))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Must not crash — should return 400
	if resp.StatusCode == http.StatusInternalServerError {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("binary body caused 500: %s", string(b))
	}
}

func TestSecurity_EmptyBody(t *testing.T) {
	env := setupTestEnv(t)

	endpoints := []struct {
		method string
		path   string
	}{
		{"POST", "/api/openai/chat/completions"},
		{"POST", "/api/openai/completions"},
		// Embeddings excluded: returns 501 (stub) regardless of body — not an empty-body issue
		{"POST", "/api/anthropic/v1/messages"},
		{"POST", "/api/anthropic/v1/messages/count_tokens"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req, _ := http.NewRequest(ep.method, env.baseURL()+ep.path, nil)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+env.apiKey)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			// Must not return 200 (success) or crash (500)
			if resp.StatusCode == http.StatusOK {
				t.Error("empty body should not succeed")
			}
			if resp.StatusCode >= 500 {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("empty body caused %d: %s", resp.StatusCode, string(b))
			}
		})
	}
}

func TestSecurity_HugePayload(t *testing.T) {
	env := setupTestEnv(t)

	// Build a request with a very long message content (~1MB)
	longContent := strings.Repeat("A", 1_000_000)
	reqBody := openai.ChatCompletionRequest{
		Model: "test-model",
		Messages: []openai.Message{
			{Role: "user", Content: longContent},
		},
	}
	body, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/chat/completions",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	// Use a client with short timeout — we don't want this to actually process
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Timeout or connection reset is acceptable for huge payloads
		t.Logf("huge payload: connection error (acceptable): %v", err)
		return
	}
	defer resp.Body.Close()

	// Must not crash — any response status is fine as long as no 500
	t.Logf("huge payload: got %d", resp.StatusCode)
	if resp.StatusCode >= 500 {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("huge payload caused %d: %s", resp.StatusCode, string(b))
	}
}

func TestSecurity_DeeplyNestedJSON(t *testing.T) {
	env := setupTestEnv(t)

	// Build deeply nested JSON to test stack overflow / recursion depth
	// 100 levels of nesting: {"a":{"a":{"a":...}}}
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		sb.WriteString(`{"a":`)
	}
	sb.WriteString(`"deep"`)
	for i := 0; i < 100; i++ {
		sb.WriteString(`}`)
	}

	req, _ := http.NewRequest("POST", env.baseURL()+"/api/openai/chat/completions",
		strings.NewReader(sb.String()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Should get 400 (missing required fields), not crash
	if resp.StatusCode >= 500 {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("deeply nested JSON caused %d: %s", resp.StatusCode, string(b))
	}
}

// ==========================================================================
// HTTP method tests — wrong methods on endpoints
// ==========================================================================

func TestSecurity_WrongHTTPMethods(t *testing.T) {
	env := setupTestEnv(t)

	cases := []struct {
		method       string
		path         string
		expectStatus int // expected status (404 or 501 — NOT 200)
	}{
		// POST-only endpoints should reject GET/PUT/PATCH/DELETE
		{"GET", "/api/openai/completions", 501},
		{"PUT", "/api/openai/chat/completions", 501},
		{"PATCH", "/api/openai/chat/completions", 501},
		{"PUT", "/api/openai/completions", 501},
		{"PUT", "/api/openai/embeddings", 501},

		// GET-only endpoints should reject POST/PUT/DELETE
		{"POST", "/api/openai/models", 501},
		{"PUT", "/api/openai/models", 501},
		{"DELETE", "/api/openai/models", 501},
		{"POST", "/api/openai/models/test-model", 501},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var body io.Reader
			if tc.method == "POST" || tc.method == "PUT" || tc.method == "PATCH" {
				body = strings.NewReader(`{"test":true}`)
			}
			req, _ := http.NewRequest(tc.method, env.baseURL()+tc.path, body)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+env.apiKey)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				t.Errorf("wrong method %s should not return 200", tc.method)
			}
		})
	}
}

// ==========================================================================
// Path traversal and injection tests
// ==========================================================================

func TestSecurity_PathTraversalInModelID(t *testing.T) {
	env := setupTestEnv(t)

	attacks := []string{
		"../../../etc/passwd",
		"..%2F..%2F..%2Fetc%2Fpasswd",
		"....//....//etc/passwd",
		"%00",
		"test-model%00.evil",
		"<script>alert(1)</script>",
		"'; DROP TABLE models; --",
	}

	for _, attack := range attacks {
		t.Run(attack, func(t *testing.T) {
			req, err := http.NewRequest("GET", env.baseURL()+"/api/openai/models/"+attack, nil)
			if err != nil {
				// Go's http.NewRequest rejects URLs with null bytes — that's fine
				t.Logf("request creation rejected (acceptable): %v", err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+env.apiKey)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				// Connection error is acceptable
				t.Logf("path traversal %q: connection error (acceptable): %v", attack, err)
				return
			}
			defer resp.Body.Close()

			// Should be 404 (not found), never 200 with sensitive data
			if resp.StatusCode == http.StatusOK {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("path traversal %q returned 200: %s", attack, string(b))
			}
			// Must not crash
			if resp.StatusCode >= 500 {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("path traversal %q caused %d: %s", attack, resp.StatusCode, string(b))
			}
		})
	}
}

func TestSecurity_PathTraversalInCompletionID(t *testing.T) {
	env := setupTestEnv(t)

	attacks := []string{
		"../../../etc/passwd",
		"%00",
		"<script>alert(1)</script>",
		"chatcmpl-'; DROP TABLE; --",
	}

	for _, attack := range attacks {
		t.Run("GET "+attack, func(t *testing.T) {
			req, err := http.NewRequest("GET", env.baseURL()+"/api/openai/chat/completions/"+attack, nil)
			if err != nil {
				t.Logf("request creation rejected (acceptable): %v", err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+env.apiKey)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return // Connection error acceptable
			}
			defer resp.Body.Close()

			if resp.StatusCode >= 500 {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("path traversal in completion ID caused %d: %s", resp.StatusCode, string(b))
			}
		})

		t.Run("DELETE "+attack, func(t *testing.T) {
			req, err := http.NewRequest("DELETE", env.baseURL()+"/api/openai/chat/completions/"+attack, nil)
			if err != nil {
				t.Logf("request creation rejected (acceptable): %v", err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+env.apiKey)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode >= 500 {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("path traversal in DELETE caused %d: %s", resp.StatusCode, string(b))
			}
		})
	}
}

// ==========================================================================
// Error response format tests — verify errors always return proper JSON
// ==========================================================================

func TestSecurity_ErrorResponsesAreJSON(t *testing.T) {
	env := setupTestEnv(t)

	// Every error response should be valid JSON with OpenAI error structure
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		auth   bool
	}{
		{"unauthed chat", "POST", "/api/openai/chat/completions",
			`{"model":"test","messages":[{"role":"user","content":"hi"}]}`, false},
		{"bad json", "POST", "/api/openai/chat/completions",
			"not json", true},
		{"empty messages", "POST", "/api/openai/chat/completions",
			`{"model":"test","messages":[]}`, true},
		{"unknown endpoint", "GET", "/api/openai/unknown", "", true},
		{"model not found", "GET", "/api/openai/models/nonexistent", "", true},
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
				req.Header.Set("Authorization", "Bearer "+env.apiKey)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			// Must be JSON
			ct := resp.Header.Get("Content-Type")
			if !strings.Contains(ct, "application/json") {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("error response Content-Type=%q, want application/json. Body: %s", ct, string(b))
				return
			}

			// Must be valid JSON
			b, _ := io.ReadAll(resp.Body)
			var errResp openai.ErrorResponse
			if err := json.Unmarshal(b, &errResp); err != nil {
				t.Errorf("error response is not valid OpenAI error JSON: %v\nBody: %s", err, string(b))
				return
			}

			// Must have an error message
			if errResp.Error.Message == "" {
				t.Errorf("error response has empty message: %s", string(b))
			}
			if errResp.Error.Type == "" {
				t.Errorf("error response has empty type: %s", string(b))
			}
		})
	}
}

// ==========================================================================
// WebSocket protocol abuse tests
// ==========================================================================

func TestSecurity_WSGarbageBeforeHello(t *testing.T) {
	env := setupTestEnv(t)

	conn, _, err := websocket.DefaultDialer.Dial(env.wsURL(), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send garbage before hello message
	conn.WriteMessage(websocket.TextMessage, []byte("this is not json"))

	// Connection should be closed or error returned
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Error("expected connection to be closed after garbage message")
	}
}

func TestSecurity_WSBinaryBeforeHello(t *testing.T) {
	env := setupTestEnv(t)

	conn, _, err := websocket.DefaultDialer.Dial(env.wsURL(), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send binary frame before hello
	garbage := make([]byte, 1024)
	for i := range garbage {
		garbage[i] = byte(i % 256)
	}
	conn.WriteMessage(websocket.BinaryMessage, garbage)

	// Connection should be closed or error returned
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Error("expected connection to be closed after binary garbage")
	}
}

func TestSecurity_WSValidJSONWrongType(t *testing.T) {
	env := setupTestEnv(t)

	conn, _, err := websocket.DefaultDialer.Dial(env.wsURL(), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send valid JSON but wrong message type (not "hello")
	msg := `{"type":"token","job_id":"fake","token":"attack"}`
	conn.WriteMessage(websocket.TextMessage, []byte(msg))

	// Should be closed — first message MUST be hello
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Error("expected connection to be closed when first message is not hello")
	}
}

// ==========================================================================
// Health endpoint — should not require auth
// ==========================================================================

func TestSecurity_HealthNoAuth(t *testing.T) {
	env := setupTestEnv(t)

	// Health should work without any auth
	resp, err := http.Get(env.baseURL() + "/health")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("health should not require auth, got %d", resp.StatusCode)
	}
}

func TestSecurity_HealthDoesNotLeakSecrets(t *testing.T) {
	env := setupTestEnv(t)

	resp, err := http.Get(env.baseURL() + "/health")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	body := string(b)

	// Health endpoint must NEVER contain keys, tokens, URLs, or internal paths
	for _, sensitive := range []string{
		env.adminKey,
		env.apiKey,
		"password",
		"secret",
		"redis://",
	} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(sensitive)) {
			t.Errorf("health response leaks sensitive data: found %q in response", sensitive)
		}
	}
}

// ==========================================================================
// XSS in error messages — verify content is properly escaped
// ==========================================================================

func TestSecurity_XSSInModelID(t *testing.T) {
	env := setupTestEnv(t)

	// If the model ID appears in the error message, it must not be raw HTML
	xssPayload := "<script>alert('xss')</script>"
	req, _ := http.NewRequest("GET", env.baseURL()+"/api/openai/models/"+xssPayload, nil)
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)

	// The response is JSON (Content-Type: application/json), so the <script>
	// tag would be JSON-escaped. Verify it's not raw HTML.
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/html") {
		t.Errorf("error response should be JSON, not HTML: Content-Type=%q", ct)
	}

	// The model name appears in the error but should be inside a JSON string
	// (which escapes < and > as \u003c and \u003e), not as raw HTML
	if strings.Contains(string(b), "<script>") && !strings.Contains(ct, "application/json") {
		t.Error("XSS payload appears unescaped in non-JSON response")
	}
}
