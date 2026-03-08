package sqlite_test

import (
	"context"
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
		stats, err := db.GetDecisionQualityStats(ctx, orgID)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Total)
		assert.Equal(t, 1, stats.WithReasoning)
	})

	t.Run("evidence coverage", func(t *testing.T) {
		stats, err := db.GetEvidenceCoverageStats(ctx, orgID)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.TotalDecisions)
		assert.Equal(t, 0, stats.WithEvidence) // no evidence attached
	})

	t.Run("conflict status counts", func(t *testing.T) {
		counts, err := db.GetConflictStatusCounts(ctx, orgID)
		require.NoError(t, err)
		assert.Equal(t, 0, counts.Total)
	})

	t.Run("outcome signals summary", func(t *testing.T) {
		summary, err := db.GetOutcomeSignalsSummary(ctx, orgID)
		require.NoError(t, err)
		assert.Equal(t, 1, summary.DecisionsTotal)
		assert.Equal(t, 1, summary.NeverSuperseded)
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
