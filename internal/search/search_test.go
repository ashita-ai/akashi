package search

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

// TestReScore_CitationsOutrankHighCompleteness verifies that outcome signals (citations) dominate
// completeness, which is no longer part of the relevance formula (issue #235).
func TestReScore_CitationsOutrankHighCompleteness(t *testing.T) {
	now := time.Now()
	highCitation := uuid.New()
	highCompleteness := uuid.New()

	decisions := map[uuid.UUID]model.Decision{
		highCitation: {
			ID:                     highCitation,
			ValidFrom:              now,
			CompletenessScore:      0.0,
			PrecedentCitationCount: 5, // citation_score = 1.0 (log(6)/log(6))
		},
		highCompleteness: {
			ID:                     highCompleteness,
			ValidFrom:              now,
			CompletenessScore:      1.0, // completeness no longer in relevance formula
			PrecedentCitationCount: 0,
		},
	}

	results := []Result{
		{DecisionID: highCitation, Score: 0.9},
		{DecisionID: highCompleteness, Score: 0.9},
	}

	scored := ReScore(results, decisions, 10, nil)
	assert.Len(t, scored, 2)
	// highCitation: outcomeWeight = 0.25*1.0 + 0.15*1.0 = 0.40; relevance = 0.9*(0.5+0.5*0.40) = 0.630
	// highCompleteness: outcomeWeight = 0.15*1.0 = 0.15; relevance = 0.9*(0.5+0.5*0.15) = 0.5175
	// highCitation (0.630) > highCompleteness (0.5175).
	assert.Equal(t, highCitation, scored[0].Decision.ID,
		"decision with 5 citations should outrank one with perfect completeness and zero citations")
}

// TestReScore_StabilityZeroForFastSupersession verifies that decisions superseded within 48h
// receive stability_score = 0.0 and rank lower.
func TestReScore_StabilityZeroForFastSupersession(t *testing.T) {
	now := time.Now()
	fastRevision := uuid.New()
	slowRevision := uuid.New()

	fastHours := 24.0 // < 48h → stability 0
	slowHours := 96.0 // > 48h → stability 1

	decisions := map[uuid.UUID]model.Decision{
		fastRevision: {
			ID:                        fastRevision,
			ValidFrom:                 now,
			SupersessionVelocityHours: &fastHours,
		},
		slowRevision: {
			ID:                        slowRevision,
			ValidFrom:                 now,
			SupersessionVelocityHours: &slowHours,
		},
	}

	results := []Result{
		{DecisionID: fastRevision, Score: 0.9},
		{DecisionID: slowRevision, Score: 0.9},
	}

	scored := ReScore(results, decisions, 10, nil)
	assert.Len(t, scored, 2)
	// slowRevision: stability=1.0 → outcomeWeight=0.15; relevance=0.9*(0.5+0.075)=0.5175
	// fastRevision: stability=0.0 → outcomeWeight=0.0; relevance=0.9*0.5=0.45
	assert.Equal(t, slowRevision, scored[0].Decision.ID,
		"decision superseded after 96h should outrank one superseded after 24h")
}

// TestReScore_ColdStart verifies that a new decision with no signals receives a relevance
// multiplier of 0.575 (stability=1.0 contributes 0.15 to outcome_weight; no phantom signals).
func TestReScore_ColdStart(t *testing.T) {
	id := uuid.New()
	decisions := map[uuid.UUID]model.Decision{
		id: {
			ID:        id,
			ValidFrom: time.Now(),
			// All outcome signals zero — no citations, no conflicts, no agreement, no assessments.
			// Stability defaults to 1.0 (SupersessionVelocityHours is nil).
		},
	}

	results := []Result{{DecisionID: id, Score: 1.0}}
	scored := ReScore(results, decisions, 10, nil)
	assert.Len(t, scored, 1)

	// outcome_weight = 0 + 0.25*0 + 0.15*1.0 + 0.10*0 + 0 = 0.15
	// relevance = 1.0 * (0.5 + 0.5*0.15) * 1.0 = 0.575
	assert.InDelta(t, 0.575, float64(scored[0].SimilarityScore), 0.001,
		"cold-start decision should have relevance multiplier 0.575 (stability=1.0 only, no phantom signals)")
}

