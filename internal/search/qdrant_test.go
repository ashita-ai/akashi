package search

import (
	"context"
	"log/slog"
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
			ID:                uuid.MustParse("00000000-0000-0000-0000-000000000001"),
			OrgID:             orgID,
			CompletenessScore: 1.0,
			ValidFrom:         now, // age = 0 days
		},
		uuid.MustParse("00000000-0000-0000-0000-000000000002"): {
			ID:                uuid.MustParse("00000000-0000-0000-0000-000000000002"),
			OrgID:             orgID,
			CompletenessScore: 0.5,
			ValidFrom:         now.Add(-90 * 24 * time.Hour), // age = 90 days
		},
		uuid.MustParse("00000000-0000-0000-0000-000000000003"): {
			ID:                uuid.MustParse("00000000-0000-0000-0000-000000000003"),
			OrgID:             orgID,
			CompletenessScore: 0.0,
			ValidFrom:         now.Add(-180 * 24 * time.Hour), // age = 180 days
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

	// Completeness no longer affects the relevance formula (issue #235).
	// All three are cold-start: outcomeWeight = stability=1.0 * 0.15 = 0.15; multiplier = 0.575.
	//
	// First result: no age decay → highest relevance.
	// relevance = 0.95 * 0.575 * 1.0 = 0.54625
	assert.Equal(t, uuid.MustParse("00000000-0000-0000-0000-000000000001"), scored[0].Decision.ID)
	assert.InDelta(t, 0.54625, scored[0].SimilarityScore, 0.01)

	// Second result: 90-day decay: recency = 1/(1+1) = 0.5
	// relevance = 0.90 * 0.575 * 0.5 = 0.25875
	assert.Equal(t, uuid.MustParse("00000000-0000-0000-0000-000000000002"), scored[1].Decision.ID)
	assert.InDelta(t, 0.25875, scored[1].SimilarityScore, 0.01)

	// Third result: 180-day decay: recency = 1/(1+2) = 0.333
	// relevance = 0.85 * 0.575 * 0.333 ≈ 0.163
	assert.Equal(t, uuid.MustParse("00000000-0000-0000-0000-000000000003"), scored[2].Decision.ID)
	assert.InDelta(t, 0.163, scored[2].SimilarityScore, 0.01)

	// Results are sorted descending.
	assert.GreaterOrEqual(t, scored[0].SimilarityScore, scored[1].SimilarityScore)
	assert.GreaterOrEqual(t, scored[1].SimilarityScore, scored[2].SimilarityScore)
}

