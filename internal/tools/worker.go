package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/auxothq/auxot/pkg/protocol"
	"github.com/auxothq/auxot/pkg/tools"
)

// Worker is the top-level tools worker.
// It maintains a persistent WebSocket connection to the router,
// dispatches tool jobs to the registry, and returns results.
type Worker struct {
	cfg       *Config
	registry  *tools.Registry
	conn      *Connection
	installer *tools.McpInstaller
	logger    *slog.Logger

	policyMu sync.Mutex
	policy   protocol.ToolsPolicy
}

// NewWorker creates a Worker with the given config and tool registry.
func NewWorker(cfg *Config, registry *tools.Registry, logger *slog.Logger) *Worker {
	toolNames := registry.Names()
	conn := NewConnection(cfg, toolNames, logger)

	w := &Worker{
		cfg:       cfg,
		registry:  registry,
		conn:      conn,
		installer: tools.NewMcpInstaller(),
		logger:    logger,
	}

	conn.OnToolJob(w.handleToolJob)
	conn.OnReloadPolicy(w.handleReloadPolicy)
	return w
}

// Run connects to the router and processes tool jobs until the context is cancelled.
// On disconnection it retries with exponential backoff.
func (w *Worker) Run(ctx context.Context) error {
	delay := w.cfg.ReconnectDelay

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := w.conn.Connect()
		if err != nil {
			w.logger.Error("router connection failed", "error", err, "retry_in", delay)
		} else {
			w.logger.Info("disconnected from router, retrying", "delay", delay)
			// Reset backoff on clean disconnects (server restart, etc.)
			delay = w.cfg.ReconnectDelay
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}

		// Exponential backoff, capped at max delay.
		delay *= 2
		if delay > w.cfg.ReconnectMaxDelay {
			delay = w.cfg.ReconnectMaxDelay
		}
	}
}

// handleReloadPolicy is called (in a goroutine) when the router sends a
// reload_policy message or when the initial policy arrives in hello_ack.
// It installs any new MCP packages in parallel, then notifies the server.
func (w *Worker) handleReloadPolicy(policy protocol.ToolsPolicy) {
	w.policyMu.Lock()
	w.policy = policy
	w.policyMu.Unlock()

	w.logger.Info("applying policy",
		"allowed_tools", policy.AllowedTools,
		"mcp_servers", len(policy.McpServers),
	)

	// Install all MCP packages in parallel.
	var wg sync.WaitGroup
	for _, srv := range policy.McpServers {
		srv := srv // capture loop variable
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := w.installer.EnsureInstalled(ctx, srv.Package, srv.Version); err != nil {
				w.logger.Error("MCP package install failed",
					"package", srv.Package,
					"version", srv.Version,
					"error", err,
				)
			}
		}()
	}
	wg.Wait()

	// Build the updated advertised tools list:
	// built-in registry names + "mcp:@pkg@version" identifiers for MCP servers.
	advertised := w.registry.Names()
	for _, srv := range policy.McpServers {
		advertised = append(advertised, "mcp:"+srv.Package+"@"+srv.Version)
	}

	// Persist the updated list so future reconnects advertise it.
	w.conn.UpdateAdvertisedTools(advertised)

	// Inform the server of the new capabilities.
	if err := w.conn.SendPolicyReloaded(advertised); err != nil {
		w.logger.Error("sending policy_reloaded", "error", err)
	}
}

// handleToolJob is called by Connection when a ToolJobMessage arrives.
// It runs in its own goroutine (spawned by Connection.messageLoop).
func (w *Worker) handleToolJob(job protocol.ToolJobMessage) {
	logger := w.logger.With(
		"job_id", job.JobID,
		"parent_job_id", job.ParentJobID,
		"tool", job.ToolName,
		"tool_call_id", job.ToolCallID,
	)
	logger.Info("executing tool")

	start := time.Now()

	// Give each tool call its own context with a generous timeout.
	// Individual tools apply their own tighter deadlines based on their args.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Inject job credentials so tools can access them via tools.Credential(ctx, key).
	// This allows per-job runtime override of operator-level env vars.
	if len(job.Credentials) > 0 {
		ctx = tools.WithCredentials(ctx, job.Credentials)
	}

	var (
		result tools.Result
		err    error
	)

	if job.McpPackage != "" {
		// MCP path: ensure the package is installed (warm cache), then invoke it.
		logger.Info("routing to MCP executor", "mcp_package", job.McpPackage, "mcp_version", job.McpVersion)
		if installErr := w.installer.EnsureInstalled(ctx, job.McpPackage, job.McpVersion); installErr != nil {
			err = fmt.Errorf("MCP package install for %s@%s: %w", job.McpPackage, job.McpVersion, installErr)
		} else {
			result, err = tools.McpExecute(ctx, job.McpPackage, job.McpVersion, job.ToolName, job.Arguments, job.Credentials)
		}
	} else {
		// Built-in path.
		result, err = w.registry.Execute(ctx, job.ToolName, job.Arguments)
	}

	durationMS := time.Since(start).Milliseconds()

	var msg protocol.ToolResultMessage
	if err != nil {
		logger.Error("tool execution failed", "error", err, "duration_ms", durationMS)
		msg = protocol.ToolResultMessage{
			Type:        protocol.TypeToolResult,
			JobID:       job.JobID,
			ParentJobID: job.ParentJobID,
			ToolCallID:  job.ToolCallID,
			ToolName:    job.ToolName,
			Error:       err.Error(),
			DurationMS:  durationMS,
		}
	} else {
		logger.Info("tool execution complete", "duration_ms", durationMS)
		msg = protocol.ToolResultMessage{
			Type:        protocol.TypeToolResult,
			JobID:       job.JobID,
			ParentJobID: job.ParentJobID,
			ToolCallID:  job.ToolCallID,
			ToolName:    job.ToolName,
			Result:      result.Output,
			DurationMS:  durationMS,
		}
	}

	if err := w.conn.SendResult(msg); err != nil {
		logger.Error("sending tool result", "error", err)
	}
}

// credentialEnvVars returns the AUXOT_TOOL_ env vars for a given tool/server name,
// stripped of the AUXOT_TOOL_{SERVER}_ prefix. Used for MCP credential injection (future).
//
// Format: AUXOT_TOOL_{SERVER}_{ENV_VAR_NAME}=value
// Example: AUXOT_TOOL_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN=ghp_xxx
//
//	→ server "github", env "GITHUB_PERSONAL_ACCESS_TOKEN=ghp_xxx"
func credentialEnvVars(serverID string) []string {
	prefix := "AUXOT_TOOL_" + uppercaseID(serverID) + "_"
	var vars []string

	// We can't import os here without introducing a dependency on the runtime env
	// at package load time — instead, callers pass in the env. This is a stub
	// for the MCP credential path that will be wired up in Phase 1c.
	_ = prefix
	return vars
}

// uppercaseID converts a tool/server ID like "brave-search" → "BRAVE_SEARCH".
func uppercaseID(id string) string {
	result := make([]byte, len(id))
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c == '-' {
			result[i] = '_'
		} else if c >= 'a' && c <= 'z' {
			result[i] = c - 32
		} else {
			result[i] = c
		}
	}
	return string(result)
}

// toolResultContent converts a tool result JSON value to the string form
// the LLM expects as a "tool" role message content.
func toolResultContent(result json.RawMessage) string {
	if result == nil {
		return ""
	}
	// If it's already a JSON string, unwrap it.
	var s string
	if err := json.Unmarshal(result, &s); err == nil {
		return s
	}
	// Otherwise pretty-print the JSON object.
	return string(result)
}
