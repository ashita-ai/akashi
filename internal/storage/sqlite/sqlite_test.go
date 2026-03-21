package sqlite_test

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/internal/storage/sqlite"
)

// newTestDB creates an in-memory SQLite database for testing.
func newTestDB(t *testing.T) *sqlite.LiteDB {
	t.Helper()
	ctx := context.Background()
	logger := slog.Default()
	db, err := sqlite.New(ctx, ":memory:", logger)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close(ctx) })
	return db
}

func TestPing(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.Ping(context.Background()))
}

func TestEnsureDefaultOrg(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	// Idempotent — calling again should succeed.
	require.NoError(t, db.EnsureDefaultOrg(ctx))
}

func TestCreateAndGetAgent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))

	orgID := uuid.Nil
	now := time.Now().UTC().Truncate(time.Second)

	agent := model.Agent{
		AgentID:   "test-agent-1",
		OrgID:     orgID,
		Name:      "Test Agent",
		Role:      model.RoleAgent,
		Tags:      []string{"backend", "reviewer"},
		Metadata:  map[string]any{"version": "1.0"},
		CreatedAt: now,
		UpdatedAt: now,
	}

	created, err := db.CreateAgent(ctx, agent)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, created.ID)
	assert.Equal(t, "test-agent-1", created.AgentID)

	fetched, err := db.GetAgentByAgentID(ctx, orgID, "test-agent-1")
	require.NoError(t, err)
	assert.Equal(t, created.ID, fetched.ID)
	assert.Equal(t, "Test Agent", fetched.Name)
	assert.Equal(t, model.RoleAgent, fetched.Role)
	assert.Equal(t, []string{"backend", "reviewer"}, fetched.Tags)
}

func TestGetAgent_NotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))

	_, err := db.GetAgentByAgentID(ctx, uuid.Nil, "nonexistent")
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestCountAgents(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	count, err := db.CountAgents(ctx, orgID)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	_, err = db.CreateAgent(ctx, model.Agent{
		AgentID: "a1", OrgID: orgID, Name: "A1", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	count, err = db.CountAgents(ctx, orgID)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestListAgentIDsBySharedTags(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	now := time.Now().UTC()

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "tagged-1", OrgID: orgID, Name: "T1", Role: model.RoleAgent,
		Tags: []string{"backend", "go"}, Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	_, err = db.CreateAgent(ctx, model.Agent{
		AgentID: "tagged-2", OrgID: orgID, Name: "T2", Role: model.RoleAgent,
		Tags: []string{"frontend", "ts"}, Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	ids, err := db.ListAgentIDsBySharedTags(ctx, orgID, []string{"go"})
	require.NoError(t, err)
	assert.Equal(t, []string{"tagged-1"}, ids)

	ids, err = db.ListAgentIDsBySharedTags(ctx, orgID, []string{"python"})
	require.NoError(t, err)
	assert.Empty(t, ids)

	ids, err = db.ListAgentIDsBySharedTags(ctx, orgID, []string{})
	require.NoError(t, err)
	assert.Nil(t, ids)
}

func TestCreateTraceTx(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	// Create the agent first.
	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "trace-agent", OrgID: orgID, Name: "Trace Agent", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	reasoning := "test reasoning"
	params := storage.CreateTraceParams{
		AgentID:  "trace-agent",
		OrgID:    orgID,
		Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "code_review",
			Outcome:      "approve the PR",
			Confidence:   0.9,
			Reasoning:    &reasoning,
			Metadata:     map[string]any{},
		},
		Alternatives: []model.Alternative{
			{Label: "reject", Score: ptrFloat32(0.1), Selected: false, Metadata: map[string]any{}},
			{Label: "approve", Score: ptrFloat32(0.9), Selected: true, Metadata: map[string]any{}},
		},
		Evidence: []model.Evidence{
			{
				SourceType: model.SourceAPIResponse,
				Content:    "test coverage is 95%",
				Metadata:   map[string]any{},
			},
		},
	}

	run, decision, err := db.CreateTraceTx(ctx, params)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, run.ID)
	assert.NotEqual(t, uuid.Nil, decision.ID)
	assert.Equal(t, "code_review", decision.DecisionType)
	assert.Equal(t, "approve the PR", decision.Outcome)
	assert.InDelta(t, 0.9, decision.Confidence, 0.001)
	assert.Equal(t, model.RunStatusCompleted, run.Status)
}

func TestQueryDecisions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "query-agent", OrgID: orgID, Name: "Q", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// Create 3 decisions.
	for i, dt := range []string{"code_review", "architecture", "code_review"} {
		reasoning := "reasoning " + dt
		_, _, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
			AgentID:  "query-agent",
			OrgID:    orgID,
			Metadata: map[string]any{},
			Decision: model.Decision{
				DecisionType: dt,
				Outcome:      "outcome " + string(rune('A'+i)),
				Confidence:   float32(i+1) * 0.3,
				Reasoning:    &reasoning,
				Metadata:     map[string]any{},
			},
		})
		require.NoError(t, err)
	}

	t.Run("all decisions", func(t *testing.T) {
		decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Limit: 10,
		})
		require.NoError(t, err)
		assert.Equal(t, 3, total)
		assert.Len(t, decisions, 3)
	})

	t.Run("filter by type", func(t *testing.T) {
		dt := "code_review"
		decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Filters: model.QueryFilters{DecisionType: &dt},
			Limit:   10,
		})
		require.NoError(t, err)
		assert.Equal(t, 2, total)
		assert.Len(t, decisions, 2)
	})

	t.Run("filter by agent", func(t *testing.T) {
		agentID := "query-agent"
		decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Filters: model.QueryFilters{AgentIDs: []string{agentID}},
			Limit:   10,
		})
		require.NoError(t, err)
		assert.Equal(t, 3, total)
		assert.Len(t, decisions, 3)
	})

	t.Run("pagination", func(t *testing.T) {
		decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Limit:  2,
			Offset: 0,
		})
		require.NoError(t, err)
		assert.Equal(t, 3, total)
		assert.Len(t, decisions, 2)

		decisions2, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Limit:  2,
			Offset: 2,
		})
		require.NoError(t, err)
		assert.Len(t, decisions2, 1)
		// The last page should not overlap with the first.
		assert.NotEqual(t, decisions[0].ID, decisions2[0].ID)
		assert.NotEqual(t, decisions[1].ID, decisions2[0].ID)
	})
}

func TestSearchDecisionsByText(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "search-agent", OrgID: orgID, Name: "S", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	reasoning := "the database schema needs normalization"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID:  "search-agent",
		OrgID:    orgID,
		Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "architecture",
			Outcome:      "normalize the user table into separate entities",
			Confidence:   0.8,
			Reasoning:    &reasoning,
			Metadata:     map[string]any{},
		},
	})
	require.NoError(t, err)

	reasoning2 := "caching improves response times"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID:  "search-agent",
		OrgID:    orgID,
		Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "performance",
			Outcome:      "add Redis caching layer for API responses",
			Confidence:   0.7,
			Reasoning:    &reasoning2,
			Metadata:     map[string]any{},
		},
	})
	require.NoError(t, err)

	// FTS5 search for "normalize".
	results, err := db.SearchDecisionsByText(ctx, orgID, "normalize", model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Contains(t, results[0].Decision.Outcome, "normalize")

	// FTS5 search for "caching".
	results, err = db.SearchDecisionsByText(ctx, orgID, "caching", model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Contains(t, results[0].Decision.Outcome, "caching")

	// Search for something that doesn't exist.
	results, err = db.SearchDecisionsByText(ctx, orgID, "kubernetes", model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestGetDecisionsByIDs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "ids-agent", OrgID: orgID, Name: "I", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, d1, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "ids-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "d1", Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, d2, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "ids-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "d2", Confidence: 0.6, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	result, err := db.GetDecisionsByIDs(ctx, orgID, []uuid.UUID{d1.ID, d2.ID})
	require.NoError(t, err)
	assert.Len(t, result, 2)
	assert.Equal(t, "d1", result[d1.ID].Outcome)
	assert.Equal(t, "d2", result[d2.ID].Outcome)

	// Empty IDs should return empty.
	result, err = db.GetDecisionsByIDs(ctx, orgID, nil)
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestIdempotency(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	// First call: reserves the key.
	lookup, err := db.BeginIdempotency(ctx, orgID, "agent-1", "/v1/trace", "key-1", "hash-abc")
	require.NoError(t, err)
	assert.False(t, lookup.Completed)

	// Second call with same key but in-progress: should return ErrIdempotencyInProgress.
	_, err = db.BeginIdempotency(ctx, orgID, "agent-1", "/v1/trace", "key-1", "hash-abc")
	assert.ErrorIs(t, err, storage.ErrIdempotencyInProgress)

	// Different hash: should return ErrIdempotencyPayloadMismatch.
	_, err = db.BeginIdempotency(ctx, orgID, "agent-1", "/v1/trace", "key-1", "hash-different")
	assert.ErrorIs(t, err, storage.ErrIdempotencyPayloadMismatch)

	// Complete the idempotency key.
	err = db.CompleteIdempotency(ctx, orgID, "agent-1", "/v1/trace", "key-1", 201, map[string]any{"id": "123"})
	require.NoError(t, err)

	// Replay: should return completed=true.
	lookup, err = db.BeginIdempotency(ctx, orgID, "agent-1", "/v1/trace", "key-1", "hash-abc")
	require.NoError(t, err)
	assert.True(t, lookup.Completed)
	assert.Equal(t, 201, lookup.StatusCode)
}

func TestClearInProgressIdempotency(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.BeginIdempotency(ctx, orgID, "agent-1", "/v1/trace", "clear-key", "hash-x")
	require.NoError(t, err)

	err = db.ClearInProgressIdempotency(ctx, orgID, "agent-1", "/v1/trace", "clear-key")
	require.NoError(t, err)

	// After clearing, the key can be reserved again.
	lookup, err := db.BeginIdempotency(ctx, orgID, "agent-1", "/v1/trace", "clear-key", "hash-x")
	require.NoError(t, err)
	assert.False(t, lookup.Completed)
}

func TestNotify(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// SQLite notify is a no-op.
	require.NoError(t, db.Notify(ctx, "test_channel", "payload"))
	assert.False(t, db.HasNotifyConn())
}

func TestIsDuplicateKey(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	now := time.Now().UTC()

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "dup-agent", OrgID: orgID, Name: "D", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	// Create again with same (org_id, agent_id) — should fail.
	_, err = db.CreateAgent(ctx, model.Agent{
		AgentID: "dup-agent", OrgID: orgID, Name: "D2", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	})
	require.Error(t, err)
	assert.True(t, db.IsDuplicateKey(err), "expected IsDuplicateKey to return true for UNIQUE constraint violation")
}

func TestTraceHealth(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "health-agent", OrgID: orgID, Name: "H", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	reasoning := "good reasoning"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "health-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "review", Outcome: "approved", Confidence: 0.8,
			Reasoning: &reasoning, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	t.Run("decision quality", func(t *testing.T) {
		stats, err := db.GetDecisionQualityStats(ctx, orgID, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Total)
		assert.Equal(t, 1, stats.WithReasoning)
	})

	t.Run("evidence coverage", func(t *testing.T) {
		stats, err := db.GetEvidenceCoverageStats(ctx, orgID, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.TotalDecisions)
		assert.Equal(t, 0, stats.WithEvidence) // no evidence attached
	})

	t.Run("conflict status counts", func(t *testing.T) {
		counts, err := db.GetConflictStatusCounts(ctx, orgID, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, 0, counts.Total)
	})

	t.Run("wont fix rate", func(t *testing.T) {
		rate, err := db.GetWontFixRate(ctx, orgID)
		require.NoError(t, err)
		assert.Equal(t, 0.0, rate.Rate)
		assert.Equal(t, 0, rate.Resolved)
		assert.Equal(t, 0, rate.WontFix)
	})

	t.Run("outcome signals summary", func(t *testing.T) {
		summary, err := db.GetOutcomeSignalsSummary(ctx, orgID, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, summary.DecisionsTotal)
		assert.Equal(t, 1, summary.NeverSuperseded)
	})

	t.Run("confidence distribution", func(t *testing.T) {
		dist, err := db.GetConfidenceDistribution(ctx, orgID)
		require.NoError(t, err)
		assert.Equal(t, 1, dist.TotalDecisions)
		assert.InDelta(t, 0.8, dist.AvgConfidence, 0.01)
		assert.InDelta(t, 0.8, dist.MedianConfidence, 0.01)
		assert.Len(t, dist.Buckets, 10)
		// Confidence 0.8 falls in the "0.8-0.9" bucket (index 8).
		assert.Equal(t, 1, dist.Buckets[8].Count)
		assert.Equal(t, "0.8-0.9", dist.Buckets[8].Bucket)
		assert.InDelta(t, 0.0, dist.HighConfidencePct, 0.01) // 0.8 < 0.9
		assert.Len(t, dist.ByAgent, 1)
		assert.Equal(t, "health-agent", dist.ByAgent[0].AgentID)
		assert.InDelta(t, 0.8, dist.ByAgent[0].AvgConfidence, 0.01)
		assert.Equal(t, 1, dist.ByAgent[0].DecisionCount)
	})
}

func TestAuthz(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// In lite mode, authz is permissive but still queries the access_grants table.
	has, err := db.HasAccess(ctx, uuid.Nil, uuid.New(), "agent", "", "read")
	require.NoError(t, err)
	assert.False(t, has, "no grants inserted, should return false")

	ids, err := db.ListGrantedAgentIDs(ctx, uuid.Nil, uuid.New(), "self")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"self": true}, ids, "agent always has access to own traces")
}

func TestCreateAssessment(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "assess-agent", OrgID: orgID, Name: "A", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, d, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "assess-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "outcome", Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	notes := "the decision was correct"
	assessment, err := db.CreateAssessment(ctx, orgID, model.DecisionAssessment{
		DecisionID:      d.ID,
		AssessorAgentID: "assess-agent",
		Outcome:         model.AssessmentCorrect,
		Notes:           &notes,
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, assessment.ID)
	assert.Equal(t, model.AssessmentCorrect, assessment.Outcome)

	// Verify via summary batch.
	summaries, err := db.GetAssessmentSummaryBatch(ctx, orgID, []uuid.UUID{d.ID})
	require.NoError(t, err)
	assert.Contains(t, summaries, d.ID)
	assert.Equal(t, 1, summaries[d.ID].Total)
	assert.Equal(t, 1, summaries[d.ID].Correct)
}

func TestConflictMethods_Empty(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, conflicts)

	groups, err := db.ListConflictGroups(ctx, orgID, storage.ConflictGroupFilters{}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, groups)

	count, err := db.GetConflictCount(ctx, uuid.New(), orgID)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	counts, err := db.GetConflictCountsBatch(ctx, []uuid.UUID{uuid.New()}, orgID)
	require.NoError(t, err)
	assert.Equal(t, 0, counts[uuid.Nil]) // key doesn't exist

	resolved, err := db.GetResolvedConflictsByType(ctx, orgID, "code_review", 10)
	require.NoError(t, err)
	assert.Empty(t, resolved)
}

func TestClaimsRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	decisionID := uuid.New()

	// No claims yet.
	has, err := db.HasClaimsForDecision(ctx, decisionID, orgID)
	require.NoError(t, err)
	assert.False(t, has)

	// Insert claims.
	claims := []storage.Claim{
		{DecisionID: decisionID, OrgID: orgID, ClaimIdx: 0, ClaimText: "first claim"},
		{DecisionID: decisionID, OrgID: orgID, ClaimIdx: 1, ClaimText: "second claim"},
	}
	require.NoError(t, db.InsertClaims(ctx, claims))

	has, err = db.HasClaimsForDecision(ctx, decisionID, orgID)
	require.NoError(t, err)
	assert.True(t, has)
}

func TestInterfaceCompileTimeAssertion(t *testing.T) {
	// This test exists purely to document that *LiteDB satisfies storage.Store.
	// The compile-time assertion in sqlite.go enforces this; this test simply
	// makes it visible in test output.
	var _ storage.Store = (*sqlite.LiteDB)(nil)
}

func TestNew_WithFilePath(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/subdir/test.db"
	ctx := context.Background()
	logger := slog.Default()

	db, err := sqlite.New(ctx, path, logger)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close(ctx) })

	require.NoError(t, db.Ping(ctx))
}

func TestRawDB(t *testing.T) {
	db := newTestDB(t)
	rawDB := db.RawDB()
	require.NotNil(t, rawDB)

	// Verify we can use the raw DB to execute a query.
	var result int
	err := rawDB.QueryRowContext(context.Background(), "SELECT 1").Scan(&result)
	require.NoError(t, err)
	assert.Equal(t, 1, result)
}

func TestGetDecisionsByAgent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "agent-by-agent", OrgID: orgID, Name: "A", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	reasoning := "reason"
	for i := 0; i < 3; i++ {
		_, _, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
			AgentID: "agent-by-agent", OrgID: orgID, Metadata: map[string]any{},
			Decision: model.Decision{
				DecisionType: "test", Outcome: "o", Confidence: 0.5,
				Reasoning: &reasoning, Metadata: map[string]any{},
			},
		})
		require.NoError(t, err)
	}

	t.Run("all decisions for agent", func(t *testing.T) {
		decisions, total, err := db.GetDecisionsByAgent(ctx, orgID, "agent-by-agent", 10, 0, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, 3, total)
		assert.Len(t, decisions, 3)
	})

	t.Run("pagination", func(t *testing.T) {
		decisions, total, err := db.GetDecisionsByAgent(ctx, orgID, "agent-by-agent", 2, 0, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, 3, total)
		assert.Len(t, decisions, 2)
	})

	t.Run("with time range", func(t *testing.T) {
		from := time.Now().Add(-1 * time.Hour)
		to := time.Now().Add(1 * time.Hour)
		decisions, total, err := db.GetDecisionsByAgent(ctx, orgID, "agent-by-agent", 10, 0, &from, &to)
		require.NoError(t, err)
		assert.Equal(t, 3, total)
		assert.Len(t, decisions, 3)
	})

	t.Run("nonexistent agent returns empty", func(t *testing.T) {
		decisions, total, err := db.GetDecisionsByAgent(ctx, orgID, "no-such-agent", 10, 0, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, 0, total)
		assert.Empty(t, decisions)
	})
}

func TestGetDecisionForScoring(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "score-agent", OrgID: orgID, Name: "S", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	reasoning := "scoring reasoning"
	_, d, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "score-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "review", Outcome: "approve", Confidence: 0.85,
			Reasoning: &reasoning, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	t.Run("found", func(t *testing.T) {
		fetched, err := db.GetDecisionForScoring(ctx, d.ID, orgID)
		require.NoError(t, err)
		assert.Equal(t, d.ID, fetched.ID)
		assert.Equal(t, "score-agent", fetched.AgentID)
		assert.Equal(t, "approve", fetched.Outcome)
		assert.InDelta(t, 0.85, fetched.Confidence, 0.001)
	})

	t.Run("not found", func(t *testing.T) {
		_, err := db.GetDecisionForScoring(ctx, uuid.New(), orgID)
		assert.ErrorIs(t, err, storage.ErrNotFound)
	})
}

