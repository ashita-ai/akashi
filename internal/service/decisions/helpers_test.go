package decisions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/conflicts"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/search"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/storage"
)

// pgDupKeyChecker is a minimal helper used by TestIsDuplicateKey.
// It mirrors the Postgres implementation of IsDuplicateKey without needing
// a full storage.DB (which requires a live connection pool).
type pgDupKeyChecker struct{}

func (pgDupKeyChecker) IsDuplicateKey(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func TestIsZeroVector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		vec      pgvector.Vector
		expected bool
	}{
		{
			name:     "all zeros",
			vec:      pgvector.NewVector([]float32{0, 0, 0, 0}),
			expected: true,
		},
		{
			name:     "first element nonzero",
			vec:      pgvector.NewVector([]float32{0.1, 0, 0, 0}),
			expected: false,
		},
		{
			name:     "last element nonzero",
			vec:      pgvector.NewVector([]float32{0, 0, 0, 0.01}),
			expected: false,
		},
		{
			name:     "all nonzero",
			vec:      pgvector.NewVector([]float32{0.5, 0.3, 0.2, 0.1}),
			expected: false,
		},
		{
			name:     "empty slice",
			vec:      pgvector.NewVector([]float32{}),
			expected: true,
		},
		{
			name:     "single zero",
			vec:      pgvector.NewVector([]float32{0}),
			expected: true,
		},
		{
			name:     "single nonzero",
			vec:      pgvector.NewVector([]float32{1.0}),
			expected: false,
		},
		{
			name:     "negative value",
			vec:      pgvector.NewVector([]float32{0, -0.5, 0}),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isZeroVector(tt.vec)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestValidateEmbeddingDims(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		dims      int
		vecLen    int
		expectErr bool
	}{
		{
			name:      "matching dimensions",
			dims:      1024,
			vecLen:    1024,
			expectErr: false,
		},
		{
			name:      "vector too short",
			dims:      1024,
			vecLen:    512,
			expectErr: true,
		},
		{
			name:      "vector too long",
			dims:      1024,
			vecLen:    2048,
			expectErr: true,
		},
		{
			name:      "zero-length vector with nonzero expected dims",
			dims:      1024,
			vecLen:    0,
			expectErr: true,
		},
		{
			name:      "single dimension match",
			dims:      1,
			vecLen:    1,
			expectErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provider := embedding.NewNoopProvider(tt.dims)
			svc := &Service{embedder: provider}

			vec := pgvector.NewVector(make([]float32, tt.vecLen))
			err := svc.validateEmbeddingDims(vec)

			if tt.expectErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "embedding dimension mismatch")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestIsDuplicateKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name: "duplicate key violation 23505",
			err: &pgconn.PgError{
				Code:    "23505",
				Message: "duplicate key value violates unique constraint",
			},
			expected: true,
		},
		{
			name: "foreign key violation 23503",
			err: &pgconn.PgError{
				Code:    "23503",
				Message: "insert or update on table violates foreign key constraint",
			},
			expected: false,
		},
		{
			name: "check constraint violation 23514",
			err: &pgconn.PgError{
				Code:    "23514",
				Message: "new row violates check constraint",
			},
			expected: false,
		},
		{
			name:     "generic non-pg error",
			err:      assert.AnError,
			expected: false,
		},
	}

	checker := pgDupKeyChecker{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := checker.IsDuplicateKey(tt.err)
			assert.Equal(t, tt.expected, got)
		})
	}
}

// ---------------------------------------------------------------------------
// cosineSimFloat32
// ---------------------------------------------------------------------------

