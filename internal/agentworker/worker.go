// Package agentworker implements the agent worker that connects to an Auxot
// server as a worker-backed agent. It reads SOUL.md and agent.yaml from the
// gitagent directory, authenticates with an agent key, and executes chat jobs
// using an embedded agentic loop backed by Auxot's OpenAI-compatible API.
package agentworker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/auxothq/auxot/pkg/protocol"
)

// Config holds the worker configuration.
type Config struct {
	// ServerURL is the normalised WebSocket URL including path: wss://host/ws
	// Set via AUXOT_ROUTER_URL. Accepts http/https/ws/wss schemes and bare
	// hostnames; the path is always replaced with /ws.
	ServerURL string
	AgentKey  string // agent key (wrk_xxx)
	Dir       string // gitagent directory (default: current directory)
}

// Worker manages the WebSocket connection to the Auxot server and executes
// chat jobs by spawning Claude Code.
type Worker struct {
	cfg    Config
	logger *slog.Logger
	mu     sync.Mutex
	conn   *websocket.Conn

	gitagent      *GitAgent
	externalTools []protocol.ExternalToolDef
	toolClient    *ToolClient
}

// New creates a Worker.
func New(cfg Config, logger *slog.Logger) *Worker {
	return &Worker{cfg: cfg, logger: logger}
}

// Run connects to the server and processes jobs until ctx is cancelled.
// Reconnects with exponential backoff on disconnect.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.validateDir(); err != nil {
		return fmt.Errorf("invalid gitagent directory %q: %w", w.cfg.Dir, err)
	}
	w.logger.Info("agent worker starting",
		"dir", w.cfg.Dir,
		"server", w.cfg.ServerURL,
	)

	backoff := 1 * time.Second
	for {
		if err := w.connectAndRun(ctx); err != nil {
			if ctx.Err() != nil {
				return nil // normal shutdown
			}
			w.logger.Warn("connection failed, reconnecting", "err", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}
}

func (w *Worker) connectAndRun(ctx context.Context) error {
	serverURL := w.cfg.ServerURL
	w.logger.Info("connecting to router", "AUXOT_ROUTER_URL", serverURL)

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, serverURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// Close the connection when the context is cancelled so conn.ReadMessage()
	// unblocks immediately on CTRL+C / SIGTERM instead of hanging forever.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	w.mu.Lock()
	w.conn = conn
	w.mu.Unlock()

	// Load gitagent directory (SOUL.md, agent.yaml, etc.).
	ga, err := LoadGitAgent(w.cfg.Dir)
	if err != nil {
		return fmt.Errorf("load gitagent: %w", err)
	}
	w.gitagent = ga

	// Send hello.
	hello := protocol.AgentHelloMessage{
		Type:       protocol.TypeHello,
		WorkerType: "agent",
		AgentKey:   w.cfg.AgentKey,
		Metadata: protocol.AgentMetadata{
			Name:        ga.Config.Name,
			Description: ga.Config.Description,
			SoulDigest:  w.soulDigest(),
		},
	}
	if err := conn.WriteJSON(hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	// Read hello_ack.
	conn.SetReadDeadline(time.Now().Add(30 * time.Second)) //nolint:errcheck
	_, rawAck, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read hello_ack: %w", err)
	}
	conn.SetReadDeadline(time.Time{}) //nolint:errcheck

	var ack protocol.AgentHelloAckMessage
	if err := json.Unmarshal(rawAck, &ack); err != nil {
		return fmt.Errorf("parse hello_ack: %w", err)
	}
	if ack.Status != "ok" {
		return fmt.Errorf("hello rejected: %s", ack.Error)
	}

	// Store external tools and build tool client from hello_ack.
	httpURL := toHTTPURL(w.cfg.ServerURL)
	w.mu.Lock()
	w.externalTools = ack.ExternalTools
	w.toolClient = NewToolClient(httpURL, w.cfg.AgentKey)
	w.mu.Unlock()

	w.logger.Info("connected to server", "agent_id", ack.AgentID, "external_tools", len(ack.ExternalTools))

	// Message loop.
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		var env struct {
			Type  string `json:"type"`
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			w.logger.Warn("unmarshal envelope failed", "err", err)
			continue
		}

		switch env.Type {
		case "heartbeat_ack":
			// nothing to do

		case "reload_policy":
			var rp protocol.AgentReloadPolicyMessage
			if err := json.Unmarshal(raw, &rp); err != nil {
				w.logger.Warn("parse reload_policy failed", "err", err)
				continue
			}
			w.mu.Lock()
			w.externalTools = rp.ExternalTools
			w.mu.Unlock()
			w.logger.Info("reload_policy received", "external_tools", len(rp.ExternalTools))

		case "agent_job":
			var msg protocol.AgentJobMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				w.logger.Warn("parse agent_job failed", "err", err)
				continue
			}
			go w.handleJob(ctx, msg)

		default:
			w.logger.Debug("unknown message type", "type", env.Type)
		}
	}
}

