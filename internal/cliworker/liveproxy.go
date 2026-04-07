package cliworker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// toolCallResult carries the server's response to a live tool call request.
type toolCallResult struct {
	Result  string
	IsError bool
	Err     error
}

// liveToolProxy is a local HTTP server started by the cliworker for live-MCP mode.
// The MCP subprocess (auxot-worker --mcp-server-live) calls POST /tool-call when
// Claude invokes a tool. The proxy forwards the call to the main worker's onToolCall
// callback (which sends it to the server via WebSocket), then blocks until the result
// is available and streams it back to the MCP subprocess.
type liveToolProxy struct {
	server    *http.Server
	addr      string
	pending   sync.Map // callID → chan toolCallResult
	log       *slog.Logger
}

// startLiveToolProxy binds to a random localhost port, starts the HTTP proxy,
// and returns its address (e.g. "http://127.0.0.1:12345"). The proxy calls
// onToolCall for every tool execution request from the MCP subprocess.
//
// onToolCall is the bridge between the proxy and the WebSocket connection:
// it sends TypeJobToolCallRequest to the server and blocks until the result
// arrives (via Connection.OnToolCallResult → DeliverToolResult).
//
// The proxy shuts down automatically when ctx is cancelled.
func startLiveToolProxy(
	ctx context.Context,
	onToolCall func(jobID, callID, toolName, arguments string) (result string, isError bool, err error),
) (*liveToolProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("binding live tool proxy: %w", err)
	}

	p := &liveToolProxy{
		addr: "http://" + ln.Addr().String(),
		log:  slog.Default(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/tool-call", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			JobID     string `json:"job_id"`
			CallID    string `json:"call_id"`
			ToolName  string `json:"tool_name"`
			Arguments string `json:"arguments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			p.log.Error("liveproxy: decode request", "err", err)
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		p.log.Info("liveproxy: tool call received",
			"job_id", req.JobID,
			"call_id", req.CallID,
			"tool", req.ToolName)

		// Call the bridge callback — blocks until server delivers the result.
		result, isError, callErr := onToolCall(req.JobID, req.CallID, req.ToolName, req.Arguments)

		w.Header().Set("Content-Type", "application/json")
		if callErr != nil {
			p.log.Error("liveproxy: tool call error", "err", callErr)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": callErr.Error(),
			})
			return
		}

		p.log.Info("liveproxy: tool call result ready",
			"call_id", req.CallID,
			"is_error", isError,
			"result_len", len(result))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result":   result,
			"is_error": isError,
		})
	})

	p.server = &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Minute, // tools can take a while
		WriteTimeout: 10 * time.Minute,
	}

	go func() {
		if err := p.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			p.log.Error("liveproxy: server error", "err", err)
		}
	}()

	// Shut down the proxy when the job context is cancelled.
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.server.Shutdown(shutCtx)
	}()

	return p, nil
}

// Addr returns the base HTTP address of the proxy (e.g. "http://127.0.0.1:12345").
func (p *liveToolProxy) Addr() string {
	return p.addr
}