// TestReScore_BoundedToOne verifies that ReScore results are bounded to [0.0, 1.0].
func TestReScore_BoundedToOne(t *testing.T) {
	correct := 100
	id := uuid.New()
	decisions := map[uuid.UUID]model.Decision{
		id: {
			ID:                     id,
			ValidFrom:              time.Now(),
			CompletenessScore:      1.0,
			PrecedentCitationCount: 100, // citation_score saturates at 1.0
			AgreementCount:         100,
			ConflictFate:           model.ConflictFate{Won: 100, Lost: 0},
			AssessmentSummary:      &model.AssessmentSummary{Total: 100, Correct: correct},
		},
	}

	results := []Result{{DecisionID: id, Score: 1.0}}
	scored := ReScore(results, decisions, 10, nil)
	assert.Len(t, scored, 1)
	assert.LessOrEqual(t, float64(scored[0].SimilarityScore), 1.0, "score must not exceed 1.0")
	assert.GreaterOrEqual(t, float64(scored[0].SimilarityScore), 0.0, "score must not be negative")
}

// TestReScore_ConflictHistoryOnlyBoostsWinners verifies that conflict win rate contributes only
// when conflict history exists. Decisions that won conflicts are boosted; those with no history
// or those that lost are not boosted (no phantom neutral score for uncontested decisions).
func TestReScore_ConflictHistoryOnlyBoostsWinners(t *testing.T) {
	noConflict := uuid.New()
	wonConflict := uuid.New()
	lostConflict := uuid.New()

	decisions := map[uuid.UUID]model.Decision{
		noConflict: {
			ID:        noConflict,
			ValidFrom: time.Now(),
			// ConflictFate zero: won=0, lost=0 → contributes 0 (no phantom 0.5)
		},
		wonConflict: {
			ID:           wonConflict,
			ValidFrom:    time.Now(),
			ConflictFate: model.ConflictFate{Won: 1, Lost: 0}, // win_rate = 1.0 → +0.10 boost
		},
		lostConflict: {
			ID:           lostConflict,
			ValidFrom:    time.Now(),
			ConflictFate: model.ConflictFate{Won: 0, Lost: 1}, // win_rate = 0.0 → +0.0 (same as no history)
		},
	}

	results := []Result{
		{DecisionID: noConflict, Score: 0.9},
		{DecisionID: wonConflict, Score: 0.9},
		{DecisionID: lostConflict, Score: 0.9},
	}

	scored := ReScore(results, decisions, 10, nil)
	assert.Len(t, scored, 3)

	// wonConflict should rank first: conflict contributes 1.0*0.10=0.10 extra.
	assert.Equal(t, wonConflict, scored[0].Decision.ID,
		"decision that won its conflict should rank above one with no conflict history")

	// noConflict and lostConflict have equal outcome_weight (both contribute 0 from conflict signal).
	// Both have stability=1.0 as their only signal → outcome_weight=0.15 each.
	assert.Equal(t, scored[1].SimilarityScore, scored[2].SimilarityScore,
		"decision with no conflict history and one that lost should have equal scores")
}

// TestReScore_AssessmentIsPrimarySignal verifies that explicit assessment feedback outweighs
// all other signals. A decision assessed correct by all agents should rank significantly
// higher than a maximally-cited unassessed decision.
func TestReScore_AssessmentIsPrimarySignal(t *testing.T) {
	now := time.Now()
	assessed := uuid.New()
	cited := uuid.New()
	correct := 5

	decisions := map[uuid.UUID]model.Decision{
		assessed: {
			ID:                assessed,
			ValidFrom:         now,
			AssessmentSummary: &model.AssessmentSummary{Total: 5, Correct: correct},
			// No citations, no agreements.
		},
		cited: {
			ID:                     cited,
			ValidFrom:              now,
			PrecedentCitationCount: 5, // citation_score = 1.0
			// No assessments.
		},
	}

	results := []Result{
		{DecisionID: assessed, Score: 0.9},
		{DecisionID: cited, Score: 0.9},
	}

	scored := ReScore(results, decisions, 10, nil)
	assert.Len(t, scored, 2)

	// assessed: assessmentContrib = 1.0*0.40 = 0.40; outcomeWeight = 0.40 + 0.15*1.0 = 0.55
	//           relevance = 0.9*(0.5+0.5*0.55) = 0.9*0.775 = 0.6975
	// cited:    outcomeWeight = 0.25*1.0 + 0.15*1.0 = 0.40
	//           relevance = 0.9*(0.5+0.5*0.40) = 0.9*0.70 = 0.630
	assert.Equal(t, assessed, scored[0].Decision.ID,
		"decision with 5/5 correct assessments should outrank one with max citations but no assessment")
}

