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
//   1. builtin         – CLI-native tools only (Bash)
//   2. mcp             – MCP tools only (get_status, get_version) — we deny all
//   3. mixed           – Bash builtin + MCP tools in the same turn
//   4. history         – Multi-turn blob injected via stdin (no --resume)
//
// Multi-turn session scenarios (verify --session-id / --resume behaviour):
//   5. session-simple   – Turn 1: plain answer. Turn 2: follow-up referencing turn 1.
//   6. session-builtin  – Turn 1: Bash tool. Turn 2: follow-up asking for its output.
//   7. session-mcp      – Turn 1: MCP tool (denied). Turn 2: follow-up.
//   8. session-missing  – Like session-simple but session file deleted between turns
//                         to verify the full-seed fallback path.
//
// Usage:
//
//	go run ./cmd/claude-probe [--scenario builtin|mcp|mixed|history] [--prompt "..."]
//	go run ./cmd/claude-probe --scenario session-simple    # multi-turn session test
//	go run ./cmd/claude-probe --mcp-server                 # internal: MCP stdio mode
//	go run ./cmd/claude-probe --proxy                      # internal: HTTP proxy mode
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
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

func runMCPServer() {
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
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	enc := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req map[string]any
		if err := json.Unmarshal([]byte(scanner.Text()), &req); err != nil {
			continue
		}
		method, _ := req["method"].(string)
		id := req["id"]
		switch method {
		case "initialize":
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "probe-mcp", "version": "1.0"},
				},
			})
		case "tools/list":
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"tools": tools}})
		case "tools/call":
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "stub"}}},
			})
		default:
			if id != nil {
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": nil})
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
		mcpCfg, err := writeMCPConfig()
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
			runMCPServer()
			return
		case "--proxy":
			addrCh := make(chan string, 1)
			go runProxy(addrCh)
			addr := <-addrCh
			fmt.Println(addr) // print address to stdout for parent process
			select {}         // block forever
		}
	}

	scenario := flag.String("scenario", "mcp", "Test scenario: builtin, mcp, mixed, history, session-simple, session-builtin")
	prompt := flag.String("prompt", "", "Custom prompt (overrides scenario default)")
	claudePath := flag.String("claude", "claude", "Path to claude binary")
	// Default to haiku — the probe is a diagnostic tool, not a production run.
	// Opus burns through the 5-hour token budget in seconds.
	model := flag.String("model", "claude-haiku-4-5", "Model to use (default: haiku-4-5 to keep probe costs low)")
	// systemPrompt mirrors what the production worker passes via --system-prompt.
	// Defaults to a minimal sentinel so we can confirm --system-prompt replaces
	// (not appends to) the CLI's default coding-agent system prompt.
	systemPrompt := flag.String("system-prompt", "You are a helpful assistant. This is a probe test system prompt.", "System prompt passed to claude via --system-prompt (replaces default)")
	respondDelay := flag.Duration("respond-delay", 0, "Delay before sending control responses")
	denyAll := flag.Bool("deny-all", true, "Deny all MCP tool permission requests (default)")
	ignoreControl := flag.Bool("ignore-control", false, "Don't respond to any control_request")
	intercept := flag.Bool("intercept", false, "Start intercepting proxy and log Anthropic API wire traffic")
	sessionID := flag.String("session-id", "", "Fixed session UUID (auto-generated if empty); printed on completion for use with --resume")
	resumeID := flag.String("resume", "", "Resume this session ID instead of starting fresh")
	deleteSession := flag.Bool("delete-session", false, "Delete the session file after the run (simulates missing-session fallback)")
	flag.Parse()

	// Multi-turn session scenarios are handled by runMultiTurn.
	switch *scenario {
	case "session-simple", "session-builtin", "session-mcp", "session-missing":
		runMultiTurn(*scenario, *model, *systemPrompt, *claudePath, *intercept, *denyAll)
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
	}
	if *prompt == "" {
		var ok bool
		*prompt, ok = prompts[*scenario]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown scenario %q\n", *scenario)
			os.Exit(1)
		}
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
	if *scenario == "mcp" || *scenario == "mixed" {
		var err error
		mcpConfigFile, err = writeMCPConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to create MCP config: %v\n", err)
			os.Exit(1)
		}
		defer os.Remove(mcpConfigFile)
	}

	args := buildArgs(*scenario, *prompt, *model, *systemPrompt, mcpConfigFile)
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

		sendJSON("initial prompt", map[string]any{
			"type": "user",
			"message": map[string]any{
				"role":    "user",
				"content": []map[string]any{{"type": "text", "text": *prompt}},
			},
		})
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

			if strings.HasPrefix(toolName, "mcp__") && *denyAll {
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

func buildArgs(scenario, prompt, model, systemPrompt, mcpConfigFile string) []string {
	args := []string{
		"--output-format", "stream-json",
		"--verbose",
		// Never persist sessions to disk — conversation history lives in the DB.
		"--no-session-persistence",
	}

	// --system-prompt REPLACES (not appends to) the CLI's default coding-agent
	// system prompt. Confirm in wire captures that block 3 becomes our content,
	// not the full Claude Code instructions.
	if systemPrompt != "" {
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
	case "history":
		// No tools — pure conversational multi-turn to test history injection.
		args = append(args,
			"--tools", "",
			"--print",
			"--input-format", "stream-json",
			"--permission-prompt-tool", "stdio",
		)
	}

	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}

// writeMCPConfig uses THIS binary in --mcp-server mode as the MCP server,
// making the probe fully self-contained.
func writeMCPConfig() (string, error) {
	probeBin, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving probe executable: %w", err)
	}
	config := map[string]any{
		"mcpServers": map[string]any{
			"auxot": map[string]any{
				"command": probeBin,
				"args":    []string{"--mcp-server"},
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
