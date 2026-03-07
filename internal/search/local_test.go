package search

import (
	"context"
	"database/sql"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"

	"github.com/ashita-ai/akashi/internal/model"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// Minimal schema for search tests.
	_, err = db.Exec(`
		CREATE TABLE decisions (
			id TEXT PRIMARY KEY,
			org_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			decision_type TEXT NOT NULL,
			outcome TEXT NOT NULL,
			confidence REAL NOT NULL,
			reasoning TEXT,
			embedding BLOB,
			outcome_embedding BLOB,
			valid_from TEXT NOT NULL,
			valid_to TEXT,
			session_id TEXT,
			tool TEXT,
			model TEXT,
			project TEXT
		)`)
	require.NoError(t, err)
	return db
}

func float32ToBlob(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

func insertDecision(t *testing.T, db *sql.DB, id, orgID uuid.UUID, agentID, decType string, embedding []float32) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO decisions (id, org_id, agent_id, decision_type, outcome, confidence, embedding, valid_from)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id.String(), orgID.String(), agentID, decType, "outcome for "+agentID,
		0.9, float32ToBlob(embedding), time.Now().UTC().Format(time.RFC3339Nano),
	)
	require.NoError(t, err)
}

func TestLocalSearcher_Healthy(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	assert.NoError(t, s.Healthy(context.Background()))
}

func TestLocalSearcher_EmptyEmbedding(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	results, err := s.Search(context.Background(), uuid.New(), nil, model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Nil(t, results)
}

func TestLocalSearcher_NoDecisions(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	query := []float32{1.0, 0, 0}
	results, err := s.Search(context.Background(), uuid.New(), query, model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Nil(t, results)
}

func TestLocalSearcher_CosineSimilarityRanking(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	// Three decisions with different embeddings.
	// Query vector: [1, 0, 0]
	// d1: [1, 0, 0]  → cosine = 1.0 (exact match)
	// d2: [0.7, 0.7, 0] → cosine ≈ 0.707
	// d3: [0, 1, 0]  → cosine = 0.0 (orthogonal)
	d1 := uuid.New()
	d2 := uuid.New()
	d3 := uuid.New()
	insertDecision(t, db, d1, orgID, "agent-a", "arch", []float32{1, 0, 0})
	insertDecision(t, db, d2, orgID, "agent-b", "arch", []float32{0.7, 0.7, 0})
	insertDecision(t, db, d3, orgID, "agent-c", "arch", []float32{0, 1, 0})

	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{}, 10)
	require.NoError(t, err)
	require.Len(t, results, 2, "d3 is orthogonal (score=0), should be excluded")

	// d1 should rank first (exact match).
	assert.Equal(t, d1, results[0].DecisionID)
	assert.InDelta(t, 1.0, results[0].Score, 0.01)

	// d2 should rank second.
	assert.Equal(t, d2, results[1].DecisionID)
	assert.InDelta(t, 0.707, results[1].Score, 0.01)
}

func TestLocalSearcher_RespectLimit(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	for range 5 {
		insertDecision(t, db, uuid.New(), orgID, "agent", "arch", []float32{0.5, 0.5, 0.5})
	}

	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{}, 2)
	require.NoError(t, err)
	assert.Len(t, results, 2)
}

func TestLocalSearcher_FilterByOrg(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgA := uuid.New()
	orgB := uuid.New()
	ctx := context.Background()

	insertDecision(t, db, uuid.New(), orgA, "agent-a", "arch", []float32{1, 0, 0})
	insertDecision(t, db, uuid.New(), orgB, "agent-b", "arch", []float32{1, 0, 0})

	results, err := s.Search(ctx, orgA, []float32{1, 0, 0}, model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1)
}

func TestLocalSearcher_FilterByDecisionType(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	insertDecision(t, db, uuid.New(), orgID, "agent-a", "architecture", []float32{1, 0, 0})
	insertDecision(t, db, uuid.New(), orgID, "agent-b", "code_review", []float32{1, 0, 0})

	dt := "architecture"
	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{DecisionType: &dt}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1)
}

func TestLocalSearcher_SkipSuperseded(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	// Active decision.
	insertDecision(t, db, uuid.New(), orgID, "agent", "arch", []float32{1, 0, 0})

	// Superseded decision (valid_to set).
	supersededID := uuid.New()
	_, err := db.Exec(
		`INSERT INTO decisions (id, org_id, agent_id, decision_type, outcome, confidence, embedding, valid_from, valid_to)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		supersededID.String(), orgID.String(), "agent", "arch", "old decision",
		0.9, float32ToBlob([]float32{1, 0, 0}),
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	require.NoError(t, err)

	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1, "superseded decisions should be excluded")
}

func TestLocalSearcher_FindSimilar(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	srcID := uuid.New()
	otherID := uuid.New()
	insertDecision(t, db, srcID, orgID, "agent-a", "arch", []float32{1, 0, 0})
	insertDecision(t, db, otherID, orgID, "agent-b", "arch", []float32{0.9, 0.1, 0})

	// FindSimilar should exclude srcID.
	results, err := s.FindSimilar(ctx, orgID, []float32{1, 0, 0}, srcID, nil, 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, otherID, results[0].DecisionID)
}

func TestLocalSearcher_DimensionMismatch(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	// Decision has 3D embedding, query is 4D.
	insertDecision(t, db, uuid.New(), orgID, "agent", "arch", []float32{1, 0, 0})
	results, err := s.Search(ctx, orgID, []float32{1, 0, 0, 0}, model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Empty(t, results, "dimension mismatch should be silently skipped")
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float32
	}{
		{"identical", []float32{1, 0, 0}, []float32{1, 0, 0}, 1.0},
		{"orthogonal", []float32{1, 0, 0}, []float32{0, 1, 0}, 0.0},
		{"opposite", []float32{1, 0, 0}, []float32{-1, 0, 0}, -1.0},
		{"45deg", []float32{1, 0}, []float32{1, 1}, float32(1.0 / math.Sqrt(2))},
		{"zero_a", []float32{0, 0}, []float32{1, 1}, 0.0},
		{"zero_b", []float32{1, 1}, []float32{0, 0}, 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cosineSimilarity(tt.a, tt.b)
			assert.InDelta(t, tt.want, got, 0.001)
		})
	}
}

func TestBlobToFloat32(t *testing.T) {
	t.Run("roundtrip", func(t *testing.T) {
		input := []float32{1.5, -2.0, 0.0, 3.14}
		blob := float32ToBlob(input)
		output := blobToFloat32(blob)
		require.Len(t, output, len(input))
		for i := range input {
			assert.InDelta(t, input[i], output[i], 1e-6)
		}
	})

	t.Run("empty", func(t *testing.T) {
		assert.Nil(t, blobToFloat32(nil))
		assert.Nil(t, blobToFloat32([]byte{}))
	})

	t.Run("bad_length", func(t *testing.T) {
		assert.Nil(t, blobToFloat32([]byte{1, 2, 3}))
	})
}