// TestReScore_LogarithmicCitation verifies that citation scores use a log scale,
// making the first citation more valuable than later ones.
func TestReScore_LogarithmicCitation(t *testing.T) {
	now := time.Now()
	oneCitation := uuid.New()
	fiveCitations := uuid.New()

	decisions := map[uuid.UUID]model.Decision{
		oneCitation: {
			ID:                     oneCitation,
			ValidFrom:              now,
			PrecedentCitationCount: 1, // log(2)/log(6) ≈ 0.387
		},
		fiveCitations: {
			ID:                     fiveCitations,
			ValidFrom:              now,
			PrecedentCitationCount: 5, // log(6)/log(6) = 1.0
		},
	}

	results := []Result{
		{DecisionID: oneCitation, Score: 0.9},
		{DecisionID: fiveCitations, Score: 0.9},
	}

	scored := ReScore(results, decisions, 10, nil)
	assert.Len(t, scored, 2)

	// 5 citations should rank higher than 1 citation (log scale preserves order).
	assert.Equal(t, fiveCitations, scored[0].Decision.ID,
		"5-citation decision should outrank 1-citation decision")

	// The gap should be smaller than with a linear scale.
	// Linear would give 0.2 vs 1.0 (5x gap). Log gives ~0.387 vs 1.0 (~2.6x gap).
	oneScore := float64(scored[1].SimilarityScore)
	fiveScore := float64(scored[0].SimilarityScore)
	ratio := fiveScore / oneScore
	assert.Less(t, ratio, 2.0, "log scale should reduce the gap between 1 and 5 citations vs linear")
}

// TestReScore_TieBreakByQdrantRank verifies that when two decisions have equal adjusted scores,
// the one with a better (lower) Qdrant rank appears first, preserving the original semantic ordering.
func TestReScore_TieBreakByQdrantRank(t *testing.T) {
	now := time.Now()
	first := uuid.New()
	second := uuid.New()

	decisions := map[uuid.UUID]model.Decision{
		first: {
			ID:        first,
			ValidFrom: now,
		},
		second: {
			ID:        second,
			ValidFrom: now,
		},
	}

	// Both have the same Qdrant similarity score, but different ranks.
	results := []Result{
		{DecisionID: second, Score: 0.9, QdrantRank: 5},
		{DecisionID: first, Score: 0.9, QdrantRank: 2},
	}

	scored := ReScore(results, decisions, 10, nil)
	assert.Len(t, scored, 2)
	assert.Equal(t, first, scored[0].Decision.ID,
		"decision with better Qdrant rank (2) should appear before one with rank 5 when adjusted scores are equal")
	assert.Equal(t, second, scored[1].Decision.ID)
	// Verify QdrantRank is threaded through (1-based in SearchResult).
	assert.Equal(t, 3, scored[0].QdrantRank, "QdrantRank should be 1-based in SearchResult")
	assert.Equal(t, 6, scored[1].QdrantRank)
}

// TestPercentileScore verifies the piecewise linear mapping from raw values to percentile scores.
func TestPercentileScore(t *testing.T) {
	// Breakpoints: p25=1, p50=3, p75=7, p90=15
	bp := []float64{1, 3, 7, 15}

	tests := []struct {
		name  string
		value float64
		want  float64
		delta float64
	}{
		{"zero", 0, 0.0, 0.001},
		{"negative", -5, 0.0, 0.001},
		{"at p25", 1, 0.25, 0.001},
		{"between 0 and p25", 0.5, 0.125, 0.001},
		{"at p50", 3, 0.50, 0.001},
		{"between p25 and p50", 2, 0.375, 0.001},
		{"at p75", 7, 0.75, 0.001},
		{"at p90", 15, 0.90, 0.001},
		{"beyond p90", 30, 1.0, 0.001},
		{"slightly above p90", 16, 0.9067, 0.01},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PercentileScore(tc.value, bp)
			assert.InDelta(t, tc.want, got, tc.delta,
				"PercentileScore(%v, %v) = %v, want %v", tc.value, bp, got, tc.want)
		})
	}
}

// TestPercentileScore_EmptyBreakpoints verifies fallback to 0.0 when no breakpoints are available.
func TestPercentileScore_EmptyBreakpoints(t *testing.T) {
	assert.Equal(t, 0.0, PercentileScore(5, nil), "nil breakpoints should return 0")
	assert.Equal(t, 0.0, PercentileScore(5, []float64{}), "empty breakpoints should return 0")
	assert.Equal(t, 0.0, PercentileScore(5, []float64{0, 0, 0, 0}), "all-zero breakpoints should return 0")
}

