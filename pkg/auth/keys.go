// Package auth provides API key generation and Argon2id verification
// for the Auxot router and worker authentication.
//
// The OSS router uses two keys:
//   - Admin key (adm_ prefix): GPU operators use this to connect worker-cli.
//   - API key (rtr_ prefix): API consumers use this to make chat requests.
//
// Keys are generated with crypto/rand and hashed with Argon2id.
// Plaintext keys are shown once at generation time, then only hashes are stored.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// KeyPrefix identifies the type of API key.
const (
	PrefixAdmin = "adm_" // GPU worker authentication
	PrefixAPI   = "rtr_" // API caller authentication
	PrefixTools = "tls_" // Tools worker authentication (tool connector key)
)

// GeneratedKey holds a newly generated key and its Argon2id hash.
// The plaintext Key is shown once to the user. Only the Hash is stored.
type GeneratedKey struct {
	Key  string // Plaintext key (e.g., "adm_abc123..."), show once then discard
	Hash string // Argon2id PHC hash string, store in env var
}

// GenerateAdminKey creates a new admin key for GPU worker authentication.
func GenerateAdminKey() (*GeneratedKey, error) {
	return generateKey(PrefixAdmin)
}

// GenerateAPIKey creates a new API key for chat request authentication.
func GenerateAPIKey() (*GeneratedKey, error) {
	return generateKey(PrefixAPI)
}

// GenerateToolKey creates a new tool connector key for tools worker authentication.
// Tools workers use a separate key type from GPU workers so they can be managed
// independently — different machines, different operators, different lifecycles.
func GenerateToolKey() (*GeneratedKey, error) {
	return generateKey(PrefixTools)
}

// generateKey creates a key with the given prefix and 32 random bytes.
// The random bytes are base64url-encoded (no padding) and appended to the prefix.
// The resulting key is hashed with Argon2id.
func generateKey(prefix string) (*GeneratedKey, error) {
	// 32 random bytes → 43 base64url characters
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generating random bytes: %w", err)
	}

	key := prefix + base64.RawURLEncoding.EncodeToString(secret)

	hash, err := HashKey(key)
	if err != nil {
		return nil, fmt.Errorf("hashing key: %w", err)
	}

	return &GeneratedKey{Key: key, Hash: hash}, nil
}

// ValidateKeyPrefix checks that a key string starts with a known prefix.
// Returns the prefix ("adm_", "rtr_", or "tls_") or an error.
func ValidateKeyPrefix(key string) (string, error) {
	switch {
	case strings.HasPrefix(key, PrefixAdmin):
		return PrefixAdmin, nil
	case strings.HasPrefix(key, PrefixAPI):
		return PrefixAPI, nil
	case strings.HasPrefix(key, PrefixTools):
		return PrefixTools, nil
	default:
		return "", fmt.Errorf("unknown key prefix: key must start with %q, %q, or %q", PrefixAdmin, PrefixAPI, PrefixTools)
	}
}
