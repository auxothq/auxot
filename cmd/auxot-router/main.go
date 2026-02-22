// Command auxot-router is the OSS GPU inference router.
//
// It accepts OpenAI-compatible API requests, routes them to GPU workers
// running llama.cpp via WebSocket connections, and streams tokens back
// to callers using Redis Streams.
//
// Usage:
//
//	# Start the router (requires AUXOT_REDIS_URL, AUXOT_ADMIN_KEY_HASH, etc.)
//	auxot-router
//
//	# Generate keys for initial setup
//	auxot-router setup
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/joho/godotenv"

	"github.com/auxothq/auxot/internal/router"
	"github.com/auxothq/auxot/pkg/auth"
	"github.com/auxothq/auxot/pkg/logutil"
	"github.com/auxothq/auxot/pkg/registry"
)

func main() {
	// Load .env if present (silently ignore if missing).
	// Environment variables already set take precedence over .env values.
	_ = godotenv.Load()

	logger := slog.New(slog.NewJSONHandler(
		logutil.Output(os.Stderr),
		&slog.HandlerOptions{Level: slogLevel()},
	))

	// Check for subcommands
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			writeEnv := false
			flySecrets := false
			newToolsKey := false
			model := ""
			for i, arg := range os.Args[2:] {
				switch arg {
				case "--write-env":
					writeEnv = true
				case "--fly":
					flySecrets = true
				case "--new-tools-key":
					newToolsKey = true
				case "--model":
					if i+1 < len(os.Args[2:]) {
						model = os.Args[2+i+1]
					}
				}
			}
			if newToolsKey {
				runNewToolsKey()
				return
			}
			runSetup(writeEnv, flySecrets, model)
			return
		case "models":
			runModels()
			return
		case "version":
			fmt.Println("auxot-router v0.1.0")
			return
		case "help", "--help", "-h":
			printHelp()
			return
		}
	}

	// --- Main server ---
	cfg, err := router.LoadConfig()
	if err != nil {
		logger.Error("configuration error", "error", err.Error())
		os.Exit(1)
	}

	// Start embedded miniredis if no REDIS_URL provided
	var miniRedis *miniredis.Miniredis
	if cfg.RedisURL == "" {
		var err error
		miniRedis, err = miniredis.Run()
		if err != nil {
			logger.Error("failed to start embedded redis", "error", err)
			os.Exit(1)
		}
		cfg.RedisURL = "redis://" + miniRedis.Addr()
		cfg.EmbeddedRedis = true
		cfg.EmbeddedRedisAddr = miniRedis.Addr()
		logger.Info("started embedded redis", "addr", miniRedis.Addr())
	}
	defer func() {
		if miniRedis != nil {
			miniRedis.Close()
		}
	}()

	// Miniredis TTLs don't decrease automatically — we must advance time
	// ourselves so heartbeat keys expire and the sweeper detects dead workers.
	if miniRedis != nil {
		go func() {
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				miniRedis.FastForward(1 * time.Second)
			}
		}()
	}

	srv, err := router.NewServer(cfg, logger)
	if err != nil {
		logger.Error("server initialization failed", "error", err)
		os.Exit(1)
	}

	// Handle signals for graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := srv.Start(ctx); err != nil {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
}