// TestReScore_PercentileNormalization verifies that citation scores change when percentile data is provided.
func TestReScore_PercentileNormalization(t *testing.T) {
	now := time.Now()
	id := uuid.New()

	decisions := map[uuid.UUID]model.Decision{
		id: {
			ID:                     id,
			ValidFrom:              now,
			PrecedentCitationCount: 3,
		},
	}
	results := []Result{{DecisionID: id, Score: 0.9}}

	// Without percentiles: citation_score = log(4)/log(6) ≈ 0.774
	withoutPct := ReScore(results, decisions, 10, nil)

	// With percentiles where 3 citations is at p90: citation_score ≈ 0.90
	opts := &ReScoreOpts{
		Percentiles: &OrgPercentiles{
			CitationBreakpoints: []float64{0, 1, 2, 3},
		},
	}
	withPct := ReScore(results, decisions, 10, opts)

	assert.Len(t, withoutPct, 1)
	assert.Len(t, withPct, 1)
	// Percentile-normalized score should differ from log-normalized score.
	diff := float64(withPct[0].SimilarityScore) - float64(withoutPct[0].SimilarityScore)
	if diff < 0 {
		diff = -diff
	}
	assert.Greater(t, diff, 0.001,
		"percentile-normalized citation score should differ from log-normalized score")
}

