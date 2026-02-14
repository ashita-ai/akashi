package conflicts

import (
	"context"
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
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/migrations"
)

var testDB *storage.DB

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

	code := m.Run()
	testDB.Close(ctx)
	_ = container.Terminate(ctx)
	os.Exit(code)
}

func TestCosineSimilarity(t *testing.T) {
	// Identical vectors -> 1.0
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	assert.InDelta(t, 1.0, cosineSimilarity(a, b), 1e-6)

	// Orthogonal -> 0
	c := []float32{1, 0, 0}
	d := []float32{0, 1, 0}
	assert.InDelta(t, 0.0, cosineSimilarity(c, d), 1e-6)

	// Opposite -> -1
	e := []float32{1, 0, 0}
	f := []float32{-1, 0, 0}
	assert.InDelta(t, -1.0, cosineSimilarity(e, f), 1e-6)

	// 45 degrees -> ~0.707
	g := []float32{1, 0, 0}
	h := []float32{1, 1, 0}
	sim := cosineSimilarity(g, h)
	assert.InDelta(t, 0.707, sim, 0.01)

	// Empty / mismatched -> 0
	assert.InDelta(t, 0.0, cosineSimilarity([]float32{}, []float32{}), 1e-6)
	assert.InDelta(t, 0.0, cosineSimilarity([]float32{1}, []float32{1, 2}), 1e-6)

	// Zero vector -> 0
	assert.InDelta(t, 0.0, cosineSimilarity([]float32{0, 0}, []float32{1, 1}), 1e-6)
}

func TestCosineSimilarity_NilSlices(t *testing.T) {
	// nil slices should return 0 (treated as empty by len check).
	assert.InDelta(t, 0.0, cosineSimilarity(nil, nil), 1e-6)
	assert.InDelta(t, 0.0, cosineSimilarity(nil, []float32{1, 0}), 1e-6)
	assert.InDelta(t, 0.0, cosineSimilarity([]float32{1, 0}, nil), 1e-6)
}

func TestCosineSimilarity_HighDimensional(t *testing.T) {
	// Two identical high-dimensional vectors should still yield 1.0.
	dims := 1024
	a := make([]float32, dims)
	b := make([]float32, dims)
	for i := range a {
		a[i] = float32(i) * 0.001
		b[i] = float32(i) * 0.001
	}
	// a[0] and b[0] are 0, but the rest are nonzero so norm is nonzero.
	assert.InDelta(t, 1.0, cosineSimilarity(a, b), 1e-5)

	// Negate b to get -1.0.
	for i := range b {
		b[i] = -b[i]
	}
	assert.InDelta(t, -1.0, cosineSimilarity(a, b), 1e-5)
}

func TestCosineSimilarity_BothZeroVectors(t *testing.T) {
	// Two zero vectors should return 0, not NaN.
	a := []float32{0, 0, 0}
	b := []float32{0, 0, 0}
	assert.InDelta(t, 0.0, cosineSimilarity(a, b), 1e-6)
}

func TestNewScorer_DefaultThreshold(t *testing.T) {
	scorer := NewScorer(nil, slog.Default(), 0)
	assert.Equal(t, 0.30, scorer.threshold)

	scorer = NewScorer(nil, slog.Default(), -0.5)
	assert.Equal(t, 0.30, scorer.threshold)

	scorer = NewScorer(nil, slog.Default(), 0.5)
	assert.Equal(t, 0.5, scorer.threshold)
}

func TestNewScorer_NotNil(t *testing.T) {
	scorer := NewScorer(testDB, slog.Default(), 0.4)
	require.NotNil(t, scorer)
	assert.Equal(t, 0.4, scorer.threshold)
	assert.NotNil(t, scorer.db)
	assert.NotNil(t, scorer.logger)
}

func TestPtr(t *testing.T) {
	v := ptr(42)
	require.NotNil(t, v)
	assert.Equal(t, 42, *v)

	s := ptr("hello")
	require.NotNil(t, s)
	assert.Equal(t, "hello", *s)
}

func TestPtr_FloatAndBool(t *testing.T) {
	f := ptr(3.14)
	require.NotNil(t, f)
	assert.InDelta(t, 3.14, *f, 1e-9)

	b := ptr(true)
	require.NotNil(t, b)
	assert.True(t, *b)
}

