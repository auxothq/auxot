// Package routerurl provides canonical URL normalisation for AUXOT_ROUTER_URL.
//
// We try to be lenient in what we accept to make configuration easier:
//   - Missing scheme defaults to wss://
//   - http:// and https:// are silently converted to ws:// and wss://
//   - Any path the caller supplied is discarded and replaced with the canonical
//     WebSocket path for the connecting worker type (/ws or /ws/agent).
//   - Trailing slashes are stripped.
//
// Note: because the path is always replaced, custom reverse-proxy deployments
// on non-standard paths (e.g. /internal/auxot/ws) will not work if that path
// is supplied in AUXOT_ROUTER_URL. Provide only the scheme and host[:port];
// the correct WebSocket path is appended automatically.
package routerurl

import (
	"fmt"
	"net/url"
	"strings"
)

// Normalize accepts AUXOT_ROUTER_URL in any reasonable format and returns a
// canonical WebSocket URL with wsPath as the path.
//
// Examples (wsPath = "/ws"):
//
//	"auxot.example.com"               → "wss://auxot.example.com/ws"
//	"http://localhost:8080"           → "ws://localhost:8080/ws"
//	"https://auxot.example.com"      → "wss://auxot.example.com/ws"
//	"ws://localhost:8080/ws"          → "ws://localhost:8080/ws"
//	"wss://auxot.example.com/old"    → "wss://auxot.example.com/ws"
//	"wss://auxot.example.com/ws/"    → "wss://auxot.example.com/ws"
func Normalize(raw, wsPath string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("AUXOT_ROUTER_URL is empty")
	}

	// No scheme → assume wss://
	if !strings.Contains(raw, "://") {
		raw = "wss://" + raw
	}

	// Convert http(s) → ws(s) so users can copy a browser URL directly.
	raw = strings.Replace(raw, "https://", "wss://", 1)
	raw = strings.Replace(raw, "http://", "ws://", 1)

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("cannot parse AUXOT_ROUTER_URL %q: %w", raw, err)
	}

	if u.Host == "" {
		return "", fmt.Errorf("AUXOT_ROUTER_URL %q has no host", raw)
	}

	// Replace whatever path the user provided with the canonical WebSocket path.
	// This is intentional: all Auxot servers expose the same fixed paths, and
	// discarding the user-supplied path makes configuration forgiving.
	u.Path = wsPath
	u.RawQuery = ""
	u.Fragment = ""

	return u.String(), nil
}

// HTTPBase converts a normalized WebSocket URL to an HTTP base URL suitable
// for REST API calls. The path is stripped so the caller can append its own.
//
//	"wss://auxot.example.com/ws/agent" → "https://auxot.example.com"
//	"ws://localhost:8080/ws"           → "http://localhost:8080"
func HTTPBase(wsURL string) string {
	u, err := url.Parse(wsURL)
	if err != nil {
		// Best-effort fallback.
		r := strings.NewReplacer("wss://", "https://", "ws://", "http://")
		return r.Replace(wsURL)
	}
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	r := strings.NewReplacer("wss://", "https://", "ws://", "http://")
	return r.Replace(u.String())
}