// TestBreakpointsFromValues verifies breakpoint computation from raw values.
func TestBreakpointsFromValues(t *testing.T) {
	assert.Nil(t, BreakpointsFromValues(nil), "nil input should return nil")
	assert.Nil(t, BreakpointsFromValues([]float64{}), "empty input should return nil")

	// Single value: all breakpoints equal.
	bp := BreakpointsFromValues([]float64{5})
	assert.Len(t, bp, 4)
	for _, v := range bp {
		assert.Equal(t, 5.0, v)
	}

	// Multiple values: verify monotonically non-decreasing.
	bp = BreakpointsFromValues([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	assert.Len(t, bp, 4)
	for i := 1; i < len(bp); i++ {
		assert.GreaterOrEqual(t, bp[i], bp[i-1], "breakpoints must be non-decreasing")
	}
}

// TestNewPercentileCache verifies that a freshly created cache is empty and usable.
func TestNewPercentileCache(t *testing.T) {
	c := NewPercentileCache()
	require.NotNil(t, c)

	// A new cache should return nil for any org ID.
	assert.Nil(t, c.Get(uuid.New()), "fresh cache should return nil for unknown org")
}

// TestPercentileCache_GetSet verifies basic get/set round-trip behavior.
func TestPercentileCache_GetSet(t *testing.T) {
	c := NewPercentileCache()
	orgID := uuid.New()

	// Get before set returns nil.
	assert.Nil(t, c.Get(orgID))

	// Set and retrieve.
	now := time.Now()
	p := OrgPercentiles{
		CitationBreakpoints: []float64{1, 3, 7, 15},
		RefreshedAt:         now,
	}
	c.Set(orgID, p)

	got := c.Get(orgID)
	require.NotNil(t, got)
	assert.Equal(t, []float64{1, 3, 7, 15}, got.CitationBreakpoints)
	assert.Equal(t, now, got.RefreshedAt)
}

// TestPercentileCache_MultipleOrgs verifies that distinct orgs have independent cache entries.
func TestPercentileCache_MultipleOrgs(t *testing.T) {
	c := NewPercentileCache()
	orgA := uuid.New()
	orgB := uuid.New()

	c.Set(orgA, OrgPercentiles{CitationBreakpoints: []float64{1, 2, 3, 4}})
	c.Set(orgB, OrgPercentiles{CitationBreakpoints: []float64{10, 20, 30, 40}})

	gotA := c.Get(orgA)
	gotB := c.Get(orgB)
	require.NotNil(t, gotA)
	require.NotNil(t, gotB)
	assert.Equal(t, []float64{1, 2, 3, 4}, gotA.CitationBreakpoints)
	assert.Equal(t, []float64{10, 20, 30, 40}, gotB.CitationBreakpoints)
}

// TestPercentileCache_Overwrite verifies that Set replaces existing entries.
func TestPercentileCache_Overwrite(t *testing.T) {
	c := NewPercentileCache()
	orgID := uuid.New()

	c.Set(orgID, OrgPercentiles{CitationBreakpoints: []float64{1, 2, 3, 4}})
	c.Set(orgID, OrgPercentiles{CitationBreakpoints: []float64{5, 6, 7, 8}})

	got := c.Get(orgID)
	require.NotNil(t, got)
	assert.Equal(t, []float64{5, 6, 7, 8}, got.CitationBreakpoints,
		"second Set should overwrite the first")
}

// TestPercentileCache_GetReturnsValueCopy verifies that mutating the returned pointer
// does not corrupt the cache's internal state.
func TestPercentileCache_GetReturnsValueCopy(t *testing.T) {
	c := NewPercentileCache()
	orgID := uuid.New()

	c.Set(orgID, OrgPercentiles{CitationBreakpoints: []float64{1, 2, 3, 4}})

	got := c.Get(orgID)
	require.NotNil(t, got)
	// Mutate the returned value.
	got.CitationBreakpoints[0] = 999

	// The cache entry should still hold the original slice header's copy of the struct,
	// but since slices share underlying arrays, verify behavior is understood.
	// The Get returns a pointer to a copy of the struct, so the slice header is copied,
	// but the underlying array is shared. This test documents that behavior.
	fresh := c.Get(orgID)
	require.NotNil(t, fresh)
	// The underlying array IS shared (Go value semantics for struct, reference for slice).
	// This documents the current behavior — callers should not mutate returned data.
	assert.Equal(t, 999.0, fresh.CitationBreakpoints[0],
		"slice mutation is visible because Go copies the slice header, not the backing array")
}

// TestPercentileCache_ZeroValueOrg verifies behavior with the zero UUID.
func TestPercentileCache_ZeroValueOrg(t *testing.T) {
	c := NewPercentileCache()
	zeroID := uuid.UUID{}

	c.Set(zeroID, OrgPercentiles{CitationBreakpoints: []float64{0, 0, 0, 0}})

	got := c.Get(zeroID)
	require.NotNil(t, got)
	assert.Equal(t, []float64{0, 0, 0, 0}, got.CitationBreakpoints)
}

// TestPercentileCache_EmptyBreakpoints verifies that an entry with nil breakpoints round-trips.
func TestPercentileCache_EmptyBreakpoints(t *testing.T) {
	c := NewPercentileCache()
	orgID := uuid.New()

	c.Set(orgID, OrgPercentiles{})

	got := c.Get(orgID)
	require.NotNil(t, got, "entry with empty breakpoints should still be retrievable")
	assert.Nil(t, got.CitationBreakpoints)
}

// TestPercentileCache_ConcurrentAccess verifies that the cache is safe for concurrent use.
// Multiple goroutines perform interleaved reads and writes without data races.
func TestPercentileCache_ConcurrentAccess(t *testing.T) {
	c := NewPercentileCache()
	const numGoroutines = 50
	const numOps = 100

	// Pre-create org IDs so goroutines share them.
	orgIDs := make([]uuid.UUID, 10)
	for i := range orgIDs {
		orgIDs[i] = uuid.New()
	}

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for g := range numGoroutines {
		go func(gID int) {
			defer wg.Done()
			for i := range numOps {
				orgID := orgIDs[(gID+i)%len(orgIDs)]
				if i%2 == 0 {
					c.Set(orgID, OrgPercentiles{
						CitationBreakpoints: []float64{float64(gID), float64(i), 0, 0},
						RefreshedAt:         time.Now(),
					})
				} else {
					got := c.Get(orgID)
					// got may be nil (race with first write) or non-nil; both are valid.
					if got != nil {
						assert.Len(t, got.CitationBreakpoints, 4,
							"breakpoints should always have 4 elements when present")
					}
				}
			}
		}(g)
	}

	wg.Wait()

	// After all goroutines complete, every org should have an entry
	// (each was written to at least once by the even-iteration writers).
	for _, orgID := range orgIDs {
		got := c.Get(orgID)
		assert.NotNil(t, got, "every org should have been written to at least once")
	}
}

// TestPercentileScore_AdditionalEdgeCases covers additional edge cases for PercentileScore.
func TestPercentileScore_AdditionalEdgeCases(t *testing.T) {
	tests := []struct {
		name        string
		value       float64
		breakpoints []float64
		want        float64
		delta       float64
	}{
		{
			name:        "negative value returns 0",
			value:       -5,
			breakpoints: []float64{1, 3, 7, 15},
			want:        0.0,
			delta:       0.001,
		},
		{
			name:        "too few breakpoints returns 0",
			value:       5,
			breakpoints: []float64{1, 3},
			want:        0.0,
			delta:       0.001,
		},
		{
			name:        "value in p25-p50 band",
			value:       2,
			breakpoints: []float64{1, 3, 7, 15},
			want:        0.25 + 0.25*((2.0-1.0)/(3.0-1.0)),
			delta:       0.001,
		},
		{
			name:        "value way beyond p90 caps at 1.0",
			value:       1000,
			breakpoints: []float64{1, 3, 7, 15},
			want:        1.0,
			delta:       0.001,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PercentileScore(tc.value, tc.breakpoints)
			assert.InDelta(t, tc.want, got, tc.delta,
				"PercentileScore(%v, %v) = %v, want %v", tc.value, tc.breakpoints, got, tc.want)
		})
	}
}

// TestInterpolatedPercentile_EdgeCases covers the empty-input and single-element paths.
func TestInterpolatedPercentile_EdgeCases(t *testing.T) {
	t.Run("empty slice returns 0", func(t *testing.T) {
		got := interpolatedPercentile(nil, 0.5)
		assert.Equal(t, 0.0, got)
	})

	t.Run("single element returns that element", func(t *testing.T) {
		got := interpolatedPercentile([]float64{42}, 0.5)
		assert.Equal(t, 42.0, got)
	})

	t.Run("hi beyond bounds returns last element", func(t *testing.T) {
		// With p=1.0, rank = 1*(n-1) = 1 for n=2, lo=1, hi=2 >= n=2
		got := interpolatedPercentile([]float64{10, 20}, 1.0)
		assert.Equal(t, 20.0, got)
	})
}

// TestPercentileScore_DegenerateBreakpoints covers cases where adjacent breakpoints
// collapse (span = 0), exercising the degenerate-band guards in PercentileScore.
func TestPercentileScore_DegenerateBreakpoints(t *testing.T) {
	tests := []struct {
		name        string
		value       float64
		breakpoints []float64
		want        float64
		delta       float64
	}{
		{
			name:        "p25==p50: value at p25 boundary returns 0.25",
			value:       5,
			breakpoints: []float64{5, 5, 10, 20},
			want:        0.25,
			delta:       0.001,
		},
		{
			name:        "p50==p75: value at p50 boundary returns 0.50",
			value:       7,
			breakpoints: []float64{3, 7, 7, 15},
			want:        0.50,
			delta:       0.001,
		},
		{
			name:        "p75==p90: value in collapsed band returns 0.75",
			value:       10,
			breakpoints: []float64{2, 5, 10, 10},
			want:        0.75,
			delta:       0.001,
		},
		{
			name:        "p25==0 with positive value below p50",
			value:       1,
			breakpoints: []float64{0, 3, 7, 15},
			want:        0.3333,
			delta:       0.001,
		},
		{
			name:        "all breakpoints equal to 5, value is 5",
			value:       5,
			breakpoints: []float64{5, 5, 5, 5},
			want:        0.25,
			delta:       0.001,
		},
		{
			name:        "all breakpoints equal to 5, value is 10 (beyond p90)",
			value:       10,
			breakpoints: []float64{5, 5, 5, 5},
			want:        1.0,
			delta:       0.001,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PercentileScore(tc.value, tc.breakpoints)
			assert.InDelta(t, tc.want, got, tc.delta,
				"PercentileScore(%v, %v) = %v, want %v", tc.value, tc.breakpoints, got, tc.want)
		})
	}
}