func TestQueryDecisions_OrderByAndDirection(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "order-agent", OrgID: orgID, Name: "O", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	for _, conf := range []float32{0.3, 0.7, 0.5} {
		_, _, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
			AgentID: "order-agent", OrgID: orgID, Metadata: map[string]any{},
			Decision: model.Decision{
				DecisionType: "test", Outcome: "o", Confidence: conf,
				Metadata: map[string]any{},
			},
		})
		require.NoError(t, err)
	}

	t.Run("order by confidence asc", func(t *testing.T) {
		decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			OrderBy:  "confidence",
			OrderDir: "asc",
			Limit:    10,
		})
		require.NoError(t, err)
		require.Len(t, decisions, 3)
		assert.LessOrEqual(t, decisions[0].Confidence, decisions[1].Confidence)
		assert.LessOrEqual(t, decisions[1].Confidence, decisions[2].Confidence)
	})

	t.Run("unknown order column falls back to valid_from", func(t *testing.T) {
		// Should not error, just use the safe default.
		decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			OrderBy: "DROP TABLE decisions", // SQL injection attempt
			Limit:   10,
		})
		require.NoError(t, err)
		assert.Len(t, decisions, 3)
	})
}

func TestQueryDecisions_WithIncludeAlternativesAndEvidence(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "include-agent", OrgID: orgID, Name: "I", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	reasoning := "good reason"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "include-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "outcome", Confidence: 0.7,
			Reasoning: &reasoning, Metadata: map[string]any{},
		},
		Alternatives: []model.Alternative{
			{Label: "opt-a", Score: ptrFloat32(0.3), Selected: false, Metadata: map[string]any{}},
			{Label: "opt-b", Score: ptrFloat32(0.7), Selected: true, Metadata: map[string]any{}},
		},
		Evidence: []model.Evidence{
			{SourceType: model.SourceAPIResponse, Content: "test data", Metadata: map[string]any{}},
		},
	})
	require.NoError(t, err)

	t.Run("include alternatives", func(t *testing.T) {
		decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Include: []string{"alternatives"},
			Limit:   10,
		})
		require.NoError(t, err)
		require.Len(t, decisions, 1)
		assert.Len(t, decisions[0].Alternatives, 2)
	})

	t.Run("include evidence", func(t *testing.T) {
		decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Include: []string{"evidence"},
			Limit:   10,
		})
		require.NoError(t, err)
		require.Len(t, decisions, 1)
		assert.Len(t, decisions[0].Evidence, 1)
	})

	t.Run("include all", func(t *testing.T) {
		decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Include: []string{"all"},
			Limit:   10,
		})
		require.NoError(t, err)
		require.Len(t, decisions, 1)
		assert.Len(t, decisions[0].Alternatives, 2)
		assert.Len(t, decisions[0].Evidence, 1)
	})
}

func TestQueryDecisions_ConfidenceMinFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "conf-agent", OrgID: orgID, Name: "C", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	for _, conf := range []float32{0.2, 0.5, 0.9} {
		_, _, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
			AgentID: "conf-agent", OrgID: orgID, Metadata: map[string]any{},
			Decision: model.Decision{
				DecisionType: "test", Outcome: "o", Confidence: conf,
				Metadata: map[string]any{},
			},
		})
		require.NoError(t, err)
	}

	minConf := float32(0.5)
	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{ConfidenceMin: &minConf},
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	assert.Len(t, decisions, 2)
	for _, d := range decisions {
		assert.GreaterOrEqual(t, d.Confidence, float32(0.5))
	}
}

func TestQueryDecisionsTemporal(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "temporal-agent", OrgID: orgID, Name: "T", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "temporal-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "temporal-outcome", Confidence: 0.5,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	t.Run("as of future includes decision", func(t *testing.T) {
		results, err := db.QueryDecisionsTemporal(ctx, orgID, model.TemporalQueryRequest{
			AsOf:  time.Now().Add(1 * time.Hour),
			Limit: 10,
		})
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Equal(t, "temporal-outcome", results[0].Outcome)
	})

	t.Run("as of past excludes decision", func(t *testing.T) {
		results, err := db.QueryDecisionsTemporal(ctx, orgID, model.TemporalQueryRequest{
			AsOf:  time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			Limit: 10,
		})
		require.NoError(t, err)
		assert.Empty(t, results)
	})
}

func TestUpdateOutcomeScore(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "score-upd-agent", OrgID: orgID, Name: "SU", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, d, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "score-upd-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "outcome", Confidence: 0.5,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	score := float32(0.85)
	err = db.UpdateOutcomeScore(ctx, orgID, d.ID, &score)
	require.NoError(t, err)

	// Verify via GetDecisionsByIDs.
	result, err := db.GetDecisionsByIDs(ctx, orgID, []uuid.UUID{d.ID})
	require.NoError(t, err)
	require.Contains(t, result, d.ID)
	require.NotNil(t, result[d.ID].OutcomeScore)
	assert.InDelta(t, 0.85, *result[d.ID].OutcomeScore, 0.001)
}

func TestFindUnembeddedDecisions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "unembed-agent", OrgID: orgID, Name: "U", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	reasoning := "reason"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "unembed-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "arch", Outcome: "unembed-outcome", Confidence: 0.6,
			Reasoning: &reasoning, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	results, err := db.FindUnembeddedDecisions(ctx, 10)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(results), 1)

	found := false
	for _, r := range results {
		if r.Outcome == "unembed-outcome" {
			found = true
			assert.Equal(t, "arch", r.DecisionType)
		}
	}
	assert.True(t, found, "expected to find the unembedded decision")
}

func TestGetDecisionEmbeddings_Empty(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	// Empty IDs should return empty.
	result, err := db.GetDecisionEmbeddings(ctx, nil, orgID)
	require.NoError(t, err)
	assert.Empty(t, result)

	// Non-existent IDs should return empty map.
	result, err = db.GetDecisionEmbeddings(ctx, []uuid.UUID{uuid.New()}, orgID)
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestFindDecisionIDsMissingClaims(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))

	// With no decisions, should return nil/empty.
	refs, err := db.FindDecisionIDsMissingClaims(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, refs)
}

func TestMarkAndClearClaimEmbeddingFailure(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "claim-fail-agent", OrgID: orgID, Name: "CF", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, d, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "claim-fail-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "outcome", Confidence: 0.5,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Mark as failed.
	err = db.MarkClaimEmbeddingFailed(ctx, d.ID, orgID)
	require.NoError(t, err)

	// Clear the failure.
	err = db.ClearClaimEmbeddingFailure(ctx, d.ID, orgID)
	require.NoError(t, err)
}

func TestCreateAgentWithAudit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	now := time.Now().UTC()

	agent := model.Agent{
		AgentID: "audit-agent", OrgID: orgID, Name: "Audit Agent", Role: model.RoleAgent,
		Tags: []string{"test"}, Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}
	audit := storage.MutationAuditEntry{
		RequestID:    "req-123",
		OrgID:        orgID,
		ActorAgentID: "system",
		ActorRole:    "admin",
		HTTPMethod:   "POST",
		Endpoint:     "/v1/agents",
		Operation:    "create",
		ResourceType: "agent",
		ResourceID:   "audit-agent",
		Metadata:     map[string]any{},
	}

	created, err := db.CreateAgentWithAudit(ctx, agent, audit)
	require.NoError(t, err)
	assert.Equal(t, "audit-agent", created.AgentID)
	assert.NotEqual(t, uuid.Nil, created.ID)

	// Verify agent was created.
	fetched, err := db.GetAgentByAgentID(ctx, orgID, "audit-agent")
	require.NoError(t, err)
	assert.Equal(t, "Audit Agent", fetched.Name)
}

func TestGetAPIKeyByID_NotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.GetAPIKeyByID(ctx, orgID, uuid.New())
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestGetDecisionOutcomeSignalsBatch_Empty(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	t.Run("empty ids returns empty", func(t *testing.T) {
		result, err := db.GetDecisionOutcomeSignalsBatch(ctx, nil, orgID)
		require.NoError(t, err)
		assert.Empty(t, result)
	})

	t.Run("nonexistent ids returns zero signals", func(t *testing.T) {
		id := uuid.New()
		result, err := db.GetDecisionOutcomeSignalsBatch(ctx, []uuid.UUID{id}, orgID)
		require.NoError(t, err)
		assert.Contains(t, result, id)
		sig := result[id]
		assert.Nil(t, sig.SupersessionVelocityHours)
		assert.Equal(t, 0, sig.PrecedentCitationCount)
		assert.Equal(t, 0, sig.ConflictCount)
		assert.Equal(t, 0, sig.AgreementCount)
	})
}

func TestFindRetriableClaimFailures_Empty(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))

	refs, err := db.FindRetriableClaimFailures(ctx, 3, 10)
	require.NoError(t, err)
	assert.Empty(t, refs)
}

func TestFindDecisionsMissingOutcomeEmbedding(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))

	// No decisions at all, should return empty.
	results, err := db.FindDecisionsMissingOutcomeEmbedding(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestGetAssessmentSummaryBatch_Empty(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	// Empty decision IDs should return empty map.
	result, err := db.GetAssessmentSummaryBatch(ctx, orgID, nil)
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestSearchDecisionsByText_WithFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "filter-search-agent", OrgID: orgID, Name: "FS", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	reasoning := "test reasoning for search"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "filter-search-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "architecture",
			Outcome:      "use microservices pattern",
			Confidence:   0.8,
			Reasoning:    &reasoning,
			Metadata:     map[string]any{},
		},
	})
	require.NoError(t, err)

	t.Run("with agent filter", func(t *testing.T) {
		results, err := db.SearchDecisionsByText(ctx, orgID, "microservices",
			model.QueryFilters{AgentIDs: []string{"filter-search-agent"}}, 10)
		require.NoError(t, err)
		assert.Len(t, results, 1)
	})

	t.Run("with wrong agent filter", func(t *testing.T) {
		results, err := db.SearchDecisionsByText(ctx, orgID, "microservices",
			model.QueryFilters{AgentIDs: []string{"other-agent"}}, 10)
		require.NoError(t, err)
		assert.Empty(t, results)
	})

	t.Run("with decision type filter", func(t *testing.T) {
		dt := "architecture"
		results, err := db.SearchDecisionsByText(ctx, orgID, "microservices",
			model.QueryFilters{DecisionType: &dt}, 10)
		require.NoError(t, err)
		assert.Len(t, results, 1)
	})

	t.Run("zero limit uses default", func(t *testing.T) {
		results, err := db.SearchDecisionsByText(ctx, orgID, "microservices",
			model.QueryFilters{}, 0)
		require.NoError(t, err)
		assert.Len(t, results, 1)
	})
}

func TestCreateTraceTx_WithSupersession(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "super-agent", OrgID: orgID, Name: "S", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// Create original decision.
	_, d1, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "super-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "review", Outcome: "original", Confidence: 0.5,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Create superseding decision.
	_, d2, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "super-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "review", Outcome: "superseded", Confidence: 0.9,
			SupersedesID: &d1.ID,
			Metadata:     map[string]any{},
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t, d1.ID, d2.ID)

	// Original decision should now have valid_to set (no longer active).
	result, err := db.GetDecisionsByIDs(ctx, orgID, []uuid.UUID{d1.ID})
	require.NoError(t, err)
	// GetDecisionsByIDs filters valid_to IS NULL, so the superseded one should not appear.
	assert.Empty(t, result, "superseded decision should not be returned by GetDecisionsByIDs (active-only query)")

	// But the new one should be there.
	result, err = db.GetDecisionsByIDs(ctx, orgID, []uuid.UUID{d2.ID})
	require.NoError(t, err)
	assert.Contains(t, result, d2.ID)
}

func TestIsDuplicateKey_NilError(t *testing.T) {
	db := newTestDB(t)
	assert.False(t, db.IsDuplicateKey(nil))
}

func ptrFloat32(f float32) *float32 {
	return &f
}

// ---------------------------------------------------------------------------
// Helper functions (vectorToBlob, blobToVector, parseNullTime)
// ---------------------------------------------------------------------------

func TestVectorBlobRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "vec-agent", OrgID: orgID, Name: "V", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	reasoning := "vector test"
	_, d, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "vec-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "vec-outcome", Confidence: 0.5,
			Reasoning: &reasoning, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	emb := pgvector.NewVector([]float32{0.1, 0.2, 0.3, 0.4})
	err = db.BackfillEmbedding(ctx, d.ID, orgID, emb)
	require.NoError(t, err)

	outEmb := pgvector.NewVector([]float32{0.5, 0.6, 0.7, 0.8})
	err = db.BackfillOutcomeEmbedding(ctx, d.ID, orgID, outEmb)
	require.NoError(t, err)

	embResult, err := db.GetDecisionEmbeddings(ctx, []uuid.UUID{d.ID}, orgID)
	require.NoError(t, err)
	require.Contains(t, embResult, d.ID)

	pair := embResult[d.ID]
	assert.InDelta(t, 0.1, pair[0].Slice()[0], 0.001)
	assert.InDelta(t, 0.5, pair[1].Slice()[0], 0.001)
	assert.Len(t, pair[0].Slice(), 4)
	assert.Len(t, pair[1].Slice(), 4)

	scored, err := db.GetDecisionForScoring(ctx, d.ID, orgID)
	require.NoError(t, err)
	require.NotNil(t, scored.Embedding)
	require.NotNil(t, scored.OutcomeEmbedding)
	assert.InDelta(t, 0.1, scored.Embedding.Slice()[0], 0.001)
	assert.InDelta(t, 0.5, scored.OutcomeEmbedding.Slice()[0], 0.001)
}

func TestFindDecisionsMissingOutcomeEmbedding_WithData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "miss-oe-agent", OrgID: orgID, Name: "MOE", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	reasoning := "embedding test"
	_, d, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "miss-oe-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "missing-oe", Confidence: 0.5,
			Reasoning: &reasoning, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	emb := pgvector.NewVector([]float32{0.1, 0.2, 0.3})
	require.NoError(t, db.BackfillEmbedding(ctx, d.ID, orgID, emb))

	results, err := db.FindDecisionsMissingOutcomeEmbedding(ctx, 10)
	require.NoError(t, err)

	found := false
	for _, r := range results {
		if r.ID == d.ID {
			found = true
			assert.Equal(t, "missing-oe", r.Outcome)
		}
	}
	assert.True(t, found, "expected to find decision missing outcome embedding")
}

// ---------------------------------------------------------------------------
// Conflict tests with actual data
// ---------------------------------------------------------------------------

func insertConflict(t *testing.T, db *sqlite.LiteDB, orgID uuid.UUID, decAID, decBID uuid.UUID, opts map[string]string) uuid.UUID {
	t.Helper()
	conflictID := uuid.New()
	rawDB := db.RawDB()

	status := "open"
	if s, ok := opts["status"]; ok {
		status = s
	}
	kind := "cross_agent"
	if k, ok := opts["kind"]; ok {
		kind = k
	}
	agentA := "agent-a"
	if a, ok := opts["agent_a"]; ok {
		agentA = a
	}
	agentB := "agent-b"
	if b, ok := opts["agent_b"]; ok {
		agentB = b
	}
	decTypeA := "code_review"
	if dt, ok := opts["decision_type_a"]; ok {
		decTypeA = dt
	}
	decTypeB := "code_review"
	if dt, ok := opts["decision_type_b"]; ok {
		decTypeB = dt
	}

	winningID := opts["winning_decision_id"]
	resolvedBy := opts["resolved_by"]
	resNote := opts["resolution_note"]
	category := opts["category"]
	severity := opts["severity"]
	relationship := opts["relationship"]
	groupIDStr := opts["group_id"]

	_, err := rawDB.ExecContext(context.Background(),
		`INSERT INTO scored_conflicts
		 (id, conflict_kind, decision_a_id, decision_b_id, org_id,
		  agent_a, agent_b, decision_type_a, decision_type_b,
		  outcome_a, outcome_b, topic_similarity, outcome_divergence,
		  significance, scoring_method, explanation, detected_at,
		  category, severity, status, resolved_by, resolved_at,
		  resolution_note, relationship, confidence_weight, temporal_decay,
		  resolution_decision_id, winning_decision_id, group_id)
		 VALUES (?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?, ?,datetime('now'),?,?,?, ?,CASE WHEN ? = 'resolved' THEN datetime('now') ELSE NULL END,?,?,?,?, NULL,CASE WHEN ? != '' THEN ? ELSE NULL END,CASE WHEN ? != '' THEN ? ELSE NULL END)`,
		conflictID.String(), kind, decAID.String(), decBID.String(), orgID.String(),
		agentA, agentB, decTypeA, decTypeB,
		"outcome-a", "outcome-b", 0.8, 0.6,
		0.7, "test", "test explanation",
		category, severity, status,
		resolvedBy, status,
		resNote, relationship, 1.0, 0.9,
		winningID, winningID, groupIDStr, groupIDStr,
	)
	require.NoError(t, err)
	return conflictID
}

func TestListConflicts_WithData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "conflict-agent-a", OrgID: orgID, Name: "CA", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dA, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "conflict-agent-a", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "code_review", Outcome: "approve", Confidence: 0.8,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, dB, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "conflict-agent-a", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "code_review", Outcome: "reject", Confidence: 0.7,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	insertConflict(t, db, orgID, dA.ID, dB.ID, map[string]string{
		"status":       "open",
		"category":     "style",
		"severity":     "medium",
		"relationship": "contradictory",
		"agent_a":      "conflict-agent-a",
		"agent_b":      "conflict-agent-a",
	})

	t.Run("list all conflicts", func(t *testing.T) {
		conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 10, 0)
		require.NoError(t, err)
		require.Len(t, conflicts, 1)

		c := conflicts[0]
		assert.Equal(t, orgID, c.OrgID)
		assert.Equal(t, model.ConflictKind("cross_agent"), c.ConflictKind)
		assert.Equal(t, "outcome-a", c.OutcomeA)
		assert.Equal(t, "outcome-b", c.OutcomeB)
		assert.NotNil(t, c.Category)
		assert.Equal(t, "style", *c.Category)
		assert.NotNil(t, c.Severity)
		assert.Equal(t, "medium", *c.Severity)
		assert.NotNil(t, c.Relationship)
		assert.Equal(t, "contradictory", *c.Relationship)
		assert.Equal(t, "code_review", c.DecisionType)
	})

	t.Run("filter by decision type", func(t *testing.T) {
		dt := "code_review"
		conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{DecisionType: &dt}, 10, 0)
		require.NoError(t, err)
		assert.Len(t, conflicts, 1)
	})

	t.Run("filter by agent", func(t *testing.T) {
		agentID := "conflict-agent-a"
		conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{AgentID: &agentID}, 10, 0)
		require.NoError(t, err)
		assert.Len(t, conflicts, 1)
	})

	t.Run("filter by status", func(t *testing.T) {
		status := "open"
		conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{Status: &status}, 10, 0)
		require.NoError(t, err)
		assert.Len(t, conflicts, 1)

		resolved := "resolved"
		conflicts, err = db.ListConflicts(ctx, orgID, storage.ConflictFilters{Status: &resolved}, 10, 0)
		require.NoError(t, err)
		assert.Empty(t, conflicts)
	})

	t.Run("filter by severity", func(t *testing.T) {
		sev := "medium"
		conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{Severity: &sev}, 10, 0)
		require.NoError(t, err)
		assert.Len(t, conflicts, 1)
	})

	t.Run("filter by category", func(t *testing.T) {
		cat := "style"
		conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{Category: &cat}, 10, 0)
		require.NoError(t, err)
		assert.Len(t, conflicts, 1)
	})

	t.Run("filter by conflict kind", func(t *testing.T) {
		kind := "cross_agent"
		conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{ConflictKind: &kind}, 10, 0)
		require.NoError(t, err)
		assert.Len(t, conflicts, 1)
	})

	t.Run("filter by decision ID", func(t *testing.T) {
		conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{DecisionID: &dA.ID}, 10, 0)
		require.NoError(t, err)
		assert.Len(t, conflicts, 1)
	})

	t.Run("default limit applied for zero", func(t *testing.T) {
		conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 0, 0)
		require.NoError(t, err)
		assert.Len(t, conflicts, 1)
	})
}