func TestCosineSimFloat32(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		a, b     []float32
		expected float64
	}{
		{
			name:     "identical unit vectors",
			a:        []float32{1, 0, 0},
			b:        []float32{1, 0, 0},
			expected: 1.0,
		},
		{
			name:     "orthogonal vectors",
			a:        []float32{1, 0, 0},
			b:        []float32{0, 1, 0},
			expected: 0.0,
		},
		{
			name:     "opposite vectors",
			a:        []float32{1, 0, 0},
			b:        []float32{-1, 0, 0},
			expected: -1.0,
		},
		{
			name:     "parallel scaled vectors",
			a:        []float32{1, 2, 3},
			b:        []float32{2, 4, 6},
			expected: 1.0,
		},
		{
			name:     "mismatched lengths returns 0",
			a:        []float32{1, 2},
			b:        []float32{1, 2, 3},
			expected: 0,
		},
		{
			name:     "both empty returns 0",
			a:        []float32{},
			b:        []float32{},
			expected: 0,
		},
		{
			name:     "a nil returns 0",
			a:        nil,
			b:        []float32{1, 2, 3},
			expected: 0,
		},
		{
			name:     "b nil returns 0",
			a:        []float32{1, 2, 3},
			b:        nil,
			expected: 0,
		},
		{
			name:     "both nil returns 0",
			a:        nil,
			b:        nil,
			expected: 0,
		},
		{
			name:     "a is zero vector returns 0",
			a:        []float32{0, 0, 0},
			b:        []float32{1, 2, 3},
			expected: 0,
		},
		{
			name:     "b is zero vector returns 0",
			a:        []float32{1, 2, 3},
			b:        []float32{0, 0, 0},
			expected: 0,
		},
		{
			name:     "both zero vectors returns 0",
			a:        []float32{0, 0, 0},
			b:        []float32{0, 0, 0},
			expected: 0,
		},
		{
			name:     "single element identical",
			a:        []float32{5},
			b:        []float32{5},
			expected: 1.0,
		},
		{
			name:     "negative vectors same direction",
			a:        []float32{-1, -2, -3},
			b:        []float32{-2, -4, -6},
			expected: 1.0,
		},
		{
			name: "known angle 45 degrees",
			// cos(45°) ≈ 0.7071
			a:        []float32{1, 0},
			b:        []float32{1, 1},
			expected: 1.0 / math.Sqrt(2),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := cosineSimFloat32(tt.a, tt.b)
			assert.InDelta(t, tt.expected, got, 1e-7, "cosineSimFloat32(%v, %v)", tt.a, tt.b)
		})
	}
}

// ---------------------------------------------------------------------------
// embeddingText
// ---------------------------------------------------------------------------

