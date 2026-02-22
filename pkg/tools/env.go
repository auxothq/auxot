package tools

import (
	"context"
	"os"
	"strings"
)

// ctxKeyCredentials is the unexported context key for injected credentials.
type ctxKeyCredentials struct{}

// WithCredentials returns a context that carries the given credential map.
// Built-in tools retrieve individual values via Credential(ctx, key).
// Shell tool executors call CredentialsFromCtx + BuildToolEnv to get a full env slice.
func WithCredentials(ctx context.Context, creds map[string]string) context.Context {
	if len(creds) == 0 {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyCredentials{}, creds)
}

// Credential retrieves a named credential. Lookup order (highest priority last):
//  1. os.Getenv(key)
//  2. Credentials injected into ctx via WithCredentials
//
// This allows per-job credentials sent by the router to override operator-level env vars.
func Credential(ctx context.Context, key string) string {
	// Check context first (job-level credentials take priority).
	if creds, ok := ctx.Value(ctxKeyCredentials{}).(map[string]string); ok {
		if v, ok := creds[key]; ok {
			return v
		}
	}
	return os.Getenv(key)
}

// CredentialsFromCtx extracts the credential map stored in ctx by WithCredentials.
// Returns nil if no credentials were injected into the context.
func CredentialsFromCtx(ctx context.Context) map[string]string {
	creds, _ := ctx.Value(ctxKeyCredentials{}).(map[string]string)
	return creds
}

// ParseToolCredentials reads AUXOT_TOOLS_{TOOL_NAME}__{VAR_NAME} environment
// variables and returns a map of tool name → credential map.
//
// Naming convention:
//
//	AUXOT_TOOLS_WEB_SEARCH__BRAVE_SEARCH_API_KEY=xxx
//	→ result["web_search"]["BRAVE_SEARCH_API_KEY"] = "xxx"
//
// Tool names are normalised to lowercase. Entries without a double-underscore
// separator, or with an empty tool name or variable name, are silently ignored.
func ParseToolCredentials() map[string]map[string]string {
	const prefix = "AUXOT_TOOLS_"
	result := make(map[string]map[string]string)

	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		// Strip the "AUXOT_TOOLS_" prefix, leaving "{TOOL_NAME}__{VAR_NAME}".
		rest := strings.TrimPrefix(k, prefix)

		// Must contain "__" to be a credential entry.
		toolUpper, varName, ok := strings.Cut(rest, "__")
		if !ok || toolUpper == "" || varName == "" {
			continue
		}

		toolName := strings.ToLower(toolUpper)
		if result[toolName] == nil {
			result[toolName] = make(map[string]string)
		}
		result[toolName][varName] = v
	}

	return result
}

// BuildToolEnv builds an environment slice (KEY=VALUE strings) suitable for exec.Cmd.Env.
// It merges four sources in increasing priority order:
//
//  1. Current process environment (os.Environ) — baseline
//  2. Legacy AUXOT_TOOL_* vars stripped of their prefix:
//     AUXOT_TOOL_BRAVE_SEARCH_API_KEY=xxx → BRAVE_SEARCH_API_KEY=xxx
//  3. New AUXOT_TOOLS_{TOOL_NAME}__{VAR_NAME} vars stripped to bare VAR_NAME:
//     AUXOT_TOOLS_WEB_SEARCH__BRAVE_SEARCH_API_KEY=xxx → BRAVE_SEARCH_API_KEY=xxx
//  4. jobCredentials — highest priority; allows per-job runtime override
//
// The original AUXOT_TOOL_* and AUXOT_TOOLS_*__* vars are preserved as-is in
// addition to the stripped copies.
func BuildToolEnv(jobCredentials map[string]string) []string {
	base := os.Environ()

	// Index by key for O(1) lookup and override.
	merged := make(map[string]string, len(base)+len(jobCredentials))

	// Step 1: seed with all process env vars.
	for _, kv := range base {
		k, v, _ := strings.Cut(kv, "=")
		merged[k] = v
	}

	// Step 2: strip legacy AUXOT_TOOL_ prefix and add bare-key copies.
	const legacyPrefix = "AUXOT_TOOL_"
	// Step 3: strip new AUXOT_TOOLS_{TOOL_NAME}__ prefix and add bare-key copies.
	const newPrefix = "AUXOT_TOOLS_"

	// We range over base (not merged) to avoid mutation-during-range issues.
	for _, kv := range base {
		k, v, _ := strings.Cut(kv, "=")

		// Legacy: AUXOT_TOOL_BRAVE_SEARCH_API_KEY → BRAVE_SEARCH_API_KEY
		if strings.HasPrefix(k, legacyPrefix) {
			bare := strings.TrimPrefix(k, legacyPrefix)
			if bare != "" {
				merged[bare] = v
			}
			continue
		}

		// New: AUXOT_TOOLS_WEB_SEARCH__BRAVE_SEARCH_API_KEY → BRAVE_SEARCH_API_KEY
		if strings.HasPrefix(k, newPrefix) {
			rest := strings.TrimPrefix(k, newPrefix)
			_, varName, ok := strings.Cut(rest, "__")
			if ok && varName != "" {
				merged[varName] = v
			}
		}
	}

	// Step 4: job credentials override everything.
	for k, v := range jobCredentials {
		if k != "" {
			merged[k] = v
		}
	}

	// Flatten to KEY=VALUE slice.
	result := make([]string, 0, len(merged))
	for k, v := range merged {
		result = append(result, k+"="+v)
	}
	return result
}
