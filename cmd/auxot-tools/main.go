// Command auxot-tools is the tools worker binary.
//
// It connects to auxot-router via WebSocket, authenticates with a GPU key,
// and announces itself as a tools worker. The router then dispatches tool call
// jobs to it — the tools worker executes them and returns results.
//
// This is the server-side equivalent of the browser-tools approach, but without
// CORS restrictions and without requiring a browser tab to be open.
//
// Lifecycle:
//  1. Connect to router → authenticate → announce tools capabilities
//  2. Receive ToolJobMessage from router
//  3. Execute the named tool (code_executor, web_fetch, etc.)
//  4. Return ToolResultMessage to router
//  5. Repeat until context cancelled
//
// On disconnect: reconnect with exponential backoff.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/auxothq/auxot/internal/tools"
	pkgtools "github.com/auxothq/auxot/pkg/tools"
	"github.com/auxothq/auxot/pkg/logutil"
)

func main() {
	_ = godotenv.Load()

	// Parse minimal CLI args.
	args := os.Args[1:]
	for _, arg := range args {
		switch arg {
		case "version":
			fmt.Println("auxot-tools v0.1.0")
			return
		case "help", "--help", "-h":
			printHelp()
			return
		}
	}

	logger := slog.New(slog.NewJSONHandler(
		logutil.Output(os.Stderr),
		&slog.HandlerOptions{Level: slogLevel()},
	))
	slog.SetDefault(logger)

	cfg, err := tools.LoadConfig()
	if err != nil {
		logger.Error("configuration error", "error", err.Error())
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, logger); err != nil && err != context.Canceled {
		logger.Error("fatal", "error", err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *tools.Config, logger *slog.Logger) error {
	// Build the tool registry. Start with all built-in tools, then discover
	// any shell tools from ./tools/ directory.
	registry := pkgtools.DefaultRegistry()
	pkgtools.LoadShellToolsIntoRegistry(registry)

	if len(cfg.AllowedTools) > 0 {
		allowed := make(map[string]bool, len(cfg.AllowedTools))
		for _, name := range cfg.AllowedTools {
			allowed[name] = true
		}
		filtered := pkgtools.NewRegistry()
		for _, name := range registry.Names() {
			if allowed[name] {
				exec, _ := registry.Executor(name)
				filtered.Register(name, exec)
			}
		}
		registry = filtered
	}

	logger.Info("auxot-tools starting",
		"version", "0.1.0",
		"router_url", cfg.RouterURL,
		"tools", registry.Names(),
	)

	worker := tools.NewWorker(cfg, registry, logger)
	return worker.Run(ctx)
}

func printHelp() {
	fmt.Println(`auxot-tools — Tool executor worker for auxot-router

Usage:
  auxot-tools           Connect to router and start executing tool calls
  auxot-tools version   Print version
  auxot-tools help      Print this help

Environment Variables:
  AUXOT_ROUTER_URL        Router WebSocket URL (default: wss://auxot.com)
  AUXOT_TOOLS_KEY         Authentication key (required; tool connector key from router setup)
  AUXOT_GPU_KEY           Legacy alias for AUXOT_TOOLS_KEY
  AUXOT_ALLOWED_TOOLS     Comma-separated list of built-in tools to enable (default: all)
                          Built-in tools: code_executor, web_fetch, web_search
  BRAVE_API_KEY           Brave Search API key (required for web_search tool)
  AUXOT_TOOL_<KEY>=value  Pre-set a credential; the prefix is stripped before passing to
                          child processes (e.g. AUXOT_TOOL_BRAVE_API_KEY=xxx)
  AUXOT_HEARTBEAT_INTERVAL  Heartbeat interval (default: 30s)
  AUXOT_LOG_LEVEL         Log level: debug, info, warn, error (default: info)

  For MCP credential injection:
  AUXOT_TOOL_{SERVER}_{ENV_VAR}=value
    Example: AUXOT_TOOL_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN=ghp_xxx
             AUXOT_TOOL_BRAVE_BRAVE_API_KEY=sk_xxx

Built-in Tools:
  code_executor     Execute sandboxed JavaScript via goja (no network/filesystem)
  web_fetch         Fetch URLs via Go's net/http (no CORS restrictions)
  web_search        Search the web via Brave Search API (requires BRAVE_API_KEY)

MCP Tools:
  MCP server packages are pushed to this worker via reload_policy messages from
  the router. Each MCP tool call spawns a fresh "bun x @package@version" process
  with user credentials injected as environment variables. Bun must be installed
  on the system (Docker images ship bun from oven/bun:1).

Shell Tools:
  Drop executable *.sh files + companion *.tool.json files into ./tools/ and they
  are auto-discovered at startup. The script receives tool arguments as JSON on
  stdin and must write a JSON result to stdout. Credentials from the job and from
  AUXOT_TOOL_* env vars are injected into the script's environment.`)
}

func slogLevel() slog.Level {
	switch os.Getenv("AUXOT_LOG_LEVEL") {
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
