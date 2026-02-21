// Package tools implements the auxot-tools worker — a Go binary that connects
// to auxot-router via WebSocket, receives tool call jobs, and executes them.
//
// It is the tools equivalent of internal/worker, following the same connection
// pattern: connect → authenticate → receive jobs → return results.
package tools

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Config holds all runtime configuration for the tools worker.
// Values are read from environment variables at startup and never re-read.
type Config struct {
	// RouterURL is the WebSocket URL of the auxot-router (or auxot-ws).
	// e.g. "ws://localhost:8080/ws" or "wss://my-router.fly.dev/ws"
	RouterURL string

	// GPUKey is the authentication key for the WebSocket handshake.
	// Despite the name (inherited from the shared key type), tools workers
	// use the same key type as GPU workers — it authenticates any connected node.
	GPUKey string

	// AllowedTools is the set of tool names this worker will advertise and handle.
	// If empty, all built-in tools are enabled.
	AllowedTools []string

	// HeartbeatInterval controls how often the worker sends heartbeats.
	HeartbeatInterval time.Duration

	// ReconnectDelay is the initial delay between reconnection attempts.
	// Doubles on each failure up to ReconnectMaxDelay.
	ReconnectDelay time.Duration

	// ReconnectMaxDelay is the upper bound for reconnection backoff.
	ReconnectMaxDelay time.Duration
}

// LoadConfig reads configuration from environment variables.
// Returns an error if required values are missing.
func LoadConfig() (*Config, error) {
	routerURL := os.Getenv("AUXOT_ROUTER_URL")
	if routerURL == "" {
		routerURL = "wss://auxot.com"
	}

	// Normalise: router may be given as http(s):// — convert to ws(s)://
	routerURL = strings.Replace(routerURL, "https://", "wss://", 1)
	routerURL = strings.Replace(routerURL, "http://", "ws://", 1)

	// Support both AUXOT_TOOLS_KEY (preferred) and the legacy AUXOT_GPU_KEY
	// so existing users don't need to update immediately.
	toolsKey := os.Getenv("AUXOT_TOOLS_KEY")
	if toolsKey == "" {
		toolsKey = os.Getenv("AUXOT_GPU_KEY") // legacy fallback
	}
	if toolsKey == "" {
		return nil, fmt.Errorf("AUXOT_TOOLS_KEY is required (tool connector key from auxot-router setup)")
	}

	cfg := &Config{
		RouterURL:         routerURL,
		GPUKey:            toolsKey,
		HeartbeatInterval: 30 * time.Second,
		ReconnectDelay:    2 * time.Second,
		ReconnectMaxDelay: 60 * time.Second,
	}

	// AUXOT_ALLOWED_TOOLS=code_executor,web_fetch,web_search
	if v := os.Getenv("AUXOT_ALLOWED_TOOLS"); v != "" {
		for _, tool := range strings.Split(v, ",") {
			tool = strings.TrimSpace(tool)
			if tool != "" {
				cfg.AllowedTools = append(cfg.AllowedTools, tool)
			}
		}
	}

	return cfg, nil
}
