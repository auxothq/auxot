// Package cliworker implements the CLI worker execution path for the auxot-worker binary.
// Instead of spawning llama.cpp, a CLI worker spawns a local AI CLI tool (claude, cursor, codex)
// and streams tokens back to the server via the same WebSocket protocol.
package cliworker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/auxothq/auxot/pkg/protocol"
)

// JobConfig carries per-execution settings derived from the worker policy.
type JobConfig struct {
	// ClaudePath is the path to the claude binary; defaults to "claude".
	ClaudePath string
	// Model is the --model flag value, e.g. "claude-sonnet-4-5"; empty = CLI default.
	Model string
	// BuiltinTools lists which claude CLI built-in tools to enable.
	// nil/empty → only MCP tools (--tools "").
	// ["default"] → all built-in tools.
	// Any other list → specific tools, e.g. ["Bash","WebSearch"].
	BuiltinTools []string
}

// RunJob executes a single job using the Claude Code CLI and streams results
// via the provided callbacks.
//
// Tool use: if the job contains tools, a temporary MCP server (this binary in --mcp-mode)
// is started to advertise tool schemas to claude (initialize + tools/list only).
// Claude's --permission-prompt-tool stdio flag causes it to emit control_request
// events before calling MCP tools. We deny these via stdin, preventing any MCP
// round-trip, and capture tool calls from the assistant stream.
//
// Batch detection: rate_limit_event is emitted by Claude CLI after all tool_use
// blocks from a model turn have been streamed. Once we see it and have collected
// MCP tool calls, we close stdin to prevent Claude from retrying after denials.
// The caller (main.go) executes tools server-side and dispatches the next job.
func RunJob(
	ctx context.Context,
	job protocol.JobMessage,
	cfg JobConfig,
	onToken func(string) error,
	onReasoningToken func(string) error,
	// onBuiltinTool is called in real-time when a CLI-native tool completes (before text tokens).
	// May be nil if the caller does not need real-time reporting.
	onBuiltinTool func(id, name, args, result string) error,
	onComplete func(fullResponse, reasoningContent string, cacheTokens, inputTokens, outputTokens, reasoningTokens int, durationMS int64, toolCalls []protocol.ToolCall) error,
	onError func(errMsg, details string) error,
) {
	log := slog.Default()

	claudePath := cfg.ClaudePath
	if claudePath == "" {
		claudePath = "claude"
	}

	claudeCtx, cancelClaude := context.WithCancel(ctx)
	defer cancelClaude()

	// Build a set of MCP tool names so buildPrompt can re-prefix bare names
	// in conversation history to match what Claude CLI sees in its tool list.
	mcpToolNames := make(map[string]string, len(job.Tools))
	for _, t := range job.Tools {
		mcpToolNames[t.Function.Name] = "mcp__auxot__" + t.Function.Name
	}
	systemPrompt, prompt := buildPrompt(job.Messages, mcpToolNames)

	effectiveBuiltins := filterShadowedBuiltins(cfg.BuiltinTools, job.Tools)
	toolsFlag := buildToolsFlag(effectiveBuiltins)
	hasTools := len(job.Tools) > 0

	args := []string{
		"--output-format", "stream-json",
		"--verbose",
		"--tools", toolsFlag,
	}

	if hasTools {
		// Permission-based interception: claude emits control_request events
		// on stdout before calling MCP tools. We deny them via stdin and
		// capture tool calls from assistant events — MCP stub never receives
		// tools/call.
		args = append(args,
			"--print",
			"--input-format", "stream-json",
			"--permission-prompt-tool", "stdio",
		)
	} else {
		args = append(args, "--dangerously-skip-permissions")
	}

	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if systemPrompt != "" {
		args = append(args, "--system-prompt", systemPrompt)
	}
	if !hasTools {
		args = append(args, "-p", prompt)
	}

	// MCP setup stays — needed for tool schema (initialize + tools/list).
	// With permission-based interception, tools/call is never reached.
	var mcpCleanup func()
	if hasTools {
		var mcpArgs []string
		var err error
		mcpArgs, mcpCleanup, err = setupMCP(job.Tools)
		if err != nil {
			log.Error("mcp_setup_failed", "error", err)
		} else {
			args = append(args, "--strict-mcp-config")
			args = append(args, mcpArgs...)
		}
	}
	defer func() {
		if mcpCleanup != nil {
			mcpCleanup()
		}
	}()

	cmd := exec.CommandContext(claudeCtx, claudePath, args...)
	// Run in a neutral directory so Claude Code does not discover CLAUDE.md
	// files from the server process's working directory ancestry. The system
	// prompt is provided explicitly via --system-prompt; project context files
	// must not override it.
	cmd.Dir = os.TempDir()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = onError("failed to create stdout pipe", err.Error())
		return
	}

	var stdinPipe io.WriteCloser
	if hasTools {
		stdinPipe, err = cmd.StdinPipe()
		if err != nil {
			_ = onError("failed to create stdin pipe", err.Error())
			return
		}
	}

	if startErr := cmd.Start(); startErr != nil {
		log.Error("claude_start_failed", "error", startErr)
		_ = onError("failed to start claude", startErr.Error())
		return
	}

	if hasTools && stdinPipe != nil {
		userMsg := map[string]any{
			"type": "user",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": prompt},
				},
			},
		}
		if err := json.NewEncoder(stdinPipe).Encode(userMsg); err != nil {
			log.Error("stdin_write_failed", "error", err)
			_ = onError("failed to write user message", err.Error())
			return
		}
	}

	var (
		fullText       strings.Builder
		preToolText    strings.Builder
		fullReasoning  strings.Builder
		inputTokens    int
		outputTokens   int
		toolCalls      []protocol.ToolCall
		seenMCPTool    bool
		batchComplete  bool // set true on rate_limit_event
		pendingBuiltin = map[string]*protocol.BuiltinToolUse{}
	)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var event claudeEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		switch event.Type {
		case "assistant":
			if event.Message == nil {
				continue
			}
			// If we already closed the batch (MCP tools collected, stdin closed),
			// ignore the retry turn Claude generates after permission denials.
			if batchComplete && len(toolCalls) > 0 {
				continue
			}

			for _, block := range event.Message.Content {
				switch block.Type {
				case "text":
					if block.Text != "" {
						fullText.WriteString(block.Text)
						if !seenMCPTool {
							preToolText.WriteString(block.Text)
							_ = onToken(block.Text)
						}
					}
				case "thinking":
					if block.Thinking != "" {
						fullReasoning.WriteString(block.Thinking)
						if !seenMCPTool {
							_ = onReasoningToken(block.Thinking)
						}
					}
				case "tool_use":
					if strings.HasPrefix(block.Name, "mcp__") {
						seenMCPTool = true
						tc := protocol.ToolCall{
							ID:   block.ID,
							Type: "function",
							Function: protocol.ToolFunction{
								Name:      stripMCPPrefix(block.Name),
								Arguments: string(block.Input),
							},
						}
						toolCalls = append(toolCalls, tc)
						// Stream MCP tool calls as real-time events so the UI
						// can show what tools the agent is requesting.
						if onBuiltinTool != nil {
							_ = onBuiltinTool(block.ID, tc.Function.Name, tc.Function.Arguments, "")
						}
					} else {
						pendingBuiltin[block.ID] = &protocol.BuiltinToolUse{
							ID:        block.ID,
							Name:      block.Name,
							Arguments: string(block.Input),
						}
					}
				}
			}

			if event.Message.Usage != nil {
				inputTokens = event.Message.Usage.InputTokens
				outputTokens = event.Message.Usage.OutputTokens
			}

		case "user":
			if event.Message == nil {
				continue
			}
			for _, block := range event.Message.Content {
				if block.Type != "tool_result" || block.ToolUseID == "" {
					continue
				}
				if pending, ok := pendingBuiltin[block.ToolUseID]; ok {
					pending.Result = block.ResultContent
					if onBuiltinTool != nil {
						_ = onBuiltinTool(pending.ID, pending.Name, pending.Arguments, pending.Result)
					}
					delete(pendingBuiltin, block.ToolUseID)
				}
			}

		case "rate_limit_event":
			batchComplete = true
			// If we collected MCP tool calls, the model turn is done.
			// Close stdin to prevent Claude from wasting tokens on a retry
			// turn after the permission denials.
			if len(toolCalls) > 0 && stdinPipe != nil {
				log.Info("batch_complete",
					"mcp_tool_calls", len(toolCalls),
					"builtin_pending", len(pendingBuiltin))
				stdinPipe.Close()
				stdinPipe = nil
				cancelClaude()
			}

		case "result":
			if event.Usage != nil {
				if event.Usage.InputTokens > inputTokens {
					inputTokens = event.Usage.InputTokens
				}
				if event.Usage.OutputTokens > outputTokens {
					outputTokens = event.Usage.OutputTokens
				}
			}
			if stdinPipe != nil {
				stdinPipe.Close()
				stdinPipe = nil
			}

		case "control_request":
			if event.Request == nil || event.Request.Subtype != "can_use_tool" || stdinPipe == nil {
				continue
			}
			toolName := event.Request.ToolName

			if strings.HasPrefix(toolName, "mcp__") {
				resp := map[string]any{
					"type": "control_response",
					"response": map[string]any{
						"subtype":    "success",
						"request_id": event.RequestID,
						"response": map[string]any{
							"behavior": "deny",
							"message":  "Tool execution is handled externally",
						},
					},
				}
				if err := json.NewEncoder(stdinPipe).Encode(resp); err != nil {
					log.Error("control_response_failed", "error", err)
				}
			} else {
				// Built-in Claude tools (Bash, WebSearch, Read, Write, etc.) — allow them.
				var updatedInput any
				if len(event.Request.Input) > 0 {
					_ = json.Unmarshal(event.Request.Input, &updatedInput)
				}
				if updatedInput == nil {
					updatedInput = map[string]any{}
				}
				resp := map[string]any{
					"type": "control_response",
					"response": map[string]any{
						"subtype":    "success",
						"request_id": event.RequestID,
						"response": map[string]any{
							"behavior":     "allow",
							"updatedInput": updatedInput,
						},
					},
				}
				_ = json.NewEncoder(stdinPipe).Encode(resp)
			}
		}
	}

	if stdinPipe != nil {
		stdinPipe.Close()
	}

	for _, pending := range pendingBuiltin {
		if onBuiltinTool != nil {
			_ = onBuiltinTool(pending.ID, pending.Name, pending.Arguments, "")
		}
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			// Context cancelled (we killed Claude after collecting tool calls) — this is expected.
			if len(toolCalls) > 0 || fullText.Len() > 0 {
				goto complete
			}
			return
		}
		if fullText.Len() == 0 && len(toolCalls) == 0 {
			log.Error("claude_exited_with_error", "error", err)
			_ = onError(fmt.Sprintf("claude exited: %v", err), "")
			return
		}
	}

