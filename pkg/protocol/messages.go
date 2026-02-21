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
	TypeToolGenerating MessageType = "tool_generating" // Sent once when the model starts writing a tool call
	TypeComplete       MessageType = "complete"
	TypeError          MessageType = "error"

	// Server → Worker
	TypeHelloAck     MessageType = "hello_ack"
	TypeHeartbeatAck MessageType = "heartbeat_ack"
	TypeConfigAck    MessageType = "config_ack"
	TypeJob          MessageType = "job"
	TypeCancel       MessageType = "cancel"
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
type Capabilities struct {
	Backend    string  `json:"backend"`
	Model      string  `json:"model"`
	CtxSize    int     `json:"ctx_size"`
	VRAMGB     float64 `json:"vram_gb,omitempty"`
	Parameters string  `json:"parameters,omitempty"`
	TotalSlots int     `json:"total_slots,omitempty"`
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

// CompleteMessage is sent by the worker when a job finishes.
type CompleteMessage struct {
	Type             MessageType `json:"type"`
	JobID            string      `json:"job_id"`
	FullResponse     string      `json:"full_response"`
	ReasoningContent string      `json:"reasoning_content,omitempty"` // Full chain-of-thought text
	DurationMS       int64       `json:"duration_ms,omitempty"`
	CacheTokens      int         `json:"cache_tokens,omitempty"`
	InputTokens      int         `json:"input_tokens,omitempty"`
	OutputTokens     int         `json:"output_tokens,omitempty"`
	ReasoningTokens  int         `json:"reasoning_tokens,omitempty"` // Count of reasoning/thinking tokens
	ToolCalls        []ToolCall  `json:"tool_calls,omitempty"`
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
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
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

	default:
		return nil, fmt.Errorf("unknown message type: %q", env.Type)
	}
}

// MarshalMessage serializes a message to JSON bytes for sending over WebSocket.
func MarshalMessage(msg any) ([]byte, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshaling message: %w", err)
	}
	return data, nil
}
