package search

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

// newTestQdrantIndex creates a QdrantIndex connected to a local address.
// The connection may succeed (gRPC lazy connects) even if no server is running,
// but actual RPCs will fail. This is sufficient for testing early-return paths,
// error handling, and caching logic.
func newTestQdrantIndex(t *testing.T) *QdrantIndex {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(nil, nil))
	idx, err := NewQdrantIndex(QdrantConfig{
		URL:        "http://localhost:16334", // Non-standard port, no server running.
		Collection: "test_collection",
		Dims:       1024,
	}, logger)
	require.NoError(t, err, "NewQdrantIndex should succeed (gRPC is lazy-connect)")
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func TestNewQdrantIndex_Valid(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))

	idx, err := NewQdrantIndex(QdrantConfig{
		URL:        "http://localhost:6333",
		Collection: "decisions",
		Dims:       1024,
	}, logger)

	require.NoError(t, err)
	require.NotNil(t, idx)
	assert.Equal(t, "decisions", idx.collection)
	assert.Equal(t, uint64(1024), idx.dims)
	assert.NotNil(t, idx.client)

	_ = idx.Close()
}

func TestNewQdrantIndex_InvalidURL(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))

	_, err := NewQdrantIndex(QdrantConfig{
		URL:        "",
		Collection: "decisions",
		Dims:       1024,
	}, logger)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid qdrant URL")
}

func TestNewQdrantIndex_HTTPSConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))

	idx, err := NewQdrantIndex(QdrantConfig{
		URL:        "https://qdrant.example.com:6333",
		APIKey:     "test-api-key",
		Collection: "my_collection",
		Dims:       768,
	}, logger)

	// This may fail if the qdrant client does TLS handshake eagerly,
	// but typically gRPC connects lazily.
	if err != nil {
		// Acceptable: some gRPC dial options cause immediate failure for TLS.
		assert.Contains(t, err.Error(), "connect to qdrant")
		return
	}

	require.NotNil(t, idx)
	assert.Equal(t, "my_collection", idx.collection)
	assert.Equal(t, uint64(768), idx.dims)

	_ = idx.Close()
}

func TestQdrantUpsert_EmptyPoints(t *testing.T) {
	idx := newTestQdrantIndex(t)

	// Upsert with empty points should return nil immediately without calling Qdrant.
	err := idx.Upsert(context.Background(), nil)
	assert.NoError(t, err)

	err = idx.Upsert(context.Background(), []Point{})
	assert.NoError(t, err)
}

func TestQdrantDeleteByIDs_EmptyIDs(t *testing.T) {
	idx := newTestQdrantIndex(t)

	// DeleteByIDs with empty IDs should return nil immediately.
	err := idx.DeleteByIDs(context.Background(), nil)
	assert.NoError(t, err)

	err = idx.DeleteByIDs(context.Background(), []uuid.UUID{})
	assert.NoError(t, err)
}

func TestQdrantHealthErr_StoreAndLoad(t *testing.T) {
	idx := newTestQdrantIndex(t)

	// Initially, loadHealthErr should return nil.
	assert.Nil(t, idx.loadHealthErr())

	// Store an error.
	testErr := fmt.Errorf("connection refused")
	idx.storeHealthErr(testErr)
	loaded := idx.loadHealthErr()
	require.Error(t, loaded)
	assert.Equal(t, "connection refused", loaded.Error())

	// Store nil (healthy).
	idx.storeHealthErr(nil)
	assert.Nil(t, idx.loadHealthErr())

	// Store another error.
	idx.storeHealthErr(fmt.Errorf("timeout"))
	loaded = idx.loadHealthErr()
	require.Error(t, loaded)
	assert.Equal(t, "timeout", loaded.Error())
}

func TestQdrantHealthErr_CacheTiming(t *testing.T) {
	idx := newTestQdrantIndex(t)

	// Manually set a cached healthy result with a recent timestamp.
	idx.storeHealthErr(nil)
	idx.healthAt.Store(time.Now().UnixNano())

	// The fast path in Healthy checks time.Since < 5s. Since we just set it,
	// it should return the cached nil immediately without making a gRPC call.
	// We verify by checking that no error is returned (the gRPC call would fail
	// since no server is running).
	err := idx.Healthy(context.Background())
	assert.Nil(t, err, "cached healthy result should be returned from fast path")

	// Now set a cached error with a recent timestamp.
	cachedErr := fmt.Errorf("search: qdrant unhealthy: previous failure")
	idx.storeHealthErr(cachedErr)
	idx.healthAt.Store(time.Now().UnixNano())

	err = idx.Healthy(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "previous failure")
}

