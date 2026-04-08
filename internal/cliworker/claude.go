// Package cliworker implements the CLI worker execution path for the auxot-worker binary.
// Instead of spawning llama.cpp, a CLI worker spawns a local AI CLI tool (claude, cursor, codex)
// and streams tokens back to the server via the same WebSocket protocol.
package cliworker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/auxothq/auxot/pkg/protocol"
	"github.com/google/uuid"
)

const (
	// DefaultWorkerDir is the working directory for the claude subprocess.
	// A neutral, fixed path prevents accidental CLAUDE.md discovery from
	// whatever directory the server process happens to be running in.
	DefaultWorkerDir = "/tmp/auxot-worker"
	// DefaultConfigDir is CLAUDE_CONFIG_DIR for the claude subprocess.
	// Isolating configuration here keeps sessions, memory, and caches
	// away from the developer's personal ~/.claude directory.
	DefaultConfigDir = "/tmp/auxot-worker/.claude"
)

// syntheticSessionUUID is the fixed session ID written into every synthetic
// JSONL session file. Claude CLI reads it back as metadata but does not use
// it for any lookup once a file path is passed directly to --resume.
const syntheticSessionUUID = "00000000-0000-4000-a000-000000000000"

// JobConfig carries per-execution settings derived from the worker policy.
type JobConfig struct {
	// ClaudePath is the path to the claude binary; defaults to "claude".
	ClaudePath string
	// Model is the --model flag value, e.g. "claude-sonnet-4-6"; empty = CLI default.
	Model string
	// BuiltinTools lists which claude CLI built-in tools to enable.
	// nil/empty → only MCP tools (--tools "").
	// ["default"] → all built-in tools.
	// Any other list → specific tools, e.g. ["Bash","WebSearch"].
	BuiltinTools []string
	// WorkDir is cmd.Dir for the claude subprocess.
	// Defaults to DefaultWorkerDir if empty.
	WorkDir string
	// ConfigDir is the CLAUDE_CONFIG_DIR env var for the claude subprocess.
	// Defaults to DefaultConfigDir if empty.
	ConfigDir string
	// LiveMCP enables live-continuation mode: instead of deny-and-kill, tool calls
	// are executed in-band through the MCP connection. Claude runs to natural
	// completion, emitting a final result event. This eliminates the session-file
	// delta bug where only the last tool result was sent to Claude in multi-tool turns.
	//
	// Requires OnToolCall to be set.
	LiveMCP bool
	// OnToolCall is the bridge between the in-process live tool proxy and the server.
	// It is called by the HTTP proxy when the MCP subprocess forwards a tool call.
	// The implementation should send TypeJobToolCallRequest to the server via WebSocket
	// and block until TypeJobToolCallResult is received, then return the result.
	// Required when LiveMCP is true; ignored otherwise.
	OnToolCall func(jobID, callID, toolName, arguments string) (result string, isError bool, err error)
}


