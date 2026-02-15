package decisions_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/search"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/migrations"
)

var (
	testDB  *storage.DB
	testSvc *decisions.Service
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "timescale/timescaledb:latest-pg18",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "akashi",
			"POSTGRES_PASSWORD": "akashi",
			"POSTGRES_DB":       "akashi",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(60 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start container: %v\n", err)
		os.Exit(1)
	}

	host, _ := container.Host(ctx)
	port, _ := container.MappedPort(ctx, "5432")
	dsn := fmt.Sprintf("postgres://akashi:akashi@%s:%s/akashi?sslmode=disable", host, port.Port())

	bootstrapConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to bootstrap: %v\n", err)
		os.Exit(1)
	}
	_, _ = bootstrapConn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector")
	_, _ = bootstrapConn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS timescaledb")
	_ = bootstrapConn.Close(ctx)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	testDB, err = storage.New(ctx, dsn, "", logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create DB: %v\n", err)
		os.Exit(1)
	}

	if err := testDB.RunMigrations(ctx, migrations.FS); err != nil {
		fmt.Fprintf(os.Stderr, "migrations failed: %v\n", err)
		os.Exit(1)
	}

	if err := testDB.EnsureDefaultOrg(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ensure default org: %v\n", err)
		os.Exit(1)
	}

	embedder := embedding.NewNoopProvider(1024)
	testSvc = decisions.New(testDB, embedder, nil, logger, nil)

	code := m.Run()
	testDB.Close(ctx)
	_ = container.Terminate(ctx)
	os.Exit(code)
}

func ptr[T any](v T) *T { return &v }

func createAgent(t *testing.T, agentID string) {
	t.Helper()
	_, err := testDB.CreateAgent(context.Background(), model.Agent{
		AgentID: agentID,
		OrgID:   uuid.Nil,
		Name:    agentID,
		Role:    model.RoleAgent,
	})
	require.NoError(t, err)
}

func TestTrace_WithNoopEmbedder(t *testing.T) {
	ctx := context.Background()
	agentID := "trace-noop-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	reasoning := "test reasoning for noop"
	result, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      "chose option A for testing",
			Confidence:   0.85,
			Reasoning:    &reasoning,
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, result.RunID)
	assert.NotEqual(t, uuid.Nil, result.DecisionID)
	assert.Equal(t, 1, result.EventCount, "1 decision, 0 alts, 0 evidence")
}

func TestTrace_WithAlternativesAndEvidence(t *testing.T) {
	ctx := context.Background()
	agentID := "trace-full-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	result, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "trade_off",
			Outcome:      "chose Redis over Memcached",
			Confidence:   0.75,
			Alternatives: []model.TraceAlternative{
				{Label: "Memcached", Score: ptr(float32(0.6)), Selected: false},
				{Label: "Redis", Score: ptr(float32(0.9)), Selected: true},
			},
			Evidence: []model.TraceEvidence{
				{SourceType: "document", Content: "Redis supports pub/sub which we need"},
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 4, result.EventCount, "1 decision + 2 alts + 1 evidence")
}

func TestSearch_TextFallback(t *testing.T) {
	ctx := context.Background()
	agentID := "search-text-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Create a decision to search for.
	_, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      "unique-search-keyword-" + agentID,
			Confidence:   0.9,
		},
	})
	require.NoError(t, err)

	// Search should fall through to text search (no Qdrant configured).
	results, err := testSvc.Search(ctx, uuid.Nil, "unique-search-keyword-"+agentID, true, model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.NotEmpty(t, results, "text search should find the decision")
}

func TestCheck_StructuredQuery(t *testing.T) {
	ctx := context.Background()
	agentID := "check-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Create a decision.
	_, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "security",
			Outcome:      "chose mTLS for service mesh",
			Confidence:   0.95,
		},
	})
	require.NoError(t, err)

	// Check should find the precedent.
	resp, err := testSvc.Check(ctx, uuid.Nil, "security", "", agentID, 5)
	require.NoError(t, err)
	assert.True(t, resp.HasPrecedent)
	assert.NotEmpty(t, resp.Decisions)
	assert.Equal(t, "security", resp.Decisions[0].DecisionType)
}

func TestResolveOrCreateAgent_Existing(t *testing.T) {
	ctx := context.Background()
	agentID := "existing-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	err := testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin)
	assert.NoError(t, err, "existing agent should resolve without error")
}

func TestResolveOrCreateAgent_AutoRegisterAsAdmin(t *testing.T) {
	ctx := context.Background()
	agentID := "auto-reg-" + uuid.New().String()[:8]

	err := testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin)
	require.NoError(t, err, "admin should auto-register new agents")

	// Verify it was actually created.
	_, err = testDB.GetAgentByAgentID(ctx, uuid.Nil, agentID)
	assert.NoError(t, err, "auto-registered agent should exist")
}

