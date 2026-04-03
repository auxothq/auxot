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
	// away from the developer's personal ~/.claude directory and makes
	// the session file path fully deterministic:
	//   <DefaultConfigDir>/projects/-tmp-auxot-worker/<session-id>.jsonl
	DefaultConfigDir = "/tmp/auxot-worker/.claude"
)

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
}

// sessionFilePath returns the path where the Claude CLI persists conversation
// history for a given session ID, given the config directory and working
// directory that the claude subprocess was started with.
//
// Path layout:  <configDir>/projects/<escaped-cwd>/<session-id>.jsonl
// CWD escaping: every "/" is replaced with "-" (including the leading slash).
//
// Symlinks in workDir are resolved before escaping — the CLI uses the real
// path (as seen by the OS) for the project directory. On macOS /tmp resolves
// to /private/tmp, changing the escaped project key.
func sessionFilePath(configDir, workDir, sessionID string) string {
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	escapedCWD := strings.ReplaceAll(workDir, "/", "-")
	return filepath.Join(configDir, "projects", escapedCWD, sessionID+".jsonl")
}

// claudeSessionIDForCompactionKey returns an ID suitable for Claude Code's
// --session-id / --resume flags, which require a valid RFC-4122 UUID.
//
// The server may send CompactionSessionID as the string form of a DB message
// row id (e.g. "10218"); passing that through makes the CLI exit immediately
// with status 1. Non-UUID keys are mapped to a stable UUIDv5-style SHA-1 name.
func claudeSessionIDForCompactionKey(raw string) string {
	if raw == "" {
		return ""
	}
	if _, err := uuid.Parse(raw); err == nil {
		return raw
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("auxot.compaction:"+raw)).String()
}

// workerEnv builds the subprocess environment for the claude CLI.
// It inherits the current process environment and overlays the variables
// that control claude's runtime behaviour in a server context.
func workerEnv(configDir, proxyAddr string) []string {
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
	}
	if proxyAddr != "" {
		overlay = append(overlay, "ANTHROPIC_BASE_URL="+proxyAddr)
	}
	return append(env, overlay...)
}

// cleanupOldSessionFiles removes any .jsonl session files in the project
// directory for workDir that do NOT match currentSessionID. This keeps the
// project directory tidy when the CompactionSessionID rotates (new chunk boundary).
func cleanupOldSessionFiles(configDir, workDir, currentSessionID string, log *slog.Logger) {
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	escapedCWD := strings.ReplaceAll(workDir, "/", "-")
	projectDir := filepath.Join(configDir, "projects", escapedCWD)
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return // directory may not exist yet
	}
	currentFile := currentSessionID + ".jsonl"
	for _, e := range entries {
		if e.IsDir() || e.Name() == currentFile {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		target := filepath.Join(projectDir, e.Name())
		if rmErr := os.Remove(target); rmErr == nil {
			log.Info("cliworker: removed stale session file", "path", target)
		}
	}
}

// CleanupStaleSessions removes session files older than maxAge from the CLI's
// project directory for the given workDir. Intended to be called periodically
// by a background goroutine to prevent unbounded growth of session files on
// long-lived worker instances.
func CleanupStaleSessions(configDir, workDir string, maxAge time.Duration) {
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	escapedCWD := strings.ReplaceAll(workDir, "/", "-")
	projectDir := filepath.Join(configDir, "projects", escapedCWD)
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(projectDir, e.Name()))
		}
	}
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
// Read-repair: if Claude returns a 400 "maximum of 4 blocks with cache_control"
// error and a session file exists, we delete the poisoned session .jsonl and
// retry once with --session-id (full reseed) to recover from legacy bugs.
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
	err := runJobOnce(ctx, job, cfg, onToken, onReasoningToken, onBuiltinTool, onComplete, onError, false)
	if err == errCacheControlRetry {
		slog.Default().Warn("cliworker: cache_control 400 detected, deleting session and retrying")
		_ = runJobOnce(ctx, job, cfg, onToken, onReasoningToken, onBuiltinTool, onComplete, onError, true)
	}
}

var errCacheControlRetry = fmt.Errorf("cache_control_400_retry")

