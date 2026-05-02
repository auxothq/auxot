package cliworker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/auxothq/auxot/pkg/protocol"
)

// dataURIRe matches image data URIs embedded anywhere in a JSON result string.
// Used to strip them from the MCP text block so Claude receives them only as
// typed image content blocks, not as raw base64 in text.
var dataURIRe = regexp.MustCompile(`data:(image/[^;\"\\]+);base64,([A-Za-z0-9+/]+=*)`)

// RunMCPStdio implements the MCP stdio transport protocol.
// It is invoked as a subprocess by the claude CLI (via the --mcp-config JSON).
// Its sole job is to advertise the tools from the job so that claude knows they exist.
//
// When claude decides to call a tool, it will:
//  1. Emit the full assistant turn (with tool_use blocks) to its stream-json stdout.
//  2. Then invoke this MCP server to execute the tool.
//
// RunJob waits until the assistant turn is fully emitted (stop_reason is set) before
// cancelling claude, so it can collect ALL tool_use blocks for parallel execution.
// Any tools/call requests that arrive before claude is killed get an MCP error
// response; the actual tool execution happens server-side.
func RunMCPStdio(toolsFile string) error {
	data, err := os.ReadFile(toolsFile)
	if err != nil {
		return fmt.Errorf("reading tools file: %w", err)
	}
	var tools []protocol.Tool
	if err := json.Unmarshal(data, &tools); err != nil {
		return fmt.Errorf("parsing tools: %w", err)
	}

	mcpTools := buildMCPTools(tools)
	reader := bufio.NewReader(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil // stdin closed — parent (claude) was killed
		}
		if line == "" || line == "\n" {
			continue
		}

		var req mcpRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue // notification — no response needed
		}

		var result any
		switch req.Method {
		case "initialize":
			result = mcpInitResult()
		case "tools/list":
			result = map[string]any{"tools": mcpTools}

		case "tools/call":
			// RunJob delays cancelClaude() until all tool_use blocks have been
			// collected (stop_reason != nil). By the time claude sends tools/call,
			// RunJob may have already killed the process (stdin will close and we
			// exit via the ReadString error). If we DO receive the request, return
			// a structured MCP error so claude handles it gracefully rather than
			// crashing from a dead pipe.
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error": map[string]any{
					"code":    -32603,
					"message": "tool execution is handled by the auxot server",
				},
			})
			continue

		default:
			result = map[string]any{}
		}

		_ = encoder.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result,
		})
	}
}