func TestResolveOrCreateAgent_DeniedAsNonAdmin(t *testing.T) {
	ctx := context.Background()
	agentID := "no-auto-" + uuid.New().String()[:8]

	err := testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAgent)
	assert.ErrorIs(t, err, decisions.ErrAgentNotFound, "non-admin should not auto-register")
}

func TestBackfillEmbeddings_NoopReturnsZero(t *testing.T) {
	ctx := context.Background()

	n, err := testSvc.BackfillEmbeddings(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "noop provider should short-circuit and return 0")
}

func TestRecent_Pagination(t *testing.T) {
	ctx := context.Background()
	agentID := "recent-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Create 3 decisions.
	for i := range 3 {
		_, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
			AgentID: agentID,
			Decision: model.TraceDecision{
				DecisionType: "planning",
				Outcome:      fmt.Sprintf("plan iteration %d for %s", i, agentID),
				Confidence:   0.7,
			},
		})
		require.NoError(t, err)
	}

	// Limit=2 should return exactly 2.
	filters := model.QueryFilters{AgentIDs: []string{agentID}}
	decs, total, err := testSvc.Recent(ctx, uuid.Nil, filters, 2, 0)
	require.NoError(t, err)
	assert.Len(t, decs, 2)
	assert.GreaterOrEqual(t, total, 3)

	// Offset=2 should return the remaining decision(s).
	decs2, _, err := testSvc.Recent(ctx, uuid.Nil, filters, 2, 2)
	require.NoError(t, err)
	assert.NotEmpty(t, decs2, "offset=2 should still return results")
}

func TestSearch_NoSearcher(t *testing.T) {
	ctx := context.Background()
	agentID := "search-nosrch-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// testSvc has nil searcher (set in TestMain). Searching with semantic=true
	// should skip the Qdrant path entirely and fall through to text search.
	results, err := testSvc.Search(ctx, uuid.Nil, "nonexistent-query-term", true, model.QueryFilters{}, 10)
	require.NoError(t, err, "nil searcher should not cause an error")
	// Results may be empty (or nil) since the query term doesn't match anything.
	// The key assertion is that no error occurs when searcher is nil.
	assert.Empty(t, results, "no matching decisions should be found for a nonexistent query")
}

func TestSearch_ZeroVector(t *testing.T) {
	ctx := context.Background()
	agentID := "search-zero-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Create a decision with a unique keyword so text search can find it.
	keyword := "zerovector-" + agentID
	_, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      keyword,
			Confidence:   0.8,
		},
	})
	require.NoError(t, err)

	// NoopProvider.Embed returns ErrNoProvider, so the service logs a warning
	// and skips embedding. With nil searcher this falls through to text search.
	results, err := testSvc.Search(ctx, uuid.Nil, keyword, true, model.QueryFilters{}, 10)
	require.NoError(t, err, "noop provider should not cause search to error")
	assert.NotEmpty(t, results, "text fallback should find the decision by keyword")
}

func TestCheck_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	agentID := "check-deflim-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Create a decision so the check has something to find.
	_, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "planning",
			Outcome:      "default limit test",
			Confidence:   0.7,
		},
	})
	require.NoError(t, err)

	// Check with limit=0 should default to 5 (service.go line 206-208).
	resp, err := testSvc.Check(ctx, uuid.Nil, "planning", "", agentID, 0)
	require.NoError(t, err, "limit=0 should default to 5, not error")
	assert.True(t, resp.HasPrecedent)
	assert.NotEmpty(t, resp.Decisions)
	assert.LessOrEqual(t, len(resp.Decisions), 5, "default limit should cap at 5")
}

func TestTrace_NilReasoning(t *testing.T) {
	ctx := context.Background()
	agentID := "trace-nilrsn-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Trace with nil Reasoning (the field is *string, nil is valid).
	result, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      "decision with no reasoning provided",
			Confidence:   0.6,
			Reasoning:    nil,
		},
	})
	require.NoError(t, err, "nil reasoning should be accepted")
	assert.NotEqual(t, uuid.Nil, result.DecisionID)
	assert.Equal(t, 1, result.EventCount)
}

func TestTrace_MultipleDecisionTypes(t *testing.T) {
	ctx := context.Background()
	agentID := "trace-multi-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	types := []string{"architecture", "security", "trade_off"}
	ids := make(map[uuid.UUID]bool, len(types))

	for _, dt := range types {
		result, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
			AgentID: agentID,
			Decision: model.TraceDecision{
				DecisionType: dt,
				Outcome:      "outcome for " + dt,
				Confidence:   0.8,
			},
		})
		require.NoError(t, err, "trace with decision_type=%q should succeed", dt)
		assert.NotEqual(t, uuid.Nil, result.DecisionID, "decision ID for %q should not be nil", dt)

		assert.False(t, ids[result.DecisionID], "decision ID %s should be unique (type=%q)", result.DecisionID, dt)
		ids[result.DecisionID] = true
	}

	assert.Len(t, ids, len(types), "should have %d unique decision IDs", len(types))
}

