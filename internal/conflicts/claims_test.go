package conflicts

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitClaims_SimpleSentences(t *testing.T) {
	input := "ReScore is correctly bounded within [0,1]. The outbox has no deadletter mechanism. Merkle proof has a timing leak."
	claims := SplitClaims(input)
	assert.Equal(t, []string{
		"ReScore is correctly bounded within [0,1].",
		"The outbox has no deadletter mechanism.",
		"Merkle proof has a timing leak.",
	}, claims)
}

func TestSplitClaims_NumberedItems(t *testing.T) {
	input := "Three critical findings: (1) search outbox worker has no deadletter mechanism, (2) Merkle proof verification timing leak, (3) ReScore is correctly bounded within [0,1] but has no unit tests."
	claims := SplitClaims(input)
	assert.Contains(t, claims, "(1) search outbox worker has no deadletter mechanism,")
	assert.Contains(t, claims, "(2) Merkle proof verification timing leak,")
	assert.Contains(t, claims, "(3) ReScore is correctly bounded within [0,1] but has no unit tests.")
}

func TestSplitClaims_ShortFragmentsFiltered(t *testing.T) {
	input := "Good. OK. This is a meaningful claim about architecture."
	claims := SplitClaims(input)
	// "Good." and "OK." are too short (< 20 chars).
	assert.Equal(t, []string{
		"This is a meaningful claim about architecture.",
	}, claims)
}

func TestSplitClaims_Empty(t *testing.T) {
	assert.Nil(t, SplitClaims(""))
}

func TestSplitClaims_SingleLongSentence(t *testing.T) {
	input := "ReScore scoring formula produces values exceeding 1.0 when the raw Qdrant cosine similarity is high"
	claims := SplitClaims(input)
	assert.Equal(t, []string{input}, claims)
}

func TestSplitClaims_AbbreviationsPreserved(t *testing.T) {
	// "e.g." and "i.e." should not cause spurious splits.
	input := "The scorer e.g. the conflict detection component works correctly. It handles edge cases."
	claims := SplitClaims(input)
	// Should split on ". It" but not on "e.g."
	assert.Len(t, claims, 2)
	assert.Contains(t, claims[0], "e.g.")
}

func TestSplitClaims_RealWorldOutcome(t *testing.T) {
	// This is based on a real decision outcome from our data.
	input := "Deep review of 14 files across search/outbox, conflict detection, MCP, embedding, integrity, quality. " +
		"Ratings: search/outbox 6/10, conflicts 6/10, MCP 7/10. " +
		"Three critical findings: (1) search outbox worker has no deadletter mechanism, " +
		"(2) Merkle proof verification timing leak, " +
		"(3) ReScore is correctly bounded within [0,1] but has no unit tests."
	claims := SplitClaims(input)
	// Should produce multiple claims including the specific ReScore claim.
	assert.GreaterOrEqual(t, len(claims), 3)

	// The ReScore claim should be isolated.
	foundReScore := false
	for _, c := range claims {
		if len(c) > 20 && contains(c, "ReScore") {
			foundReScore = true
		}
	}
	assert.True(t, foundReScore, "ReScore claim should be extractable from multi-topic outcome")
}

func TestSplitSentences_NoSplitOnDecimal(t *testing.T) {
	input := "Coverage went from 6.5 to 7.2 percent."
	sentences := splitSentences(input)
	// Should not split on "6.5" or "7.2".
	assert.Len(t, sentences, 1)
}

func TestSplitNumberedItems_NoNumbers(t *testing.T) {
	input := "A plain sentence without any numbers."
	parts := splitNumberedItems(input)
	assert.Equal(t, []string{input}, parts)
}

func TestSplitClaims_MarkdownList(t *testing.T) {
	input := "Key findings:\n- The outbox has no deadletter mechanism\n- Merkle proof has a timing leak\n- ReScore has no unit tests"
	claims := SplitClaims(input)
	assert.Contains(t, claims, "The outbox has no deadletter mechanism")
	assert.Contains(t, claims, "Merkle proof has a timing leak")
	assert.Contains(t, claims, "ReScore has no unit tests")
}

func TestSplitClaims_MarkdownListAsterisk(t *testing.T) {
	input := "Issues found:\n* Buffer flush retry can reorder events\n* Reconnect holds mutex during sleep"
	claims := SplitClaims(input)
	assert.Contains(t, claims, "Buffer flush retry can reorder events")
	assert.Contains(t, claims, "Reconnect holds mutex during sleep")
}

func TestSplitClaims_NoMarkdownList(t *testing.T) {
	// Text without list markers should pass through unchanged.
	input := "The system is working correctly with no issues. All integration tests pass successfully."
	claims := SplitClaims(input)
	assert.Equal(t, []string{
		"The system is working correctly with no issues.",
		"All integration tests pass successfully.",
	}, claims)
}

func TestSplitClaims_Semicolons(t *testing.T) {
	input := "Three issues identified: outbox has no deadletter mechanism; Merkle proof has a timing vulnerability; buffer retry ordering is non-deterministic"
	claims := SplitClaims(input)
	assert.Len(t, claims, 3)
	// First part includes the preamble because the colon is not a sentence boundary.
	assert.Contains(t, claims, "Three issues identified: outbox has no deadletter mechanism")
	assert.Contains(t, claims, "Merkle proof has a timing vulnerability")
	assert.Contains(t, claims, "buffer retry ordering is non-deterministic")
}

func TestSplitClaims_SemicolonsTooShort(t *testing.T) {
	// Short parts after semicolons should not cause a split.
	input := "The score is 6/10; OK; and it works fine."
	claims := SplitClaims(input)
	// "OK" is too short, so semicolons should not split.
	assert.Len(t, claims, 1)
	assert.Equal(t, "The score is 6/10; OK; and it works fine.", claims[0])
}

func TestSplitMarkdownLists_MixedContent(t *testing.T) {
	input := "Summary of review.\n- First issue found in storage layer\n- Second issue in auth middleware\nOverall the system works."
	parts := splitMarkdownLists(input)
	assert.Equal(t, []string{
		"Summary of review.",
		"First issue found in storage layer",
		"Second issue in auth middleware",
		"Overall the system works.",
	}, parts)
}

func TestSplitSemicolons_NoSemicolons(t *testing.T) {
	input := "A plain sentence without semicolons."
	parts := splitSemicolons(input)
	assert.Equal(t, []string{input}, parts)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsSubstring(s, substr)
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
