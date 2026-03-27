package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// jsonRPCIDMatches reports whether raw is a JSON-RPC id equal to want.
// Servers may encode ids as numbers or strings; mismatches previously caused
// us to read stdout until timeout while the real response was ignored.
func jsonRPCIDMatches(raw json.RawMessage, want int) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int(f) == want
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return false
	}
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return err == nil && n == want
}

// normalizeMCPArguments ensures tools/call receives a JSON object. Raw `null`,
// empty input, or non-objects become `{}` so servers do not see "arguments": null.
func normalizeMCPArguments(args json.RawMessage) json.RawMessage {
	b := bytes.TrimSpace(args)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return json.RawMessage(`{}`)
	}
	// Require a JSON object at the top level.
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return json.RawMessage(`{}`)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return json.RawMessage(`{}`)
	}
	return args
}

// hintForMCPToolValidationError inspects common Zod-style JSON arrays returned
// inside JSON-RPC error messages and adds LLM-oriented context for Auxot's
// aggregate MCP tools (command + arguments).
func hintForMCPToolValidationError(message string) string {
	s := strings.TrimSpace(message)
	if len(s) < 2 || s[0] != '[' {
		return ""
	}
	var issues []struct {
		Path []string `json:"path"`
	}
	if err := json.Unmarshal([]byte(s), &issues); err != nil {
		return ""
	}
	for _, iss := range issues {
		for i := 0; i < len(iss.Path)-1; i++ {
			if iss.Path[i] == "params" && iss.Path[i+1] == "arguments" {
				return "Auxot maps this to your aggregate tool's \"arguments\" field: use a JSON object (not null). Example: {\"command\":\"get_file_contents\",\"arguments\":{\"owner\":\"YOUR_ORG\",\"repo\":\"YOUR_REPO\",\"path\":\"README.md\"}}."
			}
		}
	}
	return ""
}

// mcpJSONRPCToolCallError formats JSON-RPC errors from tools/call for operators
// and the LLM, with hints when the upstream message refers to MCP params.arguments.
func mcpJSONRPCToolCallError(toolName string, code int, message string) error {
	msg := strings.TrimSpace(message)
	hint := hintForMCPToolValidationError(msg)
	if hint != "" {
		return fmt.Errorf("MCP tool %q: server rejected the request (JSON-RPC %d): %s\n\n%s", toolName, code, msg, hint)
	}
	return fmt.Errorf("MCP tool %q: server rejected the request (JSON-RPC %d): %s", toolName, code, msg)
}

// McpInstaller manages per-package bun installation state.
// Each package is installed at most once; concurrent callers for the same
// package block on the single install goroutine until it completes.
type McpInstaller struct {
	mu     sync.Mutex
	states map[string]*installState // key: "package@version"
}

type installState struct {
	done chan struct{} // closed when install completes (success or failure)
	err  error
}

// NewMcpInstaller creates a new McpInstaller.
func NewMcpInstaller() *McpInstaller {
	return &McpInstaller{
		states: make(map[string]*installState),
	}
}

// EnsureInstalled installs the given bun package if not already installed.
// If an install is already in progress, it blocks until it completes (respecting ctx).
// Returns nil on success, or an error if the install failed or ctx expired.
func (m *McpInstaller) EnsureInstalled(ctx context.Context, pkg, version string) error {
	key := pkg + "@" + version

	m.mu.Lock()
	state, exists := m.states[key]
	if !exists {
		state = &installState{done: make(chan struct{})}
		m.states[key] = state
		go m.runInstall(state, pkg, version)
	}
	m.mu.Unlock()

	select {
	case <-state.done:
		return state.err
	case <-ctx.Done():
		return fmt.Errorf("waiting for MCP install %s@%s: %w", pkg, version, ctx.Err())
	}
}

// IsInstalled reports whether the package has been successfully installed.
func (m *McpInstaller) IsInstalled(pkg, version string) bool {
	key := pkg + "@" + version

	m.mu.Lock()
	state, exists := m.states[key]
	m.mu.Unlock()

	if !exists {
		return false
	}
	select {
	case <-state.done:
		return state.err == nil
	default:
		return false
	}
}

