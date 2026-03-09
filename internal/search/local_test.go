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

func strPtr(s string) *string { return &s }

// insertDecisionFull inserts a decision with all optional filter columns populated.
func insertDecisionFull(t *testing.T, db *sql.DB, id, orgID uuid.UUID, agentID, decType string, confidence float64, embedding []float32, validFrom time.Time, sessionID, tool, mdl, project *string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO decisions (id, org_id, agent_id, decision_type, outcome, confidence, embedding, valid_from, session_id, tool, model, project)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.String(), orgID.String(), agentID, decType, "outcome",
		confidence, float32ToBlob(embedding), validFrom.UTC().Format(time.RFC3339Nano),
		sessionID, tool, mdl, project,
	)
	require.NoError(t, err)
}

func TestLocalSearcher_FilterByAgentIDs(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	insertDecision(t, db, uuid.New(), orgID, "agent-keep", "arch", []float32{1, 0, 0})
	insertDecision(t, db, uuid.New(), orgID, "agent-drop", "arch", []float32{1, 0, 0})

	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{AgentIDs: []string{"agent-keep"}}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1, "only the matching agent_id should be returned")
}

func TestLocalSearcher_FilterByAgentIDs_Multiple(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	insertDecision(t, db, uuid.New(), orgID, "agent-a", "arch", []float32{1, 0, 0})
	insertDecision(t, db, uuid.New(), orgID, "agent-b", "arch", []float32{1, 0, 0})
	insertDecision(t, db, uuid.New(), orgID, "agent-c", "arch", []float32{1, 0, 0})

	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{AgentIDs: []string{"agent-a", "agent-b"}}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 2, "two agents should match the IN filter")
}

func TestLocalSearcher_FilterByConfidenceMin(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()
	now := time.Now()

	insertDecisionFull(t, db, uuid.New(), orgID, "a", "arch", 0.5, []float32{1, 0, 0}, now, nil, nil, nil, nil)
	insertDecisionFull(t, db, uuid.New(), orgID, "b", "arch", 0.9, []float32{1, 0, 0}, now, nil, nil, nil, nil)

	minConf := float32(0.8)
	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{ConfidenceMin: &minConf}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1, "only the high-confidence decision should pass the filter")
}

func TestLocalSearcher_FilterBySessionID(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()
	now := time.Now()
	sid := uuid.New()
	sidStr := sid.String()

	insertDecisionFull(t, db, uuid.New(), orgID, "a", "arch", 0.9, []float32{1, 0, 0}, now, &sidStr, nil, nil, nil)
	insertDecisionFull(t, db, uuid.New(), orgID, "b", "arch", 0.9, []float32{1, 0, 0}, now, nil, nil, nil, nil)

	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{SessionID: &sid}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1, "only the matching session_id should be returned")
}

func TestLocalSearcher_FilterByTool(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()
	now := time.Now()

	insertDecisionFull(t, db, uuid.New(), orgID, "a", "arch", 0.9, []float32{1, 0, 0}, now, nil, strPtr("vim"), nil, nil)
	insertDecisionFull(t, db, uuid.New(), orgID, "b", "arch", 0.9, []float32{1, 0, 0}, now, nil, strPtr("emacs"), nil, nil)

	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{Tool: strPtr("vim")}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1, "only the matching tool should be returned")
}

func TestLocalSearcher_FilterByModel(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()
	now := time.Now()

	insertDecisionFull(t, db, uuid.New(), orgID, "a", "arch", 0.9, []float32{1, 0, 0}, now, nil, nil, strPtr("gpt-4"), nil)
	insertDecisionFull(t, db, uuid.New(), orgID, "b", "arch", 0.9, []float32{1, 0, 0}, now, nil, nil, strPtr("claude"), nil)

	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{Model: strPtr("claude")}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1, "only the matching model should be returned")
}

func TestLocalSearcher_FilterByProject(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()
	now := time.Now()

	insertDecisionFull(t, db, uuid.New(), orgID, "a", "arch", 0.9, []float32{1, 0, 0}, now, nil, nil, nil, strPtr("akashi"))
	insertDecisionFull(t, db, uuid.New(), orgID, "b", "arch", 0.9, []float32{1, 0, 0}, now, nil, nil, nil, strPtr("other"))

	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{Project: strPtr("akashi")}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1, "only the matching project should be returned")
}

func TestLocalSearcher_FilterByTimeRange(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	now := time.Now()
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	insertDecisionFull(t, db, uuid.New(), orgID, "a", "arch", 0.9, []float32{1, 0, 0}, old, nil, nil, nil, nil)
	insertDecisionFull(t, db, uuid.New(), orgID, "b", "arch", 0.9, []float32{1, 0, 0}, recent, nil, nil, nil, nil)

	from := now.Add(-2 * time.Hour)
	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{
		TimeRange: &model.TimeRange{From: &from},
	}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1, "only the recent decision should pass the time-from filter")

	to := now.Add(-24 * time.Hour)
	results2, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{
		TimeRange: &model.TimeRange{To: &to},
	}, 10)
	require.NoError(t, err)
	assert.Len(t, results2, 1, "only the old decision should pass the time-to filter")
}

