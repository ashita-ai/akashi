package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStore creates an in-memory store for testing.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(":memory:", testLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// insertTestDecision is a helper that creates a decision with sensible defaults.
func insertTestDecision(t *testing.T, s *Store, decisionType, outcome string, reasoning *string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := s.InsertDecision(context.Background(), CheckResult{
		DecisionID:   id,
		DecisionType: decisionType,
		Outcome:      outcome,
		Reasoning:    reasoning,
		Confidence:   0.85,
		AgentID:      "test-agent",
		CreatedAt:    time.Now(),
	})
	require.NoError(t, err)
	return id
}

func strPtr(s string) *string { return &s }

// --- FTS5 Tests ---

func TestSearchByFTS_BasicMatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	insertTestDecision(t, s, "architecture", "chose PostgreSQL for the database", strPtr("relational model fits our needs"))
	insertTestDecision(t, s, "security", "use mTLS for service mesh", strPtr("zero-trust network"))
	insertTestDecision(t, s, "caching", "Redis with 5-minute TTL", strPtr("session caching layer"))

	results, err := s.Check(ctx, "PostgreSQL database", nil, 5)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "chose PostgreSQL for the database", results[0].Outcome)
	assert.Greater(t, results[0].Score, 0.0)
}

func TestSearchByFTS_PorterStemming(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	insertTestDecision(t, s, "testing", "running integration tests nightly", strPtr("ensures stability"))

	// "run" should match "running" via porter stemming.
	results, err := s.Check(ctx, "run", nil, 5)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results[0].Outcome, "running")
}

func TestSearchByFTS_BM25Ranking(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Decision with "cache" in both outcome and reasoning should rank higher
	// than one with "cache" only in the type field.
	insertTestDecision(t, s, "caching", "implement Redis cache with cache invalidation", strPtr("cache everything"))
	insertTestDecision(t, s, "architecture", "use microservices", strPtr("better scaling"))

	results, err := s.Check(ctx, "cache", nil, 5)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(results), 1)
	assert.Contains(t, results[0].Outcome, "cache")
}

func TestSearchByFTS_NoResults(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	insertTestDecision(t, s, "architecture", "chose PostgreSQL", nil)

	results, err := s.Check(ctx, "kubernetes", nil, 5)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestSearchByFTS_MultipleWords(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	insertTestDecision(t, s, "security", "use mTLS for service communication", strPtr("encrypted traffic"))
	insertTestDecision(t, s, "architecture", "deploy on Kubernetes", strPtr("container orchestration"))

	// FTS5 with implicit AND: both words must appear.
	results, err := s.Check(ctx, "mTLS service", nil, 5)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results[0].Outcome, "mTLS")
}

func TestSearchByFTS_MatchesReasoningField(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	insertTestDecision(t, s, "architecture", "use event sourcing", strPtr("audit trail compliance requirements"))

	results, err := s.Check(ctx, "audit trail", nil, 5)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "use event sourcing", results[0].Outcome)
}

func TestSearchByFTS_MatchesDecisionTypeField(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	insertTestDecision(t, s, "security", "enable WAF rules", nil)
	insertTestDecision(t, s, "architecture", "use monorepo", nil)

	results, err := s.Check(ctx, "security", nil, 5)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "enable WAF rules", results[0].Outcome)
}

// --- Cosine Similarity Tests ---

func TestCheck_SemanticMode(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Insert decisions with embeddings.
	id1 := insertTestDecision(t, s, "caching", "use Redis for caching", nil)
	id2 := insertTestDecision(t, s, "database", "use PostgreSQL", nil)

	// Store embeddings: id1 is closer to the query vector.
	require.NoError(t, s.StoreEmbedding(ctx, id1, []float32{0.9, 0.1, 0.0}))
	require.NoError(t, s.StoreEmbedding(ctx, id2, []float32{0.1, 0.9, 0.0}))

	// Query vector is close to id1's embedding.
	queryEmb := []float32{0.8, 0.2, 0.0}
	results, err := s.Check(ctx, "", queryEmb, 5)
	require.NoError(t, err)
	require.Len(t, results, 2)

	// id1 should rank first (closer to query).
	assert.Equal(t, id1, results[0].DecisionID)
	assert.Equal(t, id2, results[1].DecisionID)
	assert.Greater(t, results[0].Score, results[1].Score)
}

func TestCheck_SemanticMode_FiltersNegativeSimilarity(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	id1 := insertTestDecision(t, s, "test", "positive match", nil)
	id2 := insertTestDecision(t, s, "test", "negative match", nil)

	require.NoError(t, s.StoreEmbedding(ctx, id1, []float32{1, 0, 0}))
	require.NoError(t, s.StoreEmbedding(ctx, id2, []float32{-1, 0, 0})) // Opposite direction.

	results, err := s.Check(ctx, "", []float32{1, 0, 0}, 5)
	require.NoError(t, err)
	require.Len(t, results, 1, "anti-correlated result should be filtered out")
	assert.Equal(t, id1, results[0].DecisionID)
}

