//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/auxothq/auxot/pkg/protocol"
)

// ─────────────────────────────────────────────────────────────────────────────
// Mock tools worker
// ─────────────────────────────────────────────────────────────────────────────

// mockToolsWorker simulates a tools worker connecting via WebSocket.
// It listens for ToolJobMessages and responds with canned results.
type mockToolsWorker struct {
	conn   *websocket.Conn
	toolID string
	mu     sync.Mutex
	closed bool

	// Canned responses keyed by tool name. If not set, returns a default.
	responses map[string]json.RawMessage

	doneCh chan struct{}
}

func newMockToolsWorker() *mockToolsWorker {
	return &mockToolsWorker{
		responses: make(map[string]json.RawMessage),
		doneCh:    make(chan struct{}),
	}
}

func (mw *mockToolsWorker) setResponse(toolName string, result any) {
	data, _ := json.Marshal(result)
	mw.responses[toolName] = data
}

func (mw *mockToolsWorker) connect(t *testing.T, env *testEnv, tools []string) {
	t.Helper()

	conn, _, err := websocket.DefaultDialer.Dial(env.wsURL(), nil)
	if err != nil {
		t.Fatalf("mock tools worker dial: %v", err)
	}
	mw.conn = conn

	hello := protocol.HelloMessage{
		Type:       protocol.TypeHello,
		GPUKey:     env.toolsKey,
		WorkerType: protocol.WorkerTypeTools,
		ToolsCapabilities: &protocol.ToolsCapabilities{
			Tools: tools,
		},
	}
	mw.sendJSON(t, hello)

	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("reading hello_ack: %v", err)
	}
	msg, err := protocol.ParseMessage(data)
	if err != nil {
		t.Fatalf("parsing hello_ack: %v", err)
	}
	ack, ok := msg.(protocol.HelloAckMessage)
	if !ok {
		t.Fatalf("expected hello_ack, got %T", msg)
	}
	if !ack.Success {
		t.Fatalf("tools worker hello_ack failed: %s", ack.Error)
	}
	mw.toolID = ack.GPUID

	go mw.readLoop(t)
}

func (mw *mockToolsWorker) readLoop(t *testing.T) {
	defer close(mw.doneCh)

	for {
		_, data, err := mw.conn.ReadMessage()
		if err != nil {
			return
		}

		msg, err := protocol.ParseMessage(data)
		if err != nil {
			continue
		}

		switch m := msg.(type) {
		case protocol.ToolJobMessage:
			mw.handleToolJob(t, m)
		case protocol.HeartbeatAckMessage:
			// nothing
		}
	}
}

func (mw *mockToolsWorker) handleToolJob(t *testing.T, job protocol.ToolJobMessage) {
	result, ok := mw.responses[job.ToolName]
	if !ok {
		// Default: return the arguments back as the result
		result = job.Arguments
	}

	resp := protocol.ToolResultMessage{
		Type:        protocol.TypeToolResult,
		JobID:       job.JobID,
		ParentJobID: job.ParentJobID,
		ToolCallID:  job.ToolCallID,
		ToolName:    job.ToolName,
		Result:      result,
		DurationMS:  5,
	}
	mw.sendJSON(t, resp)
}

func (mw *mockToolsWorker) sendJSON(t *testing.T, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling tools worker message: %v", err)
	}
	mw.mu.Lock()
	defer mw.mu.Unlock()
	if err := mw.conn.WriteMessage(websocket.TextMessage, data); err != nil && !mw.closed {
		t.Logf("sending tools worker message: %v", err)
	}
}

func (mw *mockToolsWorker) close() {
	mw.mu.Lock()
	mw.closed = true
	mw.mu.Unlock()
	if mw.conn != nil {
		mw.conn.Close()
	}
	<-mw.doneCh
}

// ─────────────────────────────────────────────────────────────────────────────
// Test: tools worker connects and authenticates
// ─────────────────────────────────────────────────────────────────────────────