func runJobOnce(
	ctx context.Context,
	job protocol.JobMessage,
	cfg JobConfig,
	onToken func(string) error,
	onReasoningToken func(string) error,
	onBuiltinTool func(id, name, args, result string) error,
	onComplete func(preToolContent, postToolContent, reasoningContent string, cacheTokens, inputTokens, outputTokens, reasoningTokens int, durationMS int64, toolCalls []protocol.ToolCall, builtinToolUses []protocol.BuiltinToolUse) error,
	onError func(errMsg, details string) error,
	forceReseed bool,
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

	// Build a set of MCP tool names so buildPrompt can re-prefix bare names
	// in conversation history to match what Claude CLI sees in its tool list.
	mcpToolNames := make(map[string]string, len(job.Tools))
	for _, t := range job.Tools {
		mcpToolNames[t.Function.Name] = "mcp__auxot__" + t.Function.Name
	}
	systemPrompt, historyBlob, currentPrompt, currentImageBlocks := buildPrompt(job.Messages, mcpToolNames)

	effectiveBuiltins := filterShadowedBuiltins(cfg.BuiltinTools, job.Tools)
	toolsFlag := buildToolsFlag(effectiveBuiltins)
	hasTools := len(job.Tools) > 0

	// ── Session management ─────────────────────────────────────────────────────
	// When the server supplies a CompactionSessionID we can reuse the CLI's
	// native session file between turns, giving it proper conversation context
	// (with cache-warm history) instead of a flat text blob every time.
	//
	//  Session file exists  →  --resume <id>    (CLI loads history, we send delta)
	//  Session file missing →  --session-id <id> (CLI seeds new file, we send full blob)
	//  No session ID given  →  --no-session-persistence (stateless, current default)
	sessionID := claudeSessionIDForCompactionKey(job.CompactionSessionID)
	sessionFileExists := false
	var sfPath string
	if sessionID != "" {
		sfPath = sessionFilePath(configDir, workDir, sessionID)
		if _, statErr := os.Stat(sfPath); statErr == nil {
			sessionFileExists = true
		}
		// forceReseed: delete the session file so we use --session-id (full seed)
		// instead of --resume. Used on retry after a cache_control 400.
		if forceReseed && sessionFileExists {
			if rmErr := os.Remove(sfPath); rmErr == nil {
				log.Info("cliworker: removed session file for reseed (cache_control read-repair)", "path", sfPath)
				sessionFileExists = false
			} else {
				log.Warn("cliworker: could not remove session file", "path", sfPath, "error", rmErr)
			}
		}
		log.Info("cliworker: session check",
			"compaction_session_id", job.CompactionSessionID,
			"claude_session_id", sessionID,
			"path", sfPath,
			"exists", sessionFileExists,
			"force_reseed", forceReseed,
		)
		// Remove any stale session files in the same project directory that
		// don't match the current session ID. This happens when a new chunk
		// compaction boundary is created and the CompactionSessionID rotates.
		cleanupOldSessionFiles(configDir, workDir, sessionID, log)
	}

	args := []string{
		"--output-format", "stream-json",
		"--verbose",
		"--tools", toolsFlag,
	}

	// Session flags — mutually exclusive with --no-session-persistence.
	switch {
	case sessionID != "" && sessionFileExists:
		args = append(args, "--resume", sessionID)
	case sessionID != "":
		args = append(args, "--session-id", sessionID)
	default:
		// No compaction session ID from server: run stateless.
		// Sessions are written to CLAUDE_CONFIG_DIR but never resumed.
		args = append(args, "--no-session-persistence")
	}

	// Always use stream-json stdin so the conversation never touches the argv.
	// On the tools path, --permission-prompt-tool stdio lets us intercept MCP
	// permission requests; on the no-tools path, --dangerously-skip-permissions
	// is equivalent (no permission events will be emitted anyway).
	args = append(args,
		"--print",
		"--input-format", "stream-json",
	)
	if hasTools {
		args = append(args, "--permission-prompt-tool", "stdio")
	} else {
		args = append(args, "--dangerously-skip-permissions")
	}

	// Always pass --model explicitly so the claude CLI never silently falls back
	// to the account's configured default (often claude-opus-4-6[1m] — the
	// 1-million-token context variant billed at premium rates).
	effectiveModel := cfg.Model
	if effectiveModel == "" {
		effectiveModel = "claude-sonnet-4-6"
	}
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
	// Fixed working directory — prevents CLAUDE.md discovery from the server
	// process's directory ancestry. System prompt is provided explicitly via
	// --system-prompt; project context files must not override it.
	cmd.Dir = workDir
	// Isolated environment: CLAUDE_CONFIG_DIR pins sessions/memory/caches to
	// a known path; the DISABLE_* vars strip all server-irrelevant behaviours.
	cmd.Env = workerEnv(configDir, "")
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

	if startErr := cmd.Start(); startErr != nil {
		log.Error("claude_start_failed", "error", startErr)
		_ = onError("failed to start claude", startErr.Error())
		return nil
	}

	{
		// Build content blocks for stream-json stdin — always, regardless of
		// whether tools are present. This keeps the conversation off the argv.
		//
		// Do not set cache_control here — Anthropic allows at most 4 breakpoints
		// for the whole request (tools + system + messages), and Claude Code uses
		// most of that budget internally.
		//
		// When --resume is used, the session .jsonl already holds prior turns.
		// Sending historyBlob again duplicates the transcript and merges with
		// session state in a way that can exceed the cache_control limit (400).
		// On resume, send only the latest turn (true delta).
		var contentBlocks []map[string]any
		if sessionFileExists {
			log.Info("cliworker: stdin resume delta only", "blocks", 1)
			contentBlocks = append(contentBlocks, map[string]any{
				"type": "text",
				"text": currentPrompt,
			})
		} else {
			if historyBlob != "" {
				contentBlocks = append(contentBlocks, map[string]any{
					"type": "text",
					"text": historyBlob,
				})
			}
			contentBlocks = append(contentBlocks, map[string]any{
				"type": "text",
				"text": currentPrompt,
			})
		}
		// Append native image blocks for the current turn.
		// Claude Code's stream-json stdin accepts Anthropic image content blocks
		// ({ "type": "image", "source": { "type": "base64", ... } }), so we can
		// pass images directly without the data: URI wrapper.
		contentBlocks = append(contentBlocks, currentImageBlocks...)

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
		seenBuiltinResult bool // true after first builtin tool_result received
		batchComplete     bool // set true on rate_limit_event
		pendingBuiltin    = map[string]*protocol.BuiltinToolUse{}
		completedBuiltins []protocol.BuiltinToolUse
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
						case seenMCPTool:
							// MCP turn: we killed Claude after collecting tool calls,
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
				}
			}

		case "rate_limit_event":
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
			return nil
		}
		if fullText.Len() == 0 && len(toolCalls) == 0 {
			stderr := strings.TrimSpace(stderrBuf.String())
			if len(stderr) > 4096 {
				stderr = stderr[:4096] + "...(truncated)"
			}
			// Read-repair: if we see cache_control 400 and we used --resume,
			// signal the caller to delete the session file and retry once.
			if sessionFileExists && strings.Contains(stderr, "cache_control") && strings.Contains(stderr, "maximum of 4") {
				log.Warn("cliworker: cache_control limit error detected with session file", "path", sfPath)
				return errCacheControlRetry
			}
			log.Error("claude_exited_with_error", "error", err, "stderr", stderr)
			_ = onError(fmt.Sprintf("claude exited: %v", err), stderr)
			return nil
		}
	}

