package sqlite

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreEmbedding_Success(t *testing.T) {
	s, err := New(":memory:", testLogger())
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	id := uuid.New()

	// Insert a decision first.
	err = s.InsertDecision(ctx, CheckResult{
		DecisionID:   id,
		DecisionType: "architecture",
		Outcome:      "use Redis for caching",
		Confidence:   0.9,
		AgentID:      "agent-1",
		CreatedAt:    time.Now(),
	})
	require.NoError(t, err)

	// Store embedding.
	emb := []float32{0.1, 0.2, 0.3, 0.4}
	err = s.StoreEmbedding(ctx, id, emb)
	require.NoError(t, err)

	// Retrieve and verify.
	got, err := s.GetEmbedding(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, emb, got)
}

func TestStoreEmbedding_NonExistentDecision(t *testing.T) {
	s, err := New(":memory:", testLogger())
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	// Storing an embedding for a non-existent decision should not error.
	err = s.StoreEmbedding(context.Background(), uuid.New(), []float32{0.1, 0.2})
	assert.NoError(t, err)
}

func TestStoreEmbedding_OverwritesExisting(t *testing.T) {
	s, err := New(":memory:", testLogger())
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	id := uuid.New()

	err = s.InsertDecision(ctx, CheckResult{
		DecisionID:   id,
		DecisionType: "test",
		Outcome:      "overwrite test",
		Confidence:   0.8,
		AgentID:      "agent-1",
		CreatedAt:    time.Now(),
	})
	require.NoError(t, err)

	// Store first embedding.
	err = s.StoreEmbedding(ctx, id, []float32{1, 2, 3})
	require.NoError(t, err)

	// Overwrite with new embedding.
	newEmb := []float32{4, 5, 6}
	err = s.StoreEmbedding(ctx, id, newEmb)
	require.NoError(t, err)

	got, err := s.GetEmbedding(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, newEmb, got)
}

func TestGetEmbedding_NoEmbedding(t *testing.T) {
	s, err := New(":memory:", testLogger())
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	id := uuid.New()

	// Insert decision without embedding.
	err = s.InsertDecision(ctx, CheckResult{
		DecisionID:   id,
		DecisionType: "test",
		Outcome:      "no embedding",
		Confidence:   0.5,
		AgentID:      "agent-1",
		CreatedAt:    time.Now(),
	})
	require.NoError(t, err)

	got, err := s.GetEmbedding(ctx, id)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestGetEmbedding_NonExistentDecision(t *testing.T) {
	s, err := New(":memory:", testLogger())
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	got, err := s.GetEmbedding(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStoreEmbedding_ConcurrentWrites(t *testing.T) {
	s, err := New(":memory:", testLogger())
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	const n = 20

	// Create n decisions.
	ids := make([]uuid.UUID, n)
	for i := range n {
		ids[i] = uuid.New()
		err := s.InsertDecision(ctx, CheckResult{
			DecisionID:   ids[i],
			DecisionType: "concurrent-test",
			Outcome:      "test outcome",
			Confidence:   0.8,
			AgentID:      "agent-1",
			CreatedAt:    time.Now(),
		})
		require.NoError(t, err)
	}

	// Write embeddings concurrently.
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			emb := make([]float32, 64)
			for j := range emb {
				emb[j] = float32(idx*64 + j)
			}
			errs[idx] = s.StoreEmbedding(ctx, ids[idx], emb)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		assert.NoError(t, err, "embedding write %d failed", i)
	}

	// Verify all embeddings were stored.
	for i := range n {
		got, err := s.GetEmbedding(ctx, ids[i])
		require.NoError(t, err)
		require.NotNil(t, got, "embedding %d not found", i)
		assert.Len(t, got, 64)
	}
}