// makeEmbedding creates a 1024-dim vector with value at position idx and zeroes elsewhere.
// This produces sparse, orthogonal-ish vectors for deterministic similarity testing.
func makeEmbedding(idx int, value float32) pgvector.Vector {
	v := make([]float32, 1024)
	v[idx%1024] = value
	return pgvector.NewVector(v)
}

// createRun creates an agent run for the given agent, required as a FK target for decisions.
func createRun(t *testing.T, agentID string, orgID uuid.UUID) model.AgentRun {
	t.Helper()
	run, err := testDB.CreateRun(context.Background(), model.CreateRunRequest{
		AgentID: agentID,
		OrgID:   orgID,
	})
	require.NoError(t, err)
	return run
}

func TestScoreForDecision(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	orgID := uuid.Nil

	// Create an agent for the decisions.
	suffix := uuid.New().String()[:8]
	agentID := "scorer-agent-" + suffix
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID,
		OrgID:   orgID,
		Name:    agentID,
		Role:    model.RoleAgent,
	})
	require.NoError(t, err)

	runA := createRun(t, agentID, orgID)
	runB := createRun(t, agentID, orgID)

	// Create two decisions with the same topic embedding (high topic similarity)
	// but different outcome embeddings (high outcome divergence).
	// This should produce a conflict with significance above threshold.
	topicEmb := makeEmbedding(0, 1.0)    // identical topic
	outcomeEmbA := makeEmbedding(1, 1.0) // outcome A: [0, 1, 0, ...]
	outcomeEmbB := makeEmbedding(2, 1.0) // outcome B: [0, 0, 1, ...] -- orthogonal to A
	decisionA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:            runA.ID,
		AgentID:          agentID,
		OrgID:            orgID,
		DecisionType:     "architecture",
		Outcome:          "chose Redis for caching",
		Confidence:       0.8,
		Embedding:        &topicEmb,
		OutcomeEmbedding: &outcomeEmbA,
	})
	require.NoError(t, err)

	decisionB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:            runB.ID,
		AgentID:          agentID,
		OrgID:            orgID,
		DecisionType:     "architecture",
		Outcome:          "chose Memcached for caching",
		Confidence:       0.7,
		Embedding:        &topicEmb,
		OutcomeEmbedding: &outcomeEmbB,
	})
	require.NoError(t, err)

	// Use a low threshold so the conflict is detected.
	scorer := NewScorer(testDB, logger, 0.1)

	// Score for decisionB — it should find decisionA as a conflict.
	scorer.ScoreForDecision(ctx, decisionB.ID, orgID)

	// Verify that a conflict was inserted.
	conflicts, err := testDB.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 100, 0)
	require.NoError(t, err)

	// Find the conflict involving our two decisions.
	var found bool
	for _, c := range conflicts {
		aMatches := c.DecisionAID == decisionA.ID || c.DecisionBID == decisionA.ID
		bMatches := c.DecisionAID == decisionB.ID || c.DecisionBID == decisionB.ID
		if aMatches && bMatches {
			found = true
			assert.Equal(t, model.ConflictKindSelfContradiction, c.ConflictKind,
				"same agent should produce self_contradiction")
			assert.Equal(t, "embedding", c.ScoringMethod)
			require.NotNil(t, c.TopicSimilarity)
			assert.InDelta(t, 1.0, *c.TopicSimilarity, 0.01,
				"identical topic embeddings should yield ~1.0 topic similarity")
			require.NotNil(t, c.OutcomeDivergence)
			assert.InDelta(t, 1.0, *c.OutcomeDivergence, 0.01,
				"orthogonal outcome embeddings should yield ~1.0 outcome divergence")
			require.NotNil(t, c.Significance)
			assert.Greater(t, *c.Significance, 0.1,
				"significance should exceed the scorer threshold")
			break
		}
	}
	assert.True(t, found, "expected a conflict between decisionA and decisionB")
}

func TestScoreForDecision_NoEmbeddings(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	orgID := uuid.Nil

	suffix := uuid.New().String()[:8]
	agentID := "no-emb-agent-" + suffix
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID,
		OrgID:   orgID,
		Name:    agentID,
		Role:    model.RoleAgent,
	})
	require.NoError(t, err)

	run := createRun(t, agentID, orgID)

	// Create a decision without embeddings.
	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		OrgID:        orgID,
		DecisionType: "code_review",
		Outcome:      "approved PR",
		Confidence:   0.9,
	})
	require.NoError(t, err)

	scorer := NewScorer(testDB, logger, 0.1)

	// Should return early without error (decision lacks embeddings).
	scorer.ScoreForDecision(ctx, d.ID, orgID)
	// No panic or error is the assertion; the function logs and returns.
}

