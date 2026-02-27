// Package worker implements the auxot-worker — the GPU worker binary that
// connects to an auxot-router, receives a model policy, downloads the model,
// spawns llama.cpp, and processes inference jobs.
//
// Worker lifecycle:
//  1. Connect to router → authenticate → receive policy (model, quant, ctx, parallelism)
//  2. Disconnect (download can take hours)
//  3. Download model from HuggingFace (cached to ~/.auxot/models/)
//  4. Download llama.cpp binary from GitHub releases (cached to ~/.auxot/llama-server/)
//  5. Detect GPU hardware (Metal/CUDA/Vulkan/CPU)
//  6. Spawn llama.cpp as a subprocess
//  7. Discover capabilities from running llama.cpp
//  8. Reconnect permanently → send capabilities → process jobs
package worker

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all configuration for the worker.
// The worker does NOT need model configuration upfront — it gets the
// model policy from the router on connect (hello_ack).
type Config struct {
	// Required: Router connection
	RouterURL string // WebSocket URL of the router (e.g., "wss://auxot.com/api/gpu/client" or "ws://localhost:8080/ws")
	AdminKey  string // Plaintext admin key for authentication (adm_...)

	// Optional: Model file override for air-gapped deployments.
	// When set, the worker uses this local GGUF file instead of downloading.
	ModelFile string

	// Optional: Models cache directory (default: ~/.auxot/models)
	ModelsDir string

	// Optional: llama.cpp cache directory (default: ~/.auxot/llama-server)
	LlamaCacheDir string

	// Optional: llama.cpp binary path override (skip download)
	LlamaBinaryPath string

	// Optional: External llama-server URL (skip model download, binary download,
	// and process spawning entirely). Use this for dev when llama.cpp is already
	// running. e.g. "http://localhost:9002". Set via AUXOT_LLAMA_URL.
	LlamaURL string

	// Timing
	HeartbeatInterval time.Duration // How often to send heartbeats (default: 10s)
	ReconnectDelay    time.Duration // Initial delay before reconnecting (default: 2s)
	ReconnectMaxDelay time.Duration // Max reconnect backoff (default: 60s)
	JobTimeout        time.Duration // Max time for a single job (default: 30m)

	// Threads / GPU
	Threads   int // llama.cpp --threads (default: auto)
	GPULayers int // llama.cpp --n-gpu-layers (default: 9999 = offload all)
}

// CLIFlags holds command-line flag values that override environment variables.
// Empty strings are ignored (the env var value is kept).
type CLIFlags struct {
	RouterURL       string
	GPUKey          string
	ModelPath       string // Path to a local GGUF model file (air-gapped)
	LlamaServerPath string // Path to a local llama-server binary (air-gapped)
}

// LoadConfig reads worker configuration from environment variables.
// CLI flag values override env vars when non-empty.
func LoadConfig(flags CLIFlags) (*Config, error) {
	cfg := &Config{
		RouterURL:         envStr("AUXOT_ROUTER_URL", "wss://auxot.com/api/gpu/client"),
		AdminKey:          os.Getenv("AUXOT_GPU_KEY"),
		ModelFile:         os.Getenv("AUXOT_MODEL_FILE"),
		ModelsDir:         os.Getenv("AUXOT_MODELS_DIR"),
		LlamaCacheDir:     os.Getenv("AUXOT_LLAMA_CACHE_DIR"),
		LlamaBinaryPath:   os.Getenv("AUXOT_LLAMA_BINARY"),
		LlamaURL:          os.Getenv("AUXOT_LLAMA_URL"),
		HeartbeatInterval: envDuration("AUXOT_HEARTBEAT_INTERVAL", 10*time.Second),
		ReconnectDelay:    envDuration("AUXOT_RECONNECT_DELAY", 2*time.Second),
		ReconnectMaxDelay: envDuration("AUXOT_RECONNECT_MAX_DELAY", 60*time.Second),
		JobTimeout:        envDuration("AUXOT_JOB_TIMEOUT", 30*time.Minute),
		Threads:           envInt("AUXOT_THREADS", 0),
		GPULayers:         envInt("AUXOT_GPU_LAYERS", 9999),
	}

	// CLI flags override env vars
	if flags.RouterURL != "" {
		cfg.RouterURL = flags.RouterURL
	}
	if flags.GPUKey != "" {
		cfg.AdminKey = flags.GPUKey
	}
	if flags.ModelPath != "" {
		cfg.ModelFile = flags.ModelPath
	}
	if flags.LlamaServerPath != "" {
		cfg.LlamaBinaryPath = flags.LlamaServerPath
	}

	if cfg.AdminKey == "" {
		return nil, fmt.Errorf("GPU key is required: use --gpu-key flag or set AUXOT_GPU_KEY")
	}

	return cfg, nil
}

func envStr(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func envInt(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}
	return n
}

func envDuration(key string, defaultVal time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return defaultVal
	}
	return d
}