func TestTrace_InvalidConfidence(t *testing.T) {
	ctx := context.Background()
	agentID := "trace-badconf-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// The service layer (Service.Trace) does NOT validate confidence bounds.
	// However, the database enforces a CHECK constraint on the confidence column:
	//   CHECK (confidence >= 0.0 AND confidence <= 1.0)
	// This test documents that the DB constraint catches out-of-range values
	// even when the service layer doesn't validate them.
	_, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      "confidence out of bounds",
			Confidence:   1.5,
		},
	})
	require.Error(t, err, "confidence > 1.0 should be rejected by the DB CHECK constraint")
	assert.Contains(t, err.Error(), "confidence", "error should reference the confidence constraint")
}

func TestTrace_InvalidAgentID(t *testing.T) {
	ctx := context.Background()

	// The service layer (Service.Trace) does NOT validate agent_id format.
	// Validation is done by the HTTP handler (model.ValidateAgentID) and
	// MCP tools layer before calling the service. An empty agent_id passes
	// through to the database, which has TEXT NOT NULL (empty string is not NULL).
	// This test documents current behavior: the service does not reject empty agent_id.
	result, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: "",
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      "trace with empty agent_id",
			Confidence:   0.5,
		},
	})
	require.NoError(t, err, "service layer does not validate agent_id; that is the handler's job")
	assert.NotEqual(t, uuid.Nil, result.DecisionID)
}

func TestCheck_SemanticPath(t *testing.T) {
	ctx := context.Background()
	agentID := "check-sem-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Create a decision so the search has data to find.
	keyword := "semanticpath-" + agentID
	_, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "investigation",
			Outcome:      keyword,
			Confidence:   0.9,
		},
	})
	require.NoError(t, err)

	// Check with a non-empty query triggers the Search path inside Check
	// (service.go line 212). With nil searcher and noop embedder, this falls
	// through to text search.
	resp, err := testSvc.Check(ctx, uuid.Nil, "investigation", keyword, agentID, 5)
	require.NoError(t, err, "semantic path via Check should not error")
	assert.True(t, resp.HasPrecedent, "text fallback should find the decision")
	assert.NotEmpty(t, resp.Decisions)
}

func TestRecent_EmptyFilters(t *testing.T) {
	ctx := context.Background()

	// Recent with empty QueryFilters should return decisions from all agents.
	// Earlier tests have already traced decisions, so the table is not empty.
	decs, total, err := testSvc.Recent(ctx, uuid.Nil, model.QueryFilters{}, 10, 0)
	require.NoError(t, err, "empty filters should not error")
	assert.NotEmpty(t, decs, "should return decisions from earlier tests")
	assert.GreaterOrEqual(t, total, 1, "total should reflect at least 1 decision")
}

func TestQuery_Basic(t *testing.T) {
	ctx := context.Background()
	agentID := "query-basic-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Create two decisions with distinct types.
	for _, dt := range []string{"architecture", "security"} {
		_, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
			AgentID: agentID,
			Decision: model.TraceDecision{
				DecisionType: dt,
				Outcome:      "query basic outcome " + dt,
				Confidence:   0.8,
			},
		})
		require.NoError(t, err)
	}

	// Query with a decision_type filter should return only matching decisions.
	archType := "architecture"
	decs, total, err := testSvc.Query(ctx, uuid.Nil, model.QueryRequest{
		Filters: model.QueryFilters{
			AgentIDs:     []string{agentID},
			DecisionType: &archType,
		},
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total, "exactly one architecture decision for this agent")
	require.Len(t, decs, 1)
	assert.Equal(t, "architecture", decs[0].DecisionType)
	assert.Equal(t, "query basic outcome architecture", decs[0].Outcome)

	// Query without type filter should return both.
	decs, total, err = testSvc.Query(ctx, uuid.Nil, model.QueryRequest{
		Filters: model.QueryFilters{AgentIDs: []string{agentID}},
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	assert.Len(t, decs, 2)
}

func TestQueryTemporal(t *testing.T) {
	ctx := context.Background()
	agentID := "query-temporal-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Record the time before any decisions exist.
	beforeTrace := time.Now().UTC()
	time.Sleep(50 * time.Millisecond)

	// Create a decision.
	_, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "planning",
			Outcome:      "temporal query test outcome",
			Confidence:   0.7,
		},
	})
	require.NoError(t, err)

	// Record the time after the decision was created.
	afterTrace := time.Now().UTC()

	// Querying as of a time after the trace should find the decision.
	decs, err := testSvc.QueryTemporal(ctx, uuid.Nil, model.TemporalQueryRequest{
		AsOf:    afterTrace,
		Filters: model.QueryFilters{AgentIDs: []string{agentID}},
		Limit:   10,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, decs, "temporal query after trace should find the decision")
	found := false
	for _, d := range decs {
		if d.Outcome == "temporal query test outcome" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected to find the specific decision in temporal results")

	// Querying as of a time before the trace should NOT find it.
	decs, err = testSvc.QueryTemporal(ctx, uuid.Nil, model.TemporalQueryRequest{
		AsOf:    beforeTrace,
		Filters: model.QueryFilters{AgentIDs: []string{agentID}},
		Limit:   10,
	})
	require.NoError(t, err)
	for _, d := range decs {
		assert.NotEqual(t, "temporal query test outcome", d.Outcome,
			"temporal query before trace should not return the decision")
	}
}

func TestBackfillEmbeddings_NoopSkips(t *testing.T) {
	ctx := context.Background()

	// With a NoopProvider, BackfillEmbeddings should probe, detect the noop,
	// and return 0 without error. This tests the early-exit path.
	n, err := testSvc.BackfillEmbeddings(ctx, 50)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "noop provider should short-circuit backfill")
}

