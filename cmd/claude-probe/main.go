// claude-probe is a diagnostic tool that spawns the Claude CLI and logs every
// NDJSON event with precise timestamps to understand the protocol ordering.
//
// It tests three scenarios:
//   1. Builtin tools only (Bash) - Claude handles these internally
//   2. MCP tools with permission interception - we deny to capture tool calls
//   3. Mixed (builtin + MCP) - both in one session
//
// Usage:
//   go run ./cmd/claude-probe [--scenario builtin|mcp|mixed] [--prompt "..."]
//
// The output is a timestamped log of every event, suitable for understanding
// whether rate_limit_event can serve as a "batch complete" signal.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type event struct {
	Seq       int       `json:"seq"`
	Direction string    `json:"dir"` // "recv" or "send"
	ElapsedMS int64     `json:"elapsed_ms"`
	Raw       string    `json:"raw"`
	Type      string    `json:"type"`
	Summary   string    `json:"summary,omitempty"`
}

func main() {
	scenario := flag.String("scenario", "mcp", "Test scenario: builtin, mcp, or mixed")
	prompt := flag.String("prompt", "", "Custom prompt (overrides scenario default)")
	claudePath := flag.String("claude", "claude", "Path to claude binary")
	model := flag.String("model", "", "Model to use (empty = CLI default)")
	respondDelay := flag.Duration("respond-delay", 0, "Delay before sending control responses (test timing)")
	denyAll := flag.Bool("deny-all", true, "Deny all MCP tool permission requests (default)")
	ignoreControl := flag.Bool("ignore-control", false, "Don't respond to any control_request (test if they appear)")
	flag.Parse()

	prompts := map[string]string{
		"builtin": "Run `pwd` and `echo hello` in two separate bash commands. Do both at once in parallel.",
		"mcp":     "Call the get_status and get_version tools in parallel.",
		"mixed":   "Run `pwd` with Bash, and also call the get_status tool. Do both in parallel.",
	}
	if *prompt == "" {
		var ok bool
		*prompt, ok = prompts[*scenario]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown scenario %q, use: builtin, mcp, mixed\n", *scenario)
			os.Exit(1)
		}
	}

	fmt.Fprintf(os.Stderr, "=== claude-probe scenario=%s ===\n", *scenario)
	fmt.Fprintf(os.Stderr, "prompt: %s\n", *prompt)
	fmt.Fprintf(os.Stderr, "deny_all=%v respond_delay=%v ignore_control=%v\n\n", *denyAll, *respondDelay, *ignoreControl)

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

	args := buildArgs(*scenario, *prompt, *model, *claudePath, mcpConfigFile)
	fmt.Fprintf(os.Stderr, "cmd: %s %s\n\n", *claudePath, strings.Join(args, " "))

	cmd := exec.Command(*claudePath, args...)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	stdinPipe, _ := cmd.StdinPipe()

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
		// Print immediately for real-time observation
		fmt.Fprintf(os.Stderr, "[%04d %6dms %s] type=%-20s %s\n",
			e.Seq, e.ElapsedMS, e.Direction, e.Type, e.Summary)
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start claude: %v\n", err)
		os.Exit(1)
	}

	// Capture stderr in background
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

	// Send the initial user message if using stream-json input
	if stdinPipe != nil {
		userMsg := map[string]any{
			"type": "user",
			"message": map[string]any{
				"role":    "user",
				"content": []map[string]any{{"type": "text", "text": *prompt}},
			},
		}
		data, _ := json.Marshal(userMsg)
		record("send", string(data), "user", "initial prompt")
		fmt.Fprintln(stdinPipe, string(data))
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

		// Handle control_request events
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
						"response": map[string]any{
							"behavior": "deny",
							"message":  "Tool execution handled externally",
						},
					},
				}
				data, _ := json.Marshal(resp)
				record("send", string(data), "control_response", fmt.Sprintf("DENY mcp tool %s", toolName))
				fmt.Fprintln(stdinPipe, string(data))
			} else {
				// Allow builtin tools
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
						"response": map[string]any{
							"behavior":     "allow",
							"updatedInput": updatedInput,
						},
					},
				}
				data, _ := json.Marshal(resp)
				record("send", string(data), "control_response", fmt.Sprintf("ALLOW tool %s", toolName))
				fmt.Fprintln(stdinPipe, string(data))
			}
		}

		// Close stdin on result event to let claude exit
		if typ == "result" && stdinPipe != nil {
			record("send", "", "close_stdin", "closing stdin after result event")
			stdinPipe.Close()
			stdinPipe = nil
		}
	}

	exitErr := cmd.Wait()
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

	// Print structured summary
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

	// Analyze rate_limit_event timing
	fmt.Println("\n--- RATE_LIMIT_EVENT ANALYSIS ---")
	analyzeRateLimitTiming(events)

	// Dump full JSON for offline analysis
	fmt.Println("\n--- FULL EVENT LOG (JSON) ---")
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(events)
}