// workerEnv builds the subprocess environment for the claude CLI.
// It inherits the current process environment, overlays the variables that
// control Claude's runtime behaviour, and injects per-job credentials so that
// builtin tools (Bash, etc.) see them as ordinary shell environment variables.
func workerEnv(configDir, proxyAddr string, credentials map[string]string) []string {
	env := os.Environ()
	overlay := []string{
		"CLAUDE_CONFIG_DIR=" + configDir,
		// Prevent Claude from creating per-user memory files that could
		// bleed context between different users' sessions on the same worker.
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
		// No background prefetch, cron jobs, or CLI feedback surveys on a server VM.
		"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1",
		"CLAUDE_CODE_DISABLE_CRON=1",
		"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1",
		// Suppress non-essential outbound traffic (update checks, etc.).
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_TELEMETRY=1",
		// When the JSONL session file ends on a user(tool_result) message,
		// Claude CLI detects it as an interrupted_turn and auto-resumes by
		// injecting "Continue from where you left off." without requiring
		// any stdin prompt. Safe to always set: the flag is a no-op when the
		// last message is an assistant (kind='none'), which is the normal path.
		"CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1",
	}
	if proxyAddr != "" {
		overlay = append(overlay, "ANTHROPIC_BASE_URL="+proxyAddr)
	}
	// Inject per-job credentials so Claude's Bash (and other builtin) tools
	// inherit them as environment variables. Appending last ensures they
	// override any same-named vars from the worker process environment.
	for k, v := range credentials {
		overlay = append(overlay, k+"="+v)
	}
	return append(env, overlay...)
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
//
// History injection: prior conversation turns (including tool_use / tool_result
// pairs with rich image content) are written to a temporary synthetic JSONL
// session file and loaded via --resume. This avoids the stream-json design gap
// where user messages are always treated as new prompts, which would cause
// Claude to re-execute tool calls from injected history.
func RunJob(
	ctx context.Context,
	job protocol.JobMessage,
	cfg JobConfig,
	onToken func(string) error,
	onReasoningToken func(string) error,
	// onBuiltinTool is called in real-time when a CLI-native tool starts or completes.
	// May be nil if the caller does not need real-time progress events.
	onBuiltinTool func(id, name, args, result string) error,
	// onComplete receives all completed CLI-native tool uses (builtinToolUses) alongside
	// the MCP tool calls so the server can persist them as part of the same turn.
	// preToolContent is the assistant text generated before any builtin tools ran
	// (empty when the model goes straight to a tool call).
	// postToolContent is the assistant text generated after builtin tools completed
	// (empty when there were no builtin tools, or when the turn ended with MCP tools).
	// The server uses pre/post to insert two separate assistant rows with tool_call
	// rows in between, preserving the actual execution order in the database.
	onComplete func(preToolContent, postToolContent, reasoningContent string, cacheTokens, inputTokens, outputTokens, reasoningTokens int, durationMS int64, toolCalls []protocol.ToolCall, builtinToolUses []protocol.BuiltinToolUse) error,
	onError func(errMsg, details string) error,
) {
	log := slog.Default()
	err := runJobOnce(ctx, job, cfg, onToken, onReasoningToken, onBuiltinTool, onComplete, onError)
	if err == errCacheControlRetry {
		log.Warn("cliworker: cache_control 400 detected, retrying")
		_ = runJobOnce(ctx, job, cfg, onToken, onReasoningToken, onBuiltinTool, onComplete, onError)
		return
	}
	if err == errPromptTooLong {
		// Trim the oldest non-system conversation messages and retry once.
		// Drop 20% of messages from the oldest end (excluding system prompt).
		trimmed := trimOldestMessages(job.Messages, 0.20)
		log.Warn("cliworker: prompt too long — retrying with trimmed history",
			"original_msgs", len(job.Messages),
			"trimmed_msgs", len(trimmed),
		)
		job.Messages = trimmed
		_ = runJobOnce(ctx, job, cfg, onToken, onReasoningToken, onBuiltinTool, onComplete, onError)
	}
}

// trimOldestMessages removes the oldest fraction of non-system messages,
// preserving the system prompt and the most recent turns.
func trimOldestMessages(msgs []protocol.ChatMessage, fraction float64) []protocol.ChatMessage {
	var sys []protocol.ChatMessage
	var conv []protocol.ChatMessage
	for _, m := range msgs {
		if m.Role == "system" {
			sys = append(sys, m)
		} else {
			conv = append(conv, m)
		}
	}
	drop := int(float64(len(conv)) * fraction)
	if drop < 1 {
		drop = 1
	}
	if drop >= len(conv) {
		drop = len(conv) / 2
	}
	return append(sys, conv[drop:]...)
}

var errCacheControlRetry = fmt.Errorf("cache_control_400_retry")
var errPromptTooLong = fmt.Errorf("prompt_too_long_retry")

func runJobOnce(
	ctx context.Context,
	job protocol.JobMessage,
	cfg JobConfig,
	onToken func(string) error,
	onReasoningToken func(string) error,
	onBuiltinTool func(id, name, args, result string) error,
	onComplete func(preToolContent, postToolContent, reasoningContent string, cacheTokens, inputTokens, outputTokens, reasoningTokens int, durationMS int64, toolCalls []protocol.ToolCall, builtinToolUses []protocol.BuiltinToolUse) error,
	onError func(errMsg, details string) error,
) error {
	log := slog.Default()

	claudePath := cfg.ClaudePath
	if claudePath == "" {
		claudePath = "claude"
	}

	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = DefaultWorkerDir
	}
	configDir := cfg.ConfigDir
	if configDir == "" {
		configDir = DefaultConfigDir
	}
	// Ensure both directories exist before launching the subprocess.
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		log.Warn("cliworker: could not create workDir", "path", workDir, "error", err)
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		log.Warn("cliworker: could not create configDir", "path", configDir, "error", err)
	}

	claudeCtx, cancelClaude := context.WithCancel(ctx)
	defer cancelClaude()

	// Build a set of MCP tool names so buildSessionFile can re-prefix bare names
	// in conversation history to match what Claude CLI sees in its tool list.
	mcpToolNames := make(map[string]string, len(job.Tools))
	for _, t := range job.Tools {
		mcpToolNames[t.Function.Name] = "mcp__auxot__" + t.Function.Name
	}

	effectiveBuiltins := filterShadowedBuiltins(cfg.BuiltinTools, job.Tools)
	toolsFlag := buildToolsFlag(effectiveBuiltins)
	hasTools := len(job.Tools) > 0

	effectiveModel := cfg.Model
	if effectiveModel == "" {
		effectiveModel = "claude-sonnet-4-6"
	}

	// Build a synthetic JSONL session file from conversation history so that
	// Claude CLI can load all prior turns — including tool_use / tool_result
	// pairs with rich content (images, etc.) — without re-executing any tools.
	// On the first turn there is no history, so sessionFilePath is empty and
	// we simply run with --no-session-persistence.
	sessionFilePath, systemPrompt, currentPrompt, currentImageBlocks, isContinuation, sfErr := buildSessionFile(
		job.Messages, mcpToolNames, workDir, effectiveModel,
	)
	if sfErr != nil {
		log.Error("cliworker: failed to write synthetic session file", "error", sfErr)
		_ = onError("failed to write synthetic session file", sfErr.Error())
		return nil
	}

	// sessionFileCleanup is set to true once the run succeeds.
	// On error we leave the file so the exact claude command can be replayed.
	sessionFileOK := false
	defer func() {
		if sessionFilePath != "" && sessionFileOK {
			os.Remove(sessionFilePath)
		}
	}()

	args := []string{
		"--output-format", "stream-json",
		"--verbose",
		"--tools", toolsFlag,
	}

	// Session flags: resume from the synthetic JSONL when there is prior history,
	// otherwise run stateless so no session file is written.
	if sessionFilePath != "" {
		args = append(args, "--resume", sessionFilePath)
	} else {
		args = append(args, "--no-session-persistence")
	}

	// Always use stream-json stdin so the conversation never touches the argv.
	// On the live-MCP path, --dangerously-skip-permissions lets Claude execute
	// tools via the MCP connection without permission prompts (the MCP server
	// subprocess forwards calls to our live proxy, which routes to the server).
	// On the deny-kill path, --permission-prompt-tool stdio intercepts MCP
	// permission requests so we can deny them after collecting tool_use blocks.
	// No-tools path: --dangerously-skip-permissions (no permission events anyway).
	args = append(args,
		"--print",
		"--input-format", "stream-json",
	)
	if hasTools && !cfg.LiveMCP {
		args = append(args, "--permission-prompt-tool", "stdio")
	} else {
		args = append(args, "--dangerously-skip-permissions")
	}

	// Always pass --model explicitly so the claude CLI never silently falls back
	// to the account's configured default (often claude-opus-4-6[1m] — the
	// 1-million-token context variant billed at premium rates).
	args = append(args, "--model", effectiveModel)
	var sysPromptTempFile string
	if systemPrompt != "" {
		// Write system prompt to a temp file and pass --system-prompt-file so the
		// content never appears on the process argv. Long system prompts passed
		// inline via --system-prompt can exceed the OS ARG_MAX limit
		// ("argument list too long"), killing the subprocess before it starts.
		// Wire behavior is identical: both flags append our content as the third
		// system block after the CLI's billing header and base identity sentence.
		spFile, spErr := os.CreateTemp("", "auxot-sysprompt-*.txt")
		if spErr != nil {
			log.Error("cliworker: failed to create system-prompt temp file", "error", spErr)
			_ = onError("failed to create system-prompt temp file", spErr.Error())
			return nil
		}
		if _, spErr = spFile.WriteString(systemPrompt); spErr != nil {
			spFile.Close()
			os.Remove(spFile.Name())
			log.Error("cliworker: failed to write system-prompt temp file", "error", spErr)
			_ = onError("failed to write system-prompt temp file", spErr.Error())
			return nil
		}
		spFile.Close()
		sysPromptTempFile = spFile.Name()
		args = append(args, "--system-prompt-file", sysPromptTempFile)
	}
	defer func() {
		if sysPromptTempFile != "" {
			os.Remove(sysPromptTempFile)
		}
	}()
	// MCP setup — needed for tool schema (initialize + tools/list).
	// In live mode, the MCP subprocess also executes tool calls by forwarding
	// them to the in-worker HTTP proxy. In deny-kill mode, tools/call responses
	// are stub errors; the actual execution happens server-side after the turn.
	var mcpCleanup func()
	if hasTools {
		var mcpArgs []string
		var setupErr error
		if cfg.LiveMCP && cfg.OnToolCall != nil {
			// Start the live tool proxy first so we have its address for the MCP config.
			proxy, proxyErr := startLiveToolProxy(claudeCtx, cfg.OnToolCall)
			if proxyErr != nil {
				log.Error("liveproxy_start_failed", "error", proxyErr)
				_ = onError("failed to start live tool proxy", proxyErr.Error())
				return nil
			}
			log.Info("cliworker: live tool proxy started", "addr", proxy.Addr())
			mcpArgs, mcpCleanup, setupErr = setupMCPLive(job.Tools, proxy.Addr(), job.JobID)
		} else {
			mcpArgs, mcpCleanup, setupErr = setupMCP(job.Tools)
		}
		if setupErr != nil {
			log.Error("mcp_setup_failed", "error", setupErr)
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
	// Fixed working directory — prevents CLAUDE.md discovery from the server
	// process's directory ancestry. System prompt is provided explicitly via
	// --system-prompt; project context files must not override it.
	cmd.Dir = workDir
	// Isolated environment: CLAUDE_CONFIG_DIR pins sessions/memory/caches to
	// a known path; the DISABLE_* vars strip all server-irrelevant behaviours.
	// job.Credentials are injected so Claude's Bash tool (and any other builtin)
	// sees them as ordinary shell environment variables.
	credKeys := make([]string, 0, len(job.Credentials))
	for k := range job.Credentials {
		credKeys = append(credKeys, k)
	}
	log.Info("cliworker: injecting credentials into subprocess env",
		"job_id", job.JobID, "credential_keys", credKeys, "count", len(job.Credentials))
	cmd.Env = workerEnv(configDir, "", job.Credentials)
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = onError("failed to create stdout pipe", err.Error())
		return nil
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		_ = onError("failed to create stdin pipe", err.Error())
		return nil
	}

	log.Info("claude_exec",
		"cwd", workDir,
		"path", claudePath,
		"session_file", sessionFilePath,
		"args", args,
	)
	if startErr := cmd.Start(); startErr != nil {
		log.Error("claude_start_failed", "error", startErr)
		_ = onError("failed to start claude", startErr.Error())
		return nil
	}

	{
		// Build content blocks for stream-json stdin.
		// For normal turns: the current user message (text + images).
		// For tool-continuation turns: a bare "Continue." so Claude synthesises
		// a final text response from the tool_result already in the session file.
		// CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1 is a no-op in --print mode;
		// Claude requires at least one stdin message before it will produce output.
		var contentBlocks []map[string]any
		if isContinuation {
			contentBlocks = []map[string]any{{"type": "text", "text": "Continue."}}
		} else {
			contentBlocks = append(contentBlocks, map[string]any{
				"type": "text",
				"text": currentPrompt,
			})
			// Append native image blocks from the current user message.
			contentBlocks = append(contentBlocks, currentImageBlocks...)
		}

		userMsg := map[string]any{
			"type": "user",
			"message": map[string]any{
				"role":    "user",
				"content": contentBlocks,
			},
		}
		if err := json.NewEncoder(stdinPipe).Encode(userMsg); err != nil {
			log.Error("stdin_write_failed", "error", err)
			_ = onError("failed to write user message", err.Error())
			return nil
		}
		stdinPipe.Close()
	}

	var (
		fullText          strings.Builder
		preToolText       strings.Builder // text before any builtin tool ran
		postToolText      strings.Builder // text after builtin tools completed
		fullReasoning     strings.Builder
		inputTokens       int
		outputTokens      int
		toolCalls         []protocol.ToolCall
		seenMCPTool       bool
		seenBuiltinResult bool // true after first tool_result (builtin or live-MCP) received
		batchComplete     bool // set true on rate_limit_event
		pendingBuiltin    = map[string]*protocol.BuiltinToolUse{}
		completedBuiltins []protocol.BuiltinToolUse
		// In live-MCP mode, tool results arrive back from Claude CLI as user events
		// (one tool_result block per event). We capture them here keyed by tool_use_id
		// so we can attach them to the corresponding toolCall at complete: time and
		// ship them as pre-resolved entries — preventing the server from re-dispatching
		// tool calls that the MCP subprocess already handled.
		liveToolResults = map[string]string{}
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
					switch {
					case seenMCPTool && !cfg.LiveMCP:
						// Deny-kill path: we killed Claude after collecting tool calls,
						// so this text is from the (suppressed) retry turn — discard.
					case seenBuiltinResult:
							// Post-builtin response: the agent's reply after tool results.
							postToolText.WriteString(block.Text)
							_ = onToken(block.Text)
						default:
							// Pre-tool or no-tool text.
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
				inputTokens = event.Message.Usage.totalInputTokens()
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
					pending.Result = block.ResultContent()
					if onBuiltinTool != nil {
						_ = onBuiltinTool(pending.ID, pending.Name, pending.Arguments, pending.Result)
					}
					completedBuiltins = append(completedBuiltins, *pending)
					delete(pendingBuiltin, block.ToolUseID)
					// Any assistant text after this point is the post-tool response.
					seenBuiltinResult = true
					// If rate_limit_event already fired (MCP tools collected) and this was
					// the last pending builtin, kill Claude now — we have everything we need.
					if batchComplete && len(toolCalls) > 0 && len(pendingBuiltin) == 0 && stdinPipe != nil {
						log.Info("batch_complete_builtins_done",
							"mcp_tool_calls", len(toolCalls),
							"completed_builtins", len(completedBuiltins))
						stdinPipe.Close()
						stdinPipe = nil
						cancelClaude()
					}
				} else if cfg.LiveMCP {
					// In live-MCP mode, each MCP tool result echoes back as a separate
					// user event with a single tool_result block (one event per tool,
					// not one event for all tools). Capture the result text and signal
					// that any subsequent assistant text is the post-tool response.
				liveToolResults[block.ToolUseID] = block.ResultContent()
				seenBuiltinResult = true
				log.Info("cliworker: live-MCP tool result captured",
					"tool_use_id", block.ToolUseID,
					"result_len", len(liveToolResults[block.ToolUseID]))
				}
			}

		case "rate_limit_event":
			// In live-MCP mode, rate_limit_event just signals the end of one
			// inference batch. Claude is still running (waiting for tool results
			// from the MCP subprocess) — do NOT kill it. It will emit a `result`
			// event when it reaches natural completion.
			if cfg.LiveMCP {
				log.Info("cliworker: live-MCP rate_limit_event — Claude still running")
				break
			}

			batchComplete = true
			// rate_limit_event fires immediately after the model finishes streaming,
			// BEFORE the user event carrying builtin tool results.  Only kill Claude
			// once all pending builtin tools have delivered their results — otherwise
			// we'd discard the WebSearch / Bash result in a mixed turn.
			if len(toolCalls) > 0 && len(pendingBuiltin) == 0 && stdinPipe != nil {
				log.Info("batch_complete",
					"mcp_tool_calls", len(toolCalls),
					"builtin_pending", 0)
				stdinPipe.Close()
				stdinPipe = nil
				cancelClaude()
			} else if len(toolCalls) > 0 && len(pendingBuiltin) > 0 {
				log.Info("batch_complete_waiting_for_builtins",
					"mcp_tool_calls", len(toolCalls),
					"builtin_pending", len(pendingBuiltin))
			}

	case "result":
		if event.Usage != nil {
			if t := event.Usage.totalInputTokens(); t > inputTokens {
				inputTokens = t
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
			return nil
		}
		if fullText.Len() == 0 && len(toolCalls) == 0 {
			stderr := strings.TrimSpace(stderrBuf.String())
			if len(stderr) > 4096 {
				stderr = stderr[:4096] + "...(truncated)"
			}
			// Read-repair: if we see a cache_control 400, signal the caller to
			// retry (a fresh synthetic session file is written each time anyway).
			if strings.Contains(stderr, "cache_control") && strings.Contains(stderr, "maximum of 4") {
				log.Warn("cliworker: cache_control limit error detected, will retry")
				return errCacheControlRetry
			}
			log.Error("claude_exited_with_error",
				"error", err,
				"stderr", stderr,
				"stdout_so_far", fullText.String(),
				"session_file", sessionFilePath,
				"args", args,
			)
			_ = onError(fmt.Sprintf("claude exited: %v", err), stderr)
			return nil
		}
	}

complete:
	sessionFileOK = true // run completed; allow deferred cleanup to remove session file

	// In live-MCP mode, MCP tool calls were executed in-band by the MCP subprocess.
	// The CLI streams back each result as a separate user event (one tool_result block
	// per event — confirmed by probe recon). We captured those results into
	// liveToolResults above. Now convert toolCalls to pre-resolved completedBuiltins
	// so the server stores them as completed tool_call rows and does NOT re-dispatch
	// them. Sending them as unresolved toolCalls would trigger a second inference turn.
	if cfg.LiveMCP && len(toolCalls) > 0 {
		for _, tc := range toolCalls {
			result := liveToolResults[tc.ID] // empty string if result not captured
			completedBuiltins = append(completedBuiltins, protocol.BuiltinToolUse{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
				Result:    result,
			})
		}
		log.Info("cliworker: live-MCP converted tool calls to pre-resolved",
			"count", len(toolCalls),
			"results_captured", len(liveToolResults))
		toolCalls = nil
	}

	// Detect Claude CLI's "Prompt is too long" synthetic response. Claude Code
	// emits this as a plain-text assistant message when the API rejects the
	// request for exceeding the context limit. Surface it as a retryable signal
	// so RunJob can trim history and retry rather than delivering it as output.
	responseText := fullText.String()
	if len(toolCalls) == 0 && len(completedBuiltins) == 0 &&
		strings.Contains(strings.ToLower(responseText), "prompt is too long") {
		log.Warn("cliworker: detected 'Prompt is too long' response — will retry with trimmed history")
		return errPromptTooLong
	}

	var preToolContent, postToolContent string
	switch {
	case len(toolCalls) > 0:
		// Deny-kill mode MCP turn: response is whatever the agent said before
		// calling MCP tools. Claude is killed after we collect the tool calls,
		// so there is no post-tool text from this run.
		preToolContent = preToolText.String()

	case len(completedBuiltins) > 0:
		// Tool turn (builtin or live-MCP): separate the pre-tool preamble from the
		// post-tool response so the server inserts two distinct assistant rows with
		// the tool_call rows in between, preserving the actual execution order.
		preToolContent = preToolText.String()
		postToolContent = postToolText.String()

	default:
		// Plain text turn: no tools at all.
		preToolContent = responseText
	}
	_ = onComplete(preToolContent, postToolContent, fullReasoning.String(), 0, inputTokens, outputTokens, 0, 0, toolCalls, completedBuiltins)
	return nil
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

// setupMCPLive is like setupMCP but for live-continuation mode. It sets two
// additional env vars in the MCP subprocess so that tools/call is forwarded to
// the in-worker HTTP proxy instead of returning a stub error.
func setupMCPLive(tools []protocol.Tool, proxyURL, jobID string) (args []string, cleanup func(), err error) {
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
					"AUXOT_MCP_TOOLS_FILE":  toolsFile.Name(),
					"AUXOT_MCP_TOOL_PROXY":  proxyURL,
					"AUXOT_MCP_JOB_ID":      jobID,
				},
			},
		},
	}
	configData, err := json.Marshal(mcpConfig)
	if err != nil {
		os.Remove(toolsFile.Name())
		return nil, nil, err
	}

	configFile, err := os.CreateTemp("", "auxot-mcp-live-*.json")
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