func TestSemanticSearchAvailable(t *testing.T) {
	// testSvc has nil searcher and NoopProvider (set in TestMain).
	// SemanticSearchAvailable should return false because:
	// 1. The searcher is nil (no Qdrant), AND
	// 2. The embedder is a NoopProvider.

	got := testSvc.SemanticSearchAvailable()
	assert.False(t, got, "semantic search should be unavailable with nil searcher and noop embedder")
}

func TestHydrateAndReScore(t *testing.T) {
	ctx := context.Background()
	agentID := "hydrate-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Create a decision with alternatives and evidence so that text search
	// returns a hydrated result.
	reasoning := "thorough analysis of caching options"
	_, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      "hydrate-rescore-unique-" + agentID,
			Confidence:   0.85,
			Reasoning:    &reasoning,
			Alternatives: []model.TraceAlternative{
				{Label: "Option A", Score: ptr(float32(0.3)), Selected: false},
				{Label: "Option B", Score: ptr(float32(0.9)), Selected: true},
			},
			Evidence: []model.TraceEvidence{
				{SourceType: "document", Content: "benchmark results for caching layers"},
			},
		},
	})
	require.NoError(t, err)

	// Search by text (falls through to text search with noop embedder / nil searcher).
	results, err := testSvc.Search(ctx, uuid.Nil, "hydrate-rescore-unique-"+agentID, false, model.QueryFilters{}, 10)
	require.NoError(t, err)
	require.NotEmpty(t, results, "text search should find the decision")

	// Verify the result has the expected fields.
	found := results[0]
	assert.Equal(t, "hydrate-rescore-unique-"+agentID, found.Decision.Outcome)
	assert.Equal(t, "architecture", found.Decision.DecisionType)
}

func TestTrace_WithEvidence(t *testing.T) {
	ctx := context.Background()
	agentID := "trace-evidence-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	sourceURI := "https://example.com/benchmark.pdf"
	relevanceScore := float32(0.95)
	result, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "trade_off",
			Outcome:      "chose option with evidence",
			Confidence:   0.9,
			Evidence: []model.TraceEvidence{
				{
					SourceType:     "document",
					SourceURI:      &sourceURI,
					Content:        "benchmark showed 3x throughput improvement",
					RelevanceScore: &relevanceScore,
				},
				{
					SourceType: "api_response",
					Content:    "API returned 200 OK with valid payload",
				},
			},
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, result.DecisionID)
	// 1 decision + 0 alternatives + 2 evidence = 3 events
	assert.Equal(t, 3, result.EventCount, "should count decision + 2 evidence items")

	// Read the decision back with evidence to verify persistence.
	dec, err := testDB.GetDecision(ctx, uuid.Nil, result.DecisionID, storage.GetDecisionOpts{
		IncludeEvidence: true,
	})
	require.NoError(t, err)
	require.Len(t, dec.Evidence, 2, "should have 2 evidence items persisted")

	// Verify first evidence item (document with URI and relevance score).
	ev0 := dec.Evidence[0]
	assert.Equal(t, model.SourceDocument, ev0.SourceType)
	assert.Equal(t, "benchmark showed 3x throughput improvement", ev0.Content)
	require.NotNil(t, ev0.SourceURI)
	assert.Equal(t, sourceURI, *ev0.SourceURI)
	require.NotNil(t, ev0.RelevanceScore)
	assert.InDelta(t, 0.95, float64(*ev0.RelevanceScore), 0.001)

	// Verify second evidence item (api_response, no URI or relevance score).
	ev1 := dec.Evidence[1]
	assert.Equal(t, model.SourceAPIResponse, ev1.SourceType)
	assert.Equal(t, "API returned 200 OK with valid payload", ev1.Content)
}

