// Package router implements the auxot-router server — the glue that connects
// API callers to GPU workers via Redis Streams.
package router

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/auxothq/auxot/pkg/registry"
	pkgtools "github.com/auxothq/auxot/pkg/tools"
)

// Config holds all configuration for the router, loaded from environment variables.
type Config struct {
	// Server
	Port int    // HTTP + WS listen port (default: 8080)
	Host string // Bind address (default: "0.0.0.0")

	// Redis
	RedisURL          string // Redis connection URL (empty = start embedded miniredis)
	EmbeddedRedis     bool   // True if using embedded miniredis (set by LoadConfig)
	EmbeddedRedisAddr string // Address of embedded miniredis if started

	// Authentication
	AdminKeyHash string // Argon2id hash of the admin key (for GPU workers, adm_...)
	APIKeyHash   string // Argon2id hash of the API key (for API callers, rtr_...)
	ToolsKeyHash string // Argon2id hash of the tool connector key (for tools workers, tls_...); optional

	// Tools
	// AllowedTools is the list of built-in tool names the router injects into GPU job
	// messages when a tools worker is connected. Empty = no tool injection.
	// Example: AUXOT_ALLOWED_TOOLS=code_executor,web_fetch
	AllowedTools []string

	// ToolCredentials maps tool names to their credential maps.
	// Populated from AUXOT_TOOLS_{TOOL_NAME}__{VAR_NAME} environment variables.
	// Example: AUXOT_TOOLS_WEB_SEARCH__BRAVE_SEARCH_API_KEY=xxx → ToolCredentials["web_search"]["BRAVE_SEARCH_API_KEY"] = "xxx"
	ToolCredentials map[string]map[string]string

	// Model policy — resolved from the registry at startup
	ModelName    string          // Resolved model name (from registry)
	Quantization string          // Resolved quantization (from registry)
	ContextSize  int             // Context window to use (default: 131072 / 128K)
	MaxParallel  int             // Max concurrent jobs per GPU (default: 2)
	ModelEntry   *registry.Model // The full registry entry — nil only in tests

	// Timeouts
	HeartbeatInterval time.Duration // Expected heartbeat interval from workers (default: 15s)
	DeadWorkerTimeout time.Duration // Time before a worker is considered dead (default: 45s)
	JobTimeout        time.Duration // Max time for a single job (default: 5m)

	// Cache
	AuthCacheTTL time.Duration // How long to cache key verification results (default: 5m)

	// MCP
	// MCPExposeLLM controls whether the MCP server exposes a "generate_text" tool
	// that lets MCP clients invoke the router's LLM directly. Disabled by default
	// to avoid unintentional LLM access from tool-only clients.
	// Enable with: AUXOT_MCP_EXPOSE_LLM=true
	MCPExposeLLM bool

	// Registry
	RegistryFile string // Optional path to override the embedded model registry
}

// LoadConfig reads configuration from environment variables and validates the
// model against the embedded (or overridden) model registry.
//
// The router will refuse to start if AUXOT_MODEL does not match a model in the
// registry. This catches typos early and ensures workers can actually download
// the model file.
//
// Model resolution order:
//  1. Exact registry ID match (e.g., AUXOT_MODEL=qwen3-30b-a3b-instruct-2507-q4-k-s)
//  2. Name + explicit AUXOT_QUANTIZATION match
//  3. Name only → auto-select best quantization (Q4_K_S > Q4_K_M > Q5_K_S > first available)
func LoadConfig() (*Config, error) {
	cfg := &Config{
		Port:              envInt("AUXOT_PORT", 8080),
		Host:              envStr("AUXOT_HOST", "0.0.0.0"),
		RedisURL:          os.Getenv("AUXOT_REDIS_URL"), // Empty string = use embedded miniredis
		AdminKeyHash:      os.Getenv("AUXOT_ADMIN_KEY_HASH"),
		APIKeyHash:        os.Getenv("AUXOT_API_KEY_HASH"),
		ToolsKeyHash:      os.Getenv("AUXOT_TOOLS_KEY_HASH"),     // Optional
		AllowedTools:      envStringList("AUXOT_ALLOWED_TOOLS"),  // Optional
		ContextSize:       envInt("AUXOT_CTX_SIZE", 131072), // 128K — good for chat + agentic use
		MaxParallel:       envInt("AUXOT_MAX_PARALLEL", 2),
		HeartbeatInterval: envDuration("AUXOT_HEARTBEAT_INTERVAL", 15*time.Second),
		DeadWorkerTimeout: envDuration("AUXOT_DEAD_WORKER_TIMEOUT", 45*time.Second),
		JobTimeout:        envDuration("AUXOT_JOB_TIMEOUT", 5*time.Minute),
		AuthCacheTTL:      envDuration("AUXOT_AUTH_CACHE_TTL", 5*time.Minute),
		MCPExposeLLM:      os.Getenv("AUXOT_MCP_EXPOSE_LLM") == "true",
		RegistryFile:      os.Getenv("AUXOT_REGISTRY_FILE"),
	}

	// Validate required auth fields
	if cfg.AdminKeyHash == "" {
		return nil, fmt.Errorf("AUXOT_ADMIN_KEY_HASH is required (run auxot-router setup to generate)")
	}
	if cfg.APIKeyHash == "" {
		return nil, fmt.Errorf("AUXOT_API_KEY_HASH is required (run auxot-router setup to generate)")
	}

	// --- Model validation against registry ---
	modelInput := os.Getenv("AUXOT_MODEL")
	if modelInput == "" {
		modelInput = "Qwen3.5-35B-A3B" // Good default: 35B MoE, capable — chat + agentic
	}

	explicitQuant := os.Getenv("AUXOT_QUANTIZATION") // Empty string if not set

	// Load registry (embedded or override file)
	reg, err := loadRegistry(cfg.RegistryFile)
	if err != nil {
		return nil, err
	}

	entry, err := resolveModel(modelInput, explicitQuant, reg)
	if err != nil {
		return nil, err
	}

	cfg.ModelName = entry.ModelName
	cfg.Quantization = entry.Quantization
	cfg.ModelEntry = entry

	// Clamp to model's max context size
	if cfg.ContextSize > entry.MaxContextSize {
		cfg.ContextSize = entry.MaxContextSize
	}

	cfg.ToolCredentials = pkgtools.ParseToolCredentials()

	return cfg, nil
}