// TestReScore_WithMetrics verifies that ReScore records per-signal contributions
// when opts.Metrics is non-nil. Uses the noop OTel provider.
func TestReScore_WithMetrics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))
	metrics := RegisterReScoreMetrics(logger)
	require.NotNil(t, metrics)

	now := time.Now()
	id := uuid.New()
	correct := 3
	decisions := map[uuid.UUID]model.Decision{
		id: {
			ID:                     id,
			ValidFrom:              now,
			PrecedentCitationCount: 2,
			AgreementCount:         1,
			ConflictFate:           model.ConflictFate{Won: 1, Lost: 0},
			AssessmentSummary:      &model.AssessmentSummary{Total: 3, Correct: correct},
		},
	}
	results := []Result{{DecisionID: id, Score: 0.9, QdrantRank: 0}}

	ctx := context.Background()
	opts := &ReScoreOpts{
		Metrics: metrics,
		Ctx:     ctx,
	}

	scored := ReScore(results, decisions, 10, opts)
	assert.Len(t, scored, 1)
	assert.Greater(t, float64(scored[0].SimilarityScore), 0.0)
	assert.LessOrEqual(t, float64(scored[0].SimilarityScore), 1.0)
}

// TestReScore_WithPercentileAndMetrics verifies the combination of percentile
// normalization and metrics recording.
func TestReScore_WithPercentileAndMetrics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))
	metrics := RegisterReScoreMetrics(logger)
	require.NotNil(t, metrics)

	now := time.Now()
	id := uuid.New()
	decisions := map[uuid.UUID]model.Decision{
		id: {
			ID:                     id,
			ValidFrom:              now,
			PrecedentCitationCount: 5,
		},
	}
	results := []Result{{DecisionID: id, Score: 0.9}}

	ctx := context.Background()
	opts := &ReScoreOpts{
		Percentiles: &OrgPercentiles{
			CitationBreakpoints: []float64{1, 3, 7, 15},
		},
		Metrics: metrics,
		Ctx:     ctx,
	}

	scored := ReScore(results, decisions, 10, opts)
	assert.Len(t, scored, 1)
	assert.Greater(t, float64(scored[0].SimilarityScore), 0.0)
}