// runSetup generates admin and API keys and prints the required environment variables.
//
// Modes:
//   - Default:       Prints configuration and plaintext keys to stdout
//   - --write-env:   Writes configuration to .env file (refuses if exists)
//   - --fly:         Outputs a single `fly secrets set` command for Fly.io deployment
//   - --model NAME:  Sets the model name (default: Qwen3-Coder-30B-A3B)
func runSetup(writeEnv, flySecrets bool, model string) {
	if writeEnv {
		if _, err := os.Stat(".env"); err == nil {
			fmt.Fprintln(os.Stderr, "Error: .env already exists. Remove it first or run setup without --write-env.")
			os.Exit(1)
		}
	}

	if model == "" {
		model = "Qwen3-Coder-30B-A3B"
	}

	adminKey, err := auth.GenerateAdminKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating admin key: %v\n", err)
		os.Exit(1)
	}

	apiKey, err := auth.GenerateAPIKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating API key: %v\n", err)
		os.Exit(1)
	}

	toolsKey, err := auth.GenerateToolKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating tools key: %v\n", err)
		os.Exit(1)
	}

	// --fly: Output a single `fly secrets set` command and key instructions
	if flySecrets {
		fmt.Println("# Auxot Router — Fly.io Setup")
		fmt.Println("#")
		fmt.Println("# SAVE THESE KEYS NOW — they will NOT be shown again.")
		fmt.Println("#")
		fmt.Println("# GPU Key (give to GPU workers running auxot-worker):")
		fmt.Printf("#   %s\n", adminKey.Key)
		fmt.Println("#")
		fmt.Println("# Tools Key (give to tools nodes running auxot-tools):")
		fmt.Printf("#   %s\n", toolsKey.Key)
		fmt.Println("#")
		fmt.Println("# API Key (give to API callers):")
		fmt.Printf("#   %s\n", apiKey.Key)
		fmt.Println("#")
		fmt.Println("# Run this command to set your Fly.io secrets:")
		fmt.Println()
		fmt.Printf("fly secrets set \\\n")
		fmt.Printf("  AUXOT_ADMIN_KEY_HASH='%s' \\\n", adminKey.Hash)
		fmt.Printf("  AUXOT_API_KEY_HASH='%s' \\\n", apiKey.Hash)
		fmt.Printf("  AUXOT_TOOLS_KEY_HASH='%s' \\\n", toolsKey.Hash)
		fmt.Printf("  AUXOT_MODEL='%s'\n", model)
		fmt.Println()
		fmt.Println("# Then deploy:")
		fmt.Println("#   fly deploy")
		return
	}

	// Default and --write-env modes
	fmt.Println("auxot-router setup")
	fmt.Println("==================")
	fmt.Println()
	fmt.Println("Generating keys...")
	fmt.Println()

	fmt.Println("=== GPU KEY (for auxot-worker) ===")
	fmt.Println("Give this key to GPU operators to connect auxot-worker:")
	fmt.Printf("  %s\n", adminKey.Key)
	fmt.Println()
	fmt.Printf("  Use it with:  AUXOT_GPU_KEY=%s auxot-worker\n", adminKey.Key)
	fmt.Println()

	existingToolsHash := os.Getenv("AUXOT_TOOLS_KEY_HASH")
	if existingToolsHash != "" {
		fmt.Println("=== TOOLS KEY (for auxot-tools) ===")
		fmt.Println("An existing tools key is already configured (AUXOT_TOOLS_KEY_HASH is set).")
		fmt.Println("To rotate it, run: auxot-router setup --new-tools-key")
		fmt.Println()
	} else {
		fmt.Println("=== TOOLS KEY (for auxot-tools) ===")
		fmt.Println("Give this key to tools nodes to connect auxot-tools:")
		fmt.Printf("  %s\n", toolsKey.Key)
		fmt.Println()
		fmt.Printf("  Use it with:  AUXOT_TOOLS_KEY=%s auxot-tools\n", toolsKey.Key)
		fmt.Println()
	}

	fmt.Println("=== API KEY (for callers) ===")
	fmt.Println("Give this key to applications calling /v1/chat/completions:")
	fmt.Printf("  %s\n", apiKey.Key)
	fmt.Println()

	fmt.Println("=== SAVE THESE KEYS NOW ===")
	fmt.Println("The plaintext keys above will NOT be shown again.")
	fmt.Println()

	toolsHashLine := fmt.Sprintf("AUXOT_TOOLS_KEY_HASH='%s'", toolsKey.Hash)
	if existingToolsHash != "" {
		// Don't overwrite existing tools key hash — user must run --new-tools-key explicitly
		toolsHashLine = fmt.Sprintf("# AUXOT_TOOLS_KEY_HASH=<existing>  # Run setup --new-tools-key to rotate")
	}
	envContent := fmt.Sprintf(
		"AUXOT_ADMIN_KEY_HASH='%s'\nAUXOT_API_KEY_HASH='%s'\n%s\nAUXOT_MODEL=%s\n# AUXOT_REDIS_URL=redis://localhost:6379  # Optional: uses embedded Redis if not set\n# AUXOT_ALLOWED_TOOLS=code_executor,web_fetch  # Optional: inject built-in tools into every LLM call\n",
		adminKey.Hash, apiKey.Hash, toolsHashLine, model,
	)

	if writeEnv {
		if err := os.WriteFile(".env", []byte(envContent), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing .env: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("✓ Wrote .env (mode 0600)")
		fmt.Println()
	} else {
		fmt.Println("=== .env FILE ===")
		fmt.Println("Copy this into your .env file (or re-run with --write-env):")
		fmt.Println()
		fmt.Print(envContent)
		fmt.Println()
	}
}