// parallelToolInstr is appended to every system prompt to encourage the model
// to emit parallel tool_use blocks and to never narrate without acting.
const parallelToolInstr = "IMPORTANT: You are operating in an environment where ALL actions must be performed via tool calls. " +
	"Never describe what you are about to do without also including the tool_use block in the same response — narrating an action is not the same as performing it. " +
	"If you intend to run a command, read a file, search the web, or take any other action, include the tool_use block immediately in your current response. " +
	"When you need to call multiple tools and their results are independent of each other, include ALL of the tool_use blocks in a single response rather than calling them one at a time. This enables parallel execution and faster responses."

// buildSessionFile writes a synthetic Claude CLI JSONL session file containing
// all conversation history up to (but not including) the last user message.
//
// The session file is in the exact format that Claude CLI expects when loading
// via --resume /path/to/file.jsonl. This lets Claude see the full prior history
// — including tool_use / tool_result pairs with rich content (images, etc.) —
// without re-executing any tool calls.
//
// Returns:
//   - path: temp file path (caller must defer os.Remove); empty when there is no history.
//   - systemPrompt: the system message content (with parallelToolInstr appended).
//   - currentPrompt: the text of the last user message (to send via stdin).
//   - imageBlocks: Anthropic image content blocks from the last user message.
func buildSessionFile(
	messages []protocol.ChatMessage,
	mcpToolNames map[string]string,
	workDir string,
	model string,
) (path, systemPrompt, currentPrompt string, imageBlocks []map[string]any, isContinuation bool, err error) {
	// Separate system prompt from conversation messages.
	var conv []protocol.ChatMessage
	for _, msg := range messages {
		if msg.Role == "system" {
			systemPrompt = msg.ContentString()
		} else {
			conv = append(conv, msg)
		}
	}

	if systemPrompt != "" {
		systemPrompt = systemPrompt + "\n\n" + parallelToolInstr
	} else {
		systemPrompt = parallelToolInstr
	}

	// Identify the last user message — that is the current turn to send via stdin.
	lastUserIdx := -1
	for i := len(conv) - 1; i >= 0; i-- {
		if conv[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}

	if lastUserIdx < 0 {
		// No user message at all — nothing to send.
		return
	}
	{
		lastUser := conv[lastUserIdx]
		currentPrompt = lastUser.ContentString()
		imageBlocks = extractImageBlocks(lastUser.Content)
	}

	// Everything before the last user message becomes history in the session file.
	// The session JSONL must end with an assistant message — the Anthropic API
	// requires alternating user/assistant turns. System-injected user messages
	// (e.g. "Pre-compaction memory flush") can trail the last real assistant
	// response in the history slice; writing them to the JSONL would put two
	// consecutive user messages at the boundary (session tail + stdin current),
	// causing Claude CLI to exit with status 1 immediately.
	// Fix: trim history to end at the last assistant message.
	history := conv[:lastUserIdx]
	lastAssistantIdx := -1
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" {
			lastAssistantIdx = i
			break
		}
	}

	// Tool-continuation case: the caller (e.g. OpenClaw via the OpenAI endpoint)
	// sent [user, assistant(tool_use), tool_result] — the standard agentic pattern
	// where tool results need to be fed back for a final text response.
	//
	// Probe-verified fix (claude-probe synthetic-session-resume): write the FULL
	// chain [user, assistant(tool_use), user(tool_result)] into the session file.
	// The tool messages map to "user" entries with tool_result content blocks via
	// the case "tool" branch in the JSONL loop below.
	// CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1 (always set in workerEnv) causes
	// Claude CLI to detect the trailing user(tool_result) as an interrupted_turn
	// and auto-inject "Continue from where you left off." — no stdin needed.
	toolContinuation := false
	if lastUserIdx >= 0 && lastUserIdx < len(conv)-1 {
		tail := conv[lastUserIdx+1:]
		isToolChain := true
		hasToolMsg := false
		for _, m := range tail {
			if m.Role == "tool" {
				hasToolMsg = true
			} else if m.Role != "assistant" {
				isToolChain = false
				break
			}
		}
		if isToolChain && hasToolMsg {
			history = conv
			lastAssistantIdx = -1
			for k := len(history) - 1; k >= 0; k-- {
				if history[k].Role == "assistant" {
					lastAssistantIdx = k
					break
				}
			}
			// currentPrompt is not sent via stdin for continuations —
			// CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1 handles the auto-resume.
			currentPrompt = ""
			imageBlocks = nil
			toolContinuation = true
			isContinuation = true
		}
	}

	if lastAssistantIdx < 0 {
		// No assistant turn in history yet — first exchange, no session file.
		return
	}
	// Trim trailing non-assistant messages so the JSONL ends on an assistant
	// entry (required by the Anthropic API alternating-turn constraint).
	// Skip the trim for tool-continuation: those trailing "tool" messages must
	// stay because they become user(tool_result) entries that Claude needs.
	if !toolContinuation {
		trimmed := len(history) - (lastAssistantIdx + 1)
		history = history[:lastAssistantIdx+1]
		if trimmed > 0 {
			slog.Default().Info("cliworker: trimmed trailing non-assistant messages from session history",
				"trimmed", trimmed, "history_len", len(history))
		}
	}

	// Safety cap: if the session file would still carry an excessive number of
	// messages, drop the oldest turns (from the beginning) to stay well within
	// Claude CLI's context limit. We keep a generous cap because the server-side
	// preCompactMessages already redacts tool results; this is a last-ditch guard
	// against the server estimate being too optimistic.
	// Drop from the oldest end, always keeping the history ending on an assistant
	// message so the alternating turn constraint stays satisfied.
	const maxHistoryMessages = 120
	if len(history) > maxHistoryMessages {
		drop := len(history) - maxHistoryMessages
		history = history[drop:]
		slog.Default().Info("cliworker: capped session file history",
			"dropped", drop, "history_len", len(history))
	}

	// Resolve symlinks: Claude CLI records and looks up the real CWD.
	cwd := workDir
	if resolved, e := filepath.EvalSymlinks(workDir); e == nil {
		cwd = resolved
	}

	f, e := os.CreateTemp("", "auxot-session-*.jsonl")
	if e != nil {
		err = fmt.Errorf("create session file: %w", e)
		return
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	prevUUID := ""

	i := 0
	for i < len(history) {
		msg := history[i]
		entryUUID := uuid.New().String()

		var claudeRole string
		var content any

		switch msg.Role {
		case "user":
			claudeRole = "user"
			var blocks []map[string]any
			if t := msg.ContentString(); t != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": t})
			}
			blocks = append(blocks, extractImageBlocks(msg.Content)...)
			content = blocks
			i++

		case "assistant":
			claudeRole = "assistant"
			var blocks []map[string]any
			if t := msg.ContentString(); t != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": t})
			}
			for _, tc := range msg.ToolCalls {
				name := tc.Function.Name
				if prefixed, ok := mcpToolNames[name]; ok {
					name = prefixed
				}
				inputRaw := json.RawMessage(tc.Function.Arguments)
				if len(inputRaw) == 0 {
					inputRaw = json.RawMessage("{}")
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  name,
					"input": inputRaw,
				})
			}
			content = blocks
			i++

		case "tool":
			// Consecutive tool-result messages → one user message with multiple
			// tool_result content blocks (the canonical Anthropic API format).
			claudeRole = "user"
			var toolBlocks []map[string]any
			for i < len(history) && history[i].Role == "tool" {
				tr := history[i]
				toolBlocks = append(toolBlocks, map[string]any{
					"type":        "tool_result",
					"tool_use_id": tr.ToolCallID,
					"content":     buildToolResultContent(tr),
					"is_error":    false,
				})
				i++
			}
			content = toolBlocks

		default:
			i++
			continue
		}

		var parentUUID any
		if prevUUID != "" {
			parentUUID = prevUUID
		} else {
			parentUUID = nil // explicit null, not omitted
		}

		// Build the inner message object. For assistant turns, the Claude CLI
		// session loader expects the full Anthropic API message shape — including
		// "type": "message", "stop_reason", "model", and "usage" — matching what
		// the probe confirmed works. User turns only need role + content.
		var messageObj map[string]any
		if claudeRole == "assistant" {
			messageObj = map[string]any{
				"id":          entryUUID, // reuse entry uuid as a stable msg id
				"type":        "message",
				"role":        "assistant",
				"content":     content,
				"stop_reason": "tool_use",
				"model":       model,
				"usage":       map[string]any{"input_tokens": 0, "output_tokens": 0},
			}
		} else {
			messageObj = map[string]any{
				"role":    claudeRole,
				"content": content,
			}
		}

		// Top-level entry matches the TranscriptMessage shape Claude CLI expects.
		// "type" must be present ("user" / "assistant") and "version" must be "1.0.0".
		entry := map[string]any{
			"uuid":        entryUUID,
			"parentUuid":  parentUUID,
			"type":        claudeRole, // ← required: "user" or "assistant"
			"sessionId":   syntheticSessionUUID,
			"timestamp":   ts,
			"cwd":         cwd,
			"userType":    "external",
			"version":     "1.0.0", // ← must be "1.0.0", not "1"
			"isSidechain": false,
			"message":     messageObj,
		}
		if encErr := enc.Encode(entry); encErr != nil {
			os.Remove(f.Name())
			err = fmt.Errorf("write session entry: %w", encErr)
			return
		}
		prevUUID = entryUUID
	}

	path = f.Name()
	return
}

