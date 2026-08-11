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
	"github.com/ashita-ai/akashi/internal/storage"
)

// seedDisjointResourcePair creates two investigation decisions on the same
// project that name provably different connectors, which is exactly the shape
// isDisjointResource suppresses pre-LLM. Identical topic embeddings put the
// pair above decisionTopicSimFloor so it takes the directToScorer path and
// reaches the structural filters instead of being pruned by early exit.
//
// Returns the two decision IDs (A is the one to score) and the org.
func seedDisjointResourcePair(t *testing.T, ctx context.Context) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	orgID := uuid.Nil
	suffix := uuid.New().String()[:8]
	project := "suppression-sample-" + suffix

	agentA := "suppsample-a-" + suffix
	agentB := "suppsample-b-" + suffix
	for _, id := range []string{agentA, agentB} {
		_, err := testDB.CreateAgent(ctx, model.Agent{
			AgentID: id, OrgID: orgID, Name: id, Role: model.RoleAgent,
		})
		require.NoError(t, err)
	}

	runA := createRun(t, agentA, orgID)
	runB := createRun(t, agentB, orgID)

	topicEmb := makeEmbedding(701, 1.0)
	outA := makeEmbedding(702, 1.0)
	outB := makeEmbedding(703, 1.0)

	// decisions.project is a STORED GENERATED column computed from
	// agent_context (migration 052), so it cannot be set through the Decision
	// struct or an UPDATE — it must be written via agent_context.client.project.
	// isDisjointResource requires a shared non-empty project on both sides.
	projectCtx := map[string]any{"client": map[string]any{"project": project}}

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA, OrgID: orgID,
		DecisionType: "investigation",
		Outcome:      "Diagnosed connector_aa11bb22 as stalled on an invalidated replication slot",
		Confidence:   0.8, AgentContext: projectCtx,
		Embedding: &topicEmb, OutcomeEmbedding: &outA,
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB, OrgID: orgID,
		DecisionType: "investigation",
		Outcome:      "Diagnosed connector_cc33dd44 as healthy after the source endpoint autoscaled",
		Confidence:   0.8, AgentContext: projectCtx,
		Embedding: &topicEmb, OutcomeEmbedding: &outB,
	})
	require.NoError(t, err)

	// Guard the fixture itself: if the filter under test stops matching this
	// pair, the assertions below would pass vacuously for the wrong reason.
	fullA, err := testDB.GetDecision(ctx, orgID, dA.ID, storage.GetDecisionOpts{})
	require.NoError(t, err)
	fullB, err := testDB.GetDecision(ctx, orgID, dB.ID, storage.GetDecisionOpts{})
	require.NoError(t, err)
	require.True(t, isDisjointResource(fullA, fullB),
		"fixture must trip isDisjointResource, otherwise this test asserts nothing")

	return dA.ID, dB.ID, orgID
}

func samplesForPair(t *testing.T, ctx context.Context, orgID, a, b uuid.UUID) []storage.SuppressedPairSample {
	t.Helper()
	lo, hi := orderPair(a, b)
	all, err := testDB.ListSuppressedPairSamples(ctx, orgID, ruleDisjointResource, 1000)
	require.NoError(t, err)
	var out []storage.SuppressedPairSample
	for _, s := range all {
		if s.DecisionAID == lo && s.DecisionBID == hi {
			out = append(out, s)
		}
	}
	return out
}

// The test that fails if the sampler is removed from the disjoint-resource drop
// site: the pair is suppressed, so it never reaches scored_conflicts, and
// suppressed_pair_samples is the only place its existence is recorded.
func TestScorer_SamplesStructurallySuppressedPairs(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	dA, dB, orgID := seedDisjointResourcePair(t, ctx)

	scorer := NewScorer(testDB, logger, 0.1, stubConflictValidator{}, 0, 0).
		WithCandidateFinder(storage.NewPgCandidateFinder(testDB)).
		WithSuppressionSampleRate(1.0)
	scorer.ScoreForDecision(ctx, dA, orgID)

	got := samplesForPair(t, ctx, orgID, dA, dB)
	require.Len(t, got, 1, "the suppressed pair must be recorded exactly once")
	assert.Equal(t, ruleDisjointResource, got[0].Rule)

	lo, hi := orderPair(dA, dB)
	assert.Equal(t, lo, got[0].DecisionAID, "pair must be stored in canonical order")
	assert.Equal(t, hi, got[0].DecisionBID, "pair must be stored in canonical order")

	// The pair was suppressed, so it must NOT have produced a conflict.
	conflicts, err := testDB.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 1000, 0)
	require.NoError(t, err)
	for _, c := range conflicts {
		involvesPair := (c.DecisionAID == dA || c.DecisionBID == dA) &&
			(c.DecisionAID == dB || c.DecisionBID == dB)
		assert.False(t, involvesPair,
			"a structurally suppressed pair must not reach the conflict queue")
	}
}

// A rescore re-suppresses the same pairs. Deterministic sampling plus the
// UNIQUE constraint must keep the corpus bounded rather than duplicating a row
// per rescore. Fails if ON CONFLICT DO NOTHING or the UNIQUE key is dropped.
func TestScorer_SuppressedSamplesDedupeAcrossRescores(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	dA, dB, orgID := seedDisjointResourcePair(t, ctx)

	scorer := NewScorer(testDB, logger, 0.1, stubConflictValidator{}, 0, 0).
		WithCandidateFinder(storage.NewPgCandidateFinder(testDB)).
		WithSuppressionSampleRate(1.0)
	scorer.ScoreForDecision(ctx, dA, orgID)
	scorer.ScoreForDecision(ctx, dA, orgID)

	assert.Len(t, samplesForPair(t, ctx, orgID, dA, dB), 1,
		"a second rescore must not add a second row for the same (pair, rule)")
}

// Off by default. This writes rows into every operator's database for a
// research purpose they did not ask for, so the default must stay 0.
func TestScorer_SuppressionSamplingDisabledByDefault(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	dA, dB, orgID := seedDisjointResourcePair(t, ctx)

	scorer := NewScorer(testDB, logger, 0.1, stubConflictValidator{}, 0, 0).
		WithCandidateFinder(storage.NewPgCandidateFinder(testDB))
	scorer.ScoreForDecision(ctx, dA, orgID)

	assert.Empty(t, samplesForPair(t, ctx, orgID, dA, dB),
		"sampling must be off unless AKASHI_CONFLICT_SUPPRESSION_SAMPLE_RATE is set")
}