func TestScoreForDecision_CrossAgent(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	orgID := uuid.Nil

	suffix := uuid.New().String()[:8]

	// Create two different agents.
	agentA := "cross-a-" + suffix
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentA, OrgID: orgID, Name: agentA, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	agentB := "cross-b-" + suffix
	_, err = testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentB, OrgID: orgID, Name: agentB, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	runA := createRun(t, agentA, orgID)
	runB := createRun(t, agentB, orgID)

	// Same topic, divergent outcomes.
	topicEmb := makeEmbedding(10, 1.0)
	outcomeA := makeEmbedding(11, 1.0)
	outcomeB := makeEmbedding(12, 1.0)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA, OrgID: orgID,
		DecisionType: "deployment", Outcome: "deploy to us-east-1", Confidence: 0.8,
		Embedding: &topicEmb, OutcomeEmbedding: &outcomeA,
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB, OrgID: orgID,
		DecisionType: "deployment", Outcome: "deploy to eu-west-1", Confidence: 0.7,
		Embedding: &topicEmb, OutcomeEmbedding: &outcomeB,
	})
	require.NoError(t, err)

	scorer := NewScorer(testDB, logger, 0.1)
	scorer.ScoreForDecision(ctx, dB.ID, orgID)

	conflicts, err := testDB.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 100, 0)
	require.NoError(t, err)

	var found bool
	for _, c := range conflicts {
		aMatch := c.DecisionAID == dA.ID || c.DecisionBID == dA.ID
		bMatch := c.DecisionAID == dB.ID || c.DecisionBID == dB.ID
		if aMatch && bMatch {
			found = true
			assert.Equal(t, model.ConflictKindCrossAgent, c.ConflictKind,
				"different agents should produce cross_agent conflict")
			break
		}
	}
	assert.True(t, found, "expected a cross-agent conflict between dA and dB")
}

func TestBackfillScoring(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	orgID := uuid.Nil

	suffix := uuid.New().String()[:8]
	agentID := "backfill-scorer-" + suffix
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: orgID, Name: agentID, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	runA := createRun(t, agentID, orgID)
	runB := createRun(t, agentID, orgID)

	// Create two decisions with identical topic but divergent outcomes.
	topicEmb := makeEmbedding(20, 1.0)
	outcomeA := makeEmbedding(21, 1.0)
	outcomeB := makeEmbedding(22, 1.0)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "use PostgreSQL for everything",
		Confidence: 0.8, Embedding: &topicEmb, OutcomeEmbedding: &outcomeA,
	})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "use MongoDB for everything",
		Confidence: 0.7, Embedding: &topicEmb, OutcomeEmbedding: &outcomeB,
	})
	require.NoError(t, err)

	scorer := NewScorer(testDB, logger, 0.1)

	// BackfillScoring should process both decisions and produce a conflict.
	processed, err := scorer.BackfillScoring(ctx, 100)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, processed, 2, "should process at least the 2 decisions we created")

	conflicts, err := testDB.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 1000, 0)
	require.NoError(t, err)

	// There should be at least one conflict between our two decisions.
	var found bool
	for _, c := range conflicts {
		if (c.OutcomeA == "use PostgreSQL for everything" && c.OutcomeB == "use MongoDB for everything") ||
			(c.OutcomeA == "use MongoDB for everything" && c.OutcomeB == "use PostgreSQL for everything") {
			found = true
			break
		}
	}
	assert.True(t, found, "backfill should produce a conflict between PostgreSQL and MongoDB decisions")
}

func TestBackfillScoring_EmptyDB(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	scorer := NewScorer(testDB, logger, 0.5)

	// Should handle gracefully when there are decisions but none with
	// significance above the high threshold.
	processed, err := scorer.BackfillScoring(ctx, 100)
	require.NoError(t, err)
	// Will process whatever exists in the test DB; just verify no error.
	_ = processed
}

