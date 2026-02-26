package worker

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/auxothq/auxot/pkg/protocol"
)

// Connection manages the WebSocket connection to the router.
type Connection struct {
	url    string
	key    string
	logger *slog.Logger
	cfg    *Config

	conn  *websocket.Conn
	mu    sync.Mutex // Protects writes to conn
	gpuID string

	// Callbacks
	onJob    func(job protocol.JobMessage)
	onCancel func(jobID string)
}

// NewConnection creates a Connection to the router.
func NewConnection(url, adminKey string, cfg *Config, logger *slog.Logger) *Connection {
	return &Connection{
		url:    url,
		key:    adminKey,
		logger: logger,
		cfg:    cfg,
	}
}

// OnJob registers a callback invoked when the router sends a job.
func (c *Connection) OnJob(fn func(job protocol.JobMessage)) {
	c.onJob = fn
}

// OnCancel registers a callback invoked when the router cancels a job.
func (c *Connection) OnCancel(fn func(jobID string)) {
	c.onCancel = fn
}

// GPUID returns the server-assigned GPU ID.
func (c *Connection) GPUID() string {
	return c.gpuID
}

// FetchPolicy opens a temporary connection to the router, authenticates,
// receives the model policy from hello_ack, and disconnects.
// This is Phase 1 — before model download and llama.cpp spawn.
func (c *Connection) FetchPolicy() (*protocol.Policy, error) {
	c.logger.Info("connecting to router", "url", c.url)

	conn, _, err := websocket.DefaultDialer.Dial(c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting to router: %w", err)
	}
	defer conn.Close()

	// Send hello with placeholder capabilities
	hello := protocol.HelloMessage{
		Type:   protocol.TypeHello,
		GPUKey: c.key,
		Capabilities: protocol.Capabilities{
			Backend: "pending",
			Model:   "pending",
			CtxSize: 0,
		},
	}
	DebugClientToServer(hello)
	data, _ := json.Marshal(hello)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return nil, fmt.Errorf("sending hello: %w", err)
	}

	// Read hello_ack
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, msgData, err := conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("reading hello_ack: %w", err)
	}

	msg, err := protocol.ParseMessage(msgData)
	if err != nil {
		return nil, fmt.Errorf("parsing hello_ack: %w", err)
	}
	DebugServerToClient(msg)

	ack, ok := msg.(protocol.HelloAckMessage)
	if !ok {
		return nil, fmt.Errorf("expected hello_ack, got %T", msg)
	}

	if !ack.Success {
		return nil, fmt.Errorf("authentication failed: %s", ack.Error)
	}

	if ack.Policy == nil {
		return nil, fmt.Errorf("server did not send policy in hello_ack")
	}

	c.gpuID = ack.GPUID
	return ack.Policy, nil
}

// ConnectPermanent establishes the permanent WebSocket connection and sends
// the discovered capabilities. This is Phase 2 — after llama.cpp is running.
func (c *Connection) ConnectPermanent(caps *DiscoveredCaps) error {
	c.logger.Info("connecting to router", "url", c.url)

	conn, _, err := websocket.DefaultDialer.Dial(c.url, nil)
	if err != nil {
		return fmt.Errorf("connecting to router: %w", err)
	}
	c.conn = conn

	// Send hello with real capabilities
	hello := protocol.HelloMessage{
		Type:   protocol.TypeHello,
		GPUKey: c.key,
		Capabilities: protocol.Capabilities{
			Backend:    caps.Backend,
			Model:      caps.Model,
			CtxSize:    caps.CtxSize,
			VRAMGB:     caps.VRAMGB,
			Parameters: caps.Parameters,
			TotalSlots: caps.TotalSlots,
		},
	}
	if err := c.sendJSON(hello); err != nil {
		return fmt.Errorf("sending hello: %w", err)
	}

	// Read hello_ack
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, msgData, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("reading hello_ack: %w", err)
	}
	conn.SetReadDeadline(time.Time{}) // Clear deadline

	msg, err := protocol.ParseMessage(msgData)
	if err != nil {
		return fmt.Errorf("parsing hello_ack: %w", err)
	}

	ack, ok := msg.(protocol.HelloAckMessage)
	if !ok {
		return fmt.Errorf("expected hello_ack, got %T", msg)
	}

	if !ack.Success {
		return fmt.Errorf("authentication failed: %s", ack.Error)
	}

	c.gpuID = ack.GPUID

	// Send config with full capabilities
	configMsg := protocol.ConfigMessage{
		Type: protocol.TypeConfig,
		Capabilities: protocol.Capabilities{
			Backend:    caps.Backend,
			Model:      caps.Model,
			CtxSize:    caps.CtxSize,
			VRAMGB:     caps.VRAMGB,
			Parameters: caps.Parameters,
			TotalSlots: caps.TotalSlots,
		},
	}
	if err := c.sendJSON(configMsg); err != nil {
		return fmt.Errorf("sending config: %w", err)
	}

	return nil
}