func (w *Worker) handleJob(ctx context.Context, msg protocol.AgentJobMessage) {
	log := w.logger.With("job_id", msg.JobID)
	log.Info("job received")

	w.mu.Lock()
	externalTools := w.externalTools
	toolClient := w.toolClient
	w.mu.Unlock()

	w.gitagent.RefreshMemory()

	// Collect all available tool names for the system prompt.
	toolNames := localToolNames()
	for _, t := range externalTools {
		toolNames = append(toolNames, t.Name)
	}
	msg.SystemPrompt = w.gitagent.BuildSystemPrompt(msg.SystemPrompt, toolNames)

	conn := w.currentConn()
	if conn == nil {
		log.Error("no connection, dropping job")
		return
	}

	tokenCh := make(chan string, 64)
	toolEventCh := make(chan ToolEvent, 16)
	errCh := make(chan error, 1)

	loop := NewAgenticLoop(w.cfg.ServerURL, w.cfg.AgentKey, w.cfg.Dir, w.gitagent, externalTools, toolClient, w.logger)

	go func() {
		defer close(tokenCh)
		defer close(toolEventCh)
		errCh <- loop.Run(ctx, msg, tokenCh, toolEventCh)
	}()

	for tokenCh != nil || toolEventCh != nil {
		select {
		case tok, ok := <-tokenCh:
			if !ok {
				tokenCh = nil
				continue
			}
			_ = conn.WriteJSON(protocol.AgentTokenMessage{
				Type:  protocol.TypeAgentToken,
				JobID: msg.JobID,
				Token: tok,
			})

		case ev, ok := <-toolEventCh:
			if !ok {
				toolEventCh = nil
				continue
			}
			if ev.IsResult {
				log.Debug("sending agent_tool_result", "tool_call_id", ev.ID)
				if err := conn.WriteJSON(protocol.AgentToolResultMessage{
					Type:       protocol.TypeAgentToolResult,
					JobID:      msg.JobID,
					ToolCallID: ev.ID,
					Content:    ev.Content,
					IsError:    ev.IsError,
				}); err != nil {
					log.Warn("failed to send agent_tool_result", "err", err)
				}
			} else {
				log.Debug("sending agent_tool_call", "tool", ev.Name, "id", ev.ID)
				if err := conn.WriteJSON(protocol.AgentToolCallMessage{
					Type:      protocol.TypeAgentToolCall,
					JobID:     msg.JobID,
					ID:        ev.ID,
					Name:      ev.Name,
					Arguments: ev.Arguments,
				}); err != nil {
					log.Warn("failed to send agent_tool_call", "err", err)
				}
			}

		case <-ctx.Done():
			log.Info("job cancelled")
			return
		}
	}

	// Both channels closed — loop.Run returned. Check result.
	if err := <-errCh; err != nil {
		log.Error("job failed", "err", err)
		_ = conn.WriteJSON(protocol.AgentErrorMessage{
			Type:  protocol.TypeAgentError,
			JobID: msg.JobID,
			Error: err.Error(),
		})
		return
	}
	_ = conn.WriteJSON(protocol.AgentCompleteMessage{
		Type:       protocol.TypeAgentComplete,
		JobID:      msg.JobID,
		StopReason: "end_turn",
	})
	log.Info("job complete")
}

func (w *Worker) currentConn() *websocket.Conn {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn
}

// validateDir checks that the gitagent directory contains SOUL.md and agent.yaml.
func (w *Worker) validateDir() error {
	for _, f := range []string{"SOUL.md", "agent.yaml"} {
		path := filepath.Join(w.cfg.Dir, f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return fmt.Errorf("missing required file: %s", f)
		}
	}
	return nil
}

// toHTTPURL converts a normalised ws/wss URL to an http/https base URL for
// REST API calls. The WebSocket path (/ws) is stripped so callers can append
// their own API paths (e.g. /api/tools/v1/execute).
func toHTTPURL(wsURL string) string {
	u, err := url.Parse(wsURL)
	if err != nil {
		r := strings.NewReplacer("wss://", "https://", "ws://", "http://")
		return r.Replace(wsURL)
	}
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	r := strings.NewReplacer("wss://", "https://", "ws://", "http://")
	return r.Replace(u.String())
}

// localToolNames returns the names of locally-executed coding tools.
func localToolNames() []string {
	return []string{"Read", "Write", "Edit", "Bash", "useSkill", "saveMemory"}
}

// soulDigest returns a simple fingerprint of SOUL.md for the hello metadata.
func (w *Worker) soulDigest() string {
	path := filepath.Join(w.cfg.Dir, "SOUL.md")
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("mtime:%d,size:%d", info.ModTime().Unix(), info.Size())
}
