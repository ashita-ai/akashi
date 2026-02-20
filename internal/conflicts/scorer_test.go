package conflicts

import (
	"context"
	"fmt"
	"log/slog"
	"math"
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
	scorer := NewScorer(nil, slog.Default(), 0, nil, 0, 0)
	assert.Equal(t, 0.30, scorer.threshold)

	scorer = NewScorer(nil, slog.Default(), -0.5, nil, 0, 0)
	assert.Equal(t, 0.30, scorer.threshold)

	scorer = NewScorer(nil, slog.Default(), 0.5, nil, 0, 0)
	assert.Equal(t, 0.5, scorer.threshold)
}

func TestNewScorer_NotNil(t *testing.T) {
	scorer := NewScorer(testDB, slog.Default(), 0.4, nil, 0, 0)
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
	scorer := NewScorer(testDB, logger, 0.1, nil, 0, 0)

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

	scorer := NewScorer(testDB, logger, 0.1, nil, 0, 0)

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

	scorer := NewScorer(testDB, logger, 0.1, nil, 0, 0)
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

	scorer := NewScorer(testDB, logger, 0.1, nil, 0, 0)

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

	scorer := NewScorer(testDB, logger, 0.5, nil, 0, 0)

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

	scorer := NewScorer(testDB, logger, 0.1, nil, 0, 0)
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

	scorer := NewScorer(testDB, logger, 0.1, nil, 0, 0)

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

	scorer := NewScorer(testDB, logger, 0.1, nil, 0, 0)
	processed, err := scorer.BackfillScoring(ctx, 100)

	// Either processes 0 or returns context.Canceled.
	if err != nil {
		assert.ErrorIs(t, err, context.Canceled)
	}
	_ = processed
}

// ---------------------------------------------------------------------------
// Claim-level scoring tests
// ---------------------------------------------------------------------------

// makeClaimVector creates a unit-length vector in a 2D subspace of R^1024.
// posA and posB identify the two dimensions used. cosSim controls the angle
// relative to the "reference" vector at (posA=1, posB=0). Returns a pair:
// the reference vector and a rotated vector with the desired cosine similarity.
func makeClaimVectorPair(posA, posB int, cosSim float64) (pgvector.Vector, pgvector.Vector) {
	ref := make([]float32, 1024)
	ref[posA%1024] = 1.0

	rotated := make([]float32, 1024)
	rotated[posA%1024] = float32(cosSim)
	rotated[posB%1024] = float32(math.Sqrt(1 - cosSim*cosSim))

	return pgvector.NewVector(ref), pgvector.NewVector(rotated)
}

func TestBestClaimConflict_AboveFloors(t *testing.T) {
	ctx := context.Background()
	orgID := uuid.Nil
	suffix := uuid.New().String()[:8]
	agentID := "claim-above-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: orgID, Name: agentID, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	run := createRun(t, agentID, orgID)

	// Create two decisions (only need IDs for claim lookup).
	topicEmb := makeEmbedding(200, 1.0)
	outcomeA := makeEmbedding(201, 1.0)
	outcomeB := makeEmbedding(202, 1.0)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "chose Redis",
		Confidence: 0.8, Embedding: &topicEmb, OutcomeEmbedding: &outcomeA,
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "chose Memcached",
		Confidence: 0.7, Embedding: &topicEmb, OutcomeEmbedding: &outcomeB,
	})
	require.NoError(t, err)

	// Insert claims with cosine similarity = 0.70 → div = 0.30.
	// Both floors satisfied: sim(0.70) >= 0.60 and div(0.30) >= 0.15.
	claimRefA, claimRotB := makeClaimVectorPair(300, 301, 0.70)

	err = testDB.InsertClaims(ctx, []storage.Claim{
		{DecisionID: dA.ID, OrgID: orgID, ClaimIdx: 0, ClaimText: "Redis has pub/sub support.", Embedding: &claimRefA},
	})
	require.NoError(t, err)

	err = testDB.InsertClaims(ctx, []storage.Claim{
		{DecisionID: dB.ID, OrgID: orgID, ClaimIdx: 0, ClaimText: "Memcached is simpler and faster.", Embedding: &claimRotB},
	})
	require.NoError(t, err)

	scorer := NewScorer(testDB, slog.Default(), 0.1, nil, 0, 0)
	sig, div, claimA, claimB := scorer.bestClaimConflict(ctx, dA.ID, dB.ID, orgID, 0.90)

	assert.Greater(t, sig, 0.0, "significance should be positive when both floors are satisfied")
	assert.InDelta(t, 0.30, div, 0.02, "divergence should be ~0.30 for cos sim 0.70")
	assert.Equal(t, "Redis has pub/sub support.", claimA)
	assert.Equal(t, "Memcached is simpler and faster.", claimB)
}