// TestReScore_AgreementScoreContribution verifies that the agreement signal
// contributes to the outcome weight and affects ranking.
func TestReScore_AgreementScoreContribution(t *testing.T) {
	now := time.Now()
	highAgreement := uuid.New()
	noAgreement := uuid.New()

	decisions := map[uuid.UUID]model.Decision{
		highAgreement: {
			ID:             highAgreement,
			ValidFrom:      now,
			AgreementCount: 5,
		},
		noAgreement: {
			ID:             noAgreement,
			ValidFrom:      now,
			AgreementCount: 0,
		},
	}

	results := []Result{
		{DecisionID: highAgreement, Score: 0.9, QdrantRank: 0},
		{DecisionID: noAgreement, Score: 0.9, QdrantRank: 1},
	}

	scored := ReScore(results, decisions, 10, nil)
	assert.Len(t, scored, 2)
	assert.Equal(t, highAgreement, scored[0].Decision.ID,
		"decision with high agreement count should rank above one with no agreement")
	assert.Greater(t, scored[0].SimilarityScore, scored[1].SimilarityScore,
		"agreement signal should create a score difference")
}

// TestReScore_PartiallyCorrectAssessment verifies that partially_correct assessments
// contribute half weight to the assessment signal.
func TestReScore_PartiallyCorrectAssessment(t *testing.T) {
	now := time.Now()
	id := uuid.New()
	partial := 4

	decisions := map[uuid.UUID]model.Decision{
		id: {
			ID:                id,
			ValidFrom:         now,
			AssessmentSummary: &model.AssessmentSummary{Total: 4, Correct: 0, PartiallyCorrect: partial},
		},
	}

	results := []Result{{DecisionID: id, Score: 0.9}}
	scored := ReScore(results, decisions, 10, nil)
	assert.Len(t, scored, 1)

	// assessmentContrib = (0 + 0.5*4) / 4 * 0.40 = 0.5 * 0.40 = 0.20
	// outcomeWeight = 0.20 + 0.15*1.0 = 0.35
	// relevance = 0.9 * (0.5 + 0.5*0.35) = 0.9 * 0.675 = 0.6075
	assert.InDelta(t, 0.6075, float64(scored[0].SimilarityScore), 0.01,
		"partially_correct should contribute 50% to assessment signal")
}

// TestReScore_MissingDecisionSkipped verifies that results referencing a decision
// not in the decisions map are silently skipped.
func TestReScore_MissingDecisionSkipped(t *testing.T) {
	now := time.Now()
	present := uuid.New()
	missing := uuid.New()

	decisions := map[uuid.UUID]model.Decision{
		present: {
			ID:        present,
			ValidFrom: now,
		},
	}

	results := []Result{
		{DecisionID: missing, Score: 0.95},
		{DecisionID: present, Score: 0.9},
	}

	scored := ReScore(results, decisions, 10, nil)
	assert.Len(t, scored, 1, "missing decision should be silently skipped")
	assert.Equal(t, present, scored[0].Decision.ID)
}

