package cliworker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// TestCallToolProxy_ImageParts verifies that callToolProxy correctly decodes
// image_parts from the tool proxy HTTP response and surfaces them as
// []protocol.ImagePart alongside the text result.
//
// This covers layer 3 of the screenshot→vision fix:
//
//	auxot/internal/cliworker/liveproxy.go + mcp_stdio.go
func TestCallToolProxy_ImageParts(t *testing.T) {
	pngData := minimalPNG()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tool-call" || r.Method != http.MethodPost {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// Verify the request contains expected fields.
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req["tool_name"] != "browser__screenshot" {
			http.Error(w, "wrong tool", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		// The proxy encodes ImagePart.Data as base64 via standard json.Marshal.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result":   "screenshot taken",
			"is_error": false,
			"image_parts": []map[string]any{
				{"mime_type": "image/png", "data": pngData},
			},
		})
	}))
	defer srv.Close()

	result, imgParts, isError, err := callToolProxy(
		context.Background(),
		srv.URL, "job-test-1", "call-test-1",
		"browser__screenshot", `{"url":"https://example.com"}`,
	)
	if err != nil {
		t.Fatalf("callToolProxy: %v", err)
	}
	if isError {
		t.Fatalf("expected isError=false, got true")
	}
	if result != "screenshot taken" {
		t.Errorf("result = %q, want %q", result, "screenshot taken")
	}
	if len(imgParts) != 1 {
		t.Fatalf("len(image_parts) = %d, want 1", len(imgParts))
	}
	if imgParts[0].MIMEType != "image/png" {
		t.Errorf("mime_type = %q, want image/png", imgParts[0].MIMEType)
	}
	if !bytes.Equal(imgParts[0].Data, pngData) {
		t.Errorf("image data mismatch: got %d bytes, want %d bytes",
			len(imgParts[0].Data), len(pngData))
	}
}

// TestCallToolProxy_NoImages verifies backward-compatible behaviour: when the
// proxy returns no image_parts, imgParts is nil and the text result is returned.
func TestCallToolProxy_NoImages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result":   "ls output: main.go",
			"is_error": false,
		})
	}))
	defer srv.Close()

	result, imgParts, isError, err := callToolProxy(context.Background(), srv.URL, "j1", "c1", "Bash", `{"command":"ls"}`)
	if err != nil {
		t.Fatalf("callToolProxy: %v", err)
	}
	if isError {
		t.Fatalf("expected isError=false, got true")
	}
	if result != "ls output: main.go" {
		t.Errorf("result = %q, want %q", result, "ls output: main.go")
	}
	if len(imgParts) != 0 {
		t.Errorf("expected no image_parts, got %d", len(imgParts))
	}
}

// TestCallToolProxy_ErrorResult verifies that when the proxy reports an error
// (via the "error" field), isError=true and the error text is returned.
func TestCallToolProxy_ErrorResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "tool timed out",
		})
	}))
	defer srv.Close()

	result, _, isError, err := callToolProxy(context.Background(), srv.URL, "j1", "c1", "Bash", `{}`)
	if err != nil {
		t.Fatalf("callToolProxy: %v", err)
	}
	if !isError {
		t.Fatalf("expected isError=true, got false")
	}
	if result != "tool timed out" {
		t.Errorf("result = %q, want %q", result, "tool timed out")
	}
}

// TestRunMCPStdioLive_ImageContent is the end-to-end integration test for layer 4
// of the screenshot→vision fix. It exercises the full tools/call path inside
// RunMCPStdioLive and asserts that the outbound MCP JSON-RPC response contains
// a {type:"image", data:"<base64>", mimeType:"image/png"} content block.
//
// Test flow:
//
//  1. Start a mock tool proxy HTTP server that returns image_parts.
//  2. Write a temp tools JSON file (required by RunMCPStdioLive).
//  3. Redirect os.Stdin / os.Stdout via os.Pipe() for the duration of the test.
//  4. Invoke RunMCPStdioLive in a goroutine (it reads from os.Stdin).
//  5. Send: initialize → tools/call via the stdin pipe.
//  6. Close stdin to terminate the RunMCPStdioLive loop.
//  7. Parse the accumulated stdout and assert the tools/call response.
//
// NOTE: This test is NOT safe for t.Parallel() because it mutates the process-global
// os.Stdin and os.Stdout pointers. The cleanup restores them via t.Cleanup.
func TestRunMCPStdioLive_ImageContent(t *testing.T) {
	pngData := minimalPNG()
	pngB64 := base64.StdEncoding.EncodeToString(pngData)

	// Start mock tool proxy.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result":   "screenshot of example.com",
			"is_error": false,
			"image_parts": []map[string]any{
				{"mime_type": "image/png", "data": pngData},
			},
		})
	}))
	defer srv.Close()

	// Write tools JSON file.
	toolsFile := writeTempToolsFile(t, `[{"type":"function","function":{"name":"browser__screenshot","description":"Take a screenshot","parameters":{"type":"object","properties":{"url":{"type":"string"}}}}}]`)

	// Redirect os.Stdin / os.Stdout.
	origStdin := os.Stdin
	origStdout := os.Stdout
	t.Cleanup(func() {
		os.Stdin = origStdin
		os.Stdout = origStdout
	})

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (stdin): %v", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (stdout): %v", err)
	}
	os.Stdin = stdinR
	os.Stdout = stdoutW

	// Run in background; exits when stdin is closed.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = RunMCPStdioLive(toolsFile, srv.URL, "job-live-test-1")
	}()

	// Send initialize (id=1) then tools/call (id=2).
	sendJSONLine(t, stdinW, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "0.0.1"},
		},
	})
	sendJSONLine(t, stdinW, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name":      "browser__screenshot",
			"arguments": map[string]any{"url": "https://example.com"},
		},
	})

	// Close stdin → RunMCPStdioLive's ReadString returns io.EOF → function returns.
	stdinW.Close()
	wg.Wait()
	stdoutW.Close()

	// Collect all JSON-RPC responses.
	responses := readJSONLines(t, stdoutR)
	stdoutR.Close()

	if len(responses) == 0 {
		t.Fatal("no responses written to stdout")
	}

	// Locate the tools/call response (id == 2).
	callResp := findByID(t, responses, 2)

	// Must be a success response (no "error" key at the top level).
	if errVal, hasErr := callResp["error"]; hasErr {
		t.Fatalf("tools/call returned JSON-RPC error: %v", errVal)
	}

	result, ok := callResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result field is %T, want map", callResp["result"])
	}

	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content is %T (len %d), want non-empty []any", result["content"], len(content))
	}

	// --- Text block (first) ---
	textBlock := assertContentBlock(t, content, 0, "text")
	if textBlock["text"] != "screenshot of example.com" {
		t.Errorf("text block text = %q, want %q", textBlock["text"], "screenshot of example.com")
	}

	// --- Image block (second) ---
	imgBlock := assertContentBlock(t, content, 1, "image")
	if imgBlock["mimeType"] != "image/png" {
		t.Errorf("image block mimeType = %q, want image/png", imgBlock["mimeType"])
	}
	gotData, _ := imgBlock["data"].(string)
	if gotData != pngB64 {
		t.Errorf("image block data:\n  got  %q\n  want %q", gotData, pngB64)
	}
}