func TestEmbeddingText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    storage.UnembeddedDecision
		expected string
	}{
		{
			name: "type and outcome only",
			input: storage.UnembeddedDecision{
				DecisionType: "architecture",
				Outcome:      "chose PostgreSQL",
			},
			expected: "architecture: chose PostgreSQL",
		},
		{
			name: "type outcome and reasoning",
			input: storage.UnembeddedDecision{
				DecisionType: "framework",
				Outcome:      "selected React",
				Reasoning:    strPtr("better ecosystem"),
			},
			expected: "framework: selected React better ecosystem",
		},
		{
			name: "empty outcome",
			input: storage.UnembeddedDecision{
				DecisionType: "config",
				Outcome:      "",
			},
			expected: "config: ",
		},
		{
			name: "nil reasoning",
			input: storage.UnembeddedDecision{
				DecisionType: "deployment",
				Outcome:      "use Kubernetes",
				Reasoning:    nil,
			},
			expected: "deployment: use Kubernetes",
		},
		{
			name: "empty reasoning string",
			input: storage.UnembeddedDecision{
				DecisionType: "database",
				Outcome:      "use Postgres",
				Reasoning:    strPtr(""),
			},
			expected: "database: use Postgres ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := embeddingText(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// Setters: SetPercentileCache, SetReScoreMetrics, SetClaimExtractor
// ---------------------------------------------------------------------------

func TestSetPercentileCache(t *testing.T) {
	t.Parallel()
	svc := &Service{}
	assert.Nil(t, svc.percentileCache, "should start nil")

	cache := search.NewPercentileCache()
	svc.SetPercentileCache(cache)
	assert.Same(t, cache, svc.percentileCache, "setter should assign the cache")
}

func TestSetReScoreMetrics(t *testing.T) {
	t.Parallel()
	svc := &Service{}
	assert.Nil(t, svc.rescoreMetrics, "should start nil")

	m := &search.ReScoreMetrics{}
	svc.SetReScoreMetrics(m)
	assert.Same(t, m, svc.rescoreMetrics, "setter should assign the metrics")
}

// stubClaimExtractor implements conflicts.ClaimExtractor for testing SetClaimExtractor.
type stubClaimExtractor struct{}

func (stubClaimExtractor) ExtractClaims(_ context.Context, _ string) ([]conflicts.ExtractedClaim, error) {
	return nil, nil
}

func TestSetClaimExtractor(t *testing.T) {
	t.Parallel()
	svc := &Service{}
	assert.Nil(t, svc.claimExtractor, "should start nil")

	ext := stubClaimExtractor{}
	svc.SetClaimExtractor(ext)
	assert.NotNil(t, svc.claimExtractor, "setter should assign the extractor")
}

// ---------------------------------------------------------------------------
// Mock types for ConsensusScores / ConsensusScoresBatch / RetryFailedClaimEmbeddings
// ---------------------------------------------------------------------------

// mockStore implements the subset of storage.Store used by the functions under test.
// Unused methods panic to surface accidental calls immediately.
type mockStore struct {
	storage.Store // embed interface; unused methods panic

	conflictCount      int
	conflictCountErr   error
	conflictCounts     map[uuid.UUID]int
	conflictCountsErr  error
	embeddings         map[uuid.UUID][2]pgvector.Vector
	embeddingsErr      error
	retriableFailures  []storage.ClaimRetryRef
	retriableErr       error
	decisionForScoring model.Decision
	decisionForScoErr  error
	hasClaims          bool
	hasClaimsErr       error
	insertClaimsErr    error
	markFailedErr      error
	clearFailureErr    error

	// Tracking calls.
	markFailedCalls   []uuid.UUID
	clearFailureCalls []uuid.UUID
	insertClaimsCalls int
}

func (m *mockStore) GetConflictCount(_ context.Context, _ uuid.UUID, _ uuid.UUID) (int, error) {
	return m.conflictCount, m.conflictCountErr
}

func (m *mockStore) GetConflictCountsBatch(_ context.Context, _ []uuid.UUID, _ uuid.UUID) (map[uuid.UUID]int, error) {
	return m.conflictCounts, m.conflictCountsErr
}

func (m *mockStore) GetDecisionEmbeddings(_ context.Context, _ []uuid.UUID, _ uuid.UUID) (map[uuid.UUID][2]pgvector.Vector, error) {
	return m.embeddings, m.embeddingsErr
}

func (m *mockStore) FindRetriableClaimFailures(_ context.Context, _ int, _ int) ([]storage.ClaimRetryRef, error) {
	return m.retriableFailures, m.retriableErr
}

func (m *mockStore) GetDecisionForScoring(_ context.Context, id, _ uuid.UUID) (model.Decision, error) {
	if m.decisionForScoErr != nil {
		return model.Decision{}, m.decisionForScoErr
	}
	d := m.decisionForScoring
	d.ID = id
	return d, nil
}

func (m *mockStore) HasClaimsForDecision(_ context.Context, _ uuid.UUID, _ uuid.UUID) (bool, error) {
	return m.hasClaims, m.hasClaimsErr
}

func (m *mockStore) InsertClaims(_ context.Context, _ []storage.Claim) error {
	m.insertClaimsCalls++
	return m.insertClaimsErr
}

func (m *mockStore) MarkClaimEmbeddingFailed(_ context.Context, id, _ uuid.UUID) error {
	m.markFailedCalls = append(m.markFailedCalls, id)
	return m.markFailedErr
}

func (m *mockStore) ClearClaimEmbeddingFailure(_ context.Context, id, _ uuid.UUID) error {
	m.clearFailureCalls = append(m.clearFailureCalls, id)
	return m.clearFailureErr
}

func (m *mockStore) IsDuplicateKey(_ error) bool { return false }

// mockSearcher implements search.Searcher and optionally search.CandidateFinder.
type mockSearcher struct {
	healthy       error
	findResults   []search.Result
	findErr       error
	findCallCount int
}

func (m *mockSearcher) Search(_ context.Context, _ uuid.UUID, _ []float32, _ model.QueryFilters, _ int) ([]search.Result, error) {
	return nil, nil
}

func (m *mockSearcher) Healthy(_ context.Context) error {
	return m.healthy
}

func (m *mockSearcher) FindSimilar(_ context.Context, _ uuid.UUID, _ []float32, _ uuid.UUID, _ *string, _ int) ([]search.Result, error) {
	m.findCallCount++
	return m.findResults, m.findErr
}

// nonCandidateSearcher implements only Searcher (not CandidateFinder).
type nonCandidateSearcher struct{}

func (nonCandidateSearcher) Search(_ context.Context, _ uuid.UUID, _ []float32, _ model.QueryFilters, _ int) ([]search.Result, error) {
	return nil, nil
}

func (nonCandidateSearcher) Healthy(_ context.Context) error { return nil }

// mockConflictScorer tracks ScoreForDecision calls.
type mockConflictScorer struct {
	calls []uuid.UUID
}

func (m *mockConflictScorer) ScoreForDecision(_ context.Context, decisionID, _ uuid.UUID) {
	m.calls = append(m.calls, decisionID)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// newTestService builds a Service with mocked dependencies for unit tests.
// Uses fakeEmbedder so probe checks pass (NoopProvider returns ErrNoProvider).
func newTestService(db storage.Store, searcher search.Searcher, scorer ConflictScorer) *Service {
	svc := New(db, fakeEmbedder{dims: 3}, searcher, testLogger(), scorer)
	return svc
}

// ---------------------------------------------------------------------------
// ConsensusScores
// ---------------------------------------------------------------------------

func TestConsensusScores(t *testing.T) {
	t.Parallel()

	orgID := uuid.Nil
	decID := uuid.New()

	// Build embeddings: index 0 = decision embedding, index 1 = outcome embedding.
	makeEmb := func(v []float32) [2]pgvector.Vector {
		return [2]pgvector.Vector{
			pgvector.NewVector(v),
			pgvector.NewVector(v), // outcome = decision for simplicity
		}
	}

	t.Run("conflict count error propagates", func(t *testing.T) {
		t.Parallel()
		ms := &mockStore{conflictCountErr: fmt.Errorf("db down")}
		svc := newTestService(ms, &mockSearcher{}, nil)
		_, _, err := svc.ConsensusScores(context.Background(), decID, orgID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "db down")
	})

	t.Run("searcher not CandidateFinder returns zero agreement", func(t *testing.T) {
		t.Parallel()
		ms := &mockStore{conflictCount: 3}
		svc := newTestService(ms, nonCandidateSearcher{}, nil)
		agreement, conflicts, err := svc.ConsensusScores(context.Background(), decID, orgID)
		require.NoError(t, err)
		assert.Equal(t, 0, agreement)
		assert.Equal(t, 3, conflicts)
	})

	t.Run("searcher unhealthy returns zero agreement", func(t *testing.T) {
		t.Parallel()
		ms := &mockStore{conflictCount: 2}
		searcher := &mockSearcher{healthy: fmt.Errorf("qdrant down")}
		svc := newTestService(ms, searcher, nil)
		agreement, conflicts, err := svc.ConsensusScores(context.Background(), decID, orgID)
		require.NoError(t, err)
		assert.Equal(t, 0, agreement)
		assert.Equal(t, 2, conflicts)
	})

	t.Run("no embedding for decision returns zero agreement", func(t *testing.T) {
		t.Parallel()
		ms := &mockStore{
			conflictCount: 1,
			embeddings:    map[uuid.UUID][2]pgvector.Vector{}, // empty: no embedding
		}
		searcher := &mockSearcher{}
		svc := newTestService(ms, searcher, nil)
		agreement, conflicts, err := svc.ConsensusScores(context.Background(), decID, orgID)
		require.NoError(t, err)
		assert.Equal(t, 0, agreement)
		assert.Equal(t, 1, conflicts)
	})

	t.Run("no qdrant results returns zero agreement", func(t *testing.T) {
		t.Parallel()
		ms := &mockStore{
			conflictCount: 0,
			embeddings:    map[uuid.UUID][2]pgvector.Vector{decID: makeEmb([]float32{1, 0, 0})},
		}
		searcher := &mockSearcher{findResults: nil}
		svc := newTestService(ms, searcher, nil)
		agreement, conflicts, err := svc.ConsensusScores(context.Background(), decID, orgID)
		require.NoError(t, err)
		assert.Equal(t, 0, agreement)
		assert.Equal(t, 0, conflicts)
	})

	t.Run("counts neighbors above 0.75 threshold", func(t *testing.T) {
		t.Parallel()

		neighborHigh := uuid.New() // outcome identical => cosine 1.0 >= 0.75
		neighborLow := uuid.New()  // outcome orthogonal => cosine 0 < 0.75
		neighborMid := uuid.New()  // outcome parallel => cosine 1.0 >= 0.75

		ms := &mockStore{
			conflictCount: 1,
			embeddings: map[uuid.UUID][2]pgvector.Vector{
				decID:        makeEmb([]float32{1, 0, 0}),
				neighborHigh: makeEmb([]float32{1, 0, 0}), // identical: cosine = 1.0
				neighborLow:  makeEmb([]float32{0, 1, 0}), // orthogonal: cosine = 0
				neighborMid:  makeEmb([]float32{2, 0, 0}), // parallel: cosine = 1.0
			},
		}
		searcher := &mockSearcher{
			findResults: []search.Result{
				{DecisionID: neighborHigh, Score: 0.95},
				{DecisionID: neighborLow, Score: 0.90},
				{DecisionID: neighborMid, Score: 0.85},
			},
		}
		svc := newTestService(ms, searcher, nil)
		agreement, conflicts, err := svc.ConsensusScores(context.Background(), decID, orgID)
		require.NoError(t, err)
		assert.Equal(t, 2, agreement, "neighborHigh and neighborMid have cosine >= 0.75")
		assert.Equal(t, 1, conflicts)
	})

	t.Run("qdrant find similar error returns zero agreement gracefully", func(t *testing.T) {
		t.Parallel()
		ms := &mockStore{
			conflictCount: 0,
			embeddings:    map[uuid.UUID][2]pgvector.Vector{decID: makeEmb([]float32{1, 0, 0})},
		}
		searcher := &mockSearcher{findErr: fmt.Errorf("qdrant timeout")}
		svc := newTestService(ms, searcher, nil)
		agreement, conflicts, err := svc.ConsensusScores(context.Background(), decID, orgID)
		require.NoError(t, err)
		assert.Equal(t, 0, agreement)
		assert.Equal(t, 0, conflicts)
	})

	t.Run("neighbor embeddings fetch error returns zero agreement gracefully", func(t *testing.T) {
		t.Parallel()
		// Use a wrapper mock that fails on the second GetDecisionEmbeddings call.
		dualMock := &dualCallEmbeddingStore{
			mockStore: mockStore{conflictCount: 0},
			calls:     0,
			firstResult: map[uuid.UUID][2]pgvector.Vector{
				decID: makeEmb([]float32{1, 0, 0}),
			},
			secondErr: fmt.Errorf("neighbor fetch failed"),
		}
		searcher := &mockSearcher{
			findResults: []search.Result{{DecisionID: uuid.New(), Score: 0.9}},
		}
		svc := newTestService(dualMock, searcher, nil)
		agreement, conflicts, err := svc.ConsensusScores(context.Background(), decID, orgID)
		require.NoError(t, err)
		assert.Equal(t, 0, agreement, "should gracefully return 0 on neighbor fetch error")
		assert.Equal(t, 0, conflicts)
	})
}

// dualCallEmbeddingStore returns different results for the first and second
// GetDecisionEmbeddings call, enabling tests to simulate the source-fetch
// succeeding while the neighbor-fetch fails.
type dualCallEmbeddingStore struct {
	mockStore
	calls       int
	firstResult map[uuid.UUID][2]pgvector.Vector
	secondErr   error
}

func (d *dualCallEmbeddingStore) GetDecisionEmbeddings(ctx context.Context, ids []uuid.UUID, orgID uuid.UUID) (map[uuid.UUID][2]pgvector.Vector, error) {
	d.calls++
	if d.calls == 1 {
		return d.firstResult, nil
	}
	return nil, d.secondErr
}

// ---------------------------------------------------------------------------
// ConsensusScoresBatch
// ---------------------------------------------------------------------------

func TestConsensusScoresBatch(t *testing.T) {
	t.Parallel()

	orgID := uuid.Nil

	makeEmb := func(v []float32) [2]pgvector.Vector {
		return [2]pgvector.Vector{
			pgvector.NewVector(v),
			pgvector.NewVector(v),
		}
	}

	t.Run("conflict counts error propagates", func(t *testing.T) {
		t.Parallel()
		ms := &mockStore{conflictCountsErr: fmt.Errorf("batch conflict error")}
		svc := newTestService(ms, &mockSearcher{}, nil)
		_, err := svc.ConsensusScoresBatch(context.Background(), []uuid.UUID{uuid.New()}, orgID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "batch conflict error")
	})

	t.Run("searcher not CandidateFinder returns conflict counts only", func(t *testing.T) {
		t.Parallel()
		id1, id2 := uuid.New(), uuid.New()
		ms := &mockStore{
			conflictCounts: map[uuid.UUID]int{id1: 2, id2: 0},
		}
		svc := newTestService(ms, nonCandidateSearcher{}, nil)
		result, err := svc.ConsensusScoresBatch(context.Background(), []uuid.UUID{id1, id2}, orgID)
		require.NoError(t, err)
		assert.Equal(t, 2, result[id1][1], "id1 conflict count")
		assert.Equal(t, 0, result[id1][0], "id1 agreement count without CandidateFinder")
	})

	t.Run("searcher unhealthy skips agreement", func(t *testing.T) {
		t.Parallel()
		id := uuid.New()
		ms := &mockStore{
			conflictCounts: map[uuid.UUID]int{id: 5},
		}
		searcher := &mockSearcher{healthy: fmt.Errorf("qdrant down")}
		svc := newTestService(ms, searcher, nil)
		result, err := svc.ConsensusScoresBatch(context.Background(), []uuid.UUID{id}, orgID)
		require.NoError(t, err)
		assert.Equal(t, 5, result[id][1])
		assert.Equal(t, 0, result[id][0])
	})

	t.Run("no embeddings returns conflict counts only", func(t *testing.T) {
		t.Parallel()
		id := uuid.New()
		ms := &mockStore{
			conflictCounts: map[uuid.UUID]int{id: 1},
			embeddings:     map[uuid.UUID][2]pgvector.Vector{},
		}
		searcher := &mockSearcher{}
		svc := newTestService(ms, searcher, nil)
		result, err := svc.ConsensusScoresBatch(context.Background(), []uuid.UUID{id}, orgID)
		require.NoError(t, err)
		assert.Equal(t, 1, result[id][1])
		assert.Equal(t, 0, result[id][0])
	})

	t.Run("computes agreement for multiple decisions", func(t *testing.T) {
		t.Parallel()
		dec1, dec2 := uuid.New(), uuid.New()
		neighbor := uuid.New()

		ms := &mockStore{
			conflictCounts: map[uuid.UUID]int{dec1: 0, dec2: 1},
			embeddings: map[uuid.UUID][2]pgvector.Vector{
				dec1:     makeEmb([]float32{1, 0, 0}),
				dec2:     makeEmb([]float32{0, 1, 0}),
				neighbor: makeEmb([]float32{1, 0, 0}), // identical to dec1
			},
		}
		searcher := &mockSearcher{
			findResults: []search.Result{{DecisionID: neighbor, Score: 0.9}},
		}
		svc := newTestService(ms, searcher, nil)
		result, err := svc.ConsensusScoresBatch(context.Background(), []uuid.UUID{dec1, dec2}, orgID)
		require.NoError(t, err)
		// dec1 outcome [1,0,0] vs neighbor [1,0,0] => cosine 1.0 >= 0.75 => agreement 1
		assert.Equal(t, 1, result[dec1][0], "dec1 agreement")
		assert.Equal(t, 0, result[dec1][1], "dec1 conflicts")
		// dec2 outcome [0,1,0] vs neighbor [1,0,0] => cosine 0 < 0.75 => agreement 0
		assert.Equal(t, 0, result[dec2][0], "dec2 agreement")
		assert.Equal(t, 1, result[dec2][1], "dec2 conflicts")
	})

	t.Run("empty input returns empty map", func(t *testing.T) {
		t.Parallel()
		ms := &mockStore{conflictCounts: map[uuid.UUID]int{}}
		svc := newTestService(ms, &mockSearcher{}, nil)
		result, err := svc.ConsensusScoresBatch(context.Background(), []uuid.UUID{}, orgID)
		require.NoError(t, err)
		assert.Empty(t, result)
	})
}

// ---------------------------------------------------------------------------
// RetryFailedClaimEmbeddings
// ---------------------------------------------------------------------------

func TestRetryFailedClaimEmbeddings(t *testing.T) {
	t.Parallel()

	orgID := uuid.Nil

	t.Run("noop embedder returns 0", func(t *testing.T) {
		t.Parallel()
		// NoopProvider.Embed returns ErrNoProvider, so RetryFailedClaimEmbeddings
		// detects the absence of a real provider and returns 0 immediately.
		ms := &mockStore{}
		svc := New(ms, embedding.NewNoopProvider(3), nil, testLogger(), nil)
		count, err := svc.RetryFailedClaimEmbeddings(context.Background(), 10, 5)
		require.NoError(t, err)
		assert.Equal(t, 0, count)
	})

	t.Run("no retriable failures returns 0", func(t *testing.T) {
		t.Parallel()
		ms := &mockStore{retriableFailures: nil}
		svc := newTestService(ms, nil, nil)
		count, err := svc.RetryFailedClaimEmbeddings(context.Background(), 10, 5)
		require.NoError(t, err)
		assert.Equal(t, 0, count)
	})

	t.Run("find retriable errors propagate", func(t *testing.T) {
		t.Parallel()
		ms := &mockStore{retriableErr: fmt.Errorf("db error")}
		svc := newTestService(ms, nil, nil)
		_, err := svc.RetryFailedClaimEmbeddings(context.Background(), 10, 5)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "retry claims: find")
	})

	t.Run("successful retry clears failure and scores conflicts", func(t *testing.T) {
		t.Parallel()
		decID := uuid.New()
		scorer := &mockConflictScorer{}
		ms := &mockStore{
			retriableFailures: []storage.ClaimRetryRef{
				{ID: decID, OrgID: orgID, Attempts: 1},
			},
			decisionForScoring: model.Decision{Outcome: "chose Go"},
			hasClaims:          true, // claims already exist => generateClaims returns nil (skip)
		}
		svc := newTestService(ms, nil, scorer)
		count, err := svc.RetryFailedClaimEmbeddings(context.Background(), 10, 5)
		require.NoError(t, err)
		assert.Equal(t, 1, count)
		assert.Len(t, ms.clearFailureCalls, 1, "should clear failure on success")
		assert.Equal(t, decID, ms.clearFailureCalls[0])
		assert.Len(t, scorer.calls, 1, "should trigger conflict scoring")
		assert.Equal(t, decID, scorer.calls[0])
	})

	t.Run("failed retry marks failure and increments counter", func(t *testing.T) {
		t.Parallel()
		decID := uuid.New()
		ms := &mockStore{
			retriableFailures: []storage.ClaimRetryRef{
				{ID: decID, OrgID: orgID, Attempts: 2},
			},
			decisionForScoErr: fmt.Errorf("decision not found"),
		}
		svc := newTestService(ms, nil, nil)
		count, err := svc.RetryFailedClaimEmbeddings(context.Background(), 10, 5)
		require.NoError(t, err)
		assert.Equal(t, 0, count, "failed retry should not count as success")
	})

	t.Run("context cancellation stops iteration", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel immediately
		ms := &mockStore{
			retriableFailures: []storage.ClaimRetryRef{
				{ID: uuid.New(), OrgID: orgID, Attempts: 0},
				{ID: uuid.New(), OrgID: orgID, Attempts: 0},
			},
			decisionForScoring: model.Decision{Outcome: "irrelevant"},
			hasClaims:          true,
		}
		svc := newTestService(ms, nil, nil)
		count, err := svc.RetryFailedClaimEmbeddings(ctx, 10, 5)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 0, count)
	})
}