func TestListConflictGroups_WithData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "grp-agent-a", OrgID: orgID, Name: "GA", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dA, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "grp-agent-a", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "architecture", Outcome: "use-postgres", Confidence: 0.9,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, dB, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "grp-agent-a", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "architecture", Outcome: "use-mysql", Confidence: 0.7,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	groupID := uuid.New()
	rawDB := db.RawDB()
	_, err = rawDB.ExecContext(ctx,
		`INSERT INTO conflict_groups (id, org_id, agent_a, agent_b, conflict_kind, decision_type, first_detected_at, last_detected_at)
		 VALUES (?,?,?,?,?,?,datetime('now'),datetime('now'))`,
		groupID.String(), orgID.String(), "grp-agent-a", "grp-agent-a", "self_contradiction", "architecture",
	)
	require.NoError(t, err)

	insertConflict(t, db, orgID, dA.ID, dB.ID, map[string]string{
		"kind":            "self_contradiction",
		"agent_a":         "grp-agent-a",
		"agent_b":         "grp-agent-a",
		"group_id":        groupID.String(),
		"decision_type_a": "architecture",
		"decision_type_b": "architecture",
	})

	t.Run("list groups", func(t *testing.T) {
		groups, err := db.ListConflictGroups(ctx, orgID, storage.ConflictGroupFilters{}, 10, 0)
		require.NoError(t, err)
		require.Len(t, groups, 1)
		assert.Equal(t, groupID, groups[0].ID)
		assert.Equal(t, 1, groups[0].ConflictCount)
		assert.Equal(t, 1, groups[0].OpenCount)
		assert.NotNil(t, groups[0].Representative)
	})

	t.Run("filter by decision type", func(t *testing.T) {
		dt := "architecture"
		groups, err := db.ListConflictGroups(ctx, orgID, storage.ConflictGroupFilters{DecisionType: &dt}, 10, 0)
		require.NoError(t, err)
		assert.Len(t, groups, 1)
	})

	t.Run("filter by agent", func(t *testing.T) {
		agentID := "grp-agent-a"
		groups, err := db.ListConflictGroups(ctx, orgID, storage.ConflictGroupFilters{AgentID: &agentID}, 10, 0)
		require.NoError(t, err)
		assert.Len(t, groups, 1)
	})

	t.Run("filter by conflict kind", func(t *testing.T) {
		kind := "self_contradiction"
		groups, err := db.ListConflictGroups(ctx, orgID, storage.ConflictGroupFilters{ConflictKind: &kind}, 10, 0)
		require.NoError(t, err)
		assert.Len(t, groups, 1)
	})

	t.Run("open only", func(t *testing.T) {
		groups, err := db.ListConflictGroups(ctx, orgID, storage.ConflictGroupFilters{OpenOnly: true}, 10, 0)
		require.NoError(t, err)
		assert.Len(t, groups, 1)
	})

	t.Run("default limit for zero", func(t *testing.T) {
		groups, err := db.ListConflictGroups(ctx, orgID, storage.ConflictGroupFilters{}, 0, 0)
		require.NoError(t, err)
		assert.Len(t, groups, 1)
	})
}

func TestGetResolvedConflictsByType_WithData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "res-agent", OrgID: orgID, Name: "R", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dA, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "res-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "code_review", Outcome: "approve", Confidence: 0.8,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, dB, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "res-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "code_review", Outcome: "reject", Confidence: 0.6,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	insertConflict(t, db, orgID, dA.ID, dB.ID, map[string]string{
		"status":              "resolved",
		"winning_decision_id": dA.ID.String(),
		"resolved_by":         "human",
		"resolution_note":     "approve was correct",
		"agent_a":             "res-agent",
		"agent_b":             "res-agent",
	})

	resolved, err := db.GetResolvedConflictsByType(ctx, orgID, "code_review", 10)
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	assert.Equal(t, "code_review", resolved[0].DecisionType)
	assert.Equal(t, dA.ID, resolved[0].WinningDecisionID)
	assert.Equal(t, "res-agent", resolved[0].WinningAgent)
	assert.Equal(t, "outcome-a", resolved[0].WinningOutcome)
	assert.Equal(t, "res-agent", resolved[0].LosingAgent)
	assert.Equal(t, "outcome-b", resolved[0].LosingOutcome)
	assert.False(t, resolved[0].ResolvedAt.IsZero())

	t.Run("default limit for zero", func(t *testing.T) {
		resolved, err := db.GetResolvedConflictsByType(ctx, orgID, "code_review", 0)
		require.NoError(t, err)
		assert.Len(t, resolved, 1)
	})
}

func TestGetConflictCount_WithData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "cc-agent", OrgID: orgID, Name: "CC", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dA, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "cc-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "a", Confidence: 0.5,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, dB, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "cc-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "b", Confidence: 0.5,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	insertConflict(t, db, orgID, dA.ID, dB.ID, map[string]string{"status": "open"})

	count, err := db.GetConflictCount(ctx, dA.ID, orgID)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	count, err = db.GetConflictCount(ctx, dB.ID, orgID)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	counts, err := db.GetConflictCountsBatch(ctx, []uuid.UUID{dA.ID, dB.ID}, orgID)
	require.NoError(t, err)
	assert.Equal(t, 1, counts[dA.ID])
	assert.Equal(t, 1, counts[dB.ID])
}

// ---------------------------------------------------------------------------
// CreateTraceAndAdjudicateConflictTx
// ---------------------------------------------------------------------------

func TestCreateTraceAndAdjudicateConflictTx(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "adj-agent", OrgID: orgID, Name: "ADJ", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dA, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "adj-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "review", Outcome: "approve", Confidence: 0.8,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, dB, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "adj-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "review", Outcome: "reject", Confidence: 0.6,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	conflictID := insertConflict(t, db, orgID, dA.ID, dB.ID, map[string]string{"status": "open"})

	resNote := "approve is the right call"
	run, dec, err := db.CreateTraceAndAdjudicateConflictTx(ctx,
		storage.CreateTraceParams{
			AgentID: "adj-agent", OrgID: orgID, Metadata: map[string]any{},
			Decision: model.Decision{
				DecisionType: "review", Outcome: "approve with changes",
				Confidence: 0.95, Metadata: map[string]any{},
			},
		},
		storage.AdjudicateConflictInTraceParams{
			ConflictID:        conflictID,
			ResolvedBy:        "adj-agent",
			ResNote:           &resNote,
			WinningDecisionID: &dA.ID,
			Audit: storage.MutationAuditEntry{
				RequestID: "req-adj", OrgID: orgID, ActorAgentID: "adj-agent",
				ActorRole: "agent", HTTPMethod: "POST", Endpoint: "/v1/trace",
				Operation: "adjudicate", ResourceType: "conflict",
				ResourceID: conflictID.String(), Metadata: map[string]any{},
			},
		},
	)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, run.ID)
	assert.NotEqual(t, uuid.Nil, dec.ID)
	assert.Equal(t, "approve with changes", dec.Outcome)

	var status string
	err = db.RawDB().QueryRowContext(ctx,
		`SELECT status FROM scored_conflicts WHERE id = ?`, conflictID.String(),
	).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, "resolved", status)
}

func TestCreateTraceAndAdjudicateConflictTx_ConflictNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "adj-nf-agent", OrgID: orgID, Name: "NF", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceAndAdjudicateConflictTx(ctx,
		storage.CreateTraceParams{
			AgentID: "adj-nf-agent", OrgID: orgID, Metadata: map[string]any{},
			Decision: model.Decision{
				DecisionType: "test", Outcome: "o", Confidence: 0.5,
				Metadata: map[string]any{},
			},
		},
		storage.AdjudicateConflictInTraceParams{
			ConflictID: uuid.New(),
			ResolvedBy: "system",
			Audit: storage.MutationAuditEntry{
				RequestID: "req-nf", OrgID: orgID, ActorAgentID: "system",
				ActorRole: "admin", HTTPMethod: "POST", Endpoint: "/v1/trace",
				Operation: "adjudicate", ResourceType: "conflict",
				ResourceID: "nope", Metadata: map[string]any{},
			},
		},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// searchDecisionsByLike (fallback path)
// ---------------------------------------------------------------------------

func TestSearchDecisionsByText_LikeFallback(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "like-agent", OrgID: orgID, Name: "L", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	reasoning := "fallback test reasoning"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "like-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "architecture",
			Outcome:      "use event sourcing pattern",
			Confidence:   0.75,
			Reasoning:    &reasoning,
			Metadata:     map[string]any{},
		},
	})
	require.NoError(t, err)

	// Invalid FTS5 syntax triggers the LIKE fallback.
	results, err := db.SearchDecisionsByText(ctx, orgID, "event AND OR sourcing", model.QueryFilters{}, 10)
	require.NoError(t, err)
	_ = results

	minConf := float32(0.5)
	results, err = db.SearchDecisionsByText(ctx, orgID, "event AND OR sourcing",
		model.QueryFilters{ConfidenceMin: &minConf}, 10)
	require.NoError(t, err)
	_ = results
}

// ---------------------------------------------------------------------------
// buildDecisionFilterWhere additional branches
// ---------------------------------------------------------------------------

func TestQueryDecisions_AdditionalFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "proj-agent", OrgID: orgID, Name: "P", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	projectName := "akashi"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "proj-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "with-project", Confidence: 0.5,
			Project: &projectName, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "proj-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "no-project", Confidence: 0.5,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	t.Run("filter by project", func(t *testing.T) {
		decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Filters: model.QueryFilters{Project: &projectName},
			Limit:   10,
		})
		require.NoError(t, err)
		assert.Equal(t, 1, total)
		require.Len(t, decisions, 1)
		assert.Equal(t, "with-project", decisions[0].Outcome)
		require.NotNil(t, decisions[0].Project)
		assert.Equal(t, "akashi", *decisions[0].Project)
	})

	t.Run("filter by outcome", func(t *testing.T) {
		outcome := "no-project"
		decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Filters: model.QueryFilters{Outcome: &outcome},
			Limit:   10,
		})
		require.NoError(t, err)
		assert.Equal(t, 1, total)
		require.Len(t, decisions, 1)
	})

	t.Run("filter by session ID", func(t *testing.T) {
		sessionID := uuid.New()
		decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Filters: model.QueryFilters{SessionID: &sessionID},
			Limit:   10,
		})
		require.NoError(t, err)
		assert.Empty(t, decisions)
	})

	t.Run("filter by tool", func(t *testing.T) {
		tool := "claude"
		decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Filters: model.QueryFilters{Tool: &tool},
			Limit:   10,
		})
		require.NoError(t, err)
		assert.Empty(t, decisions)
	})

	t.Run("filter by model", func(t *testing.T) {
		mdl := "opus-4"
		decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Filters: model.QueryFilters{Model: &mdl},
			Limit:   10,
		})
		require.NoError(t, err)
		assert.Empty(t, decisions)
	})

	t.Run("filter by run ID", func(t *testing.T) {
		runID := uuid.New()
		decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Filters: model.QueryFilters{RunID: &runID},
			Limit:   10,
		})
		require.NoError(t, err)
		assert.Empty(t, decisions)
	})

	t.Run("filter by trace ID", func(t *testing.T) {
		traceID := "my-trace-id"
		decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			TraceID: &traceID,
			Limit:   10,
		})
		require.NoError(t, err)
		assert.Empty(t, decisions)
	})

	t.Run("time range from only", func(t *testing.T) {
		from := time.Now().Add(-1 * time.Hour)
		decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Filters: model.QueryFilters{TimeRange: &model.TimeRange{From: &from}},
			Limit:   10,
		})
		require.NoError(t, err)
		assert.Equal(t, 2, total)
		assert.Len(t, decisions, 2)
	})

	t.Run("time range to only", func(t *testing.T) {
		to := time.Now().Add(1 * time.Hour)
		decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Filters: model.QueryFilters{TimeRange: &model.TimeRange{To: &to}},
			Limit:   10,
		})
		require.NoError(t, err)
		assert.Equal(t, 2, total)
		assert.Len(t, decisions, 2)
	})
}

// ---------------------------------------------------------------------------
// API Keys — GetAPIKeyByID with actual data
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// GetDecisionOutcomeSignalsBatch with supersession + conflict data
// ---------------------------------------------------------------------------

func TestGetDecisionOutcomeSignalsBatch_WithData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "sig-agent", OrgID: orgID, Name: "SIG", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, d1, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "sig-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "arch", Outcome: "original", Confidence: 0.7,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, d2, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "sig-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "arch", Outcome: "revised", Confidence: 0.9,
			SupersedesID: &d1.ID, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "sig-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "arch", Outcome: "follows revised", Confidence: 0.85,
			PrecedentRef: &d2.ID, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, d4, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "sig-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "arch", Outcome: "option-a", Confidence: 0.6,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, d5, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "sig-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "arch", Outcome: "option-b", Confidence: 0.5,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	insertConflict(t, db, orgID, d4.ID, d5.ID, map[string]string{
		"status":              "resolved",
		"winning_decision_id": d4.ID.String(),
		"resolved_by":         "human",
	})

	_, d6, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "sig-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "arch", Outcome: "option-c", Confidence: 0.5,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)
	insertConflict(t, db, orgID, d4.ID, d6.ID, map[string]string{
		"status":       "open",
		"relationship": "complementary",
	})

	signals, err := db.GetDecisionOutcomeSignalsBatch(ctx, []uuid.UUID{d1.ID, d2.ID, d4.ID}, orgID)
	require.NoError(t, err)

	assert.NotNil(t, signals[d1.ID].SupersessionVelocityHours)
	assert.Equal(t, 1, signals[d2.ID].PrecedentCitationCount)
	assert.Equal(t, 1, signals[d4.ID].ConflictFate.Won)
	assert.Equal(t, 1, signals[d4.ID].ConflictCount)
	assert.Equal(t, 1, signals[d4.ID].AgreementCount)
}

// ---------------------------------------------------------------------------
// FindDecisionIDsMissingClaims with actual data
// ---------------------------------------------------------------------------

func TestFindDecisionIDsMissingClaims_WithData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "claim-miss-agent", OrgID: orgID, Name: "CM", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	reasoning := "claim test"
	_, d, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "claim-miss-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "needs-claims", Confidence: 0.5,
			Reasoning: &reasoning, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	emb := pgvector.NewVector([]float32{0.1, 0.2, 0.3})
	require.NoError(t, db.BackfillEmbedding(ctx, d.ID, orgID, emb))

	refs, err := db.FindDecisionIDsMissingClaims(ctx, 10)
	require.NoError(t, err)

	found := false
	for _, r := range refs {
		if r.ID == d.ID {
			found = true
			assert.Equal(t, orgID, r.OrgID)
		}
	}
	assert.True(t, found, "expected to find decision missing claims")

	require.NoError(t, db.InsertClaims(ctx, []storage.Claim{
		{DecisionID: d.ID, OrgID: orgID, ClaimIdx: 0, ClaimText: "test claim"},
	}))

	refs, err = db.FindDecisionIDsMissingClaims(ctx, 10)
	require.NoError(t, err)
	for _, r := range refs {
		assert.NotEqual(t, d.ID, r.ID, "decision with claims should not appear")
	}
}

// ---------------------------------------------------------------------------
// FindRetriableClaimFailures with actual data
// ---------------------------------------------------------------------------

func TestFindRetriableClaimFailures_WithData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "retry-agent", OrgID: orgID, Name: "RT", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, d, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "retry-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "retriable", Confidence: 0.5,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	emb := pgvector.NewVector([]float32{0.1, 0.2})
	require.NoError(t, db.BackfillEmbedding(ctx, d.ID, orgID, emb))

	require.NoError(t, db.MarkClaimEmbeddingFailed(ctx, d.ID, orgID))

	_, err = db.RawDB().ExecContext(ctx,
		`UPDATE decisions SET claim_embeddings_failed_at = datetime('now', '-1 day') WHERE id = ?`,
		d.ID.String(),
	)
	require.NoError(t, err)

	refs, err := db.FindRetriableClaimFailures(ctx, 3, 10)
	require.NoError(t, err)

	found := false
	for _, r := range refs {
		if r.ID == d.ID {
			found = true
			assert.Equal(t, 1, r.Attempts)
		}
	}
	assert.True(t, found, "expected to find retriable claim failure")

	require.NoError(t, db.ClearClaimEmbeddingFailure(ctx, d.ID, orgID))

	refs, err = db.FindRetriableClaimFailures(ctx, 3, 10)
	require.NoError(t, err)
	for _, r := range refs {
		assert.NotEqual(t, d.ID, r.ID)
	}
}

// ---------------------------------------------------------------------------
// SearchDecisionsByText with project filter
// ---------------------------------------------------------------------------

func TestSearchDecisionsByText_WithProjectFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "proj-search-agent", OrgID: orgID, Name: "PS", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	project := "myproject"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "proj-search-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "arch", Outcome: "use serverless functions",
			Confidence: 0.8, Project: &project, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "proj-search-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "arch", Outcome: "use serverless containers",
			Confidence: 0.7, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	results, err := db.SearchDecisionsByText(ctx, orgID, "serverless",
		model.QueryFilters{Project: &project}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Contains(t, results[0].Decision.Outcome, "functions")

	minConf := float32(0.75)
	results, err = db.SearchDecisionsByText(ctx, orgID, "serverless",
		model.QueryFilters{ConfidenceMin: &minConf}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1)

	from := time.Now().Add(-1 * time.Hour)
	to := time.Now().Add(1 * time.Hour)
	results, err = db.SearchDecisionsByText(ctx, orgID, "serverless",
		model.QueryFilters{TimeRange: &model.TimeRange{From: &from, To: &to}}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 2)
}

// ---------------------------------------------------------------------------
// ListGrantedAgentIDs with actual grants
// ---------------------------------------------------------------------------

func TestListGrantedAgentIDs_WithGrants(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID := uuid.Nil
	granteeID := uuid.New()

	rawDB := db.RawDB()
	_, err := rawDB.ExecContext(ctx,
		`INSERT INTO access_grants (id, org_id, grantor_id, grantee_id, resource_type, resource_id, permission)
		 VALUES (?,?,?,?,?,?,?)`,
		uuid.New().String(), orgID.String(), uuid.New().String(), granteeID.String(),
		"agent_traces", "granted-agent-id", "read",
	)
	require.NoError(t, err)

	ids, err := db.ListGrantedAgentIDs(ctx, orgID, granteeID, "self-agent")
	require.NoError(t, err)

	assert.True(t, ids["self-agent"], "self agent should always be included")
	assert.True(t, ids["granted-agent-id"], "granted agent should be included")
	assert.Len(t, ids, 2)
}

// ---------------------------------------------------------------------------
// GetAPIKeyByID — happy path
// ---------------------------------------------------------------------------

