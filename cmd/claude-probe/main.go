// claude-probe is a diagnostic tool that spawns the Claude CLI and logs every
// NDJSON event with precise timestamps to understand the protocol ordering.
//
// It is self-contained:
//   - Embeds a minimal MCP stdio server (--mcp-server flag) so MCP scenarios
//     work without needing auxot-worker.
//   - Embeds an intercepting HTTP proxy (--proxy flag) that forwards requests
//     to api.anthropic.com and dumps every request/response body so we can
//     inspect the exact message structure and cache_control headers on the wire.
//     Run with --intercept to start the proxy and point ANTHROPIC_BASE_URL at it.
//
// Single-turn test scenarios:
//   1. builtin              – CLI-native tools only (Bash)
//   2. mcp                  – MCP tools only (get_status, get_version) — we deny all
//   3. mixed                – Bash builtin + MCP tools in the same turn
//   4. history              – Multi-turn blob injected via stdin (no --resume)
//   5. parallel-mcp-kill    – Two parallel MCP tool_use blocks; close stdin at first
//                             control_request WITHOUT sending deny. Proves all tool_use
//                             blocks arrive in the assistant event before any control_request.
//   6. mcp-live             – Full MCP round-trip: allow all tool calls, MCP server
//                             returns real results, Claude runs to natural completion.
//                             Tests the "live continuation" architecture.
//   7. mcp-parallel-live    – Two parallel MCP tools (tool1, tool2) both executed live.
//                             Dumps FULL raw JSON for every event so we can see exactly
//                             what stream-json Claude CLI emits: how many assistant events,
//                             when tool_use blocks appear, what user events carry tool
//                             results, and when rate_limit_event fires relative to all of
//                             the above. This is the ground-truth recon run.
//   8. synthetic-session-resume – Write a synthetic JSONL session file containing a full
//                             conversation (user ask → assistant tool_use → user tool_result
//                             with image), then pass it to Claude CLI via --resume /path.jsonl.
//                             The follow-up prompt is sent via stdin. If Claude synthesises
//                             a response without re-executing the tool, the session file
//                             approach is valid for history injection with tool results.
//
// Multi-turn session scenarios (verify --session-id / --resume behaviour):
//   7. session-simple   – Turn 1: plain answer. Turn 2: follow-up referencing turn 1.
//   8. session-builtin  – Turn 1: Bash tool. Turn 2: follow-up asking for its output.
//   9. session-mcp      – Turn 1: MCP tool (denied). Turn 2: follow-up.
//  10. session-missing  – Like session-simple but session file deleted between turns
//                         to verify the full-seed fallback path.
//
// Usage:
//
//	go run ./cmd/claude-probe [--scenario builtin|mcp|mixed|history] [--prompt "..."]
//	go run ./cmd/claude-probe --scenario parallel-mcp-kill  # key regression test
//	go run ./cmd/claude-probe --scenario mcp-live           # live continuation test
//	go run ./cmd/claude-probe --scenario session-simple    # multi-turn session test
//	go run ./cmd/claude-probe --scenario synthetic-session-resume  # key new test
//	go run ./cmd/claude-probe --mcp-server                 # internal: MCP stdio mode
//	go run ./cmd/claude-probe --proxy                      # internal: HTTP proxy mode
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ── MCP server mode ──────────────────────────────────────────────────────────

// runMCPServer runs a minimal MCP stdio server.
// When live=true the server actually executes tools/call requests:
//   - bash: runs the command in a shell, returns stdout+stderr
//   - get_status / get_version: return canned responses
//   - tool1 / tool2: return "tool1 fired!" / "tool2 fired!" (for parallel-live recon)
//
// When live=false (default stub mode) all tools/call requests return "stub".
func runMCPServer(live bool) {
	tools := []map[string]any{
		{
			"name":        "get_status",
			"description": "Get the current system status.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "get_version",
			"description": "Get the system version string.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "bash",
			"description": "Run a shell command and return its output.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string", "description": "Shell command to execute"},
				},
				"required": []string{"command"},
			},
		},
		{
			"name":        "tool1",
			"description": "First probe tool. Call this to confirm tool1 executes.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "tool2",
			"description": "Second probe tool. Call this to confirm tool2 executes.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	enc := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		line := scanner.Text()
		// Log every MCP message to stderr for recon visibility.
		fmt.Fprintf(os.Stderr, "[mcp-server ←] %s\n", line)
		var req map[string]any
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		method, _ := req["method"].(string)
		id := req["id"]
		switch method {
		case "initialize":
			resp := map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "probe-mcp", "version": "1.0"},
				},
			}
			respBytes, _ := json.Marshal(resp)
			fmt.Fprintf(os.Stderr, "[mcp-server →] %s\n", respBytes)
			_ = enc.Encode(resp)
		case "tools/list":
			resp := map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"tools": tools}}
			respBytes, _ := json.Marshal(resp)
			fmt.Fprintf(os.Stderr, "[mcp-server →] %s\n", respBytes)
			_ = enc.Encode(resp)
		case "tools/call":
			var result string
			if live {
				params, _ := req["params"].(map[string]any)
				toolName, _ := params["name"].(string)
				arguments, _ := params["arguments"].(map[string]any)
				switch toolName {
				case "bash":
					command, _ := arguments["command"].(string)
					out, err := exec.Command("sh", "-c", command).CombinedOutput()
					if err != nil {
						result = fmt.Sprintf("exit error: %v\n%s", err, out)
					} else {
						result = string(out)
					}
				case "get_status":
					result = "status: ok"
				case "get_version":
					result = "version: probe-1.0"
				case "tool1":
					result = "tool1 fired!"
				case "tool2":
					result = "tool2 fired!"
				default:
					result = fmt.Sprintf("unknown tool: %s", toolName)
				}
			} else {
				result = "stub"
			}
			resp := map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"content": []map[string]any{{"type": "text", "text": result}}},
			}
			respBytes, _ := json.Marshal(resp)
			fmt.Fprintf(os.Stderr, "[mcp-server →] %s\n", respBytes)
			_ = enc.Encode(resp)
		default:
			if id != nil {
				resp := map[string]any{"jsonrpc": "2.0", "id": id, "result": nil}
				respBytes, _ := json.Marshal(resp)
				fmt.Fprintf(os.Stderr, "[mcp-server →] %s\n", respBytes)
				_ = enc.Encode(resp)
			}
		}
	}
}

// ── Intercepting proxy mode ───────────────────────────────────────────────────
//
// Starts a plain-HTTP server on a free port, forwards every request to
// https://api.anthropic.com, and dumps request + response JSON bodies.
// The caller sets ANTHROPIC_BASE_URL=http://127.0.0.1:<port> on the claude
// subprocess so all API traffic passes through here unencrypted.

func runProxy(addrCh chan<- string) {
	upstream, _ := url.Parse("https://api.anthropic.com")
	rp := httputil.NewSingleHostReverseProxy(upstream)
	rp.Director = func(req *http.Request) {
		req.URL.Scheme = upstream.Scheme
		req.URL.Host = upstream.Host
		req.Host = upstream.Host
	}

	seq := 0
	var mu sync.Mutex

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seq++
		n := seq
		mu.Unlock()

		// Capture and log the request body.
		var reqBody []byte
		if r.Body != nil {
			reqBody, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(reqBody))
			r.ContentLength = int64(len(reqBody))
		}

		fmt.Fprintf(os.Stderr, "\n━━━ REQUEST #%d %s %s ━━━\n", n, r.Method, r.URL.Path)
		if len(reqBody) > 0 {
			printJSON(os.Stderr, reqBody, "  ")
		}

		// Intercept the response.
		rec := &responseRecorder{ResponseWriter: w, body: &bytes.Buffer{}}
		rp.ServeHTTP(rec, r)

		respBody := rec.body.Bytes()
		// Decompress if gzip (Anthropic compresses responses).
		if strings.Contains(rec.Header().Get("Content-Encoding"), "gzip") {
			if gr, err := gzip.NewReader(bytes.NewReader(respBody)); err == nil {
				if plain, err := io.ReadAll(gr); err == nil {
					respBody = plain
				}
			}
		}

		fmt.Fprintf(os.Stderr, "\n━━━ RESPONSE #%d status=%d ━━━\n", n, rec.status)
		if len(respBody) > 0 {
			// For streaming responses, log up to the first 4 KB for cache_control inspection.
			preview := respBody
			truncated := false
			if len(preview) > 4096 {
				preview = preview[:4096]
				truncated = true
			}
			printJSON(os.Stderr, preview, "  ")
			if truncated {
				fmt.Fprintf(os.Stderr, "  ... (truncated, full length=%d bytes)\n", len(respBody))
			}
		}
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy: listen failed: %v\n", err)
		addrCh <- ""
		return
	}
	addrCh <- "http://" + ln.Addr().String()
	_ = http.Serve(ln, handler)
}

type responseRecorder struct {
	http.ResponseWriter
	body   *bytes.Buffer
	status int
}