complete:
	responseText := fullText.String()
	if len(toolCalls) > 0 {
		responseText = preToolText.String()
	}
	_ = onComplete(responseText, fullReasoning.String(), 0, inputTokens, outputTokens, 0, 0, toolCalls)
}

// setupMCP writes the job tools to a temp file and creates an mcp-config JSON file
// that instructs claude to spawn this binary as an MCP stdio subprocess.
func setupMCP(tools []protocol.Tool) (args []string, cleanup func(), err error) {
	toolsData, err := json.Marshal(tools)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling tools: %w", err)
	}

	toolsFile, err := os.CreateTemp("", "auxot-tools-*.json")
	if err != nil {
		return nil, nil, fmt.Errorf("creating tools temp file: %w", err)
	}
	if _, err := toolsFile.Write(toolsData); err != nil {
		toolsFile.Close()
		os.Remove(toolsFile.Name())
		return nil, nil, err
	}
	toolsFile.Close()

	// Use the current executable so the same binary handles MCP mode.
	workerBin, err := os.Executable()
	if err != nil {
		os.Remove(toolsFile.Name())
		return nil, nil, fmt.Errorf("resolving executable path: %w", err)
	}

	mcpConfig := map[string]any{
		"mcpServers": map[string]any{
			"auxot": map[string]any{
				"command": workerBin,
				"args":    []string{},
				"env": map[string]string{
					// Triggers MCP stdio mode in main.go.
					"AUXOT_MCP_TOOLS_FILE": toolsFile.Name(),
				},
			},
		},
	}
	configData, err := json.Marshal(mcpConfig)
	if err != nil {
		os.Remove(toolsFile.Name())
		return nil, nil, err
	}

	configFile, err := os.CreateTemp("", "auxot-mcp-*.json")
	if err != nil {
		os.Remove(toolsFile.Name())
		return nil, nil, err
	}
	if _, err := configFile.Write(configData); err != nil {
		configFile.Close()
		os.Remove(toolsFile.Name())
		os.Remove(configFile.Name())
		return nil, nil, err
	}
	configFile.Close()

	cleanup = func() {
		os.Remove(toolsFile.Name())
		os.Remove(configFile.Name())
	}
	args = []string{"--mcp-config", configFile.Name()}
	return args, cleanup, nil
}

