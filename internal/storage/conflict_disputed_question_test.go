//go:build !lite && integration

package storage_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// The disputed question is the validator's justification for a contradiction
// verdict — the one clause a reviewer needs in order to adjudicate. It was
// parsed and discarded before migration 108, so these tests exist to keep the
// value on the record: write it, read it back through the list path, and make
// sure a re-score does not quietly drop it.
func TestInsertScoredConflict_PersistsDisputedQuestion(t *testing.T) {
	ctx := context.Background()
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: "disputed-question-test"})
	require.NoError(t, err)

	dA, err2 := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: "planner",
		DecisionType: "architecture", Outcome: "run CreateFieldIndex on startup", Confidence: 0.8,
	})
	require.NoError(t, err2)
	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: "reviewer",
		DecisionType: "architecture", Outcome: "do not run CreateFieldIndex on startup", Confidence: 0.8,
	})
	require.NoError(t, err)

	question := "whether to run CreateFieldIndex at startup"
	relationship := "contradiction"
	topicSim, outcomeDiv := 0.90, 0.80
	sig := topicSim * outcomeDiv

	conflict := model.DecisionConflict{
		ConflictKind:      model.ConflictKindCrossAgent,
		DecisionAID:       dA.ID,
		DecisionBID:       dB.ID,
		OrgID:             uuid.Nil,
		AgentA:            "planner",
		AgentB:            "reviewer",
		DecisionTypeA:     "architecture",
		DecisionTypeB:     "architecture",
		OutcomeA:          dA.Outcome,
		OutcomeB:          dB.Outcome,
		TopicSimilarity:   &topicSim,
		OutcomeDivergence: &outcomeDiv,
		Significance:      &sig,
		ScoringMethod:     "llm_v2",
		Relationship:      &relationship,
		DisputedQuestion:  &question,
	}

	id, err := testDB.InsertScoredConflict(ctx, conflict)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, id)

	got := findConflictByID(t, ctx, id)
	require.NotNil(t, got.DisputedQuestion, "disputed question must survive the write")
	assert.Equal(t, question, *got.DisputedQuestion)

	// Re-scoring the same pair is an upsert. The question must be refreshed
	// rather than stranded at its first value, otherwise a reviewer reads a
	// justification that no longer matches the stored verdict.
	revised := "whether index creation belongs in startup or in a migration"
	conflict.DisputedQuestion = &revised
	_, err = testDB.InsertScoredConflict(ctx, conflict)
	require.NoError(t, err)

	got = findConflictByID(t, ctx, id)
	require.NotNil(t, got.DisputedQuestion)
	assert.Equal(t, revised, *got.DisputedQuestion, "re-score must update the disputed question")
}

// A non-contradiction carries no disputed question: the parser clears it for
// every other relationship, so a non-nil value downstream is positive evidence
// that a dispute was identified rather than that a pair merely scored highly.
func TestInsertScoredConflict_NilDisputedQuestionRoundTrips(t *testing.T) {
	ctx := context.Background()
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: "disputed-question-nil-test"})
	require.NoError(t, err)

	dA, err2 := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: "coder",
		DecisionType: "implementation", Outcome: "added retry to the writer", Confidence: 0.7,
	})
	require.NoError(t, err2)
	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: "reviewer",
		DecisionType: "code_review", Outcome: "reviewed the writer retry and found no issues", Confidence: 0.7,
	})
	require.NoError(t, err)

	topicSim, outcomeDiv := 0.75, 0.40
	sig := topicSim * outcomeDiv
	id, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind:      model.ConflictKindCrossAgent,
		DecisionAID:       dA.ID,
		DecisionBID:       dB.ID,
		OrgID:             uuid.Nil,
		AgentA:            "coder",
		AgentB:            "reviewer",
		DecisionTypeA:     "implementation",
		DecisionTypeB:     "code_review",
		OutcomeA:          dA.Outcome,
		OutcomeB:          dB.Outcome,
		TopicSimilarity:   &topicSim,
		OutcomeDivergence: &outcomeDiv,
		Significance:      &sig,
		ScoringMethod:     "text",
	})
	require.NoError(t, err)

	got := findConflictByID(t, ctx, id)
	assert.Nil(t, got.DisputedQuestion, "a conflict with no named dispute must read back nil, not empty string")
}

func findConflictByID(t *testing.T, ctx context.Context, id uuid.UUID) model.DecisionConflict {
	t.Helper()
	conflicts, err := testDB.ListConflicts(ctx, uuid.Nil, storage.ConflictFilters{}, 200, 0)
	require.NoError(t, err)
	for _, c := range conflicts {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("conflict %s not found in list results", id)
	return model.DecisionConflict{}
}
