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
	TypeToolJob              MessageType = "tool_job"
	TypeReloadPolicy         MessageType = "reload_policy"
	TypeValidateConfiguration MessageType = "validate_configuration"

	// Tools worker → Server
	TypeToolResult               MessageType = "tool_result"
	TypePolicyReloaded           MessageType = "policy_reloaded"
	TypeValidateConfigurationResult MessageType = "validate_configuration_result"
)

// ToolsCapabilities describes what a tools worker can execute.
// Sent in HelloMessage.ToolsCapabilities when WorkerType == "tools".
type ToolsCapabilities struct {
	// Tools is the list of tool names this worker can handle.
	// Examples: "code_executor", "web_fetch", "web_search", "shell:weather"
	Tools []string `json:"tools"`
}

// McpHttpHeader describes how one HTTP header is built for an HTTP MCP server.
// It mirrors McpEnvVar semantics but targets an HTTP header instead of a
// subprocess environment variable. The resolved value is:
//
//	<value_prefix><resolved_credential_value>
//
// Sources: "oauth" uses ResolveAccessToken via OauthProvider+CredentialID;
// "org_secret" / "user_secret" use SecretName; "static" uses StaticValue.
type McpHttpHeader struct {
	HeaderName    string `json:"header_name"`              // e.g. "Authorization", "X-Api-Key"
	ValuePrefix   string `json:"value_prefix,omitempty"`   // e.g. "Bearer "
	Source        string `json:"source"`                   // "oauth" | "org_secret" | "user_secret" | "static"
	OauthProvider string `json:"oauth_provider,omitempty"` // provider slug for source=="oauth"
	CredentialID  string `json:"credential_id,omitempty"`  // connection id for source=="oauth"
	SecretName    string `json:"secret_name,omitempty"`    // for source=="org_secret"|"user_secret"
	StaticValue   string `json:"static_value,omitempty"`   // for source=="static"
}