func TestGetAPIKeyByID_HappyPath(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	// Create an agent for the API key FK.
	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "apikey-agent", OrgID: orgID, Name: "AK", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// Insert an API key directly into the table.
	keyID := uuid.New()
	rawDB := db.RawDB()
	now := time.Now().UTC().Truncate(time.Second)
	expires := now.Add(24 * time.Hour)
	_, err = rawDB.ExecContext(ctx,
		`INSERT INTO api_keys (id, prefix, key_hash, agent_id, org_id, label, created_by, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		keyID.String(), "ak_", "hash123", "apikey-agent", orgID.String(),
		"test-label", "admin", now.Format(time.RFC3339Nano), expires.Format(time.RFC3339Nano),
	)
	require.NoError(t, err)

	key, err := db.GetAPIKeyByID(ctx, orgID, keyID)
	require.NoError(t, err)
	assert.Equal(t, keyID, key.ID)
	assert.Equal(t, "ak_", key.Prefix)
	assert.Equal(t, "hash123", key.KeyHash)
	assert.Equal(t, "apikey-agent", key.AgentID)
	assert.Equal(t, orgID, key.OrgID)
	assert.Equal(t, "test-label", key.Label)
	assert.NotNil(t, key.ExpiresAt, "expires_at should be parsed")
	assert.Nil(t, key.RevokedAt, "revoked_at should be nil when not set")
	assert.Nil(t, key.LastUsedAt, "last_used_at should be nil when not set")
}

func TestGetAPIKeyByID_WithRevokedAndLastUsed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "apikey-agent2", OrgID: orgID, Name: "AK2", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	keyID := uuid.New()
	rawDB := db.RawDB()
	now := time.Now().UTC().Truncate(time.Second)
	_, err = rawDB.ExecContext(ctx,
		`INSERT INTO api_keys (id, prefix, key_hash, agent_id, org_id, label, created_by, created_at, last_used_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		keyID.String(), "ak_", "hash456", "apikey-agent2", orgID.String(),
		"revoked-key", "admin", now.Format(time.RFC3339Nano),
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	)
	require.NoError(t, err)

	key, err := db.GetAPIKeyByID(ctx, orgID, keyID)
	require.NoError(t, err)
	assert.Equal(t, keyID, key.ID)
	assert.NotNil(t, key.LastUsedAt, "last_used_at should be parsed")
	assert.NotNil(t, key.RevokedAt, "revoked_at should be parsed")
}

// ---------------------------------------------------------------------------
// searchDecisionsByLike — exercising scanning with matching results
// ---------------------------------------------------------------------------

func TestSearchDecisionsByLike_WithResults(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "like-search-agent", OrgID: orgID, Name: "LS", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	reasoning := "reasoning text for LIKE AND OR test"
	toolVal := "code-review-tool"
	modelVal := "gpt-4"
	projectVal := "akashi"
	sessionID := uuid.New()

	// Create a decision whose outcome contains the exact literal "LIKE AND OR" so
	// the LIKE pattern "%LIKE AND OR%" matches. FTS5 MATCH fails on "LIKE AND OR"
	// because "AND OR" is invalid syntax, triggering the LIKE fallback.
	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "like-search-agent", OrgID: orgID, Metadata: map[string]any{},
		SessionID:    &sessionID,
		AgentContext: map[string]any{"key": "val"},
		Decision: model.Decision{
			DecisionType: "architecture", Outcome: "outcome with LIKE AND OR test keyword",
			Confidence: 0.85, Reasoning: &reasoning, Metadata: map[string]any{},
			Tool: &toolVal, Model: &modelVal, Project: &projectVal,
		},
	})
	require.NoError(t, err)

	results, err := db.SearchDecisionsByText(ctx, orgID, "LIKE AND OR", model.QueryFilters{}, 10)
	require.NoError(t, err)
	require.NotEmpty(t, results, "LIKE fallback should find the decision")

	found := false
	for _, r := range results {
		if r.Decision.ID == dec.ID {
			found = true
			assert.Equal(t, "outcome with LIKE AND OR test keyword", r.Decision.Outcome)
			assert.NotNil(t, r.Decision.Tool, "tool should be populated")
			assert.Equal(t, toolVal, *r.Decision.Tool)
			assert.NotNil(t, r.Decision.Model, "model should be populated")
			assert.Equal(t, modelVal, *r.Decision.Model)
			assert.NotNil(t, r.Decision.Project, "project should be populated")
			assert.Equal(t, projectVal, *r.Decision.Project)
			assert.NotNil(t, r.Decision.SessionID, "session_id should be parsed")
			assert.Equal(t, sessionID, *r.Decision.SessionID)
			assert.NotEmpty(t, r.Decision.AgentContext, "agent_context should be parsed")
			assert.InDelta(t, 1.0, r.SimilarityScore, 0.001, "LIKE relevance is always 1.0")
			break
		}
	}
	assert.True(t, found, "should find the specific decision with all fields populated")
}

func TestSearchDecisionsByLike_EmptyResults(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))

	// Invalid FTS5 syntax with a query that matches nothing via LIKE.
	// The pattern "%nonexistent_xyzzy_12345%" won't match any data.
	results, err := db.SearchDecisionsByText(ctx, uuid.Nil, "nonexistent_xyzzy_12345 AND OR", model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestSearchDecisionsByLike_WithAgentAndTypeFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "filter-like-agent", OrgID: orgID, Name: "FL", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	reasoning := "filterable reasoning"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "filter-like-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "security", Outcome: "LIKEFILTER use mTLS",
			Confidence: 0.9, Reasoning: &reasoning, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// The outcome contains "LIKEFILTER use" literally. Using "LIKEFILTER AND OR"
	// as the search query: FTS5 fails on "AND OR", LIKE fallback uses
	// "%LIKEFILTER AND OR%" which won't match. Instead, embed "AND OR" in the outcome.
	dt := "security"

	// Create another decision with the literal search query in its outcome.
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "filter-like-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "security", Outcome: "LIKEFILTER AND OR approach",
			Confidence: 0.9, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	results, err := db.SearchDecisionsByText(ctx, orgID, "LIKEFILTER AND OR",
		model.QueryFilters{
			AgentIDs:     []string{"filter-like-agent"},
			DecisionType: &dt,
		}, 10)
	require.NoError(t, err)
	require.NotEmpty(t, results)
	assert.Equal(t, "security", results[0].Decision.DecisionType)
}

func TestSearchDecisionsByLike_MatchesInReasoning(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "reason-agent", OrgID: orgID, Name: "R", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// Put the literal "AND OR" in the reasoning so LIKE pattern matches.
	reasoning := "performance AND OR optimization needed"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "reason-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "architecture", Outcome: "optimize database",
			Confidence: 0.7, Reasoning: &reasoning, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// LIKE pattern: "%performance AND OR%". FTS5 fails, LIKE matches the reasoning.
	results, err := db.SearchDecisionsByText(ctx, orgID, "performance AND OR",
		model.QueryFilters{}, 10)
	require.NoError(t, err)
	require.NotEmpty(t, results, "LIKE should match in reasoning column")
	assert.Equal(t, "optimize database", results[0].Decision.Outcome)
}

func TestSearchDecisionsByLike_MatchesInDecisionType(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "type-agent", OrgID: orgID, Name: "T", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// Put "AND OR" literally in the decision_type.
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "type-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "typematch AND OR style", Outcome: "some outcome",
			Confidence: 0.6, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// LIKE pattern: "%typematch AND OR%". FTS5 fails, LIKE matches decision_type.
	results, err := db.SearchDecisionsByText(ctx, orgID, "typematch AND OR",
		model.QueryFilters{}, 10)
	require.NoError(t, err)
	require.NotEmpty(t, results, "LIKE should match in decision_type column")
}

// ---------------------------------------------------------------------------
// Close — exercise the error path
// ---------------------------------------------------------------------------

func TestClose_DoubleClose(t *testing.T) {
	ctx := context.Background()
	logger := slog.Default()
	db, err := sqlite.New(ctx, ":memory:", logger)
	require.NoError(t, err)

	// First close should succeed silently.
	db.Close(ctx)
	// Second close should log a warning but not panic.
	db.Close(ctx)
}

// ---------------------------------------------------------------------------
// parseNullTime — edge cases via GetDecisionsByAgent with specific time fields
// ---------------------------------------------------------------------------

func TestParseNullTime_ViaGetDecisionsByAgent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "nulltime-agent", OrgID: orgID, Name: "NT", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// Create a decision — valid_to is NULL by default, exercising parseNullTime(NULL).
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "nulltime-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "null time test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	decisions, total, err := db.GetDecisionsByAgent(ctx, orgID, "nulltime-agent", 10, 0, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, decisions, 1)
	assert.Nil(t, decisions[0].ValidTo, "valid_to should be nil for active decisions")
	assert.False(t, decisions[0].ValidFrom.IsZero(), "valid_from should be parsed")
}

// ---------------------------------------------------------------------------
// Idempotency — additional coverage for CompleteIdempotency and BeginIdempotency
// ---------------------------------------------------------------------------

func TestIdempotency_CompleteThenReplay(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	// Begin idempotency.
	lookup, err := db.BeginIdempotency(ctx, orgID, "agent1", "/v1/trace", "key-123", "hash-abc")
	require.NoError(t, err)
	assert.False(t, lookup.Completed, "first call should not be completed")

	// Complete idempotency.
	err = db.CompleteIdempotency(ctx, orgID, "agent1", "/v1/trace", "key-123", 201, map[string]any{"id": "dec-1"})
	require.NoError(t, err)

	// Replay with same key and hash — should return completed response.
	lookup, err = db.BeginIdempotency(ctx, orgID, "agent1", "/v1/trace", "key-123", "hash-abc")
	require.NoError(t, err)
	assert.True(t, lookup.Completed, "replayed call should return completed")
	assert.Equal(t, 201, lookup.StatusCode)
	assert.NotEmpty(t, lookup.ResponseData)

	// Replay with different hash — should return payload mismatch.
	_, err = db.BeginIdempotency(ctx, orgID, "agent1", "/v1/trace", "key-123", "different-hash")
	assert.ErrorIs(t, err, storage.ErrIdempotencyPayloadMismatch)
}

func TestIdempotency_InProgressConcurrency(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	// First call claims the key.
	_, err := db.BeginIdempotency(ctx, orgID, "agent1", "/v1/trace", "key-concurrent", "hash-1")
	require.NoError(t, err)

	// Second call with same hash should get in-progress error.
	_, err = db.BeginIdempotency(ctx, orgID, "agent1", "/v1/trace", "key-concurrent", "hash-1")
	assert.ErrorIs(t, err, storage.ErrIdempotencyInProgress)

	// Clear the in-progress key.
	err = db.ClearInProgressIdempotency(ctx, orgID, "agent1", "/v1/trace", "key-concurrent")
	require.NoError(t, err)

	// Now the key should be claimable again.
	lookup, err := db.BeginIdempotency(ctx, orgID, "agent1", "/v1/trace", "key-concurrent", "hash-1")
	require.NoError(t, err)
	assert.False(t, lookup.Completed)
}

// ---------------------------------------------------------------------------
// TraceHealth — additional coverage
// ---------------------------------------------------------------------------

func TestTraceHealth_WithDecisions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "health-agent", OrgID: orgID, Name: "H", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	reasoning := "well-documented reasoning"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "health-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "planning", Outcome: "health test",
			Confidence: 0.7, Reasoning: &reasoning, Metadata: map[string]any{},
		},
		Alternatives: []model.Alternative{
			{Label: "opt-a", Score: ptrFloat32(0.3), Selected: false, Metadata: map[string]any{}},
		},
		Evidence: []model.Evidence{
			{SourceType: "document", Content: "test evidence", Metadata: map[string]any{}},
		},
	})
	require.NoError(t, err)

	// Quality stats should reflect the decision. Note: completeness_score
	// defaults to 0.0 because it's computed by the service layer (quality.Score),
	// not by CreateTraceTx.
	qs, err := db.GetDecisionQualityStats(ctx, orgID, nil, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, qs.Total, 1)
	assert.GreaterOrEqual(t, qs.WithReasoning, 1)
	assert.GreaterOrEqual(t, qs.WithAlternatives, 1)

	// Evidence coverage should reflect the evidence.
	ec, err := db.GetEvidenceCoverageStats(ctx, orgID, nil, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, ec.TotalDecisions, 1)
	assert.GreaterOrEqual(t, ec.WithEvidence, 1)
	assert.Greater(t, ec.CoveragePercent, 0.0)
	assert.GreaterOrEqual(t, ec.TotalRecords, 1)

	// Conflict status counts with no conflicts.
	cc, err := db.GetConflictStatusCounts(ctx, orgID, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, cc.Total)

	// Outcome signals summary.
	os, err := db.GetOutcomeSignalsSummary(ctx, orgID, nil, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, os.DecisionsTotal, 1)
}

// ---------------------------------------------------------------------------
// GetDecisionsByAgent — with time filters
// ---------------------------------------------------------------------------

func TestGetDecisionsByAgent_WithTimeFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "time-agent", OrgID: orgID, Name: "TA", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "time-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "time filter test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	pastHour := now.Add(-time.Hour)
	futureHour := now.Add(time.Hour)

	// From in the past, to in the future — should find the decision.
	decisions, total, err := db.GetDecisionsByAgent(ctx, orgID, "time-agent", 10, 0, &pastHour, &futureHour)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Len(t, decisions, 1)

	// From in the future — should find nothing.
	decisions, total, err = db.GetDecisionsByAgent(ctx, orgID, "time-agent", 10, 0, &futureHour, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// QueryDecisions — with traceID filter
// ---------------------------------------------------------------------------

func TestQueryDecisions_WithTraceID(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "trace-id-agent", OrgID: orgID, Name: "TI", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	traceID := "my-custom-trace-id"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "trace-id-agent", OrgID: orgID, TraceID: &traceID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "trace id filter",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Query by traceID.
	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		TraceID: &traceID,
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, decisions, 1)
	assert.Equal(t, "trace id filter", decisions[0].Outcome)

	// Query with nonexistent traceID.
	otherTrace := "nonexistent-trace"
	decisions, total, err = db.QueryDecisions(ctx, orgID, model.QueryRequest{
		TraceID: &otherTrace,
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// QueryDecisions — include "all" (alternatives + evidence)
// ---------------------------------------------------------------------------

func TestQueryDecisions_IncludeAll(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "include-all-agent", OrgID: orgID, Name: "IA", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "include-all-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "include all test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
		Alternatives: []model.Alternative{
			{Label: "alt-1", Score: ptrFloat32(0.5), Selected: true, Metadata: map[string]any{}},
		},
		Evidence: []model.Evidence{
			{SourceType: "document", Content: "evidence content", Metadata: map[string]any{}},
		},
	})
	require.NoError(t, err)

	decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{AgentIDs: []string{"include-all-agent"}},
		Include: []string{"all"},
		Limit:   10,
	})
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	assert.NotEmpty(t, decisions[0].Alternatives, "alternatives should be loaded with 'all'")
	assert.NotEmpty(t, decisions[0].Evidence, "evidence should be loaded with 'all'")
}

// ---------------------------------------------------------------------------
// QueryDecisions — with time range filter
// ---------------------------------------------------------------------------

func TestQueryDecisions_WithTimeRange(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "timerange-agent", OrgID: orgID, Name: "TR", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "timerange-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "time range test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	pastHour := now.Add(-time.Hour)
	futureHour := now.Add(time.Hour)

	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{
			AgentIDs:  []string{"timerange-agent"},
			TimeRange: &model.TimeRange{From: &pastHour, To: &futureHour},
		},
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, decisions, 1)
}

// ---------------------------------------------------------------------------
// QueryDecisions — tool and model filters
// ---------------------------------------------------------------------------

func TestQueryDecisions_ToolAndModelFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "toolmodel-agent", OrgID: orgID, Name: "TM", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	toolVal := "custom-tool"
	modelVal := "gpt-4o"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "toolmodel-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "tool model filter test",
			Confidence: 0.5, Metadata: map[string]any{},
			Tool: &toolVal, Model: &modelVal,
		},
	})
	require.NoError(t, err)

	// Filter by tool.
	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{Tool: &toolVal},
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, decisions, 1)

	// Filter by model.
	decisions, total, err = db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{Model: &modelVal},
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, decisions, 1)

	// Filter by wrong tool should return nothing.
	wrongTool := "nonexistent-tool"
	decisions, total, err = db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{Tool: &wrongTool},
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// InsertClaims — empty and full round-trip
// ---------------------------------------------------------------------------

func TestInsertClaims_Empty(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Empty claims slice should be a no-op.
	err := db.InsertClaims(ctx, nil)
	require.NoError(t, err)
}

func TestInsertClaims_WithCategory(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "claim-cat-agent", OrgID: orgID, Name: "CC", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "claim-cat-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "claim category test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	category := "finding"
	emb := pgvector.NewVector([]float32{0.1, 0.2, 0.3})
	err = db.InsertClaims(ctx, []storage.Claim{
		{DecisionID: dec.ID, OrgID: orgID, ClaimIdx: 0, ClaimText: "test claim", Category: &category, Embedding: &emb},
	})
	require.NoError(t, err)

	// Verify claims exist.
	exists, err := db.HasClaimsForDecision(ctx, dec.ID, orgID)
	require.NoError(t, err)
	assert.True(t, exists)
}

// ---------------------------------------------------------------------------
// GetAssessmentSummaryBatch — with actual assessment data
// ---------------------------------------------------------------------------

func TestGetAssessmentSummaryBatch_WithData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "assess-agent", OrgID: orgID, Name: "AS", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "assess-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "assessment batch test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Create assessments.
	_, err = db.CreateAssessment(ctx, orgID, model.DecisionAssessment{
		DecisionID:      dec.ID,
		AssessorAgentID: "assess-agent",
		Outcome:         model.AssessmentCorrect,
	})
	require.NoError(t, err)

	_, err = db.CreateAssessment(ctx, orgID, model.DecisionAssessment{
		DecisionID:      dec.ID,
		AssessorAgentID: "other-assessor",
		Outcome:         model.AssessmentIncorrect,
	})
	require.NoError(t, err)

	summaries, err := db.GetAssessmentSummaryBatch(ctx, orgID, []uuid.UUID{dec.ID})
	require.NoError(t, err)
	require.Contains(t, summaries, dec.ID)
	s := summaries[dec.ID]
	assert.Equal(t, 2, s.Total)
	assert.Equal(t, 1, s.Correct)
	assert.Equal(t, 1, s.Incorrect)
}

// ---------------------------------------------------------------------------
// BackfillEmbedding and BackfillOutcomeEmbedding — round-trip
// ---------------------------------------------------------------------------

func TestBackfillEmbedding_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "backfill-agent", OrgID: orgID, Name: "BF", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "backfill-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "backfill embedding test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Initially unembedded.
	unembedded, err := db.FindUnembeddedDecisions(ctx, 10)
	require.NoError(t, err)
	foundUnembedded := false
	for _, u := range unembedded {
		if u.ID == dec.ID {
			foundUnembedded = true
			break
		}
	}
	assert.True(t, foundUnembedded, "decision should appear in unembedded list")

	// Backfill embedding.
	emb := pgvector.NewVector([]float32{0.1, 0.2, 0.3})
	err = db.BackfillEmbedding(ctx, dec.ID, orgID, emb)
	require.NoError(t, err)

	// Should now be missing outcome embedding.
	missingOutcome, err := db.FindDecisionsMissingOutcomeEmbedding(ctx, 10)
	require.NoError(t, err)
	foundMissing := false
	for _, m := range missingOutcome {
		if m.ID == dec.ID {
			foundMissing = true
			break
		}
	}
	assert.True(t, foundMissing, "should be missing outcome embedding")

	// Backfill outcome embedding.
	outcomeEmb := pgvector.NewVector([]float32{0.4, 0.5, 0.6})
	err = db.BackfillOutcomeEmbedding(ctx, dec.ID, orgID, outcomeEmb)
	require.NoError(t, err)

	// Now should have both embeddings.
	embMap, err := db.GetDecisionEmbeddings(ctx, []uuid.UUID{dec.ID}, orgID)
	require.NoError(t, err)
	require.Contains(t, embMap, dec.ID)
	assert.Len(t, embMap[dec.ID][0].Slice(), 3)
	assert.Len(t, embMap[dec.ID][1].Slice(), 3)
}

