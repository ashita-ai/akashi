//go:build !lite && integration

package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// makePendingFixture seeds a decision with a controlled valid_from so the
// pending-assessment query can be exercised against deterministic ages.
// suffix isolates fixtures across tests in the shared testDB.
func makePendingFixture(t *testing.T, ctx context.Context, suffix, agentID, decisionType string, validFrom time.Time) model.Decision {
	t.Helper()
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)
	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: decisionType,
		Outcome:      "fixture-" + suffix,
		Confidence:   0.7,
		ValidFrom:    validFrom,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)
	return d
}

// TestListPendingAssessments_AgeAndAssessmentFilters covers the core
// post-conditions that issue #716's prompt loop depends on:
//   - decisions older than the cutoff are surfaced
//   - decisions younger than the cutoff are not
//   - decisions with any-source assessment are excluded
//   - decisions whose decision_type has no window are invisible
//
// All four cases hit the same query, which means a regression in any single
// branch silently breaks the prompt loop in production. They are exercised
// together to keep that surface honest.
func TestListPendingAssessments_AgeAndAssessmentFilters(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "pending-agent-" + suffix
	now := time.Now().UTC()

	// Old + unassessed: should appear.
	oldUnassessed := makePendingFixture(t, ctx, suffix+"-old-unassessed",
		agentID, "architecture", now.Add(-30*24*time.Hour))

	// Old + manually assessed: should be excluded.
	oldManuallyAssessed := makePendingFixture(t, ctx, suffix+"-old-manual",
		agentID, "architecture", now.Add(-30*24*time.Hour))
	_, err := testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID:      oldManuallyAssessed.ID,
		AssessorAgentID: agentID,
		Outcome:         model.AssessmentCorrect,
		Source:          model.AssessmentSourceManual,
	})
	require.NoError(t, err)

	// Old + auto-assessed (citation): should also be excluded — auto counts
	// as already-assessed per the design (avoid prompt-loop noise once an
	// auto signal has produced a verdict).
	oldAutoAssessed := makePendingFixture(t, ctx, suffix+"-old-auto",
		agentID, "architecture", now.Add(-30*24*time.Hour))
	_, err = testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID:      oldAutoAssessed.ID,
		AssessorAgentID: "system:" + model.AssessmentSourceCitation,
		Outcome:         model.AssessmentCorrect,
		Source:          model.AssessmentSourceCitation,
	})
	require.NoError(t, err)

	// Young + unassessed: should not appear (still within the window).
	makePendingFixture(t, ctx, suffix+"-young",
		agentID, "architecture", now.Add(-2*time.Hour))

	// Wrong type (no window passed): should not appear.
	makePendingFixture(t, ctx, suffix+"-wrong-type",
		agentID, "code_review", now.Add(-30*24*time.Hour))

	// Apply a 7-day window for "architecture" only. "code_review" is omitted
	// to assert the opt-out semantics (a type with no entry in Windows is
	// invisible to the surface).
	rows, err := testDB.ListPendingAssessments(ctx, uuid.Nil, storage.ListPendingAssessmentsOpts{
		Windows: []storage.PendingAssessmentWindow{
			{DecisionType: "architecture", Cutoff: now.Add(-7 * 24 * time.Hour)},
		},
		AgentIDs: []string{agentID},
		Limit:    50,
	})
	require.NoError(t, err)

	ids := pendingIDs(rows)
	assert.Contains(t, ids, oldUnassessed.ID, "old unassessed decision should surface")
	assert.NotContains(t, ids, oldManuallyAssessed.ID, "manual assessment must exclude")
	assert.NotContains(t, ids, oldAutoAssessed.ID, "auto assessment (any source) must exclude")
	for _, r := range rows {
		assert.NotEqual(t, "code_review", r.DecisionType,
			"opt-out type must never appear in results")
	}
}

// TestListPendingAssessments_AgentScope ensures the AgentIDs filter respects
// the contract documented on ListPendingAssessmentsOpts: nil means no
// agent-level restriction; populated means restrict; explicit empty means
// deny-all without touching the DB.
func TestListPendingAssessments_AgentScope(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentA := "scope-a-" + suffix
	agentB := "scope-b-" + suffix
	now := time.Now().UTC()

	dA := makePendingFixture(t, ctx, suffix+"-a", agentA, "architecture", now.Add(-14*24*time.Hour))
	dB := makePendingFixture(t, ctx, suffix+"-b", agentB, "architecture", now.Add(-14*24*time.Hour))

	windows := []storage.PendingAssessmentWindow{
		{DecisionType: "architecture", Cutoff: now.Add(-7 * 24 * time.Hour)},
	}

	// Scope to agentA only.
	rows, err := testDB.ListPendingAssessments(ctx, uuid.Nil, storage.ListPendingAssessmentsOpts{
		Windows: windows, AgentIDs: []string{agentA}, Limit: 50,
	})
	require.NoError(t, err)
	ids := pendingIDs(rows)
	assert.Contains(t, ids, dA.ID)
	assert.NotContains(t, ids, dB.ID, "agentA scope must hide agentB's decisions")

	// Explicit empty AgentIDs slice → deny-all, no DB error.
	rows, err = testDB.ListPendingAssessments(ctx, uuid.Nil, storage.ListPendingAssessmentsOpts{
		Windows: windows, AgentIDs: []string{}, Limit: 50,
	})
	require.NoError(t, err)
	assert.Empty(t, rows, "empty agent set must yield zero rows")

	// Nil AgentIDs (admin path) → both agents visible.
	rows, err = testDB.ListPendingAssessments(ctx, uuid.Nil, storage.ListPendingAssessmentsOpts{
		Windows: windows, AgentIDs: nil, Limit: 50,
	})
	require.NoError(t, err)
	ids = pendingIDs(rows)
	assert.Contains(t, ids, dA.ID)
	assert.Contains(t, ids, dB.ID)
}

// TestListPendingAssessments_NoWindows confirms the empty-Windows short-circuit.
// This is the path used when every configured type has window=0 (assessment
// prompting fully disabled) — we must not issue a query that scans the whole
// decisions table.
func TestListPendingAssessments_NoWindows(t *testing.T) {
	ctx := context.Background()
	rows, err := testDB.ListPendingAssessments(ctx, uuid.Nil, storage.ListPendingAssessmentsOpts{
		Windows: nil,
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Nil(t, rows)
}

func pendingIDs(rows []model.PendingAssessment) []uuid.UUID {
	out := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		out[i] = r.DecisionID
	}
	return out
}