func TestScoreForDecision_SkipsRevisionChain(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	orgID := uuid.Nil

	suffix := uuid.New().String()[:8]
	agentID := "rev-excl-" + suffix
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: orgID, Name: agentID, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	run := createRun(t, agentID, orgID)

	// Create decision A with embeddings (stays active — valid_to IS NULL).
	topicEmb := makeEmbedding(30, 1.0)
	outcomeEmbA := makeEmbedding(31, 1.0)
	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "code_review", Outcome: "ReScore is bounded correctly",
		Confidence: 0.8, Embedding: &topicEmb, OutcomeEmbedding: &outcomeEmbA,
	})
	require.NoError(t, err)

	// Create decision B with supersedes_id pointing to A, but via CreateDecision
	// (not ReviseDecision) so A remains active. This simulates a trace API call
	// where an agent declares it supersedes a prior decision without invalidating it.
	outcomeEmbB := makeEmbedding(32, 1.0) // orthogonal to A — would normally trigger conflict
	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "code_review", Outcome: "ReScore can exceed 1.0",
		Confidence: 0.9, Embedding: &topicEmb, OutcomeEmbedding: &outcomeEmbB,
		SupersedesID: &dA.ID,
	})
	require.NoError(t, err)

	scorer := NewScorer(testDB, logger, 0.1)
	scorer.ScoreForDecision(ctx, dB.ID, orgID)

	// Verify NO conflict was inserted between A and B despite divergent outcomes.
	conflicts, err := testDB.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 1000, 0)
	require.NoError(t, err)

	for _, c := range conflicts {
		aMatch := c.DecisionAID == dA.ID || c.DecisionBID == dA.ID
		bMatch := c.DecisionAID == dB.ID || c.DecisionBID == dB.ID
		if aMatch && bMatch {
			t.Fatalf("revision chain pair should NOT produce a conflict, but got: sig=%v method=%s",
				c.Significance, c.ScoringMethod)
		}
	}
}

func TestScoreForDecision_RevisionChainTransitive(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	orgID := uuid.Nil

	suffix := uuid.New().String()[:8]
	agentID := "rev-trans-" + suffix
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: orgID, Name: agentID, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	run := createRun(t, agentID, orgID)

	// A -> B -> C revision chain via CreateDecision (all remain active).
	topicEmb := makeEmbedding(40, 1.0)
	outcomeA := makeEmbedding(41, 1.0)
	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "use REST API",
		Confidence: 0.7, Embedding: &topicEmb, OutcomeEmbedding: &outcomeA,
	})
	require.NoError(t, err)

	outcomeB := makeEmbedding(42, 1.0)
	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "use GraphQL API",
		Confidence: 0.8, Embedding: &topicEmb, OutcomeEmbedding: &outcomeB,
		SupersedesID: &dA.ID,
	})
	require.NoError(t, err)

	outcomeC := makeEmbedding(43, 1.0)
	dC, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "use gRPC API",
		Confidence: 0.9, Embedding: &topicEmb, OutcomeEmbedding: &outcomeC,
		SupersedesID: &dB.ID,
	})
	require.NoError(t, err)

	scorer := NewScorer(testDB, logger, 0.1)

	// Score for C — should NOT conflict with A or B (transitive chain).
	scorer.ScoreForDecision(ctx, dC.ID, orgID)

	conflicts, err := testDB.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 1000, 0)
	require.NoError(t, err)

	for _, c := range conflicts {
		cMatch := c.DecisionAID == dC.ID || c.DecisionBID == dC.ID
		aMatch := c.DecisionAID == dA.ID || c.DecisionBID == dA.ID
		bMatch := c.DecisionAID == dB.ID || c.DecisionBID == dB.ID
		if cMatch && (aMatch || bMatch) {
			t.Fatalf("transitive revision chain members should NOT produce a conflict: A=%s B=%s C=%s conflict=%s<->%s",
				dA.ID, dB.ID, dC.ID, c.DecisionAID, c.DecisionBID)
		}
	}
}

func TestBackfillScoring_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cancel() // Cancel immediately.

	scorer := NewScorer(testDB, logger, 0.1)
	processed, err := scorer.BackfillScoring(ctx, 100)

	// Either processes 0 or returns context.Canceled.
	if err != nil {
		assert.ErrorIs(t, err, context.Canceled)
	}
	_ = processed
}