func (m *McpInstaller) runInstall(state *installState, pkg, version string) {
	defer close(state.done)

	pkgSpec := pkg + "@" + version
	slog.Info("installing MCP package", "package", pkgSpec, "bun_install", auxotBunDir())

	cmd := exec.Command(bunBinary(), "add", "--global", pkgSpec)
	cmd.Env = buildBunEnv(nil)
	out, err := cmd.CombinedOutput()
	if err != nil {
		state.err = fmt.Errorf("bun add --global %s: %w\noutput: %s", pkgSpec, err, string(out))
		slog.Error("MCP package install failed", "package", pkgSpec, "error", state.err)
		return
	}
	slog.Info("MCP package installed", "package", pkgSpec)
}

// bunBinary returns the path to the bun binary.
// It checks ~/.bun/bin/bun (installed by the official installer) first,
// then PATH, then falls back to the Docker-installed path.
func bunBinary() string {
	// Check ~/.bun/bin/bun first (standard bun install location outside PATH).
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".bun", "bin", "bun")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if path, err := exec.LookPath("bun"); err == nil {
		return path
	}
	return "/usr/local/bin/bun"
}

// auxotBunDir returns the directory where bun stores global packages and cache
// for MCP servers. Defaults to ~/.auxot/bun; can be overridden via BUN_INSTALL.
func auxotBunDir() string {
	if d := os.Getenv("BUN_INSTALL"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/auxot-bun"
	}
	return filepath.Join(home, ".auxot", "bun")
}

// buildBunEnv returns an environment slice for bun subprocesses that forces
// BUN_INSTALL to auxotBunDir() (keeping all other process env vars). The
// optional extra map is merged in last (highest priority), intended for
// per-job MCP credentials injected by the router.
func buildBunEnv(extra map[string]string) []string {
	bunDir := auxotBunDir()
	base := os.Environ()
	merged := make(map[string]string, len(base)+len(extra)+1)
	for _, kv := range base {
		k, v, _ := strings.Cut(kv, "=")
		merged[k] = v
	}
	// Force bun to store packages/cache under ~/.auxot/bun.
	merged["BUN_INSTALL"] = bunDir
	// Add bun's bin dir to PATH so installed packages are discoverable.
	if existing := merged["PATH"]; existing != "" {
		merged["PATH"] = filepath.Join(bunDir, "bin") + string(os.PathListSeparator) + existing
	}
	for k, v := range extra {
		if k != "" {
			merged[k] = v
		}
	}
	result := make([]string, 0, len(merged))
	for k, v := range merged {
		result = append(result, k+"="+v)
	}
	return result
}

// --- JSON-RPC types for the MCP stdio protocol ---

// jsonRPCRequest is a JSON-RPC 2.0 request or notification.
// Notifications omit the ID field (nil pointer).
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"` // nil for notifications
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonRPCResponse is a JSON-RPC 2.0 response (success or error).
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// McpToolDef is the schema for a single tool exposed by an MCP server.
// Populated by McpIntrospect via the tools/list JSON-RPC call.
type McpToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	ParamNames  []string        `json:"param_names,omitempty"` // top-level required+optional param keys
	InputSchema json.RawMessage `json:"input_schema,omitempty"` // full JSON Schema for validate_configuration
}

