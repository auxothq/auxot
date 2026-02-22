package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/auxothq/auxot/pkg/protocol"
)

// Connection manages the WebSocket connection from a tools worker to the router.
// It mirrors the pattern in internal/worker/connection.go but is simpler —
// tools workers don't have a two-phase startup (no model download).
type Connection struct {
	url    string
	key    string
	tools  []string // tool names to advertise (updated by UpdateAdvertisedTools)
	logger *slog.Logger
	cfg    *Config

	conn   *websocket.Conn
	mu     sync.Mutex // Protects writes to conn and reads of tools
	toolID string     // Server-assigned tools worker ID

	// Callback invoked when the router sends a tool job.
	onToolJob func(job protocol.ToolJobMessage)

	// Callback invoked when the router sends a reload_policy message,
	// or when the initial policy is received in hello_ack.
	onReloadPolicy func(policy protocol.ToolsPolicy)
}

// NewConnection creates a Connection to the router.
func NewConnection(cfg *Config, tools []string, logger *slog.Logger) *Connection {
	return &Connection{
		url:    cfg.RouterURL,
		key:    cfg.GPUKey,
		tools:  tools,
		logger: logger,
		cfg:    cfg,
	}
}

// OnToolJob registers the callback invoked when the router sends a tool job.
func (c *Connection) OnToolJob(fn func(job protocol.ToolJobMessage)) {
	c.onToolJob = fn
}

// OnReloadPolicy registers the callback invoked when the router sends a
// reload_policy message (or when the initial policy arrives in hello_ack).
// The callback is called in a separate goroutine so it does not block the
// message loop.
func (c *Connection) OnReloadPolicy(fn func(policy protocol.ToolsPolicy)) {
	c.onReloadPolicy = fn
}

// ToolID returns the server-assigned tools worker ID (available after Connect).
func (c *Connection) ToolID() string {
	return c.toolID
}

// UpdateAdvertisedTools updates the list of tools advertised to the router.
// The new list is used on the next reconnection attempt.
func (c *Connection) UpdateAdvertisedTools(tools []string) {
	c.mu.Lock()
	c.tools = tools
	c.mu.Unlock()
}

// Connect dials the router, authenticates, and enters the message loop.
// It blocks until the connection is closed or ctx is cancelled. The caller
// should call this in a retry loop (handled by Worker.Run).
func (c *Connection) Connect(ctx context.Context) error {
	c.logger.Info("connecting to router", "url", c.url)

	conn, _, err := websocket.DefaultDialer.Dial(c.url, nil)
	if err != nil {
		return fmt.Errorf("dialing router: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	currentTools := make([]string, len(c.tools))
	copy(currentTools, c.tools)
	c.mu.Unlock()

	// Send hello with tools worker capabilities.
	hello := protocol.HelloMessage{
		Type:       protocol.TypeHello,
		GPUKey:     c.key,
		WorkerType: protocol.WorkerTypeTools,
		ToolsCapabilities: &protocol.ToolsCapabilities{
			Tools: currentTools,
		},
	}
	if err := c.sendJSON(hello); err != nil {
		conn.Close()
		return fmt.Errorf("sending hello: %w", err)
	}

	// Read hello_ack (with deadline so we don't hang forever).
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, msgData, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return fmt.Errorf("reading hello_ack: %w", err)
	}
	conn.SetReadDeadline(time.Time{}) // Clear deadline for normal operation

	msg, err := protocol.ParseMessage(msgData)
	if err != nil {
		conn.Close()
		return fmt.Errorf("parsing hello_ack: %w", err)
	}

	ack, ok := msg.(protocol.HelloAckMessage)
	if !ok {
		conn.Close()
		return fmt.Errorf("expected hello_ack, got %T", msg)
	}
	if !ack.Success {
		conn.Close()
		return fmt.Errorf("authentication failed: %s", ack.Error)
	}

	c.toolID = ack.GPUID // Router uses the same ID field for any worker type
	c.logger.Info("connected to router",
		"tool_id", c.toolID,
		"tools", currentTools,
	)

	// Apply the initial policy if the server provided one.
	if ack.ToolsPolicy != nil && c.onReloadPolicy != nil {
		go c.onReloadPolicy(*ack.ToolsPolicy)
	}

	// Start heartbeat goroutine.
	done := make(chan struct{})
	go c.heartbeatLoop(done)
	defer close(done)

	// When the context is cancelled (SIGTERM/SIGINT), close the WebSocket so
	// that messageLoop's ReadMessage call returns immediately instead of
	// blocking until the 30-second SIGKILL timeout.
	go func() {
		select {
		case <-done:
			// messageLoop exited normally — nothing to do.
		case <-ctx.Done():
			c.logger.Info("shutting down: closing router connection")
			c.Close()
		}
	}()

	return c.messageLoop()
}

// SendResult sends a tool result back to the router.
func (c *Connection) SendResult(result protocol.ToolResultMessage) error {
	return c.sendJSON(result)
}

// SendPolicyReloaded notifies the router that the worker has applied a new policy
// and lists the tool identifiers it now handles, including any MCP aggregate schemas.
func (c *Connection) SendPolicyReloaded(tools []string, mcpSchemas []protocol.McpAggregateSchema) error {
	msg := protocol.PolicyReloadedMessage{
		Type:            protocol.TypePolicyReloaded,
		AdvertisedTools: tools,
		McpSchemas:      mcpSchemas,
	}
	return c.sendJSON(msg)
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

// messageLoop reads messages from the router until the connection closes.
func (c *Connection) messageLoop() error {
	for {
		_, msgData, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway,
				websocket.CloseNormalClosure,
				websocket.CloseNoStatusReceived, // server process restart (hot-reload)
			) {
				return fmt.Errorf("unexpected disconnect: %w", err)
			}
			return nil // Normal or server-initiated close — backoff resets
		}

		msg, err := protocol.ParseMessage(msgData)
		if err != nil {
			c.logger.Warn("invalid message from router", "error", err)
			continue
		}

		switch m := msg.(type) {
		case protocol.ToolJobMessage:
			c.logger.Info("tool job received",
				"job_id", m.JobID,
				"tool", m.ToolName,
				"parent_job_id", m.ParentJobID,
			)
			if c.onToolJob != nil {
				// Run in a goroutine — tool execution can take seconds and we
				// don't want to block heartbeats or incoming cancels.
				go c.onToolJob(m)
			}

		case protocol.ReloadPolicyMessage:
			c.logger.Info("reload_policy received")
			if c.onReloadPolicy != nil {
				go c.onReloadPolicy(m.Policy)
			}

		case protocol.HeartbeatAckMessage:
			// Silent — expected response to our heartbeat.

		default:
			c.logger.Warn("unexpected message from router", "type", fmt.Sprintf("%T", m))
		}
	}
}

func (c *Connection) sendJSON(v any) error {
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
			hb := protocol.HeartbeatMessage{Type: protocol.TypeHeartbeat}
			if err := c.sendJSON(hb); err != nil {
				c.logger.Warn("heartbeat send failed", "error", err)
			}
		}
	}
}