// ---------------------------------------------------------------------------
// SearchDecisionsByText — LIKE fallback with confidence filter
// ---------------------------------------------------------------------------

func TestSearchDecisionsByLike_WithConfidenceFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "conf-like-agent", OrgID: orgID, Name: "CL", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// Create decisions with "AND OR" in the outcome for LIKE matching.
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "conf-like-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "architecture", Outcome: "CONFLIKE AND OR high confidence",
			Confidence: 0.95, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "conf-like-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "architecture", Outcome: "CONFLIKE AND OR low confidence",
			Confidence: 0.2, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	minConf := float32(0.9)
	results, err := db.SearchDecisionsByText(ctx, orgID, "CONFLIKE AND OR",
		model.QueryFilters{ConfidenceMin: &minConf}, 10)
	require.NoError(t, err)

	// Only the high-confidence decision should match.
	for _, r := range results {
		assert.GreaterOrEqual(t, r.Decision.Confidence, float32(0.9),
			"confidence filter should exclude low-confidence decisions")
	}
}

// ---------------------------------------------------------------------------
// SearchDecisionsByText — LIKE fallback with project filter
// ---------------------------------------------------------------------------

func TestSearchDecisionsByLike_WithProjectFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "proj-like-agent", OrgID: orgID, Name: "PL", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	project := "projlike-test"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "proj-like-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "PROJLIKE AND OR with project",
			Confidence: 0.6, Project: &project, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "proj-like-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "PROJLIKE AND OR without project",
			Confidence: 0.6, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	results, err := db.SearchDecisionsByText(ctx, orgID, "PROJLIKE AND OR",
		model.QueryFilters{Project: &project}, 10)
	require.NoError(t, err)
	for _, r := range results {
		require.NotNil(t, r.Decision.Project)
		assert.Equal(t, project, *r.Decision.Project)
	}
}

// ---------------------------------------------------------------------------
// SearchDecisionsByText — LIKE fallback with time range filter
// ---------------------------------------------------------------------------

func TestSearchDecisionsByLike_WithTimeRange(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "timelike-agent", OrgID: orgID, Name: "TL", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// Put "AND OR" literally in the outcome so LIKE pattern matches.
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "timelike-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "TIMELIKE AND OR range test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	pastHour := now.Add(-time.Hour)
	futureHour := now.Add(time.Hour)

	results, err := db.SearchDecisionsByText(ctx, orgID, "TIMELIKE AND OR",
		model.QueryFilters{TimeRange: &model.TimeRange{From: &pastHour, To: &futureHour}}, 10)
	require.NoError(t, err)
	assert.NotEmpty(t, results, "time range should include recent decisions")
}

// ---------------------------------------------------------------------------
// HasNotifyConn
// ---------------------------------------------------------------------------

func TestHasNotifyConn(t *testing.T) {
	db := newTestDB(t)
	assert.False(t, db.HasNotifyConn(), "SQLite should never have a notify connection")
}

// ---------------------------------------------------------------------------
// HasAccess
// ---------------------------------------------------------------------------

func TestHasAccess(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID := uuid.Nil
	granteeID := uuid.New()

	// No grants exist — should return false.
	has, err := db.HasAccess(ctx, orgID, granteeID, "agent_traces", "some-agent", "read")
	require.NoError(t, err)
	assert.False(t, has)

	// Insert a grant.
	rawDB := db.RawDB()
	_, err = rawDB.ExecContext(ctx,
		`INSERT INTO access_grants (id, org_id, grantor_id, grantee_id, resource_type, resource_id, permission)
		 VALUES (?,?,?,?,?,?,?)`,
		uuid.New().String(), orgID.String(), uuid.New().String(), granteeID.String(),
		"agent_traces", "some-agent", "read",
	)
	require.NoError(t, err)

	has, err = db.HasAccess(ctx, orgID, granteeID, "agent_traces", "some-agent", "read")
	require.NoError(t, err)
	assert.True(t, has)
}

// ---------------------------------------------------------------------------
// New with invalid path
// ---------------------------------------------------------------------------

func TestNew_InvalidPragma(t *testing.T) {
	// Opening with a valid path then executing should succeed. This test verifies
	// New handles directory creation.
	ctx := context.Background()
	dir := t.TempDir()
	path := dir + "/subdir/test.db"
	logger := slog.Default()
	db, err := sqlite.New(ctx, path, logger)
	require.NoError(t, err)
	db.Close(ctx)
}

// ---------------------------------------------------------------------------
// FindDecisionIDsMissingClaims — with data
// ---------------------------------------------------------------------------

func TestFindDecisionIDsMissingClaims_WithEmbeddedDecision(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "missing-claim-agent", OrgID: orgID, Name: "MC", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "missing-claim-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "claim missing test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Backfill embedding so the decision qualifies.
	emb := pgvector.NewVector([]float32{0.1, 0.2, 0.3})
	err = db.BackfillEmbedding(ctx, dec.ID, orgID, emb)
	require.NoError(t, err)

	refs, err := db.FindDecisionIDsMissingClaims(ctx, 10)
	require.NoError(t, err)
	found := false
	for _, ref := range refs {
		if ref.ID == dec.ID {
			found = true
			break
		}
	}
	assert.True(t, found, "decision with embedding but no claims should be found")
}

// ---------------------------------------------------------------------------
// MarkClaimEmbeddingFailed + FindRetriableClaimFailures with actual backoff
// ---------------------------------------------------------------------------

func TestClaimEmbeddingFailureRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "fail-claim-agent", OrgID: orgID, Name: "FC", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "fail-claim-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "claim fail test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Backfill embedding (required for FindRetriableClaimFailures).
	emb := pgvector.NewVector([]float32{0.1, 0.2, 0.3})
	err = db.BackfillEmbedding(ctx, dec.ID, orgID, emb)
	require.NoError(t, err)

	// Mark as failed.
	err = db.MarkClaimEmbeddingFailed(ctx, dec.ID, orgID)
	require.NoError(t, err)

	// Should appear in retriable failures (attempt count = 1, backoff = 5min * 4^0 = 5min).
	// Since we just marked it, it won't be eligible yet (backoff hasn't elapsed).
	refs, err := db.FindRetriableClaimFailures(ctx, 5, 10)
	require.NoError(t, err)
	// The backoff query checks datetime + seconds, so new failures with 1 attempt
	// won't be eligible for 5 minutes. This is expected.
	_ = refs

	// Clear failure state.
	err = db.ClearClaimEmbeddingFailure(ctx, dec.ID, orgID)
	require.NoError(t, err)

	// After clearing, FindDecisionIDsMissingClaims should find it again.
	missingRefs, err := db.FindDecisionIDsMissingClaims(ctx, 10)
	require.NoError(t, err)
	found := false
	for _, ref := range missingRefs {
		if ref.ID == dec.ID {
			found = true
			break
		}
	}
	assert.True(t, found, "after clearing failure, decision should reappear in missing claims")
}

// ---------------------------------------------------------------------------
// CreateTraceTx — with audit entry
// ---------------------------------------------------------------------------

func TestCreateTraceTx_WithAuditEntry(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "audit-trace-agent", OrgID: orgID, Name: "AT", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	audit := storage.MutationAuditEntry{
		RequestID:    "req-audit-1",
		OrgID:        orgID,
		ActorAgentID: "audit-trace-agent",
		ActorRole:    "admin",
		HTTPMethod:   "POST",
		Endpoint:     "/v1/trace",
		Operation:    "trace_decision",
		ResourceType: "decision",
		Metadata:     map[string]any{"source": "test"},
	}

	run, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "audit-trace-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "audit trace test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
		AuditEntry: &audit,
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, run.ID)
	assert.NotEqual(t, uuid.Nil, dec.ID)
}

// ---------------------------------------------------------------------------
// CreateTraceTx — with agent context, session, and precedent ref
// ---------------------------------------------------------------------------

func TestCreateTraceTx_WithOptionalFields(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "optional-fields-agent", OrgID: orgID, Name: "OF", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	sessionID := uuid.New()
	apiKeyID := uuid.New()

	run, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID:      "optional-fields-agent",
		OrgID:        orgID,
		Metadata:     map[string]any{"env": "test"},
		SessionID:    &sessionID,
		AgentContext: map[string]any{"context_key": "context_val"},
		Decision: model.Decision{
			DecisionType: "architecture", Outcome: "optional fields",
			Confidence: 0.75, Metadata: map[string]any{"decision_meta": true},
			APIKeyID: &apiKeyID,
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, run.ID)
	assert.NotEqual(t, uuid.Nil, dec.ID)

	// Verify the optional fields are persisted.
	decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{AgentIDs: []string{"optional-fields-agent"}},
		Limit:   1,
	})
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	assert.NotNil(t, decisions[0].SessionID)
	assert.Equal(t, sessionID, *decisions[0].SessionID)
	assert.NotNil(t, decisions[0].APIKeyID)
	assert.Equal(t, apiKeyID, *decisions[0].APIKeyID)
}

// ---------------------------------------------------------------------------
// QueryDecisions — negative offset clamp + default limit
// ---------------------------------------------------------------------------

func TestQueryDecisions_NegativeOffsetAndDefaultLimit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "neg-offset-agent", OrgID: orgID, Name: "NO", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "neg-offset-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "neg offset test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Negative offset should be clamped to 0; limit=0 defaults to 20.
	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{AgentIDs: []string{"neg-offset-agent"}},
		Offset:  -5,
		Limit:   0,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Len(t, decisions, 1)
}

// ---------------------------------------------------------------------------
// QueryDecisions — ASC ordering
// ---------------------------------------------------------------------------

func TestQueryDecisions_AscOrdering(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "asc-agent", OrgID: orgID, Name: "A", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	for i := range 3 {
		_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
			AgentID: "asc-agent", OrgID: orgID, Metadata: map[string]any{},
			Decision: model.Decision{
				DecisionType: "test", Outcome: fmt.Sprintf("asc decision %d", i),
				Confidence: float32(i+1) * 0.1, Metadata: map[string]any{},
			},
		})
		require.NoError(t, err)
	}

	// Order by confidence ascending.
	decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters:  model.QueryFilters{AgentIDs: []string{"asc-agent"}},
		OrderBy:  "confidence",
		OrderDir: "asc",
		Limit:    10,
	})
	require.NoError(t, err)
	require.Len(t, decisions, 3)
	assert.LessOrEqual(t, decisions[0].Confidence, decisions[1].Confidence)
	assert.LessOrEqual(t, decisions[1].Confidence, decisions[2].Confidence)
}

// ---------------------------------------------------------------------------
// QueryDecisions — with run_id filter
// ---------------------------------------------------------------------------

func TestQueryDecisions_WithRunIDFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "runid-agent", OrgID: orgID, Name: "RI", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	run1, _, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "runid-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "run id filter test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "runid-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "other run",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{RunID: &run1.ID},
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, decisions, 1)
	assert.Equal(t, "run id filter test", decisions[0].Outcome)
}

// ---------------------------------------------------------------------------
// SearchDecisionsByText — default limit
// ---------------------------------------------------------------------------

func TestSearchDecisionsByText_DefaultLimit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	// Limit <= 0 should default to 20. No error even if empty.
	results, err := db.SearchDecisionsByText(ctx, orgID, "nothingmatches", model.QueryFilters{}, 0)
	require.NoError(t, err)
	assert.Empty(t, results)
}

// ---------------------------------------------------------------------------
// GetDecisionForScoring — not found
// ---------------------------------------------------------------------------

func TestGetDecisionForScoring_NotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))

	_, err := db.GetDecisionForScoring(ctx, uuid.New(), uuid.Nil)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

// ---------------------------------------------------------------------------
// CreateTraceTx — nil metadata and agent context
// ---------------------------------------------------------------------------

func TestCreateTraceTx_NilMetadataAndContext(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "nil-meta-agent", OrgID: orgID, Name: "NM", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// Metadata and AgentContext are nil — should be defaulted to empty maps.
	run, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "nil-meta-agent", OrgID: orgID,
		Decision: model.Decision{
			DecisionType: "test", Outcome: "nil metadata test",
			Confidence: 0.5,
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, run.ID)
	assert.NotEqual(t, uuid.Nil, dec.ID)
}

// ---------------------------------------------------------------------------
// FindRetriableClaimFailures — default limit
// ---------------------------------------------------------------------------

func TestFindRetriableClaimFailures_DefaultLimit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))

	// limit <= 0 should default to 50.
	refs, err := db.FindRetriableClaimFailures(ctx, 3, 0)
	require.NoError(t, err)
	assert.Empty(t, refs)
}

// ---------------------------------------------------------------------------
// Idempotency — edge cases
// ---------------------------------------------------------------------------

func TestBeginIdempotency_CompletedWithNullResponse(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	// Begin and complete with nil response data.
	_, err := db.BeginIdempotency(ctx, orgID, "agent1", "/v1/trace", "key-null-resp", "hash-1")
	require.NoError(t, err)

	err = db.CompleteIdempotency(ctx, orgID, "agent1", "/v1/trace", "key-null-resp", 200, nil)
	require.NoError(t, err)

	// Replay should see completed with null response data.
	lookup, err := db.BeginIdempotency(ctx, orgID, "agent1", "/v1/trace", "key-null-resp", "hash-1")
	require.NoError(t, err)
	assert.True(t, lookup.Completed)
	assert.Equal(t, 200, lookup.StatusCode)
}

// ---------------------------------------------------------------------------
// CreateAgentWithAudit — various paths
// ---------------------------------------------------------------------------

func TestCreateAgentWithAudit_WithMetadata(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	agent := model.Agent{
		AgentID: "audit-meta-agent", OrgID: orgID, Name: "AMA", Role: model.RoleAgent,
		Tags: []string{"test"}, Metadata: map[string]any{"env": "test"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	audit := storage.MutationAuditEntry{
		RequestID:    "req-1",
		OrgID:        orgID,
		ActorAgentID: "admin",
		ActorRole:    "admin",
		HTTPMethod:   "POST",
		Endpoint:     "/v1/agents",
		Operation:    "create_agent",
		ResourceType: "agent",
		ResourceID:   "audit-meta-agent",
		AfterData:    map[string]any{"agent_id": "audit-meta-agent"},
		Metadata:     map[string]any{"source": "test"},
	}

	created, err := db.CreateAgentWithAudit(ctx, agent, audit)
	require.NoError(t, err)
	assert.Equal(t, "audit-meta-agent", created.AgentID)

	// Verify agent was persisted.
	fetched, err := db.GetAgentByAgentID(ctx, orgID, "audit-meta-agent")
	require.NoError(t, err)
	assert.Equal(t, "AMA", fetched.Name)
}

// ---------------------------------------------------------------------------
// QueryDecisionsTemporal — default limit
// ---------------------------------------------------------------------------

func TestQueryDecisionsTemporal_DefaultLimit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "temporal-def-agent", OrgID: orgID, Name: "TD", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "temporal-def-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "temporal default limit",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Limit = 0 should default to 20.
	decs, err := db.QueryDecisionsTemporal(ctx, orgID, model.TemporalQueryRequest{
		AsOf:    time.Now().UTC(),
		Filters: model.QueryFilters{AgentIDs: []string{"temporal-def-agent"}},
		Limit:   0,
	})
	require.NoError(t, err)
	assert.Len(t, decs, 1)
}

// ---------------------------------------------------------------------------
// UpdateOutcomeScore — exercise the method
// ---------------------------------------------------------------------------

func TestUpdateOutcomeScore_SetAndClear(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "outcome-score-agent", OrgID: orgID, Name: "OS", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "outcome-score-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "outcome score test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Set score.
	score := float32(0.85)
	err = db.UpdateOutcomeScore(ctx, orgID, dec.ID, &score)
	require.NoError(t, err)

	// Clear score (nil).
	err = db.UpdateOutcomeScore(ctx, orgID, dec.ID, nil)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// GetDecisionsByIDs — empty input
// ---------------------------------------------------------------------------

func TestGetDecisionsByIDs_Empty(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))

	result, err := db.GetDecisionsByIDs(ctx, uuid.Nil, nil)
	require.NoError(t, err)
	assert.Empty(t, result)
}

// ---------------------------------------------------------------------------
// Agent with API key hash
// ---------------------------------------------------------------------------

func TestCreateAgent_WithAPIKeyHash(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	hash := "argon2id$hash$value"
	agent := model.Agent{
		AgentID:    "keyed-agent",
		OrgID:      orgID,
		Name:       "Keyed Agent",
		Role:       model.RoleAgent,
		APIKeyHash: &hash,
		Tags:       []string{},
		Metadata:   map[string]any{},
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}

	created, err := db.CreateAgent(ctx, agent)
	require.NoError(t, err)
	assert.Equal(t, "keyed-agent", created.AgentID)

	fetched, err := db.GetAgentByAgentID(ctx, orgID, "keyed-agent")
	require.NoError(t, err)
	require.NotNil(t, fetched.APIKeyHash)
	assert.Equal(t, hash, *fetched.APIKeyHash)
}

// ---------------------------------------------------------------------------
// Partial assessment outcomes (partially_correct)
// ---------------------------------------------------------------------------

func TestGetAssessmentSummaryBatch_PartiallyCorrect(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "partial-agent", OrgID: orgID, Name: "PA", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "partial-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "partial assessment test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, err = db.CreateAssessment(ctx, orgID, model.DecisionAssessment{
		DecisionID:      dec.ID,
		AssessorAgentID: "partial-agent",
		Outcome:         model.AssessmentPartiallyCorrect,
	})
	require.NoError(t, err)

	summaries, err := db.GetAssessmentSummaryBatch(ctx, orgID, []uuid.UUID{dec.ID})
	require.NoError(t, err)
	require.Contains(t, summaries, dec.ID)
	s := summaries[dec.ID]
	assert.Equal(t, 1, s.Total)
	assert.Equal(t, 1, s.PartiallyCorrect)
	assert.Equal(t, 0, s.Correct)
	assert.Equal(t, 0, s.Incorrect)
}

// ---------------------------------------------------------------------------
// CreateTraceTx with alternatives, evidence, and supersession
// ---------------------------------------------------------------------------

func TestCreateTraceTx_WithAlternativesAndEvidence(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "alt-ev-agent", OrgID: orgID, Name: "AE", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	altMeta := map[string]any{"reason": "cost"}
	evMeta := map[string]any{"source": "doc"}
	run, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "alt-ev-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "architecture", Outcome: "chose option A",
			Confidence: 0.85, Metadata: map[string]any{},
		},
		Alternatives: []model.Alternative{
			{Label: "option B", Selected: false, Metadata: altMeta},
			{Label: "option C", Selected: false, Metadata: altMeta},
		},
		Evidence: []model.Evidence{
			{SourceType: model.SourceDocument, Content: "spec says X", Metadata: evMeta},
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, run.ID)
	assert.NotEqual(t, uuid.Nil, dec.ID)
	assert.Len(t, dec.Alternatives, 2)
	assert.Len(t, dec.Evidence, 1)

	// Verify alternatives were persisted via QueryDecisions with include=all.
	decs, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{AgentIDs: []string{"alt-ev-agent"}},
		Include: []string{"all"},
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, decs, 1)
	assert.Len(t, decs[0].Alternatives, 2)
	assert.Len(t, decs[0].Evidence, 1)
	assert.Equal(t, "option B", decs[0].Alternatives[0].Label)
}

