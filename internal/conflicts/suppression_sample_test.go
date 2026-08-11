//go:build !lite

package conflicts

import (
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The sampling hash must not depend on which decision the scorer happened to
// be scoring: the same pair is reached from both sides across rescores, and
// suppressed_pair_samples stores one canonically-ordered row per pair.
// Fails if orderPair is dropped from suppressionSampled.
func TestSuppressionSampled_SideIndependent(t *testing.T) {
	for range 1000 {
		a, b := uuid.New(), uuid.New()
		assert.Equal(t, suppressionSampled(a, b, 0.05), suppressionSampled(b, a, 0.05),
			"sampling verdict must be side-independent for pair (%s, %s)", a, b)
	}
}

// Deterministic, not random. This is the specific regression that would
// silently unbound the corpus: a rescore re-suppresses the same pairs, so with
// math/rand the sampled set walks toward 100% of all suppressed pairs over
// successive rescores and "a 5% sample" stops being true.
func TestSuppressionSampled_Deterministic(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	want := suppressionSampled(a, b, 0.05)
	for range 100 {
		assert.Equal(t, want, suppressionSampled(a, b, 0.05),
			"repeated calls for the same pair must agree")
	}
}

func TestSuppressionSampled_RateBounds(t *testing.T) {
	for range 10_000 {
		a, b := uuid.New(), uuid.New()
		assert.False(t, suppressionSampled(a, b, 0), "rate 0 must sample nothing")
		assert.True(t, suppressionSampled(a, b, 1), "rate 1 must sample everything")
	}
}

func TestSuppressionSampled_ApproximatesRate(t *testing.T) {
	const n = 10_000
	hits := 0
	for range n {
		if suppressionSampled(uuid.New(), uuid.New(), 0.05) {
			hits++
		}
	}
	// Expected 500. The window is wide enough that a healthy hash never trips
	// it and narrow enough that an off-by-an-order-of-magnitude bug does.
	assert.GreaterOrEqual(t, hits, 400, "sampled %d of %d at rate 0.05", hits, n)
	assert.LessOrEqual(t, hits, 600, "sampled %d of %d at rate 0.05", hits, n)
}

// Every structural suppression rule must have a sampleSuppression call at its
// drop site. This is asserted at the source level on purpose: the property
// being enforced is a call-site property, and a guard present on 11 of 12
// sibling sites is a recurring blind-spot bug class in this codebase — the
// blind spot lands exactly where the new rule is wrong. A rename breaks this
// test loudly in the same file, which is the correct failure mode.
func TestStructuralSuppressionRules_CallSiteParity(t *testing.T) {
	src, err := os.ReadFile("scorer.go")
	require.NoError(t, err)
	body := string(src)

	assert.Equal(t, len(structuralSuppressionRules),
		strings.Count(body, "s.sampleSuppression(&suppressionSamples,"),
		"every rule in structuralSuppressionRules needs exactly one sampleSuppression call site in scoreForDecision")

	seen := make(map[string]bool, len(structuralSuppressionRules))
	for _, rule := range structuralSuppressionRules {
		assert.False(t, seen[rule], "duplicate rule name %q in structuralSuppressionRules", rule)
		seen[rule] = true
		assert.Equal(t, 1, strings.Count(body, `"`+rule+`"`),
			"rule value %q must appear exactly once in scorer.go (its const declaration)", rule)
	}
}