func TestCheck_FallbackToFTS_NoStoredEmbeddings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	insertTestDecision(t, s, "caching", "use Redis for caching", strPtr("fast lookups"))

	// Provide an embedding, but no stored embeddings exist → falls through to FTS5.
	results, err := s.Check(ctx, "Redis caching", []float32{0.5, 0.5, 0.0}, 5)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results[0].Outcome, "Redis")
}

func TestCheck_SkipsCosineForZeroVector(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	insertTestDecision(t, s, "test", "zero vector test", nil)

	// Zero vector should be treated as "no embedding" → FTS5 path.
	results, err := s.Check(ctx, "zero vector", []float32{0, 0, 0}, 5)
	require.NoError(t, err)
	require.Len(t, results, 1)
}

// --- Fallback & Edge Cases ---

func TestCheck_NoQuery_ReturnsRecent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Insert decisions with different timestamps.
	for i := range 5 {
		err := s.InsertDecision(ctx, CheckResult{
			DecisionID:   uuid.New(),
			DecisionType: "test",
			Outcome:      "decision " + string(rune('A'+i)),
			Confidence:   0.8,
			AgentID:      "agent",
			CreatedAt:    time.Now().Add(time.Duration(i) * time.Second),
		})
		require.NoError(t, err)
	}

	results, err := s.Check(ctx, "", nil, 3)
	require.NoError(t, err)
	require.Len(t, results, 3)
	// Most recent first.
	assert.Equal(t, "decision E", results[0].Outcome)
	assert.Equal(t, "decision D", results[1].Outcome)
	assert.Equal(t, "decision C", results[2].Outcome)
}

func TestCheck_DefaultLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for range 10 {
		insertTestDecision(t, s, "test", "decision", nil)
	}

	results, err := s.Check(ctx, "", nil, 0) // 0 should default to 5.
	require.NoError(t, err)
	assert.Len(t, results, 5)
}

func TestCheck_EmptyStore(t *testing.T) {
	s := newTestStore(t)

	results, err := s.Check(context.Background(), "anything", nil, 5)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestSearchByLike_Fallback(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	insertTestDecision(t, s, "testing", "run unit tests", strPtr("coverage above 80%"))

	// LIKE search works when FTS5 can't parse the query.
	results, err := s.searchByLike(ctx, "unit tests", 5)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results[0].Outcome, "unit tests")
}

func TestSearchByLike_EscapesSpecialCharacters(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	insertTestDecision(t, s, "test", "100% coverage", nil)

	results, err := s.searchByLike(ctx, "100%", 5)
	require.NoError(t, err)
	require.Len(t, results, 1)
}

// --- InsertDecision / FTS Sync Tests ---

func TestInsertDecision_UpdatesFTSOnUpsert(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := uuid.New()

	// Insert original.
	err := s.InsertDecision(ctx, CheckResult{
		DecisionID:   id,
		DecisionType: "architecture",
		Outcome:      "use monolith",
		Confidence:   0.7,
		AgentID:      "agent-1",
		CreatedAt:    time.Now(),
	})
	require.NoError(t, err)

	// Update (upsert) with new outcome.
	err = s.InsertDecision(ctx, CheckResult{
		DecisionID:   id,
		DecisionType: "architecture",
		Outcome:      "use microservices",
		Confidence:   0.9,
		AgentID:      "agent-1",
		CreatedAt:    time.Now(),
	})
	require.NoError(t, err)

	// FTS should find the new text, not the old.
	results, err := s.Check(ctx, "microservices", nil, 5)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, id, results[0].DecisionID)

	// Old text should not be found.
	results, err = s.Check(ctx, "monolith", nil, 5)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestDeleteDecision_RemovesFTSEntry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	id := insertTestDecision(t, s, "test", "deletable decision", nil)

	require.NoError(t, s.DeleteDecision(ctx, id))

	results, err := s.Check(ctx, "deletable", nil, 5)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestCheck_RespectsLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for range 10 {
		insertTestDecision(t, s, "architecture", "some architecture decision", nil)
	}

	results, err := s.Check(ctx, "architecture", nil, 3)
	require.NoError(t, err)
	assert.Len(t, results, 3)
}

func TestCheck_CosineRespectsLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Create 5 decisions with embeddings.
	for range 5 {
		id := insertTestDecision(t, s, "test", "cosine limit test", nil)
		require.NoError(t, s.StoreEmbedding(ctx, id, []float32{0.5, 0.5, 0.0}))
	}

	results, err := s.Check(ctx, "", []float32{0.5, 0.5, 0.0}, 2)
	require.NoError(t, err)
	assert.Len(t, results, 2)
}

// --- isZeroVector ---

func TestIsZeroVector(t *testing.T) {
	assert.True(t, isZeroVector(nil))
	assert.True(t, isZeroVector([]float32{}))
	assert.True(t, isZeroVector([]float32{0, 0, 0}))
	assert.False(t, isZeroVector([]float32{0, 0, 0.001}))
}