func TestTrace_WithPrecedentRef(t *testing.T) {
	ctx := context.Background()
	agentID := "trace-prec-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Decision A: the precedent.
	resultA, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      "chose PostgreSQL for persistence",
			Confidence:   0.9,
		},
	})
	require.NoError(t, err, "decision A should succeed")
	assert.NotEqual(t, uuid.Nil, resultA.DecisionID)

	// Decision B: references A as a precedent.
	precedentID := resultA.DecisionID
	resultB, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID:      agentID,
		PrecedentRef: &precedentID,
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      "chose pgvector extension for embeddings",
			Confidence:   0.85,
		},
	})
	require.NoError(t, err, "decision B with precedent_ref should succeed")
	assert.NotEqual(t, uuid.Nil, resultB.DecisionID)
	assert.NotEqual(t, resultA.DecisionID, resultB.DecisionID, "A and B should have distinct IDs")

	// Read decision B back from storage and verify the precedent_ref was persisted.
	decB, err := testDB.GetDecision(ctx, uuid.Nil, resultB.DecisionID, storage.GetDecisionOpts{})
	require.NoError(t, err, "should be able to read decision B from storage")
	require.NotNil(t, decB.PrecedentRef, "decision B should have a non-nil precedent_ref")
	assert.Equal(t, resultA.DecisionID, *decB.PrecedentRef, "precedent_ref should point to decision A")
}

// ---------------------------------------------------------------------------
// Mock types for testing Qdrant path and real embedding scenarios.
// ---------------------------------------------------------------------------

// mockSearcher implements search.Searcher for testing the Qdrant code path
// in Service.Search without requiring an actual Qdrant instance.
type mockSearcher struct {
	results []search.Result
	err     error
	healthy error
}

func (m *mockSearcher) Search(_ context.Context, _ uuid.UUID, _ []float32, _ model.QueryFilters, _ int) ([]search.Result, error) {
	return m.results, m.err
}

func (m *mockSearcher) Healthy(_ context.Context) error {
	return m.healthy
}

// mockEmbedder implements embedding.Provider and returns deterministic
// non-zero vectors so the Service.Search Qdrant path can proceed past
// the isZeroVector check and the validateEmbeddingDims check.
type mockEmbedder struct {
	dims int
}

func (m *mockEmbedder) Embed(_ context.Context, _ string) (pgvector.Vector, error) {
	return pgvector.NewVector(m.nonZeroSlice()), nil
}

func (m *mockEmbedder) EmbedBatch(_ context.Context, texts []string) ([]pgvector.Vector, error) {
	vecs := make([]pgvector.Vector, len(texts))
	for i := range vecs {
		vecs[i] = pgvector.NewVector(m.nonZeroSlice())
	}
	return vecs, nil
}

func (m *mockEmbedder) Dimensions() int { return m.dims }

// nonZeroSlice returns a float32 slice of length m.dims filled with 0.1
// so isZeroVector returns false.
func (m *mockEmbedder) nonZeroSlice() []float32 {
	s := make([]float32, m.dims)
	for i := range s {
		s[i] = 0.1
	}
	return s
}

// failingEmbedder returns an error on Embed to trigger the embedding-failure
// fallback inside Service.Search.
type failingEmbedder struct {
	dims int
}

func (f *failingEmbedder) Embed(_ context.Context, _ string) (pgvector.Vector, error) {
	return pgvector.Vector{}, errors.New("embedding unavailable")
}

func (f *failingEmbedder) EmbedBatch(_ context.Context, _ []string) ([]pgvector.Vector, error) {
	return nil, errors.New("embedding unavailable")
}

func (f *failingEmbedder) Dimensions() int { return f.dims }

// ---------------------------------------------------------------------------
// Tests: Search with mock Qdrant (Qdrant path)
// ---------------------------------------------------------------------------

func TestSearch_QdrantPath(t *testing.T) {
	ctx := context.Background()
	agentID := "search-qdrant-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Trace a decision so it exists in Postgres for hydration.
	reasoning := "thorough analysis"
	traceResult, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      "qdrant-path-test-" + agentID,
			Confidence:   0.9,
			Reasoning:    &reasoning,
			Alternatives: []model.TraceAlternative{
				{Label: "Option X", Score: ptr(float32(0.4)), Selected: false},
				{Label: "Option Y", Score: ptr(float32(0.9)), Selected: true},
			},
			Evidence: []model.TraceEvidence{
				{SourceType: "document", Content: "supporting evidence text"},
			},
		},
	})
	require.NoError(t, err)

	// Build a Service with a mock searcher that returns the traced decision ID.
	searcher := &mockSearcher{
		results: []search.Result{
			{DecisionID: traceResult.DecisionID, Score: 0.95},
		},
	}
	embedder := &mockEmbedder{dims: 1024}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := decisions.New(testDB, embedder, searcher, logger, nil)

	results, err := svc.Search(ctx, uuid.Nil, "qdrant-path-test", true, model.QueryFilters{}, 10)
	require.NoError(t, err)
	require.NotEmpty(t, results, "Qdrant path should return hydrated results")

	// Verify the hydrated decision matches what we traced.
	found := results[0]
	assert.Equal(t, traceResult.DecisionID, found.Decision.ID)
	assert.Equal(t, "qdrant-path-test-"+agentID, found.Decision.Outcome)
	assert.Equal(t, "architecture", found.Decision.DecisionType)
	assert.Greater(t, found.SimilarityScore, float32(0), "re-scored similarity should be positive")
}

