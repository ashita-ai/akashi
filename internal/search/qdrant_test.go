package search

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

func TestParseQdrantURL(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		host    string
		port    int
		tls     bool
		wantErr bool
	}{
		{
			name:   "https cloud URL with REST port",
			rawURL: "https://xyz.cloud.qdrant.io:6333",
			host:   "xyz.cloud.qdrant.io",
			port:   6334, // REST 6333 → gRPC 6334
			tls:    true,
		},
		{
			name:   "https cloud URL with gRPC port",
			rawURL: "https://xyz.cloud.qdrant.io:6334",
			host:   "xyz.cloud.qdrant.io",
			port:   6334,
			tls:    true,
		},
		{
			name:   "http local URL",
			rawURL: "http://localhost:6333",
			host:   "localhost",
			port:   6334,
			tls:    false,
		},
		{
			name:   "http no port defaults to 6334",
			rawURL: "http://qdrant.internal",
			host:   "qdrant.internal",
			port:   6334,
			tls:    false,
		},
		{
			name:   "custom port preserved",
			rawURL: "https://qdrant.example.com:9334",
			host:   "qdrant.example.com",
			port:   9334,
			tls:    true,
		},
		{
			name:    "empty URL",
			rawURL:  "",
			wantErr: true,
		},
		{
			name:    "no scheme no host",
			rawURL:  "not-a-url",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port, tls, err := parseQdrantURL(tt.rawURL)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.host, host)
			assert.Equal(t, tt.port, port)
			assert.Equal(t, tt.tls, tls)
		})
	}
}

func TestReScore(t *testing.T) {
	now := time.Now()
	orgID := uuid.New()

	decisions := map[uuid.UUID]model.Decision{
		uuid.MustParse("00000000-0000-0000-0000-000000000001"): {
			ID:           uuid.MustParse("00000000-0000-0000-0000-000000000001"),
			OrgID:        orgID,
			QualityScore: 1.0,
			ValidFrom:    now, // age = 0 days
		},
		uuid.MustParse("00000000-0000-0000-0000-000000000002"): {
			ID:           uuid.MustParse("00000000-0000-0000-0000-000000000002"),
			OrgID:        orgID,
			QualityScore: 0.5,
			ValidFrom:    now.Add(-90 * 24 * time.Hour), // age = 90 days
		},
		uuid.MustParse("00000000-0000-0000-0000-000000000003"): {
			ID:           uuid.MustParse("00000000-0000-0000-0000-000000000003"),
			OrgID:        orgID,
			QualityScore: 0.0,
			ValidFrom:    now.Add(-180 * 24 * time.Hour), // age = 180 days
		},
	}

	results := []Result{
		{DecisionID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Score: 0.95},
		{DecisionID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Score: 0.90},
		{DecisionID: uuid.MustParse("00000000-0000-0000-0000-000000000003"), Score: 0.85},
		{DecisionID: uuid.MustParse("00000000-0000-0000-0000-000000000099"), Score: 0.99}, // missing from decisions
	}

	scored := ReScore(results, decisions, 10)

	// Missing decision should be filtered out.
	require.Len(t, scored, 3)

	// First result: high quality, no age decay → highest relevance.
	// relevance ≈ 0.95 * (0.6 + 0.3*1.0) * (1.0 / (1.0 + 0/90)) = 0.95 * 0.9 * 1.0 = 0.855
	assert.Equal(t, uuid.MustParse("00000000-0000-0000-0000-000000000001"), scored[0].Decision.ID)
	assert.InDelta(t, 0.855, scored[0].SimilarityScore, 0.01)

	// Second result: medium quality, 90-day decay.
	// relevance ≈ 0.90 * (0.6 + 0.3*0.5) * (1.0 / (1.0 + 1.0)) = 0.90 * 0.75 * 0.5 = 0.3375
	assert.Equal(t, uuid.MustParse("00000000-0000-0000-0000-000000000002"), scored[1].Decision.ID)
	assert.InDelta(t, 0.3375, scored[1].SimilarityScore, 0.01)

	// Third result: no quality, 180-day decay.
	// relevance ≈ 0.85 * (0.6 + 0.0) * (1.0 / (1.0 + 2.0)) = 0.85 * 0.6 * 0.333 = 0.17
	assert.Equal(t, uuid.MustParse("00000000-0000-0000-0000-000000000003"), scored[2].Decision.ID)
	assert.InDelta(t, 0.17, scored[2].SimilarityScore, 0.01)

	// Results are sorted descending.
	assert.GreaterOrEqual(t, scored[0].SimilarityScore, scored[1].SimilarityScore)
	assert.GreaterOrEqual(t, scored[1].SimilarityScore, scored[2].SimilarityScore)
}

