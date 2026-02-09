package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // 64 MB
	argonThreads = 4
	argonKeyLen  = 32
	saltLen      = 16
)

// HashAPIKey hashes an API key using Argon2id.
func HashAPIKey(apiKey string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}

	hash := argon2.IDKey([]byte(apiKey), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	encoded := fmt.Sprintf("%s$%s",
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(hash),
	)
	return encoded, nil
}

// DummyVerify performs an Argon2id hash with the same cost parameters as real
// verification. Call this on auth failure paths where no real hash was checked,
// so that response timing does not reveal whether an agent_id exists.
func DummyVerify() {
	argon2.IDKey([]byte("dummy"), make([]byte, saltLen), argonTime, argonMemory, argonThreads, argonKeyLen)
}

// VerifyAPIKey checks an API key against an Argon2id hash.
func VerifyAPIKey(apiKey, encoded string) (bool, error) {
	parts := strings.SplitN(encoded, "$", 2)
	if len(parts) != 2 {
		return false, fmt.Errorf("auth: invalid hash format")
	}

	salt, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return false, fmt.Errorf("auth: decode salt: %w", err)
	}

	expectedHash, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return false, fmt.Errorf("auth: decode hash: %w", err)
	}

	computedHash := argon2.IDKey([]byte(apiKey), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	return subtle.ConstantTimeCompare(expectedHash, computedHash) == 1, nil
}
