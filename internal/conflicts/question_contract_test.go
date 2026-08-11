//go:build !lite && integration

package conflicts

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The contradiction contract: a CONTRADICTION verdict must name the single
// question the two decisions answer differently, and a verdict that cannot is
// downgraded rather than trusted.
//
// This is enforced in the parser, not the prompt, because prompt-level hints
// for this failure class were measured not to work: full-corpus gold labelling
// found the validator returning CONTRADICTION for 97.8% of pairs regardless of
// how many "IMPORTANT: this is not a contradiction when..." clauses the prompt
// carried.

func TestParseValidatorResponse_ContradictionWithoutQuestionIsDowngraded(t *testing.T) {
	result, err := ParseValidatorResponse(
		"RELATIONSHIP: contradiction\nCATEGORY: strategic\nSEVERITY: high\nEXPLANATION: They disagree.")
	require.NoError(t, err)

	assert.Equal(t, "complementary", result.Relationship,
		"a contradiction that names no disputed question must not stand")
	assert.False(t, result.IsConflict(), "downgraded verdicts must not open conflicts")
	assert.Empty(t, result.SharedQuestion)
	assert.Contains(t, result.Explanation, "downgraded",
		"the downgrade must be visible in the explanation, not silent")
	assert.Contains(t, result.Explanation, "They disagree.",
		"the validator's own explanation must be preserved")
}

func TestParseValidatorResponse_ContradictionWithQuestionSurvives(t *testing.T) {
	result, err := ParseValidatorResponse(
		"RELATIONSHIP: contradiction\nQUESTION: whether to run CreateFieldIndex at startup\n" +
			"CATEGORY: factual\nSEVERITY: critical\nEXPLANATION: planner and coder take opposite positions.")
	require.NoError(t, err)

	assert.Equal(t, "contradiction", result.Relationship)
	assert.True(t, result.IsConflict())
	assert.Equal(t, "whether to run CreateFieldIndex at startup", result.SharedQuestion)
	assert.NotContains(t, result.Explanation, "downgraded")
}

// Models emit several spellings of "there is no question here". Each must fail
// the contract, otherwise the placeholder becomes a bypass that reinstates the
// 97.8%-contradiction behavior the contract exists to stop.
func TestParseValidatorResponse_PlaceholderQuestionsDoNotSatisfyContract(t *testing.T) {
	for _, placeholder := range []string{"n/a", "N/A", "none", "NONE", "null", "nil", "not applicable", "-", "  ", "\"n/a\"", "[n/a]", "N/A."} {
		t.Run(placeholder, func(t *testing.T) {
			result, err := ParseValidatorResponse(fmt.Sprintf(
				"RELATIONSHIP: contradiction\nQUESTION: %s\nEXPLANATION: x", placeholder))
			require.NoError(t, err)
			assert.Equal(t, "complementary", result.Relationship,
				"placeholder %q must not satisfy the named-question requirement", placeholder)
		})
	}
}

// Non-contradiction verdicts carry no question, even when the model supplies
// one, so downstream consumers can treat a populated SharedQuestion as proof a
// dispute was actually identified.
func TestParseValidatorResponse_QuestionClearedForNonContradiction(t *testing.T) {
	for _, rel := range []string{"supersession", "complementary", "refinement", "unrelated"} {
		// A supersession verdict owes a REPLACES side; without it the parser
		// downgrades to refinement and this test would be asserting the wrong
		// relationship rather than the question-clearing property it is about.
		var extra string
		if rel == "supersession" {
			extra = "REPLACES: A\n"
		}
		result, err := ParseValidatorResponse(fmt.Sprintf(
			"RELATIONSHIP: %s\nQUESTION: whether to use Redis\n%sEXPLANATION: x", rel, extra))
		require.NoError(t, err, "relationship=%s", rel)
		assert.Equal(t, rel, result.Relationship)
		assert.Empty(t, result.SharedQuestion, "relationship=%s must not carry a disputed question", rel)
	}
}