func TestBestClaimConflict_BelowSimFloor(t *testing.T) {
	ctx := context.Background()
	orgID := uuid.Nil
	suffix := uuid.New().String()[:8]
	agentID := "claim-lowsim-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: orgID, Name: agentID, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	run := createRun(t, agentID, orgID)
	topicEmb := makeEmbedding(210, 1.0)
	outcomeA := makeEmbedding(211, 1.0)
	outcomeB := makeEmbedding(212, 1.0)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "unrelated A",
		Confidence: 0.8, Embedding: &topicEmb, OutcomeEmbedding: &outcomeA,
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "unrelated B",
		Confidence: 0.7, Embedding: &topicEmb, OutcomeEmbedding: &outcomeB,
	})
	require.NoError(t, err)

	// Cosine similarity = 0.50 → below claimTopicSimFloor (0.60).
	// These claims are about different things and should not constitute a conflict.
	claimRefA, claimRotB := makeClaimVectorPair(310, 311, 0.50)

	err = testDB.InsertClaims(ctx, []storage.Claim{
		{DecisionID: dA.ID, OrgID: orgID, ClaimIdx: 0, ClaimText: "Claim about database design.", Embedding: &claimRefA},
	})
	require.NoError(t, err)

	err = testDB.InsertClaims(ctx, []storage.Claim{
		{DecisionID: dB.ID, OrgID: orgID, ClaimIdx: 0, ClaimText: "Claim about UI framework.", Embedding: &claimRotB},
	})
	require.NoError(t, err)

	scorer := NewScorer(testDB, slog.Default(), 0.1, nil, 0, 0)
	sig, div, claimA, claimB := scorer.bestClaimConflict(ctx, dA.ID, dB.ID, orgID, 0.90)

	assert.Equal(t, 0.0, sig, "significance should be 0 when claim sim is below floor")
	assert.Equal(t, 0.0, div, "divergence should be 0 when no qualifying pairs exist")
	assert.Empty(t, claimA)
	assert.Empty(t, claimB)
}

func TestBestClaimConflict_BelowDivFloor(t *testing.T) {
	ctx := context.Background()
	orgID := uuid.Nil
	suffix := uuid.New().String()[:8]
	agentID := "claim-lowdiv-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: orgID, Name: agentID, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	run := createRun(t, agentID, orgID)
	topicEmb := makeEmbedding(220, 1.0)
	outcomeA := makeEmbedding(221, 1.0)
	outcomeB := makeEmbedding(222, 1.0)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "nearly identical A",
		Confidence: 0.8, Embedding: &topicEmb, OutcomeEmbedding: &outcomeA,
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "nearly identical B",
		Confidence: 0.7, Embedding: &topicEmb, OutcomeEmbedding: &outcomeB,
	})
	require.NoError(t, err)

	// Cosine similarity = 0.90 → div = 0.10 → below claimDivFloor (0.15).
	// Claims are about the same thing AND effectively agree — not a conflict.
	claimRefA, claimRotB := makeClaimVectorPair(320, 321, 0.90)

	err = testDB.InsertClaims(ctx, []storage.Claim{
		{DecisionID: dA.ID, OrgID: orgID, ClaimIdx: 0, ClaimText: "PostgreSQL handles ACID transactions.", Embedding: &claimRefA},
	})
	require.NoError(t, err)

	err = testDB.InsertClaims(ctx, []storage.Claim{
		{DecisionID: dB.ID, OrgID: orgID, ClaimIdx: 0, ClaimText: "PostgreSQL supports ACID compliance.", Embedding: &claimRotB},
	})
	require.NoError(t, err)

	scorer := NewScorer(testDB, slog.Default(), 0.1, nil, 0, 0)
	sig, div, claimA, claimB := scorer.bestClaimConflict(ctx, dA.ID, dB.ID, orgID, 0.90)

	assert.Equal(t, 0.0, sig, "significance should be 0 when claims effectively agree (div < floor)")
	assert.Equal(t, 0.0, div)
	assert.Empty(t, claimA)
	assert.Empty(t, claimB)
}