// buildPrompt extracts the system message and formats the conversation history
// into a single prompt string for claude's --print mode.
//
// For tool result turns (role "tool"), the content is formatted so claude understands
// it received the result of a tool call it previously requested.
func buildPrompt(messages []protocol.ChatMessage, mcpToolNames map[string]string) (systemPrompt, prompt string) {
	var turns []string

	for _, msg := range messages {
		switch msg.Role {
		case "system":
			systemPrompt = msg.ContentString()
		case "user":
			turns = append(turns, "[user]\n"+msg.ContentString())
		case "assistant":
			// Reconstruct assistant turn — may include tool calls.
			// Re-prefix bare tool names to MCP names so the conversation
			// history matches Claude CLI's tool list.
			text := reconstructAssistantTurn(msg, mcpToolNames)
			if text != "" {
				turns = append(turns, "[assistant]\n"+text)
			}
		case "tool":
			// Tool result — format so claude sees it as a structured response.
			toolID := ""
			if msg.ToolCallID != "" {
				toolID = fmt.Sprintf(" (id: %s)", msg.ToolCallID)
			}
			turns = append(turns, fmt.Sprintf("[tool_result%s]\n%s", toolID, msg.ContentString()))
		}
	}

	// Encourage the model to emit multiple tool_use blocks in a single response
	// when tool results are independent, enabling parallel execution.
	const parallelInstr = "When you need to call multiple tools and their results are independent of each other, include ALL of the tool_use blocks in a single response rather than calling them one at a time. This enables parallel execution and faster responses."
	if systemPrompt != "" {
		systemPrompt = systemPrompt + "\n\n" + parallelInstr
	} else {
		systemPrompt = parallelInstr
	}

	if len(turns) == 0 {
		return
	}

	last := turns[len(turns)-1]
	history := turns[:len(turns)-1]

	if len(history) > 0 {
		prompt = "<conversation_history>\n" +
			strings.Join(history, "\n\n") +
			"\n</conversation_history>\n\n" +
			stripRolePrefix(last)
	} else {
		prompt = stripRolePrefix(last)
	}
	return
}