// fakeEmbedder returns a fixed non-zero vector so the probe check in methods
// like RetryFailedClaimEmbeddings passes (NoopProvider returns ErrNoProvider,
// which causes early return).
type fakeEmbedder struct {
	dims int
}

func (f fakeEmbedder) Embed(_ context.Context, _ string) (pgvector.Vector, error) {
	v := make([]float32, f.dims)
	v[0] = 1.0
	return pgvector.NewVector(v), nil
}

func (f fakeEmbedder) EmbedBatch(_ context.Context, texts []string) ([]pgvector.Vector, error) {
	vecs := make([]pgvector.Vector, len(texts))
	for i := range texts {
		v := make([]float32, f.dims)
		v[0] = 1.0
		vecs[i] = pgvector.NewVector(v)
	}
	return vecs, nil
}

func (f fakeEmbedder) Dimensions() int { return f.dims }

// ---------------------------------------------------------------------------
// isDuplicateKey (Service method — delegates to db.IsDuplicateKey)
// ---------------------------------------------------------------------------

func TestServiceIsDuplicateKey(t *testing.T) {
	t.Parallel()

	t.Run("delegates to store returning true", func(t *testing.T) {
		t.Parallel()
		ms := &mockStoreWithDupKey{isDup: true}
		svc := &Service{db: ms}
		assert.True(t, svc.isDuplicateKey(errors.New("some error")))
	})

	t.Run("delegates to store returning false", func(t *testing.T) {
		t.Parallel()
		ms := &mockStoreWithDupKey{isDup: false}
		svc := &Service{db: ms}
		assert.False(t, svc.isDuplicateKey(errors.New("some error")))
	})
}