// loadRegistry loads the model registry from the embedded data or an override file.
func loadRegistry(registryFile string) (*registry.Registry, error) {
	if registryFile != "" {
		reg, err := registry.LoadFromFile(registryFile)
		if err != nil {
			return nil, fmt.Errorf("loading registry file %q: %w", registryFile, err)
		}
		return reg, nil
	}
	reg, err := registry.Load()
	if err != nil {
		return nil, fmt.Errorf("loading embedded model registry: %w", err)
	}
	return reg, nil
}

// resolveModel finds the correct registry entry given user input.
//
// Resolution order:
//  1. Exact registry ID match
//  2. Name + explicit quantization match
//  3. Name only → auto-select best available quantization
func resolveModel(modelInput, explicitQuant string, reg *registry.Registry) (*registry.Model, error) {
	// 1. Exact ID match (e.g., "qwen3-30b-a3b-instruct-2507-q4-k-s")
	if entry := reg.FindByID(modelInput); entry != nil {
		return entry, nil
	}

	// 2. If quantization was explicitly set, require exact name+quant match
	if explicitQuant != "" {
		entry := reg.FindByNameAndQuant(modelInput, explicitQuant)
		if entry != nil {
			return entry, nil
		}
		return nil, quantNotFoundError(modelInput, explicitQuant, reg)
	}

	// 3. No explicit quantization → auto-select from available variants
	variants := reg.FindByName(modelInput)
	if len(variants) == 0 {
		return nil, modelNotFoundError(modelInput, reg)
	}

	// Pick the best quantization: prefer Q4_K_S > Q4_K_M > Q5_K_S > first
	preferredQuants := []string{"Q4_K_S", "Q4_K_M", "Q5_K_S", "Q5_K_M", "Q6_K", "Q8_0"}
	for _, pq := range preferredQuants {
		for i := range variants {
			if strings.EqualFold(variants[i].Quantization, pq) {
				return &variants[i], nil
			}
		}
	}

	// No preferred quant found — just use the first non-F16 variant
	for i := range variants {
		if !strings.EqualFold(variants[i].Quantization, "F16") {
			return &variants[i], nil
		}
	}

	// Last resort: first variant (even if F16)
	return &variants[0], nil
}

// quantNotFoundError is shown when the model name matches but the requested
// quantization doesn't exist. Lists available quantizations for that model.
func quantNotFoundError(modelName, quant string, reg *registry.Registry) error {
	variants := reg.FindByName(modelName)
	if len(variants) == 0 {
		// Model name doesn't match either — fall through to full model error
		return modelNotFoundError(modelName, reg)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("model %q found, but quantization %q is not available\n", modelName, quant))
	sb.WriteString("\nAvailable quantizations:\n\n")

	for _, v := range variants {
		vram := ""
		if v.VRAMRequirementsGB > 0 {
			vram = fmt.Sprintf("  (%.1f GB VRAM)", v.VRAMRequirementsGB)
		}
		sb.WriteString(fmt.Sprintf("  AUXOT_QUANTIZATION=%-10s%s\n", v.Quantization, vram))
	}

	sb.WriteString(fmt.Sprintf("\nOr just set AUXOT_MODEL=%s without AUXOT_QUANTIZATION\n", modelName))
	sb.WriteString("and the router will auto-select the best available quantization.\n")

	return fmt.Errorf("%s", sb.String())
}

// modelNotFoundError is shown when the model name doesn't match anything.
// Searches for close matches by substring to suggest corrections.
func modelNotFoundError(input string, reg *registry.Registry) error {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("model %q not found in registry\n", input))

	// Find close matches (case-insensitive substring)
	inputLower := strings.ToLower(input)
	var suggestions []string
	seen := make(map[string]bool)

	for _, m := range reg.Models {
		if seen[m.ModelName] {
			continue
		}
		if strings.Contains(strings.ToLower(m.ModelName), inputLower) ||
			strings.Contains(strings.ToLower(m.ID), inputLower) {
			suggestions = append(suggestions, fmt.Sprintf("  AUXOT_MODEL=%s", m.ModelName))
			seen[m.ModelName] = true
		}
	}

	if len(suggestions) > 0 {
		sb.WriteString("\nDid you mean one of these?\n\n")
		sort.Strings(suggestions)
		limit := len(suggestions)
		if limit > 15 {
			limit = 15
		}
		for _, s := range suggestions[:limit] {
			sb.WriteString(s + "\n")
		}
		if len(suggestions) > 15 {
			sb.WriteString(fmt.Sprintf("\n  ... and %d more\n", len(suggestions)-15))
		}
	} else {
		sb.WriteString("\n  (no close matches found)\n")
	}

	sb.WriteString("\nRun 'auxot-router models' for the full list of available models.\n")
	return fmt.Errorf("%s", sb.String())
}

// envStr reads an env var with a default value.
func envStr(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// envInt reads an env var as an integer with a default value.
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

// envDuration reads an env var as a duration string (e.g., "15s", "5m") with a default.
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

// envStringList reads a comma-separated env var into a string slice.
// Returns nil if the env var is unset or empty.
func envStringList(key string) []string {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	var result []string
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}