// RunMCPStdioLive implements the MCP stdio transport protocol for live-continuation
// mode. Unlike RunMCPStdio, this mode actually executes tool calls by forwarding
// them to the in-worker HTTP proxy (started by RunJob in live mode). The proxy
// routes them to the server via WebSocket for execution on the sentinel agent VM.
//
// Environment variables set by RunJob's setupMCP (live mode):
//
//	AUXOT_MCP_TOOL_PROXY  — HTTP URL of the in-worker tool proxy (e.g. http://127.0.0.1:12345)
//	AUXOT_MCP_JOB_ID      — the active job ID for correlation
//	AUXOT_MCP_TOOLS_FILE  — path to the JSON tools file (same as non-live mode)
func RunMCPStdioLive(toolsFile, proxyURL, jobID string) error {
	data, err := os.ReadFile(toolsFile)
	if err != nil {
		return fmt.Errorf("reading tools file: %w", err)
	}
	var tools []protocol.Tool
	if err := json.Unmarshal(data, &tools); err != nil {
		return fmt.Errorf("parsing tools: %w", err)
	}

	mcpTools := buildMCPTools(tools)
	reader := bufio.NewReader(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil // stdin closed — session ended
		}
		if line == "" || line == "\n" {
			continue
		}

		var req mcpRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue
		}

		var result any
		switch req.Method {
		case "initialize":
			result = mcpInitResult()
		case "tools/list":
			result = map[string]any{"tools": mcpTools}

		case "tools/call":
			// Parse the call params to extract name, arguments, and optional meta.
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
				Meta      struct {
					ToolUseID string `json:"claudecode/toolUseId"`
				} `json:"_meta"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				_ = encoder.Encode(mcpErrorResp(req.ID, -32602, "invalid params: "+err.Error()))
				continue
			}

			// Prefer the Claude tool use ID from _meta so that liveproxy's
			// originalResults map is keyed by the same ID that claude.go uses
			// when scanning the session JSONL (block.ToolUseID). Fall back to
			// the JSON-RPC request ID only when _meta is absent (older builds).
			var callID string
			if params.Meta.ToolUseID != "" {
				callID = params.Meta.ToolUseID
			} else {
				callIDBytes, _ := json.Marshal(req.ID)
				callID = string(callIDBytes)
			}

			argsStr := string(params.Arguments)
			if argsStr == "" {
				argsStr = "{}"
			}

			// Forward to the in-worker HTTP proxy. This call blocks until the
			// server has executed the tool and returned the result.
			callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Minute)
			toolResult, imageParts, isError, proxyErr := callToolProxy(callCtx, proxyURL, jobID, callID, params.Name, argsStr)
			callCancel()
			if proxyErr != nil {
				_ = encoder.Encode(mcpErrorResp(req.ID, -32603,
					fmt.Sprintf("tool proxy error: %v", proxyErr)))
				continue
			}

			// Strip data URIs from the text portion before sending to Claude —
			// they arrive as proper image blocks below, so the text block must not
			// also carry them (wastes context and confuses the model).
			mpcTextResult := dataURIRe.ReplaceAllString(toolResult, "[image]")

			// Build MCP content blocks: text first (if non-empty), then image parts.
			var contentBlocks []map[string]any
			if mpcTextResult != "" {
				contentBlocks = append(contentBlocks, map[string]any{
					"type": "text",
					"text": mpcTextResult,
				})
			}
			for _, img := range imageParts {
				contentBlocks = append(contentBlocks, map[string]any{
					"type":     "image",
					"data":     base64.StdEncoding.EncodeToString(img.Data),
					"mimeType": img.MIMEType,
				})
			}
			// Always include at least one content block for backward compatibility
			// when there are no images and the text result is empty.
			if len(contentBlocks) == 0 {
				contentBlocks = append(contentBlocks, map[string]any{
					"type": "text",
					"text": mpcTextResult,
				})
			}

			// MCP result: either content (success) or error.
			if isError {
				result = map[string]any{
					"isError": true,
					"content": contentBlocks,
				}
			} else {
				result = map[string]any{
					"content": contentBlocks,
				}
			}

		default:
			result = map[string]any{}
		}

		_ = encoder.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result,
		})
	}
}

// callToolProxy sends a tool call to the in-worker HTTP proxy and blocks until
// the result is available. The proxy forwards the call to the server via WebSocket.
func callToolProxy(ctx context.Context, proxyURL, jobID, callID, toolName, arguments string) (result string, imageParts []protocol.ImagePart, isError bool, err error) {
	body, _ := json.Marshal(map[string]any{
		"job_id":    jobID,
		"call_id":   callID,
		"tool_name": toolName,
		"arguments": arguments,
	})
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, proxyURL+"/tool-call", bytes.NewReader(body))
	if reqErr != nil {
		return "", nil, false, fmt.Errorf("building proxy request: %w", reqErr)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, httpErr := http.DefaultClient.Do(req)
	if httpErr != nil {
		return "", nil, false, fmt.Errorf("posting to proxy: %w", httpErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", nil, false, fmt.Errorf("proxy returned %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}

	var decoded struct {
		Result     string               `json:"result"`
		IsError    bool                 `json:"is_error"`
		ImageParts []protocol.ImagePart `json:"image_parts,omitempty"`
		Error      string               `json:"error,omitempty"`
	}
	if decErr := json.NewDecoder(resp.Body).Decode(&decoded); decErr != nil {
		return "", nil, false, fmt.Errorf("decoding proxy response: %w", decErr)
	}
	if decoded.Error != "" {
		return decoded.Error, nil, true, nil
	}
	return decoded.Result, decoded.ImageParts, decoded.IsError, nil
}

// ── shared MCP helpers ────────────────────────────────────────────────────────

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      any             `json:"id,omitempty"`
}

func mcpInitResult() map[string]any {
	return map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "auxot", "version": "1.0.0"},
	}
}

func mcpErrorResp(id any, code int, msg string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	}
}

func buildMCPTools(tools []protocol.Tool) []map[string]any {
	mcpTools := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		entry := map[string]any{
			"name":        t.Function.Name,
			"description": t.Function.Description,
		}
		if len(t.Function.Parameters) > 0 {
			entry["inputSchema"] = json.RawMessage(t.Function.Parameters)
		}
		mcpTools = append(mcpTools, entry)
	}
	return mcpTools
}