func TestLocalSearcher_FindSimilar_EmptyEmbedding(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	results, err := s.FindSimilar(context.Background(), uuid.New(), nil, uuid.New(), nil, 10)
	require.NoError(t, err)
	assert.Nil(t, results, "empty embedding should return nil")
}

func TestLocalSearcher_FindSimilar_WithProject(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()
	now := time.Now()

	// One decision in the target project, one in a different project, one with NULL project.
	d1 := uuid.New()
	d2 := uuid.New()
	d3 := uuid.New()
	insertDecisionFull(t, db, d1, orgID, "a", "arch", 0.9, []float32{1, 0, 0}, now, nil, nil, nil, strPtr("myproj"))
	insertDecisionFull(t, db, d2, orgID, "b", "arch", 0.9, []float32{1, 0, 0}, now, nil, nil, nil, strPtr("other"))
	insertDecisionFull(t, db, d3, orgID, "c", "arch", 0.9, []float32{1, 0, 0}, now, nil, nil, nil, nil) // NULL project

	proj := "myproj"
	results, err := s.FindSimilar(ctx, orgID, []float32{1, 0, 0}, uuid.Nil, &proj, 10)
	require.NoError(t, err)
	// Should match d1 (project = myproj) and d3 (project IS NULL), but not d2 (project = other).
	assert.Len(t, results, 2, "FindSimilar project filter should include matching project and NULL project")
}

func TestLocalSearcher_DefaultLimit(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	for range 15 {
		insertDecision(t, db, uuid.New(), orgID, "agent", "arch", []float32{0.5, 0.5, 0.5})
	}

	// Passing limit=0 should default to 10.
	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{}, 0)
	require.NoError(t, err)
	assert.Len(t, results, 10, "limit <= 0 should default to 10")
}

func TestLocalSearcher_CombinedFilters(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()
	now := time.Now()

	// Insert decisions with various attributes.
	insertDecisionFull(t, db, uuid.New(), orgID, "agent-a", "arch", 0.95, []float32{1, 0, 0}, now, nil, strPtr("vim"), strPtr("gpt-4"), strPtr("akashi"))
	insertDecisionFull(t, db, uuid.New(), orgID, "agent-a", "security", 0.95, []float32{1, 0, 0}, now, nil, strPtr("vim"), strPtr("gpt-4"), strPtr("akashi"))
	insertDecisionFull(t, db, uuid.New(), orgID, "agent-b", "arch", 0.95, []float32{1, 0, 0}, now, nil, strPtr("vim"), strPtr("gpt-4"), strPtr("akashi"))

	dt := "arch"
	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{
		AgentIDs:     []string{"agent-a"},
		DecisionType: &dt,
		Tool:         strPtr("vim"),
		Model:        strPtr("gpt-4"),
		Project:      strPtr("akashi"),
	}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1, "combined filters should narrow to exactly one decision")
}

// TestLocalSearcher_FindSimilar_NilProject verifies FindSimilar with nil project
// returns all decisions (no project filter applied).
func TestLocalSearcher_FindSimilar_NilProject(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()
	now := time.Now()

	insertDecisionFull(t, db, uuid.New(), orgID, "a", "arch", 0.9, []float32{1, 0, 0}, now, nil, nil, nil, strPtr("proj-a"))
	insertDecisionFull(t, db, uuid.New(), orgID, "b", "arch", 0.9, []float32{1, 0, 0}, now, nil, nil, nil, nil)

	// nil project means no project filter — should return both decisions.
	results, err := s.FindSimilar(ctx, orgID, []float32{1, 0, 0}, uuid.Nil, nil, 10)
	require.NoError(t, err)
	assert.Len(t, results, 2, "nil project should match all decisions regardless of their project value")
}

// TestLocalSearcher_NegativeLimit verifies that a negative limit defaults to 10.
func TestLocalSearcher_NegativeLimit(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	for range 15 {
		insertDecision(t, db, uuid.New(), orgID, "agent", "arch", []float32{0.5, 0.5, 0.5})
	}

	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{}, -1)
	require.NoError(t, err)
	assert.Len(t, results, 10, "negative limit should default to 10")
}

// TestLocalSearcher_AllNegativeScores verifies that decisions with negative cosine
// similarity (score <= 0) are excluded from results.
func TestLocalSearcher_AllNegativeScores(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	// Insert a decision whose embedding is opposite to the query vector.
	insertDecision(t, db, uuid.New(), orgID, "agent", "arch", []float32{-1, 0, 0})

	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Empty(t, results, "decisions with score <= 0 should be excluded")
}

