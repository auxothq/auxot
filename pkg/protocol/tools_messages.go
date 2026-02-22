package protocol

import (
	"encoding/json"
	"fmt"
)

// WorkerType distinguishes GPU workers from tools workers on the WebSocket endpoint.
// An empty or missing worker_type is treated as "gpu" for backward compatibility.
type WorkerType string

const (
	WorkerTypeGPU   WorkerType = "gpu"
	WorkerTypeTools WorkerType = "tools"
)

// Tools-specific message types.
const (
	// Server → Tools worker
	TypeToolJob      MessageType = "tool_job"
	TypeReloadPolicy MessageType = "reload_policy"

	// Tools worker → Server
	TypeToolResult     MessageType = "tool_result"
	TypePolicyReloaded MessageType = "policy_reloaded"
)

// ToolsCapabilities describes what a tools worker can execute.
// Sent in HelloMessage.ToolsCapabilities when WorkerType == "tools".
type ToolsCapabilities struct {
	// Tools is the list of tool names this worker can handle.
	// Examples: "code_executor", "web_fetch", "web_search", "shell:weather"
	Tools []string `json:"tools"`
}

// McpServerConfig describes a bun-compatible MCP server to run per-request.
// Every tool call spawns a fresh "bun x @package@version" process.
type McpServerConfig struct {
	Package string `json:"package"` // e.g. "@modelcontextprotocol/server-github"
	Version string `json:"version"` // e.g. "1.2.3" or "latest"
}

// ToolsPolicy is sent in hello_ack and reload_policy.
// It tells the worker what it is permitted to run.
type ToolsPolicy struct {
	AllowedTools []string          `json:"allowed_tools"` // built-in tool names; empty = none
	McpServers   []McpServerConfig `json:"mcp_servers"`
}

// ReloadPolicyMessage is sent by the server to update the worker's allowed tools and MCP servers.
type ReloadPolicyMessage struct {
	Type   MessageType `json:"type"`
	Policy ToolsPolicy `json:"policy"`
}

// McpAggregateSchema describes a single MCP server exposed to the LLM as one
// aggregate tool (e.g. tool name "github" for @modelcontextprotocol/server-github).
// The LLM invokes it with {command: "create_issue", arguments: {...}}.
type McpAggregateSchema struct {
	// ToolName is the LLM-visible aggregate tool name (slug derived from package).
	// Example: "github", "filesystem", "my_custom_server"
	ToolName string `json:"tool_name"`
	// Package is the npm package identifier.
	Package string `json:"package"`
	// Version is the npm package version.
	Version string `json:"version"`
	// Description is a compact listing of available commands and their parameter
	// names, suitable for embedding directly in the LLM tool description field.
	Description string `json:"description"`
	// Commands lists the MCP tool names this server exposes.
	Commands []string `json:"commands"`
}

// PolicyReloadedMessage is sent by the worker after applying a new policy.
// AdvertisedTools lists every tool identifier the worker now handles, including
// built-ins (e.g. "web_fetch") and MCP aggregate slugs (e.g. "github").
// McpSchemas carries the introspected schema for each MCP server, keyed by slug.
type PolicyReloadedMessage struct {
	Type            MessageType          `json:"type"`
	AdvertisedTools []string             `json:"advertised_tools"`
	McpSchemas      []McpAggregateSchema `json:"mcp_schemas,omitempty"`
}

// ToolJobMessage is sent by the server to assign a tool call to a tools worker.
// The router sends one ToolJobMessage per tool call in the LLM's response.
type ToolJobMessage struct {
	Type        MessageType       `json:"type"`
	JobID       string            `json:"job_id"`                // Unique ID for this tool invocation
	ParentJobID string            `json:"parent_job_id"`         // The LLM job that triggered this
	ToolName    string            `json:"tool_name"`             // e.g. "code_executor", "web_fetch"
	ToolCallID  string            `json:"tool_call_id"`          // The LLM's tool_call.id — echoed in result
	Arguments   json.RawMessage   `json:"arguments"`             // JSON object matching the tool's schema
	Credentials map[string]string `json:"credentials,omitempty"` // Env-var style credential envelope (e.g. GITHUB_API_KEY=xxx)

	// MCP-specific fields (zero value for built-in tools).
	McpPackage string `json:"mcp_package,omitempty"` // e.g. "@modelcontextprotocol/server-github"
	McpVersion string `json:"mcp_version,omitempty"` // e.g. "1.2.3"
}

// ToolResultMessage is sent by a tools worker after completing (or failing) a tool call.
type ToolResultMessage struct {
	Type        MessageType     `json:"type"`
	JobID       string          `json:"job_id"`        // Mirrors ToolJobMessage.JobID
	ParentJobID string          `json:"parent_job_id"` // Mirrors ToolJobMessage.ParentJobID
	ToolCallID  string          `json:"tool_call_id"`  // Mirrors ToolJobMessage.ToolCallID
	ToolName    string          `json:"tool_name"`     // Mirrors ToolJobMessage.ToolName
	Result      json.RawMessage `json:"result,omitempty"`
	Error       string          `json:"error,omitempty"`
	DurationMS  int64           `json:"duration_ms,omitempty"`
}

// ParseToolsMessage reads a raw WebSocket message sent from a tools worker.
// This is the complement to ParseMessage — it handles the tools-worker-specific
// message types that ParseMessage doesn't know about.
func ParseToolsMessage(data []byte) (any, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("parsing tools message envelope: %w", err)
	}

	switch env.Type {
	case TypeToolResult:
		var msg ToolResultMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing tool_result message: %w", err)
		}
		return msg, nil

	case TypePolicyReloaded:
		var msg PolicyReloadedMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing policy_reloaded message: %w", err)
		}
		return msg, nil

	// Tools workers also send heartbeats and hello — those are handled by ParseMessage.
	default:
		return ParseMessage(data)
	}
}