// McpIntrospect starts the MCP server, calls tools/list, and returns the tool definitions.
// The process is killed immediately after the list is obtained.
// Returns an empty slice (not an error) if the server starts but advertises no tools.
func McpIntrospect(ctx context.Context, pkg, version string) ([]McpToolDef, error) {
	pkgSpec := pkg + "@" + version

	// Give introspection a short deadline — this is a fast metadata call.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bunBinary(), "x", pkgSpec)
	cmd.Env = buildBunEnv(nil)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe for MCP introspect %s: %w", pkgSpec, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe for MCP introspect %s: %w", pkgSpec, err)
	}
	cmd.Stderr = nil // discard stderr during introspection

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting MCP server %s for introspect: %w", pkgSpec, err)
	}
	defer cmd.Process.Kill() //nolint:errcheck

	lines := make(chan []byte, 32)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 256*1024), 256*1024)
		for scanner.Scan() {
			b := make([]byte, len(scanner.Bytes()))
			copy(b, scanner.Bytes())
			lines <- b
		}
		close(lines)
	}()

	writeReq := func(req jsonRPCRequest) error {
		data, _ := json.Marshal(req)
		data = append(data, '\n')
		_, err := stdin.Write(data)
		return err
	}

	readResp := func(expectedID int) (jsonRPCResponse, error) {
		deadline := time.After(10 * time.Second)
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					return jsonRPCResponse{}, fmt.Errorf("MCP server closed stdout")
				}
				var resp jsonRPCResponse
				if err := json.Unmarshal(line, &resp); err != nil {
					continue // skip non-JSON lines (e.g. startup messages)
				}
				if !jsonRPCIDMatches(resp.ID, expectedID) {
					continue
				}
				if resp.Error != nil {
					return jsonRPCResponse{}, fmt.Errorf("MCP JSON-RPC error %d: %s", resp.Error.Code, resp.Error.Message)
				}
				return resp, nil
			case <-deadline:
				return jsonRPCResponse{}, fmt.Errorf("timeout waiting for response id=%d", expectedID)
			case <-ctx.Done():
				return jsonRPCResponse{}, ctx.Err()
			}
		}
	}

	// Step 1: initialize.
	id1 := 1
	_ = writeReq(jsonRPCRequest{JSONRPC: "2.0", ID: &id1, Method: "initialize",
		Params: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "auxot-tools", "version": "1.0"},
		}})
	if _, err := readResp(1); err != nil {
		return nil, fmt.Errorf("MCP introspect initialize: %w", err)
	}

	// Step 2: initialized notification.
	_ = writeReq(jsonRPCRequest{JSONRPC: "2.0", Method: "notifications/initialized"})

	// Step 3: list tools.
	id2 := 2
	_ = writeReq(jsonRPCRequest{JSONRPC: "2.0", ID: &id2, Method: "tools/list"})
	resp, err := readResp(2)
	if err != nil {
		return nil, fmt.Errorf("MCP introspect tools/list: %w", err)
	}

	// Parse tools/list result: {"tools": [{name, description, inputSchema}]}
	var result struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("parsing tools/list result: %w", err)
	}

	defs := make([]McpToolDef, 0, len(result.Tools))
	for _, t := range result.Tools {
		// Collect param names from inputSchema for backward compat.
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		_ = json.Unmarshal(t.InputSchema, &schema)
		seen := make(map[string]bool)
		params := append([]string{}, schema.Required...)
		for _, p := range params {
			seen[p] = true
		}
		for k := range schema.Properties {
			if !seen[k] {
				params = append(params, k)
			}
		}
		defs = append(defs, McpToolDef{
			Name:        t.Name,
			Description: t.Description,
			ParamNames:  params,
			InputSchema: t.InputSchema,
		})
	}
	return defs, nil
}

// McpPackageSlug derives a clean tool name from an npm package name.
// Examples:
//
//	@modelcontextprotocol/server-github → github
//	@company/mcp-weather               → weather
//	my-custom-server                   → my_custom_server
func McpPackageSlug(pkg string) string {
	// Strip @scope/ prefix.
	if idx := strings.LastIndex(pkg, "/"); idx >= 0 {
		pkg = pkg[idx+1:]
	}
	// Strip leading "server-" or "mcp-" (common MCP package naming conventions).
	for _, prefix := range []string{"server-", "mcp-"} {
		if strings.HasPrefix(pkg, prefix) {
			pkg = pkg[len(prefix):]
			break
		}
	}
	// Replace hyphens with underscores for a valid identifier.
	return strings.ReplaceAll(pkg, "-", "_")
}