func TestToolsWorkerConnect(t *testing.T) {
	env := setupTestEnv(t)

	tw := newMockToolsWorker()
	tw.connect(t, env, []string{"code_executor", "web_fetch"})
	defer tw.close()

	// Give the router a moment to register the worker.
	time.Sleep(50 * time.Millisecond)

	// Verify via the tools list endpoint.
	req, _ := http.NewRequest(http.MethodGet, env.baseURL()+"/api/tools/v1/tools", nil)
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("listing tools: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)

	tools, _ := body["tools"].([]any)
	if len(tools) == 0 {
		t.Error("expected at least one tool in the list")
	}

	t.Logf("tools list: %v", tools)
}

func TestToolsWorkerBadKey(t *testing.T) {
	env := setupTestEnv(t)

	// Try to connect with an invalid key.
	conn, _, err := websocket.DefaultDialer.Dial(env.wsURL(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	hello := protocol.HelloMessage{
		Type:       protocol.TypeHello,
		GPUKey:     "tls_this_is_wrong",
		WorkerType: protocol.WorkerTypeTools,
		ToolsCapabilities: &protocol.ToolsCapabilities{
			Tools: []string{"code_executor"},
		},
	}
	data, _ := json.Marshal(hello)
	conn.WriteMessage(websocket.TextMessage, data)

	// Router should respond with an error and close the connection.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msgData, err := conn.ReadMessage()
	if err != nil {
		// Connection closed is also acceptable
		return
	}

	msg, _ := protocol.ParseMessage(msgData)
	if errMsg, ok := msg.(protocol.ErrorMessage); ok {
		t.Logf("got expected error: %s", errMsg.Error)
	} else {
		t.Logf("got unexpected ack type %T — connection may have been closed instead", msg)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test: direct tool execution via HTTP API
// ─────────────────────────────────────────────────────────────────────────────

func TestDirectToolExecution(t *testing.T) {
	env := setupTestEnv(t)

	// Connect a tools worker that returns canned results.
	tw := newMockToolsWorker()
	tw.setResponse("code_executor", map[string]any{
		"output": "42",
		"logs":   []string{},
	})
	tw.connect(t, env, []string{"code_executor"})
	defer tw.close()

	time.Sleep(50 * time.Millisecond)

	// Execute a tool directly via the API.
	body := map[string]any{
		"tool":      "code_executor",
		"arguments": map[string]string{"code": "6 * 7"},
	}
	bodyBytes, _ := json.Marshal(body)

	req, _ := http.NewRequest(http.MethodPost,
		env.baseURL()+"/api/tools/v1/execute",
		bytes.NewReader(bodyBytes),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("executing tool: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	t.Logf("direct tool result: %+v", result)

	if result["tool"] != "code_executor" {
		t.Errorf("expected tool=code_executor in response, got %v", result["tool"])
	}
	if result["result"] == nil {
		t.Error("expected result in response")
	}
}

func TestDirectToolNoWorker(t *testing.T) {
	env := setupTestEnv(t)
	// No tools worker connected.

	body := map[string]any{
		"tool":      "code_executor",
		"arguments": map[string]string{"code": "1+1"},
	}
	bodyBytes, _ := json.Marshal(body)

	req, _ := http.NewRequest(http.MethodPost,
		env.baseURL()+"/api/tools/v1/execute",
		bytes.NewReader(bodyBytes),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("executing tool: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test: LLM → tool calls → continuation → final response
// ─────────────────────────────────────────────────────────────────────────────

// mockToolCallingGPUWorker is a GPU worker that responds with tool calls on the
// first job and then a normal completion on the second (continuation) job.
type mockToolCallingGPUWorker struct {
	*mockWorker
	mu           sync.Mutex
	callCount    int
	toolCallResp *protocol.CompleteMessage // first response: tool calls
	finalResp    *protocol.CompleteMessage // second response: final answer
}

func newMockToolCallingGPUWorker(toolCallID, toolName, toolArgs string) *mockToolCallingGPUWorker {
	base := newMockWorker()
	return &mockToolCallingGPUWorker{
		mockWorker: base,
		toolCallResp: &protocol.CompleteMessage{
			Type:         protocol.TypeComplete,
			FullResponse: "Let me calculate that for you.",
			ToolCalls: []protocol.ToolCall{
				{
					ID:   toolCallID,
					Type: "function",
					Function: protocol.ToolFunction{
						Name:      toolName,
						Arguments: toolArgs,
					},
				},
			},
			InputTokens:  10,
			OutputTokens: 8,
		},
		finalResp: &protocol.CompleteMessage{
			Type:         protocol.TypeComplete,
			FullResponse: "The answer is 42.",
			InputTokens:  20,
			OutputTokens: 5,
		},
	}
}

func (mw *mockToolCallingGPUWorker) processJobs() {
	for job := range mw.jobsCh {
		mw.mu.Lock()
		callN := mw.callCount
		mw.callCount++
		mw.mu.Unlock()

		var msg *protocol.CompleteMessage
		if callN == 0 {
			// First call: respond with tool calls
			resp := *mw.toolCallResp
			resp.JobID = job.JobID
			msg = &resp
		} else {
			// Continuation round: final answer
			resp := *mw.finalResp
			resp.JobID = job.JobID
			// Send a final token first
			tokenMsg := protocol.TokenMessage{
				Type:  protocol.TypeToken,
				JobID: job.JobID,
				Token: "The answer is 42.",
			}
			tokenData, _ := json.Marshal(tokenMsg)
			mw.mu.Lock()
			if !mw.closed {
				mw.conn.WriteMessage(websocket.TextMessage, tokenData)
			}
			mw.mu.Unlock()
			msg = &resp
		}

		data, _ := json.Marshal(msg)
		mw.mu.Lock()
		if !mw.closed {
			mw.conn.WriteMessage(websocket.TextMessage, data)
		}
		mw.mu.Unlock()
	}
}

func (mw *mockToolCallingGPUWorker) connectAndRun(t *testing.T, env *testEnv) {
	t.Helper()

	conn, _, err := websocket.DefaultDialer.Dial(env.wsURL(), nil)
	if err != nil {
		t.Fatalf("mock GPU worker dial: %v", err)
	}
	mw.conn = conn

	hello := protocol.HelloMessage{
		Type:   protocol.TypeHello,
		GPUKey: env.adminKey,
		Capabilities: protocol.Capabilities{
			Backend:    "mock",
			Model:      "test-model",
			CtxSize:    4096,
			TotalSlots: 2,
		},
	}
	mw.sendJSON(t, hello)

	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("reading hello_ack: %v", err)
	}
	msg, _ := protocol.ParseMessage(data)
	ack, ok := msg.(protocol.HelloAckMessage)
	if !ok || !ack.Success {
		t.Fatalf("hello_ack failed or missing")
	}
	mw.gpuID = ack.GPUID

	go mw.readLoop()
	go mw.processJobs()
}

func TestToolCallContinuation(t *testing.T) {
	env := setupTestEnv(t)

	// Set up a tools worker that returns a canned code_executor result.
	tw := newMockToolsWorker()
	tw.setResponse("code_executor", map[string]any{
		"output": "42",
		"logs":   []string{},
	})
	tw.connect(t, env, []string{"code_executor"})
	defer tw.close()

	// Set up a GPU worker that makes a tool call on the first request.
	toolCallID := "call_test123"
	gw := newMockToolCallingGPUWorker(toolCallID, "code_executor", `{"code":"6*7","timeout":5}`)
	gw.connectAndRun(t, env)
	defer gw.close()

	time.Sleep(100 * time.Millisecond) // let workers register

	// Send a chat completion request.
	reqBody := map[string]any{
		"model": "test-model",
		"messages": []map[string]string{
			{"role": "user", "content": "What is 6 times 7?"},
		},
		"stream": false,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest(http.MethodPost,
		env.baseURL()+"/api/openai/chat/completions",
		bytes.NewReader(bodyBytes),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	// Use a generous timeout — tool call round trip takes some time.
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("chat completion request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	t.Logf("continuation response: %+v", result)

	// The final response should contain the answer text (not tool_calls).
	choices, _ := result["choices"].([]any)
	if len(choices) == 0 {
		t.Fatal("expected at least one choice in response")
	}
	choice := choices[0].(map[string]any)
	message := choice["message"].(map[string]any)
	content, _ := message["content"].(string)

	if !strings.Contains(content, "42") {
		t.Errorf("expected final response to contain '42', got: %q", content)
	}

	// GPU worker should have been called twice (initial + continuation).
	gw.mu.Lock()
	callCount := gw.callCount
	gw.mu.Unlock()

	if callCount < 2 {
		t.Errorf("expected GPU worker to be called at least twice (got %d) — continuation may not have fired", callCount)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test: tool calls fall through to caller when no tools worker connected
// ─────────────────────────────────────────────────────────────────────────────

func TestToolCallFallthrough(t *testing.T) {
	env := setupTestEnv(t)
	// No tools worker — tool calls should be passed straight to the API caller.

	// GPU worker that always responds with a tool call.
	gw := newMockToolCallingGPUWorker("call_fallthrough", "code_executor", `{"code":"1+1"}`)
	gw.connectAndRun(t, env)
	defer gw.close()

	time.Sleep(50 * time.Millisecond)

	reqBody := map[string]any{
		"model": "test-model",
		"messages": []map[string]string{
			{"role": "user", "content": "Use a tool"},
		},
		"stream": false,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest(http.MethodPost,
		env.baseURL()+"/api/openai/chat/completions",
		bytes.NewReader(bodyBytes),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.apiKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("chat completion request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	// finish_reason should be "tool_calls" (caller handles them).
	choices, _ := result["choices"].([]any)
	if len(choices) == 0 {
		t.Fatal("expected at least one choice")
	}
	choice := choices[0].(map[string]any)
	finishReason, _ := choice["finish_reason"].(string)
	if finishReason != "tool_calls" {
		t.Errorf("expected finish_reason=tool_calls, got %q", finishReason)
	}

	t.Logf("fallthrough response finish_reason: %s", finishReason)
}
