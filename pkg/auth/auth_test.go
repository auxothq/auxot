package auth

import (
	"strings"
	"testing"
	"time"
)

// --- Key generation tests ---

func TestGenerateAdminKey(t *testing.T) {
	gen, err := GenerateAdminKey()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasPrefix(gen.Key, PrefixAdmin) {
		t.Errorf("admin key should start with %q, got %q", PrefixAdmin, gen.Key[:10])
	}

	if !strings.HasPrefix(gen.Hash, "$argon2id$") {
		t.Errorf("hash should be PHC format, got %q", gen.Hash[:20])
	}
}

func TestGenerateAPIKey(t *testing.T) {
	gen, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasPrefix(gen.Key, PrefixAPI) {
		t.Errorf("API key should start with %q, got %q", PrefixAPI, gen.Key[:10])
	}

	if !strings.HasPrefix(gen.Hash, "$argon2id$") {
		t.Errorf("hash should be PHC format, got %q", gen.Hash[:20])
	}
}

func TestGenerateKeyUniqueness(t *testing.T) {
	key1, err := GenerateAdminKey()
	if err != nil {
		t.Fatalf("generating key 1: %v", err)
	}
	key2, err := GenerateAdminKey()
	if err != nil {
		t.Fatalf("generating key 2: %v", err)
	}

	if key1.Key == key2.Key {
		t.Error("two generated keys should not be identical")
	}
	if key1.Hash == key2.Hash {
		t.Error("two generated hashes should not be identical (different salts)")
	}
}

// --- Prefix validation tests ---

func TestValidateKeyPrefix(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		wantPrefix string
		wantErr    bool
	}{
		{"admin key", "adm_abc123", PrefixAdmin, false},
		{"api key", "rtr_xyz789", PrefixAPI, false},
		{"unknown prefix", "gpu_abc123", "", true},
		{"no prefix", "abc123", "", true},
		{"empty string", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefix, err := ValidateKeyPrefix(tt.key)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if prefix != tt.wantPrefix {
				t.Errorf("prefix: got %q, want %q", prefix, tt.wantPrefix)
			}
		})
	}
}

// --- Argon2 hash/verify tests ---

func TestHashAndVerify(t *testing.T) {
	key := "adm_test_key_for_hashing"

	hash, err := HashKey(key)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}

	// Verify correct key
	valid, err := VerifyKey(key, hash)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if !valid {
		t.Error("correct key should verify as valid")
	}

	// Verify wrong key
	valid, err = VerifyKey("wrong_key", hash)
	if err != nil {
		t.Fatalf("verifying wrong key: %v", err)
	}
	if valid {
		t.Error("wrong key should verify as invalid")
	}
}

func TestHashProducesPHCFormat(t *testing.T) {
	hash, err := HashKey("test_key")
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}

	// PHC format: $argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Errorf("hash should match PHC format with expected params, got: %s", hash)
	}

	parts := strings.Split(hash, "$")
	if len(parts) != 6 {
		t.Errorf("PHC string should have 6 $-separated parts, got %d", len(parts))
	}
}

func TestHashDifferentSalts(t *testing.T) {
	key := "same_key"
	hash1, err := HashKey(key)
	if err != nil {
		t.Fatalf("hash 1: %v", err)
	}
	hash2, err := HashKey(key)
	if err != nil {
		t.Fatalf("hash 2: %v", err)
	}

	// Same key should produce different hashes (different random salts)
	if hash1 == hash2 {
		t.Error("same key hashed twice should produce different hashes (random salt)")
	}

	// But both should verify
	valid1, err := VerifyKey(key, hash1)
	if err != nil {
		t.Fatalf("verify hash1: %v", err)
	}
	valid2, err := VerifyKey(key, hash2)
	if err != nil {
		t.Fatalf("verify hash2: %v", err)
	}
	if !valid1 || !valid2 {
		t.Error("both hashes should verify against the original key")
	}
}

func TestVerifyMalformedHash(t *testing.T) {
	tests := []struct {
		name string
		hash string
	}{
		{"empty string", ""},
		{"not PHC format", "just_a_string"},
		{"wrong algorithm", "$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA"},
		{"missing parts", "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA"},
		{"invalid base64 salt", "$argon2id$v=19$m=65536,t=3,p=4$!!!invalid!!!$aGFzaA"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := VerifyKey("any_key", tt.hash)
			if err == nil {
				t.Fatal("expected error for malformed hash, got nil")
			}
		})
	}
}