// mockStoreWithDupKey overrides IsDuplicateKey to return a controlled value.
type mockStoreWithDupKey struct {
	mockStore
	isDup bool
}

func (m *mockStoreWithDupKey) IsDuplicateKey(_ error) bool { return m.isDup }

// ---------------------------------------------------------------------------
// ResolveOrCreateAgent — error paths
// ---------------------------------------------------------------------------

// mockAgentStore extends mockStore with agent-related methods for ResolveOrCreateAgent tests.
type mockAgentStore struct {
	mockStore

	getAgentErr   error
	getAgentAgent model.Agent

	createAgentErr   error
	createAgentAgent model.Agent

	createAgentWithAuditErr   error
	createAgentWithAuditAgent model.Agent

	isDup bool
}

func (m *mockAgentStore) GetAgentByAgentID(_ context.Context, _ uuid.UUID, _ string) (model.Agent, error) {
	return m.getAgentAgent, m.getAgentErr
}

func (m *mockAgentStore) CreateAgent(_ context.Context, agent model.Agent) (model.Agent, error) {
	return m.createAgentAgent, m.createAgentErr
}

func (m *mockAgentStore) CreateAgentWithAudit(_ context.Context, agent model.Agent, _ storage.MutationAuditEntry) (model.Agent, error) {
	return m.createAgentWithAuditAgent, m.createAgentWithAuditErr
}