complete:
	var preToolContent, postToolContent string
	switch {
	case len(toolCalls) > 0:
		// MCP turn: response is whatever the agent said before calling MCP tools.
		// Claude is killed after we collect the tool calls, so there is no post-tool text.
		preToolContent = preToolText.String()
	case len(completedBuiltins) > 0:
		// CLI-native builtin turn: separate the pre-tool preamble from the
		// post-tool response so the server can insert them as two distinct
		// assistant rows with the tool_call rows in between.
		preToolContent = preToolText.String()
		postToolContent = postToolText.String()
	default:
		// Plain text turn: no tools at all.
		preToolContent = fullText.String()
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

// buildPrompt extracts the system message and formats the conversation history
// into a single prompt string for claude's --print mode.
//
// For tool result turns (role "tool"), the content is formatted so claude understands
// it received the result of a tool call it previously requested.
// buildPrompt parses the stored message history into:
//   - systemPrompt: the system message content (with parallel-tool instruction appended)
//   - historyBlob: all prior turns formatted as a conversation transcript; empty
//     when there is no prior history. Sent as plain text (no cache_control) so we
//     stay under Anthropic's 4-breakpoint limit once CLI system/tools are counted.
//   - currentPrompt: the text of the final user message.
func buildPrompt(messages []protocol.ChatMessage, mcpToolNames map[string]string) (systemPrompt, historyBlob, currentPrompt string, imageBlocks []map[string]any) {
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
		// Hard-cap the history blob to avoid "Prompt is too long" from Claude.
		// Chunk summaries are already folded into the system prompt by ComputeWindow,
		// so dropping the oldest raw turns here is safe — they are covered by that
		// summary. We keep dropping from the front until we're under the limit.
		const maxHistoryBytes = 80_000
		for len(history) > 1 {
			candidate := "<conversation_history>\n" +
				strings.Join(history, "\n\n") +
				"\n</conversation_history>"
			if len(candidate) <= maxHistoryBytes {
				break
			}
			history = history[1:] // drop oldest turn
		}
		dropped := len(turns) - 1 - len(history) // turns-1 == original history len
		if dropped > 0 {
			slog.Default().Warn("cliworker: historyBlob too large, dropped oldest turns",
				"dropped", dropped, "remaining", len(history))
		}
		historyBlob = "<conversation_history>\n" +
			strings.Join(history, "\n\n") +
			"\n</conversation_history>"
	}
	currentPrompt = stripRolePrefix(last)

	// Extract image blocks from the last user message for native rendering.
	// Claude Code's stream-json stdin accepts Anthropic image content blocks;
	// we pass images here rather than embedding data: URIs in the text prompt.
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			imageBlocks = extractImageBlocks(messages[i].Content)
			break
		}
	}
	return
}

// extractImageBlocks converts OpenAI-style image_url parts from a message's
// RawMessage content into Anthropic-native image content blocks for Claude CLI
// stream-json stdin. data: URIs are decoded; https:// URLs use the "url" source type.
func extractImageBlocks(content json.RawMessage) []map[string]any {
	if len(content) == 0 || content[0] != '[' {
		return nil
	}
	var parts []struct {
		Type     string `json:"type"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url,omitempty"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil
	}
	var blocks []map[string]any
	for _, p := range parts {
		if p.Type != "image_url" || p.ImageURL == nil || p.ImageURL.URL == "" {
			continue
		}
		url := p.ImageURL.URL
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
	// tool_result fields (user → result of builtin tool execution)
	ToolUseID     string `json:"tool_use_id,omitempty"`
	ResultContent string `json:"content,omitempty"` // plain text or JSON result
}

type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