func buildArgs(scenario, prompt, model, claudePath, mcpConfigFile string) []string {
	args := []string{
		"--output-format", "stream-json",
		"--verbose",
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
	}

	if model != "" {
		args = append(args, "--model", model)
	}
	return args
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
		var blockSummaries []string
		for _, c := range content {
			block, _ := c.(map[string]any)
			bType, _ := block["type"].(string)
			switch bType {
			case "text":
				text, _ := block["text"].(string)
				if len(text) > 60 {
					text = text[:60] + "..."
				}
				blockSummaries = append(blockSummaries, fmt.Sprintf("text(%d chars)", len(text)))
			case "tool_use":
				name, _ := block["name"].(string)
				id, _ := block["id"].(string)
				blockSummaries = append(blockSummaries, fmt.Sprintf("tool_use(%s id=%s)", name, id[:min(8, len(id))]))
			case "thinking":
				blockSummaries = append(blockSummaries, "thinking")
			default:
				blockSummaries = append(blockSummaries, bType)
			}
		}
		return fmt.Sprintf("msg=%s %s blocks=[%s]", msgID[:min(12, len(msgID))], stopReason, strings.Join(blockSummaries, ", "))

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
				parts = append(parts, fmt.Sprintf("result(id=%s)", toolID[:min(8, len(toolID))]))
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
		return fmt.Sprintf("tool=%s req_id=%s", toolName, reqID[:min(8, len(reqID))])

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

	default:
		return ""
	}
}

func analyzeRateLimitTiming(events []event) {
	var lastAssistantToolUseSeq int
	var lastControlRequestSeq int
	var rateLimitSeqs []int
	var firstDenySeq int

	for _, e := range events {
		switch {
		case e.Type == "assistant" && strings.Contains(e.Summary, "tool_use"):
			lastAssistantToolUseSeq = e.Seq
		case e.Type == "control_request":
			lastControlRequestSeq = e.Seq
		case e.Type == "rate_limit_event":
			rateLimitSeqs = append(rateLimitSeqs, e.Seq)
		case e.Type == "control_response" && strings.Contains(e.Summary, "DENY") && firstDenySeq == 0:
			firstDenySeq = e.Seq
		}
	}

	if len(rateLimitSeqs) == 0 {
		fmt.Println("  No rate_limit_event observed in this session.")
		return
	}

	for _, rlSeq := range rateLimitSeqs {
		fmt.Printf("  rate_limit_event at seq=%d\n", rlSeq)
		if lastAssistantToolUseSeq > 0 {
			if rlSeq > lastAssistantToolUseSeq {
				fmt.Printf("    AFTER last assistant tool_use (seq=%d) - gap=%d events\n",
					lastAssistantToolUseSeq, rlSeq-lastAssistantToolUseSeq)
			} else {
				fmt.Printf("    BEFORE last assistant tool_use (seq=%d)\n", lastAssistantToolUseSeq)
			}
		}
		if lastControlRequestSeq > 0 {
			if rlSeq > lastControlRequestSeq {
				fmt.Printf("    AFTER last control_request (seq=%d) - gap=%d events\n",
					lastControlRequestSeq, rlSeq-lastControlRequestSeq)
			} else {
				fmt.Printf("    BEFORE last control_request (seq=%d)\n", lastControlRequestSeq)
			}
		}
		if firstDenySeq > 0 {
			if rlSeq < firstDenySeq {
				fmt.Printf("    BEFORE first deny response (seq=%d) *** USABLE AS BATCH SIGNAL ***\n", firstDenySeq)
			} else {
				fmt.Printf("    AFTER first deny response (seq=%d)\n", firstDenySeq)
			}
		}
	}
}

// writeMCPConfig creates a temp MCP config using the auxot-worker binary in MCP mode.
// The auxot-worker binary serves as an MCP stdio server when AUXOT_MCP_TOOLS_FILE is set.
func writeMCPConfig() (string, error) {
	// OpenAI-format tool definitions (what auxot-worker MCP mode expects)
	tools := []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "get_status",
				"description": "Get system status",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "get_version",
				"description": "Get system version",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
	}

	toolsData, _ := json.Marshal(tools)
	toolsFile, err := os.CreateTemp("", "probe-tools-*.json")
	if err != nil {
		return "", err
	}
	toolsFile.Write(toolsData)
	toolsFile.Close()

	// Use the auxot-worker binary in MCP mode
	workerBin, err := os.Executable()
	if err != nil {
		// Fall back to finding auxot-worker
		workerBin = "auxot-worker"
	}
	// If we're running as claude-probe, find auxot-worker next to us
	if strings.HasSuffix(workerBin, "claude-probe") {
		dir := workerBin[:len(workerBin)-len("claude-probe")]
		candidate := dir + "auxot-worker"
		if _, err := os.Stat(candidate); err == nil {
			workerBin = candidate
		}
	}

	// Try to build auxot-worker if needed
	if _, err := exec.LookPath(workerBin); err != nil {
		workerBin = "go"
	}

	config := map[string]any{
		"mcpServers": map[string]any{
			"auxot": map[string]any{
				"command": workerBin,
				"args":    []string{},
				"env": map[string]string{
					"AUXOT_MCP_TOOLS_FILE": toolsFile.Name(),
				},
			},
		},
	}

	// If workerBin is "go", run it via go run
	if workerBin == "go" {
		config = map[string]any{
			"mcpServers": map[string]any{
				"auxot": map[string]any{
					"command": "go",
					"args":    []string{"run", "./cmd/auxot-worker/"},
					"env": map[string]string{
						"AUXOT_MCP_TOOLS_FILE": toolsFile.Name(),
					},
				},
			},
		}
	}

	configData, _ := json.Marshal(config)
	configFile, err := os.CreateTemp("", "probe-mcp-config-*.json")
	if err != nil {
		os.Remove(toolsFile.Name())
		return "", err
	}
	configFile.Write(configData)
	configFile.Close()

	return configFile.Name(), nil
}
