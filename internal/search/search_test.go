package search

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

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

	scored := ReScore(results, decisions, 10)
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

	scored := ReScore(results, decisions, 10)
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
	scored := ReScore(results, decisions, 10)
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
	scored := ReScore(results, decisions, 10)
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

	scored := ReScore(results, decisions, 10)
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

	scored := ReScore(results, decisions, 10)
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

	scored := ReScore(results, decisions, 10)
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
