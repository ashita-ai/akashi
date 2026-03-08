package model_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

// ---------- GenerateRawKey tests ----------

func TestGenerateRawKey(t *testing.T) {
	t.Run("format", func(t *testing.T) {
		rawKey, prefix, err := model.GenerateRawKey()
		require.NoError(t, err)

		assert.True(t, strings.HasPrefix(rawKey, "ak_"), "raw key must start with ak_")
		assert.Len(t, prefix, 8, "prefix is 4 bytes = 8 hex chars")
		assert.Contains(t, rawKey, prefix, "raw key must contain the prefix")

		// Format: ak_<8>_<32>
		parts := strings.SplitN(rawKey, "_", 3)
		require.Len(t, parts, 3, "key must have format ak_<prefix>_<secret>")
		assert.Equal(t, "ak", parts[0])
		assert.Len(t, parts[1], 8, "prefix portion is 8 hex chars")
		assert.Len(t, parts[2], 32, "secret portion is 32 hex chars")
	})

	t.Run("uniqueness", func(t *testing.T) {
		key1, _, err := model.GenerateRawKey()
		require.NoError(t, err)
		key2, _, err := model.GenerateRawKey()
		require.NoError(t, err)
		assert.NotEqual(t, key1, key2, "consecutive keys must be unique")
	})

	t.Run("prefix uniqueness", func(t *testing.T) {
		// Generate several keys and check that prefixes vary (probabilistic but
		// collision of 10 consecutive 4-byte random prefixes is astronomically unlikely).
		seen := make(map[string]bool)
		for range 10 {
			_, prefix, err := model.GenerateRawKey()
			require.NoError(t, err)
			seen[prefix] = true
		}
		assert.Greater(t, len(seen), 1, "prefixes should not all be identical")
	})

	t.Run("hex characters only", func(t *testing.T) {
		rawKey, _, err := model.GenerateRawKey()
		require.NoError(t, err)

		// Strip the "ak_" prefix and underscores, remainder should be hex.
		stripped := strings.ReplaceAll(rawKey[3:], "_", "")
		assert.Regexp(t, `^[0-9a-f]+$`, stripped, "key material must be lowercase hex")
	})
}

// ---------- ParseRawKey tests ----------

func TestParseRawKey(t *testing.T) {
	t.Run("valid key", func(t *testing.T) {
		rawKey, expectedPrefix, err := model.GenerateRawKey()
		require.NoError(t, err)

		prefix, fullKey, err := model.ParseRawKey(rawKey)
		require.NoError(t, err)
		assert.Equal(t, expectedPrefix, prefix)
		assert.Equal(t, rawKey, fullKey)
	})

	t.Run("valid synthetic key", func(t *testing.T) {
		prefix, fullKey, err := model.ParseRawKey("ak_abcd1234_" + strings.Repeat("f", 32))
		require.NoError(t, err)
		assert.Equal(t, "abcd1234", prefix)
		assert.Equal(t, "ak_abcd1234_"+strings.Repeat("f", 32), fullKey)
	})

	tests := []struct {
		name    string
		rawKey  string
		wantErr string
	}{
		{
			name:    "missing ak_ prefix",
			rawKey:  "bad_abcd1234_" + strings.Repeat("f", 32),
			wantErr: "missing ak_ prefix",
		},
		{
			name:    "empty string",
			rawKey:  "",
			wantErr: "missing ak_ prefix",
		},
		{
			name:    "only prefix no underscore",
			rawKey:  "ak_abcdef12",
			wantErr: "expected ak_<prefix>_<secret>",
		},
		{
			name:    "trailing underscore no secret",
			rawKey:  "ak_abcdef12_",
			wantErr: "expected ak_<prefix>_<secret>",
		},
		{
			name:    "no prefix between underscores",
			rawKey:  "ak__secret",
			wantErr: "expected ak_<prefix>_<secret>",
		},
		{
			name:    "just ak_",
			rawKey:  "ak_",
			wantErr: "expected ak_<prefix>_<secret>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := model.ParseRawKey(tt.rawKey)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// ---------- ValidateKeyLabel tests ----------

func TestValidateKeyLabel(t *testing.T) {
	tests := []struct {
		name    string
		label   string
		wantErr bool
	}{
		{"empty label is valid", "", false},
		{"short label", "prod-key", false},
		{"exactly 255 chars", strings.Repeat("x", 255), false},
		{"256 chars is too long", strings.Repeat("x", 256), true},
		{"1000 chars is too long", strings.Repeat("x", 1000), true},
		{"unicode label", "日本語ラベル", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := model.ValidateKeyLabel(tt.label)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "at most 255")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// ---------- ParseRawKey roundtrip ----------

func TestParseRawKey_Roundtrip(t *testing.T) {
	// Generate a key and verify ParseRawKey extracts the correct prefix.
	for range 5 {
		rawKey, genPrefix, err := model.GenerateRawKey()
		require.NoError(t, err)

		parsedPrefix, parsedFull, err := model.ParseRawKey(rawKey)
		require.NoError(t, err)
		assert.Equal(t, genPrefix, parsedPrefix, "roundtrip prefix must match")
		assert.Equal(t, rawKey, parsedFull, "roundtrip full key must match")
	}
}