func TestBestClaimConflict_NoClaims(t *testing.T) {
	ctx := context.Background()
	orgID := uuid.Nil
	suffix := uuid.New().String()[:8]
	agentID := "claim-none-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: orgID, Name: agentID, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	run := createRun(t, agentID, orgID)
	topicEmb := makeEmbedding(230, 1.0)
	outcomeA := makeEmbedding(231, 1.0)
	outcomeB := makeEmbedding(232, 1.0)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "no claims A",
		Confidence: 0.8, Embedding: &topicEmb, OutcomeEmbedding: &outcomeA,
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "no claims B",
		Confidence: 0.7, Embedding: &topicEmb, OutcomeEmbedding: &outcomeB,
	})
	require.NoError(t, err)

	// No claims inserted for either decision.
	scorer := NewScorer(testDB, slog.Default(), 0.1, nil, 0, 0)
	sig, div, claimA, claimB := scorer.bestClaimConflict(ctx, dA.ID, dB.ID, orgID, 0.90)

	assert.Equal(t, 0.0, sig, "no claims means no claim-level conflict")
	assert.Equal(t, 0.0, div)
	assert.Empty(t, claimA)
	assert.Empty(t, claimB)
}

func TestBestClaimConflict_MultiplePairs_ReturnsBest(t *testing.T) {
	ctx := context.Background()
	orgID := uuid.Nil
	suffix := uuid.New().String()[:8]
	agentID := "claim-multi-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: orgID, Name: agentID, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	run := createRun(t, agentID, orgID)
	topicEmb := makeEmbedding(240, 1.0)
	outcomeA := makeEmbedding(241, 1.0)
	outcomeB := makeEmbedding(242, 1.0)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "code_review", Outcome: "multi-claim review A",
		Confidence: 0.8, Embedding: &topicEmb, OutcomeEmbedding: &outcomeA,
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "code_review", Outcome: "multi-claim review B",
		Confidence: 0.7, Embedding: &topicEmb, OutcomeEmbedding: &outcomeB,
	})
	require.NoError(t, err)

	// Claim pair 1: cos = 0.65 → div = 0.35 (moderate conflict)
	claimRef1, claimRot1 := makeClaimVectorPair(400, 401, 0.65)
	// Claim pair 2: cos = 0.70 → div = 0.30 (weaker conflict)
	claimRef2, claimRot2 := makeClaimVectorPair(402, 403, 0.70)

	err = testDB.InsertClaims(ctx, []storage.Claim{
		{DecisionID: dA.ID, OrgID: orgID, ClaimIdx: 0, ClaimText: "ReScore can exceed 1.0.", Embedding: &claimRef1},
		{DecisionID: dA.ID, OrgID: orgID, ClaimIdx: 1, ClaimText: "Buffer flush is safe.", Embedding: &claimRef2},
	})
	require.NoError(t, err)

	err = testDB.InsertClaims(ctx, []storage.Claim{
		{DecisionID: dB.ID, OrgID: orgID, ClaimIdx: 0, ClaimText: "ReScore is bounded within [0,1].", Embedding: &claimRot1},
		{DecisionID: dB.ID, OrgID: orgID, ClaimIdx: 1, ClaimText: "Buffer flush is mostly safe.", Embedding: &claimRot2},
	})
	require.NoError(t, err)

	scorer := NewScorer(testDB, slog.Default(), 0.1, nil, 0, 0)
	sig, div, claimA, claimB := scorer.bestClaimConflict(ctx, dA.ID, dB.ID, orgID, 0.90)

	assert.Greater(t, sig, 0.0, "should find a claim-level conflict")
	// The pair with higher divergence (0.35) should be selected as best.
	assert.InDelta(t, 0.35, div, 0.02, "best pair should have div ~0.35")
	assert.Equal(t, "ReScore can exceed 1.0.", claimA)
	assert.Equal(t, "ReScore is bounded within [0,1].", claimB)
}

