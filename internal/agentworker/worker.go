// Package agentworker implements the agent worker that connects to an Auxot
// server as a context-provider agent. It reads SOUL.md and agent.yaml from
// the gitagent directory, authenticates with an agent key, and:
//
//  1. Sends system_prompt (SOUL.md content) and local tool definitions on hello.
//  2. Listens for tool.execute messages from the server.
//  3. Executes local coding tools (Read, Write, Edit, Bash, …) in the gitagent
//     directory and returns results via tool.result.
//
// The server runs LLM inference on behalf of the agent using the provided
// system_prompt and tool definitions — the agent binary does NOT call any
// inference API itself.
package agentworker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/auxothq/auxot/pkg/codingtools"
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

// Worker manages the WebSocket connection to the Auxot server.
// It is a context provider: it sends its identity (system_prompt, local_tools)
// on hello and executes local tool calls dispatched by the server.
type Worker struct {
	cfg     Config
	logger  *slog.Logger
	mu      sync.Mutex
	conn    *websocket.Conn
	writeMu sync.Mutex // guards all conn.WriteJSON calls

	gitagent *GitAgent
}

func (w *Worker) writeJSON(v any) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	c := w.currentConn()
	if c == nil {
		return fmt.Errorf("no connection")
	}
	return c.WriteJSON(v)
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
		} else {
			backoff = 1 * time.Second
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

	// Build system prompt from SOUL.md + RULES.md + memory etc.
	// Pass empty auxotContext — the server enriches it with skills and knowledge files.
	systemPrompt := ga.BuildSystemPrompt("", localToolNames())

	// Build local tool definitions from the codingtools package.
	localTools := buildLocalToolDefs()

	// Send hello with system_prompt and local_tools.
	hello := protocol.AgentHelloMessage{
		Type:         protocol.TypeHello,
		WorkerType:   "agent",
		AgentKey:     w.cfg.AgentKey,
		SystemPrompt: systemPrompt,
		LocalTools:   localTools,
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

	w.logger.Info("connected to server", "agent_id", ack.AgentID)

	// Heartbeat keeps the connection alive.
	heartbeatStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeatStop:
				return
			case <-ticker.C:
				if err := w.writeJSON(map[string]string{"type": "heartbeat"}); err != nil {
					w.logger.Debug("heartbeat write failed", "err", err)
					return
				}
			}
		}
	}()
	defer close(heartbeatStop)

	// Message loop — context provider model:
	// - handle tool.execute from server (dispatch local tool, return result)
	// - handle context_update (re-read SOUL.md and send updated context)
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			w.logger.Warn("unmarshal envelope failed", "err", err)
			continue
		}

		switch env.Type {
		case "heartbeat_ack":
			// nothing to do

		case string(protocol.TypeAgentToolExecute): // "tool.execute"
			var msg protocol.AgentToolExecuteMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				w.logger.Warn("parse tool.execute failed", "err", err)
				continue
			}
			go w.handleToolExecute(ctx, msg)

		case "reload_policy":
			// Re-read the gitagent directory and push an updated context_update.
			go w.sendContextUpdate()

		default:
			w.logger.Debug("unknown message type", "type", env.Type)
		}
	}
}

// handleToolExecute executes a local coding tool and sends the result back to the server.
func (w *Worker) handleToolExecute(ctx context.Context, msg protocol.AgentToolExecuteMessage) {
	log := w.logger.With("call_id", msg.CallID, "tool", msg.ToolName)
	log.Debug("executing local tool")

	tool := codingtools.FindTool(msg.ToolName)
	if tool == nil {
		log.Warn("unknown tool requested")
		_ = w.writeJSON(protocol.AgentLocalToolResultMessage{
			Type:   protocol.TypeAgentLocalToolResult,
			CallID: msg.CallID,
			Error:  fmt.Sprintf("unknown tool: %s", msg.ToolName),
		})
		return
	}

	result, err := tool.Execute(ctx, w.cfg.Dir, json.RawMessage(msg.Arguments))
	if err != nil {
		log.Warn("tool execution failed", "err", err)
		_ = w.writeJSON(protocol.AgentLocalToolResultMessage{
			Type:   protocol.TypeAgentLocalToolResult,
			CallID: msg.CallID,
			Error:  err.Error(),
		})
		return
	}

	log.Debug("tool executed successfully")
	_ = w.writeJSON(protocol.AgentLocalToolResultMessage{
		Type:   protocol.TypeAgentLocalToolResult,
		CallID: msg.CallID,
		Result: result,
	})
}

// sendContextUpdate re-reads SOUL.md and pushes an updated context to the server.
func (w *Worker) sendContextUpdate() {
	ga, err := LoadGitAgent(w.cfg.Dir)
	if err != nil {
		w.logger.Warn("context_update: reload failed", "err", err)
		return
	}
	w.mu.Lock()
	w.gitagent = ga
	w.mu.Unlock()

	systemPrompt := ga.BuildSystemPrompt("", localToolNames())
	localTools := buildLocalToolDefs()

	type contextUpdateMsg struct {
		Type         string                   `json:"type"`
		SystemPrompt string                   `json:"system_prompt"`
		LocalTools   []protocol.ToolDefinition `json:"local_tools"`
	}
	_ = w.writeJSON(contextUpdateMsg{
		Type:         "context_update",
		SystemPrompt: systemPrompt,
		LocalTools:   localTools,
	})
	w.logger.Info("sent context_update to server")
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

// localToolNames returns the names of locally-executable coding tools.
func localToolNames() []string {
	names := make([]string, 0, len(codingtools.AllTools()))
	for _, t := range codingtools.AllTools() {
		names = append(names, t.Name)
	}
	return names
}

// buildLocalToolDefs converts the codingtools into protocol ToolDefinition records
// for inclusion in the hello message.
func buildLocalToolDefs() []protocol.ToolDefinition {
	tools := codingtools.AllTools()
	defs := make([]protocol.ToolDefinition, len(tools))
	for i, t := range tools {
		defs[i] = protocol.ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		}
	}
	return defs
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