// runNewToolsKey generates a single new tool connector key and prints what to update.
// This is useful for key rotation — the existing GPU and API keys are left untouched.
func runNewToolsKey() {
	toolsKey, err := auth.GenerateToolKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating tools key: %v\n", err)
		os.Exit(1)
	}

	existingHash := os.Getenv("AUXOT_TOOLS_KEY_HASH")

	fmt.Println("auxot-router setup --new-tools-key")
	fmt.Println("====================================")
	fmt.Println()

	if existingHash != "" {
		fmt.Println("Current AUXOT_TOOLS_KEY_HASH is set — replacing it with a new key.")
		fmt.Println("Connected auxot-tools nodes will need to reconnect with the new key.")
	} else {
		fmt.Println("No AUXOT_TOOLS_KEY_HASH was set — generating your first tools key.")
	}

	fmt.Println()
	fmt.Println("=== NEW TOOLS KEY (for auxot-tools) ===")
	fmt.Println("Give this key to tools nodes to connect auxot-tools:")
	fmt.Printf("  %s\n", toolsKey.Key)
	fmt.Println()
	fmt.Printf("  Use it with:  AUXOT_TOOLS_KEY=%s auxot-tools\n", toolsKey.Key)
	fmt.Println()
	fmt.Println("=== SAVE THIS KEY NOW ===")
	fmt.Println("The plaintext key above will NOT be shown again.")
	fmt.Println()
	fmt.Println("Update your configuration with:")
	fmt.Printf("  AUXOT_TOOLS_KEY_HASH='%s'\n", toolsKey.Hash)
	fmt.Println()
}

// runModels prints all available models from the embedded registry.
func runModels() {
	reg, err := registry.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading registry: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("auxot model registry (v%s, %d entries)\n", reg.Version, len(reg.Models))
	fmt.Println()

	// Group by model name
	type modelGroup struct {
		name       string
		quants     []string
		vrams      []float64
		family     string
		params     string
		maxCtxSize int
	}

	groups := make(map[string]*modelGroup)
	var order []string

	for _, m := range reg.Models {
		g, exists := groups[m.ModelName]
		if !exists {
			g = &modelGroup{
				name:       m.ModelName,
				family:     m.Family,
				params:     m.Parameters,
				maxCtxSize: m.MaxContextSize,
			}
			groups[m.ModelName] = g
			order = append(order, m.ModelName)
		}
		g.quants = append(g.quants, m.Quantization)
		g.vrams = append(g.vrams, m.VRAMRequirementsGB)
	}

	for _, name := range order {
		g := groups[name]
		// Find min VRAM (skip F16)
		minVRAM := 0.0
		for i, q := range g.quants {
			if q != "F16" && (minVRAM == 0 || g.vrams[i] < minVRAM) {
				minVRAM = g.vrams[i]
			}
		}
		quantList := ""
		for i, q := range g.quants {
			if i > 0 {
				quantList += ", "
			}
			quantList += q
		}
		fmt.Printf("  %-50s %s  %s  ctx %-6s  min %.0fGB  [%s]\n",
			name, g.family, g.params, formatCtxSize(g.maxCtxSize), minVRAM, quantList)
	}

	fmt.Println()
	fmt.Println("Usage: AUXOT_MODEL=<model-name>")
	fmt.Println("       AUXOT_QUANTIZATION=<quant>  (optional — auto-selects if omitted)")
}