func TestScoreForDecision_ClaimMethodWinsOverEmbedding(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	orgID := uuid.Nil
	suffix := uuid.New().String()[:8]
	agentID := "claim-wins-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: orgID, Name: agentID, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	runA := createRun(t, agentID, orgID)
	runB := createRun(t, agentID, orgID)

	// High topic similarity (identical topic embeddings).
	topicEmb := makeEmbedding(250, 1.0)

	// VERY similar full-outcome embeddings (low outcome divergence for pass 1).
	// This makes the embedding-level significance low.
	outcomeA := make([]float32, 1024)
	outcomeA[251] = 1.0
	outcomeA[252] = 0.1
	outcomeB := make([]float32, 1024)
	outcomeB[251] = 1.0
	outcomeB[252] = 0.15
	outcomeEmbA := pgvector.NewVector(outcomeA)
	outcomeEmbB := pgvector.NewVector(outcomeB)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "code_review",
		Outcome:      "Multi-topic review with a specific ReScore finding. Overall looks good.",
		Confidence:   0.8, Embedding: &topicEmb, OutcomeEmbedding: &outcomeEmbA,
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "code_review",
		Outcome:      "Multi-topic review with opposite ReScore finding. Overall looks good.",
		Confidence:   0.7, Embedding: &topicEmb, OutcomeEmbedding: &outcomeEmbB,
	})
	require.NoError(t, err)

	// Insert claims with a strong specific disagreement (cos = 0.65 → div = 0.35).
	claimRef, claimRot := makeClaimVectorPair(500, 501, 0.65)

	err = testDB.InsertClaims(ctx, []storage.Claim{
		{DecisionID: dA.ID, OrgID: orgID, ClaimIdx: 0, ClaimText: "ReScore formula can exceed 1.0 bounds.", Embedding: &claimRef},
	})
	require.NoError(t, err)

	err = testDB.InsertClaims(ctx, []storage.Claim{
		{DecisionID: dB.ID, OrgID: orgID, ClaimIdx: 0, ClaimText: "ReScore is correctly bounded within [0,1].", Embedding: &claimRot},
	})
	require.NoError(t, err)

	scorer := NewScorer(testDB, logger, 0.1, nil, 0, 0)
	scorer.ScoreForDecision(ctx, dB.ID, orgID)

	// Find the conflict between our two decisions.
	conflicts, err := testDB.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 1000, 0)
	require.NoError(t, err)

	var found bool
	for _, c := range conflicts {
		aMatch := c.DecisionAID == dA.ID || c.DecisionBID == dA.ID
		bMatch := c.DecisionAID == dB.ID || c.DecisionBID == dB.ID
		if aMatch && bMatch {
			found = true
			assert.Equal(t, "claim", c.ScoringMethod,
				"claim method should win when it produces higher significance than embedding")
			// Verify the outcomes are the claim texts, not the full outcomes.
			assert.Contains(t, c.OutcomeA, "ReScore",
				"claim-level conflict should use claim text as outcome")
			assert.Contains(t, c.OutcomeB, "ReScore",
				"claim-level conflict should use claim text as outcome")
			break
		}
	}
	assert.True(t, found, "expected a claim-level conflict between dA and dB")
}

