//go:build !lite

package conflicts

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// ---------------------------------------------------------------------------
// Synthetic dataset for conflict detection precision/recall measurement.
//
// Three categories, 100 decision pairs each:
//
// 1. genuine — same topic, opposite outcomes. High topic similarity,
//    high outcome divergence. The scorer SHOULD detect these.
//
// 2. related_not_contradicting — same topic, very similar outcomes (paraphrase).
//    High topic similarity, low outcome divergence. The scorer should NOT detect.
//
// 3. unrelated_false_positive — different topics, different outcomes.
//    Low topic similarity. The scorer should NOT detect.
//
// Embedding design:
//   - 1024-dim vectors. Each pair uses a unique 2D subspace of R^1024.
//   - Topic embeddings: both decisions share the same unit vector for genuine
//     and related_not_contradicting; unrelated pairs use orthogonal topic vectors.
//   - Outcome embeddings: genuine pairs are orthogonal within the subspace;
//     related pairs have cosine similarity ~0.95; unrelated pairs are arbitrary.
// ---------------------------------------------------------------------------

const (
	pairsPerCategory = 100
	// Embedding dimension offsets per category to avoid cross-contamination.
	// Each pair uses 4 dimensions: 2 for topic, 2 for outcome subspace.
	genuineBase   = 0   // dims [0, 399]
	relatedBase   = 400 // dims [400, 799]
	unrelatedBase = 800 // dims [800, 1023] — wraps via mod 1024
)

type syntheticPair struct {
	label    string // "genuine", "related_not_contradicting", "unrelated_false_positive"
	outcomeA string
	outcomeB string
	// Embeddings set during creation
	topicEmbA   pgvector.Vector
	topicEmbB   pgvector.Vector
	outcomeEmbA pgvector.Vector
	outcomeEmbB pgvector.Vector
}

// makeUnitVector creates a 1024-dim vector with value 1.0 at position idx.
func makeUnitVector(idx int) pgvector.Vector {
	v := make([]float32, 1024)
	v[idx%1024] = 1.0
	return pgvector.NewVector(v)
}

// makeRotatedVector creates a 1024-dim vector in the 2D subspace (posA, posB)
// with a given cosine similarity to the reference vector at posA.
func makeRotatedVector(posA, posB int, cosSim float64) pgvector.Vector {
	v := make([]float32, 1024)
	v[posA%1024] = float32(cosSim)
	v[posB%1024] = float32(math.Sqrt(1 - cosSim*cosSim))
	return pgvector.NewVector(v)
}

