package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/auxothq/auxot/pkg/tools"
)

// AllowedTools is the set of Playwright MCP tool names that may be individually
// registered. Context-lifecycle tools (e.g. browser_close, browser_new_context)
// are intentionally omitted — the registry owns those.
var AllowedTools = map[string]bool{
	"browser_navigate":         true,
	"browser_navigate_back":    true,
	"browser_click":            true,
	"browser_hover":            true,
	"browser_drag":             true,
	"browser_type":             true,
	"browser_press_key":        true,
	"browser_fill_form":        true,
	"browser_select_option":    true,
	"browser_file_upload":      true,
	"browser_take_screenshot":  true,
	"browser_snapshot":         true,
	"browser_evaluate":         true,
	"browser_run_code":         true,
	"browser_wait_for":         true,
	"browser_network_requests": true,
	"browser_console_messages": true,
	"browser_handle_dialog":    true,
	"browser_resize":           true,
}

// AllowedToolNames returns the sorted list of allowed Playwright MCP tool names.
func AllowedToolNames() []string {
	names := make([]string, 0, len(AllowedTools))
	for name := range AllowedTools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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

// PerToolExecutor executes a single named Playwright MCP tool against the sidecar.
// One instance is registered per allowed tool name so the LLM can call each tool
// directly — e.g. browser_navigate({ url: "https://example.com" }) — with a proper
// schema instead of the opaque aggregate browser({ tool_name, params }) wrapper.
type PerToolExecutor struct {
	toolName string
	registry *Registry
}

// NewPerToolExecutor creates a PerToolExecutor for the named Playwright MCP tool.
func NewPerToolExecutor(toolName string, registry *Registry) *PerToolExecutor {
	return &PerToolExecutor{toolName: toolName, registry: registry}
}

// Execute implements tools.Executor.
//
// args must be a JSON object containing the parameters for this specific Playwright
// MCP tool (e.g. {"url": "https://example.com"} for browser_navigate).
// The thread_id is read from ctx via tools.ThreadIDFromContext for per-thread
// session isolation — an empty thread_id is a hard error to prevent cross-tenant
// browser state leakage.
func (e *PerToolExecutor) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	// Resolve the thread ID from context for session isolation.
	threadID := tools.ThreadIDFromContext(ctx)
	if threadID == "" {
		return tools.Result{}, fmt.Errorf("browser: %s requires a thread_id in context; ensure the job's reference_id is set", e.toolName)
	}

	// Get or create the per-thread browser session (SSE connection + MCP handshake).
	sess, err := e.registry.GetOrCreate(threadID)
	if err != nil {
		return tools.Result{}, fmt.Errorf("browser: session for thread %q: %w", threadID, err)
	}

	// Pass args directly as the MCP arguments — the LLM sends the actual Playwright
	// params with no wrapper. Default to an empty JSON object when omitted entirely.
	arguments := args
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}

	// Call the sidecar via the registry (which touches lastUsed on the session).
	raw, err := e.registry.Call(ctx, sess, "tools/call", map[string]any{
		"name":      e.toolName,
		"arguments": arguments,
	})
	if err != nil {
		return tools.Result{}, fmt.Errorf("browser: sidecar call %q: %w", e.toolName, err)
	}

	// Parse the Playwright MCP response and map it to tools.Result.
	var pw playwrightResult
	if err := json.Unmarshal(raw, &pw); err != nil {
		return tools.Result{}, fmt.Errorf("browser: parsing sidecar response for %q: %w", e.toolName, err)
	}

	return e.mapResult(pw)
}

// mapResult converts a Playwright MCP tools/call response to a tools.Result.
//
// Playwright MCP returns one of three shapes:
//   - text-only content: {"content":[{"type":"text","text":"..."}],"isError":false}
//   - image content:     {"content":[{"type":"image","data":"<base64>","mimeType":"image/png"}],...}
//   - error:             {"content":[{"type":"text","text":"error msg"}],"isError":true}
//
// Mixed responses (text + image) are supported: text goes into Output, images into ImageParts.
func (e *PerToolExecutor) mapResult(pw playwrightResult) (tools.Result, error) {
	if pw.IsError {
		var parts []string
		for _, c := range pw.Content {
			if c.Type == "text" && c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		msg := strings.Join(parts, "; ")
		if msg == "" {
			msg = fmt.Sprintf("browser tool %q returned an error (no message)", e.toolName)
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
					"tool", e.toolName, "error", err)
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

// RegisterAll registers a PerToolExecutor for each allowed Playwright MCP tool name
// in the given tools registry. After this call, the LLM invokes each browser action
// directly with its proper schema — e.g. browser_navigate({ url: "..." }) — rather
// than routing through an opaque aggregate wrapper.
func RegisterAll(reg interface {
	Register(string, tools.Executor)
}, registry *Registry) {
	for _, name := range AllowedToolNames() {
		exec := NewPerToolExecutor(name, registry)
		reg.Register(name, exec.Execute)
	}
}