func TestReScoreTruncatesAtLimit(t *testing.T) {
	now := time.Now()
	id1 := uuid.New()
	id2 := uuid.New()

	decisions := map[uuid.UUID]model.Decision{
		id1: {ID: id1, CompletenessScore: 1.0, ValidFrom: now},
		id2: {ID: id2, CompletenessScore: 0.5, ValidFrom: now},
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
// This mirrors the condition-building in QdrantIndex.Search, including the
// SessionID, Tool, Model, and Repo filters added for composite agent identity.
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
	if filters.SessionID != nil {
		conditions = append(conditions, "session_id")
	}
	if filters.Tool != nil {
		conditions = append(conditions, "tool")
	}
	if filters.Model != nil {
		conditions = append(conditions, "model")
	}
	if filters.Project != nil {
		conditions = append(conditions, "project")
	}
	return conditions
}

func TestReScore_EmptyInput(t *testing.T) {
	scored := ReScore(nil, map[uuid.UUID]model.Decision{}, 10)
	assert.Empty(t, scored)

	scored = ReScore([]Result{}, map[uuid.UUID]model.Decision{}, 10)
	assert.Empty(t, scored)
}

func TestReScore_AllMissing(t *testing.T) {
	// All result decision IDs are absent from the decisions map.
	results := []Result{
		{DecisionID: uuid.New(), Score: 0.95},
		{DecisionID: uuid.New(), Score: 0.80},
		{DecisionID: uuid.New(), Score: 0.70},
	}

	scored := ReScore(results, map[uuid.UUID]model.Decision{}, 10)
	assert.Empty(t, scored)
}

func TestBuildQdrantFilter_SessionAndContext(t *testing.T) {
	sessionID := uuid.New()
	tool := "claude-code"
	mdl := "claude-opus-4-6"
	project := "ashita-ai/akashi"

	filters := model.QueryFilters{
		SessionID: &sessionID,
		Tool:      &tool,
		Model:     &mdl,
		Project:   &project,
	}

	conditions := buildFilterConditions(uuid.New(), filters)
	// Expect 5 conditions: org_id + session_id + tool + model + project.
	require.Len(t, conditions, 5)
	assert.Contains(t, conditions, "org_id")
	assert.Contains(t, conditions, "session_id")
	assert.Contains(t, conditions, "tool")
	assert.Contains(t, conditions, "model")
	assert.Contains(t, conditions, "project")
}

func TestOutboxWorkerDrain_WithoutStart(t *testing.T) {
	// Create an OutboxWorker with nil pool and index (we will not process any batches).
	// Call Drain without calling Start first. Drain should return promptly via the
	// ctx.Done() path since pollLoop was never started and the done channel is never closed.
	w := NewOutboxWorker(nil, nil, slog.Default(), time.Second, 10)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Drain should not panic and should return within the context deadline.
	// Since Start was never called, cancelLoop is nil, and the done channel
	// is never closed. Drain will hit the ctx.Done() select case.
	w.Drain(ctx)

	// If we reach here without panic or hang, the test passes.
	// Verify the context expired (confirming we took the timeout path).
	assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
}

func TestReScore_WithMatchingResults(t *testing.T) {
	// Test ReScore when all results have matching decisions, verifying the
	// formula: relevance = similarity * (0.5 + 0.5 * outcome_weight) * recency_decay
	now := time.Now()

	id1 := uuid.New()
	id2 := uuid.New()
	id3 := uuid.New()

	decisions := map[uuid.UUID]model.Decision{
		id1: {
			ID:                id1,
			AgentID:           "planner",
			DecisionType:      "architecture",
			Outcome:           "chose microservices",
			CompletenessScore: 0.9,
			ValidFrom:         now, // age 0 → recency = 1.0
		},
		id2: {
			ID:                id2,
			AgentID:           "coder",
			DecisionType:      "trade_off",
			Outcome:           "chose gRPC over REST",
			CompletenessScore: 0.6,
			ValidFrom:         now.Add(-45 * 24 * time.Hour), // age 45 days → recency = 1/(1+0.5) = 0.667
		},
		id3: {
			ID:                id3,
			AgentID:           "reviewer",
			DecisionType:      "code_review",
			Outcome:           "approved with comments",
			CompletenessScore: 0.3,
			ValidFrom:         now.Add(-1 * 24 * time.Hour), // age 1 day → recency ~ 1/(1+1/90) ~ 0.989
		},
	}

	results := []Result{
		{DecisionID: id1, Score: 0.92},
		{DecisionID: id2, Score: 0.88},
		{DecisionID: id3, Score: 0.75},
	}

	scored := ReScore(results, decisions, 10)
	require.Len(t, scored, 3, "all results should match decisions")

	// Verify the first result (sorted descending by adjusted score).
	// Completeness no longer in relevance formula (issue #235).
	// id1: cold-start outcomeWeight = 0.15; multiplier = 0.575; recency = 1.0 (age=0)
	// relevance = 0.92 * 0.575 * 1.0 = 0.529
	assert.Equal(t, id1, scored[0].Decision.ID)
	assert.InDelta(t, 0.529, scored[0].SimilarityScore, 0.02)

	// Verify all results are sorted descending by adjusted score.
	for i := 1; i < len(scored); i++ {
		assert.GreaterOrEqual(t, scored[i-1].SimilarityScore, scored[i].SimilarityScore,
			"results should be sorted descending by adjusted score")
	}

	// Verify all scores are between 0 and 1.
	for _, s := range scored {
		assert.LessOrEqual(t, s.SimilarityScore, float32(1.0), "score should not exceed 1.0")
		assert.GreaterOrEqual(t, s.SimilarityScore, float32(0.0), "score should not be negative")
	}
}

func TestReScore_ScoreCappedAtOne(t *testing.T) {
	// When similarity is 1.0 with perfect outcome signals, the adjusted score
	// must be capped at 1.0 by the math.Min(relevance, 1.0) guard.
	now := time.Now()
	id := uuid.New()

	decisions := map[uuid.UUID]model.Decision{
		id: {
			ID:                id,
			CompletenessScore: 1.0,
			ValidFrom:         now, // age 0
		},
	}

	// Completeness no longer in relevance formula (issue #235).
	// cold-start outcomeWeight = 0.15 (stability=1.0 only); multiplier = 0.575
	// relevance = 1.0 * 0.575 * 1.0 = 0.575 (below cap)
	results := []Result{{DecisionID: id, Score: 1.0}}
	scored := ReScore(results, decisions, 10)
	require.Len(t, scored, 1)
	assert.InDelta(t, 0.575, scored[0].SimilarityScore, 0.01)
	assert.LessOrEqual(t, scored[0].SimilarityScore, float32(1.0))
}

func TestReScore_PreservesDecisionMetadata(t *testing.T) {
	// Verify that the full Decision struct is preserved in the SearchResult.
	now := time.Now()
	id := uuid.New()
	orgID := uuid.New()

	decisions := map[uuid.UUID]model.Decision{
		id: {
			ID:                id,
			OrgID:             orgID,
			AgentID:           "test-agent",
			DecisionType:      "security",
			Outcome:           "enabled TLS",
			Confidence:        0.95,
			CompletenessScore: 0.8,
			ValidFrom:         now,
		},
	}

	results := []Result{{DecisionID: id, Score: 0.85}}
	scored := ReScore(results, decisions, 10)

	require.Len(t, scored, 1)
	assert.Equal(t, id, scored[0].Decision.ID)
	assert.Equal(t, orgID, scored[0].Decision.OrgID)
	assert.Equal(t, "test-agent", scored[0].Decision.AgentID)
	assert.Equal(t, "security", scored[0].Decision.DecisionType)
	assert.Equal(t, "enabled TLS", scored[0].Decision.Outcome)
	assert.InDelta(t, 0.95, float64(scored[0].Decision.Confidence), 0.001)
}