// McpServerConfig describes an MCP server the worker can connect to.
//
// Stdio mode (legacy): Package and Version are set; URL is empty.
// Every tool call spawns a fresh "bun x @package@version" process.
//
// HTTP mode: URL is non-empty. The worker uses Streamable HTTP (POST JSON-RPC)
// to reach the remote server. No bun install is required.
// Headers carries the auth header binding descriptors (persisted in DB).
// ResolvedHeaders carries pre-resolved header values sent on the wire from
// server to worker only — it is never persisted.
type McpServerConfig struct {
	// ID is an optional stable identifier for the server (used for aggregate
	// tool naming when the package slug is ambiguous or absent for HTTP servers).
	ID      string `json:"id,omitempty"`
	Package string `json:"package,omitempty"` // e.g. "@modelcontextprotocol/server-github"
	Version string `json:"version,omitempty"` // e.g. "1.2.3" or "latest"

	// HTTP mode fields.
	URL       string          `json:"url,omitempty"`       // e.g. "https://api.githubcopilot.com/mcp/"
	Transport string          `json:"transport,omitempty"` // "streamable-http" (default for HTTP) or "sse"
	Headers   []McpHttpHeader `json:"headers,omitempty"`   // auth header bindings (persisted)

	// ResolvedHeaders is populated by the server at reload_policy send time
	// with the actual header name→value pairs derived from Headers bindings.
	// It is intentionally omitted from persistence (DB stores Headers only).
	// Workers use ResolvedHeaders for HTTP introspect; it has the same
	// sensitivity level as Credentials already carried on ToolJobMessage.
	ResolvedHeaders map[string]string `json:"resolved_headers,omitempty"`
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
//
// Credential contract for HTTP MCP jobs:
//   - Credentials (map[string]string) continues to carry env-var-style values
//     for stdio MCP (e.g. GITHUB_TOKEN=ghp_xxx).
//   - McpHeaders (map[string]string) carries pre-resolved HTTP header values
//     for HTTP MCP jobs (e.g. {"Authorization": "Bearer ghp_xxx"}).
//     The server fills McpHeaders by resolving the McpHttpHeader bindings from
//     the tool key; the worker injects them verbatim into every HTTP request.
//     When McpURL is non-empty, the worker routes via McpHttpExecute, not stdio.
type ToolJobMessage struct {
	Type        MessageType       `json:"type"`
	JobID       string            `json:"job_id"`                // Unique ID for this tool invocation
	ParentJobID string            `json:"parent_job_id"`         // The LLM job that triggered this
	// ThreadID is the conversation thread (reference_id from the GPU job). Used
	// to key browser contexts and for sticky worker routing. Omitted for
	// non-chat or legacy job types that do not carry a reference_id.
	ThreadID    string            `json:"thread_id,omitempty"`
	ToolName    string            `json:"tool_name"`             // e.g. "code_executor", "web_fetch"
	ToolCallID  string            `json:"tool_call_id"`          // The LLM's tool_call.id — echoed in result
	Arguments   json.RawMessage   `json:"arguments"`             // JSON object matching the tool's schema
	Credentials map[string]string `json:"credentials,omitempty"` // Env-var style credential envelope (e.g. GITHUB_API_KEY=xxx)

	// Stdio MCP-specific fields (zero value for built-in tools and HTTP MCP).
	McpPackage string `json:"mcp_package,omitempty"` // e.g. "@modelcontextprotocol/server-github"
	McpVersion string `json:"mcp_version,omitempty"` // e.g. "1.2.3"

	// HTTP MCP-specific fields. McpURL non-empty signals HTTP mode.
	McpURL       string            `json:"mcp_url,omitempty"`       // e.g. "https://api.githubcopilot.com/mcp/"
	McpTransport string            `json:"mcp_transport,omitempty"` // "streamable-http" (default) or "sse"
	McpHeaders   map[string]string `json:"mcp_headers,omitempty"`   // pre-resolved header name→value pairs
}

// ValidateConfigurationMessage is sent by the server when an admin wants to
// verify an MCP server configuration (e.g. when adding a new MCP server).
// The worker must respond with ValidateConfigurationResultMessage.
//
// Stdio mode: Package and Version are set; URL is empty.
// HTTP mode: URL is non-empty. ResolvedHeaders carries pre-resolved header
// values the worker must inject on every Streamable HTTP request. The worker
// does NOT resolve OAuth itself — the server pre-resolves and sends the values.
type ValidateConfigurationMessage struct {
	Type      MessageType `json:"type"`
	RequestID string      `json:"request_id"`
	Package   string      `json:"package,omitempty"` // pkg in Go; JSON key "package"
	Version   string      `json:"version,omitempty"`

	// HTTP mode fields.
	URL             string            `json:"url,omitempty"`              // non-empty = HTTP mode
	Transport       string            `json:"transport,omitempty"`        // "streamable-http" or "sse"
	ResolvedHeaders map[string]string `json:"resolved_headers,omitempty"` // pre-resolved header name→value
}

// ValidateConfigurationTool is a single tool from MCP tools/list, for the
// validate_configuration_result response.
type ValidateConfigurationTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// ValidateConfigurationResultMessage is sent by the worker in response to
// validate_configuration. Tools lists the available tools; Error is set on failure.
type ValidateConfigurationResultMessage struct {
	Type      MessageType                 `json:"type"`
	RequestID string                      `json:"request_id"`
	Tools     []ValidateConfigurationTool `json:"tools,omitempty"`
	Error     string                      `json:"error,omitempty"`
}

// ImagePart is a binary image returned by a tool, surfaced as multimodal content.
// Workers populate this alongside the JSON Result when the tool produces screenshots
// or other binary image output (e.g. the browser executor).
type ImagePart struct {
	MIMEType string `json:"mime_type"`
	Data     []byte `json:"data"` // raw bytes; JSON marshal base64-encodes automatically
}

// ToolResultMessage is sent by a tools worker after completing (or failing) a tool call.
type ToolResultMessage struct {
	Type        MessageType     `json:"type"`
	JobID       string          `json:"job_id"`        // Mirrors ToolJobMessage.JobID
	ParentJobID string          `json:"parent_job_id"` // Mirrors ToolJobMessage.ParentJobID
	ToolCallID  string          `json:"tool_call_id"`  // Mirrors ToolJobMessage.ToolCallID
	ToolName    string          `json:"tool_name"`     // Mirrors ToolJobMessage.ToolName
	Result      json.RawMessage `json:"result,omitempty"`
	// ImageParts carries binary images (e.g. screenshots) produced alongside the
	// text result. The router surfaces these as OpenAI-style multimodal content
	// parts in the continuation LLM message. Nil for non-image tools.
	ImageParts  []ImagePart     `json:"image_parts,omitempty"`
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