// TestRunMCPStdioLive_EmptyTextWithImage covers the edge case where the tool
// proxy returns no text but does return image_parts.  The MCP content should
// contain only the image block (no empty text block).
func TestRunMCPStdioLive_EmptyTextWithImage(t *testing.T) {
	pngData := minimalPNG()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result":   "",
			"is_error": false,
			"image_parts": []map[string]any{
				{"mime_type": "image/png", "data": pngData},
			},
		})
	}))
	defer srv.Close()

	toolsFile := writeTempToolsFile(t, `[{"type":"function","function":{"name":"snap","description":"snap","parameters":{}}}]`)

	origStdin, origStdout := os.Stdin, os.Stdout
	t.Cleanup(func() { os.Stdin = origStdin; os.Stdout = origStdout })

	stdinR, stdinW, _ := os.Pipe()
	stdoutR, stdoutW, _ := os.Pipe()
	os.Stdin = stdinR
	os.Stdout = stdoutW

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = RunMCPStdioLive(toolsFile, srv.URL, "job-empty-text-1")
	}()

	sendJSONLine(t, stdinW, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "snap", "arguments": map[string]any{}},
	})
	stdinW.Close()
	wg.Wait()
	stdoutW.Close()

	responses := readJSONLines(t, stdoutR)
	stdoutR.Close()

	resp := findByID(t, responses, 1)
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]any)

	// When text is empty but images are present, we expect only the image block
	// (the code skips the empty-text block and only adds the image).
	var imageFound bool
	for _, b := range content {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if block["type"] == "image" {
			imageFound = true
		}
		if block["type"] == "text" {
			txt, _ := block["text"].(string)
			if txt == "" {
				t.Errorf("unexpected empty text block in content: %v", block)
			}
		}
	}
	if !imageFound {
		t.Errorf("expected an image content block, none found: %v", content)
	}
}

// ── test helpers ──────────────────────────────────────────────────────────────

// minimalPNG returns the bytes of a 1×1 transparent PNG used as test fixture data.
func minimalPNG() []byte {
	const b64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	b, _ := base64.StdEncoding.DecodeString(b64)
	return b
}

func writeTempToolsFile(t *testing.T, toolsJSON string) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/tools.json"
	if err := os.WriteFile(path, []byte(toolsJSON), 0o600); err != nil {
		t.Fatalf("writeTempToolsFile: %v", err)
	}
	return path
}

func sendJSONLine(t *testing.T, w *os.File, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("sendJSONLine marshal: %v", err)
	}
	b = append(b, '\n')
	if _, err := w.Write(b); err != nil {
		t.Fatalf("sendJSONLine write: %v", err)
	}
}

func readJSONLines(t *testing.T, r *os.File) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Logf("readJSONLines: skipping non-JSON line: %q", line)
			continue
		}
		out = append(out, obj)
	}
	return out
}

// findByID returns the first response whose "id" field equals wantID (as float64).
func findByID(t *testing.T, responses []map[string]any, wantID int) map[string]any {
	t.Helper()
	for _, r := range responses {
		id, _ := r["id"].(float64)
		if int(id) == wantID {
			return r
		}
	}
	t.Fatalf("no response with id=%d found among %d responses: %v", wantID, len(responses), responses)
	return nil
}

// assertContentBlock checks that content[idx] is a map with the given type and returns it.
func assertContentBlock(t *testing.T, content []any, idx int, wantType string) map[string]any {
	t.Helper()
	if idx >= len(content) {
		t.Fatalf("content[%d] does not exist (len=%d)", idx, len(content))
	}
	block, ok := content[idx].(map[string]any)
	if !ok {
		t.Fatalf("content[%d] is %T, want map", idx, content[idx])
	}
	if block["type"] != wantType {
		t.Fatalf("content[%d].type = %q, want %q", idx, block["type"], wantType)
	}
	return block
}