// reconstructAssistantTurn rebuilds an assistant message that may carry tool calls
// (i.e. the OpenAI-format ToolCalls field on the message).
func reconstructAssistantTurn(msg protocol.ChatMessage, mcpToolNames map[string]string) string {
	text := msg.ContentString()
	var parts []string
	if text != "" {
		parts = append(parts, text)
	}
	for _, tc := range msg.ToolCalls {
		// Re-prefix bare names (e.g. "web_search") to MCP names
		// (e.g. "mcp__auxot__web_search") so the conversation history
		// matches the tool names in Claude CLI's tool list.
		name := tc.Function.Name
		if prefixed, ok := mcpToolNames[name]; ok {
			name = prefixed
		}
		parts = append(parts, fmt.Sprintf("<tool_use name=%q id=%q>\n%s\n</tool_use>",
			name, tc.ID, tc.Function.Arguments))
	}
	return strings.Join(parts, "\n")
}

// filterShadowedBuiltins removes built-in tool names that collide with tools
// provided by the API caller. Colliding tools will be exposed via MCP instead
// so Claude calls them through the caller's tool resolution, not its own.
func filterShadowedBuiltins(builtins []string, jobTools []protocol.Tool) []string {
	if len(builtins) == 0 || len(jobTools) == 0 {
		return builtins
	}
	incoming := make(map[string]bool, len(jobTools))
	for _, t := range jobTools {
		incoming[t.Function.Name] = true
	}
	var filtered []string
	for _, b := range builtins {
		if !incoming[b] {
			filtered = append(filtered, b)
		}
	}
	return filtered
}