func TestSearch_QdrantFallbackOnError(t *testing.T) {
	ctx := context.Background()
	agentID := "search-qdfall-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Trace a decision with a unique keyword so text search can find it.
	keyword := "qdrantfallback-" + agentID
	_, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      keyword,
			Confidence:   0.8,
		},
	})
	require.NoError(t, err)

	// Mock searcher that returns an error, forcing fallback to text search.
	searcher := &mockSearcher{
		err: errors.New("qdrant connection refused"),
	}
	embedder := &mockEmbedder{dims: 1024}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := decisions.New(testDB, embedder, searcher, logger, nil)

	results, err := svc.Search(ctx, uuid.Nil, keyword, true, model.QueryFilters{}, 10)
	require.NoError(t, err, "Qdrant error should fall through to text search, not propagate")
	assert.NotEmpty(t, results, "text fallback should find the decision by keyword")
	assert.Equal(t, keyword, results[0].Decision.Outcome)
}

func TestSearch_QdrantUnhealthyFallback(t *testing.T) {
	ctx := context.Background()
	agentID := "search-qdunhl-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	keyword := "qdunhealthy-" + agentID
	_, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      keyword,
			Confidence:   0.8,
		},
	})
	require.NoError(t, err)

	// Mock searcher that reports unhealthy — Search should skip Qdrant entirely.
	searcher := &mockSearcher{
		healthy: errors.New("qdrant unreachable"),
	}
	embedder := &mockEmbedder{dims: 1024}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := decisions.New(testDB, embedder, searcher, logger, nil)

	results, err := svc.Search(ctx, uuid.Nil, keyword, true, model.QueryFilters{}, 10)
	require.NoError(t, err, "unhealthy Qdrant should fall through to text search")
	assert.NotEmpty(t, results, "text fallback should find the decision")
}

func TestSearch_QdrantEmptyResultsFallback(t *testing.T) {
	ctx := context.Background()
	agentID := "search-qdempty-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	keyword := "qdemptyresult-" + agentID
	_, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      keyword,
			Confidence:   0.8,
		},
	})
	require.NoError(t, err)

	// Mock searcher returns empty results (Qdrant has no matches).
	searcher := &mockSearcher{
		results: []search.Result{}, // empty
	}
	embedder := &mockEmbedder{dims: 1024}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := decisions.New(testDB, embedder, searcher, logger, nil)

	results, err := svc.Search(ctx, uuid.Nil, keyword, true, model.QueryFilters{}, 10)
	require.NoError(t, err, "empty Qdrant results should fall through to text search")
	assert.NotEmpty(t, results, "text fallback should find the decision")
}

func TestSearch_EmbeddingFailureFallback(t *testing.T) {
	ctx := context.Background()
	agentID := "search-embfail-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	keyword := "embfailure-" + agentID
	_, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      keyword,
			Confidence:   0.8,
		},
	})
	require.NoError(t, err)

	// Searcher is healthy but the embedder fails, so Search should fall back.
	searcher := &mockSearcher{}
	embedder := &failingEmbedder{dims: 1024}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := decisions.New(testDB, embedder, searcher, logger, nil)

	results, err := svc.Search(ctx, uuid.Nil, keyword, true, model.QueryFilters{}, 10)
	require.NoError(t, err, "embedding failure should fall through to text search")
	assert.NotEmpty(t, results, "text fallback should find the decision")
}

// ---------------------------------------------------------------------------
// Tests: hydrateAndReScore (exercised through the Qdrant Search path)
// ---------------------------------------------------------------------------

func TestSearch_HydrateAndReScore_WithAlternativesAndEvidence(t *testing.T) {
	ctx := context.Background()
	agentID := "hydrate-qdrant-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Trace decisions with alternatives and evidence.
	reasoning1 := "first reasoning"
	reasoning2 := "second reasoning"
	result1, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      "hydrate-multi-1-" + agentID,
			Confidence:   0.9,
			Reasoning:    &reasoning1,
			Alternatives: []model.TraceAlternative{
				{Label: "Alt A", Score: ptr(float32(0.3)), Selected: false},
				{Label: "Alt B", Score: ptr(float32(0.9)), Selected: true},
			},
			Evidence: []model.TraceEvidence{
				{SourceType: "document", Content: "evidence for option B"},
			},
		},
	})
	require.NoError(t, err)

	result2, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "security",
			Outcome:      "hydrate-multi-2-" + agentID,
			Confidence:   0.6,
			Reasoning:    &reasoning2,
		},
	})
	require.NoError(t, err)

	// Mock searcher returns both decisions with different scores.
	searcher := &mockSearcher{
		results: []search.Result{
			{DecisionID: result1.DecisionID, Score: 0.95},
			{DecisionID: result2.DecisionID, Score: 0.70},
		},
	}
	embedder := &mockEmbedder{dims: 1024}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := decisions.New(testDB, embedder, searcher, logger, nil)

	results, err := svc.Search(ctx, uuid.Nil, "hydrate test", true, model.QueryFilters{}, 10)
	require.NoError(t, err)
	require.Len(t, results, 2, "should return both hydrated decisions")

	// Results should be sorted by re-scored similarity (descending).
	assert.GreaterOrEqual(t, results[0].SimilarityScore, results[1].SimilarityScore,
		"results should be sorted by re-scored similarity descending")

	// The higher-scored decision (0.95 raw) should come first.
	assert.Equal(t, result1.DecisionID, results[0].Decision.ID,
		"higher-scored decision should come first after re-scoring")
}