// TestLocalSearcher_NullEmbeddingSkipped verifies that decisions with NULL embedding
// in the database are excluded from loadCandidates.
func TestLocalSearcher_NullEmbeddingSkipped(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	// Insert a decision with no embedding (NULL).
	_, err := db.Exec(
		`INSERT INTO decisions (id, org_id, agent_id, decision_type, outcome, confidence, embedding, valid_from)
		 VALUES (?, ?, ?, ?, ?, ?, NULL, ?)`,
		uuid.New().String(), orgID.String(), "agent", "arch", "outcome", 0.9,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	require.NoError(t, err)

	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Empty(t, results, "decisions with NULL embedding should be excluded")
}

// TestLocalSearcher_EmptyBlobEmbeddingSkipped verifies that decisions with empty
// blob embedding are excluded (blobToFloat32 returns nil for empty blobs).
func TestLocalSearcher_EmptyBlobEmbeddingSkipped(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	// Insert a decision with an empty blob (0 bytes) as embedding.
	_, err := db.Exec(
		`INSERT INTO decisions (id, org_id, agent_id, decision_type, outcome, confidence, embedding, valid_from)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.New().String(), orgID.String(), "agent", "arch", "outcome", 0.9,
		[]byte{},
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	require.NoError(t, err)

	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Empty(t, results, "decisions with empty blob embedding should be excluded")
}

// TestLocalSearcher_FindSimilar_ExcludeNilUUID verifies that FindSimilar with
// uuid.Nil as excludeID does not exclude any decision (the WHERE clause
// uses `id != ?` which is false for uuid.Nil when no decision has that ID).
func TestLocalSearcher_FindSimilar_ExcludeNilUUID(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	insertDecision(t, db, uuid.New(), orgID, "a", "arch", []float32{1, 0, 0})
	insertDecision(t, db, uuid.New(), orgID, "b", "arch", []float32{0.9, 0.1, 0})

	// uuid.Nil excludeID should not exclude any real decision.
	results, err := s.FindSimilar(ctx, orgID, []float32{1, 0, 0}, uuid.Nil, nil, 10)
	require.NoError(t, err)
	assert.Len(t, results, 2, "uuid.Nil excludeID should not exclude any decisions")
}

// TestLocalSearcher_FilterByTimeRangeBothBounds verifies that both from and to
// time range bounds are applied simultaneously.
func TestLocalSearcher_FilterByTimeRangeBothBounds(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	now := time.Now()
	old := now.Add(-72 * time.Hour)
	mid := now.Add(-24 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	insertDecisionFull(t, db, uuid.New(), orgID, "a", "arch", 0.9, []float32{1, 0, 0}, old, nil, nil, nil, nil)
	insertDecisionFull(t, db, uuid.New(), orgID, "b", "arch", 0.9, []float32{1, 0, 0}, mid, nil, nil, nil, nil)
	insertDecisionFull(t, db, uuid.New(), orgID, "c", "arch", 0.9, []float32{1, 0, 0}, recent, nil, nil, nil, nil)

	from := now.Add(-48 * time.Hour)
	to := now.Add(-2 * time.Hour)
	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{
		TimeRange: &model.TimeRange{From: &from, To: &to},
	}, 10)
	require.NoError(t, err)
	assert.Len(t, results, 1, "only the mid-range decision should pass both time bounds")
}

// TestLocalSearcher_MalformedBlobSkipped verifies that decisions with a blob
// whose length is not a multiple of 4 (invalid float32 encoding) are skipped.
func TestLocalSearcher_MalformedBlobSkipped(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	// Insert a decision with a 3-byte blob (not valid float32 encoding).
	_, err := db.Exec(
		`INSERT INTO decisions (id, org_id, agent_id, decision_type, outcome, confidence, embedding, valid_from)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.New().String(), orgID.String(), "agent", "arch", "outcome", 0.9,
		[]byte{0x01, 0x02, 0x03},
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	require.NoError(t, err)

	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Empty(t, results, "decisions with malformed embedding blob should be skipped")
}

// TestLocalSearcher_LargeResultSet verifies that the searcher handles many
// candidates and correctly limits to the requested count, sorted by score.
func TestLocalSearcher_LargeResultSet(t *testing.T) {
	db := openTestDB(t)
	s := NewLocalSearcher(db)
	orgID := uuid.New()
	ctx := context.Background()

	// Insert 25 decisions with slightly varying embeddings.
	for i := range 25 {
		// Vary the x component to produce different cosine similarities.
		x := float32(0.5) + float32(i)*0.02
		insertDecision(t, db, uuid.New(), orgID, "agent", "arch", []float32{x, 0.3, 0.2})
	}

	results, err := s.Search(ctx, orgID, []float32{1, 0, 0}, model.QueryFilters{}, 5)
	require.NoError(t, err)
	assert.Len(t, results, 5, "should limit to 5 results")

	// Verify descending score order.
	for i := 1; i < len(results); i++ {
		assert.GreaterOrEqual(t, results[i-1].Score, results[i].Score,
			"results should be sorted by descending score")
	}
}