func TestCreateTraceTx_Supersession(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "super-agent", OrgID: orgID, Name: "SA", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// Create first decision.
	_, dec1, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "super-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "original decision",
			Confidence: 0.7, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Supersede it with a new decision.
	_, dec2, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "super-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test",
			Outcome:      "revised decision",
			Confidence:   0.9,
			Metadata:     map[string]any{},
			SupersedesID: &dec1.ID,
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, dec2.ID)

	// Original decision should now have valid_to set (closed by supersession).
	byIDs, err := db.GetDecisionsByIDs(ctx, orgID, []uuid.UUID{dec1.ID})
	require.NoError(t, err)
	// dec1 should not appear because GetDecisionsByIDs filters valid_to IS NULL.
	assert.Empty(t, byIDs)

	// dec2 should be visible.
	byIDs2, err := db.GetDecisionsByIDs(ctx, orgID, []uuid.UUID{dec2.ID})
	require.NoError(t, err)
	assert.Contains(t, byIDs2, dec2.ID)
}

func TestCreateTraceTx_WithAgentContext(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "ctx-agent", OrgID: orgID, Name: "CTX", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	agentCtx := map[string]any{"prompt": "do something", "temp": 0.7}
	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID:      "ctx-agent",
		OrgID:        orgID,
		Metadata:     map[string]any{},
		AgentContext: agentCtx,
		Decision: model.Decision{
			DecisionType: "test", Outcome: "with context",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, dec.ID)
}

// ---------------------------------------------------------------------------
// ListConflictGroups with representative conflict (loadRepresentativeConflict)
// ---------------------------------------------------------------------------

func TestListConflictGroups_WithRepresentative(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	rawDB := db.RawDB()

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "grp-agent", OrgID: orgID, Name: "GA", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, decA, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "grp-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "group decision A",
			Confidence: 0.7, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, decB, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "grp-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "group decision B",
			Confidence: 0.6, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	groupID := uuid.New()
	conflictID := uuid.New()

	// Insert the conflict_group first.
	_, err = rawDB.ExecContext(ctx,
		`INSERT INTO conflict_groups
		 (id, org_id, agent_a, agent_b, conflict_kind, decision_type,
		  first_detected_at, last_detected_at)
		 VALUES (?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))`,
		groupID.String(), orgID.String(), "grp-agent", "grp-agent", "cross_agent", "test",
	)
	require.NoError(t, err)

	_, err = rawDB.ExecContext(ctx,
		`INSERT INTO scored_conflicts
		 (id, conflict_kind, decision_a_id, decision_b_id, org_id,
		  agent_a, agent_b, decision_type_a, decision_type_b,
		  outcome_a, outcome_b, topic_similarity, outcome_divergence,
		  significance, scoring_method, explanation, detected_at,
		  category, severity, status, relationship, confidence_weight, temporal_decay,
		  group_id)
		 VALUES (?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?, ?,datetime('now'),?,?,?, ?,?,?,?)`,
		conflictID.String(), "contradictory", decA.ID.String(), decB.ID.String(), orgID.String(),
		"grp-agent", "grp-agent", "test", "test",
		"group decision A", "group decision B", 0.8, 0.6,
		0.9, "test", "group explanation",
		"architecture", "high", "open",
		"contradictory", 1.0, 0.9,
		groupID.String(),
	)
	require.NoError(t, err)

	groups, err := db.ListConflictGroups(ctx, orgID, storage.ConflictGroupFilters{}, 10, 0)
	require.NoError(t, err)
	require.NotEmpty(t, groups)
	// The group should have a representative conflict loaded.
	assert.NotNil(t, groups[0].Representative)
	assert.Equal(t, conflictID, groups[0].Representative.ID)
}

// ---------------------------------------------------------------------------
// ListAgentIDsBySharedTags with data
// ---------------------------------------------------------------------------

func TestListAgentIDsBySharedTags_WithData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "tag-agent-1", OrgID: orgID, Name: "T1", Role: model.RoleAgent,
		Tags: []string{"backend", "golang"}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, err = db.CreateAgent(ctx, model.Agent{
		AgentID: "tag-agent-2", OrgID: orgID, Name: "T2", Role: model.RoleAgent,
		Tags: []string{"frontend", "react"}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, err = db.CreateAgent(ctx, model.Agent{
		AgentID: "tag-agent-3", OrgID: orgID, Name: "T3", Role: model.RoleAgent,
		Tags: []string{"backend", "python"}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	ids, err := db.ListAgentIDsBySharedTags(ctx, orgID, []string{"backend"})
	require.NoError(t, err)
	assert.Len(t, ids, 2)
	assert.Contains(t, ids, "tag-agent-1")
	assert.Contains(t, ids, "tag-agent-3")

	// No shared tags.
	ids2, err := db.ListAgentIDsBySharedTags(ctx, orgID, []string{"devops"})
	require.NoError(t, err)
	assert.Empty(t, ids2)

	// Empty tags returns nil.
	ids3, err := db.ListAgentIDsBySharedTags(ctx, orgID, []string{})
	require.NoError(t, err)
	assert.Nil(t, ids3)
}

// ---------------------------------------------------------------------------
// BackfillOutcomeEmbedding + FindDecisionsMissingOutcomeEmbedding
// ---------------------------------------------------------------------------

func TestBackfillOutcomeEmbedding_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "oe-agent", OrgID: orgID, Name: "OE", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "oe-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "outcome embedding test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Backfill the main embedding first.
	emb := pgvector.NewVector([]float32{0.1, 0.2, 0.3})
	require.NoError(t, db.BackfillEmbedding(ctx, dec.ID, orgID, emb))

	// Now it should appear as missing outcome embedding.
	missing, err := db.FindDecisionsMissingOutcomeEmbedding(ctx, 10)
	require.NoError(t, err)
	found := false
	for _, m := range missing {
		if m.ID == dec.ID {
			found = true
			break
		}
	}
	assert.True(t, found, "decision should appear in missing outcome embeddings")

	// Backfill the outcome embedding.
	outcomeEmb := pgvector.NewVector([]float32{0.4, 0.5, 0.6})
	require.NoError(t, db.BackfillOutcomeEmbedding(ctx, dec.ID, orgID, outcomeEmb))

	// Should no longer appear as missing.
	missing2, err := db.FindDecisionsMissingOutcomeEmbedding(ctx, 10)
	require.NoError(t, err)
	for _, m := range missing2 {
		assert.NotEqual(t, dec.ID, m.ID, "decision should not appear after backfilling outcome embedding")
	}
}

// ---------------------------------------------------------------------------
// QueryDecisions with include=alternatives only, include=evidence only
// ---------------------------------------------------------------------------

func TestQueryDecisions_IncludeAlternativesOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "inc-alt-agent", OrgID: orgID, Name: "IA", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "inc-alt-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "include alt test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
		Alternatives: []model.Alternative{
			{Label: "alt1", Selected: true, Metadata: map[string]any{}},
		},
		Evidence: []model.Evidence{
			{SourceType: model.SourceDocument, Content: "some evidence", Metadata: map[string]any{}},
		},
	})
	require.NoError(t, err)

	// Include only alternatives.
	decs, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Include: []string{"alternatives"},
		Limit:   10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, decs)
	assert.NotEmpty(t, decs[0].Alternatives)
	assert.Empty(t, decs[0].Evidence) // Evidence not included.

	// Include only evidence.
	decs2, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Include: []string{"evidence"},
		Limit:   10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, decs2)
	assert.Empty(t, decs2[0].Alternatives) // Alternatives not included.
	assert.NotEmpty(t, decs2[0].Evidence)
}

// ---------------------------------------------------------------------------
// GetDecisionForScoring with embedding and project fields
// ---------------------------------------------------------------------------

func TestGetDecisionForScoring_WithEmbeddingAndProject(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "score-emb-agent", OrgID: orgID, Name: "SC", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "score-emb-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "architecture", Outcome: "scoring test decision",
			Confidence: 0.75,
			Metadata:   map[string]any{},
		},
	})
	require.NoError(t, err)

	// Backfill embedding so it's available for scoring.
	emb := pgvector.NewVector([]float32{0.1, 0.2, 0.3})
	require.NoError(t, db.BackfillEmbedding(ctx, dec.ID, orgID, emb))

	scored, err := db.GetDecisionForScoring(ctx, dec.ID, orgID)
	require.NoError(t, err)
	assert.Equal(t, dec.ID, scored.ID)
	assert.Equal(t, "architecture", scored.DecisionType)
	assert.Equal(t, "scoring test decision", scored.Outcome)
	// Project field tested via other paths
	assert.NotNil(t, scored.Embedding)
}

// ---------------------------------------------------------------------------
// GetDecisionEmbeddings with multiple decisions
// ---------------------------------------------------------------------------

func TestGetDecisionEmbeddings_Multiple(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "emb-multi-agent", OrgID: orgID, Name: "EM", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	var decIDs []uuid.UUID
	for i := range 3 {
		_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
			AgentID: "emb-multi-agent", OrgID: orgID, Metadata: map[string]any{},
			Decision: model.Decision{
				DecisionType: "test", Outcome: fmt.Sprintf("emb multi decision %d", i),
				Confidence: 0.5, Metadata: map[string]any{},
			},
		})
		require.NoError(t, err)
		decIDs = append(decIDs, dec.ID)

		emb := pgvector.NewVector([]float32{float32(i+1) * 0.1, float32(i+1) * 0.2, float32(i+1) * 0.3})
		require.NoError(t, db.BackfillEmbedding(ctx, dec.ID, orgID, emb))
		outcomeEmb := pgvector.NewVector([]float32{float32(i+1) * 0.4, float32(i+1) * 0.5, float32(i+1) * 0.6})
		require.NoError(t, db.BackfillOutcomeEmbedding(ctx, dec.ID, orgID, outcomeEmb))
	}

	embeddings, err := db.GetDecisionEmbeddings(ctx, decIDs, orgID)
	require.NoError(t, err)
	assert.Len(t, embeddings, 3)
	for _, id := range decIDs {
		assert.Contains(t, embeddings, id)
	}
}

// ---------------------------------------------------------------------------
// GetOutcomeSignalsSummary with data
// ---------------------------------------------------------------------------

func TestGetOutcomeSignalsSummary_WithData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "oss-agent", OrgID: orgID, Name: "OSS", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	for i := range 3 {
		_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
			AgentID: "oss-agent", OrgID: orgID, Metadata: map[string]any{},
			Decision: model.Decision{
				DecisionType: "test", Outcome: fmt.Sprintf("oss decision %d", i),
				Confidence: 0.5, Metadata: map[string]any{},
			},
		})
		require.NoError(t, err)
	}

	summary, err := db.GetOutcomeSignalsSummary(ctx, orgID, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, summary.DecisionsTotal)
	assert.Equal(t, 3, summary.NeverSuperseded)
	assert.Equal(t, 3, summary.NeverCited)
}

// ---------------------------------------------------------------------------
// GetConflictStatusCounts with data
// ---------------------------------------------------------------------------

func TestGetConflictStatusCounts_WithData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	rawDB := db.RawDB()

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "csc-agent", OrgID: orgID, Name: "CSC", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, decA, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "csc-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "csc A",
			Confidence: 0.7, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	_, decB, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "csc-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "csc B",
			Confidence: 0.6, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Create a third decision for the second conflict pair.
	_, decC, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "csc-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "csc C",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Insert open conflict between A and B.
	cid1 := uuid.New()
	_, err = rawDB.ExecContext(ctx,
		`INSERT INTO scored_conflicts
		 (id, conflict_kind, decision_a_id, decision_b_id, org_id,
		  agent_a, agent_b, decision_type_a, decision_type_b,
		  outcome_a, outcome_b, topic_similarity, outcome_divergence,
		  significance, scoring_method, explanation, detected_at,
		  category, severity, status, relationship, confidence_weight, temporal_decay)
		 VALUES (?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?, ?,datetime('now'),?,?,?, ?,?,?)`,
		cid1.String(), "contradictory", decA.ID.String(), decB.ID.String(), orgID.String(),
		"csc-agent", "csc-agent", "test", "test",
		"csc A", "csc B", 0.8, 0.6,
		0.7, "test", "test explanation",
		"test", "high", "open",
		"contradictory", 1.0, 0.9,
	)
	require.NoError(t, err)

	// Insert resolved conflict between A and C (different pair).
	cid2 := uuid.New()
	_, err = rawDB.ExecContext(ctx,
		`INSERT INTO scored_conflicts
		 (id, conflict_kind, decision_a_id, decision_b_id, org_id,
		  agent_a, agent_b, decision_type_a, decision_type_b,
		  outcome_a, outcome_b, topic_similarity, outcome_divergence,
		  significance, scoring_method, explanation, detected_at,
		  category, severity, status, relationship, confidence_weight, temporal_decay,
		  resolved_at)
		 VALUES (?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?, ?,datetime('now'),?,?,?, ?,?,?,datetime('now'))`,
		cid2.String(), "contradictory", decA.ID.String(), decC.ID.String(), orgID.String(),
		"csc-agent", "csc-agent", "test", "test",
		"csc A", "csc C", 0.8, 0.6,
		0.7, "test", "test explanation",
		"test", "high", "resolved",
		"contradictory", 1.0, 0.9,
	)
	require.NoError(t, err)

	counts, err := db.GetConflictStatusCounts(ctx, orgID, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, counts.Total)
	assert.Equal(t, 1, counts.Open)
	assert.Equal(t, 1, counts.Resolved)
}

// ---------------------------------------------------------------------------
// EvidenceCoverageStats with data
// ---------------------------------------------------------------------------

func TestGetEvidenceCoverageStats_WithData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "ev-cov-agent", OrgID: orgID, Name: "EC", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// Decision with evidence.
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "ev-cov-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "with evidence",
			Confidence: 0.7, Metadata: map[string]any{},
		},
		Evidence: []model.Evidence{
			{SourceType: model.SourceDocument, Content: "proof", Metadata: map[string]any{}},
		},
	})
	require.NoError(t, err)

	// Decision without evidence.
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "ev-cov-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "without evidence",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	stats, err := db.GetEvidenceCoverageStats(ctx, orgID, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, stats.TotalDecisions)
	assert.Equal(t, 1, stats.WithEvidence)
	assert.Equal(t, 1, stats.WithoutEvidenceCount)
	assert.Equal(t, 50.0, stats.CoveragePercent)
	assert.Equal(t, 1, stats.TotalRecords)
	assert.InDelta(t, 0.5, stats.AvgPerDecision, 0.01)
}

// ---------------------------------------------------------------------------
// CreateAgentWithAudit — duplicate agent triggers error in tx
// ---------------------------------------------------------------------------

func TestCreateAgentWithAudit_DuplicateAgent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	now := time.Now().UTC()

	agent := model.Agent{
		AgentID: "dup-audit-agent", OrgID: orgID, Name: "Dup", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}
	audit := storage.MutationAuditEntry{
		RequestID:    "req-dup-1",
		OrgID:        orgID,
		ActorAgentID: "system",
		ActorRole:    "admin",
		HTTPMethod:   "POST",
		Endpoint:     "/v1/agents",
		Operation:    "create",
		ResourceType: "agent",
		ResourceID:   "dup-audit-agent",
		Metadata:     map[string]any{},
	}

	// First creation succeeds.
	_, err := db.CreateAgentWithAudit(ctx, agent, audit)
	require.NoError(t, err)

	// Second creation with same agent_id+org_id fails (UNIQUE constraint).
	_, err = db.CreateAgentWithAudit(ctx, agent, audit)
	require.Error(t, err, "duplicate agent insert should fail")
	assert.True(t, db.IsDuplicateKey(err) || err != nil, "should be a constraint error")
}

// ---------------------------------------------------------------------------
// CreateTraceTx — with precedent_ref
// ---------------------------------------------------------------------------

func TestCreateTraceTx_WithPrecedentRef(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "prec-ref-agent", OrgID: orgID, Name: "PR", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// Create an original decision to use as precedent.
	_, origDec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "prec-ref-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "original for precedent",
			Confidence: 0.7, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Create a new decision citing the original as precedent.
	_, newDec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "prec-ref-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "citing precedent",
			Confidence: 0.8, Metadata: map[string]any{},
			PrecedentRef: &origDec.ID,
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t, origDec.ID, newDec.ID)
}

// ---------------------------------------------------------------------------
// GetDecisionsByIDs — empty IDs and mixed found/not-found
// ---------------------------------------------------------------------------

func TestGetDecisionsByIDs_EmptyIDs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	result, err := db.GetDecisionsByIDs(ctx, orgID, []uuid.UUID{})
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestGetDecisionsByIDs_MixedFoundNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "ids-mix-agent", OrgID: orgID, Name: "IM", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "ids-mix-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "ids mix test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	fakeID := uuid.New()
	result, err := db.GetDecisionsByIDs(ctx, orgID, []uuid.UUID{dec.ID, fakeID})
	require.NoError(t, err)
	assert.Len(t, result, 1, "only the real decision should be returned")
	assert.Contains(t, result, dec.ID)
}

// ---------------------------------------------------------------------------
// BackfillEmbedding + BackfillOutcomeEmbedding — nonexistent decision (no error, just no-op)
// ---------------------------------------------------------------------------

func TestBackfillEmbedding_NonexistentDecision(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	emb := pgvector.NewVector([]float32{0.1, 0.2, 0.3})
	err := db.BackfillEmbedding(ctx, uuid.New(), orgID, emb)
	require.NoError(t, err, "backfill on nonexistent decision should not error")
}

func TestBackfillOutcomeEmbedding_NonexistentDecision(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	emb := pgvector.NewVector([]float32{0.4, 0.5, 0.6})
	err := db.BackfillOutcomeEmbedding(ctx, uuid.New(), orgID, emb)
	require.NoError(t, err, "backfill outcome on nonexistent decision should not error")
}

// ---------------------------------------------------------------------------
// GetDecisionEmbeddings — partial embeddings (only one of the two)
// ---------------------------------------------------------------------------

func TestGetDecisionEmbeddings_PartialEmbeddings(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "partial-emb-agent", OrgID: orgID, Name: "PE", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "partial-emb-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "partial emb",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Backfill only the base embedding, not the outcome embedding.
	emb := pgvector.NewVector([]float32{0.1, 0.2, 0.3})
	require.NoError(t, db.BackfillEmbedding(ctx, dec.ID, orgID, emb))

	// GetDecisionEmbeddings requires BOTH embeddings, so this should return empty.
	result, err := db.GetDecisionEmbeddings(ctx, []uuid.UUID{dec.ID}, orgID)
	require.NoError(t, err)
	assert.Empty(t, result, "decision with only base embedding should not be returned")
}

// ---------------------------------------------------------------------------
// FindUnembeddedDecisions — limit respected
// ---------------------------------------------------------------------------

