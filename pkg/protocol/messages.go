// Package protocol defines the WebSocket message types for communication
// between GPU workers and the server (both OSS router and paid product).
//
// This is the shared contract. Both auxot-router and auxot-ws use these types.
package protocol

import (
	"encoding/json"
	"fmt"
)

// MessageType identifies the kind of message sent over the WebSocket connection.
type MessageType string

const (
	// Worker → Server
	TypeHello          MessageType = "hello"
	TypeHeartbeat      MessageType = "heartbeat"
	TypeConfig         MessageType = "config"
	TypeToken          MessageType = "token"
	TypeToolGenerating MessageType = "tool_generating"  // Sent once when the model starts writing a tool call
	TypeBuiltinTool    MessageType = "builtin_tool"     // CLI worker: native tool completed (Bash, Read, etc.)
	TypePromptProgress MessageType = "prompt_progress"  // GPU worker: prompt processing progress
	TypeComplete       MessageType = "complete"
	TypeError          MessageType = "error"

	// Server → Worker
	TypeHelloAck     MessageType = "hello_ack"
	TypeHeartbeatAck MessageType = "heartbeat_ack"
	TypeConfigAck    MessageType = "config_ack"
	TypeJob          MessageType = "job"
	TypeCancel       MessageType = "cancel"

	// Agent worker → Server
	TypeAgentToken      MessageType = "agent_token"       // streaming token from agent
	TypeAgentComplete   MessageType = "agent_complete"    // agent finished job
	TypeAgentError      MessageType = "agent_error"       // agent job failed
	TypeAgentToolCall   MessageType = "agent_tool_call"   // agent is calling a tool
	TypeAgentToolResult MessageType = "agent_tool_result" // agent received a tool result

	// Server → Agent worker
	TypeAgentJob MessageType = "agent_job" // dispatch a chat job
	// TypeReloadPolicy (already defined above) is also used for agent system prompt reload.
)

// Envelope is the first-pass parse of any WebSocket message.
// We read the "type" field to determine which concrete type to unmarshal into.
type Envelope struct {
	Type MessageType `json:"type"`
}

// --- Worker → Server messages ---

// HelloMessage is sent by the worker on connection to authenticate and
// announce its capabilities.
//
// If WorkerType is empty or "gpu", this is a GPU worker (backward compatible).
// If WorkerType is "tools", ToolsCapabilities is populated and Capabilities is ignored.
type HelloMessage struct {
	Type              MessageType        `json:"type"`
	GPUKey            string             `json:"gpu_key"`
	WorkerType        WorkerType         `json:"worker_type,omitempty"`
	Capabilities      Capabilities       `json:"capabilities"`
	ToolsCapabilities *ToolsCapabilities `json:"tools_capabilities,omitempty"`
}

// Capabilities describes what the GPU worker can do.
// ModelCapabilities is the list of model capabilities (e.g. vision when mmproj is loaded).
// When set, the server uses it for validation instead of inferring from model name only.
type Capabilities struct {
	Backend            string   `json:"backend"`
	Model              string   `json:"model"`
	CtxSize            int      `json:"ctx_size"`
	VRAMGB             float64  `json:"vram_gb,omitempty"`
	Parameters         string   `json:"parameters,omitempty"`
	TotalSlots         int      `json:"total_slots,omitempty"`
	ModelCapabilities  []string `json:"model_capabilities,omitempty"`
}

// HeartbeatMessage is a keepalive from the worker.
type HeartbeatMessage struct {
	Type MessageType `json:"type"`
}

// ConfigMessage is sent by the worker after spawning llama.cpp to advertise
// its discovered capabilities. The server validates these against the policy.
type ConfigMessage struct {
	Type         MessageType  `json:"type"`
	Capabilities Capabilities `json:"capabilities"`
}

// TokenMessage streams a single token from the worker for a running job.
// When Reasoning is true, the token is part of the model's internal
// chain-of-thought (e.g. <think> blocks) and should be displayed separately.
type TokenMessage struct {
	Type      MessageType `json:"type"`
	JobID     string      `json:"job_id"`
	Token     string      `json:"token"`
	Reasoning bool        `json:"reasoning,omitempty"`
}

// ToolGeneratingMessage is sent by the worker when the model starts
// writing a tool call. It gives the frontend immediate feedback that
// a tool call is being prepared (before the full call is assembled).
type ToolGeneratingMessage struct {
	Type  MessageType `json:"type"`
	JobID string      `json:"job_id"`
}

