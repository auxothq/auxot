package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/auxothq/auxot/pkg/tools"
)

// AllowedTools is the set of Playwright MCP tool names the LLM may call via the
// aggregate "browser" tool. Context-lifecycle tools (e.g. browser_close,
// browser_new_context) are intentionally omitted — the registry owns those.
var AllowedTools = map[string]bool{
	"browser_navigate":      true,
	"browser_click":         true,
	"browser_type":          true,
	"browser_fill":          true,
	"browser_select_option": true,
	"browser_check":         true,
	"browser_uncheck":       true,
	"browser_hover":         true,
	"browser_screenshot":    true,
	"browser_wait_for":      true,
	"browser_evaluate":      true,
	"browser_get_text":      true,
	"browser_get_attribute": true,
	"browser_go_back":       true,
	"browser_go_forward":    true,
	"browser_reload":        true,
}

// Definition is the LLM-facing tool definition for the aggregate browser tool.
// The router includes this in the tool list sent to the LLM when a browser-capable
// tools worker is connected.
var Definition = tools.ToolDefinition{
	Name:        "browser",
	Description: "Control a real browser to navigate pages, interact with elements, and capture screenshots. Use tool_name to select a Playwright MCP action (e.g. browser_navigate, browser_screenshot) and params for that action's arguments.",
	Parameters: json.RawMessage(`{
		"type": "object",
		"required": ["tool_name"],
		"properties": {
			"tool_name": {
				"type": "string",
				"description": "The Playwright MCP tool to invoke (e.g. browser_navigate, browser_screenshot, browser_click)."
			},
			"params": {
				"type": "object",
				"description": "Arguments for the selected Playwright MCP tool.",
				"additionalProperties": true
			}
		}
	}`),
}

// Executor executes LLM-facing "browser" tool calls against the Playwright MCP sidecar.
// It is registered as the "browser" built-in tool in the tools worker's registry.
type Executor struct {
	registry *Registry
}

// NewExecutor creates an Executor backed by the given Registry.
func NewExecutor(registry *Registry) *Executor {
	return &Executor{registry: registry}
}

// browserArgs is the shape of the JSON the LLM sends to the aggregate browser tool.
type browserArgs struct {
	ToolName string          `json:"tool_name"`
	Params   json.RawMessage `json:"params"`
}

// playwrightContent is one item in the Playwright MCP tools/call content array.
type playwrightContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`     // base64 for image type
	MimeType string `json:"mimeType,omitempty"` // present when type == "image"
}

// playwrightResult is the top-level Playwright MCP tools/call response.
type playwrightResult struct {
	Content []playwrightContent `json:"content"`
	IsError bool                `json:"isError"`
}

// Execute implements tools.Executor.
//
// args must be a JSON object: {"tool_name": string, "params": object}.
// The thread_id is read from ctx via tools.ThreadIDFromContext so the correct
// per-thread browser session is resolved from the registry without passing it
// explicitly through the call stack.
func (e *Executor) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	// 1. Parse the aggregate tool arguments.
	var a browserArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return tools.Result{}, fmt.Errorf("browser: invalid arguments: %w", err)
	}
	toolName := strings.TrimSpace(a.ToolName)
	if toolName == "" {
		return tools.Result{}, fmt.Errorf("browser: tool_name is required")
	}

	// 2. Allowlist check — block dangerous context-lifecycle tools.
	if !AllowedTools[toolName] {
		return tools.Result{}, fmt.Errorf("browser: tool %q is not in the allowed list", toolName)
	}

	// 3. Resolve the thread ID from context for session isolation.
	//    An empty thread_id is a hard error — sharing a fallback session across
	//    threads would cause cross-tenant browser state leakage.
	threadID := tools.ThreadIDFromContext(ctx)
	if threadID == "" {
		return tools.Result{}, fmt.Errorf("browser: tool requires a thread_id in context; ensure the job's reference_id is set")
	}

	// 4. Get or create the per-thread browser session (SSE connection + MCP handshake).
	sess, err := e.registry.GetOrCreate(threadID)
	if err != nil {
		return tools.Result{}, fmt.Errorf("browser: session for thread %q: %w", threadID, err)
	}

	// 5. Build the MCP tools/call params.
	//    Default to an empty JSON object when the LLM omits params entirely,
	//    so the Playwright MCP server always receives a valid arguments field.
	params := a.Params
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}

	// 6. Call the sidecar via the registry (which touches lastUsed on the session).
	raw, err := e.registry.Call(ctx, sess, "tools/call", map[string]any{
		"name":      toolName,
		"arguments": params,
	})
	if err != nil {
		return tools.Result{}, fmt.Errorf("browser: sidecar call %q: %w", toolName, err)
	}

	// 7. Parse the Playwright MCP response and map it to tools.Result.
	var pw playwrightResult
	if err := json.Unmarshal(raw, &pw); err != nil {
		return tools.Result{}, fmt.Errorf("browser: parsing sidecar response for %q: %w", toolName, err)
	}

	return e.mapResult(toolName, pw)
}

// mapResult converts a Playwright MCP tools/call response to a tools.Result.
//
// Playwright MCP returns one of three shapes:
//   - text-only content: {"content":[{"type":"text","text":"..."}],"isError":false}
//   - image content:     {"content":[{"type":"image","data":"<base64>","mimeType":"image/png"}],...}
//   - error:             {"content":[{"type":"text","text":"error msg"}],"isError":true}
//
// Mixed responses (text + image) are supported: text goes into Output, images into ImageParts.
func (e *Executor) mapResult(toolName string, pw playwrightResult) (tools.Result, error) {
	if pw.IsError {
		// Collect all text content to form the error message.
		var parts []string
		for _, c := range pw.Content {
			if c.Type == "text" && c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		msg := strings.Join(parts, "; ")
		if msg == "" {
			msg = fmt.Sprintf("browser tool %q returned an error (no message)", toolName)
		}
		return tools.Result{}, fmt.Errorf("browser: %s", msg)
	}

	var textParts []string
	var imageParts []tools.ImagePart

	for _, c := range pw.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				textParts = append(textParts, c.Text)
			}
		case "image":
			decoded, err := base64.StdEncoding.DecodeString(c.Data)
			if err != nil {
				// Log and skip rather than failing the whole call — the text
				// result (if any) may still be useful.
				e.registry.logger.Warn("browser: failed to decode image data",
					"tool", toolName, "error", err)
				continue
			}
			mimeType := c.MimeType
			if mimeType == "" {
				mimeType = "image/png"
			}
			imageParts = append(imageParts, tools.ImagePart{
				MIMEType: mimeType,
				Data:     decoded,
			})
		}
	}

	result := tools.Result{ImageParts: imageParts}

	if len(textParts) > 0 {
		joined := strings.Join(textParts, "\n")
		encoded, err := json.Marshal(joined)
		if err != nil {
			return tools.Result{}, fmt.Errorf("browser: marshaling text output: %w", err)
		}
		result.Output = json.RawMessage(encoded)
	}

	return result, nil
}