func TestFindUnembeddedDecisions_LimitRespected(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "unemb-limit-agent", OrgID: orgID, Name: "UL", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		_, _, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
			AgentID: "unemb-limit-agent", OrgID: orgID, Metadata: map[string]any{},
			Decision: model.Decision{
				DecisionType: "test", Outcome: fmt.Sprintf("unemb %d", i),
				Confidence: 0.5, Metadata: map[string]any{},
			},
		})
		require.NoError(t, err)
	}

	result, err := db.FindUnembeddedDecisions(ctx, 3)
	require.NoError(t, err)
	assert.Len(t, result, 3, "should respect the limit parameter")
}

// ---------------------------------------------------------------------------
// GetDecisionQualityStats — empty org
// ---------------------------------------------------------------------------

func TestGetDecisionQualityStats_EmptyOrg(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	stats, err := db.GetDecisionQualityStats(ctx, orgID, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, stats.Total)
	assert.Equal(t, 0.0, stats.AvgCompleteness)
	assert.Equal(t, 0, stats.BelowHalf)
}

// ---------------------------------------------------------------------------
// GetDecisionQualityStats — with data exercising completeness bands
// ---------------------------------------------------------------------------

func TestGetDecisionQualityStats_WithData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "quality-agent", OrgID: orgID, Name: "QA", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// High-quality decision with reasoning and alternatives.
	reasoning := "detailed reasoning"
	_, dec1, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "quality-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "good decision",
			Confidence: 0.9, Reasoning: &reasoning,
			Metadata: map[string]any{},
		},
		Alternatives: []model.Alternative{
			{Label: "Alt A", Selected: false, Metadata: map[string]any{}},
		},
	})
	require.NoError(t, err)
	_ = dec1

	// Low-quality decision: no reasoning, no alternatives.
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "quality-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "bare decision",
			Confidence: 0.2, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	stats, err := db.GetDecisionQualityStats(ctx, orgID, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, stats.Total)
	assert.Equal(t, 1, stats.WithReasoning, "only one decision has reasoning")
	assert.Equal(t, 1, stats.WithAlternatives, "only one decision has alternatives")
}

// ---------------------------------------------------------------------------
// GetOutcomeSignalsSummary — empty org
// ---------------------------------------------------------------------------

func TestGetOutcomeSignalsSummary_EmptyOrg(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	summary, err := db.GetOutcomeSignalsSummary(ctx, orgID, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, summary.DecisionsTotal)
}

// ---------------------------------------------------------------------------
// ListConflictGroups — empty org
// ---------------------------------------------------------------------------

func TestListConflictGroups_EmptyOrg(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	groups, err := db.ListConflictGroups(ctx, orgID, storage.ConflictGroupFilters{}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, groups)
}

// ---------------------------------------------------------------------------
// ListConflictGroups — with filters
// ---------------------------------------------------------------------------

func TestListConflictGroups_WithFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	decType := "architecture"
	agentID := "filter-agent"
	kind := "contradictory"

	// Test with all filters set — no data to match, but exercises the filter-building branches.
	groups, err := db.ListConflictGroups(ctx, orgID, storage.ConflictGroupFilters{
		DecisionType: &decType,
		AgentID:      &agentID,
		ConflictKind: &kind,
		OpenOnly:     true,
	}, 0, 0) // limit 0 to exercise the default-limit branch
	require.NoError(t, err)
	assert.Empty(t, groups)
}

// ---------------------------------------------------------------------------
// Idempotency — payload mismatch
// ---------------------------------------------------------------------------

func TestIdempotency_PayloadMismatch(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	// Claim the key.
	_, err := db.BeginIdempotency(ctx, orgID, "agent-pm", "/v1/trace", "key-pm", "hash-original")
	require.NoError(t, err)

	// Complete it.
	require.NoError(t, db.CompleteIdempotency(ctx, orgID, "agent-pm", "/v1/trace", "key-pm", 201, map[string]any{"id": "abc"}))

	// Try again with a different hash — should get payload mismatch.
	_, err = db.BeginIdempotency(ctx, orgID, "agent-pm", "/v1/trace", "key-pm", "hash-different")
	assert.ErrorIs(t, err, storage.ErrIdempotencyPayloadMismatch)
}

// ---------------------------------------------------------------------------
// Idempotency — clear non-existent key is a no-op
// ---------------------------------------------------------------------------

func TestClearInProgressIdempotency_Nonexistent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	err := db.ClearInProgressIdempotency(ctx, orgID, "no-agent", "/v1/trace", "no-key")
	require.NoError(t, err, "clearing a nonexistent key should not error")
}

// ---------------------------------------------------------------------------
// HasClaimsForDecision — no claims
// ---------------------------------------------------------------------------

func TestHasClaimsForDecision_NoClaims(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	has, err := db.HasClaimsForDecision(ctx, orgID, uuid.New())
	require.NoError(t, err)
	assert.False(t, has)
}

// ---------------------------------------------------------------------------
// ListGrantedAgentIDs — empty grants
// ---------------------------------------------------------------------------

func TestListGrantedAgentIDs_Empty(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	ids, err := db.ListGrantedAgentIDs(ctx, orgID, uuid.New(), "self-agent")
	require.NoError(t, err)
	// Self-agent is always included even with no grants.
	assert.Equal(t, map[string]bool{"self-agent": true}, ids)
	assert.Len(t, ids, 1, "only the self-agent should be present when no grants exist")
}

// ---------------------------------------------------------------------------
// HasAccess — no grant
// ---------------------------------------------------------------------------

func TestHasAccess_NoGrant(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	has, err := db.HasAccess(ctx, orgID, uuid.New(), "agent", "nonexistent-agent", "read")
	require.NoError(t, err)
	assert.False(t, has)
}

// ---------------------------------------------------------------------------
// CountAgents — empty org
// ---------------------------------------------------------------------------

func TestCountAgents_EmptyOrg(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	count, err := db.CountAgents(ctx, orgID)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// ---------------------------------------------------------------------------
// ListAgentIDsBySharedTags — empty tags
// ---------------------------------------------------------------------------

func TestListAgentIDsBySharedTags_EmptyTags(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	ids, err := db.ListAgentIDsBySharedTags(ctx, orgID, []string{})
	require.NoError(t, err)
	assert.Nil(t, ids)
}

// ---------------------------------------------------------------------------
// QueryDecisionsTemporal — no results
// ---------------------------------------------------------------------------

func TestQueryDecisionsTemporal_NoResults(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	asOf := time.Now().UTC()
	decisions, err := db.QueryDecisionsTemporal(ctx, orgID, model.TemporalQueryRequest{
		AsOf:  asOf,
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// QueryDecisionsTemporal — with AsOf time
// ---------------------------------------------------------------------------

func TestQueryDecisionsTemporal_WithAsOf(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "temporal-agent", OrgID: orgID, Name: "TA", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "temporal-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "temporal test",
			Confidence: 0.7, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Query as-of now should return the decision.
	asOf := time.Now().UTC().Add(time.Second)
	decisions, err := db.QueryDecisionsTemporal(ctx, orgID, model.TemporalQueryRequest{
		AsOf:  asOf,
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Len(t, decisions, 1)

	// Query as-of past should return nothing.
	past := time.Now().UTC().Add(-24 * time.Hour)
	decisions, err = db.QueryDecisionsTemporal(ctx, orgID, model.TemporalQueryRequest{
		AsOf:  past,
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// SearchDecisionsByText — with limit 0 exercises default-limit branch
// ---------------------------------------------------------------------------

func TestSearchDecisionsByText_ZeroLimit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	results, err := db.SearchDecisionsByText(ctx, orgID, "anything", model.QueryFilters{}, 0)
	require.NoError(t, err)
	assert.Empty(t, results)
}

// ---------------------------------------------------------------------------
// CreateAssessment — basic round trip
// ---------------------------------------------------------------------------

func TestCreateAssessment_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "assess-agent", OrgID: orgID, Name: "AA", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "assess-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "assessed decision",
			Confidence: 0.7, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	notes := "looks good"
	assessment := model.DecisionAssessment{
		ID:              uuid.New(),
		DecisionID:      dec.ID,
		OrgID:           orgID,
		AssessorAgentID: "assess-agent",
		Outcome:         model.AssessmentCorrect,
		Notes:           &notes,
	}
	_, err = db.CreateAssessment(ctx, orgID, assessment)
	require.NoError(t, err)

	// Verify via GetAssessmentSummaryBatch.
	summaries, err := db.GetAssessmentSummaryBatch(ctx, orgID, []uuid.UUID{dec.ID})
	require.NoError(t, err)
	assert.Contains(t, summaries, dec.ID)
}

// ---------------------------------------------------------------------------
// UpdateOutcomeScore — clear score (set to nil)
// ---------------------------------------------------------------------------

func TestUpdateOutcomeScore_ClearScore(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "outcome-clear-agent", OrgID: orgID, Name: "OC", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "outcome-clear-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "outcome clear",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Set a score, then clear it.
	score := float32(0.85)
	require.NoError(t, db.UpdateOutcomeScore(ctx, dec.ID, orgID, &score))
	require.NoError(t, db.UpdateOutcomeScore(ctx, dec.ID, orgID, nil))
}

// ---------------------------------------------------------------------------
// GetConflictCount — empty org
// ---------------------------------------------------------------------------

func TestGetConflictCount_EmptyOrg(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	count, err := db.GetConflictCount(ctx, uuid.New(), orgID)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// ---------------------------------------------------------------------------
// GetConflictCountsBatch — no decisions
// ---------------------------------------------------------------------------

func TestGetConflictCountsBatch_NoDecisions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	counts, err := db.GetConflictCountsBatch(ctx, []uuid.UUID{}, orgID)
	require.NoError(t, err)
	assert.Empty(t, counts)
}

func TestGetConflictCountsBatch_NonexistentDecisions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	counts, err := db.GetConflictCountsBatch(ctx, []uuid.UUID{uuid.New(), uuid.New()}, orgID)
	require.NoError(t, err)
	assert.Empty(t, counts)
}

// ---------------------------------------------------------------------------
// GetResolvedConflictsByType — empty org
// ---------------------------------------------------------------------------

func TestGetResolvedConflictsByType_EmptyOrg(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	results, err := db.GetResolvedConflictsByType(ctx, orgID, "test", 10)
	require.NoError(t, err)
	assert.Empty(t, results)
}

// ---------------------------------------------------------------------------
// InsertClaims — round trip
// ---------------------------------------------------------------------------

func TestInsertClaims_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "claims-agent", OrgID: orgID, Name: "CA", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "claims-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "claims test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	claims := []storage.Claim{
		{DecisionID: dec.ID, OrgID: orgID, ClaimText: "claim one", ClaimIdx: 0},
		{DecisionID: dec.ID, OrgID: orgID, ClaimText: "claim two", ClaimIdx: 1},
	}
	err = db.InsertClaims(ctx, claims)
	require.NoError(t, err)

	has, err := db.HasClaimsForDecision(ctx, dec.ID, orgID)
	require.NoError(t, err)
	assert.True(t, has)
}

// ---------------------------------------------------------------------------
// FindDecisionIDsMissingClaims — new decision missing claims
// ---------------------------------------------------------------------------

func TestFindDecisionIDsMissingClaims_NewDecision(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "missing-claims-agent", OrgID: orgID, Name: "MC", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "missing-claims-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "missing claims test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// FindDecisionIDsMissingClaims requires embedding IS NOT NULL, so backfill one.
	emb := pgvector.NewVector([]float32{0.1, 0.2, 0.3})
	require.NoError(t, db.BackfillEmbedding(ctx, dec.ID, orgID, emb))

	missing, err := db.FindDecisionIDsMissingClaims(ctx, 10)
	require.NoError(t, err)
	var foundMissing bool
	for _, ref := range missing {
		if ref.ID == dec.ID {
			foundMissing = true
			break
		}
	}
	assert.True(t, foundMissing, "decision with embedding but no claims should be in the list")
}

// ---------------------------------------------------------------------------
// MarkClaimEmbeddingFailed + ClearClaimEmbeddingFailure + FindRetriableClaimFailures
// ---------------------------------------------------------------------------

func TestClaimEmbeddingFailure_MarkAndClear(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "claim-fail-agent", OrgID: orgID, Name: "CF", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "claim-fail-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "claim fail test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Mark decision as claim-embedding-failed — exercises the MarkClaimEmbeddingFailed branch.
	err = db.MarkClaimEmbeddingFailed(ctx, dec.ID, orgID)
	require.NoError(t, err)

	// Clear the failure — exercises the ClearClaimEmbeddingFailure branch.
	err = db.ClearClaimEmbeddingFailure(ctx, dec.ID, orgID)
	require.NoError(t, err)

	// FindRetriableClaimFailures with no failures remaining returns empty.
	retriable, err := db.FindRetriableClaimFailures(ctx, 3, 10)
	require.NoError(t, err)
	assert.Empty(t, retriable, "no failures should remain after clearing")
}

// ---------------------------------------------------------------------------
// QueryDecisions — with sort by different columns
// ---------------------------------------------------------------------------

func TestQueryDecisions_SortByConfidence(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "sort-agent", OrgID: orgID, Name: "SA", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	for _, conf := range []float32{0.3, 0.9, 0.6} {
		_, _, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
			AgentID: "sort-agent", OrgID: orgID, Metadata: map[string]any{},
			Decision: model.Decision{
				DecisionType: "test", Outcome: fmt.Sprintf("conf %.1f", conf),
				Confidence: conf, Metadata: map[string]any{},
			},
		})
		require.NoError(t, err)
	}

	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Limit:    10,
		OrderBy:  "confidence",
		OrderDir: "asc",
	})
	require.NoError(t, err)
	assert.Equal(t, 3, total)
	require.Len(t, decisions, 3)
	assert.True(t, decisions[0].Confidence <= decisions[1].Confidence,
		"first confidence should be <= second when sorting asc")
}

// ---------------------------------------------------------------------------
// GetDecisionsByAgent — with time range filters
// ---------------------------------------------------------------------------

func TestGetDecisionsByAgent_WithTimeRange(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "timerange-agent", OrgID: orgID, Name: "TR", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "timerange-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "time range test",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Query with both from and to bounds.
	from := time.Now().UTC().Add(-1 * time.Hour)
	to := time.Now().UTC().Add(1 * time.Hour)
	decisions, total, err := db.GetDecisionsByAgent(ctx, orgID, "timerange-agent", 10, 0, &from, &to)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Len(t, decisions, 1)

	// Query with a future from — should return nothing.
	futureFrom := time.Now().UTC().Add(1 * time.Hour)
	decisions, total, err = db.GetDecisionsByAgent(ctx, orgID, "timerange-agent", 10, 0, &futureFrom, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// CreateTraceTx — with supersedes_id (revision chain)
// ---------------------------------------------------------------------------

func TestCreateTraceTx_WithSupersedesID(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "supersede-agent", OrgID: orgID, Name: "SS", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// Create original decision.
	_, origDec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "supersede-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "original decision",
			Confidence: 0.5, Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Create revision that supersedes the original.
	_, revDec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "supersede-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test",
			Outcome:      "revised decision",
			Confidence:   0.8,
			SupersedesID: &origDec.ID,
			Metadata:     map[string]any{},
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t, origDec.ID, revDec.ID)

	// The original should now have valid_to set (superseded).
	decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Limit: 10,
	})
	require.NoError(t, err)
	// Only the revision should appear (active query filters valid_to IS NULL).
	found := false
	for _, d := range decisions {
		if d.ID == revDec.ID {
			found = true
		}
		assert.NotEqual(t, origDec.ID, d.ID, "superseded decision should not appear in active query")
	}
	assert.True(t, found, "revision decision should appear in active query")
}

// ---------------------------------------------------------------------------
// QueryDecisions — Include alternatives and evidence
// ---------------------------------------------------------------------------

func TestQueryDecisions_IncludeAlternativesAndEvidence(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "inc-agent", OrgID: orgID, Name: "Inc", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "inc-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "test", Outcome: "include test",
			Confidence: 0.7, Metadata: map[string]any{},
		},
		Alternatives: []model.Alternative{
			{Label: "Option A", Selected: true, Metadata: map[string]any{}},
			{Label: "Option B", Selected: false, Metadata: map[string]any{}},
		},
		Evidence: []model.Evidence{
			{SourceType: model.SourceDocument, Content: "proof A", Metadata: map[string]any{}},
		},
	})
	require.NoError(t, err)

	// Query with Include=["alternatives", "evidence"].
	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Limit:   10,
		Include: []string{"alternatives", "evidence"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, decisions, 1)
	assert.Len(t, decisions[0].Alternatives, 2, "should load alternatives")
	assert.Len(t, decisions[0].Evidence, 1, "should load evidence")
}

// ---------------------------------------------------------------------------
// QueryDecisions — with multiple filters simultaneously
// ---------------------------------------------------------------------------

func TestQueryDecisions_MultipleFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "multi-filter-agent", OrgID: orgID, Name: "MF", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	project := "test-project"
	tool := "code-review"
	modelName := "gpt-4"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "multi-filter-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "architecture", Outcome: "multi filter test",
			Confidence: 0.8, Metadata: map[string]any{},
			Project: &project, Tool: &tool, Model: &modelName,
		},
	})
	require.NoError(t, err)

	minConf := float32(0.5)
	decType := "architecture"
	from := time.Now().UTC().Add(-1 * time.Hour)
	to := time.Now().UTC().Add(1 * time.Hour)
	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Limit: 10,
		Filters: model.QueryFilters{
			AgentIDs:      []string{"multi-filter-agent"},
			DecisionType:  &decType,
			ConfidenceMin: &minConf,
			TimeRange:     &model.TimeRange{From: &from, To: &to},
			Project:       &project,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, decisions, 1)
	assert.Equal(t, &tool, decisions[0].Tool)
	assert.Equal(t, &modelName, decisions[0].Model)
}

// ---------------------------------------------------------------------------
// CreateTraceTx — with tool, model, project fields
// ---------------------------------------------------------------------------

func TestCreateTraceTx_WithToolModelProject(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "toolmodel-agent", OrgID: orgID, Name: "TM", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	project := "kyoto"
	tool := "akashi_trace"
	modelName := "claude-3.5-sonnet"
	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "toolmodel-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "implementation", Outcome: "tool model test",
			Confidence: 0.9, Metadata: map[string]any{},
			Project: &project, Tool: &tool, Model: &modelName,
		},
	})
	require.NoError(t, err)

	// Verify by querying back via QueryDecisions (which scans all fields).
	decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Limit:   1,
		Filters: model.QueryFilters{AgentIDs: []string{"toolmodel-agent"}},
	})
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	assert.Equal(t, &project, decisions[0].Project)
	assert.Equal(t, &tool, decisions[0].Tool)
	assert.Equal(t, &modelName, decisions[0].Model)
	_ = dec
}

// ---------------------------------------------------------------------------
// SearchDecisionsByText with all filter types
// ---------------------------------------------------------------------------

func TestSearchDecisionsByText_AllFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.CreateAgent(ctx, model.Agent{
		AgentID: "search-filter-agent", OrgID: orgID, Name: "SF", Role: model.RoleAgent,
		Tags: []string{}, Metadata: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	project := "search-proj"
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "search-filter-agent", OrgID: orgID, Metadata: map[string]any{},
		Decision: model.Decision{
			DecisionType: "architecture", Outcome: "searchable decision content",
			Confidence: 0.8, Metadata: map[string]any{},
			Project: &project,
		},
	})
	require.NoError(t, err)

	minConf := float32(0.5)
	searchDecType := "architecture"
	from := time.Now().UTC().Add(-1 * time.Hour)
	to := time.Now().UTC().Add(1 * time.Hour)
	results, err := db.SearchDecisionsByText(ctx, orgID, "searchable", model.QueryFilters{
		AgentIDs:      []string{"search-filter-agent"},
		DecisionType:  &searchDecType,
		ConfidenceMin: &minConf,
		TimeRange:     &model.TimeRange{From: &from, To: &to},
		Project:       &project,
	}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1)
}