// PromptProgressMessage reports prompt processing progress from the GPU worker.
// Emitted during the prompt evaluation phase before token generation starts.
type PromptProgressMessage struct {
	Type      MessageType `json:"type"`
	JobID     string      `json:"job_id"`
	Total     int         `json:"total"`     // total prompt tokens
	Cached    int         `json:"cached"`    // tokens served from KV cache
	Processed int         `json:"processed"` // tokens processed so far
}

// BuiltinToolMessage is sent by the CLI worker in real-time when a CLI-native
// tool (e.g. Bash, Read, WebSearch) completes. Sent before the first text token
// of the following assistant turn to preserve correct display order.
type BuiltinToolMessage struct {
	Type   MessageType `json:"type"` // TypeBuiltinTool
	JobID  string      `json:"job_id"`
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Args   string      `json:"arguments"`
	Result string      `json:"result"`
}

// ToolCall represents a function call requested by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction contains the name and arguments of a tool call (response side).
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDefinition describes a tool the model can call (request side).
// Parameters is the raw JSON Schema for the function's input.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// BuiltinToolUse records a single invocation of a CLI-native tool (e.g. Bash, Read)
// that the CLI model executed autonomously. These are display-only — the Auxot server
// shows them as collapsed pills but does not execute or re-dispatch them.
type BuiltinToolUse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string of the input
	Result    string `json:"result,omitempty"`
}

// CompleteMessage is sent by the worker when a job finishes.
type CompleteMessage struct {
	Type             MessageType      `json:"type"`
	JobID            string           `json:"job_id"`
	FullResponse     string           `json:"full_response"`
	ReasoningContent string           `json:"reasoning_content,omitempty"` // Full chain-of-thought text
	DurationMS       int64            `json:"duration_ms,omitempty"`
	CacheTokens      int              `json:"cache_tokens,omitempty"`
	InputTokens      int              `json:"input_tokens,omitempty"`
	OutputTokens     int              `json:"output_tokens,omitempty"`
	ReasoningTokens  int              `json:"reasoning_tokens,omitempty"` // Count of reasoning/thinking tokens
	ToolCalls        []ToolCall       `json:"tool_calls,omitempty"`
	BuiltinToolUses  []BuiltinToolUse `json:"builtin_tool_uses,omitempty"`
}

// ErrorMessage is sent by the worker when a job fails.
type ErrorMessage struct {
	Type    MessageType `json:"type"`
	JobID   string      `json:"job_id,omitempty"`
	Error   string      `json:"error"`
	Details string      `json:"details,omitempty"`
}

// --- Server → Worker messages ---

// Policy tells the worker what model and configuration to use.
type Policy struct {
	// WorkerType identifies the execution backend.
	// "gpu" (default/empty) = llama.cpp; "cli" = local CLI subprocess.
	WorkerType string `json:"worker_type,omitempty"`
	// CLIType identifies which CLI tool to spawn when WorkerType is "cli".
	// Supported values: "claude" | "cursor" | "codex"
	CLIType string `json:"cli_type,omitempty"`
	// BuiltinTools lists which claude CLI built-in tools the worker should enable.
	// nil/empty → --tools "" (only MCP/external tools, no built-ins).
	// ["default"] → --tools default (all built-ins enabled).
	// Any other list → --tools "Bash,Read,WebSearch,..." (specific built-ins).
	BuiltinTools []string `json:"builtin_tools,omitempty"`

	ModelName      string   `json:"model_name"`
	Quantization   string   `json:"quantization"`
	ContextSize    int      `json:"context_size"`
	MaxParallelism int      `json:"max_parallelism"`
	Parameters     string   `json:"parameters,omitempty"`
	Family         string   `json:"family,omitempty"`
	Capabilities   []string `json:"capabilities"`
}

// HelloAckMessage is the server's response to a HelloMessage.
type HelloAckMessage struct {
	Type               MessageType  `json:"type"`
	Success            bool         `json:"success"`
	GPUID              string       `json:"gpu_id,omitempty"`
	Policy             *Policy      `json:"policy,omitempty"`
	ToolsPolicy        *ToolsPolicy `json:"tools_policy,omitempty"` // Initial policy for tools workers
	Error              string       `json:"error,omitempty"`
	ReconnectInSeconds int          `json:"reconnectInSeconds,omitempty"`
}

// HeartbeatAckMessage is the server's response to a HeartbeatMessage.
type HeartbeatAckMessage struct {
	Type MessageType `json:"type"`
}

// ConfigAckMessage is the server's response to a ConfigMessage.
type ConfigAckMessage struct {
	Type    MessageType `json:"type"`
	Success bool        `json:"success"`
	Error   string      `json:"error,omitempty"`
}

