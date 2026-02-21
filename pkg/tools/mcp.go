package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
)

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
	slog.Info("installing MCP package", "package", pkgSpec)

	cmd := exec.Command(bunBinary(), "add", "--global", pkgSpec)
	out, err := cmd.CombinedOutput()
	if err != nil {
		state.err = fmt.Errorf("bun add --global %s: %w\noutput: %s", pkgSpec, err, string(out))
		slog.Error("MCP package install failed", "package", pkgSpec, "error", state.err)
		return
	}
	slog.Info("MCP package installed", "package", pkgSpec)
}

// bunBinary returns the path to the bun binary.
// It prefers the PATH-resolved location and falls back to the Docker-installed path.
func bunBinary() string {
	if path, err := exec.LookPath("bun"); err == nil {
		return path
	}
	return "/usr/local/bin/bun"
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
	ID      *int            `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
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

	cmd := exec.CommandContext(ctx, bunBinary(), "x", pkgSpec)
	cmd.Env = BuildToolEnv(credentials)

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
				if resp.ID != nil && *resp.ID == targetID {
					return &resp, nil
				}
			case <-ctx.Done():
				return nil, fmt.Errorf("MCP %s timed out: %w", pkgSpec, ctx.Err())
			}
		}
	}

	// Ensure args is a valid JSON object (fall back to empty object).
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}

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
	if resp.Error != nil {
		return Result{}, fmt.Errorf("MCP tool %q error %d: %s", toolName, resp.Error.Code, resp.Error.Message)
	}
	if resp.Result == nil {
		return Result{Output: json.RawMessage(`null`)}, nil
	}
	return Result{Output: resp.Result}, nil
}
