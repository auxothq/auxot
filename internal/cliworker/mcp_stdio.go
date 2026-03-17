package cliworker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/auxothq/auxot/pkg/protocol"
)

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

	// Convert from OpenAI tool format to MCP tool format.
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

		var req struct {
			JSONRPC string           `json:"jsonrpc"`
			Method  string           `json:"method"`
			Params  json.RawMessage  `json:"params,omitempty"`
			ID      *json.RawMessage `json:"id,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}

		// Notifications (no ID) don't need a response.
		if req.ID == nil {
			continue
		}

		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "auxot", "version": "1.0.0"},
			}
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
			// Unknown method — return empty result to avoid breaking the handshake.
			result = map[string]any{}
		}

		_ = encoder.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result,
		})
	}
}
