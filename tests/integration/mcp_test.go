//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// parseMCPEndpointEvent reads the first SSE event from the MCP SSE stream.
// The first event should be: event: endpoint\ndata: "<url>"\n\n
func parseMCPEndpointEvent(t *testing.T, body *bufio.Reader) string {
	t.Helper()

	var eventName, dataLine string
	deadline := time.Now().Add(3 * time.Second)

	for time.Now().Before(deadline) {
		line, err := body.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SSE: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")

		switch {
		case strings.HasPrefix(line, "event: "):
			eventName = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			dataLine = strings.TrimPrefix(line, "data: ")
		case line == "" && eventName != "":
			// End of event
			if eventName == "endpoint" {
				// data is a JSON-quoted string like `"/mcp/message?sessionId=abc"`
				var url string
				if err := json.Unmarshal([]byte(dataLine), &url); err != nil {
					t.Fatalf("parsing endpoint URL from SSE: %v", err)
				}
				return url
			}
			eventName, dataLine = "", ""
		}
	}
	t.Fatal("timed out waiting for MCP endpoint SSE event")
	return ""
}

// sendMCPRequest sends a JSON-RPC request to the MCP message endpoint and
// reads the SSE response line from the body reader.
func sendMCPRequest(t *testing.T, baseURL, sessionPath string, req map[string]any, body *bufio.Reader) map[string]any {
	t.Helper()

	reqBytes, _ := json.Marshal(req)
	resp, err := http.Post(baseURL+sessionPath, "application/json", bytes.NewReader(reqBytes))
	if err != nil {
		t.Fatalf("POST /mcp/message: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}

	// Read the SSE response from the persistent stream.
	deadline := time.Now().Add(3 * time.Second)
	var eventName, dataLine string

	for time.Now().Before(deadline) {
		line, err := body.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SSE response: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")

		switch {
		case strings.HasPrefix(line, "event: "):
			eventName = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			dataLine = strings.TrimPrefix(line, "data: ")
		case line == "":
			if eventName == "message" && dataLine != "" {
				var result map[string]any
				if err := json.Unmarshal([]byte(dataLine), &result); err != nil {
					t.Fatalf("parsing MCP response JSON: %v", err)
				}
				return result
			}
			// Could be keepalive or endpoint event — skip
			eventName, dataLine = "", ""
		}
	}
	t.Fatal("timed out waiting for MCP response")
	return nil
}

func TestMCP_Initialize(t *testing.T) {
	env := setupTestEnv(t)

	// Open SSE stream.
	req, _ := http.NewRequest(http.MethodGet, env.baseURL()+"/mcp/sse", nil)
	req.Header.Set("Authorization", "Bearer "+env.apiKey)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("GET /mcp/sse: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body := bufio.NewReader(resp.Body)
	sessionPath := parseMCPEndpointEvent(t, body)
	t.Logf("MCP session path: %s", sessionPath)

	// Send initialize.
	result := sendMCPRequest(t, env.baseURL(), sessionPath, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test-client", "version": "1.0"},
		},
	}, body)

	t.Logf("initialize result: %+v", result)

	if result["error"] != nil {
		t.Fatalf("unexpected error: %v", result["error"])
	}

	inner, _ := result["result"].(map[string]any)
	if inner == nil {
		t.Fatal("expected result in initialize response")
	}
	if inner["protocolVersion"] == nil {
		t.Error("expected protocolVersion in initialize result")
	}
	serverInfo, _ := inner["serverInfo"].(map[string]any)
	if serverInfo["name"] != "auxot-router" {
		t.Errorf("expected serverInfo.name=auxot-router, got %v", serverInfo["name"])
	}
}

func TestMCP_ToolsList(t *testing.T) {
	env := setupTestEnv(t)

	// Connect a tools worker first.
	tw := newMockToolsWorker()
	tw.connect(t, env, []string{"code_executor", "web_fetch"})
	defer tw.close()

	time.Sleep(50 * time.Millisecond)

	// Open SSE stream.
	req, _ := http.NewRequest(http.MethodGet, env.baseURL()+"/mcp/sse", nil)
	req.Header.Set("Authorization", "Bearer "+env.apiKey)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("GET /mcp/sse: %v", err)
	}
	defer resp.Body.Close()

	body := bufio.NewReader(resp.Body)
	sessionPath := parseMCPEndpointEvent(t, body)

	// tools/list.
	result := sendMCPRequest(t, env.baseURL(), sessionPath, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	}, body)

	t.Logf("tools/list result: %+v", result)

	if result["error"] != nil {
		t.Fatalf("unexpected error: %v", result["error"])
	}

	inner, _ := result["result"].(map[string]any)
	tools, _ := inner["tools"].([]any)
	if len(tools) == 0 {
		t.Error("expected at least one tool in MCP tools/list")
	}

	// Verify structure: each tool should have name, description, inputSchema.
	for _, tool := range tools {
		tm, _ := tool.(map[string]any)
		if tm["name"] == nil || tm["inputSchema"] == nil {
			t.Errorf("tool missing required fields: %+v", tm)
		}
	}
}