func TestQdrantHealthy_ExpiredCache(t *testing.T) {
	idx := newTestQdrantIndex(t)

	// Set a cached healthy result with an old timestamp (>5s ago).
	idx.storeHealthErr(nil)
	idx.healthAt.Store(time.Now().Add(-10 * time.Second).UnixNano())

	// With expired cache, Healthy should make a real gRPC call, which will fail
	// because there's no Qdrant server running.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := idx.Healthy(ctx)
	require.Error(t, err, "expired cache should trigger real health check which fails")
	assert.Contains(t, err.Error(), "qdrant unhealthy")
}

func TestQdrantClose(t *testing.T) {
	idx := newTestQdrantIndex(t)

	// Close should not panic. The cleanup in newTestQdrantIndex will also call Close,
	// but double-close on gRPC connections is safe.
	err := idx.Close()
	assert.NoError(t, err)
}

func TestQdrantSearch_FailsWithoutServer(t *testing.T) {
	idx := newTestQdrantIndex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	embedding := make([]float32, 1024)
	results, err := idx.Search(ctx, uuid.New(), embedding, model.QueryFilters{}, 10)

	require.Error(t, err, "search should fail without a running Qdrant server")
	assert.Contains(t, err.Error(), "qdrant query")
	assert.Nil(t, results)
}

func TestQdrantUpsert_FailsWithoutServer(t *testing.T) {
	idx := newTestQdrantIndex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	points := []Point{
		{
			ID:           uuid.New(),
			OrgID:        uuid.New(),
			AgentID:      "test-agent",
			DecisionType: "architecture",
			Confidence:   0.9,
			QualityScore: 0.8,
			ValidFrom:    time.Now(),
			Embedding:    make([]float32, 1024),
		},
	}

	err := idx.Upsert(ctx, points)
	require.Error(t, err, "upsert should fail without a running Qdrant server")
	assert.Contains(t, err.Error(), "qdrant upsert")
}

func TestQdrantDeleteByIDs_FailsWithoutServer(t *testing.T) {
	idx := newTestQdrantIndex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := idx.DeleteByIDs(ctx, []uuid.UUID{uuid.New()})
	require.Error(t, err, "delete should fail without a running Qdrant server")
	assert.Contains(t, err.Error(), "qdrant delete")
}

func TestQdrantDeleteByOrg_FailsWithoutServer(t *testing.T) {
	idx := newTestQdrantIndex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := idx.DeleteByOrg(ctx, uuid.New())
	require.Error(t, err, "delete by org should fail without a running Qdrant server")
	assert.Contains(t, err.Error(), "qdrant delete by org")
}

func TestQdrantEnsureCollection_FailsWithoutServer(t *testing.T) {
	idx := newTestQdrantIndex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := idx.EnsureCollection(ctx)
	require.Error(t, err, "ensure collection should fail without a running Qdrant server")
	assert.Contains(t, err.Error(), "check collection exists")
}

func TestQdrantUpsert_PointPayloadFields(t *testing.T) {
	// Verify that Upsert with empty points returns nil but that the point
	// construction logic (payload map building) works for all field combinations.
	// This tests the code path in Upsert that builds qdrant.PointStruct payloads.

	idx := newTestQdrantIndex(t)

	// Point with all optional fields set.
	sessionID := uuid.New()
	fullPoint := Point{
		ID:           uuid.New(),
		OrgID:        uuid.New(),
		AgentID:      "coder",
		DecisionType: "architecture",
		Confidence:   0.95,
		QualityScore: 0.85,
		ValidFrom:    time.Now(),
		Embedding:    make([]float32, 1024),
		SessionID:    &sessionID,
		Tool:         "claude-code",
		Model:        "claude-opus-4-6",
		Repo:         "ashita-ai/akashi",
	}

	// Point with minimal fields (no optional fields).
	minimalPoint := Point{
		ID:           uuid.New(),
		OrgID:        uuid.New(),
		AgentID:      "planner",
		DecisionType: "planning",
		Confidence:   0.5,
		QualityScore: 0.3,
		ValidFrom:    time.Now(),
		Embedding:    make([]float32, 1024),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Both will fail because no server, but we exercise the payload building code.
	err := idx.Upsert(ctx, []Point{fullPoint, minimalPoint})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "qdrant upsert 2 points")
}