// buildToolResultContent converts a protocol tool message into Anthropic API
// content blocks for a tool_result entry in the session JSONL.
// Text and image parts are preserved; images are represented as native blocks.
func buildToolResultContent(msg protocol.ChatMessage) []map[string]any {
	var blocks []map[string]any
	if t := msg.ContentString(); t != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": t})
	}
	blocks = append(blocks, extractImageBlocks(msg.Content)...)
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	return blocks
}

// extractImageBlocks converts image_url content parts from a message's
// RawMessage content into Anthropic-native image content blocks for Claude CLI
// stream-json stdin. data: URIs are decoded; https:// URLs use the "url" source type.
//
// Handles two wire formats:
//   - Internal flat:  {"type":"image_url","image_url":"data:image/jpeg;base64,..."}
//   - OpenAI nested:  {"type":"image_url","image_url":{"url":"data:image/jpeg;base64,..."}}
func extractImageBlocks(content json.RawMessage) []map[string]any {
	if len(content) == 0 || content[0] != '[' {
		return nil
	}
	// Use json.RawMessage for image_url so we can decode it as either a
	// flat string or a nested {"url":"..."} object.
	var parts []struct {
		Type     string          `json:"type"`
		ImageURL json.RawMessage `json:"image_url,omitempty"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil
	}
	var blocks []map[string]any
	for _, p := range parts {
		if p.Type != "image_url" || len(p.ImageURL) == 0 {
			continue
		}
		// Try nested object: {"url": "..."}
		var nested struct {
			URL string `json:"url"`
		}
		var url string
		if err := json.Unmarshal(p.ImageURL, &nested); err == nil && nested.URL != "" {
			url = nested.URL
		} else if err := json.Unmarshal(p.ImageURL, &url); err != nil || url == "" {
			continue
		}
		if strings.HasPrefix(url, "data:") {
			// Parse data:<mediaType>;base64,<data>
			rest := strings.TrimPrefix(url, "data:")
			semi := strings.IndexByte(rest, ';')
			comma := strings.IndexByte(rest, ',')
			if semi < 0 || comma < 0 || comma <= semi {
				continue
			}
			mediaType := rest[:semi]
			encoding := rest[semi+1 : comma]
			if encoding != "base64" {
				continue
			}
			b64data := rest[comma+1:]
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": mediaType,
					"data":       b64data,
				},
			})
		} else {
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type": "url",
					"url":  url,
				},
			})
		}
	}
	return blocks
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
//
//	"mcp__auxot__auxot"              → "auxot"
//	"my_tool"                        → "my_tool" (unchanged)
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


// ── Claude NDJSON types ────────────────────────────────────────────────────────

type claudeEvent struct {
	Type    string         `json:"type"`
	Message *claudeMessage `json:"message,omitempty"`
	Usage   *claudeUsage   `json:"usage,omitempty"`
	// control_request fields (permission-prompt-tool stdio protocol)
	RequestID string                `json:"request_id,omitempty"`
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
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	// tool_use fields (assistant → MCP or builtin)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result fields (user → result of builtin or MCP tool execution).
	// Builtin tools: `content` is a plain JSON string.
	// MCP tools:    `content` is a JSON array of {type, text} objects.
	// Use ResultContent() to get a normalised string in both cases.
	ToolUseID         string          `json:"tool_use_id,omitempty"`
	ResultContentRaw  json.RawMessage `json:"content,omitempty"`
}

// ResultContent returns the tool result as a plain string regardless of
// whether it was encoded as a plain JSON string (builtin tools) or as an
// array of content objects (MCP tools).
func (b *claudeBlock) ResultContent() string {
	if len(b.ResultContentRaw) == 0 {
		return ""
	}
	// Plain string (builtin tools: bash, read, etc.)
	var s string
	if json.Unmarshal(b.ResultContentRaw, &s) == nil {
		return s
	}
	// Array of {type, text} objects (MCP tools).
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(b.ResultContentRaw, &blocks) == nil {
		parts := make([]string, 0, len(blocks))
		for _, blk := range blocks {
			if blk.Type == "text" && blk.Text != "" {
				parts = append(parts, blk.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	// Fallback: raw JSON.
	return string(b.ResultContentRaw)
}

type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// totalInputTokens returns the normalized input token count: uncached tokens
// plus cache-creation and cache-read tokens, matching how other providers report usage.
func (u *claudeUsage) totalInputTokens() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}
