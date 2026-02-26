package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
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

	// runCtx is the context passed to Run; tool job goroutines inherit it so
	// they are cancelled when the process receives SIGTERM/SIGINT.
	runCtx context.Context
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
	conn.OnValidateConfiguration(w.handleValidateConfiguration)
	return w
}

// Run connects to the router and processes tool jobs until the context is cancelled.
// On disconnection it retries immediately, then uses exponential backoff on repeated failures.
// Backoff resets to zero when a connection succeeds (clean disconnect).
func (w *Worker) Run(ctx context.Context) error {
	w.runCtx = ctx
	var delay time.Duration // 0 = immediate retry

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := w.conn.Connect(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// Context cancelled — this is expected on shutdown, not an error.
				return ctx.Err()
			}
			w.logger.Error("router connection failed", "error", err, "retry_in_sec", int64(delay.Seconds()))
		} else {
			w.logger.Info("disconnected from router, retrying", "retry_in_sec", int64(delay.Seconds()))
			// Reset backoff on clean disconnects (server restart, etc.) — immediate retry.
			delay = 0
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}

		// Exponential backoff for failures; stays 0 after successful connect.
		if delay == 0 {
			delay = w.cfg.ReconnectDelay
		} else {
			delay *= 2
			if delay > w.cfg.ReconnectMaxDelay {
				delay = w.cfg.ReconnectMaxDelay
			}
		}
	}
}