// buildToolsFlag converts the BuiltinTools policy list into the value for --tools.
// Empty/nil → "" (disable all built-ins).
// Any other list → comma-joined names, e.g. "Bash,Read,WebSearch".
func buildToolsFlag(tools []string) string {
	if len(tools) == 0 {
		return ""
	}
	return strings.Join(tools, ",")
}

// stripMCPPrefix removes the "mcp__<server>__" prefix that claude CLI adds to MCP tool names.
// e.g. "mcp__auxot__get_license_status" → "get_license_status"
//      "mcp__auxot__auxot"              → "auxot"
//      "my_tool"                        → "my_tool" (unchanged)
func stripMCPPrefix(name string) string {
	// Format is mcp__<server>__<tool> — find the second "__" and take everything after.
	if !strings.HasPrefix(name, "mcp__") {
		return name
	}
	parts := strings.SplitN(name, "__", 3)
	if len(parts) == 3 {
		return parts[2]
	}
	return name
}

func stripRolePrefix(turn string) string {
	for _, prefix := range []string{"[user]\n", "[assistant]\n", "[tool_result]\n"} {
		if strings.HasPrefix(turn, prefix) {
			return strings.TrimPrefix(turn, prefix)
		}
	}
	// Handle [tool_result (id: ...)] prefixes.
	if idx := strings.Index(turn, "]\n"); idx != -1 && strings.HasPrefix(turn, "[") {
		return turn[idx+2:]
	}
	return turn
}

// ── Claude NDJSON types ────────────────────────────────────────────────────────

type claudeEvent struct {
	Type    string         `json:"type"`
	Message *claudeMessage `json:"message,omitempty"`
	Usage   *claudeUsage   `json:"usage,omitempty"`
	// control_request fields (permission-prompt-tool stdio protocol)
	RequestID string               `json:"request_id,omitempty"`
	Request   *claudeControlRequest `json:"request,omitempty"`
}

type claudeControlRequest struct {
	Subtype   string          `json:"subtype"`
	ToolName  string          `json:"tool_name"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
}

type claudeMessage struct {
	ID         string        `json:"id,omitempty"`
	Content    []claudeBlock `json:"content"`
	Usage      *claudeUsage  `json:"usage,omitempty"`
	StopReason *string       `json:"stop_reason"`
}

type claudeBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
	// tool_use fields (assistant → MCP or builtin)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result fields (user → result of builtin tool execution)
	ToolUseID     string `json:"tool_use_id,omitempty"`
	ResultContent string `json:"content,omitempty"` // plain text or JSON result
}

type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