func (r *responseRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

func printJSON(w io.Writer, data []byte, indent string) {
	// Try pretty-print; fall back to raw.
	var v any
	if err := json.Unmarshal(data, &v); err == nil {
		enc := json.NewEncoder(w)
		enc.SetIndent(indent, "  ")
		_ = enc.Encode(v)
	} else {
		// NDJSON (streaming) — print first line fully, summarise rest.
		lines := bytes.Split(data, []byte("\n"))
		for i, line := range lines {
			if len(line) == 0 {
				continue
			}
			if i == 0 {
				var v2 any
				if err2 := json.Unmarshal(line, &v2); err2 == nil {
					enc := json.NewEncoder(w)
					enc.SetIndent(indent, "  ")
					_ = enc.Encode(v2)
				} else {
					fmt.Fprintf(w, "%s%s\n", indent, line)
				}
			} else {
				fmt.Fprintf(w, "%s[+%d more lines]\n", indent, len(lines)-1)
				break
			}
		}
	}
}

// ── Probe event type ──────────────────────────────────────────────────────────

type event struct {
	Seq       int    `json:"seq"`
	Direction string `json:"dir"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Raw       string `json:"raw"`
	Type      string `json:"type"`
	Summary   string `json:"summary,omitempty"`
}

// ── Probe directories ──────────────────────────────────────────────────────────
//
// The probe uses isolated directories, separate from the production worker
// defaults, so probe runs never contaminate real worker sessions (and vice versa).
const (
	probeWorkDir   = "/tmp/auxot-probe"
	probeConfigDir = "/tmp/auxot-probe/.claude"
)

// probeEnv returns the subprocess environment for claude CLI probe runs.
// Mirrors workerEnv in cliworker/claude.go but uses probe-specific paths.
func probeEnv(proxyAddr string) []string {
	env := os.Environ()
	overlay := []string{
		"CLAUDE_CONFIG_DIR=" + probeConfigDir,
		// Match cliworker.workerEnv: avoid 5m top-level vs 1h block cache_control 400s.
		"DISABLE_PROMPT_CACHING=1",
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
		"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1",
		"CLAUDE_CODE_DISABLE_CRON=1",
		"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_TELEMETRY=1",
	}
	if proxyAddr != "" {
		overlay = append(overlay, "ANTHROPIC_BASE_URL="+proxyAddr)
	}
	return append(env, overlay...)
}

// ── Multi-turn session scenarios ──────────────────────────────────────────────
//
// These scenarios run TWO consecutive CLI invocations:
//   Turn 1 — seeds a session (captures cliSessionId from system/init)
//   Turn 2 — resumes that session (or falls back to full seed if file missing)
//
// Available scenarios:
//   session-simple   — Turn 1: plain answer. Turn 2: follow-up. No tools.
//   session-builtin  — Turn 1: Bash tool use. Turn 2: follow-up referencing result.
//   session-mcp      — Turn 1: MCP tools (denied). Turn 2: follow-up.
//   session-missing  — Same as session-simple but session file is deleted between turns
//                      to test the full-seed fallback path.

// sessionFilePath returns the expected session JSONL path given the config
// directory and working directory that the claude subprocess used.
// Path: <configDir>/projects/<escaped-cwd>/<session-id>.jsonl
// CWD escaping: every "/" is replaced with "-".
//
// Symlinks in workDir are resolved before escaping because the OS-level CWD
// reported inside the subprocess (and used by the CLI for the project path) is
// the real path, not the symlink. On macOS /tmp → /private/tmp, so a workDir
// of "/tmp/auxot-probe" maps to "-private-tmp-auxot-probe", not "-tmp-auxot-probe".
func sessionFilePath(configDir, workDir, cliSessionID string) string {
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	escapedCWD := strings.ReplaceAll(workDir, "/", "-")
	return filepath.Join(configDir, "projects", escapedCWD, cliSessionID+".jsonl")
}

func runTurn(label, claudePath, model, systemPrompt, sessionID, resumeID, prompt string, tools []string, mcpConfig string, proxyAddr string, denyAll bool) (cliSessionID string, ok bool) {
	fmt.Fprintf(os.Stderr, "\n╔══ %s ══╗\n", label)

	var args []string
	// Note: --no-session-persistence is intentionally omitted here.
	// These multi-turn tests depend on the session file being written to disk
	// between turns. It is only appropriate on stateless production workers.
	args = append(args, "--output-format", "stream-json", "--verbose")

	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	} else if sessionID != "" {
		args = append(args, "--session-id", sessionID)
	}

	if systemPrompt != "" {
		args = append(args, "--system-prompt", systemPrompt)
	}

	// Tools flag — always pass it, even as empty string (matches production behaviour).
	toolsFlag := strings.Join(tools, ",")
	args = append(args,
		"--tools", toolsFlag,
		"--print",
		"--input-format", "stream-json",
		"--permission-prompt-tool", "stdio",
	)

	if mcpConfig != "" {
		args = append(args, "--strict-mcp-config", "--mcp-config", mcpConfig)
	}

	if model != "" {
		args = append(args, "--model", model)
	}

	fmt.Fprintf(os.Stderr, "cmd: %s %s\n", claudePath, strings.Join(args, " "))

	_ = os.MkdirAll(probeWorkDir, 0o755)
	_ = os.MkdirAll(probeConfigDir, 0o755)

	cmd := exec.Command(claudePath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = probeWorkDir
	cmd.Env = probeEnv(proxyAddr)

	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		<-ch
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		os.Exit(1)
	}()

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	stdinPipe, _ := cmd.StdinPipe()

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start claude: %v\n", err)
		return "", false
	}

	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if line := sc.Text(); line != "" {
				fmt.Fprintf(os.Stderr, "  [stderr] %s\n", line)
			}
		}
	}()

	// Send user message.
	userMsg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": prompt}},
		},
	}
	data, _ := json.Marshal(userMsg)
	fmt.Fprintln(stdinPipe, string(data))

	seq := 0
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		seq++
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			fmt.Fprintf(os.Stderr, "  [%03d] PARSE_ERROR: %v\n", seq, err)
			continue
		}
		typ, _ := parsed["type"].(string)
		summary := summarizeEvent(typ, parsed)
		fmt.Fprintf(os.Stderr, "  [%03d] %-22s %s\n", seq, typ, summary)

		// Capture cliSessionId from system/init.
		if typ == "system" {
			if sid, ok2 := parsed["session_id"].(string); ok2 && sid != "" {
				cliSessionID = sid
			}
		}

		// Handle control_request (deny all MCP tool calls).
		if typ == "control_request" && denyAll {
			reqID, _ := parsed["request_id"].(string)
			resp := map[string]any{
				"type":       "control_response",
				"request_id": reqID,
				"decision":   "deny",
				"reason":     "probe: deny all",
			}
			d, _ := json.Marshal(resp)
			fmt.Fprintln(stdinPipe, string(d))
		}

		if typ == "result" {
			stdinPipe.Close()
			break
		}
	}

	_ = cmd.Wait()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	fmt.Fprintf(os.Stderr, "╚══ %s complete | cliSessionID=%s ══╝\n", label, cliSessionID)
	return cliSessionID, true
}

// runSessionFileReplay is a two-phase test:
//
//  Phase 1 — run a live Read session WITH session persistence so Claude CLI
//             writes a session JSONL file to disk. Capture the session ID.
//
//  Phase 2 — read the session file Claude wrote, then feed those exact NDJSON
//             lines as stream-json stdin to a BRAND NEW subprocess (no --resume,
//             no --tools Read). Ask the follow-up question. If the model knows
//             the secret code without re-executing Read, the session file format
//             is the correct history injection format — the one that bypasses
//             Claude CLI's tool interception.
func runSessionFileReplay(model, claudePath string) {
	_ = os.MkdirAll(probeWorkDir, 0o755)
	_ = os.MkdirAll(probeConfigDir, 0o755)

	cliPath := claudePath
	if cliPath == "" {
		var err error
		cliPath, err = exec.LookPath("claude")
		if err != nil {
			fmt.Fprintln(os.Stderr, "claude not found in PATH")
			os.Exit(1)
		}
	}

	// ── Phase 1: live Read session, session persistence ON ──────────────────
	fmt.Fprintln(os.Stderr, "\n=== session-file-replay PHASE 1: live Read (session persisted) ===")

	phase1Args := []string{
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--print",
		"--tools", "Read",
		"--dangerously-skip-permissions",
		// NO --no-session-persistence — we want the file written.
	}
	if model != "" {
		phase1Args = append(phase1Args, "--model", model)
	}
	fmt.Fprintf(os.Stderr, "cmd: %s %s\n\n", cliPath, strings.Join(phase1Args, " "))

	phase1 := exec.Command(cliPath, phase1Args...)
	phase1.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	phase1.Dir = probeWorkDir
	phase1.Env = probeEnv("")

	p1stdin, _ := phase1.StdinPipe()
	p1stdout, _ := phase1.StdoutPipe()
	p1stderr, _ := phase1.StderrPipe()

	if err := phase1.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "phase1 start: %v\n", err)
		os.Exit(1)
	}
	go func() {
		s := bufio.NewScanner(p1stderr)
		for s.Scan() {
		}
	}()

	// Send the prompt.
	prompt1, _ := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": "Read the file /tmp/probe-test.txt using the Read tool. Tell me the secret code it contains.",
		},
	})
	fmt.Fprintln(p1stdin, string(prompt1))

	// Collect all output lines and extract session ID.
	// Close stdin as soon as we see the "result" event — otherwise the CLI
	// waits for more input forever (stream-json mode keeps stdin open).
	var sessionID string
	var phase1Lines []string
	p1scanner := bufio.NewScanner(p1stdout)
	p1scanner.Buffer(make([]byte, 10<<20), 10<<20)
	for p1scanner.Scan() {
		line := p1scanner.Text()
		if line == "" {
			continue
		}
		phase1Lines = append(phase1Lines, line)
		// Extract session_id from any line that has it.
		var probe struct {
			Type      string  `json:"type"`
			SessionID string  `json:"session_id"`
			Result    *string `json:"result"`
		}
		if json.Unmarshal([]byte(line), &probe) == nil {
			if probe.SessionID != "" {
				sessionID = probe.SessionID
			}
			// Close stdin on result — signals CLI to exit.
			if probe.Type == "result" {
				_ = p1stdin.Close()
			}
		}
		fmt.Fprintf(os.Stderr, "[p1] %s\n", line[:min(120, len(line))])
	}
	_ = phase1.Wait()

	if sessionID == "" {
		fmt.Fprintln(os.Stderr, "\nPhase 1 failed: no session_id captured")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "\nPhase 1 complete. session_id=%s\n", sessionID)

	// ── Read the session file Claude wrote ──────────────────────────────────
	sfPath := sessionFilePath(probeConfigDir, probeWorkDir, sessionID)
	fmt.Fprintf(os.Stderr, "\n=== session-file-replay: reading session file ===\n%s\n\n", sfPath)

	sfBytes, err := os.ReadFile(sfPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read session file: %v\n", err)
		fmt.Fprintln(os.Stderr, "Phase 1 subprocess may have used --no-session-persistence or wrong config dir.")
		os.Exit(1)
	}

	sessionLines := strings.Split(strings.TrimSpace(string(sfBytes)), "\n")
	fmt.Fprintf(os.Stderr, "Session file has %d lines. First 3:\n", len(sessionLines))
	for i, l := range sessionLines {
		if i >= 3 {
			break
		}
		fmt.Fprintf(os.Stderr, "  [%d] %s\n", i, l[:min(200, len(l))])
	}
	fmt.Fprintln(os.Stderr)

	// ── Phase 2: fresh session, replay session file lines as stream-json ─────
	fmt.Fprintln(os.Stderr, "=== session-file-replay PHASE 2: fresh session, inject session file lines ===")

	phase2Args := []string{
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--print",
		"--no-session-persistence",
		// NO --tools Read — if it works without the tool registered, re-execution is impossible.
		// NO --resume — we're testing whether the raw session file format works as stdin history.
	}
	if model != "" {
		phase2Args = append(phase2Args, "--model", model)
	}
	fmt.Fprintf(os.Stderr, "cmd: %s %s\n\n", cliPath, strings.Join(phase2Args, " "))

	phase2 := exec.Command(cliPath, phase2Args...)
	phase2.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	phase2.Dir = probeWorkDir
	phase2.Env = probeEnv("")

	p2stdin, _ := phase2.StdinPipe()
	p2stdout, _ := phase2.StdoutPipe()
	p2stderr, _ := phase2.StderrPipe()

	if err := phase2.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "phase2 start: %v\n", err)
		os.Exit(1)
	}
	go func() {
		s := bufio.NewScanner(p2stderr)
		for s.Scan() {
		}
	}()

	// Send every line from the session file as-is, then the follow-up prompt.
	seq := 0
	sendLine := func(label, line string) {
		seq++
		fmt.Fprintf(os.Stderr, "[p2 send %03d] %s: %s\n", seq, label, line[:min(120, len(line))])
		fmt.Fprintln(p2stdin, line)
	}

	for i, line := range sessionLines {
		if line == "" {
			continue
		}
		sendLine(fmt.Sprintf("session line %d", i), line)
	}

	// Follow-up prompt that requires knowledge of the previous tool result.
	followUp, _ := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": "What was the secret code in the file you just read?",
		},
	})
	sendLine("follow-up prompt", string(followUp))
	// Don't close stdin yet — we close it when we see the result event.

	fmt.Fprintln(os.Stderr, "\n=== Phase 2 output ===")
	p2scanner := bufio.NewScanner(p2stdout)
	p2scanner.Buffer(make([]byte, 10<<20), 10<<20)
	var resultText string
	for p2scanner.Scan() {
		line := p2scanner.Text()
		if line == "" {
			continue
		}
		// Pretty-print for inspection.
		var pretty bytes.Buffer
		if json.Indent(&pretty, []byte(line), "  ", "  ") == nil {
			fmt.Fprintf(os.Stderr, "%s\n\n", pretty.String())
		} else {
			fmt.Fprintf(os.Stderr, "%s\n", line)
		}
		// Close stdin and capture result on the result event.
		var probe struct {
			Type   string  `json:"type"`
			Result *string `json:"result"`
		}
		if json.Unmarshal([]byte(line), &probe) == nil && probe.Type == "result" {
			if probe.Result != nil {
				resultText = *probe.Result
			}
			_ = p2stdin.Close()
		}
	}
	_ = phase2.Wait()

	fmt.Fprintln(os.Stderr, "\n=== session-file-replay VERDICT ===")
	if strings.Contains(resultText, "AUXOT-PROBE-42") {
		fmt.Fprintln(os.Stderr, "✓ PASS: model knew the secret code — session file format works as history injection!")
	} else {
		fmt.Fprintf(os.Stderr, "✗ FAIL: model did not report the secret code.\nResult: %s\n", resultText)
	}
}

// writeSyntheticSessionFile writes a minimal Claude CLI JSONL session file
// containing a canned conversation:
//
//	user  → "Read /tmp/probe-logo.png and describe what you see."
//	asst  → tool_use: read_file({path: "/tmp/probe-logo.png"})
//	user  → tool_result with the image bytes (base64 PNG)
//
// Each JSONL line is a TranscriptMessage — the format Claude CLI uses internally
// for its session files (parentUuid chain, sessionId, cwd, userType, version).
// Passing the resulting file path to --resume triggers loadMessagesFromJsonlPath
// which builds the conversation chain and returns ALL messages (including user
// tool_result) as initialMessages, bypassing the stream-json stdin routing that
// always treats user messages as new prompts.
//
// Returns the path of the written file.
func writeSyntheticSessionFile(imgPath, sessionID, workDir string) (string, error) {
	imgBytes, err := os.ReadFile(imgPath)
	if err != nil {
		return "", fmt.Errorf("reading image: %w", err)
	}
	b64img := base64.StdEncoding.EncodeToString(imgBytes)

	// Resolve symlinks — the CLI uses the real path for project dir derivation.
	cwd := workDir
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		cwd = resolved
	}

	// UUIDs for the three messages in the chain.
	uuidUser1 := "aaaaaaaa-0001-4000-a000-000000000001"
	uuidAsst1 := "aaaaaaaa-0002-4000-a000-000000000002"
	uuidUser2 := "aaaaaaaa-0003-4000-a000-000000000003"

	toolUseID := "toolu_synth_probe_0000000001"
	msgID := "msg_synth_probe_000000000001"

	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Helper to build the boilerplate session-file fields every TranscriptMessage carries.
	meta := func(uuid, parentUUID *string) map[string]any {
		m := map[string]any{
			"uuid":      *uuid,
			"sessionId": sessionID,
			"timestamp": now,
			"cwd":       cwd,
			"userType":  "external",
			"version":   "1.0.0",
			"isSidechain": false,
		}
		if parentUUID != nil {
			m["parentUuid"] = *parentUUID
		} else {
			m["parentUuid"] = nil
		}
		return m
	}

	// Message 1: user asks to read the image.
	msg1 := meta(&uuidUser1, nil)
	msg1["type"] = "user"
	msg1["message"] = map[string]any{
		"role": "user",
		"content": []map[string]any{
			{"type": "text", "text": "Read /tmp/probe-logo.png and describe what you see in detail."},
		},
	}

	// Message 2: assistant responds with a tool_use.
	msg2 := meta(&uuidAsst1, &uuidUser1)
	msg2["type"] = "assistant"
	msg2["message"] = map[string]any{
		"id":   msgID,
		"type": "message",
		"role": "assistant",
		"content": []map[string]any{
			{
				"type":  "tool_use",
				"id":    toolUseID,
				"name":  "Read",
				"input": map[string]any{"file_path": "/tmp/probe-logo.png"},
			},
		},
		"stop_reason": "tool_use",
		"model":       "claude-haiku-4-5-20251001",
		"usage": map[string]any{
			"input_tokens":  100,
			"output_tokens": 20,
		},
	}

	// Message 3: user supplies the tool_result (image bytes).
	msg3 := meta(&uuidUser2, &uuidAsst1)
	msg3["type"] = "user"
	msg3["message"] = map[string]any{
		"role": "user",
		"content": []map[string]any{
			{
				"type":        "tool_result",
				"tool_use_id": toolUseID,
				"is_error":    false,
				"content": []map[string]any{
					{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "image/png",
							"data":       b64img,
						},
					},
				},
			},
		},
	}

	// Write all three lines as NDJSON.
	f, err := os.CreateTemp("", "probe-synthetic-session-*.jsonl")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	enc := json.NewEncoder(f)
	for _, m := range []map[string]any{msg1, msg2, msg3} {
		if err := enc.Encode(m); err != nil {
			f.Close()
			os.Remove(f.Name())
			return "", fmt.Errorf("encoding session line: %w", err)
		}
	}
	f.Close()
	return f.Name(), nil
}

// syntheticRunResult captures token usage from a single synthetic-session run.
type syntheticRunResult struct {
	inputTokens       int
	outputTokens      int
	cacheCreation     int
	cacheRead         int
	resultText        string
	sawToolUse        bool
	sawToolUseNames   []string
}

// runOneSyntheticRun runs a single Claude CLI invocation with --resume /path.jsonl
// and returns token counts from the result event. proxyAddr may be empty.
// printOutput controls whether event JSON is printed to stderr.
func runOneSyntheticRun(label, claudePath, model, jsonlPath, followUpPrompt, proxyAddr string, printOutput bool) syntheticRunResult {
	fmt.Fprintf(os.Stderr, "\n── %s ──\n", label)

	args := []string{
		"--output-format", "stream-json",
		"--verbose",
		"--resume", jsonlPath,
		"--tools", "",
		"--print",
		"--input-format", "stream-json",
		"--dangerously-skip-permissions",
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	fmt.Fprintf(os.Stderr, "cmd: claude %s\n", strings.Join(args, " "))

	cmd := exec.Command(claudePath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = probeWorkDir
	cmd.Env = probeEnv(proxyAddr)

	stdout, _ := cmd.StdoutPipe()
	stdinPipe, _ := cmd.StdinPipe()
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start claude: %v\n", err)
		os.Exit(1)
	}

	msg, _ := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": followUpPrompt}},
		},
	})
	fmt.Fprintf(os.Stderr, "[probe → stdin] %s\n", string(msg))
	fmt.Fprintln(stdinPipe, string(msg))

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 10<<20), 10<<20)

	var res syntheticRunResult
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if printOutput {
			var pretty bytes.Buffer
			if json.Indent(&pretty, []byte(line), "  ", "  ") == nil {
				fmt.Fprintf(os.Stderr, "%s\n\n", pretty.String())
			} else {
				fmt.Fprintf(os.Stderr, "%s\n", line)
			}
		}

		var parsed map[string]any
		if json.Unmarshal([]byte(line), &parsed) != nil {
			continue
		}
		typ, _ := parsed["type"].(string)

		if typ == "assistant" {
			if msg, ok := parsed["message"].(map[string]any); ok {
				if content, ok := msg["content"].([]any); ok {
					for _, c := range content {
						if block, ok := c.(map[string]any); ok && block["type"] == "tool_use" {
							res.sawToolUse = true
							name, _ := block["name"].(string)
							res.sawToolUseNames = append(res.sawToolUseNames, name)
							fmt.Fprintf(os.Stderr, "⚠️  Claude making new tool call: %s\n", name)
						}
					}
				}
			}
		}

		if typ == "result" {
			if r, ok := parsed["result"].(string); ok {
				res.resultText = r
			}
			// Extract token counts from the usage block.
			if u, ok := parsed["usage"].(map[string]any); ok {
				res.inputTokens = int(floatFromMap(u, "input_tokens"))
				res.outputTokens = int(floatFromMap(u, "output_tokens"))
				res.cacheCreation = int(floatFromMap(u, "cache_creation_input_tokens"))
				res.cacheRead = int(floatFromMap(u, "cache_read_input_tokens"))
			}
			_ = stdinPipe.Close()
		}
	}
	_ = cmd.Wait()

	fmt.Fprintf(os.Stderr, "  tokens: input=%d output=%d cache_creation=%d cache_read=%d\n",
		res.inputTokens, res.outputTokens, res.cacheCreation, res.cacheRead)
	return res
}

func floatFromMap(m map[string]any, key string) float64 {
	v, _ := m[key].(float64)
	return v
}

// runSyntheticSessionResume tests whether a synthetic JSONL session file
// can be used to inject conversation history (including tool_use + tool_result
// with image) into Claude CLI via --resume /path.jsonl, bypassing the stream-json
// stdin routing that always re-executes user messages with tool_result content.
// Pass intercept=true to dump the full Anthropic API wire format.
func runSyntheticSessionResume(model, claudePath, imgPath string, intercept bool) {
	fmt.Fprintln(os.Stderr, "\n╔══ synthetic-session-resume ══╗")
	fmt.Fprintln(os.Stderr, "Writing synthetic JSONL session file with: user ask → assistant tool_use → user tool_result (image)")

	_ = os.MkdirAll(probeWorkDir, 0o755)
	_ = os.MkdirAll(probeConfigDir, 0o755)

	// Start intercept proxy if requested — captures the exact API request so we
	// can see whether cache_control markers are present.
	var proxyAddr string
	if intercept {
		addrCh := make(chan string, 1)
		go runProxy(addrCh)
		proxyAddr = <-addrCh
		fmt.Fprintf(os.Stderr, "intercepting proxy at %s\n", proxyAddr)
	}

	syntheticSessionID := "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
	ensureProbeImage(imgPath)

	jsonlPath, err := writeSyntheticSessionFile(imgPath, syntheticSessionID, probeWorkDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to write synthetic session file: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(jsonlPath)
	fmt.Fprintf(os.Stderr, "synthetic session file: %s\n", jsonlPath)

	// Print first-time session file contents.
	contents, _ := os.ReadFile(jsonlPath)
	lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
	fmt.Fprintf(os.Stderr, "session file (%d lines):\n", len(lines))
	for i, line := range lines {
		var pretty bytes.Buffer
		if json.Indent(&pretty, []byte(line), "  ", "  ") == nil {
			fmt.Fprintf(os.Stderr, "  line %d: %s\n\n", i+1, pretty.String())
		}
	}

	followUp := "Based on what the Read tool returned, describe the image in detail. Do NOT call any tools again."
	res := runOneSyntheticRun("synthetic-session-resume run", claudePath, model, jsonlPath, followUp, proxyAddr, true)

	fmt.Fprintln(os.Stderr, "\n=== synthetic-session-resume VERDICT ===")
	if res.sawToolUse {
		fmt.Fprintf(os.Stderr, "✗ FAIL: Claude made new tool call(s): %v\n", res.sawToolUseNames)
	} else if res.resultText != "" {
		fmt.Fprintln(os.Stderr, "✓ PASS: Claude synthesized from injected history — NO re-execution!")
		fmt.Fprintf(os.Stderr, "  Result: %s\n", res.resultText)
	} else {
		fmt.Fprintln(os.Stderr, "? INCONCLUSIVE: no result observed")
	}
	fmt.Fprintf(os.Stderr, "  cache_creation=%d  cache_read=%d  (0 means no cache_control in request)\n",
		res.cacheCreation, res.cacheRead)
}

// runSyntheticSessionCache runs the synthetic session TWICE with identical content
// to determine whether prompt caching works across runs.
//
// Run 1 should show cache_creation_input_tokens > 0 if Claude CLI adds
// cache_control markers when loading from synthetic JSONL.
// Run 2 (same content) should show cache_read_input_tokens > 0 if Anthropic
// cached the prefix from Run 1. If both are 0 every run, Claude CLI is not
// emitting cache_control for synthetic JSONL and we need a different approach.
func runSyntheticSessionCache(model, claudePath, imgPath string, intercept bool) {
	fmt.Fprintln(os.Stderr, "\n╔══ synthetic-session-cache (two-run cache test) ══╗")

	_ = os.MkdirAll(probeWorkDir, 0o755)
	_ = os.MkdirAll(probeConfigDir, 0o755)

	var proxyAddr string
	if intercept {
		addrCh := make(chan string, 1)
		go runProxy(addrCh)
		proxyAddr = <-addrCh
		fmt.Fprintf(os.Stderr, "intercepting proxy at %s\n", proxyAddr)
	}

	syntheticSessionID := "cccccccc-cccc-4ccc-cccc-cccccccccccc"
	ensureProbeImage(imgPath)

	// Write once — reuse the SAME file for both runs so content is byte-identical.
	jsonlPath, err := writeSyntheticSessionFile(imgPath, syntheticSessionID, probeWorkDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to write synthetic session file: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(jsonlPath)
	fmt.Fprintf(os.Stderr, "synthetic session file (shared across both runs): %s\n\n", jsonlPath)

	followUp := "Based on what the Read tool returned, describe the image in detail. Do NOT call any tools again."

	fmt.Fprintln(os.Stderr, "━━━ Run 1 (expect cache_creation > 0 if Claude CLI adds cache_control) ━━━")
	run1 := runOneSyntheticRun("run-1", claudePath, model, jsonlPath, followUp, proxyAddr, false)

	// Brief pause to ensure Anthropic's cache is populated before run 2.
	fmt.Fprintln(os.Stderr, "\nSleeping 2s before run 2 to allow cache propagation...")
	time.Sleep(2 * time.Second)

	fmt.Fprintln(os.Stderr, "━━━ Run 2 (expect cache_read > 0 if caching works) ━━━")
	run2 := runOneSyntheticRun("run-2", claudePath, model, jsonlPath, followUp, proxyAddr, false)

	fmt.Fprintln(os.Stderr, "\n=== synthetic-session-cache RESULTS ===")
	fmt.Fprintf(os.Stderr, "Run 1:  input=%d  output=%d  cache_creation=%d  cache_read=%d\n",
		run1.inputTokens, run1.outputTokens, run1.cacheCreation, run1.cacheRead)
	fmt.Fprintf(os.Stderr, "Run 2:  input=%d  output=%d  cache_creation=%d  cache_read=%d\n",
		run2.inputTokens, run2.outputTokens, run2.cacheCreation, run2.cacheRead)

	fmt.Fprintln(os.Stderr, "\n=== VERDICT ===")
	switch {
	case run1.cacheCreation == 0 && run2.cacheCreation == 0 && run2.cacheRead == 0:
		fmt.Fprintln(os.Stderr, "✗ NO CACHING: Claude CLI is NOT adding cache_control to synthetic JSONL requests.")
		fmt.Fprintln(os.Stderr, "  → We need to inject cache_control into the session file, OR accept no caching,")
		fmt.Fprintln(os.Stderr, "    OR keep the real --session-id/--resume flow for cache benefit.")
	case run1.cacheCreation > 0 && run2.cacheRead > 0:
		saved := run2.inputTokens - (run2.inputTokens - run2.cacheRead)
		fmt.Fprintf(os.Stderr, "✓ CACHING WORKS: Run 2 hit the cache. ~%d tokens saved vs run 1.\n", saved)
		fmt.Fprintln(os.Stderr, "  → Synthetic JSONL + --resume is sufficient. No need for real session tracking.")
	case run1.cacheCreation > 0 && run2.cacheRead == 0:
		fmt.Fprintln(os.Stderr, "? PARTIAL: Cache was created in run 1 but not hit in run 2.")
		fmt.Fprintln(os.Stderr, "  → Content may differ slightly between runs (timestamp in JSONL?), or cache TTL issue.")
	default:
		fmt.Fprintf(os.Stderr, "? UNEXPECTED: run1_creation=%d run2_read=%d — investigate proxy logs.\n",
			run1.cacheCreation, run2.cacheRead)
	}
}

// ensureProbeImage copies imgPath to /tmp/probe-logo.png if not already present.
func ensureProbeImage(imgPath string) {
	if _, err := os.Stat("/tmp/probe-logo.png"); err != nil {
		src, err2 := os.ReadFile(imgPath)
		if err2 != nil {
			fmt.Fprintf(os.Stderr, "cannot read image %q: %v\n", imgPath, err2)
			os.Exit(1)
		}
		if err3 := os.WriteFile("/tmp/probe-logo.png", src, 0o644); err3 != nil {
			fmt.Fprintf(os.Stderr, "cannot write /tmp/probe-logo.png: %v\n", err3)
			os.Exit(1)
		}
	}
}

func runMultiTurn(scenario, model, systemPrompt, claudePath string, intercept, denyAll bool) {
	var proxyAddr string
	if intercept {
		addrCh := make(chan string, 1)
		go runProxy(addrCh)
		proxyAddr = <-addrCh
		fmt.Fprintf(os.Stderr, "intercepting proxy at %s\n", proxyAddr)
	}

	// Scenario-specific configuration.
	type turnCfg struct {
		label     string
		prompt    string
		tools     []string
		mcpConfig string
	}

	var turns [2]turnCfg
	deleteSessionBetween := scenario == "session-missing"

	switch scenario {
	case "session-simple", "session-missing":
		turns[0] = turnCfg{
			label:  "Turn1/seed",
			prompt: "My favourite colour is blue. Acknowledge this and nothing else.",
			tools:  []string{},
		}
		turns[1] = turnCfg{
			label:  "Turn2/resume",
			prompt: "What is my favourite colour? Answer with just the colour name.",
			tools:  []string{},
		}

	case "session-builtin":
		turns[0] = turnCfg{
			label:  "Turn1/seed",
			prompt: "Run `echo probe-sentinel` with Bash. Report the exact output.",
			tools:  []string{"Bash"},
		}
		turns[1] = turnCfg{
			label:  "Turn2/resume",
			prompt: "What was the output of the command you ran? Repeat it exactly.",
			tools:  []string{"Bash"},
		}

	case "session-mcp":
		mcpCfg, err := writeMCPConfig(false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to write MCP config: %v\n", err)
			os.Exit(1)
		}
		defer os.Remove(mcpCfg)
		turns[0] = turnCfg{
			label:     "Turn1/seed",
			prompt:    "Call the get_status tool. Report what it returned.",
			tools:     []string{},
			mcpConfig: mcpCfg,
		}
		turns[1] = turnCfg{
			label:     "Turn2/resume",
			prompt:    "What did the get_status tool return in the previous turn? Summarise in one sentence.",
			tools:     []string{},
			mcpConfig: mcpCfg,
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown multi-turn scenario %q\n", scenario)
		os.Exit(1)
	}

	// Turn 1: seed — always start fresh with a fixed session UUID so we control the ID.
	fixedSessionID := "550e8400-e29b-41d4-a716-" + fmt.Sprintf("%012x", os.Getpid())
	// Ensure it looks like a valid UUID segment.
	fixedSessionID = fmt.Sprintf("550e8400-e29b-41d4-a716-%012d", os.Getpid())

	cliSessionID, ok := runTurn(
		turns[0].label, claudePath, model, systemPrompt,
		fixedSessionID, "", turns[0].prompt,
		turns[0].tools, turns[0].mcpConfig, proxyAddr, denyAll,
	)
	if !ok {
		fmt.Fprintf(os.Stderr, "turn 1 failed\n")
		os.Exit(1)
	}

	// Optionally delete session file to test fallback.
	if deleteSessionBetween && cliSessionID != "" {
		p := sessionFilePath(probeConfigDir, probeWorkDir, cliSessionID)
		if err := os.Remove(p); err == nil {
			fmt.Fprintf(os.Stderr, "\n[probe] deleted session file %s to simulate missing-session fallback\n", p)
		} else {
			fmt.Fprintf(os.Stderr, "\n[probe] session file not found to delete (%v) — --no-session-persistence is active?\n", err)
		}
	}

	// Verify session file existence.
	if cliSessionID != "" {
		p := sessionFilePath(probeConfigDir, probeWorkDir, cliSessionID)
		if _, err := os.Stat(p); err == nil {
			fmt.Fprintf(os.Stderr, "\n[probe] session file exists: %s\n", p)
		} else {
			fmt.Fprintf(os.Stderr, "\n[probe] session file NOT found: %s (%v)\n", p, err)
			fmt.Fprintf(os.Stderr, "[probe] turn 2 will have to fall back to full seed\n")
		}
	}

	// Turn 2: resume (or fallback seed if session file missing).
	resumeID := cliSessionID
	if _, err := os.Stat(sessionFilePath(probeConfigDir, probeWorkDir, cliSessionID)); err != nil {
		fmt.Fprintf(os.Stderr, "[probe] using seed path for turn 2 (session unavailable)\n")
		resumeID = ""
	}

	runTurn(
		turns[1].label, claudePath, model, systemPrompt,
		"", resumeID, turns[1].prompt,
		turns[1].tools, turns[1].mcpConfig, proxyAddr, denyAll,
	)
}

func main() {
	// Internal modes.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--mcp-server":
			runMCPServer(false)
			return
		case "--mcp-server-live":
			runMCPServer(true)
			return
		case "--proxy":
			addrCh := make(chan string, 1)
			go runProxy(addrCh)
			addr := <-addrCh
			fmt.Println(addr) // print address to stdout for parent process
			select {}         // block forever
		}
	}

	scenario := flag.String("scenario", "mcp", "Test scenario: builtin, mcp, mixed, history, session-simple, session-builtin, mcp-parallel-live")
	prompt := flag.String("prompt", "", "Custom prompt (overrides scenario default)")
	claudePath := flag.String("claude", "claude", "Path to claude binary")
	// Default to haiku — the probe is a diagnostic tool, not a production run.
	// Opus burns through the 5-hour token budget in seconds.
	model := flag.String("model", "claude-haiku-4-5", "Model to use (default: haiku-4-5 to keep probe costs low)")
	// systemPrompt mirrors what the production worker passes via --system-prompt.
	// Defaults to a minimal sentinel so we can confirm --system-prompt replaces
	// (not appends to) the CLI's default coding-agent system prompt.
	systemPrompt := flag.String("system-prompt", "You are a helpful assistant. This is a probe test system prompt.", "System prompt passed to claude via --system-prompt (replaces default)")
	systemPromptFile := flag.String("system-prompt-file", "", "Path to a file whose contents are passed via --system-prompt-file instead of --system-prompt (mutually exclusive with -system-prompt)")
	respondDelay := flag.Duration("respond-delay", 0, "Delay before sending control responses")
	denyAll := flag.Bool("deny-all", true, "Deny all MCP tool permission requests (default)")
	ignoreControl := flag.Bool("ignore-control", false, "Don't respond to any control_request")
	intercept := flag.Bool("intercept", false, "Start intercepting proxy and log Anthropic API wire traffic")
	rawDump := flag.Bool("raw", false, "Print full raw JSON for every received event (always on for mcp-parallel-live)")
	sessionID := flag.String("session-id", "", "Fixed session UUID (auto-generated if empty); printed on completion for use with --resume")
	resumeID := flag.String("resume", "", "Resume this session ID instead of starting fresh")
	deleteSession := flag.Bool("delete-session", false, "Delete the session file after the run (simulates missing-session fallback)")
	imagePath := flag.String("image", "/Users/kminkler/src/auxot/auxothq/auxot-server/web/node_modules/vue-grid-layout-v3/website/docs/logo.png", "Path to image file used by history-tool-image scenario")
	flag.Parse()
	_ = imagePath
	// Always dump raw JSON for recon scenarios.
	if *scenario == "mcp-parallel-live" || *scenario == "live-read-image" ||
		*scenario == "live-read-text" || *scenario == "history-tool-image" ||
		*scenario == "history-tool-text" || *scenario == "session-file-replay" ||
		*scenario == "synthetic-session-resume" || *scenario == "synthetic-session-cache" {
		*rawDump = true
	}

	// -system-prompt-file and -system-prompt are mutually exclusive.
	// When -system-prompt-file is set it takes precedence; clear the inline value.
	if *systemPromptFile != "" {
		*systemPrompt = ""
	}

	// Multi-turn session scenarios are handled by runMultiTurn.
	switch *scenario {
	case "session-simple", "session-builtin", "session-mcp", "session-missing":
		runMultiTurn(*scenario, *model, *systemPrompt, *claudePath, *intercept, *denyAll)
		return
	case "session-file-replay":
		runSessionFileReplay(*model, *claudePath)
		return
	case "synthetic-session-resume":
		imgPath := flag.Lookup("image").Value.String()
		runSyntheticSessionResume(*model, *claudePath, imgPath, *intercept)
		return
	case "synthetic-session-cache":
		imgPath := flag.Lookup("image").Value.String()
		runSyntheticSessionCache(*model, *claudePath, imgPath, *intercept)
		return
	}

	prompts := map[string]string{
		"builtin": "Run `pwd` and `echo hello` in two separate bash commands. Do both at once in parallel.",
		"mcp":     "Call the get_status and get_version tools in parallel.",
		"mixed":   "Run `echo hello` with Bash, and also call the get_status tool. Do both in parallel.",
		// history: verifies multi-turn stdin injection — prior user+assistant turn
		// is sent before this prompt; check --intercept wire to confirm the API
		// receives a proper 3-entry messages array, not a flat single-message blob.
		"history": "What did I just ask you? Repeat it back word for word.",
		// history-tool: verifies that tool_use + tool_result pairs can be injected
		// in stream-json stdin history. If Claude correctly reports what the tool
		// returned, full structured history works and the text-blob approach is
		// unnecessary.
		"history-tool": "Earlier you used a tool called lookup_colour. What exact string did it return? Repeat it verbatim.",
		// history-tool-image: injects an opaque tool (generate_image) + is_error:false.
		// No re-execution possible. If the injected image reaches the model, the model
		// will describe it. If Claude CLI strips it, the model will say it has no image.
		// Definitive test of whether is_error:false unlocks structured tool history.
		"history-tool-image": "", // no trailing prompt; tool_result IS the last message
		// history-blob-image: tests our ACTUAL approach — the cliworker sends
		// conversation history as a text blob, and images from tool results are
		// injected as native Anthropic image content blocks in the SAME user
		// message alongside the text. This is what buildPrompt + extractImageBlocks
		// produces. If Claude can describe the image, our fix is correct.
		"history-blob-image": "Describe in detail what you see in the image that was returned by the read tool.",
		// live-read-image: let Claude CLI actually execute Read on a real image.
		// Full raw JSON dumped for every event so we can see the EXACT stream-json
		// format of the tool_use block and the tool_result user event that Claude CLI
		// produces. We need this format to replay it correctly as history input.
		"live-read-image": "Read the image at /tmp/probe-logo.png using the Read tool. Then describe what you see in it.",
		// live-read-text: control test — let Claude CLI read a TEXT file with Read.
		// Captures exact wire format for a text tool_result for comparison with
		// the image case, and as ground truth for history-tool-text injection.
		"live-read-text": "Read the file /tmp/probe-test.txt using the Read tool. Tell me the secret code it contains.",
		// history-tool-text: control injection test — injects Read tool_use +
		// tool_result with TEXT content (not image). If the model reports the
		// secret code correctly, structured text tool history injection works.
		// Compare with history-tool-image to isolate whether stripping is
		// content-type-specific or affects all injected tool results.
		"history-tool-text": "", // no trailing prompt; model must synthesise from injected result
		// session-file-replay: two-phase test.
		//   Phase 1 — run a live Read session WITH session persistence, capture session ID.
		//   Phase 2 — read the session file Claude wrote, then replay those exact NDJSON
		//             lines as stream-json stdin to a NEW session (no --resume). Ask the
		//             model the follow-up question. If it knows the secret code without
		//             re-executing, the session file format IS the correct injection format.
		"session-file-replay": "What was the secret code in the file you just read?",
		// parallel-mcp-kill: probe whether ALL tool_use blocks are present in the
		// assistant event BEFORE the first control_request fires. If they are, we
		// can kill Claude at first control_request without denying and capture all
		// tool calls. Key test for the "kill-not-deny" architecture.
		"parallel-mcp-kill": "Call the get_status and get_version tools in parallel. I need both results.",
		// mcp-live: Claude runs a full agentic MCP loop — tool calls are actually
		// executed by the MCP server (bash included) and results fed back to Claude.
		// Claude terminates naturally when done. Tests the "live continuation" model.
		"mcp-live": "Call the bash tool with command `echo hello-from-live && date`. Report the exact output.",
		// mcp-parallel-live: ground-truth recon run. Two simple MCP tools (tool1, tool2)
		// executed in parallel with real results. Full raw JSON dumped for every event.
		// This tells us exactly how many assistant events Claude emits, when tool_use
		// blocks appear, what the user event carrying both results looks like, and when
		// rate_limit_event fires. Required reading before fixing the cliworker.
		"mcp-parallel-live": "Call tool1 and tool2 simultaneously in parallel right now. Do not explain anything first, just call both tools at the same time.",
	}
	if *prompt == "" {
		p, ok := prompts[*scenario]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown scenario %q\n", *scenario)
			os.Exit(1)
		}
		*prompt = p
	}
	_ = sessionID
	_ = resumeID
	_ = deleteSession

	fmt.Fprintf(os.Stderr, "=== claude-probe scenario=%s intercept=%v ===\n", *scenario, *intercept)
	fmt.Fprintf(os.Stderr, "prompt: %s\n", *prompt)
	fmt.Fprintf(os.Stderr, "deny_all=%v respond_delay=%v ignore_control=%v\n\n", *denyAll, *respondDelay, *ignoreControl)

	// Start the intercepting proxy if requested.
	var proxyAddr string
	if *intercept {
		addrCh := make(chan string, 1)
		go runProxy(addrCh)
		proxyAddr = <-addrCh
		fmt.Fprintf(os.Stderr, "intercepting proxy listening at %s\n\n", proxyAddr)
	}

	var mcpConfigFile string
	needsMCP := map[string]bool{"mcp": true, "mixed": true, "parallel-mcp-kill": true, "mcp-live": true, "mcp-parallel-live": true}
	if needsMCP[*scenario] {
		liveMCP := *scenario == "mcp-live" || *scenario == "mcp-parallel-live"
		var err error
		mcpConfigFile, err = writeMCPConfig(liveMCP)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to create MCP config: %v\n", err)
			os.Exit(1)
		}
		defer os.Remove(mcpConfigFile)
	}

	args := buildArgs(*scenario, *prompt, *model, *systemPrompt, *systemPromptFile, mcpConfigFile)
	fmt.Fprintf(os.Stderr, "cmd: %s %s\n\n", *claudePath, strings.Join(args, " "))

	_ = os.MkdirAll(probeWorkDir, 0o755)
	_ = os.MkdirAll(probeConfigDir, 0o755)

	cmd := exec.Command(*claudePath, args...)
	// Place claude and all its children (including probe --mcp-server instances)
	// in their own process group so we can kill the entire tree on exit.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = probeWorkDir
	cmd.Env = probeEnv(proxyAddr)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	stdinPipe, _ := cmd.StdinPipe()

	// Kill the entire process group on SIGINT/SIGTERM so no claude children or
	// probe --mcp-server grandchildren linger and burn subscription tokens.
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		<-ch
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		os.Exit(1)
	}()

	start := time.Now()
	var mu sync.Mutex
	var events []event
	seq := 0

	record := func(dir, raw, typ, summary string) {
		mu.Lock()
		defer mu.Unlock()
		seq++
		e := event{
			Seq:       seq,
			Direction: dir,
			ElapsedMS: time.Since(start).Milliseconds(),
			Raw:       raw,
			Type:      typ,
			Summary:   summary,
		}
		events = append(events, e)
		fmt.Fprintf(os.Stderr, "[%04d %6dms %s] type=%-20s %s\n",
			e.Seq, e.ElapsedMS, e.Direction, e.Type, e.Summary)
		// Full raw JSON dump — essential for recon scenarios.
		if *rawDump && raw != "" && dir == "recv" {
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, []byte(raw), "  ", "  "); err == nil {
				fmt.Fprintf(os.Stderr, "  RAW:\n  %s\n", pretty.String())
			} else {
				fmt.Fprintf(os.Stderr, "  RAW: %s\n", raw)
			}
		}
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start claude: %v\n", err)
		os.Exit(1)
	}

	go func() {
		stderrScanner := bufio.NewScanner(stderr)
		stderrScanner.Buffer(make([]byte, 1<<20), 1<<20)
		for stderrScanner.Scan() {
			line := stderrScanner.Text()
			if line != "" {
				record("stderr", line, "STDERR", line[:min(80, len(line))])
			}
		}
	}()

	sendJSON := func(label string, v any) {
		data, _ := json.Marshal(v)
		record("send", string(data), "user", label)
		fmt.Fprintln(stdinPipe, string(data))
	}

	if stdinPipe != nil {
		// history scenario: inject a prior user+assistant turn before the real
		// prompt to verify the CLI builds a proper multi-turn messages array on
		// the wire rather than requiring us to flatten history into a text blob.
		if *scenario == "history" {
			sendJSON("history user turn", map[string]any{
				"type": "user",
				"message": map[string]any{
					"role":    "user",
					"content": []map[string]any{{"type": "text", "text": "What is the capital of France?"}},
				},
			})
			sendJSON("history assistant turn", map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": []map[string]any{{"type": "text", "text": "Paris."}},
				},
			})
		}

		// history-blob-image scenario: mirrors exactly what our cliworker does.
		// The full conversation (including a synthetic tool_use + tool_result) is
		// sent as a TEXT BLOB in a single user message, with the image bytes
		// appended as a separate native image content block in that same message.
		// This is the approach in buildPrompt after our image-injection fix.
		if *scenario == "history-blob-image" {
			imgPath := flag.Lookup("image").Value.String()
			imgBytes, err := os.ReadFile(imgPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "history-blob-image: cannot read image %q: %v\n", imgPath, err)
				os.Exit(1)
			}
			b64img := base64.StdEncoding.EncodeToString(imgBytes)

			// Simulate what buildPrompt produces: a text blob that represents the
			// entire conversation history including the tool_use + tool_result turns.
			historyBlob := "<conversation_history>\n" +
				"[user]\nPlease read the file /tmp/logo.png and describe what it contains.\n\n" +
				"[assistant]\nI'll read that file for you.\n[tool_use: read({\"path\":\"/tmp/logo.png\"})]\n\n" +
				"[tool_result (id: toolu_probe_read_01)]\nRead image file [image/png]\n" +
				"[Image data from this tool result is attached as a native image block in this message.]\n" +
				"</conversation_history>"

			// Send ONE user message: text blob + native image block.
			// This is exactly what cliworker does when building the stdin message.
			sendJSON("history-blob + image block", map[string]any{
				"type": "user",
				"message": map[string]any{
					"role": "user",
					"content": []map[string]any{
						{"type": "text", "text": historyBlob},
						{
							"type": "image",
							"source": map[string]any{
								"type":       "base64",
								"media_type": "image/png",
								"data":       b64img,
							},
						},
					},
				},
			})
		}

		// history-tool-image scenario: inject an OPAQUE tool (generate_image) that
		// Claude CLI has no handler for, so re-execution is impossible.
		// Combined with is_error:false (per GH #16712), this is the cleanest test of
		// whether injected tool history with an image passes through to the model.
		// If the model describes the image -> structured tool history works.
		// If it errors or asks to generate -> image data is stripped by Claude CLI.
		if *scenario == "history-tool-image" {
			imgPath := flag.Lookup("image").Value.String()
			imgBytes, err := os.ReadFile(imgPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "history-tool-image: cannot read image %q: %v\n", imgPath, err)
				os.Exit(1)
			}
			b64img := base64.StdEncoding.EncodeToString(imgBytes)

			// 1. User turn: ask to generate and describe an image.
			sendJSON("history user ask", map[string]any{
				"type": "user",
				"message": map[string]any{
					"role":    "user",
					"content": []map[string]any{{"type": "text", "text": "Generate a random image using the generate_image tool, then describe what it looks like."}},
				},
			})

			// 2. Assistant turn: generate_image tool_use (opaque — no handler in CLI).
			sendJSON("history assistant tool_use (generate_image, opaque)", map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"id":   "msg_01GLwak4EYd8Afcfy46dPhRH",
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{{
						"type":  "tool_use",
						"id":    "toolu_01RZKzStX57MbeAsNZWqfX5w",
						"name":  "generate_image",
						"input": map[string]any{"prompt": "a random abstract shape"},
					}},
					"stop_reason": "tool_use",
				},
			})

			// 3. User turn: tool_result with is_error:false and the image payload.
			//    No re-execution is possible (generate_image is not a registered tool).
			//    If this image reaches the model, structured tool history works.
			sendJSON("history tool_result with image (is_error:false)", map[string]any{
				"type": "user",
				"message": map[string]any{
					"role": "user",
					"content": []map[string]any{{
						"type":        "tool_result",
						"tool_use_id": "toolu_01RZKzStX57MbeAsNZWqfX5w",
						"is_error":    false,
						"content": []map[string]any{{
							"type": "image",
							"source": map[string]any{
								"type":       "base64",
								"media_type": "image/png",
								"data":       b64img,
							},
						}},
					}},
				},
			})
		}

		// history-tool-text: control injection test with TEXT content.
		// Exact same structure as history-tool-image but content is a text string.
		// If the model reports the secret code, text injection works.
		// If it fails the same way as image injection, all tool_use blocks are
		// intercepted by Claude CLI regardless of content type.
		if *scenario == "history-tool-text" {
			secretText := "This is a probe test file. It contains a secret code: AUXOT-PROBE-42. The quick brown fox jumps over the lazy dog."

			// 1. User turn: ask to read the file.
			sendJSON("history user ask", map[string]any{
				"type": "user",
				"message": map[string]any{
					"role":    "user",
					"content": []map[string]any{{"type": "text", "text": "Read the file /tmp/probe-test.txt and tell me the secret code it contains."}},
				},
			})

			// 2. Assistant turn: Read tool_use (opaque — no Read tool registered).
			sendJSON("history assistant tool_use (Read, no tools registered)", map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"id":   "msg_01TextProbeAAAAAAAAAAAAAAA",
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{{
						"type":  "tool_use",
						"id":    "toolu_01TextProbeAAAAAAAAAAAA",
						"name":  "Read",
						"input": map[string]any{"file_path": "/tmp/probe-test.txt"},
					}},
					"stop_reason": "tool_use",
				},
			})

			// 3. User turn: tool_result with the text content + is_error:false.
			sendJSON("history tool_result with text (is_error:false)", map[string]any{
				"type": "user",
				"message": map[string]any{
					"role": "user",
					"content": []map[string]any{{
						"type":        "tool_result",
						"tool_use_id": "toolu_01TextProbeAAAAAAAAAAAA",
						"is_error":    false,
						"content": []map[string]any{{
							"type": "text",
							"text": secretText,
						}},
					}},
				},
			})
		}

		// history-tool scenario: inject an assistant turn with a tool_use block
		// followed by a user turn with the tool_result. Then ask Claude what the
		// tool returned. If it answers correctly, structured tool history works in
		// stream-json and the text-blob approach is unnecessary.
		if *scenario == "history-tool" {
			// Turn 1: user asks.
			sendJSON("history user prompt", map[string]any{
				"type": "user",
				"message": map[string]any{
					"role":    "user",
					"content": []map[string]any{{"type": "text", "text": "What colour is the sky? Use the lookup_colour tool."}},
				},
			})
			// Turn 2: assistant calls the tool.
			sendJSON("history assistant tool_use", map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"role": "assistant",
					"content": []map[string]any{{
						"type":  "tool_use",
						"id":    "toolu_probe_colour_01",
						"name":  "lookup_colour",
						"input": map[string]any{"query": "sky"},
					}},
				},
			})
			// Turn 3: tool result (the "server" returns a canned string).
			sendJSON("history tool_result", map[string]any{
				"type": "user",
				"message": map[string]any{
					"role": "user",
					"content": []map[string]any{{
						"type":        "tool_result",
						"tool_use_id": "toolu_probe_colour_01",
						"content": []map[string]any{{
							"type": "text",
							"text": "SENTINEL_RESULT: the sky is cornflower-blue",
						}},
					}},
				},
			})
		}

		// Scenarios with an empty prompt end with the injected tool_result.
		// The model must synthesize without a further user message.
		// Also guard against sending an empty text block (causes API 400).
		if *prompt != "" {
			sendJSON("initial prompt", map[string]any{
				"type": "user",
				"message": map[string]any{
					"role":    "user",
					"content": []map[string]any{{"type": "text", "text": *prompt}},
				},
			})
		}
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			record("recv", line, "PARSE_ERROR", err.Error())
			continue
		}
		typ, _ := parsed["type"].(string)
		summary := summarizeEvent(typ, parsed)
		record("recv", line, typ, summary)

		if typ == "control_request" && stdinPipe != nil && !*ignoreControl {
			if *respondDelay > 0 {
				time.Sleep(*respondDelay)
			}
			reqID, _ := parsed["request_id"].(string)
			req, _ := parsed["request"].(map[string]any)
			toolName, _ := req["tool_name"].(string)

			if *scenario == "parallel-mcp-kill" && strings.HasPrefix(toolName, "mcp__") {
				// Kill-not-deny: close stdin immediately without sending any response.
				// The question: were ALL tool_use blocks already captured from the
				// assistant event before this first control_request fired?
				// If yes, we have all tool calls and can proceed without denying.
				record("send", "", "kill_not_deny", fmt.Sprintf("closing stdin at control_request for %s (no deny sent)", toolName))
				stdinPipe.Close()
				stdinPipe = nil
				break
			} else if strings.HasPrefix(toolName, "mcp__") && *denyAll {
				resp := map[string]any{
					"type": "control_response",
					"response": map[string]any{
						"subtype":    "success",
						"request_id": reqID,
						"response":   map[string]any{"behavior": "deny", "message": "handled externally"},
					},
				}
				data, _ := json.Marshal(resp)
				record("send", string(data), "control_response", fmt.Sprintf("DENY %s", toolName))
				fmt.Fprintln(stdinPipe, string(data))
			} else {
				var updatedInput any
				if inputRaw, ok := req["input"]; ok {
					updatedInput = inputRaw
				}
				if updatedInput == nil {
					updatedInput = map[string]any{}
				}
				resp := map[string]any{
					"type": "control_response",
					"response": map[string]any{
						"subtype":    "success",
						"request_id": reqID,
						"response":   map[string]any{"behavior": "allow", "updatedInput": updatedInput},
					},
				}
				data, _ := json.Marshal(resp)
				record("send", string(data), "control_response", fmt.Sprintf("ALLOW %s", toolName))
				fmt.Fprintln(stdinPipe, string(data))
			}
		}

		if typ == "result" && stdinPipe != nil {
			record("send", "", "close_stdin", "closing stdin after result event")
			stdinPipe.Close()
			stdinPipe = nil
		}
	}

	exitErr := cmd.Wait()
	// Always kill the entire process group after claude exits so that any
	// lingering probe --mcp-server grandchildren are reaped immediately.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	exitCode := 0
	if exitErr != nil {
		if e, ok := exitErr.(*exec.ExitError); ok {
			exitCode = e.ExitCode()
		} else {
			exitCode = -1
		}
	}

	fmt.Fprintf(os.Stderr, "\n=== probe complete (exit=%d, %dms, %d events) ===\n\n",
		exitCode, time.Since(start).Milliseconds(), len(events))

	fmt.Println("--- EVENT TIMELINE ---")
	for _, e := range events {
		fmt.Printf("[%04d %6dms %s] %-20s %s\n", e.Seq, e.ElapsedMS, e.Direction, e.Type, e.Summary)
	}

	fmt.Println("\n--- EVENT TYPE COUNTS ---")
	counts := map[string]int{}
	for _, e := range events {
		counts[e.Type]++
	}
	for t, c := range counts {
		fmt.Printf("  %-25s %d\n", t, c)
	}

	fmt.Println("\n--- RATE_LIMIT_EVENT ANALYSIS ---")
	analyzeRateLimitTiming(events)

	fmt.Println("\n--- FULL EVENT LOG (JSON) ---")
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(events)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func buildArgs(scenario, prompt, model, systemPrompt, systemPromptFile, mcpConfigFile string) []string {
	args := []string{
		"--output-format", "stream-json",
		"--verbose",
		// Never persist sessions to disk — conversation history lives in the DB.
		"--no-session-persistence",
	}

	// --system-prompt-file and --system-prompt both replace (or append to?) the
	// CLI's default coding-agent system prompt. The intercept probe lets us
	// confirm which behaviour applies on the wire.
	switch {
	case systemPromptFile != "":
		args = append(args, "--system-prompt-file", systemPromptFile)
	case systemPrompt != "":
		args = append(args, "--system-prompt", systemPrompt)
	}

	switch scenario {
	case "builtin":
		args = append(args,
			"--tools", "Bash",
			"--print",
			"--input-format", "stream-json",
			"--permission-prompt-tool", "stdio",
		)
	case "mcp":
		args = append(args,
			"--tools", "",
			"--print",
			"--input-format", "stream-json",
			"--permission-prompt-tool", "stdio",
			"--strict-mcp-config",
			"--mcp-config", mcpConfigFile,
		)
	case "mixed":
		args = append(args,
			"--tools", "Bash",
			"--print",
			"--input-format", "stream-json",
			"--permission-prompt-tool", "stdio",
			"--strict-mcp-config",
			"--mcp-config", mcpConfigFile,
		)
	case "history", "history-tool", "history-blob-image":
		// No tools — pure conversational multi-turn to test history injection.
		// history-tool additionally injects a synthetic tool_use+tool_result pair
		// to verify that structured tool history works in stream-json stdin.
		// history-blob-image tests our actual cliworker approach: text blob + native image block.
		args = append(args,
			"--tools", "",
			"--print",
			"--input-format", "stream-json",
			"--permission-prompt-tool", "stdio",
		)
	case "history-tool-image":
		// generate_image (opaque tool) + is_error:false + no tools registered.
		// Claude CLI can't re-execute generate_image. With is_error:false on the
		// tool_result, does the state builder treat it as complete and pass the
		// image to the model? Or does it still strip it and replace with an error?
		args = append(args,
			"--tools", "",
			"--print",
			"--input-format", "stream-json",
			"--permission-prompt-tool", "stdio",
		)
	case "live-read-image":
		// Let Claude CLI actually execute Read on a real image file.
		// --dangerously-skip-permissions so Read fires without a control_request.
		// Full raw JSON is always dumped (rawDump forced on below).
		args = append(args,
			"--tools", "Read",
			"--print",
			"--input-format", "stream-json",
			"--dangerously-skip-permissions",
		)
	case "live-read-text":
		// Control test: let Claude CLI read a TEXT file with Read.
		// Compare wire format vs live-read-image to confirm text results differ.
		args = append(args,
			"--tools", "Read",
			"--print",
			"--input-format", "stream-json",
			"--dangerously-skip-permissions",
		)
	case "session-file-replay":
		// Phase 1: run with session persistence ON so the CLI writes a session file.
		// Phase 2 is handled inline in the stdin-injection block below.
		// No --no-session-persistence here — that's intentional.
		args = append(args,
			"--tools", "Read",
			"--print",
			"--input-format", "stream-json",
			"--dangerously-skip-permissions",
		)
	case "history-tool-text":
		// Control injection test: inject Read tool_use + tool_result with TEXT.
		// No tools registered so no re-execution possible via that path.
		// --tools "" means Claude CLI can't re-execute Read either.
		args = append(args,
			"--tools", "",
			"--print",
			"--input-format", "stream-json",
			"--permission-prompt-tool", "stdio",
		)
	case "parallel-mcp-kill":
		// Same as mcp but we close stdin at first control_request without denying.
		// If all tool_use blocks are already captured from the assistant event,
		// we should have N tool calls even though Claude only saw N-1 (or 0) denials.
		args = append(args,
			"--tools", "",
			"--print",
			"--input-format", "stream-json",
			"--permission-prompt-tool", "stdio",
			"--strict-mcp-config",
			"--mcp-config", mcpConfigFile,
		)
	case "mcp-live":
		// Allow all tool calls — the MCP server (--mcp-server-live) actually executes them.
		// Claude runs to natural completion; no denial, no kill.
		// Uses dangerously-skip-permissions because all tools are MCP tools we trust.
		args = append(args,
			"--tools", "",
			"--print",
			"--input-format", "stream-json",
			"--dangerously-skip-permissions",
			"--strict-mcp-config",
			"--mcp-config", mcpConfigFile,
		)
	case "mcp-parallel-live":
		// Ground-truth recon: two parallel MCP tools (tool1, tool2) both executed live.
		// Uses dangerously-skip-permissions — no control_request events, tool calls go
		// directly to the MCP server subprocess.  The full raw JSON of every stream-json
		// event is printed so we can see exactly what Claude CLI emits.
		args = append(args,
			"--tools", "",
			"--print",
			"--input-format", "stream-json",
			"--dangerously-skip-permissions",
			"--strict-mcp-config",
			"--mcp-config", mcpConfigFile,
		)
	}

	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}

// writeMCPConfig uses THIS binary in --mcp-server mode as the MCP server,
// making the probe fully self-contained.
// When live=true the server uses --mcp-server-live so tools/call requests
// are actually executed (bash runs for real, etc.).
func writeMCPConfig(live bool) (string, error) {
	probeBin, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving probe executable: %w", err)
	}
	serverArg := "--mcp-server"
	if live {
		serverArg = "--mcp-server-live"
	}
	config := map[string]any{
		"mcpServers": map[string]any{
			"auxot": map[string]any{
				"command": probeBin,
				"args":    []string{serverArg},
			},
		},
	}
	configData, _ := json.Marshal(config)
	f, err := os.CreateTemp("", "probe-mcp-*.json")
	if err != nil {
		return "", err
	}
	f.Write(configData)
	f.Close()
	return f.Name(), nil
}

func summarizeEvent(typ string, parsed map[string]any) string {
	switch typ {
	case "assistant":
		msg, _ := parsed["message"].(map[string]any)
		if msg == nil {
			return "(no message)"
		}
		msgID, _ := msg["id"].(string)
		stopReason := "(streaming)"
		if sr, ok := msg["stop_reason"]; ok && sr != nil {
			stopReason = fmt.Sprintf("stop=%v", sr)
		}
		content, _ := msg["content"].([]any)
		var bs []string
		for _, c := range content {
			block, _ := c.(map[string]any)
			bType, _ := block["type"].(string)
			switch bType {
			case "text":
				text, _ := block["text"].(string)
				bs = append(bs, fmt.Sprintf("text(%d chars)", len(text)))
			case "tool_use":
				name, _ := block["name"].(string)
				id, _ := block["id"].(string)
				if len(id) > 8 {
					id = id[:8]
				}
				bs = append(bs, fmt.Sprintf("tool_use(%s id=%s)", name, id))
			case "thinking":
				think, _ := block["thinking"].(string)
				bs = append(bs, fmt.Sprintf("thinking(%d chars)", len(think)))
			default:
				bs = append(bs, bType)
			}
		}
		if len(msgID) > 12 {
			msgID = msgID[:12]
		}
		// Extract cache usage if present.
		usage := ""
		if u, ok := msg["usage"].(map[string]any); ok {
			cacheRead, _ := u["cache_read_input_tokens"].(float64)
			cacheWrite, _ := u["cache_creation_input_tokens"].(float64)
			if cacheRead > 0 || cacheWrite > 0 {
				usage = fmt.Sprintf(" [cache_read=%.0f cache_write=%.0f]", cacheRead, cacheWrite)
			}
		}
		return fmt.Sprintf("msg=%s %s blocks=[%s]%s", msgID, stopReason, strings.Join(bs, ", "), usage)

	case "user":
		msg, _ := parsed["message"].(map[string]any)
		if msg == nil {
			return "(no message)"
		}
		content, _ := msg["content"].([]any)
		var parts []string
		for _, c := range content {
			block, _ := c.(map[string]any)
			bType, _ := block["type"].(string)
			if bType == "tool_result" {
				toolID, _ := block["tool_use_id"].(string)
				if len(toolID) > 8 {
					toolID = toolID[:8]
				}
				parts = append(parts, fmt.Sprintf("result(id=%s)", toolID))
			} else {
				parts = append(parts, bType)
			}
		}
		return fmt.Sprintf("blocks=[%s]", strings.Join(parts, ", "))

	case "control_request":
		req, _ := parsed["request"].(map[string]any)
		if req == nil {
			return ""
		}
		toolName, _ := req["tool_name"].(string)
		reqID, _ := parsed["request_id"].(string)
		if len(reqID) > 8 {
			reqID = reqID[:8]
		}
		return fmt.Sprintf("tool=%s req_id=%s", toolName, reqID)

	case "rate_limit_event":
		info, _ := parsed["rate_limit_info"].(map[string]any)
		if info == nil {
			return ""
		}
		status, _ := info["status"].(string)
		rlType, _ := info["rateLimitType"].(string)
		return fmt.Sprintf("status=%s type=%s", status, rlType)

	case "result":
		return "session complete"
	}
	return ""
}

func analyzeRateLimitTiming(events []event) {
	var lastAssistantToolUseSeq int
	var firstUserResultSeq int
	var rateLimitSeqs []int
	var firstControlRequestSeq int

	for _, e := range events {
		switch {
		case e.Type == "assistant" && strings.Contains(e.Summary, "tool_use"):
			lastAssistantToolUseSeq = e.Seq
		case e.Type == "user" && strings.Contains(e.Summary, "result") && firstUserResultSeq == 0:
			firstUserResultSeq = e.Seq
		case e.Type == "rate_limit_event":
			rateLimitSeqs = append(rateLimitSeqs, e.Seq)
		case e.Type == "control_request" && firstControlRequestSeq == 0:
			firstControlRequestSeq = e.Seq
		}
	}

	if len(rateLimitSeqs) == 0 {
		fmt.Println("  No rate_limit_event observed.")
		return
	}

	for _, rlSeq := range rateLimitSeqs {
		fmt.Printf("  rate_limit_event at seq=%d\n", rlSeq)
		if lastAssistantToolUseSeq > 0 {
			rel := "AFTER"
			if rlSeq < lastAssistantToolUseSeq {
				rel = "BEFORE"
			}
			fmt.Printf("    %s last assistant tool_use (seq=%d)\n", rel, lastAssistantToolUseSeq)
		}
		if firstControlRequestSeq > 0 {
			rel := "AFTER"
			if rlSeq < firstControlRequestSeq {
				rel = "BEFORE *** rate_limit_event precedes control_request ***"
			}
			fmt.Printf("    %s first control_request (seq=%d)\n", rel, firstControlRequestSeq)
		}
		if firstUserResultSeq > 0 {
			rel := "AFTER"
			if rlSeq < firstUserResultSeq {
				rel = "BEFORE *** kills Claude before builtin tool_result arrives ***"
			}
			fmt.Printf("    %s first user tool_result (seq=%d)\n", rel, firstUserResultSeq)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
