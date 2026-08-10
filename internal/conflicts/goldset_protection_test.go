//go:build !lite && integration

package conflicts

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

// Gold labels are the only ground truth the detector is measured against, and
// nothing in this repository can regenerate them: they come from an
// out-of-repo blind rating pass whose reliability was established by a
// 200-pair re-rate. The foreign key cascades, which is correct for a genuine
// deletion but catastrophic for the two bulk clears that run at startup after
// a prompt change — and silent, because the mutation-audit row counts
// scored_conflicts rather than labels.
//
// These tests exist because the failure has no symptom. A partial loss does
// not error; it just quietly skews the next corpus-projected precision figure.

func seedLabelledConflict(t *testing.T, ctx context.Context, scoringMethod string) (conflictID uuid.UUID, orgID uuid.UUID) {
	t.Helper()
	orgID = uuid.Nil

	suffix := uuid.New().String()[:8]
	agentID := "goldset-guard-" + suffix
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: orgID, Name: agentID, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	runA := createRun(t, agentID, orgID)
	runB := createRun(t, agentID, orgID)
	topicEmb := makeEmbedding(910, 1.0)
	outA := makeEmbedding(911, 1.0)
	outB := makeEmbedding(912, 1.0)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "goldset guard A " + suffix,
		Confidence: 0.8, Embedding: &topicEmb, OutcomeEmbedding: &outA,
	})
	require.NoError(t, err)
	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "goldset guard B " + suffix,
		Confidence: 0.8, Embedding: &topicEmb, OutcomeEmbedding: &outB,
	})
	require.NoError(t, err)

	err = testDB.Pool().QueryRow(ctx,
		`INSERT INTO scored_conflicts (decision_a_id, decision_b_id, org_id, agent_a, agent_b,
			conflict_kind, scoring_method, status, decision_type_a, decision_type_b,
			outcome_a, outcome_b, topic_similarity, outcome_divergence, significance)
		 VALUES ($1, $2, $3, $4, $4, 'self_contradiction', $5, 'open',
			'architecture', 'architecture', 'guard A', 'guard B', 0.9, 0.8, 0.7)
		 RETURNING id`,
		dA.ID, dB.ID, orgID, agentID, scoringMethod).Scan(&conflictID)
	require.NoError(t, err)

	_, err = testDB.Pool().Exec(ctx,
		`INSERT INTO conflict_gold_labels (scored_conflict_id, org_id, label, method, labeled_by)
		 VALUES ($1, $2, 'contradiction', 'test_guard', 'goldset_protection_test')`,
		conflictID, orgID)
	require.NoError(t, err)

	return conflictID, orgID
}

func goldLabelExists(t *testing.T, ctx context.Context, conflictID uuid.UUID) bool {
	t.Helper()
	var n int
	require.NoError(t, testDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM conflict_gold_labels WHERE scored_conflict_id = $1`, conflictID).Scan(&n))
	return n > 0
}

// The force-rescore path is the documented way to re-evaluate every pair after
// a prompt change — exactly the operation that follows a validator rewrite. It
// must not take the corpus that measures whether the rewrite helped.
func TestClearAllConflicts_PreservesGoldLabelledPairs(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	conflictID, _ := seedLabelledConflict(t, ctx, "llm_v2")

	scorer := NewScorer(testDB, logger, 0.3, stubConflictValidator{}, 0, 0)
	_, err := scorer.ClearAllConflicts(ctx)
	require.NoError(t, err)

	assert.True(t, goldLabelExists(t, ctx, conflictID),
		"ClearAllConflicts must not cascade away a gold-labelled pair")

	var stillThere bool
	require.NoError(t, testDB.Pool().QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM scored_conflicts WHERE id = $1)`, conflictID).Scan(&stillThere))
	assert.True(t, stillThere, "the labelled conflict row itself must survive, or the label dangles")
}

// The unvalidated-clear path runs whenever an LLM validator is configured and
// legacy-scored rows exist — no env var required. Those legacy rows are
// precisely what the gold set was built over, so this is the likelier of the
// two paths to hit real labels.
func TestClearUnvalidatedConflicts_PreservesGoldLabelledPairs(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	conflictID, _ := seedLabelledConflict(t, ctx, "embedding")

	scorer := NewScorer(testDB, logger, 0.3, stubConflictValidator{}, 0, 0)
	_, err := scorer.ClearUnvalidatedConflicts(ctx)
	require.NoError(t, err)

	assert.True(t, goldLabelExists(t, ctx, conflictID),
		"ClearUnvalidatedConflicts must not cascade away a gold-labelled pair")
}

// Unlabelled rows must still be cleared: the guard protects ground truth, it
// does not disable the maintenance path.
func TestClearUnvalidatedConflicts_StillDeletesUnlabelled(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	labelled, _ := seedLabelledConflict(t, ctx, "embedding")
	unlabelled, _ := seedLabelledConflict(t, ctx, "embedding")
	_, err := testDB.Pool().Exec(ctx,
		`DELETE FROM conflict_gold_labels WHERE scored_conflict_id = $1`, unlabelled)
	require.NoError(t, err)

	scorer := NewScorer(testDB, logger, 0.3, stubConflictValidator{}, 0, 0)
	_, err = scorer.ClearUnvalidatedConflicts(ctx)
	require.NoError(t, err)

	var labelledLives, unlabelledLives bool
	require.NoError(t, testDB.Pool().QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM scored_conflicts WHERE id = $1)`, labelled).Scan(&labelledLives))
	require.NoError(t, testDB.Pool().QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM scored_conflicts WHERE id = $1)`, unlabelled).Scan(&unlabelledLives))

	assert.True(t, labelledLives, "labelled pair must be exempt from the clear")
	assert.False(t, unlabelledLives, "unlabelled pair must still be cleared")
}
