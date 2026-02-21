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

// BuildToolEnv builds an environment slice (KEY=VALUE strings) suitable for exec.Cmd.Env.
// It merges three sources in increasing priority order:
//
//  1. Current process environment (os.Environ) — baseline
//  2. AUXOT_TOOL_* vars stripped of their prefix:
//     AUXOT_TOOL_BRAVE_API_KEY=xxx → BRAVE_API_KEY=xxx
//  3. jobCredentials — highest priority; allows per-job runtime override
//
// The original AUXOT_TOOL_* vars are preserved as-is in addition to the stripped copies.
func BuildToolEnv(jobCredentials map[string]string) []string {
	base := os.Environ()

	// Index by key for O(1) lookup and override.
	merged := make(map[string]string, len(base)+len(jobCredentials))

	// Step 1: seed with all process env vars.
	for _, kv := range base {
		k, v, _ := strings.Cut(kv, "=")
		merged[k] = v
	}

	// Step 2: strip AUXOT_TOOL_ prefix and add bare-key copies.
	// We range over base (not merged) to avoid mutation-during-range issues.
	const prefix = "AUXOT_TOOL_"
	for _, kv := range base {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, prefix) {
			bare := strings.TrimPrefix(k, prefix)
			if bare != "" {
				// AUXOT_TOOL_ value overrides the bare process env value.
				merged[bare] = v
			}
		}
	}

	// Step 3: job credentials override everything.
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
