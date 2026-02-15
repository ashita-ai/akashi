package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/model"
)

func TestCompactDecision(t *testing.T) {
	reasoning := "Because Redis handles our expected QPS and TTL prevents stale reads"
	sessionID := uuid.New()
	d := model.Decision{
		ID:           uuid.New(),
		RunID:        uuid.New(),
		AgentID:      "planner",
		OrgID:        uuid.New(),
		DecisionType: "architecture",
		Outcome:      "chose Redis with 5min TTL",
		Confidence:   0.85,
		Reasoning:    &reasoning,
		QualityScore: 0.55,
		ContentHash:  "v2:abc123",
		ValidFrom:    time.Now(),
		CreatedAt:    time.Now(),
		SessionID:    &sessionID,
		AgentContext: map[string]any{"tool": "claude-code", "model": "claude-opus-4-6", "operator": "System Admin"},
	}

	m := compactDecision(d)

	// Kept fields.
	assert.Equal(t, d.ID, m["id"])
	assert.Equal(t, "planner", m["agent_id"])
	assert.Equal(t, "architecture", m["decision_type"])
	assert.Equal(t, "chose Redis with 5min TTL", m["outcome"])
	assert.Equal(t, float32(0.85), m["confidence"])
	assert.Equal(t, reasoning, m["reasoning"])
	assert.Equal(t, &sessionID, m["session_id"])
	assert.Equal(t, "claude-code", m["tool"])
	assert.Equal(t, "claude-opus-4-6", m["model"])

	// Dropped fields.
	_, hasRunID := m["run_id"]
	_, hasOrgID := m["org_id"]
	_, hasQuality := m["quality_score"]
	_, hasContentHash := m["content_hash"]
	_, hasValidFrom := m["valid_from"]
	_, hasMetadata := m["metadata"]
	assert.False(t, hasRunID, "run_id should be dropped")
	assert.False(t, hasOrgID, "org_id should be dropped")
	assert.False(t, hasQuality, "quality_score should be dropped")
	assert.False(t, hasContentHash, "content_hash should be dropped")
	assert.False(t, hasValidFrom, "valid_from should be dropped")
	assert.False(t, hasMetadata, "metadata should be dropped")
}

func TestCompactDecision_TruncatesReasoning(t *testing.T) {
	long := strings.Repeat("x", 300)
	d := model.Decision{
		ID:           uuid.New(),
		AgentID:      "a",
		DecisionType: "t",
		Outcome:      "o",
		Reasoning:    &long,
	}

	m := compactDecision(d)
	r := m["reasoning"].(string)
	assert.True(t, strings.HasSuffix(r, "..."), "should be truncated")
	assert.LessOrEqual(t, len(r), maxCompactReasoning+3, "should be at most maxCompactReasoning + ellipsis")
}

func TestCompactConflict(t *testing.T) {
	cat := "strategic"
	sev := "high"
	expl := "Redis vs in-memory cache disagreement"
	c := model.DecisionConflict{
		ID:                uuid.New(),
		ConflictKind:      model.ConflictKindCrossAgent,
		AgentA:            "planner",
		AgentB:            "coder",
		OutcomeA:          "chose Redis",
		OutcomeB:          "chose in-memory cache",
		TopicSimilarity:   ptrFloat64(0.85),
		OutcomeDivergence: ptrFloat64(0.42),
		Significance:      ptrFloat64(0.36),
		ScoringMethod:     "llm",
		Explanation:       &expl,
		Category:          &cat,
		Severity:          &sev,
		Status:            "open",
		DetectedAt:        time.Now(),
	}

	m := compactConflict(c)

	// Kept fields.
	assert.Equal(t, c.ID, m["id"])
	assert.Equal(t, "planner", m["agent_a"])
	assert.Equal(t, "coder", m["agent_b"])
	assert.Equal(t, "strategic", m["category"])
	assert.Equal(t, "high", m["severity"])
	assert.Equal(t, expl, m["explanation"])
	assert.Equal(t, "open", m["status"])
	assert.Equal(t, "chose Redis", m["outcome_a"])
	assert.Equal(t, "chose in-memory cache", m["outcome_b"])

	// Dropped scoring internals.
	_, hasSim := m["topic_similarity"]
	_, hasDiv := m["outcome_divergence"]
	_, hasSig := m["significance"]
	_, hasMethod := m["scoring_method"]
	assert.False(t, hasSim, "topic_similarity should be dropped")
	assert.False(t, hasDiv, "outcome_divergence should be dropped")
	assert.False(t, hasSig, "significance should be dropped")
	assert.False(t, hasMethod, "scoring_method should be dropped")
}

func TestGenerateCheckSummary_NoPrecedents(t *testing.T) {
	s := generateCheckSummary(nil, nil)
	assert.Contains(t, s, "No prior decisions found")
}

func TestGenerateCheckSummary_WithDecisions(t *testing.T) {
	decs := []model.Decision{
		{Outcome: "chose Redis", Confidence: 0.85, DecisionType: "architecture"},
		{Outcome: "chose PostgreSQL", Confidence: 0.9, DecisionType: "architecture"},
	}
	s := generateCheckSummary(decs, nil)
	assert.Contains(t, s, "2 prior decision(s)")
	assert.Contains(t, s, "chose Redis")
	assert.Contains(t, s, "85%")
}

func TestGenerateCheckSummary_WithConflicts(t *testing.T) {
	sev := "high"
	decs := []model.Decision{
		{Outcome: "chose Redis", Confidence: 0.85, DecisionType: "architecture"},
	}
	conflicts := []model.DecisionConflict{
		{Status: "open", Severity: &sev},
	}
	s := generateCheckSummary(decs, conflicts)
	assert.Contains(t, s, "1 open conflict(s)")
	assert.Contains(t, s, "high")
}

func TestActionNeeded(t *testing.T) {
	critical := "critical"
	high := "high"
	medium := "medium"

	tests := []struct {
		name      string
		conflicts []model.DecisionConflict
		want      bool
	}{
		{"no conflicts", nil, false},
		{"medium only", []model.DecisionConflict{{Status: "open", Severity: &medium}}, false},
		{"high open", []model.DecisionConflict{{Status: "open", Severity: &high}}, true},
		{"critical acknowledged", []model.DecisionConflict{{Status: "acknowledged", Severity: &critical}}, true},
		{"high resolved", []model.DecisionConflict{{Status: "resolved", Severity: &high}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, actionNeeded(tt.conflicts))
		})
	}
}

func TestTruncate(t *testing.T) {
	assert.Equal(t, "hello", truncate("hello", 10))
	assert.Equal(t, "hel...", truncate("hello world", 3))
	assert.Equal(t, "", truncate("", 5))
}

func ptrFloat64(f float64) *float64 { return &f }