func TestMCP_ToolsCall(t *testing.T) {
	env := setupTestEnv(t)

	// Tools worker with canned result.
	tw := newMockToolsWorker()
	tw.setResponse("code_executor", map[string]any{"output": "42", "logs": []any{}})
	tw.connect(t, env, []string{"code_executor"})
	defer tw.close()

	time.Sleep(50 * time.Millisecond)

	// Open SSE stream.
	req, _ := http.NewRequest(http.MethodGet, env.baseURL()+"/mcp/sse", nil)
	req.Header.Set("Authorization", "Bearer "+env.apiKey)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("GET /mcp/sse: %v", err)
	}
	defer resp.Body.Close()

	body := bufio.NewReader(resp.Body)
	sessionPath := parseMCPEndpointEvent(t, body)

	// tools/call.
	result := sendMCPRequest(t, env.baseURL(), sessionPath, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "code_executor",
			"arguments": map[string]string{"code": "6*7"},
		},
	}, body)

	t.Logf("tools/call result: %+v", result)

	if result["error"] != nil {
		t.Fatalf("unexpected error: %v", result["error"])
	}

	inner, _ := result["result"].(map[string]any)
	content, _ := inner["content"].([]any)
	if len(content) == 0 {
		t.Fatal("expected content in tools/call result")
	}

	item, _ := content[0].(map[string]any)
	if item["type"] != "text" {
		t.Errorf("expected content[0].type=text, got %v", item["type"])
	}
	text, _ := item["text"].(string)
	if !strings.Contains(text, "42") {
		t.Errorf("expected result to contain '42', got %q", text)
	}
}

func TestMCP_UnknownMethod(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest(http.MethodGet, env.baseURL()+"/mcp/sse", nil)
	req.Header.Set("Authorization", "Bearer "+env.apiKey)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("GET /mcp/sse: %v", err)
	}
	defer resp.Body.Close()

	body := bufio.NewReader(resp.Body)
	sessionPath := parseMCPEndpointEvent(t, body)

	result := sendMCPRequest(t, env.baseURL(), sessionPath, map[string]any{
		"jsonrpc": "2.0",
		"id":      99,
		"method":  "nonexistent/method",
	}, body)

	if result["error"] == nil {
		t.Fatal("expected error for unknown method")
	}
	errObj, _ := result["error"].(map[string]any)
	code, _ := errObj["code"].(float64)
	if code != -32601 {
		t.Errorf("expected error code -32601 (method not found), got %v", code)
	}
}

func TestMCP_NoSessionID(t *testing.T) {
	env := setupTestEnv(t)

	reqBytes, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
	})
	resp, err := http.Post(env.baseURL()+"/mcp/message", "application/json", bytes.NewReader(reqBytes))
	if err != nil {
		t.Fatalf("POST /mcp/message: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for missing session, got %d", resp.StatusCode)
	}
}
