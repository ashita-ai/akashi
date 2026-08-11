//go:build !lite

package conflicts

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The supersession contract: a SUPERSESSION verdict must name WHICH side
// retired the other on its REPLACES line, and a verdict that cannot is
// downgraded rather than trusted.
//
// It is the symmetric sibling of the contradiction contract in
// question_contract_test.go, and exists for the same measured reason: the
// prompt already refuses recency as evidence ("every pair you see has a time
// order, so ordering alone means nothing"), but before this contract the
// scorer re-derived supersedes direction from ValidFrom one layer down —
// reinstating exactly what the prompt rejects, and getting backdated traces,
// late-filed decisions, and reverts backwards.
//
// The contract gates on the side token only. "Did you quote real replacement
// language" is not machine-checkable, and enforcing it would only teach the
// model to emit filler — the failure mode #740 documents.

func TestParseValidatorResponse_SupersessionWithoutReplacesIsDowngraded(t *testing.T) {
	result, err := ParseValidatorResponse(
		"RELATIONSHIP: supersession\nCATEGORY: strategic\nSEVERITY: medium\nEXPLANATION: B replaces A.")
	require.NoError(t, err)

	assert.Equal(t, "refinement", result.Relationship,
		"a supersession that names no superseding side must not stand")
	assert.False(t, result.IsConflict(), "downgraded verdicts must not open conflicts")
	assert.Empty(t, result.SupersedingSide)
	assert.Empty(t, result.ReplacementEvidence)
	assert.Contains(t, result.Explanation, "downgraded",
		"the downgrade must be visible in the explanation, not silent")
	assert.Contains(t, result.Explanation, "B replaces A.",
		"the validator's own explanation must be preserved")
}

func TestParseValidatorResponse_SupersessionWithReplacesSurvives(t *testing.T) {
	result, err := ParseValidatorResponse(
		"RELATIONSHIP: supersession\nREPLACES: B: replaced REST v1 with gRPC\n" +
			"CATEGORY: strategic\nSEVERITY: medium\nEXPLANATION: the gRPC decision retires the REST one.")
	require.NoError(t, err)

	assert.Equal(t, "supersession", result.Relationship)
	assert.True(t, result.IsConflict())
	assert.Equal(t, "B", result.SupersedingSide)
	assert.Equal(t, "replaced REST v1 with gRPC", result.ReplacementEvidence)
	assert.NotContains(t, result.Explanation, "downgraded")
}

// The side token alone satisfies the contract. Quoted replacement language is
// advisory: it enriches the supersedes-suggestion reason but cannot be
// validated, and gating on it would produce filler rather than evidence.
func TestParseValidatorResponse_ReplacesSideOnlyIsSufficient(t *testing.T) {
	result, err := ParseValidatorResponse(
		"RELATIONSHIP: supersession\nREPLACES: A\nEXPLANATION: x")
	require.NoError(t, err)

	assert.Equal(t, "supersession", result.Relationship)
	assert.Equal(t, "A", result.SupersedingSide)
	assert.Empty(t, result.ReplacementEvidence)
	assert.NotContains(t, result.Explanation, "downgraded")
}

// Two failure families must both miss the contract: the placeholder spellings
// models emit for "nothing here", and the answers that mean "I could not pick a
// side". The second family is why the accepted set is closed rather than
// "anything non-placeholder" — "Both" would otherwise read as B and
// "Ambiguous" as A, turning the two clearest non-answers into confident
// directions.
func TestParseValidatorResponse_PlaceholderReplacesDoesNotSatisfyContract(t *testing.T) {
	for _, placeholder := range []string{
		"n/a", "N/A", "none", "NONE", "null", "nil", "not applicable", "-", "  ", "\"n/a\"", "[n/a]", "N/A.",
		"both", "Both decisions", "A/B", "ambiguous", "neither", "unclear",
		// Enumerations. The value's job is to SELECT one side; naming both is a
		// refusal to choose. The first of these is the prompt's own response-format
		// template verbatim — an echoed template is a routine LLM failure, and it
		// parsed as a confident selection of side A until this was rejected.
		"A or B, then the replacement language",
		"A and B", "A, B", "A or B", "B and A",
		// Named-then-retracted. Storing a withdrawal as the justification for a
		// durable, agent-facing link is worse than storing nothing.
		"A: no explicit replacement language found",
		"A. Actually neither, they are complementary",
		"B: none found",
		// The English article, not a side label.
		"a bit unclear", "a little of both",
	} {
		t.Run(placeholder, func(t *testing.T) {
			result, err := ParseValidatorResponse(fmt.Sprintf(
				"RELATIONSHIP: supersession\nREPLACES: %s\nEXPLANATION: x", placeholder))
			require.NoError(t, err)
			assert.Equal(t, "refinement", result.Relationship,
				"placeholder %q must not satisfy the named-side requirement", placeholder)
			assert.Empty(t, result.SupersedingSide, "placeholder %q must not resolve to a side", placeholder)
		})
	}
}

// A genuine REPLACES value wrapped in markdown must survive — the trimming must
// not be so aggressive that it eats the side token or the evidence.
func TestParseValidatorResponse_MarkdownWrappedReplacesSurvives(t *testing.T) {
	result, err := ParseValidatorResponse(
		"RELATIONSHIP: supersession\nREPLACES: **B — replaced REST v1**\nEXPLANATION: x")
	require.NoError(t, err)

	assert.Equal(t, "supersession", result.Relationship)
	assert.Equal(t, "B", result.SupersedingSide)
	assert.Equal(t, "replaced REST v1", result.ReplacementEvidence)
}

