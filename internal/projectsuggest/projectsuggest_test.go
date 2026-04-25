package projectsuggest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSuggest_SubstringMatch(t *testing.T) {
	// The dominant incident pattern: an agent passes an org-prefixed or
	// path-style name when the canonical is the basename. Substring
	// containment must surface the canonical as the top suggestion.
	known := []string{"akashi", "mono", "tiger"}

	cases := []struct {
		submitted string
		want      string
	}{
		{"ardent-mono", "mono"},
		{"ArdentAILabs/mono", "mono"},
		{"AshitaAIlabs/mono", "mono"},
		{"akashi-server", "akashi"},
		{"tigers", "tiger"},
	}
	for _, tc := range cases {
		t.Run(tc.submitted, func(t *testing.T) {
			got := Suggest(tc.submitted, known)
			assert.Contains(t, got, tc.want, "expected %q in suggestions for %q (got %v)", tc.want, tc.submitted, got)
		})
	}
}

func TestSuggest_NoMatch(t *testing.T) {
	// Genuinely unrelated names should not be suggested. This guards
	// against a noisy error message that points the agent at the wrong
	// canonical.
	known := []string{"akashi", "mono", "tiger"}
	cases := []string{
		"casablanca",
		"totally-hallucinated-xxx",
		"riyadh-v1",
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			assert.Empty(t, Suggest(tc, known))
		})
	}
}

func TestSuggest_OrdersByScore(t *testing.T) {
	// When several known projects clear the threshold, the closest match
	// must come first so the agent's first-choice retry is the canonical.
	known := []string{"mono-server", "mono", "monorepo"}
	got := Suggest("monoo", known)
	assert.NotEmpty(t, got)
	assert.Equal(t, "mono", got[0], "exact-substring 'mono' should outrank longer matches")
}

func TestSuggest_LimitsToMax(t *testing.T) {
	// Even when many candidates clear the threshold, we cap output so the
	// rejection message stays scannable.
	known := []string{"akashi", "akashi-1", "akashi-2", "akashi-3", "akashi-4"}
	got := Suggest("akashi", known)
	assert.LessOrEqual(t, len(got), MaxSuggestions)
}

func TestSuggest_SkipsExactMatch(t *testing.T) {
	// If the submitted name is itself in the known list, callers shouldn't
	// be in this code path. Defensive: never suggest "X" for "X".
	known := []string{"mono", "akashi"}
	got := Suggest("mono", known)
	assert.NotContains(t, got, "mono")
}

func TestSuggest_EmptyInputs(t *testing.T) {
	assert.Nil(t, Suggest("", []string{"a", "b"}))
	assert.Nil(t, Suggest("foo", nil))
	assert.Nil(t, Suggest("foo", []string{}))
}

func TestFormatRejectionSuffix_BothSections(t *testing.T) {
	known := []string{"akashi", "mono", "tiger"}
	msg := FormatRejectionSuffix("ardent-mono", known)
	assert.Contains(t, msg, "Did you mean")
	assert.Contains(t, msg, `"mono"`)
	assert.Contains(t, msg, "Known projects")
}

func TestFormatRejectionSuffix_NoMatchOmitsDidYouMean(t *testing.T) {
	// When nothing clears the threshold, the agent still benefits from
	// seeing the full project list — but the misleading "Did you mean"
	// header should not appear.
	known := []string{"akashi", "mono", "tiger"}
	msg := FormatRejectionSuffix("casablanca", known)
	assert.NotContains(t, msg, "Did you mean")
	assert.Contains(t, msg, "Known projects")
}

func TestFormatRejectionSuffix_TruncatesLongList(t *testing.T) {
	// Prevent the rejection from becoming a wall of text in orgs with
	// many projects.
	many := make([]string, 25)
	for i := range many {
		many[i] = "p-" + string(rune('a'+i))
	}
	msg := FormatRejectionSuffix("zzz", many)
	assert.Contains(t, msg, "…", "long lists should be truncated with an ellipsis marker")
}

func TestFormatRejectionSuffix_EmptyKnown(t *testing.T) {
	// No projects in the org → suppress the appendix entirely. The
	// "first project bootstrap" path handles this case upstream.
	assert.Equal(t, "", FormatRejectionSuffix("anything", nil))
}

func TestLevenshtein_KnownDistances(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"kitten", "sitting", 3},
		{"casablanca", "mono", 9},
	}
	for _, tc := range cases {
		name := tc.a + "_" + tc.b
		if name == "_" {
			name = "both_empty"
		}
		t.Run(name, func(t *testing.T) {
			got := levenshtein(tc.a, tc.b)
			assert.Equal(t, tc.want, got, "levenshtein(%q,%q)", tc.a, tc.b)
		})
	}
}

func TestSimilarityScore_SubstringWinsOverEditDistance(t *testing.T) {
	// "ardent-mono" → "mono": Levenshtein distance is 7, normalized
	// similarity would be ~0.36 (below threshold). Substring containment
	// must override so the canonical is suggested.
	score := similarityScore("ardent-mono", "mono")
	assert.GreaterOrEqual(t, score, Threshold,
		"substring match must clear the suggestion threshold (got %.3f)", score)
}

func TestFormatRejectionSuffix_LeadingSpace(t *testing.T) {
	// Callers concatenate this onto an existing sentence — the suffix
	// must start with a space to avoid awkward joins.
	known := []string{"mono"}
	msg := FormatRejectionSuffix("ardent-mono", known)
	assert.True(t, strings.HasPrefix(msg, " "), "suffix should start with a space, got %q", msg)
}
