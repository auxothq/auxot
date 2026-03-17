// Package workerconn provides a reusable WebSocket connection lifecycle for
// Auxot worker processes. It handles dialing, reconnection with exponential
// backoff, heartbeats, and thread-safe message writes.
package workerconn

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultHeartbeatInterval = 10 * time.Second
	defaultMaxReconnectDelay = 30 * time.Second
	heartbeatType            = "heartbeat"
)

// Config configures the connection lifecycle.
type Config struct {
	// ServerURL is the base Auxot server URL. The WebSocket path is appended
	// by OnConnect (e.g. "/ws/agent", "/ws/gpu").
	ServerURL string

	// Path is the WebSocket endpoint path, e.g. "/ws/agent".
	Path string

	// HeartbeatInterval controls how often heartbeat messages are sent.
	// Zero uses the default (10s).
	HeartbeatInterval time.Duration

	// MaxReconnectDelay caps the exponential backoff between reconnect attempts.
	// Zero uses the default (30s).
	MaxReconnectDelay time.Duration

	Logger *slog.Logger
}

// Handlers contains the callbacks invoked by RunForever.
type Handlers struct {
	// OnConnect is called once after a successful dial. The conn is ready to
	// send and receive. Return an error to close the connection immediately.
	OnConnect func(conn *Conn) error

	// OnMessage is called for each inbound message.
	OnMessage func(conn *Conn, raw json.RawMessage) error

	// OnDisconnect is called when the connection closes (before reconnect).
	// err is nil for clean closes, non-nil for transport errors.
	OnDisconnect func(err error)
}

// Conn wraps a websocket.Conn with a mutex for thread-safe writes.
// Multiple goroutines may call WriteJSON concurrently.
type Conn struct {
	ws  *websocket.Conn
	mu  sync.Mutex
}

// WriteJSON marshals v to JSON and sends it as a WebSocket text message.
func (c *Conn) WriteJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.WriteJSON(v)
}

// ReadMessage reads the next WebSocket message. Intended for use in OnConnect
// for initial handshakes before the message loop starts.
func (c *Conn) ReadMessage() ([]byte, error) {
	_, data, err := c.ws.ReadMessage()
	return data, err
}

// SetReadDeadline sets the read deadline on the underlying connection.
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.ws.SetReadDeadline(t)
}

// RunForever dials the server, calls OnConnect, runs the message loop, and
// reconnects with exponential backoff until ctx is cancelled.
func RunForever(ctx context.Context, cfg Config, h Handlers) error {
	heartbeat := cfg.HeartbeatInterval
	if heartbeat == 0 {
		heartbeat = defaultHeartbeatInterval
	}
	maxDelay := cfg.MaxReconnectDelay
	if maxDelay == 0 {
		maxDelay = defaultMaxReconnectDelay
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	backoff := time.Second
	for {
		if err := runOnce(ctx, cfg, h, heartbeat, log); err != nil {
			if ctx.Err() != nil {
				return nil // normal shutdown
			}
			log.Warn("connection lost, reconnecting", "err", err, "backoff", backoff)
			if h.OnDisconnect != nil {
				h.OnDisconnect(err)
			}
		} else {
			if h.OnDisconnect != nil {
				h.OnDisconnect(nil)
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
			backoff = min(backoff*2, maxDelay)
		}
	}
}

func runOnce(ctx context.Context, cfg Config, h Handlers, heartbeat time.Duration, log *slog.Logger) error {
	url := cfg.ServerURL + cfg.Path
	log.Info("dialing server", "url", url)

	ws, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", url, err)
	}
	defer ws.Close()

	conn := &Conn{ws: ws}

	if h.OnConnect != nil {
		if err := h.OnConnect(conn); err != nil {
			return fmt.Errorf("onConnect: %w", err)
		}
	}

	// Start heartbeat ticker.
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	// Read messages in a goroutine.
	msgCh := make(chan json.RawMessage, 16)
	errCh := make(chan error, 1)
	go func() {
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			msgCh <- json.RawMessage(data)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
			_ = conn.WriteJSON(map[string]string{"type": heartbeatType})

		case raw := <-msgCh:
			if h.OnMessage != nil {
				if err := h.OnMessage(conn, raw); err != nil {
					return fmt.Errorf("onMessage: %w", err)
				}
			}

		case err := <-errCh:
			return err
		}
	}
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