func generateSyntheticDataset() []syntheticPair {
	pairs := make([]syntheticPair, 0, 3*pairsPerCategory)

	// --- Genuine conflicts: same topic, orthogonal outcomes ---
	genuineTemplates := [][2]string{
		{"chose Redis for caching", "chose Memcached for caching"},
		{"use PostgreSQL as primary store", "use MongoDB as primary store"},
		{"deploy to us-east-1", "deploy to eu-west-1"},
		{"adopt microservices architecture", "start with a monolith"},
		{"use REST API for inter-service communication", "use gRPC for inter-service communication"},
		{"store sessions in Redis", "store sessions in database"},
		{"use JWT tokens for auth", "use session cookies for auth"},
		{"implement CQRS pattern", "keep single read-write model"},
		{"use Kafka for event streaming", "use RabbitMQ for message queuing"},
		{"deploy on Kubernetes", "deploy on bare metal with systemd"},
	}
	for i := 0; i < pairsPerCategory; i++ {
		tmpl := genuineTemplates[i%len(genuineTemplates)] //nolint:gosec // bounded by modulo
		outA, outB := tmpl[0], tmpl[1]                    //nolint:gosec // [2]string is fixed-size
		topicDim := genuineBase + i*4
		outDimA := topicDim + 1
		outDimB := topicDim + 2

		topicVec := makeUnitVector(topicDim)
		pairs = append(pairs, syntheticPair{
			label:       "genuine",
			outcomeA:    fmt.Sprintf("[G%03d] %s", i, outA),
			outcomeB:    fmt.Sprintf("[G%03d] %s", i, outB),
			topicEmbA:   topicVec,
			topicEmbB:   topicVec,
			outcomeEmbA: makeUnitVector(outDimA), // orthogonal to B
			outcomeEmbB: makeUnitVector(outDimB), // cos sim = 0.0
		})
	}

	// --- Related but not contradicting: same topic, near-identical outcomes ---
	relatedTemplates := [][2]string{
		{"added Redis caching layer", "implemented Redis-based cache"},
		{"added PostgreSQL indexes for query performance", "created database indexes to speed up queries"},
		{"set Redis TTL to 5 minutes", "configured Redis TTL to 300 seconds"},
		{"deployed service to production", "rolled out service to prod environment"},
		{"added rate limiting at 100 req/s", "implemented rate limit of 100 requests per second"},
		{"enabled TLS 1.3 for all endpoints", "configured TLS v1.3 across services"},
		{"set connection pool size to 20", "configured pool with 20 connections"},
		{"added health check endpoint", "implemented /health readiness probe"},
		{"enabled gzip compression for responses", "turned on gzip for HTTP responses"},
		{"added request ID middleware", "implemented request tracing via middleware"},
	}
	for i := 0; i < pairsPerCategory; i++ {
		tmpl := relatedTemplates[i%len(relatedTemplates)] //nolint:gosec // bounded by modulo
		outA, outB := tmpl[0], tmpl[1]                    //nolint:gosec // [2]string is fixed-size
		topicDim := relatedBase + i*4
		outDimA := topicDim + 1
		outDimB := topicDim + 2

		topicVec := makeUnitVector(topicDim)
		pairs = append(pairs, syntheticPair{
			label:       "related_not_contradicting",
			outcomeA:    fmt.Sprintf("[R%03d] %s", i, outA),
			outcomeB:    fmt.Sprintf("[R%03d] %s", i, outB),
			topicEmbA:   topicVec,
			topicEmbB:   topicVec,
			outcomeEmbA: makeUnitVector(outDimA),                   // reference
			outcomeEmbB: makeRotatedVector(outDimA, outDimB, 0.95), // cos sim ~0.95 → div ~0.05
		})
	}

	// --- Unrelated false positives: different topics, different outcomes ---
	unrelatedTemplates := [][2]string{
		{"added Redis caching", "added PostgreSQL indexes"},
		{"chose React for frontend", "configured Nginx reverse proxy"},
		{"implemented user authentication", "added CSV export feature"},
		{"set up CI/CD pipeline", "designed database schema"},
		{"added logging middleware", "implemented payment processing"},
		{"configured DNS records", "wrote unit tests for auth module"},
		{"set up monitoring alerts", "implemented search functionality"},
		{"added Docker support", "designed API rate limiting"},
		{"configured load balancer", "implemented email notifications"},
		{"added WebSocket support", "set up database replication"},
	}
	for i := 0; i < pairsPerCategory; i++ {
		tmpl := unrelatedTemplates[i%len(unrelatedTemplates)] //nolint:gosec // bounded by modulo
		outA, outB := tmpl[0], tmpl[1]                        //nolint:gosec // [2]string is fixed-size
		topicDimA := unrelatedBase + i*2
		topicDimB := unrelatedBase + i*2 + 1
		// Orthogonal topic vectors → topic_sim ≈ 0
		// Outcome embeddings reuse the topic dims (orthogonal to each other)

		pairs = append(pairs, syntheticPair{
			label:       "unrelated_false_positive",
			outcomeA:    fmt.Sprintf("[U%03d] %s", i, outA),
			outcomeB:    fmt.Sprintf("[U%03d] %s", i, outB),
			topicEmbA:   makeUnitVector(topicDimA),
			topicEmbB:   makeUnitVector(topicDimB),
			outcomeEmbA: makeUnitVector(topicDimA), // same as topic (orthogonal to B)
			outcomeEmbB: makeUnitVector(topicDimB),
		})
	}

	return pairs
}