// ---------------------------------------------------------------------------
// Pair cache tests
// ---------------------------------------------------------------------------

func TestNormalizePair_Canonical(t *testing.T) {
	a := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	b := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	pair1 := normalizePair(a, b)
	pair2 := normalizePair(b, a)
	assert.Equal(t, pair1, pair2, "normalizePair should return the same order regardless of input order")
	assert.Equal(t, a, pair1[0], "smaller UUID should be first")
	assert.Equal(t, b, pair1[1], "larger UUID should be second")
}

func TestNormalizePair_Equal(t *testing.T) {
	a := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	pair := normalizePair(a, a)
	assert.Equal(t, a, pair[0])
	assert.Equal(t, a, pair[1])
}

func TestPairCache_CheckAndMark(t *testing.T) {
	cache := &pairCache{seen: make(map[[2]uuid.UUID]bool)}
	a := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	b := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	// First check: not seen.
	assert.False(t, cache.checkAndMark(a, b), "first check should return false (not seen)")
	// Second check: already seen.
	assert.True(t, cache.checkAndMark(a, b), "second check should return true (already seen)")
	// Reversed order: still seen (canonical ordering).
	assert.True(t, cache.checkAndMark(b, a), "reversed order should still be seen")
}

func TestPairCache_DifferentPairs(t *testing.T) {
	cache := &pairCache{seen: make(map[[2]uuid.UUID]bool)}
	a := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	b := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	c := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")

	cache.checkAndMark(a, b)
	// (a, c) is a different pair — should not be seen.
	assert.False(t, cache.checkAndMark(a, c))
	// (b, c) is also different.
	assert.False(t, cache.checkAndMark(b, c))
}

func TestNewScorer_DefaultWorkers(t *testing.T) {
	scorer := NewScorer(nil, slog.Default(), 0.3, nil, 0, 0)
	assert.Equal(t, 4, scorer.backfillWorkers, "0 should default to 4")

	scorer = NewScorer(nil, slog.Default(), 0.3, nil, -1, 0)
	assert.Equal(t, 4, scorer.backfillWorkers, "-1 should default to 4")

	scorer = NewScorer(nil, slog.Default(), 0.3, nil, 8, 0)
	assert.Equal(t, 8, scorer.backfillWorkers, "explicit value should be respected")
}

func TestBackfillScoring_MarksDecisionsScored(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	orgID := uuid.Nil

	suffix := uuid.New().String()[:8]
	agentID := "backfill-mark-" + suffix
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: orgID, Name: agentID, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	runA := createRun(t, agentID, orgID)
	runB := createRun(t, agentID, orgID)

	topicEmb := makeEmbedding(700, 1.0)
	outcomeA := makeEmbedding(701, 1.0)
	outcomeB := makeEmbedding(702, 1.0)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "use event sourcing",
		Confidence: 0.8, Embedding: &topicEmb, OutcomeEmbedding: &outcomeA,
	})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentID, OrgID: orgID,
		DecisionType: "architecture", Outcome: "use CRUD",
		Confidence: 0.7, Embedding: &topicEmb, OutcomeEmbedding: &outcomeB,
	})
	require.NoError(t, err)

	scorer := NewScorer(testDB, logger, 0.1, nil, 2, 0)

	// First backfill should process decisions.
	n1, err := scorer.BackfillScoring(ctx, 500)
	require.NoError(t, err)
	assert.Greater(t, n1, 0, "first backfill should process decisions")

	// Second backfill should find no unscored decisions (all marked).
	n2, err := scorer.BackfillScoring(ctx, 500)
	require.NoError(t, err)
	assert.Equal(t, 0, n2, "second backfill should find no unscored decisions")
}

// ---------------------------------------------------------------------------
// Helper function tests
// ---------------------------------------------------------------------------

