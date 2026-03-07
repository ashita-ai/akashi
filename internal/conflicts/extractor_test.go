package conflicts

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseExtractedClaims_ValidJSON(t *testing.T) {
	raw := `[{"text":"The outbox has no deadletter mechanism","category":"finding"},{"text":"Security posture is strong","category":"assessment"}]`
	claims, err := parseExtractedClaims(raw)
	require.NoError(t, err)
	assert.Len(t, claims, 2)
	assert.Equal(t, "The outbox has no deadletter mechanism", claims[0].Text)
	assert.Equal(t, "finding", claims[0].Category)
	assert.Equal(t, "Security posture is strong", claims[1].Text)
	assert.Equal(t, "assessment", claims[1].Category)
}

func TestParseExtractedClaims_MarkdownCodeFence(t *testing.T) {
	raw := "```json\n" +
		`[{"text":"Use PostgreSQL for storage","category":"recommendation"}]` +
		"\n```"
	claims, err := parseExtractedClaims(raw)
	require.NoError(t, err)
	assert.Len(t, claims, 1)
	assert.Equal(t, "recommendation", claims[0].Category)
}

func TestParseExtractedClaims_ShortClaimsFiltered(t *testing.T) {
	raw := `[{"text":"OK","category":"status"},{"text":"The outbox has no deadletter mechanism","category":"finding"}]`
	claims, err := parseExtractedClaims(raw)
	require.NoError(t, err)
	assert.Len(t, claims, 1)
	assert.Equal(t, "finding", claims[0].Category)
}

func TestParseExtractedClaims_InvalidCategory(t *testing.T) {
	raw := `[{"text":"Merkle proof has a timing leak vulnerability","category":"bug"}]`
	claims, err := parseExtractedClaims(raw)
	require.NoError(t, err)
	assert.Len(t, claims, 1)
	assert.Equal(t, "finding", claims[0].Category, "unknown categories default to finding")
}

func TestParseExtractedClaims_CategoryNormalization(t *testing.T) {
	raw := `[{"text":"The scoring formula is correctly bounded","category":"  ASSESSMENT  "}]`
	claims, err := parseExtractedClaims(raw)
	require.NoError(t, err)
	assert.Len(t, claims, 1)
	assert.Equal(t, "assessment", claims[0].Category)
}

func TestParseExtractedClaims_InvalidJSON(t *testing.T) {
	_, err := parseExtractedClaims("not json at all")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parse JSON")
}

func TestParseExtractedClaims_EmptyArray(t *testing.T) {
	claims, err := parseExtractedClaims("[]")
	require.NoError(t, err)
	assert.Empty(t, claims)
}

func TestConflictRelevantCategory(t *testing.T) {
	finding := "finding"
	assessment := "assessment"
	recommendation := "recommendation"
	status := "status"

	assert.True(t, ConflictRelevantCategory(nil), "nil (regex-extracted) should be relevant")
	assert.True(t, ConflictRelevantCategory(&finding))
	assert.True(t, ConflictRelevantCategory(&assessment))
	assert.False(t, ConflictRelevantCategory(&recommendation))
	assert.False(t, ConflictRelevantCategory(&status))
}
