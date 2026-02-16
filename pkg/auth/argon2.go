package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. These match the OWASP recommended defaults.
// Memory: 64 MiB, Iterations: 3, Parallelism: 4, Key length: 32 bytes.
const (
	argon2Memory      = 64 * 1024 // 64 MiB in KiB
	argon2Iterations  = 3
	argon2Parallelism = 4
	argon2KeyLength   = 32
	argon2SaltLength  = 16
)

// HashKey hashes a plaintext key using Argon2id and returns a PHC-format string.
//
// The output format is:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<salt_b64>$<hash_b64>
//
// This is the same format produced by most Argon2 libraries (Node.js, Python, etc.)
// so hashes are interoperable across languages.
func HashKey(key string) (string, error) {
	salt := make([]byte, argon2SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating salt: %w", err)
	}

	hash := argon2.IDKey([]byte(key), salt, argon2Iterations, argon2Memory, argon2Parallelism, argon2KeyLength)

	// Encode in PHC string format
	saltB64 := base64.RawStdEncoding.EncodeToString(salt)
	hashB64 := base64.RawStdEncoding.EncodeToString(hash)

	phc := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argon2Memory, argon2Iterations, argon2Parallelism,
		saltB64, hashB64)

	return phc, nil
}

// VerifyKey checks a plaintext key against an Argon2id PHC-format hash string.
// Returns true if the key matches the hash, false otherwise.
// Uses constant-time comparison to prevent timing attacks.
func VerifyKey(key string, phcHash string) (bool, error) {
	salt, params, expectedHash, err := parsePHC(phcHash)
	if err != nil {
		return false, fmt.Errorf("parsing hash: %w", err)
	}

	computedHash := argon2.IDKey([]byte(key), salt, params.iterations, params.memory, params.parallelism, uint32(len(expectedHash)))

	if subtle.ConstantTimeCompare(computedHash, expectedHash) == 1 {
		return true, nil
	}
	return false, nil
}

// argon2Params holds the parsed parameters from a PHC string.
type argon2Params struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
}

// parsePHC extracts the salt, parameters, and hash from a PHC-format string.
//
// Expected format: $argon2id$v=19$m=65536,t=3,p=4$<salt_b64>$<hash_b64>
func parsePHC(phc string) (salt []byte, params argon2Params, hash []byte, err error) {
	// Split on '$'. Leading '$' creates an empty first element.
	// Expected parts: ["", "argon2id", "v=19", "m=65536,t=3,p=4", "<salt>", "<hash>"]
	parts := strings.Split(phc, "$")
	if len(parts) != 6 {
		return nil, params, nil, fmt.Errorf("invalid PHC format: expected 6 parts, got %d", len(parts))
	}

	if parts[1] != "argon2id" {
		return nil, params, nil, fmt.Errorf("unsupported algorithm: %q (only argon2id supported)", parts[1])
	}

	// Parse version (we accept any v=19 variant but don't enforce it strictly)
	// Parse parameters: m=65536,t=3,p=4
	var m, t uint32
	var p uint8
	n, scanErr := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p)
	if scanErr != nil || n != 3 {
		return nil, params, nil, fmt.Errorf("invalid parameters: %q", parts[3])
	}
	params = argon2Params{memory: m, iterations: t, parallelism: p}

	// Decode salt
	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, params, nil, fmt.Errorf("decoding salt: %w", err)
	}

	// Decode hash
	hash, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, params, nil, fmt.Errorf("decoding hash: %w", err)
	}

	return salt, params, hash, nil
}

// Verifier wraps VerifyKey with a process-local cache.
//
// Argon2id verification is intentionally slow (~100ms). For a router handling
// many requests with the same API key, we cache the result so only the first
// verification pays the cost.
//
// The cache maps plaintext keys to cached results. Since the OSS router
// typically has exactly two keys (admin + API), this cache stays tiny.
// Entries expire after the configured TTL to limit exposure if a key is revoked.
type Verifier struct {
	adminHash string
	apiHash   string

	cache   map[string]cacheEntry
	cacheMu sync.RWMutex
	ttl     time.Duration
}

type cacheEntry struct {
	valid     bool
	expiresAt time.Time
}

// NewVerifier creates a Verifier with the given Argon2id hashes for the admin
// and API keys. Either hash may be empty if that key type is not configured.
// The cache TTL controls how long successful verifications are remembered.
func NewVerifier(adminHash, apiHash string, cacheTTL time.Duration) *Verifier {
	return &Verifier{
		adminHash: adminHash,
		apiHash:   apiHash,
		cache:     make(map[string]cacheEntry),
		ttl:       cacheTTL,
	}
}

// VerifyAdminKey checks if the given key is a valid admin key.
// Results are cached for the configured TTL.
func (v *Verifier) VerifyAdminKey(key string) (bool, error) {
	if v.adminHash == "" {
		return false, fmt.Errorf("admin key not configured")
	}
	return v.verifyWithCache(key, v.adminHash)
}

// VerifyAPIKey checks if the given key is a valid API key.
// Results are cached for the configured TTL.
func (v *Verifier) VerifyAPIKey(key string) (bool, error) {
	if v.apiHash == "" {
		return false, fmt.Errorf("API key not configured")
	}
	return v.verifyWithCache(key, v.apiHash)
}

// verifyWithCache checks the cache first, falls back to Argon2 verification.
// Both positive and negative results are cached.
func (v *Verifier) verifyWithCache(key, hash string) (bool, error) {
	// Cache key combines the plaintext key and the hash it's verified against.
	// This ensures changing the hash invalidates the cache.
	cacheKey := key + "|" + hash

	// Check cache (read lock)
	v.cacheMu.RLock()
	entry, ok := v.cache[cacheKey]
	v.cacheMu.RUnlock()

	if ok && time.Now().Before(entry.expiresAt) {
		return entry.valid, nil
	}

	// Cache miss or expired — do the slow Argon2 verification
	valid, err := VerifyKey(key, hash)
	if err != nil {
		return false, err
	}

	// Store in cache (write lock)
	v.cacheMu.Lock()
	v.cache[cacheKey] = cacheEntry{
		valid:     valid,
		expiresAt: time.Now().Add(v.ttl),
	}
	v.cacheMu.Unlock()

	return valid, nil
}