func TestAgentContextString(t *testing.T) {
	// nil map.
	assert.Equal(t, "", agentContextString(nil, "repo"))

	// Missing key.
	m := map[string]any{"tool": "claude-code"}
	assert.Equal(t, "", agentContextString(m, "repo"))

	// Wrong type (not a string).
	m2 := map[string]any{"repo": 42}
	assert.Equal(t, "", agentContextString(m2, "repo"))

	// Valid string value.
	m3 := map[string]any{"repo": "ashita-ai/akashi", "tool": "claude-code"}
	assert.Equal(t, "ashita-ai/akashi", agentContextString(m3, "repo"))
	assert.Equal(t, "claude-code", agentContextString(m3, "tool"))
}

func TestUuidString(t *testing.T) {
	// nil pointer.
	assert.Equal(t, "", uuidString(nil))

	// Valid UUID.
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", uuidString(&id))
}

func TestDerefString(t *testing.T) {
	// nil pointer.
	assert.Equal(t, "", derefString(nil))

	// Valid pointer.
	s := "hello"
	assert.Equal(t, "hello", derefString(&s))

	// Empty string pointer.
	empty := ""
	assert.Equal(t, "", derefString(&empty))
}

func TestBackfillScoring_Parallel(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	orgID := uuid.Nil

	suffix := uuid.New().String()[:8]
	agentID := "backfill-parallel-" + suffix
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: orgID, Name: agentID, Role: model.RoleAgent,
	})
	require.NoError(t, err)

	// Create several decisions so parallelism actually exercises multiple goroutines.
	for i := 0; i < 6; i++ {
		run := createRun(t, agentID, orgID)
		topicEmb := makeEmbedding(800+i*3, 1.0)
		outcomeEmb := makeEmbedding(801+i*3, 1.0)
		_, err := testDB.CreateDecision(ctx, model.Decision{
			RunID: run.ID, AgentID: agentID, OrgID: orgID,
			DecisionType: "architecture",
			Outcome:      fmt.Sprintf("parallel decision %d", i),
			Confidence:   0.8,
			Embedding:    &topicEmb, OutcomeEmbedding: &outcomeEmb,
		})
		require.NoError(t, err)
	}

	// Use 3 workers to exercise parallel paths.
	scorer := NewScorer(testDB, logger, 0.1, nil, 3, 0)
	processed, err := scorer.BackfillScoring(ctx, 500)
	require.NoError(t, err)
	assert.Greater(t, processed, 0, "parallel backfill should process decisions")

	// Verify all were marked scored (second run returns 0).
	n2, err := scorer.BackfillScoring(ctx, 500)
	require.NoError(t, err)
	assert.Equal(t, 0, n2, "all decisions should be marked scored after parallel backfill")
}

