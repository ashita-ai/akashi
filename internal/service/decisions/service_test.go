package decisions_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/storage"
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

	if err := testDB.RunMigrations(ctx, os.DirFS("../../../migrations")); err != nil {
		fmt.Fprintf(os.Stderr, "migrations failed: %v\n", err)
		os.Exit(1)
	}

	if err := testDB.EnsureDefaultOrg(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ensure default org: %v\n", err)
		os.Exit(1)
	}

	embedder := embedding.NewNoopProvider(1024)
	testSvc = decisions.New(testDB, embedder, nil, logger)

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
