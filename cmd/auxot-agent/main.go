// Command auxot-agent is the agent worker binary.
//
// It connects to an Auxot server via WebSocket, authenticates with an agent key,
// and executes chat jobs by spawning Claude Code in the gitagent directory.
//
// The gitagent directory must contain:
//   - SOUL.md  — agent identity, personality, and mission
//   - agent.yaml — agent configuration
//
// On startup:
//  1. Validate the gitagent directory
//  2. Connect to the server via WebSocket at /ws
//  3. Send hello with agent key and metadata
//  4. Receive hello_ack with system prompt
//  5. Enter job loop: for each agent_job, spawn claude, stream tokens back
//
// Reconnects automatically with exponential backoff on disconnect.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/auxothq/auxot/internal/agentworker"
	"github.com/auxothq/auxot/pkg/logutil"
	"github.com/auxothq/auxot/pkg/routerurl"
)

func main() {
	// Defaults from environment.
	routerDefault := os.Getenv("AUXOT_ROUTER_URL")
	if routerDefault == "" {
		routerDefault = os.Getenv("AUXOT_SERVER_URL") // legacy compat
	}
	agentKeyDefault := os.Getenv("AUXOT_AGENT_KEY")
	dirDefault := os.Getenv("AUXOT_AGENT_DIR")
	if dirDefault == "" {
		var err error
		if dirDefault, err = os.Getwd(); err != nil {
			dirDefault = "."
		}
	}
	logLevelDefault := os.Getenv("AUXOT_LOG_LEVEL")
	if logLevelDefault == "" {
		logLevelDefault = "info"
	}

	var routerURL, serverURL, agentKey, dir, logLevel string
	var debug bool
	flag.StringVar(&routerURL, "router-url", routerDefault, "Auxot router URL (overrides AUXOT_ROUTER_URL)")
	flag.StringVar(&serverURL, "server", "", "Deprecated: use --router-url instead")
	flag.StringVar(&agentKey, "agent-key", agentKeyDefault, "Agent key (overrides AUXOT_AGENT_KEY)")
	flag.StringVar(&dir, "dir", dirDefault, "Gitagent directory containing SOUL.md and agent.yaml (overrides AUXOT_AGENT_DIR)")
	flag.StringVar(&logLevel, "log-level", logLevelDefault, "Log level: debug, info, warn, error")
	flag.BoolVar(&debug, "debug", false, "Enable debug logging (shorthand for --log-level debug)")

	flag.Parse()

	if debug {
		logLevel = "debug"
	}

	// --server is the legacy alias for --router-url
	if routerURL == "" && serverURL != "" {
		routerURL = serverURL
	}

	if agentKey == "" {
		fmt.Fprintln(os.Stderr, "error: --agent-key (or AUXOT_AGENT_KEY) is required")
		os.Exit(1)
	}
	if routerURL == "" {
		fmt.Fprintln(os.Stderr, "error: --router-url (or AUXOT_ROUTER_URL) is required")
		os.Exit(1)
	}

	normalizedURL, err := routerurl.Normalize(routerURL, "/ws")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: AUXOT_ROUTER_URL: %v\n", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(
		logutil.Output(os.Stderr),
		&slog.HandlerOptions{Level: slogLevel(logLevel)},
	))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	worker := agentworker.New(agentworker.Config{
		ServerURL: normalizedURL,
		AgentKey:  agentKey,
		Dir:       dir,
	}, logger)

	if err := worker.Run(ctx); err != nil {
		logger.Error("agent worker exited with error", "err", err)
		os.Exit(1)
	}
}

func slogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