// TestScoreForDecision_DifferentReposSuppressConflict verifies that two decisions
// with identical topic embeddings and orthogonal outcome embeddings do NOT produce
// a conflict when they belong to different repos. This prevents cross-project false
// positives when multiple codebases share an org (e.g. reviewers working on
// "project-alpha" and "project-beta" in the same Akashi org).
func TestScoreForDecision_DifferentReposSuppressConflict(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	orgID := uuid.Nil

	suffix := uuid.New().String()[:8]
	agentA := "repo-filter-a-" + suffix
	agentB := "repo-filter-b-" + suffix
	for _, ag := range []string{agentA, agentB} {
		_, err := testDB.CreateAgent(ctx, model.Agent{
			AgentID: ag, OrgID: orgID, Name: ag, Role: model.RoleAgent,
		})
		require.NoError(t, err)
	}

	runA := createRun(t, agentA, orgID)
	runB := createRun(t, agentB, orgID)

	// Identical topic embeddings and orthogonal outcome embeddings: without the repo
	// filter these would produce a high-significance conflict.
	topicEmb := makeEmbedding(500, 1.0)
	outcomeEmbA := makeEmbedding(501, 1.0)
	outcomeEmbB := makeEmbedding(502, 1.0) // orthogonal to A

	// agent_context->>'repo' drives the generated `repo` column (migration 048).
	decisionA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA, OrgID: orgID,
		DecisionType: "architecture",
		Outcome:      "chose Redis for caching — project alpha",
		Confidence:   0.8,
		Embedding:    &topicEmb, OutcomeEmbedding: &outcomeEmbA,
		AgentContext: map[string]any{"repo": "project-alpha"},
	})
	require.NoError(t, err)

	decisionB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB, OrgID: orgID,
		DecisionType: "architecture",
		Outcome:      "chose Memcached for caching — project beta",
		Confidence:   0.7,
		Embedding:    &topicEmb, OutcomeEmbedding: &outcomeEmbB,
		AgentContext: map[string]any{"repo": "project-beta"},
	})
	require.NoError(t, err)

	scorer := NewScorer(testDB, logger, 0.1, nil, 0, 0)
	scorer.ScoreForDecision(ctx, decisionA.ID, orgID)

	// Verify that NO conflict was created between the two decisions. The repo filter
	// should have excluded decisionB from the candidate set entirely.
	conflicts, err := testDB.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 500, 0)
	require.NoError(t, err)

	for _, c := range conflicts {
		aInvolved := c.DecisionAID == decisionA.ID || c.DecisionBID == decisionA.ID
		bInvolved := c.DecisionAID == decisionB.ID || c.DecisionBID == decisionB.ID
		assert.False(t, aInvolved && bInvolved,
			"cross-repo decisions should not produce a conflict (got one between %s and %s)",
			decisionA.ID, decisionB.ID)
	}
}

// TestScoreForDecision_SameRepoAllowsConflict confirms the positive case: decisions
// from the same repo with conflicting embeddings DO produce a conflict. This guards
// against accidentally over-filtering when the repo filter is active.
func TestScoreForDecision_SameRepoAllowsConflict(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	orgID := uuid.Nil

	suffix := uuid.New().String()[:8]
	agentA := "same-repo-a-" + suffix
	agentB := "same-repo-b-" + suffix
	for _, ag := range []string{agentA, agentB} {
		_, err := testDB.CreateAgent(ctx, model.Agent{
			AgentID: ag, OrgID: orgID, Name: ag, Role: model.RoleAgent,
		})
		require.NoError(t, err)
	}

	runA := createRun(t, agentA, orgID)
	runB := createRun(t, agentB, orgID)

	topicEmb := makeEmbedding(510, 1.0)
	outcomeEmbA := makeEmbedding(511, 1.0)
	outcomeEmbB := makeEmbedding(512, 1.0) // orthogonal to A

	decisionA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA, OrgID: orgID,
		DecisionType: "architecture",
		Outcome:      "chose Redis — shared repo",
		Confidence:   0.8,
		Embedding:    &topicEmb, OutcomeEmbedding: &outcomeEmbA,
		AgentContext: map[string]any{"repo": "shared-project"},
	})
	require.NoError(t, err)

	decisionB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB, OrgID: orgID,
		DecisionType: "architecture",
		Outcome:      "chose Memcached — shared repo",
		Confidence:   0.7,
		Embedding:    &topicEmb, OutcomeEmbedding: &outcomeEmbB,
		AgentContext: map[string]any{"repo": "shared-project"},
	})
	require.NoError(t, err)

	scorer := NewScorer(testDB, logger, 0.1, nil, 0, 0)
	scorer.ScoreForDecision(ctx, decisionA.ID, orgID)

	conflicts, err := testDB.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 500, 0)
	require.NoError(t, err)

	var found bool
	for _, c := range conflicts {
		aInvolved := c.DecisionAID == decisionA.ID || c.DecisionBID == decisionA.ID
		bInvolved := c.DecisionAID == decisionB.ID || c.DecisionBID == decisionB.ID
		if aInvolved && bInvolved {
			found = true
			break
		}
	}
	assert.True(t, found, "same-repo decisions with conflicting embeddings should produce a conflict")
}