// McpExecute executes a single MCP tool call by spawning a fresh "bun x" subprocess.
// The process is killed after the tool call completes.
//
// Protocol (MCP stdio, newline-delimited JSON-RPC):
//  1. Send initialize request, wait for response.
//  2. Send notifications/initialized notification.
//  3. Send tools/call request, wait for response.
//  4. Kill process.
func McpExecute(ctx context.Context, pkg, version, toolName string, args json.RawMessage, credentials map[string]string) (Result, error) {
	pkgSpec := pkg + "@" + version

	// Log credential env vars: name, length, and first 3 characters for verification.
	credLog := make([]any, 0, len(credentials)*2)
	for k, v := range credentials {
		preview := v
		if len(v) > 3 {
			preview = v[:3] + fmt.Sprintf("...{%d}", len(v))
		}
		credLog = append(credLog, k, preview)
	}
	slog.Info("MCP execute",
		"package", pkgSpec,
		"tool", toolName,
		"args", string(args),
		"credential_count", len(credentials),
		"credentials", credLog,
	)

	cmd := exec.CommandContext(ctx, bunBinary(), "x", pkgSpec)
	cmd.Env = buildBunEnv(credentials)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, fmt.Errorf("creating stdin pipe for MCP %s: %w", pkgSpec, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("creating stdout pipe for MCP %s: %w", pkgSpec, err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("starting MCP server %s: %w", pkgSpec, err)
	}
	defer cmd.Process.Kill() //nolint:errcheck

	// Read stdout lines asynchronously so we never block on write.
	// Use a generous scanner buffer since MCP responses can be large.
	lines := make(chan []byte, 64)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			b := make([]byte, len(scanner.Bytes()))
			copy(b, scanner.Bytes())
			lines <- b
		}
		close(lines)
	}()

	writeReq := func(req jsonRPCRequest) error {
		data, err := json.Marshal(req)
		if err != nil {
			return fmt.Errorf("marshaling JSON-RPC request: %w", err)
		}
		data = append(data, '\n')
		if _, err := stdin.Write(data); err != nil {
			return fmt.Errorf("writing to MCP stdin: %w", err)
		}
		return nil
	}

	// readResp reads stdout lines until it finds the response matching targetID.
	// It discards notifications (which have no id) and responses for other ids.
	readResp := func(targetID int) (*jsonRPCResponse, error) {
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					return nil, fmt.Errorf("MCP server %s stdout closed unexpectedly (stderr: %s)", pkgSpec, stderrBuf.String())
				}
				var resp jsonRPCResponse
				if err := json.Unmarshal(line, &resp); err != nil {
					// Could be a log line or a notification without id; skip.
					continue
				}
				if !jsonRPCIDMatches(resp.ID, targetID) {
					continue
				}
				if resp.Error != nil {
					if targetID == 2 {
						return nil, mcpJSONRPCToolCallError(toolName, resp.Error.Code, resp.Error.Message)
					}
					return nil, fmt.Errorf("MCP JSON-RPC error %d: %s", resp.Error.Code, resp.Error.Message)
				}
				return &resp, nil
			case <-ctx.Done():
				return nil, fmt.Errorf("MCP %s timed out: %w", pkgSpec, ctx.Err())
			}
		}
	}

	args = normalizeMCPArguments(args)

	// Step 1: initialize.
	id1 := 1
	if err := writeReq(jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      &id1,
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "auxot-tools",
				"version": "1.0",
			},
		},
	}); err != nil {
		return Result{}, fmt.Errorf("MCP initialize request: %w", err)
	}
	if _, err := readResp(1); err != nil {
		return Result{}, fmt.Errorf("MCP initialize response: %w", err)
	}

	// Step 2: send initialized notification (no id = notification, not a request).
	if err := writeReq(jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}); err != nil {
		return Result{}, fmt.Errorf("MCP initialized notification: %w", err)
	}

	// Step 3: call the tool.
	id2 := 2
	if err := writeReq(jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      &id2,
		Method:  "tools/call",
		Params: map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	}); err != nil {
		return Result{}, fmt.Errorf("MCP tools/call request: %w", err)
	}

	// Close stdin to signal EOF — some MCP servers need this to flush output.
	_ = stdin.Close()

	resp, err := readResp(2)
	if err != nil {
		return Result{}, fmt.Errorf("MCP tools/call response: %w", err)
	}
	if resp.Result == nil {
		return Result{Output: json.RawMessage(`null`)}, nil
	}
	return interpretMcpToolsCallResult(toolName, resp.Result)
}

// interpretMcpToolsCallResult maps MCP tools/call "result" into a tools.Result.
// When the server sets isError, we return a Go error so the worker sends a
// tool_result with Error set instead of silent "success" with an error payload.
func interpretMcpToolsCallResult(toolName string, raw json.RawMessage) (Result, error) {
	var envelope struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Result{Output: raw}, nil
	}
	if !envelope.IsError {
		return Result{Output: raw}, nil
	}
	var parts []string
	for _, c := range envelope.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	msg := strings.Join(parts, "\n")
	if msg == "" {
		msg = "MCP tool returned isError with no text content"
	}
	return Result{}, fmt.Errorf("MCP tool %q: %s", toolName, msg)
}