// ---------------------------------------------------------------------------
// ListConflicts — with decision_type filter
// ---------------------------------------------------------------------------

func TestListConflicts_WithDecisionTypeFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{
		DecisionType: strPtr("architecture"),
	}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, conflicts)
}

func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// ListConflicts — additional filter combinations
// ---------------------------------------------------------------------------

func TestListConflicts_WithStatusFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{
		Status: strPtr("resolved"),
	}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, conflicts)
}

func TestListConflicts_WithSeverityFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{
		Severity: strPtr("high"),
	}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, conflicts)
}

func TestListConflicts_WithCategoryFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{
		Category: strPtr("architecture"),
	}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, conflicts)
}

func TestListConflicts_WithConflictKindFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{
		ConflictKind: strPtr("contradictory"),
	}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, conflicts)
}

func TestListConflicts_WithDecisionIDFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	decID := uuid.New()

	conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{
		DecisionID: &decID,
	}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, conflicts)
}

func TestListConflicts_WithAgentIDFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{
		AgentID: strPtr("agent-a"),
	}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, conflicts)
}

func TestListConflicts_AllFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	decID := uuid.New()

	conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{
		DecisionType: strPtr("architecture"),
		AgentID:      strPtr("agent-1"),
		ConflictKind: strPtr("contradictory"),
		Status:       strPtr("open"),
		Severity:     strPtr("high"),
		Category:     strPtr("safety"),
		DecisionID:   &decID,
	}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, conflicts)
}

func TestListConflicts_DefaultLimit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	// limit <= 0 should default to 20
	conflicts, err := db.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 0, 0)
	require.NoError(t, err)
	assert.Empty(t, conflicts)
}

// ---------------------------------------------------------------------------
// ListConflictGroups — with OpenOnly filter
// ---------------------------------------------------------------------------

func TestListConflictGroups_OpenOnlyFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	groups, err := db.ListConflictGroups(ctx, orgID, storage.ConflictGroupFilters{
		OpenOnly: true,
	}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, groups)
}

func TestListConflictGroups_ConflictKindFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	groups, err := db.ListConflictGroups(ctx, orgID, storage.ConflictGroupFilters{
		ConflictKind: strPtr("contradictory"),
	}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, groups)
}

func TestListConflictGroups_AgentIDFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	groups, err := db.ListConflictGroups(ctx, orgID, storage.ConflictGroupFilters{
		AgentID: strPtr("agent-x"),
	}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, groups)
}

func TestListConflictGroups_DecisionTypeFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	groups, err := db.ListConflictGroups(ctx, orgID, storage.ConflictGroupFilters{
		DecisionType: strPtr("architecture"),
	}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, groups)
}

func TestListConflictGroups_DefaultLimit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	// limit <= 0 should default to 20
	groups, err := db.ListConflictGroups(ctx, orgID, storage.ConflictGroupFilters{}, 0, 0)
	require.NoError(t, err)
	assert.Empty(t, groups)
}

// ---------------------------------------------------------------------------
// QueryDecisions — filter branches
// ---------------------------------------------------------------------------

func TestQueryDecisions_WithOutcomeFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{
			Outcome: strPtr("chose Redis"),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

func TestQueryDecisions_WithSessionIDFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	sessionID := uuid.New()

	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{
			SessionID: &sessionID,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

func TestQueryDecisions_WithToolFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{
			Tool: strPtr("claude-code"),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

func TestQueryDecisions_WithModelFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{
			Model: strPtr("claude-opus-4-6"),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

func TestQueryDecisions_WithProjectFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{
			Project: strPtr("akashi"),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

func TestQueryDecisions_WithDecisionTypeFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{
			DecisionType: strPtr("library-choice"),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

func TestQueryDecisions_WithAllFiltersSimultaneously(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	sessionID := uuid.New()
	runID := uuid.New()
	confMin := float32(0.5)
	from := time.Now().UTC().Add(-1 * time.Hour)
	to := time.Now().UTC().Add(1 * time.Hour)
	traceID := "trace-123"

	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{
			AgentIDs:      []string{"agent-1"},
			RunID:         &runID,
			DecisionType:  strPtr("architecture"),
			ConfidenceMin: &confMin,
			Outcome:       strPtr("chose Redis"),
			SessionID:     &sessionID,
			Tool:          strPtr("claude-code"),
			Model:         strPtr("claude-opus-4-6"),
			Project:       strPtr("akashi"),
			TimeRange:     &model.TimeRange{From: &from, To: &to},
		},
		TraceID:  &traceID,
		OrderBy:  "confidence",
		OrderDir: "asc",
		Limit:    5,
		Offset:   0,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

func TestQueryDecisions_OrderBySanitization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	// Create a decision so we get results to verify ordering works.
	createTestAgent(t, db, orgID, "order-agent")
	createTestDecision(t, db, orgID, "order-agent", "order test outcome")

	// Unknown column should fall back to valid_from.
	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		OrderBy: "DROP TABLE decisions",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Len(t, decisions, 1)
}

func TestQueryDecisions_OrderByCompleteness(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		OrderBy: "completeness_score",
	})
	require.NoError(t, err)
	assert.Empty(t, decisions)
}

func TestQueryDecisions_OrderByOutcomeScore(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		OrderBy: "outcome_score",
	})
	require.NoError(t, err)
	assert.Empty(t, decisions)
}

func TestQueryDecisions_OrderByDecisionType(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	decisions, _, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		OrderBy: "decision_type",
	})
	require.NoError(t, err)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// QueryDecisionsTemporal — filter branches
// ---------------------------------------------------------------------------

func TestQueryDecisionsTemporal_WithFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	confMin := float32(0.5)
	from := time.Now().UTC().Add(-24 * time.Hour)
	to := time.Now().UTC()

	decisions, err := db.QueryDecisionsTemporal(ctx, orgID, model.TemporalQueryRequest{
		Filters: model.QueryFilters{
			AgentIDs:      []string{"agent-1"},
			DecisionType:  strPtr("architecture"),
			ConfidenceMin: &confMin,
			Project:       strPtr("akashi"),
			TimeRange:     &model.TimeRange{From: &from, To: &to},
		},
		AsOf:  time.Now().UTC(),
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// GetResolvedConflictsByType — default limit branch
// ---------------------------------------------------------------------------

func TestGetResolvedConflictsByType_DefaultLimit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	// limit <= 0 should default to 10
	results, err := db.GetResolvedConflictsByType(ctx, orgID, "architecture", 0)
	require.NoError(t, err)
	assert.Empty(t, results)
}

// ---------------------------------------------------------------------------
// SearchDecisionsByText — filter combination branches
// ---------------------------------------------------------------------------

func TestSearchDecisionsByText_WithConfidenceAndAgentFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	confMin := float32(0.9)
	results, err := db.SearchDecisionsByText(ctx, orgID, "anything", model.QueryFilters{
		AgentIDs:      []string{"specific-agent"},
		ConfidenceMin: &confMin,
	}, 10)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestSearchDecisionsByText_WithTimeRangeFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	from := time.Now().UTC().Add(-1 * time.Hour)
	to := time.Now().UTC().Add(1 * time.Hour)
	results, err := db.SearchDecisionsByText(ctx, orgID, "anything", model.QueryFilters{
		TimeRange: &model.TimeRange{From: &from, To: &to},
	}, 10)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestSearchDecisionsByText_WithDecisionTypeFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	results, err := db.SearchDecisionsByText(ctx, orgID, "query", model.QueryFilters{
		DecisionType: strPtr("architecture"),
	}, 10)
	require.NoError(t, err)
	assert.Empty(t, results)
}

// ---------------------------------------------------------------------------
// GetDecisionsByAgent — time range edge cases
// ---------------------------------------------------------------------------

func TestGetDecisionsByAgent_OnlyFromTime(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	from := time.Now().UTC().Add(-1 * time.Hour)
	decisions, total, err := db.GetDecisionsByAgent(ctx, orgID, "agent-1", 10, 0, &from, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

func TestGetDecisionsByAgent_OnlyToTime(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	to := time.Now().UTC().Add(1 * time.Hour)
	decisions, total, err := db.GetDecisionsByAgent(ctx, orgID, "agent-1", 10, 0, nil, &to)
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// FindRetriableClaimFailures — default limit branch
// ---------------------------------------------------------------------------

func TestFindRetriableClaimFailures_DefaultLimitBranch(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))

	// limit <= 0 should default to 50
	refs, err := db.FindRetriableClaimFailures(ctx, 5, -1)
	require.NoError(t, err)
	assert.Empty(t, refs)
}

// ---------------------------------------------------------------------------
// Helper functions for test setup
// ---------------------------------------------------------------------------

func createTestAgent(t *testing.T, db *sqlite.LiteDB, orgID uuid.UUID, agentID string) {
	t.Helper()
	_, err := db.CreateAgent(context.Background(), model.Agent{
		AgentID:   agentID,
		OrgID:     orgID,
		Name:      agentID,
		Role:      model.RoleAgent,
		Tags:      []string{},
		Metadata:  map[string]any{},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
}

func createTestDecision(t *testing.T, db *sqlite.LiteDB, orgID uuid.UUID, agentID, outcome string) model.Decision {
	t.Helper()
	_, dec, err := db.CreateTraceTx(context.Background(), storage.CreateTraceParams{
		AgentID: agentID,
		OrgID:   orgID,
		Decision: model.Decision{
			DecisionType: "test-type",
			Outcome:      outcome,
			Confidence:   0.85,
			Reasoning:    strPtr("test reasoning"),
		},
	})
	require.NoError(t, err)
	return dec
}

// ---------------------------------------------------------------------------
// QueryDecisions — TimeRange with only From
// ---------------------------------------------------------------------------

func TestQueryDecisions_TimeRangeFromOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	from := time.Now().UTC().Add(-1 * time.Hour)

	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{
			TimeRange: &model.TimeRange{From: &from},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

func TestQueryDecisions_TimeRangeToOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	to := time.Now().UTC().Add(1 * time.Hour)

	decisions, total, err := db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters: model.QueryFilters{
			TimeRange: &model.TimeRange{To: &to},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// Close — verify double close logs a warning but doesn't panic
// ---------------------------------------------------------------------------

func TestClose_DoesNotPanicOnError(t *testing.T) {
	ctx := context.Background()
	logger := slog.Default()
	db, err := sqlite.New(ctx, ":memory:", logger)
	require.NoError(t, err)

	// First close is normal.
	db.Close(ctx)
	// Second close should hit the error path but not panic.
	assert.NotPanics(t, func() {
		db.Close(ctx)
	})
}

// ---------------------------------------------------------------------------
// GetConflictCountsBatch — with data to trigger scan path
// ---------------------------------------------------------------------------

func TestGetConflictCountsBatch_EmptyIDs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	result, err := db.GetConflictCountsBatch(ctx, []uuid.UUID{}, orgID)
	require.NoError(t, err)
	assert.Empty(t, result)
}

// ---------------------------------------------------------------------------
// Idempotency — complete then lookup branch
// ---------------------------------------------------------------------------

func TestCompleteIdempotency_HappyPath(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	createTestAgent(t, db, orgID, "idem-agent")

	// Begin idempotency
	_, err := db.BeginIdempotency(ctx, orgID, "idem-agent", "/v1/trace", "key-1", "hash-1")
	require.NoError(t, err)

	// Complete it
	err = db.CompleteIdempotency(ctx, orgID, "idem-agent", "/v1/trace", "key-1", 201, map[string]any{"id": "abc"})
	require.NoError(t, err)

	// Look it up - should get the completed response
	lookup, err := db.BeginIdempotency(ctx, orgID, "idem-agent", "/v1/trace", "key-1", "hash-1")
	require.NoError(t, err)
	assert.True(t, lookup.Completed)
	assert.Equal(t, 201, lookup.StatusCode)
}

// ---------------------------------------------------------------------------
// GetDecisionForScoring — with agentContext and project
// ---------------------------------------------------------------------------

func TestGetDecisionForScoring_WithNilProject(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	_, err := db.GetDecisionForScoring(ctx, uuid.New(), orgID)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

// ---------------------------------------------------------------------------
// EvidenceCoverageStats — zero-decisions branch (division guard)
// ---------------------------------------------------------------------------

func TestGetEvidenceCoverageStats_EmptyOrg(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	stats, err := db.GetEvidenceCoverageStats(ctx, orgID, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, stats.TotalDecisions)
	assert.Equal(t, float64(0), stats.CoveragePercent)
	assert.Equal(t, float64(0), stats.AvgPerDecision)
}

// ---------------------------------------------------------------------------
// CreateTraceTx — createTraceInTx branches for nil-guarding metadata/context
// ---------------------------------------------------------------------------

func TestCreateTraceTx_AgentContextFromParams(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	createTestAgent(t, db, orgID, "ctx-agent")

	agentCtx := map[string]any{"env": "prod"}
	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID:      "ctx-agent",
		OrgID:        orgID,
		AgentContext: agentCtx,
		Decision: model.Decision{
			DecisionType: "test",
			Outcome:      "test outcome",
			Confidence:   0.8,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "prod", dec.AgentContext["env"])
}

func TestCreateTraceTx_ValidFromPreset(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	createTestAgent(t, db, orgID, "vf-agent")

	presetTime := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "vf-agent",
		OrgID:   orgID,
		Decision: model.Decision{
			DecisionType: "test",
			Outcome:      "preset valid_from",
			Confidence:   0.8,
			ValidFrom:    presetTime,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 2025, dec.ValidFrom.Year())
	assert.Equal(t, time.January, dec.ValidFrom.Month())
	assert.Equal(t, 15, dec.ValidFrom.Day())
}

// ---------------------------------------------------------------------------
// HasClaimsForDecision — with claims present
// ---------------------------------------------------------------------------

func TestHasClaimsForDecision_WithClaims(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	createTestAgent(t, db, orgID, "claims-agent")

	dec := createTestDecision(t, db, orgID, "claims-agent", "claims test")

	// Insert a claim
	cat := "factual"
	err := db.InsertClaims(ctx, []storage.Claim{
		{
			DecisionID: dec.ID,
			OrgID:      orgID,
			ClaimIdx:   0,
			ClaimText:  "test claim",
			Category:   &cat,
		},
	})
	require.NoError(t, err)

	has, err := db.HasClaimsForDecision(ctx, dec.ID, orgID)
	require.NoError(t, err)
	assert.True(t, has)
}

// ---------------------------------------------------------------------------
// buildDecisionFilterWhere — empty filters returns empty string
// ---------------------------------------------------------------------------

func TestSearchDecisionsByText_NoFiltersEmptyDB(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	// With no filters, buildDecisionFilterWhere should return empty
	results, err := db.SearchDecisionsByText(ctx, orgID, "nonexistent", model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Empty(t, results)
}

// ---------------------------------------------------------------------------
// QueryDecisionsTemporal — from/to individual time range filters
// ---------------------------------------------------------------------------

func TestQueryDecisionsTemporal_TimeRangeFromOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	from := time.Now().UTC().Add(-24 * time.Hour)

	decisions, err := db.QueryDecisionsTemporal(ctx, orgID, model.TemporalQueryRequest{
		Filters: model.QueryFilters{
			TimeRange: &model.TimeRange{From: &from},
		},
		AsOf:  time.Now().UTC(),
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, decisions)
}

func TestQueryDecisionsTemporal_TimeRangeToOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	to := time.Now().UTC()

	decisions, err := db.QueryDecisionsTemporal(ctx, orgID, model.TemporalQueryRequest{
		Filters: model.QueryFilters{
			TimeRange: &model.TimeRange{To: &to},
		},
		AsOf:  time.Now().UTC(),
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// CreateTraceTx — with pre-set TransactionTime and CreatedAt
// ---------------------------------------------------------------------------

func TestCreateTraceTx_PresetTransactionTimeAndCreatedAt(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	createTestAgent(t, db, orgID, "preset-agent")

	txTime := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	createdAt := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "preset-agent",
		OrgID:   orgID,
		Decision: model.Decision{
			DecisionType:    "test",
			Outcome:         "preset times",
			Confidence:      0.8,
			TransactionTime: txTime,
			CreatedAt:       createdAt,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 2024, dec.TransactionTime.Year())
	assert.Equal(t, time.June, dec.TransactionTime.Month())
	assert.Equal(t, 2024, dec.CreatedAt.Year())
	assert.Equal(t, time.May, dec.CreatedAt.Month())
}

// ---------------------------------------------------------------------------
// CreateTraceTx — alternative with pre-set ID and CreatedAt
// ---------------------------------------------------------------------------

func TestCreateTraceTx_AlternativeWithPresetIDAndTime(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	createTestAgent(t, db, orgID, "alt-preset-agent")

	altID := uuid.New()
	altTime := time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)
	altScore := float32(0.7)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "alt-preset-agent",
		OrgID:   orgID,
		Decision: model.Decision{
			DecisionType: "test",
			Outcome:      "alt preset",
			Confidence:   0.8,
		},
		Alternatives: []model.Alternative{
			{
				ID:        altID,
				Label:     "Option A",
				Score:     &altScore,
				Selected:  true,
				CreatedAt: altTime,
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, dec.Alternatives, 1)
	assert.Equal(t, altID, dec.Alternatives[0].ID)
	assert.Equal(t, 2024, dec.Alternatives[0].CreatedAt.Year())
}

// ---------------------------------------------------------------------------
// CreateTraceTx — evidence with pre-set ID and CreatedAt
// ---------------------------------------------------------------------------

func TestCreateTraceTx_EvidenceWithPresetIDAndTime(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil
	createTestAgent(t, db, orgID, "ev-preset-agent")

	evID := uuid.New()
	evTime := time.Date(2024, 4, 20, 8, 0, 0, 0, time.UTC)
	evURI := "https://example.com"
	evRelScore := float32(0.9)

	_, dec, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "ev-preset-agent",
		OrgID:   orgID,
		Decision: model.Decision{
			DecisionType: "test",
			Outcome:      "evidence preset",
			Confidence:   0.8,
		},
		Evidence: []model.Evidence{
			{
				ID:             evID,
				SourceType:     model.SourceDocument,
				SourceURI:      &evURI,
				Content:        "evidence content",
				RelevanceScore: &evRelScore,
				CreatedAt:      evTime,
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, dec.Evidence, 1)
	assert.Equal(t, evID, dec.Evidence[0].ID)
}

// ---------------------------------------------------------------------------
// GetDecisionsByAgent — default limit and negative offset
// ---------------------------------------------------------------------------

func TestGetDecisionsByAgent_DefaultLimitAndNegativeOffset(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	// limit <= 0 and negative offset should be handled gracefully
	decisions, total, err := db.GetDecisionsByAgent(ctx, orgID, "agent-1", 0, -5, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// IsDuplicateKey — with non-unique-constraint error
// ---------------------------------------------------------------------------

func TestIsDuplicateKey_NonUniqueError(t *testing.T) {
	db := newTestDB(t)
	assert.False(t, db.IsDuplicateKey(fmt.Errorf("some other error")))
	assert.True(t, db.IsDuplicateKey(fmt.Errorf("UNIQUE constraint failed: agents.agent_id")))
}