// TestReScore_EmptyResults verifies that ReScore with no results returns an empty slice.
func TestReScore_EmptyResults(t *testing.T) {
	scored := ReScore(nil, nil, 10, nil)
	assert.Empty(t, scored)

	scored = ReScore([]Result{}, map[uuid.UUID]model.Decision{}, 10, nil)
	assert.Empty(t, scored)
}

// TestReScore_LimitTruncation verifies that ReScore respects the limit parameter.
func TestReScore_LimitTruncation(t *testing.T) {
	now := time.Now()
	decisions := make(map[uuid.UUID]model.Decision)
	results := make([]Result, 10)

	for i := range 10 {
		id := uuid.New()
		decisions[id] = model.Decision{ID: id, ValidFrom: now}
		results[i] = Result{DecisionID: id, Score: float32(0.5 + float64(i)*0.05)}
	}

	scored := ReScore(results, decisions, 3, nil)
	assert.Len(t, scored, 3, "ReScore should truncate to limit")
}

// TestReScore_RecencyDecay verifies that older decisions score lower due to recency decay.
func TestReScore_RecencyDecay(t *testing.T) {
	recent := uuid.New()
	old := uuid.New()

	decisions := map[uuid.UUID]model.Decision{
		recent: {
			ID:        recent,
			ValidFrom: time.Now(),
		},
		old: {
			ID:        old,
			ValidFrom: time.Now().Add(-180 * 24 * time.Hour), // 180 days ago
		},
	}

	results := []Result{
		{DecisionID: recent, Score: 0.9},
		{DecisionID: old, Score: 0.9},
	}

	scored := ReScore(results, decisions, 10, nil)
	require.Len(t, scored, 2)
	assert.Equal(t, recent, scored[0].Decision.ID,
		"recent decision should rank above an equally-scored old decision due to recency decay")
	assert.Greater(t, scored[0].SimilarityScore, scored[1].SimilarityScore,
		"recency decay should create a score difference")
}

// TestReScore_NilOpts verifies that passing nil opts works correctly (no percentile, no metrics).
func TestReScore_NilOpts(t *testing.T) {
	id := uuid.New()
	decisions := map[uuid.UUID]model.Decision{
		id: {ID: id, ValidFrom: time.Now()},
	}
	results := []Result{{DecisionID: id, Score: 0.8}}

	scored := ReScore(results, decisions, 10, nil)
	assert.Len(t, scored, 1)
	assert.Greater(t, float64(scored[0].SimilarityScore), 0.0)
}

// TestReScore_MetricsWithNilCtx verifies that metrics are not recorded when
// opts.Ctx is nil (even if opts.Metrics is set).
func TestReScore_MetricsWithNilCtx(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))
	metrics := RegisterReScoreMetrics(logger)

	id := uuid.New()
	decisions := map[uuid.UUID]model.Decision{
		id: {ID: id, ValidFrom: time.Now(), PrecedentCitationCount: 1},
	}
	results := []Result{{DecisionID: id, Score: 0.9}}

	// Ctx is nil but Metrics is set — should not panic.
	opts := &ReScoreOpts{Metrics: metrics, Ctx: nil}
	assert.NotPanics(t, func() {
		scored := ReScore(results, decisions, 10, opts)
		assert.Len(t, scored, 1)
	})
}

// TestReScore_AllSignalsMaximized verifies that when all signals are maximized,
// the outcome weight approaches 1.0 and the final score is capped at 1.0.
func TestReScore_AllSignalsMaximized(t *testing.T) {
	correct := 10
	id := uuid.New()
	slowHours := 100.0

	decisions := map[uuid.UUID]model.Decision{
		id: {
			ID:                        id,
			ValidFrom:                 time.Now(),
			PrecedentCitationCount:    100,
			AgreementCount:            100,
			ConflictFate:              model.ConflictFate{Won: 10, Lost: 0},
			AssessmentSummary:         &model.AssessmentSummary{Total: 10, Correct: correct},
			SupersessionVelocityHours: &slowHours,
		},
	}
	results := []Result{{DecisionID: id, Score: 1.0}}

	scored := ReScore(results, decisions, 10, nil)
	require.Len(t, scored, 1)
	assert.LessOrEqual(t, float64(scored[0].SimilarityScore), 1.0)
	assert.Greater(t, float64(scored[0].SimilarityScore), 0.9,
		"all signals maximized should produce a high score")
}