func TestParseValidatorResponse_ReplacesAcceptsDecisionPrefixAndCase(t *testing.T) {
	cases := []struct {
		raw          string
		expectedSide string
	}{
		{"Decision B", "B"},
		{"decision b — x", "B"},
		{"b", "B"},
		{"**A**", "A"},
		{"[A]", "A"},
		{"A: withdrew the earlier plan", "A"},
		// Decorated token FOLLOWED BY evidence — the exact shape the prompt asks
		// for. normalizeValue only unwraps a value's outer ends, so the closing
		// "**" survives as an interior character; these all failed until the
		// separator set covered it, and the failure was a silent downgrade.
		{"**B**: replaced REST v1 with gRPC", "B"},
		{"**Decision B**: replaced REST v1", "B"},
		{`"B": replaced REST v1`, "B"},
		{"B — replaced REST v1 with gRPC", "B"},
		{"B, replaced REST v1", "B"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			result, err := ParseValidatorResponse(fmt.Sprintf(
				"RELATIONSHIP: supersession\nREPLACES: %s\nEXPLANATION: x", tc.raw))
			require.NoError(t, err)
			assert.Equal(t, "supersession", result.Relationship)
			assert.Equal(t, tc.expectedSide, result.SupersedingSide)
		})
	}
}

// Non-supersession verdicts carry no side, even when the model supplies one, so
// downstream consumers can treat a populated SupersedingSide as proof the judge
// actually chose a direction. Exact mirror of the question-contract sibling.
func TestParseValidatorResponse_ReplacesClearedForNonSupersession(t *testing.T) {
	for _, rel := range []string{"contradiction", "complementary", "refinement", "unrelated"} {
		// contradiction owes a QUESTION or it would be downgraded, and this
		// test is not about that contract.
		var extra string
		if rel == "contradiction" {
			extra = "QUESTION: whether to use Redis\n"
		}
		result, err := ParseValidatorResponse(fmt.Sprintf(
			"RELATIONSHIP: %s\n%sREPLACES: A: replaced the earlier plan\nEXPLANATION: x", rel, extra))
		require.NoError(t, err, "relationship=%s", rel)
		assert.Equal(t, rel, result.Relationship)
		assert.Empty(t, result.SupersedingSide, "relationship=%s must not carry a superseding side", rel)
		assert.Empty(t, result.ReplacementEvidence, "relationship=%s must not carry replacement evidence", rel)
	}
}

// The contract must run AFTER the truncation-alias switch, otherwise a model
// that shortens "supersession" to "supersede" escapes it entirely.
func TestParseValidatorResponse_SupersedeAliasStillBoundByContract(t *testing.T) {
	withSide, err := ParseValidatorResponse("RELATIONSHIP: supersede\nREPLACES: A\nEXPLANATION: x")
	require.NoError(t, err)
	assert.Equal(t, "supersession", withSide.Relationship)
	assert.Equal(t, "A", withSide.SupersedingSide)

	withoutSide, err := ParseValidatorResponse("RELATIONSHIP: supersede\nEXPLANATION: x")
	require.NoError(t, err)
	assert.Equal(t, "refinement", withoutSide.Relationship,
		"the alias must be normalized before the contract runs, not after")
}

// The question contract exempts the pre-taxonomy "VERDICT: yes/no" form because
// that prompt never asked for a question. The supersession contract needs no
// such exemption and deliberately carries no "&& !legacyVerdict" conjunct: the
// legacy form can only ever produce "contradiction" or "unrelated", so the
// conjunct would be dead code implying a reachable path that does not exist.
// This test pins that reasoning — if the legacy mapping ever grows a
// supersession branch, it fails here and the exemption question gets reopened.
func TestParseValidatorResponse_LegacyVerdictCannotProduceSupersession(t *testing.T) {
	yes, err := ParseValidatorResponse("VERDICT: yes\nCATEGORY: assessment\nSEVERITY: high\nEXPLANATION: legacy format")
	require.NoError(t, err)
	assert.Equal(t, "contradiction", yes.Relationship,
		"the legacy form maps yes→contradiction; it can never reach supersession")
	assert.NotContains(t, yes.Explanation, "downgraded")

	no, err := ParseValidatorResponse("VERDICT: no\nCATEGORY: factual\nSEVERITY: low\nEXPLANATION: legacy format")
	require.NoError(t, err)
	assert.Equal(t, "unrelated", no.Relationship,
		"the legacy form maps no→unrelated; it can never reach supersession")
	assert.NotContains(t, no.Explanation, "downgraded")
}

// The two contracts must not interfere: a contradiction missing its question
// downgrades to complementary regardless of any REPLACES line the model
// volunteered.
func TestParseValidatorResponse_ContradictionContractUnaffectedByReplaces(t *testing.T) {
	result, err := ParseValidatorResponse("RELATIONSHIP: contradiction\nREPLACES: A\nEXPLANATION: x")
	require.NoError(t, err)

	assert.Equal(t, "complementary", result.Relationship,
		"a REPLACES line is not a substitute for the disputed question")
	assert.Empty(t, result.SupersedingSide)
	assert.Contains(t, result.Explanation, "no disputed question")
}