func (m *mockAgentStore) IsDuplicateKey(_ error) bool { return m.isDup }

func TestResolveOrCreateAgent_DBLookupFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ms := &mockAgentStore{
		getAgentErr: fmt.Errorf("connection refused"),
	}
	svc := &Service{db: ms, logger: testLogger()}

	_, err := svc.ResolveOrCreateAgent(ctx, uuid.Nil, "agent-x", model.RoleAdmin, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused",
		"non-ErrNotFound lookup errors should propagate directly")
}

func TestResolveOrCreateAgent_AutoRegisterFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ms := &mockAgentStore{
		getAgentErr:    storage.ErrNotFound,
		createAgentErr: fmt.Errorf("disk full"),
		isDup:          false,
	}
	svc := &Service{db: ms, logger: testLogger()}

	_, err := svc.ResolveOrCreateAgent(ctx, uuid.Nil, "agent-x", model.RoleAdmin, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auto-register agent")
	assert.Contains(t, err.Error(), "disk full")
}

func TestResolveOrCreateAgent_DuplicateKeyRace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ms := &mockAgentStore{
		getAgentErr:    storage.ErrNotFound,
		createAgentErr: fmt.Errorf("unique constraint violation"),
		isDup:          true, // simulate concurrent creation race
	}
	svc := &Service{db: ms, logger: testLogger()}

	agent, err := svc.ResolveOrCreateAgent(ctx, uuid.Nil, "agent-x", model.RoleAdmin, nil)
	require.NoError(t, err, "duplicate key race should not return an error")
	assert.Equal(t, "", agent.AgentID, "should return zero-value agent on dup key race")
}