// formatCtxSize formats a context size as a human-readable string (e.g. 128K, 1M).
func formatCtxSize(size int) string {
	if size >= 1000000 && size%1000000 == 0 {
		return fmt.Sprintf("%dM", size/1000000)
	}
	if size >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(size)/1000000)
	}
	if size >= 1024 && size%1024 == 0 {
		return fmt.Sprintf("%dK", size/1024)
	}
	return fmt.Sprintf("%dK", size/1000)
}

// printHelp prints usage information.
func printHelp() {
	fmt.Println(`auxot-router — open-source GPU inference router

Usage:
  auxot-router                       Start the router server
  auxot-router setup                 Generate keys and print configuration
  auxot-router setup --write-env     Write keys to .env file
  auxot-router setup --fly           Output "fly secrets set" command for Fly.io
  auxot-router setup --model NAME    Use a specific model (default: Qwen3-Coder-30B-A3B)
  auxot-router setup --new-tools-key Generate a new tools connector key (for key rotation)
  auxot-router models                List all available models
  auxot-router version               Print version
  auxot-router help                  Print this help

Environment Variables:
  AUXOT_MODEL                  Model name (default: Qwen3-Coder-30B-A3B)
  AUXOT_ADMIN_KEY_HASH         Argon2id hash of the GPU key (required)
  AUXOT_API_KEY_HASH           Argon2id hash of the API key (required)
  AUXOT_TOOLS_KEY_HASH         Argon2id hash of the tools connector key (optional)
  AUXOT_ALLOWED_TOOLS          Comma-separated built-in tools to inject (optional, e.g. code_executor,web_fetch)
  AUXOT_QUANTIZATION           Quantization (default: Q4_K_S — auto-selects if omitted)
  AUXOT_CTX_SIZE               Context window size (default: 131072 / 128K)
  AUXOT_MAX_PARALLEL           Max concurrent jobs per GPU (default: 2)
  AUXOT_PORT                   HTTP listen port (default: 8080)
  AUXOT_HOST                   Bind address (default: 0.0.0.0)
  AUXOT_LOG_LEVEL              Log level: debug, info, warn, error (default: info)
  AUXOT_JOB_TIMEOUT            Max job duration (default: 5m)
  AUXOT_HEARTBEAT_INTERVAL     Worker heartbeat interval (default: 15s)
  AUXOT_DEAD_WORKER_TIMEOUT    Dead worker threshold (default: 45s)
  AUXOT_AUTH_CACHE_TTL         Key verification cache TTL (default: 5m)
  AUXOT_REDIS_URL              Redis URL (optional — uses embedded in-memory Redis if not set)
  AUXOT_REGISTRY_FILE          Path to override embedded model registry

Tool Credentials:
  AUXOT_TOOLS_{TOOL_NAME}__{VAR_NAME}=value
    Store per-tool credentials on the router. The router injects the relevant
    credential map into each tool job message — the tools worker never needs
    secrets set as local env vars.

    Tool name uses underscores (uppercase); variable name is the bare env var name.
    A double-underscore (__) separates the tool name from the variable name.

    Examples:
      AUXOT_TOOLS_WEB_SEARCH__BRAVE_SEARCH_API_KEY=xxx
      AUXOT_TOOLS_WEB_ANSWERS__BRAVE_ANSWERS_API_KEY=xxx
      AUXOT_TOOLS_GITHUB__GITHUB_PERSONAL_ACCESS_TOKEN=ghp_xxx

More information: https://github.com/auxothq/auxot`)
}

// slogLevel reads AUXOT_LOG_LEVEL from the environment.
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