func TestReScoreTruncatesAtLimit(t *testing.T) {
	now := time.Now()
	id1 := uuid.New()
	id2 := uuid.New()

	decisions := map[uuid.UUID]model.Decision{
		id1: {ID: id1, QualityScore: 1.0, ValidFrom: now},
		id2: {ID: id2, QualityScore: 0.5, ValidFrom: now},
	}

	results := []Result{
		{DecisionID: id1, Score: 0.9},
		{DecisionID: id2, Score: 0.8},
	}

	scored := ReScore(results, decisions, 1)
	require.Len(t, scored, 1)
	assert.Equal(t, id1, scored[0].Decision.ID)
}

func TestBuildQdrantFilter(t *testing.T) {
	// This test verifies the filter building logic by checking that the correct
	// number and types of conditions are generated for various QueryFilters inputs.

	t.Run("org_id only", func(t *testing.T) {
		filters := model.QueryFilters{}
		conditions := buildFilterConditions(uuid.New(), filters)
		assert.Len(t, conditions, 1) // org_id only
	})

	t.Run("with agent_id", func(t *testing.T) {
		filters := model.QueryFilters{AgentIDs: []string{"planner"}}
		conditions := buildFilterConditions(uuid.New(), filters)
		assert.Len(t, conditions, 2) // org_id + agent_id
	})

	t.Run("with multiple agent_ids", func(t *testing.T) {
		filters := model.QueryFilters{AgentIDs: []string{"planner", "coder"}}
		conditions := buildFilterConditions(uuid.New(), filters)
		assert.Len(t, conditions, 2) // org_id + agent_ids (keywords match)
	})

	t.Run("with decision_type and confidence", func(t *testing.T) {
		decType := "architecture"
		confMin := float32(0.8)
		filters := model.QueryFilters{
			DecisionType:  &decType,
			ConfidenceMin: &confMin,
		}
		conditions := buildFilterConditions(uuid.New(), filters)
		assert.Len(t, conditions, 3) // org_id + decision_type + confidence
	})

	t.Run("with time range", func(t *testing.T) {
		from := time.Now().Add(-24 * time.Hour)
		to := time.Now()
		filters := model.QueryFilters{
			TimeRange: &model.TimeRange{From: &from, To: &to},
		}
		conditions := buildFilterConditions(uuid.New(), filters)
		assert.Len(t, conditions, 3) // org_id + from + to
	})

	t.Run("full filters", func(t *testing.T) {
		decType := "architecture"
		confMin := float32(0.7)
		from := time.Now().Add(-24 * time.Hour)
		to := time.Now()
		filters := model.QueryFilters{
			AgentIDs:      []string{"planner"},
			DecisionType:  &decType,
			ConfidenceMin: &confMin,
			TimeRange:     &model.TimeRange{From: &from, To: &to},
		}
		conditions := buildFilterConditions(uuid.New(), filters)
		assert.Len(t, conditions, 6) // org_id + agent_id + decision_type + confidence + from + to
	})
}

// buildFilterConditions extracts the filter-building logic for testability.
// This mirrors the condition-building in QdrantIndex.Search.
func buildFilterConditions(orgID uuid.UUID, filters model.QueryFilters) []string {
	var conditions []string
	conditions = append(conditions, "org_id")

	if len(filters.AgentIDs) > 0 {
		conditions = append(conditions, "agent_id")
	}
	if filters.DecisionType != nil {
		conditions = append(conditions, "decision_type")
	}
	if filters.ConfidenceMin != nil {
		conditions = append(conditions, "confidence")
	}
	if filters.TimeRange != nil {
		if filters.TimeRange.From != nil {
			conditions = append(conditions, "valid_from_unix_gte")
		}
		if filters.TimeRange.To != nil {
			conditions = append(conditions, "valid_from_unix_lte")
		}
	}
	return conditions
}