func TestResolveOrCreateAgent_WithAudit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	created := model.Agent{AgentID: "agent-new", OrgID: uuid.Nil, Name: "agent-new", Role: model.RoleAgent}
	ms := &mockAgentStore{
		getAgentErr:               storage.ErrNotFound,
		createAgentWithAuditAgent: created,
	}
	svc := &Service{db: ms, logger: testLogger()}

	audit := &storage.MutationAuditEntry{}
	agent, err := svc.ResolveOrCreateAgent(ctx, uuid.Nil, "agent-new", model.RolePlatformAdmin, audit)
	require.NoError(t, err)
	assert.Equal(t, "agent-new", agent.AgentID)
	assert.Equal(t, "agent_auto_registered", audit.Operation,
		"audit entry should be populated with auto-registration metadata")
	assert.Equal(t, "agent", audit.ResourceType)
}

func TestResolveOrCreateAgent_WithAuditFailureDupKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ms := &mockAgentStore{
		getAgentErr:             storage.ErrNotFound,
		createAgentWithAuditErr: fmt.Errorf("unique constraint"),
		isDup:                   true,
	}
	svc := &Service{db: ms, logger: testLogger()}

	audit := &storage.MutationAuditEntry{}
	agent, err := svc.ResolveOrCreateAgent(ctx, uuid.Nil, "agent-dup", model.RoleAdmin, audit)
	require.NoError(t, err, "dup key via audit path should be treated as success")
	assert.Equal(t, "", agent.AgentID, "zero-value agent on dup key race")
}