func TestSearch_HydrateAndReScore_LimitApplied(t *testing.T) {
	ctx := context.Background()
	agentID := "hydrate-limit-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Trace 3 decisions.
	var traceIDs []uuid.UUID
	for i := range 3 {
		res, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
			AgentID: agentID,
			Decision: model.TraceDecision{
				DecisionType: "architecture",
				Outcome:      fmt.Sprintf("hydrate-limit-%d-%s", i, agentID),
				Confidence:   0.8,
			},
		})
		require.NoError(t, err)
		traceIDs = append(traceIDs, res.DecisionID)
	}

	// Mock searcher returns all 3.
	searcher := &mockSearcher{
		results: []search.Result{
			{DecisionID: traceIDs[0], Score: 0.9},
			{DecisionID: traceIDs[1], Score: 0.8},
			{DecisionID: traceIDs[2], Score: 0.7},
		},
	}
	embedder := &mockEmbedder{dims: 1024}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := decisions.New(testDB, embedder, searcher, logger, nil)

	// Request limit=2 — hydrateAndReScore should truncate.
	results, err := svc.Search(ctx, uuid.Nil, "limit test", true, model.QueryFilters{}, 2)
	require.NoError(t, err)
	assert.Len(t, results, 2, "should respect limit=2 in hydrateAndReScore")
}

// ---------------------------------------------------------------------------
// Tests: BackfillOutcomeEmbeddings
// ---------------------------------------------------------------------------

func TestBackfillOutcomeEmbeddings_NoopSkips(t *testing.T) {
	ctx := context.Background()

	// With a NoopProvider, BackfillOutcomeEmbeddings should probe, detect the
	// noop, and return 0 without error.
	n, err := testSvc.BackfillOutcomeEmbeddings(ctx, 50)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "noop provider should short-circuit outcome backfill")
}

// ---------------------------------------------------------------------------
// Tests: BackfillEmbeddings with mock provider (exercises the real backfill path)
// ---------------------------------------------------------------------------

func TestBackfillEmbeddings_WithProvider(t *testing.T) {
	ctx := context.Background()
	agentID := "backfill-prov-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Trace decisions using testSvc (noop embedder), so they have no embeddings.
	for i := range 3 {
		_, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
			AgentID: agentID,
			Decision: model.TraceDecision{
				DecisionType: "planning",
				Outcome:      fmt.Sprintf("backfill-test-%d-%s", i, agentID),
				Confidence:   0.7,
			},
		})
		require.NoError(t, err)
	}

	// Create a service with a real (mock) embedder that returns valid vectors.
	embedder := &mockEmbedder{dims: 1024}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := decisions.New(testDB, embedder, nil, logger, nil)

	// Backfill should find unembedded decisions and embed them.
	n, err := svc.BackfillEmbeddings(ctx, 100)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 3, "should backfill at least the 3 decisions we just created")
}

func TestBackfillOutcomeEmbeddings_WithProvider(t *testing.T) {
	ctx := context.Background()

	// After TestBackfillEmbeddings_WithProvider runs, some decisions have
	// embedding but no outcome_embedding. Create a service with mock embedder
	// and backfill outcome embeddings.
	embedder := &mockEmbedder{dims: 1024}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := decisions.New(testDB, embedder, nil, logger, nil)

	n, err := svc.BackfillOutcomeEmbeddings(ctx, 100)
	require.NoError(t, err)
	// We can't assert an exact count since other tests may have created embedded
	// decisions, but we can assert it ran without error.
	assert.GreaterOrEqual(t, n, 0, "should not error when backfilling outcome embeddings")
}

// ---------------------------------------------------------------------------
// Tests: SemanticSearchAvailable
// ---------------------------------------------------------------------------

func TestSemanticSearchAvailable_WithSearcherAndRealEmbedder(t *testing.T) {
	searcher := &mockSearcher{}
	embedder := &mockEmbedder{dims: 1024}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := decisions.New(testDB, embedder, searcher, logger, nil)

	assert.True(t, svc.SemanticSearchAvailable(),
		"semantic search should be available with non-nil searcher and non-noop embedder")
}

func TestSemanticSearchAvailable_WithSearcherButNoopEmbedder(t *testing.T) {
	searcher := &mockSearcher{}
	embedder := embedding.NewNoopProvider(1024)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := decisions.New(testDB, embedder, searcher, logger, nil)

	assert.False(t, svc.SemanticSearchAvailable(),
		"semantic search should be unavailable with noop embedder even if searcher is present")
}