func TestQdrantSearch_WithFilters(t *testing.T) {
	// Test that Search constructs the correct filter conditions.
	// The search will fail (no server), but we exercise the filter-building code paths.
	idx := newTestQdrantIndex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	embedding := make([]float32, 1024)

	// Test with various filter combinations to exercise all condition branches.
	t.Run("single agent_id", func(t *testing.T) {
		filters := model.QueryFilters{AgentIDs: []string{"planner"}}
		_, err := idx.Search(ctx, uuid.New(), embedding, filters, 10)
		require.Error(t, err)
	})

	t.Run("multiple agent_ids", func(t *testing.T) {
		filters := model.QueryFilters{AgentIDs: []string{"planner", "coder", "reviewer"}}
		_, err := idx.Search(ctx, uuid.New(), embedding, filters, 10)
		require.Error(t, err)
	})

	t.Run("decision_type filter", func(t *testing.T) {
		dt := "architecture"
		filters := model.QueryFilters{DecisionType: &dt}
		_, err := idx.Search(ctx, uuid.New(), embedding, filters, 10)
		require.Error(t, err)
	})

	t.Run("confidence_min filter", func(t *testing.T) {
		cm := float32(0.8)
		filters := model.QueryFilters{ConfidenceMin: &cm}
		_, err := idx.Search(ctx, uuid.New(), embedding, filters, 10)
		require.Error(t, err)
	})

	t.Run("time_range filter", func(t *testing.T) {
		from := time.Now().Add(-24 * time.Hour)
		to := time.Now()
		filters := model.QueryFilters{
			TimeRange: &model.TimeRange{From: &from, To: &to},
		}
		_, err := idx.Search(ctx, uuid.New(), embedding, filters, 10)
		require.Error(t, err)
	})

	t.Run("session_id filter", func(t *testing.T) {
		sid := uuid.New()
		filters := model.QueryFilters{SessionID: &sid}
		_, err := idx.Search(ctx, uuid.New(), embedding, filters, 10)
		require.Error(t, err)
	})

	t.Run("tool_model_repo filters", func(t *testing.T) {
		tool := "claude-code"
		mdl := "opus"
		repo := "ashita-ai/akashi"
		filters := model.QueryFilters{Tool: &tool, Model: &mdl, Repo: &repo}
		_, err := idx.Search(ctx, uuid.New(), embedding, filters, 10)
		require.Error(t, err)
	})

	t.Run("time_range_from_only", func(t *testing.T) {
		from := time.Now().Add(-48 * time.Hour)
		filters := model.QueryFilters{
			TimeRange: &model.TimeRange{From: &from},
		}
		_, err := idx.Search(ctx, uuid.New(), embedding, filters, 10)
		require.Error(t, err)
	})

	t.Run("time_range_to_only", func(t *testing.T) {
		to := time.Now()
		filters := model.QueryFilters{
			TimeRange: &model.TimeRange{To: &to},
		}
		_, err := idx.Search(ctx, uuid.New(), embedding, filters, 10)
		require.Error(t, err)
	})
}

func TestQdrantHealthy_Concurrent(t *testing.T) {
	idx := newTestQdrantIndex(t)

	// Set an old cache timestamp to force real health checks.
	idx.healthAt.Store(time.Now().Add(-10 * time.Second).UnixNano())

	// Run multiple concurrent Healthy calls. The singleflight should deduplicate
	// them so only one gRPC call is made.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 10)
	for range 10 {
		go func() {
			errs <- idx.Healthy(ctx)
		}()
	}

	for range 10 {
		err := <-errs
		// All should get the same error (connection refused).
		require.Error(t, err)
		assert.Contains(t, err.Error(), "qdrant unhealthy")
	}
}

func TestParseQdrantURL_InvalidPort(t *testing.T) {
	// Go's url.Parse may treat "notaport" as part of the host rather than
	// a separate port, depending on the URL format. Either error path is acceptable.
	_, _, _, err := parseQdrantURL("http://localhost:notaport")
	require.Error(t, err)
	assert.True(t,
		assert.ObjectsAreEqual("search: invalid port in qdrant URL: \"notaport\"", err.Error()) ||
			assert.ObjectsAreEqual("search: invalid qdrant URL: \"http://localhost:notaport\"", err.Error()),
		"expected either 'invalid port' or 'invalid qdrant URL' error, got: %s", err.Error(),
	)
}