// TestScorerPrecisionRecall is an integration test that inserts 300 synthetic
// decision pairs into a real TimescaleDB, runs the embedding-only scorer
// (NoopValidator), and logs precision/recall metrics.
//
// Skipped by default — set AKASHI_BENCH=1 to run. This tests scorer math
// with synthetic embeddings; for real evaluation against production data,
// use the scorer-eval API endpoint or: go run ./cmd/eval-conflicts --mode=scorer
func TestScorerPrecisionRecall(t *testing.T) {
	if os.Getenv("AKASHI_BENCH") == "" {
		t.Skip("set AKASHI_BENCH=1 to run synthetic scorer benchmark")
	}
	if testDB == nil {
		t.Skip("testDB not initialized (requires testcontainers)")
	}

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Use a dedicated org to isolate benchmark data from other tests sharing testDB.
	orgID := uuid.New()
	_, err := testDB.Pool().Exec(ctx,
		`INSERT INTO organizations (id, name, slug, plan, created_at, updated_at)
		 VALUES ($1, 'benchmark', 'benchmark', 'oss', NOW(), NOW())`, orgID)
	require.NoError(t, err)

	suffix := uuid.New().String()[:8]
	dataset := generateSyntheticDataset()

	// Create two agents: one per side of each pair, to get cross_agent conflicts.
	agentA := "bench-a-" + suffix
	agentB := "bench-b-" + suffix
	for _, aid := range []string{agentA, agentB} {
		_, err := testDB.CreateAgent(ctx, model.Agent{
			AgentID: aid, OrgID: orgID, Name: aid, Role: model.RoleAgent,
		})
		require.NoError(t, err)
	}

	runA := createRun(t, agentA, orgID)
	runB := createRun(t, agentB, orgID)

	// Insert all decision pairs.
	type decisionPairIDs struct {
		aID   uuid.UUID
		bID   uuid.UUID
		label string
	}
	pairIDs := make([]decisionPairIDs, 0, len(dataset))

	for _, sp := range dataset {
		dA, err := testDB.CreateDecision(ctx, model.Decision{
			RunID: runA.ID, AgentID: agentA, OrgID: orgID,
			DecisionType: "architecture",
			Outcome:      sp.outcomeA,
			Confidence:   0.8,
			Embedding:    &sp.topicEmbA, OutcomeEmbedding: &sp.outcomeEmbA,
		})
		require.NoError(t, err)

		dB, err := testDB.CreateDecision(ctx, model.Decision{
			RunID: runB.ID, AgentID: agentB, OrgID: orgID,
			DecisionType: "architecture",
			Outcome:      sp.outcomeB,
			Confidence:   0.8,
			Embedding:    &sp.topicEmbB, OutcomeEmbedding: &sp.outcomeEmbB,
		})
		require.NoError(t, err)

		pairIDs = append(pairIDs, decisionPairIDs{aID: dA.ID, bID: dB.ID, label: sp.label})
	}

	// Run scorer on all B-side decisions (each finds its A-side candidate).
	// Use low threshold and disabled early exit to let all candidates through.
	scorer := NewScorer(testDB, logger, 0.05, nil, 0, 0).
		WithCandidateFinder(storage.NewPgCandidateFinder(testDB)).
		WithEarlyExitFloor(0.01)

	for _, p := range pairIDs {
		scorer.ScoreForDecision(ctx, p.bID, orgID)
	}

	// Collect all detected conflicts.
	allConflicts, err := testDB.ListConflicts(ctx, orgID, storage.ConflictFilters{}, 1000, 0)
	require.NoError(t, err)

	// Build a lookup set of detected pairs (normalized: smaller UUID first).
	type uuidPair [2]uuid.UUID
	detected := make(map[uuidPair]bool)
	for _, c := range allConflicts {
		a, b := c.DecisionAID, c.DecisionBID
		if a.String() > b.String() {
			a, b = b, a
		}
		detected[uuidPair{a, b}] = true
	}

	// Evaluate each synthetic pair.
	results := make([]ScorerEvalResult, 0, len(pairIDs))
	for i, p := range pairIDs {
		a, b := p.aID, p.bID
		if a.String() > b.String() {
			a, b = b, a
		}
		results = append(results, ScorerEvalResult{
			DecisionAOutcome: dataset[i].outcomeA,
			DecisionBOutcome: dataset[i].outcomeB,
			ExpectedLabel:    p.label,
			Detected:         detected[uuidPair{a, b}],
		})
	}

	pr := ComputePrecisionRecall(results)

	t.Logf("Precision: %.3f (TP=%d, FP=%d)", pr.Precision, pr.TruePositives, pr.FalsePositives)
	t.Logf("Recall:    %.3f (TP=%d, FN=%d)", pr.Recall, pr.TruePositives, pr.FalseNegatives)
	t.Logf("F1:        %.3f", pr.F1)

	// Log misclassifications for debugging.
	for _, r := range results {
		if r.ExpectedLabel == "genuine" && !r.Detected {
			t.Logf("MISSED genuine: %q vs %q", r.DecisionAOutcome, r.DecisionBOutcome)
		}
		if r.ExpectedLabel != "genuine" && r.Detected {
			t.Logf("FALSE POSITIVE (%s): %q vs %q", r.ExpectedLabel, r.DecisionAOutcome, r.DecisionBOutcome)
		}
	}

	// CI gates.
	assert.GreaterOrEqual(t, pr.Precision, 0.80,
		"precision must be >= 0.80 (got %.3f)", pr.Precision)
	assert.GreaterOrEqual(t, pr.Recall, 0.95,
		"recall must be >= 0.95 (got %.3f)", pr.Recall)
}