// handleValidateConfiguration is called when the router sends a
// validate_configuration message (e.g. when an admin adds an MCP server).
// It instantiates the MCP server without env vars, calls tools/list, and
// returns the tool definitions.
func (w *Worker) handleValidateConfiguration(req protocol.ValidateConfigurationMessage) {
	version := req.Version
	if version == "" {
		version = "latest"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	defs, err := tools.McpIntrospect(ctx, req.Package, version)
	if err != nil {
		w.logger.Warn("validate_configuration failed",
			"request_id", req.RequestID,
			"package", req.Package,
			"error", err,
		)
		_ = w.conn.SendValidateConfigurationResult(protocol.ValidateConfigurationResultMessage{
			Type:      protocol.TypeValidateConfigurationResult,
			RequestID: req.RequestID,
			Error:     err.Error(),
		})
		return
	}

	toolList := make([]protocol.ValidateConfigurationTool, 0, len(defs))
	for _, d := range defs {
		toolList = append(toolList, protocol.ValidateConfigurationTool{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: d.InputSchema,
		})
	}

	w.logger.Info("validate_configuration success",
		"request_id", req.RequestID,
		"package", req.Package,
		"tools", len(toolList),
	)

	if err := w.conn.SendValidateConfigurationResult(protocol.ValidateConfigurationResultMessage{
		Type:      protocol.TypeValidateConfigurationResult,
		RequestID: req.RequestID,
		Tools:     toolList,
	}); err != nil {
		w.logger.Error("sending validate_configuration_result", "error", err)
	}
}

// handleReloadPolicy is called (in a goroutine) when the router sends a
// reload_policy message or when the initial policy arrives in hello_ack.
// It installs MCP packages, introspects their tool lists, and notifies the server.
func (w *Worker) handleReloadPolicy(policy protocol.ToolsPolicy) {
	w.policyMu.Lock()
	w.policy = policy
	w.policyMu.Unlock()

	w.logger.Info("applying policy",
		"allowed_tools", policy.AllowedTools,
		"mcp_servers", len(policy.McpServers),
	)

	// Install all MCP packages in parallel, then introspect each one.
	type mcpResult struct {
		srv    protocol.McpServerConfig
		schema protocol.McpAggregateSchema
	}
	results := make([]mcpResult, len(policy.McpServers))

	var wg sync.WaitGroup
	for i, srv := range policy.McpServers {
		i, srv := i, srv
		wg.Add(1)
		go func() {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			if err := w.installer.EnsureInstalled(ctx, srv.Package, srv.Version); err != nil {
				w.logger.Error("MCP package install failed",
					"package", srv.Package, "version", srv.Version, "error", err)
				return
			}

			slug := tools.McpPackageSlug(srv.Package)

			// Introspect the MCP server to get its available tools.
			defs, err := tools.McpIntrospect(ctx, srv.Package, srv.Version)
			if err != nil {
				w.logger.Warn("MCP introspect failed — advertising tool without schema",
					"package", srv.Package, "error", err)
				results[i] = mcpResult{srv: srv, schema: protocol.McpAggregateSchema{
					ToolName:    slug,
					Package:     srv.Package,
					Version:     srv.Version,
					Description: "MCP server " + srv.Package + " (schema unavailable)",
				}}
				return
			}

			// Build a compact description listing all commands + parameter names.
			desc := buildMcpDescription(srv.Package, defs)
			commands := make([]string, len(defs))
			for j, d := range defs {
				commands[j] = d.Name
			}

			results[i] = mcpResult{srv: srv, schema: protocol.McpAggregateSchema{
				ToolName:    slug,
				Package:     srv.Package,
				Version:     srv.Version,
				Description: desc,
				Commands:    commands,
			}}

			w.logger.Info("MCP server ready",
				"tool_name", slug,
				"package", srv.Package,
				"commands", len(defs),
			)
		}()
	}
	wg.Wait()

	// Build the updated advertised tools list and MCP aggregate schemas.
	advertised := w.registry.Names()
	var mcpSchemas []protocol.McpAggregateSchema
	for _, r := range results {
		if r.schema.ToolName == "" {
			continue // install failed
		}
		advertised = append(advertised, r.schema.ToolName)
		mcpSchemas = append(mcpSchemas, r.schema)
	}

	// Persist the updated list so future reconnects advertise it.
	w.conn.UpdateAdvertisedTools(advertised)

	// Inform the server of the new capabilities.
	if err := w.conn.SendPolicyReloaded(advertised, mcpSchemas); err != nil {
		w.logger.Error("sending policy_reloaded", "error", err)
	}
}

// buildMcpDescription constructs a compact human-readable description of an MCP
// server's capabilities for embedding in the aggregate LLM tool description.
func buildMcpDescription(pkg string, defs []tools.McpToolDef) string {
	if len(defs) == 0 {
		return "MCP server " + pkg + " (no tools advertised)"
	}
	var sb strings.Builder
	sb.WriteString("MCP integration via ")
	sb.WriteString(pkg)
	sb.WriteString(".\n\nAvailable commands:\n")
	for _, d := range defs {
		sb.WriteString("  - ")
		sb.WriteString(d.Name)
		if len(d.ParamNames) > 0 {
			sb.WriteString("(")
			for i, p := range d.ParamNames {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString(p)
			}
			sb.WriteString(")")
		}
		if d.Description != "" {
			// Truncate long descriptions to keep the schema compact.
			desc := d.Description
			if len(desc) > 80 {
				desc = desc[:77] + "..."
			}
			sb.WriteString(": ")
			sb.WriteString(desc)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\nCall this tool with {\"command\": \"<command_name>\", \"params\": {\"<param>\": \"<value>\", ...}}.\nExample: {\"command\": \"create_issue\", \"params\": {\"owner\": \"acme\", \"repo\": \"api\", \"title\": \"Bug\"}}")
	return sb.String()
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
	// Inherit from runCtx so tool calls are cancelled on SIGTERM/SIGINT.
	// Individual tools apply their own tighter deadlines based on their args.
	baseCtx := w.runCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(baseCtx, 5*time.Minute)
	defer cancel()

	// Inject job credentials so tools can access them via tools.Credential(ctx, key).
	// This allows per-job runtime override of operator-level env vars.
	if len(job.Credentials) > 0 {
		ctx = tools.WithCredentials(ctx, job.Credentials)
	}

	// Log received credentials: name + length + 3-char prefix for verification.
	if len(job.Credentials) > 0 {
		credParts := make([]any, 0, len(job.Credentials)*2)
		for k, v := range job.Credentials {
			preview := v
			if len(v) > 3 {
				preview = v[:3] + fmt.Sprintf("...{%d}", len(v))
			}
			credParts = append(credParts, k, preview)
		}
		logger.Info("job credentials received", "credentials", credParts)
	} else {
		logger.Warn("job has no credentials")
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
