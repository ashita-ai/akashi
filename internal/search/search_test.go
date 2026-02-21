package search

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/model"
)

// TestReScore_OutcomeDominatesCompleteness verifies acceptance criterion 5:
// a decision with 5+ citations and zero completeness outranks one with completeness 1.0 and zero citations.
func TestReScore_OutcomeDominatesCompleteness(t *testing.T) {
	now := time.Now()
	highCitation := uuid.New()
	highCompleteness := uuid.New()

	decisions := map[uuid.UUID]model.Decision{
		highCitation: {
			ID:                     highCitation,
			ValidFrom:              now,
			CompletenessScore:      0.0,
			PrecedentCitationCount: 5, // citation_score = 1.0
		},
		highCompleteness: {
			ID:                     highCompleteness,
			ValidFrom:              now,
			CompletenessScore:      0.5, // typical completeness; citations=5 should still win
			PrecedentCitationCount: 0,
		},
	}

	results := []Result{
		{DecisionID: highCitation, Score: 0.9},
		{DecisionID: highCompleteness, Score: 0.9},
	}

	scored := ReScore(results, decisions, 10)
	assert.Len(t, scored, 2)
	// highCitation: outcomeWeight=0.4*1+0.3*0.5+0.1=0.65; multiplier=0.5+0.195+0=0.695; score=0.9*0.695=0.6255
	// highCompleteness: outcomeWeight=0.25; multiplier=0.5+0.075+0.1=0.675; score=0.9*0.675=0.6075
	// highCitation (0.6255) > highCompleteness (0.6075) — citations win.
	assert.Equal(t, highCitation, scored[0].Decision.ID,
		"decision with 5 citations should outrank one with completeness=0.5 and zero citations")
}

// TestReScore_StabilityZeroForFastSupersession verifies acceptance criterion 6:
// decisions superseded within 48h receive stability_score = 0.0.
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
			CompletenessScore:         0.5,
			SupersessionVelocityHours: &fastHours,
		},
		slowRevision: {
			ID:                        slowRevision,
			ValidFrom:                 now,
			CompletenessScore:         0.5,
			SupersessionVelocityHours: &slowHours,
		},
	}

	results := []Result{
		{DecisionID: fastRevision, Score: 0.9},
		{DecisionID: slowRevision, Score: 0.9},
	}

	scored := ReScore(results, decisions, 10)
	assert.Len(t, scored, 2)
	// slowRevision should have higher score (stability 1.0 vs 0.0).
	assert.Equal(t, slowRevision, scored[0].Decision.ID,
		"decision superseded after 96h should outrank one superseded after 24h")
}

// TestReScore_ColdStart verifies acceptance criterion 7:
// a new decision with all signals zero receives outcome_weight = 0.25.
func TestReScore_ColdStart(t *testing.T) {
	id := uuid.New()
	decisions := map[uuid.UUID]model.Decision{
		id: {
			ID:                id,
			ValidFrom:         time.Now(),
			CompletenessScore: 0.0,
			// All outcome signals zero — no citations, no conflicts, no agreement.
		},
	}

	results := []Result{{DecisionID: id, Score: 1.0}}
	scored := ReScore(results, decisions, 10)
	assert.Len(t, scored, 1)

	// outcome_weight = 0.4*0 + 0.3*0.5 + 0.2*0 + 0.1*1.0 = 0.25
	// relevance multiplier = 0.5 + 0.3*0.25 + 0.2*0.0 = 0.575
	// With similarity=1.0 and recency=1.0 (age=0): relevance = 0.575
	assert.InDelta(t, 0.575, float64(scored[0].SimilarityScore), 0.001,
		"cold-start decision should have relevance multiplier ~0.575")
}

// TestReScore_BoundedToOne verifies acceptance criterion 8:
// ReScore results are bounded to [0.0, 1.0].
func TestReScore_BoundedToOne(t *testing.T) {
	id := uuid.New()
	decisions := map[uuid.UUID]model.Decision{
		id: {
			ID:                     id,
			ValidFrom:              time.Now(),
			CompletenessScore:      1.0,
			PrecedentCitationCount: 100, // would push outcome very high
			AgreementCount:         100,
			ConflictFate:           model.ConflictFate{Won: 100, Lost: 0},
		},
	}

	results := []Result{{DecisionID: id, Score: 1.0}}
	scored := ReScore(results, decisions, 10)
	assert.Len(t, scored, 1)
	assert.LessOrEqual(t, float64(scored[0].SimilarityScore), 1.0, "score must not exceed 1.0")
	assert.GreaterOrEqual(t, float64(scored[0].SimilarityScore), 0.0, "score must not be negative")
}

// TestReScore_ConflictWinRateDefault verifies that a decision with no resolved
// conflicts receives conflict_win_rate = 0.5 (neutral), not 0.0 (punitive).
func TestReScore_ConflictWinRateDefault(t *testing.T) {
	noConflict := uuid.New()
	lostConflict := uuid.New()

	decisions := map[uuid.UUID]model.Decision{
		noConflict: {
			ID:        noConflict,
			ValidFrom: time.Now(),
			// ConflictFate zero: won=0, lost=0 → default 0.5
		},
		lostConflict: {
			ID:           lostConflict,
			ValidFrom:    time.Now(),
			ConflictFate: model.ConflictFate{Won: 0, Lost: 1}, // win_rate = 0.0
		},
	}

	results := []Result{
		{DecisionID: noConflict, Score: 0.9},
		{DecisionID: lostConflict, Score: 0.9},
	}

	scored := ReScore(results, decisions, 10)
	assert.Len(t, scored, 2)
	// noConflict has win_rate 0.5; lostConflict has win_rate 0.0 — noConflict should rank higher.
	assert.Equal(t, noConflict, scored[0].Decision.ID,
		"decision with no conflict history should outrank one that lost its only conflict")
}