// The pre-taxonomy "VERDICT: yes" form comes from a prompt that never asked for
// a question. Applying the contract to it would downgrade every legacy response
// to complementary, silently disabling conflict detection for any caller still
// on that format.
func TestParseValidatorResponse_LegacyVerdictExemptFromContract(t *testing.T) {
	result, err := ParseValidatorResponse("VERDICT: yes\nCATEGORY: assessment\nSEVERITY: high\nEXPLANATION: legacy format")
	require.NoError(t, err)

	assert.Equal(t, "contradiction", result.Relationship,
		"legacy VERDICT responses predate the question contract and must not be downgraded")
	assert.True(t, result.IsConflict())
	assert.NotContains(t, result.Explanation, "downgraded")
}

// An explicit RELATIONSHIP line is the new format and IS subject to the
// contract, even when a legacy VERDICT line is also present.
func TestParseValidatorResponse_ExplicitRelationshipStillBoundByContract(t *testing.T) {
	result, err := ParseValidatorResponse("RELATIONSHIP: contradiction\nVERDICT: yes\nEXPLANATION: both present")
	require.NoError(t, err)

	assert.Equal(t, "complementary", result.Relationship,
		"a RELATIONSHIP line means the model saw the current prompt and owed a question")
}

func TestRelationshipEquivalent(t *testing.T) {
	assert.True(t, RelationshipEquivalent("contradiction", "contradiction"))
	assert.True(t, RelationshipEquivalent("complementary", "refinement"),
		"the gold taxonomy has one compatible-but-related bucket where the validator has two")
	assert.True(t, RelationshipEquivalent("refinement", "complementary"))
	assert.False(t, RelationshipEquivalent("contradiction", "supersession"),
		"conflating dispute with timeline is the error the taxonomy exists to catch")
	assert.False(t, RelationshipEquivalent("supersession", "complementary"))
	assert.False(t, RelationshipEquivalent("unrelated", "complementary"))
}

func TestGoldSummary(t *testing.T) {
	pairs := []EvalPair{
		{ExpectedRelationship: "contradiction"},
		{ExpectedRelationship: "supersession"},
		{ExpectedRelationship: "complementary"},
		{ExpectedRelationship: "complementary"},
	}
	s := GoldSummary(pairs)
	assert.Contains(t, s, "gold set: 4 pairs")
	assert.Contains(t, s, "contradiction=1")
	assert.Contains(t, s, "complementary=2")
	assert.Contains(t, s, "50.0%")
}

// A legacy VERDICT line appearing BEFORE an explicit RELATIONSHIP line must not
// buy an exemption from the question contract. Before this was fixed, line
// order alone decided whether the contract applied.
func TestParseValidatorResponse_VerdictBeforeRelationshipStillBoundByContract(t *testing.T) {
	result, err := ParseValidatorResponse("VERDICT: yes\nRELATIONSHIP: contradiction\nEXPLANATION: ordering matters")
	require.NoError(t, err)
	assert.Equal(t, "complementary", result.Relationship,
		"an explicit RELATIONSHIP line means the model saw the current prompt and owes a question")
}

// Markdown-wrapped placeholders must still fail the contract; the key parses
// fine, so a survivor would satisfy the contract with no question at all.
func TestParseValidatorResponse_MarkdownWrappedPlaceholderRejected(t *testing.T) {
	for _, q := range []string{"**n/a**", "*none*", "__N/A__", "**[n/a]**"} {
		t.Run(q, func(t *testing.T) {
			result, err := ParseValidatorResponse(
				"RELATIONSHIP: contradiction\nQUESTION: " + q + "\nEXPLANATION: x")
			require.NoError(t, err)
			assert.Equal(t, "complementary", result.Relationship,
				"markdown-wrapped placeholder %q must not satisfy the contract", q)
		})
	}
}

// A genuine question wrapped in markdown must survive — the trimming must not
// be so aggressive that it eats real content.
func TestParseValidatorResponse_MarkdownWrappedQuestionSurvives(t *testing.T) {
	result, err := ParseValidatorResponse(
		"RELATIONSHIP: contradiction\nQUESTION: **whether to run CreateFieldIndex at startup**\nEXPLANATION: x")
	require.NoError(t, err)
	assert.Equal(t, "contradiction", result.Relationship)
	assert.Equal(t, "whether to run CreateFieldIndex at startup", result.SharedQuestion)
}