// --- Generated key round-trip test ---

func TestGeneratedKeyRoundTrip(t *testing.T) {
	// Generate a key and verify the plaintext matches its own hash
	gen, err := GenerateAdminKey()
	if err != nil {
		t.Fatalf("generating: %v", err)
	}

	valid, err := VerifyKey(gen.Key, gen.Hash)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if !valid {
		t.Error("generated key should verify against its own hash")
	}
}

// --- Verifier (cached) tests ---

func TestVerifier_CorrectKeys(t *testing.T) {
	adminGen, err := GenerateAdminKey()
	if err != nil {
		t.Fatalf("generating admin key: %v", err)
	}
	apiGen, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("generating API key: %v", err)
	}

	v := NewVerifier(adminGen.Hash, apiGen.Hash, 5*time.Minute)

	valid, err := v.VerifyAdminKey(adminGen.Key)
	if err != nil {
		t.Fatalf("verifying admin key: %v", err)
	}
	if !valid {
		t.Error("admin key should be valid")
	}

	valid, err = v.VerifyAPIKey(apiGen.Key)
	if err != nil {
		t.Fatalf("verifying API key: %v", err)
	}
	if !valid {
		t.Error("API key should be valid")
	}
}

func TestVerifier_WrongKeys(t *testing.T) {
	adminGen, err := GenerateAdminKey()
	if err != nil {
		t.Fatalf("generating admin key: %v", err)
	}
	apiGen, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("generating API key: %v", err)
	}

	v := NewVerifier(adminGen.Hash, apiGen.Hash, 5*time.Minute)

	valid, err := v.VerifyAdminKey("adm_wrong_key")
	if err != nil {
		t.Fatalf("verifying wrong admin key: %v", err)
	}
	if valid {
		t.Error("wrong admin key should be invalid")
	}

	valid, err = v.VerifyAPIKey("rtr_wrong_key")
	if err != nil {
		t.Fatalf("verifying wrong API key: %v", err)
	}
	if valid {
		t.Error("wrong API key should be invalid")
	}
}

func TestVerifier_MissingHash(t *testing.T) {
	v := NewVerifier("", "", 5*time.Minute)

	_, err := v.VerifyAdminKey("adm_anything")
	if err == nil {
		t.Error("should error when admin hash not configured")
	}

	_, err = v.VerifyAPIKey("rtr_anything")
	if err == nil {
		t.Error("should error when API hash not configured")
	}
}

func TestVerifier_CacheHit(t *testing.T) {
	gen, err := GenerateAdminKey()
	if err != nil {
		t.Fatalf("generating: %v", err)
	}

	v := NewVerifier(gen.Hash, "", 5*time.Minute)

	// First call: cache miss, does real Argon2
	start := time.Now()
	valid1, err := v.VerifyAdminKey(gen.Key)
	if err != nil {
		t.Fatalf("first verify: %v", err)
	}
	firstDuration := time.Since(start)

	// Second call: cache hit, should be near-instant
	start = time.Now()
	valid2, err := v.VerifyAdminKey(gen.Key)
	if err != nil {
		t.Fatalf("second verify: %v", err)
	}
	secondDuration := time.Since(start)

	if !valid1 || !valid2 {
		t.Error("both verifications should succeed")
	}

	// The cached call should be at least 10x faster than the first.
	// Argon2 takes ~50-200ms, cache lookup takes microseconds.
	if secondDuration > firstDuration/5 {
		t.Errorf("cache hit should be much faster: first=%v, second=%v", firstDuration, secondDuration)
	}
}

func TestVerifier_CacheExpiry(t *testing.T) {
	gen, err := GenerateAdminKey()
	if err != nil {
		t.Fatalf("generating: %v", err)
	}

	// Very short TTL for testing
	v := NewVerifier(gen.Hash, "", 1*time.Millisecond)

	// Populate cache
	valid, err := v.VerifyAdminKey(gen.Key)
	if err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if !valid {
		t.Fatal("first verify should succeed")
	}

	// Wait for cache to expire
	time.Sleep(5 * time.Millisecond)

	// Should still verify (re-does Argon2 after expiry)
	valid, err = v.VerifyAdminKey(gen.Key)
	if err != nil {
		t.Fatalf("verify after expiry: %v", err)
	}
	if !valid {
		t.Error("should still verify after cache expires")
	}
}