// RunMessageLoop reads messages from the router until the connection closes.
// It dispatches jobs and cancels to the registered callbacks.
// Also sends heartbeats on a timer.
func (c *Connection) RunMessageLoop() error {
	// Start heartbeat goroutine
	done := make(chan struct{})
	go c.heartbeatLoop(done)
	defer close(done)

	for {
		_, msgData, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				return fmt.Errorf("unexpected disconnect: %w", err)
			}
			return nil // Normal close
		}

		msg, err := protocol.ParseMessage(msgData)
		if err != nil {
			c.logger.Warn("invalid message from router", "error", err)
			continue
		}

		// Debug log inbound messages (level 1), skip heartbeat_ack
		if _, isHBAck := msg.(protocol.HeartbeatAckMessage); !isHBAck {
			DebugServerToClient(msg)
		}

		switch m := msg.(type) {
		case protocol.JobMessage:
			// Log job receipt with progressive detail based on debug level
			logJobReceived(c.logger, m)
			if c.onJob != nil {
				go c.onJob(m)
			}
		case protocol.CancelMessage:
			c.logger.Info("received cancel", "job_id", m.JobID)
			if c.onCancel != nil {
				c.onCancel(m.JobID)
			}
		case protocol.HeartbeatAckMessage:
			// Silent
		case protocol.ConfigAckMessage:
			if m.Success {
				c.logger.Info("config acknowledged by router")
			} else {
				c.logger.Error("config rejected by router", "error", m.Error)
			}
		default:
			c.logger.Warn("unexpected message from router", "type", fmt.Sprintf("%T", m))
		}
	}
}

// SendToolGenerating notifies the router that the model has started
// generating a tool call. This lets the frontend show a spinner before
// the full call is assembled and dispatched.
func (c *Connection) SendToolGenerating(jobID string) error {
	return c.sendJSON(protocol.ToolGeneratingMessage{
		Type:  protocol.TypeToolGenerating,
		JobID: jobID,
	})
}

// SendToken sends a token back to the router for a running job.
func (c *Connection) SendToken(jobID, token string) error {
	return c.sendJSON(protocol.TokenMessage{
		Type:  protocol.TypeToken,
		JobID: jobID,
		Token: token,
	})
}

// SendReasoningToken sends a reasoning/thinking token back to the router.
func (c *Connection) SendReasoningToken(jobID, token string) error {
	return c.sendJSON(protocol.TokenMessage{
		Type:      protocol.TypeToken,
		JobID:     jobID,
		Token:     token,
		Reasoning: true,
	})
}

// SendComplete sends a completion message to the router.
func (c *Connection) SendComplete(jobID, fullResponse, reasoningContent string, durationMS int64, cacheTokens, inputTokens, outputTokens, reasoningTokens int, toolCalls []protocol.ToolCall) error {
	return c.sendJSON(protocol.CompleteMessage{
		Type:             protocol.TypeComplete,
		JobID:            jobID,
		FullResponse:     fullResponse,
		ReasoningContent: reasoningContent,
		DurationMS:       durationMS,
		CacheTokens:      cacheTokens,
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		ReasoningTokens:  reasoningTokens,
		ToolCalls:        toolCalls,
	})
}

// SendError sends an error message to the router for a job.
func (c *Connection) SendError(jobID, errMsg string) error {
	return c.sendJSON(protocol.ErrorMessage{
		Type:  protocol.TypeError,
		JobID: jobID,
		Error: errMsg,
	})
}

// Close closes the WebSocket connection.
func (c *Connection) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

func (c *Connection) sendJSON(v interface{}) error {
	// Debug log outbound messages (level 1), skip heartbeats
	if _, isHB := v.(protocol.HeartbeatMessage); !isHB {
		DebugClientToServer(v)
	}

	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *Connection) heartbeatLoop(done <-chan struct{}) {
	ticker := time.NewTicker(c.cfg.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := c.sendJSON(protocol.HeartbeatMessage{Type: protocol.TypeHeartbeat}); err != nil {
				c.logger.Warn("heartbeat send failed", "error", err)
			}
		}
	}
}

// logJobReceived logs a job receipt with progressive detail based on debug level.
// Level 0: Summary stats only (job_id, message count, tool count, size estimates)
// Level 1: Level 0 + latest message content + tool names
// Level 2: Level 0 + Level 1 + full JSON payload
func logJobReceived(logger *slog.Logger, job protocol.JobMessage) {
	level := DebugLevel()

	// Level 0: Always log summary stats
	attrs := []any{
		"job_id", job.JobID,
		"num_messages", len(job.Messages),
		"num_tools", len(job.Tools),
	}

	// Estimate token count (rough: 4 chars per token)
	totalChars := 0
	for _, msg := range job.Messages {
		totalChars += len(msg.Content)
	}
	estTokens := totalChars / 4
	attrs = append(attrs, "est_input_tokens", estTokens)

	// Estimate tool size
	toolSizeKB := 0
	for _, tool := range job.Tools {
		toolSizeKB += len(tool.Function.Name) + len(tool.Function.Description) + len(tool.Function.Parameters)
	}
	toolSizeKB /= 1024
	if len(job.Tools) > 0 {
		attrs = append(attrs, "tools_size_kb", toolSizeKB)
	}

	// Level 1+: Add latest message content (full, no truncation) and tool names
	if level >= 1 {
		if len(job.Messages) > 0 {
			lastMsg := job.Messages[len(job.Messages)-1]
			attrs = append(attrs, "latest_message_role", lastMsg.Role)
			attrs = append(attrs, "latest_message_content", lastMsg.ContentString())
		}

		if len(job.Tools) > 0 {
			toolNames := make([]string, len(job.Tools))
			for i, t := range job.Tools {
				toolNames[i] = t.Function.Name
			}
			attrs = append(attrs, "tool_names", toolNames)
		}
	}

	// Level 2+: Add full payload
	if level >= 2 {
		attrs = append(attrs, "full_payload", job)
	}

	logger.Info("job started", attrs...)
}