func TestSemanticSearchAvailable_NilSearcherRealEmbedder(t *testing.T) {
	embedder := &mockEmbedder{dims: 1024}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := decisions.New(testDB, embedder, nil, logger, nil)

	assert.False(t, svc.SemanticSearchAvailable(),
		"semantic search should be unavailable with nil searcher even if embedder is real")
}

// ---------------------------------------------------------------------------
// Tests: Search semantic=false bypasses Qdrant even with searcher configured
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Tests: BackfillClaims
// ---------------------------------------------------------------------------

func TestBackfillClaims_NoopSkips(t *testing.T) {
	ctx := context.Background()

	// With a NoopProvider, BackfillClaims should probe, detect the noop,
	// and return 0 without error (same pattern as BackfillEmbeddings).
	n, err := testSvc.BackfillClaims(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "noop provider should short-circuit claims backfill")
}

func TestBackfillClaims_WithProvider(t *testing.T) {
	ctx := context.Background()
	agentID := "bfclaims-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Create a decision with an embedding (needed for FindDecisionIDsMissingClaims)
	// and a multi-sentence outcome (needed to produce claims via SplitClaims).
	embedder := &mockEmbedder{dims: 1024}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svcWithEmbedder := decisions.New(testDB, embedder, nil, logger, nil)

	// Trace a decision — mockEmbedder provides real (non-zero) embeddings.
	result, err := svcWithEmbedder.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "code_review",
			Outcome:      "ReScore formula can exceed 1.0 when raw Qdrant similarity is high. The outbox has no deadletter mechanism. Merkle proof has a timing leak.",
			Confidence:   0.8,
		},
	})
	require.NoError(t, err)

	// Wait briefly for the async claim generation goroutine in Trace to complete.
	// Trace fires generateClaims in a goroutine, and BackfillClaims skips decisions
	// that already have claims — so we need to wait and then check.
	time.Sleep(2 * time.Second)

	// Check if claims were generated by the Trace goroutine.
	has, err := testDB.HasClaimsForDecision(ctx, result.DecisionID)
	require.NoError(t, err)

	if has {
		// Claims were generated by Trace's async goroutine. Verify they exist.
		claims, err := testDB.FindClaimsByDecision(ctx, result.DecisionID)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(claims), 2,
			"multi-sentence outcome should produce multiple claims")

		// BackfillClaims should return 0 (idempotent — claims already exist).
		n, err := svcWithEmbedder.BackfillClaims(ctx, 100)
		require.NoError(t, err)
		// Our decision already has claims, so it won't be processed again.
		// Other decisions in the test DB may or may not need claims.
		_ = n
	} else {
		// Claims weren't generated yet — BackfillClaims should process this decision.
		n, err := svcWithEmbedder.BackfillClaims(ctx, 100)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, n, 1, "should backfill claims for at least our decision")

		// Verify claims were generated.
		claims, err := testDB.FindClaimsByDecision(ctx, result.DecisionID)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(claims), 2,
			"multi-sentence outcome should produce multiple claims")
	}
}

func TestBackfillClaims_Idempotent(t *testing.T) {
	ctx := context.Background()
	agentID := "bfclaims-idem-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	embedder := &mockEmbedder{dims: 1024}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svcWithEmbedder := decisions.New(testDB, embedder, nil, logger, nil)

	// Trace a decision with a multi-sentence outcome.
	result, err := svcWithEmbedder.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      "First finding about the architecture. Second finding about performance. Third finding about security posture.",
			Confidence:   0.85,
		},
	})
	require.NoError(t, err)

	// Wait for async claim generation from Trace.
	time.Sleep(2 * time.Second)

	// Count claims after first generation.
	claims1, err := testDB.FindClaimsByDecision(ctx, result.DecisionID)
	require.NoError(t, err)
	count1 := len(claims1)

	// Run backfill — should be a no-op for this decision since claims exist.
	_, err = svcWithEmbedder.BackfillClaims(ctx, 100)
	require.NoError(t, err)

	// Count should be unchanged (no duplicate claims).
	claims2, err := testDB.FindClaimsByDecision(ctx, result.DecisionID)
	require.NoError(t, err)
	assert.Equal(t, count1, len(claims2),
		"backfill should not create duplicate claims for a decision that already has them")
}

func TestSearch_SemanticFalseSkipsQdrant(t *testing.T) {
	ctx := context.Background()
	agentID := "search-nosem-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	keyword := "nosemantic-" + agentID
	_, err := testSvc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      keyword,
			Confidence:   0.8,
		},
	})
	require.NoError(t, err)

	// Even with a healthy searcher, semantic=false should go straight to text.
	searcher := &mockSearcher{
		results: []search.Result{}, // would return empty if called
	}
	embedder := &mockEmbedder{dims: 1024}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := decisions.New(testDB, embedder, searcher, logger, nil)

	results, err := svc.Search(ctx, uuid.Nil, keyword, false, model.QueryFilters{}, 10)
	require.NoError(t, err, "semantic=false should use text search directly")
	assert.NotEmpty(t, results, "text search should find the decision")
}
