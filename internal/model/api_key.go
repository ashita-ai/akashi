package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// APIKey represents a decoupled API key that authenticates as a specific agent.
// Multiple keys can exist per agent, enabling rotation and per-environment credentials.
type APIKey struct {
	ID         uuid.UUID  `json:"id"`
	Prefix     string     `json:"prefix"`
	KeyHash    string     `json:"-"` // Never serialized.
	AgentID    string     `json:"agent_id"`
	OrgID      uuid.UUID  `json:"org_id"`
	Label      string     `json:"label"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// APIKeyWithRawKey is returned only on creation/rotation — the only time
// the raw key is available. After this, only the prefix is visible.
type APIKeyWithRawKey struct {
	APIKey
	RawKey string `json:"raw_key"`
}

// CreateKeyRequest is the request body for POST /v1/keys.
type CreateKeyRequest struct {
	AgentID   string  `json:"agent_id"`
	Label     string  `json:"label"`
	ExpiresAt *string `json:"expires_at,omitempty"` // RFC3339
}

// APIKeyResponse is the list response for GET /v1/keys.
type APIKeyResponse struct {
	Keys    []APIKey `json:"keys"`
	Total   int      `json:"total"`
	Limit   int      `json:"limit"`
	Offset  int      `json:"offset"`
	HasMore bool     `json:"has_more"`
}

// RotateKeyResponse is the response for POST /v1/keys/{id}/rotate.
type RotateKeyResponse struct {
	NewKey       APIKeyWithRawKey `json:"new_key"`
	RevokedKeyID uuid.UUID        `json:"revoked_key_id"`
}

const (
	// keyPrefixLen is the number of random bytes used for the key prefix (8 hex chars).
	keyPrefixLen = 4
	// keySecretLen is the number of random bytes for the secret portion (32 hex chars).
	keySecretLen = 16
	// keyFormatPrefix is the static prefix for all Akashi API keys.
	keyFormatPrefix = "ak_"
)

// GenerateRawKey produces a new raw API key in the format: ak_<8-char-prefix>_<32-char-secret>.
// Returns the full raw key and the prefix separately.
func GenerateRawKey() (rawKey, prefix string, err error) {
	prefixBytes := make([]byte, keyPrefixLen)
	if _, err := rand.Read(prefixBytes); err != nil {
		return "", "", fmt.Errorf("model: generate key prefix: %w", err)
	}

	secretBytes := make([]byte, keySecretLen)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", "", fmt.Errorf("model: generate key secret: %w", err)
	}

	prefix = hex.EncodeToString(prefixBytes)
	secret := hex.EncodeToString(secretBytes)
	rawKey = keyFormatPrefix + prefix + "_" + secret

	return rawKey, prefix, nil
}

// ParseRawKey extracts the prefix and full key from a raw key string.
// Returns an error if the format is invalid.
func ParseRawKey(rawKey string) (prefix, fullKey string, err error) {
	if !strings.HasPrefix(rawKey, keyFormatPrefix) {
		return "", "", fmt.Errorf("model: invalid key format: missing %s prefix", keyFormatPrefix)
	}

	rest := rawKey[len(keyFormatPrefix):]
	underIdx := strings.IndexByte(rest, '_')
	if underIdx < 1 || underIdx == len(rest)-1 {
		return "", "", fmt.Errorf("model: invalid key format: expected ak_<prefix>_<secret>")
	}

	prefix = rest[:underIdx]
	return prefix, rawKey, nil
}

// ValidateKeyLabel checks that a key label is reasonable.
func ValidateKeyLabel(label string) error {
	if len(label) > 255 {
		return fmt.Errorf("label must be at most 255 characters")
	}
	return nil
}