// ChatMessage represents a single message in a chat conversation.
// Content is json.RawMessage to accept both string and array (OpenAI multimodal:
// images use content: [{type:"text",text:"..."},{type:"image_url",image_url:{...}}]).
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage  `json:"content"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
}

// ContentString returns a string representation of Content for logging.
// If Content is a JSON string, returns it; if an array of parts, concatenates text parts.
func (m *ChatMessage) ContentString() string {
	if len(m.Content) == 0 {
		return ""
	}
	// Try string first
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	// Try array of content parts (OpenAI multimodal)
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url,omitempty"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var b []byte
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				b = append(b, p.Text...)
			} else if p.Type == "image_url" && p.ImageURL != nil {
				b = append(b, "[image]"...)
			}
		}
		return string(b)
	}
	return string(m.Content)
}

// ChatContentString returns json.RawMessage for a plain text content string.
func ChatContentString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// Tool describes a tool/function the model can call (request side).
// Uses ToolDefinition (not ToolFunction) because definitions carry
// description and parameters schema, not call arguments.
type Tool struct {
	Type     string         `json:"type"`
	Function ToolDefinition `json:"function"`
}

// JobMessage is sent by the server to assign work to a worker.
type JobMessage struct {
	Type            MessageType   `json:"type"`
	JobID           string        `json:"job_id"`
	Messages        []ChatMessage `json:"messages"`
	Tools           []Tool        `json:"tools,omitempty"`
	Temperature     *float64      `json:"temperature,omitempty"`
	MaxTokens       *int          `json:"max_tokens,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"` // "none", "low", "medium", "high"
	Data            map[string]any `json:"data,omitempty"`           // Optional: tool arguments (e.g. size, steps for image_gen)
}

// CancelMessage tells the worker to stop processing a job.
type CancelMessage struct {
	Type  MessageType `json:"type"`
	JobID string      `json:"job_id"`
}

// ParseMessage reads a raw WebSocket message and returns the typed message.
// It first parses the envelope to determine the type, then unmarshals
// into the concrete struct.
func ParseMessage(data []byte) (any, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("parsing message envelope: %w", err)
	}

	switch env.Type {
	// Worker → Server
	case TypeHello:
		var msg HelloMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing hello message: %w", err)
		}
		return msg, nil

	case TypeHeartbeat:
		return HeartbeatMessage{Type: TypeHeartbeat}, nil

	case TypeConfig:
		var msg ConfigMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing config message: %w", err)
		}
		return msg, nil

	case TypeToken:
		var msg TokenMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing token message: %w", err)
		}
		return msg, nil

	case TypeToolGenerating:
		var msg ToolGeneratingMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing tool_generating message: %w", err)
		}
		return msg, nil

	case TypePromptProgress:
		var msg PromptProgressMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing prompt_progress message: %w", err)
		}
		return msg, nil

	case TypeBuiltinTool:
		var msg BuiltinToolMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing builtin_tool message: %w", err)
		}
		return msg, nil

	case TypeComplete:
		var msg CompleteMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing complete message: %w", err)
		}
		return msg, nil

	case TypeError:
		var msg ErrorMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing error message: %w", err)
		}
		return msg, nil

	// Server → Worker
	case TypeHelloAck:
		var msg HelloAckMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing hello_ack message: %w", err)
		}
		return msg, nil

	case TypeHeartbeatAck:
		return HeartbeatAckMessage{Type: TypeHeartbeatAck}, nil

	case TypeConfigAck:
		var msg ConfigAckMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing config_ack message: %w", err)
		}
		return msg, nil

	case TypeJob:
		var msg JobMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing job message: %w", err)
		}
		return msg, nil

	case TypeCancel:
		var msg CancelMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing cancel message: %w", err)
		}
		return msg, nil

	// Agent worker messages
	case TypeAgentJob:
		var msg AgentJobMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing agent_job message: %w", err)
		}
		return msg, nil

	case TypeAgentToken:
		var msg AgentTokenMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing agent_token message: %w", err)
		}
		return msg, nil

	case TypeAgentComplete:
		var msg AgentCompleteMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing agent_complete message: %w", err)
		}
		return msg, nil

	case TypeAgentError:
		var msg AgentErrorMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing agent_error message: %w", err)
		}
		return msg, nil

	case TypeAgentToolCall:
		var msg AgentToolCallMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing agent_tool_call message: %w", err)
		}
		return msg, nil

	case TypeAgentToolResult:
		var msg AgentToolResultMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing agent_tool_result message: %w", err)
		}
		return msg, nil

	// Tools-worker messages (defined in tools_messages.go)
	case TypeToolJob:
		var msg ToolJobMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing tool_job message: %w", err)
		}
		return msg, nil

	case TypeToolResult:
		var msg ToolResultMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing tool_result message: %w", err)
		}
		return msg, nil

	case TypeReloadPolicy:
		var msg ReloadPolicyMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing reload_policy message: %w", err)
		}
		return msg, nil

	case TypePolicyReloaded:
		var msg PolicyReloadedMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing policy_reloaded message: %w", err)
		}
		return msg, nil

	case TypeValidateConfiguration:
		var msg ValidateConfigurationMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("parsing validate_configuration message: %w", err)
		}
		return msg, nil

	default:
		return nil, fmt.Errorf("unknown message type: %q", env.Type)
	}
}

// ── Agent worker messages ─────────────────────────────────────────────────────

// AgentHelloMessage is sent by the agent worker on connection.
type AgentHelloMessage struct {
	Type       MessageType   `json:"type"`        // TypeHello
	WorkerType string        `json:"worker_type"` // "agent"
	AgentKey   string        `json:"agent_key"`
	Metadata   AgentMetadata `json:"metadata,omitempty"`
}

// AgentMetadata is extra info the agent sends at connect time.
type AgentMetadata struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	SoulDigest  string `json:"soul_digest,omitempty"` // sha256 of SOUL.md
}

// ExternalToolDef describes a tool that is executed server-side (not locally).
// Source is "builtin" for Auxot built-in server tools, or "mcp:package-name"
// for tools provided by an MCP server.
type ExternalToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Source      string          `json:"source"` // "builtin", "mcp:package-name"
}

// AgentHelloAckMessage is the server's response to a valid agent hello.
type AgentHelloAckMessage struct {
	Type          MessageType       `json:"type"` // TypeHelloAck
	Status        string            `json:"status"`
	AgentID       string            `json:"agent_id,omitempty"`
	SystemPrompt  string            `json:"system_prompt,omitempty"`
	ExternalTools []ExternalToolDef `json:"external_tools,omitempty"`
	Error         string            `json:"error,omitempty"`
}

// AgentChatMsg is a single turn in the conversation history sent to the agent.
type AgentChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// AgentJobMessage is sent by the server to dispatch a chat job to the agent worker.
type AgentJobMessage struct {
	Type         MessageType    `json:"type"` // TypeAgentJob
	JobID        string         `json:"job_id"`
	SystemPrompt string         `json:"system_prompt,omitempty"`
	Messages     []AgentChatMsg `json:"messages"`
}

// AgentTokenMessage streams a single token from the agent back to the server.
type AgentTokenMessage struct {
	Type  MessageType `json:"type"` // TypeAgentToken
	JobID string      `json:"job_id"`
	Token string      `json:"token"`
}

// AgentCompleteMessage signals the agent has finished processing a job.
type AgentCompleteMessage struct {
	Type       MessageType `json:"type"` // TypeAgentComplete
	JobID      string      `json:"job_id"`
	StopReason string      `json:"stop_reason,omitempty"`
}

// AgentErrorMessage signals that the agent encountered an error on a job.
type AgentErrorMessage struct {
	Type  MessageType `json:"type"` // TypeAgentError
	JobID string      `json:"job_id,omitempty"`
	Error string      `json:"error"`
}

// AgentToolCallMessage is sent by the agent when it's about to execute a tool.
// This allows the server to publish the event to the UI in real time.
type AgentToolCallMessage struct {
	Type      MessageType `json:"type"` // TypeAgentToolCall
	JobID     string      `json:"job_id"`
	ID        string      `json:"id"`        // tool call ID from the model
	Name      string      `json:"name"`      // tool function name
	Arguments string      `json:"arguments"` // JSON string of tool input
}

// AgentToolResultMessage is sent by the agent after a tool finishes execution.
type AgentToolResultMessage struct {
	Type       MessageType `json:"type"` // TypeAgentToolResult
	JobID      string      `json:"job_id"`
	ToolCallID string      `json:"tool_call_id"` // matches AgentToolCallMessage.ID
	Content    string      `json:"content"`      // tool output
	IsError    bool        `json:"is_error,omitempty"`
}

// AgentReloadPolicyMessage is sent by the server to push an updated system prompt.
// Uses TypeReloadPolicy ("reload_policy") as the wire type.
type AgentReloadPolicyMessage struct {
	Type          MessageType       `json:"type"` // TypeReloadPolicy
	SystemPrompt  string            `json:"system_prompt"`
	ExternalTools []ExternalToolDef `json:"external_tools,omitempty"`
}

// MarshalMessage serializes a message to JSON bytes for sending over WebSocket.
func MarshalMessage(msg any) ([]byte, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshaling message: %w", err)
	}
	return data, nil
}
