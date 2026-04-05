package mcp

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

func TestCompactDecision(t *testing.T) {
	reasoning := "Because Redis handles our expected QPS and TTL prevents stale reads"
	sessionID := uuid.New()
	d := model.Decision{
		ID:                uuid.New(),
		RunID:             uuid.New(),
		AgentID:           "planner",
		OrgID:             uuid.New(),
		DecisionType:      "architecture",
		Outcome:           "chose Redis with 5min TTL",
		Confidence:        0.85,
		Reasoning:         &reasoning,
		CompletenessScore: 0.55,
		ContentHash:       "v2:abc123",
		ValidFrom:         time.Now(),
		CreatedAt:         time.Now(),
		SessionID:         &sessionID,
		AgentContext:      map[string]any{"tool": "claude-code", "model": "claude-opus-4-6", "task": "session infra redesign", "operator": "System Admin"},
	}

	m := compactDecision(d)

	// Kept fields.
	assert.Equal(t, d.ID, m["id"])
	assert.Equal(t, "planner", m["agent_id"])
	assert.Equal(t, "architecture", m["decision_type"])
	assert.Equal(t, "chose Redis with 5min TTL", m["outcome"])
	assert.Equal(t, float32(0.85), m["confidence"])
	assert.Equal(t, reasoning, m["reasoning"])
	assert.Equal(t, sessionID, m["session_id"])
	assert.Equal(t, "claude-code", m["tool"])
	assert.Equal(t, "claude-opus-4-6", m["model"])
	assert.Equal(t, "session infra redesign", m["task"])

	// Dropped fields.
	_, hasRunID := m["run_id"]
	_, hasOrgID := m["org_id"]
	_, hasCompleteness := m["completeness_score"]
	_, hasContentHash := m["content_hash"]
	_, hasValidFrom := m["valid_from"]
	_, hasMetadata := m["metadata"]
	assert.False(t, hasRunID, "run_id should be dropped")
	assert.False(t, hasOrgID, "org_id should be dropped")
	assert.False(t, hasCompleteness, "completeness_score should be dropped")
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

	m := compactConflict(c, "")

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
		{"critical open", []model.DecisionConflict{{Status: "open", Severity: &critical}}, true},
		{"high resolved", []model.DecisionConflict{{Status: "resolved", Severity: &high}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, actionNeeded(tt.conflicts))
		})
	}
}

// ---------- compactConflictGroup tests ----------

func TestCompactConflictGroup(t *testing.T) {
	now := time.Now()
	sev := "high"
	cat := "factual"
	expl := "agents disagree on storage engine"

	t.Run("with representative", func(t *testing.T) {
		g := model.ConflictGroup{
			ID:              uuid.New(),
			AgentA:          "planner",
			AgentB:          "coder",
			ConflictKind:    model.ConflictKindCrossAgent,
			DecisionType:    "architecture",
			ConflictCount:   5,
			OpenCount:       2,
			FirstDetectedAt: now.Add(-24 * time.Hour),
			LastDetectedAt:  now,
			Representative: &model.DecisionConflict{
				Severity:    &sev,
				Category:    &cat,
				Explanation: &expl,
				OutcomeA:    "chose Redis",
				OutcomeB:    "chose Memcached",
				Status:      "open",
			},
		}
		m := compactConflictGroup(g)

		assert.Equal(t, g.ID, m["id"])
		assert.Equal(t, "planner", m["agent_a"])
		assert.Equal(t, "coder", m["agent_b"])
		assert.Equal(t, model.ConflictKindCrossAgent, m["conflict_kind"])
		assert.Equal(t, "architecture", m["decision_type"])
		assert.Equal(t, 5, m["conflict_count"])
		assert.Equal(t, 2, m["open_count"])
		assert.Equal(t, "high", m["severity"])
		assert.Equal(t, "factual", m["category"])
		assert.Equal(t, expl, m["explanation"])
		assert.Equal(t, "chose Redis", m["outcome_a"])
		assert.Equal(t, "chose Memcached", m["outcome_b"])
		assert.Equal(t, "open", m["status"])
	})

	t.Run("without representative", func(t *testing.T) {
		g := model.ConflictGroup{
			ID:              uuid.New(),
			AgentA:          "a",
			AgentB:          "b",
			ConflictKind:    model.ConflictKindSelfContradiction,
			DecisionType:    "security",
			ConflictCount:   1,
			OpenCount:       0,
			FirstDetectedAt: now,
			LastDetectedAt:  now,
		}
		m := compactConflictGroup(g)

		assert.Equal(t, g.ID, m["id"])
		_, hasSev := m["severity"]
		_, hasCat := m["category"]
		_, hasExpl := m["explanation"]
		_, hasStatus := m["status"]
		assert.False(t, hasSev, "no severity without representative")
		assert.False(t, hasCat, "no category without representative")
		assert.False(t, hasExpl, "no explanation without representative")
		assert.False(t, hasStatus, "no status without representative")
	})

	t.Run("representative with nil optional fields", func(t *testing.T) {
		g := model.ConflictGroup{
			ID:           uuid.New(),
			AgentA:       "x",
			AgentB:       "y",
			ConflictKind: model.ConflictKindCrossAgent,
			DecisionType: "deploy",
			Representative: &model.DecisionConflict{
				OutcomeA: "deploy now",
				OutcomeB: "wait",
				Status:   "open",
			},
		}
		m := compactConflictGroup(g)
		_, hasSev := m["severity"]
		_, hasCat := m["category"]
		_, hasExpl := m["explanation"]
		assert.False(t, hasSev)
		assert.False(t, hasCat)
		assert.False(t, hasExpl)
		assert.Equal(t, "open", m["status"])
	})
}

// ---------- compactSearchResult tests ----------

func TestCompactSearchResult(t *testing.T) {
	d := model.Decision{
		ID:           uuid.New(),
		AgentID:      "searcher",
		DecisionType: "architecture",
		Outcome:      "chose PostgreSQL",
		Confidence:   0.9,
		CreatedAt:    time.Now(),
	}
	sr := model.SearchResult{
		Decision:        d,
		SimilarityScore: 0.87,
	}

	m := compactSearchResult(sr)

	// Should include all compactDecision fields plus similarity_score.
	assert.Equal(t, d.ID, m["id"])
	assert.Equal(t, "searcher", m["agent_id"])
	assert.Equal(t, float32(0.87), m["similarity_score"])
}

// ---------- buildConsensusNote tests ----------

func TestBuildConsensusNote(t *testing.T) {
	idA := uuid.New()
	idB := uuid.New()

	c := model.DecisionConflict{
		DecisionAID: idA,
		DecisionBID: idB,
		OutcomeA:    "chose Redis",
		OutcomeB:    "chose Memcached",
	}

	tests := []struct {
		name   string
		counts map[[16]byte]int
		want   string
	}{
		{
			name:   "no asymmetry returns empty",
			counts: map[[16]byte]int{[16]byte(idA): 1, [16]byte(idB): 1},
			want:   "",
		},
		{
			name:   "diff of 1 returns empty",
			counts: map[[16]byte]int{[16]byte(idA): 2, [16]byte(idB): 1},
			want:   "",
		},
		{
			name:   "A has 2 more corroborations",
			counts: map[[16]byte]int{[16]byte(idA): 3, [16]byte(idB): 1},
			want:   `Decision A (chose Redis) has 3 corroborating decision(s). Decision B (chose Memcached) has 1.`,
		},
		{
			name:   "B has 2 more corroborations",
			counts: map[[16]byte]int{[16]byte(idA): 0, [16]byte(idB): 2},
			want:   `Decision B (chose Memcached) has 2 corroborating decision(s). Decision A (chose Redis) has 0.`,
		},
		{
			name:   "missing IDs default to zero",
			counts: map[[16]byte]int{[16]byte(idA): 3},
			want:   `Decision A (chose Redis) has 3 corroborating decision(s). Decision B (chose Memcached) has 0.`,
		},
		{
			name:   "empty map returns empty",
			counts: map[[16]byte]int{},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildConsensusNote(c, tt.counts)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------- compactResolution tests ----------

func TestCompactResolution(t *testing.T) {
	now := time.Now()
	expl := "Redis is better for this use case"
	note := "Confirmed by team lead"
	winID := uuid.New()

	t.Run("full resolution", func(t *testing.T) {
		r := model.ConflictResolution{
			DecisionType:      "architecture",
			WinningDecisionID: winID,
			WinningAgent:      "planner",
			WinningOutcome:    "chose Redis",
			LosingAgent:       "coder",
			LosingOutcome:     "chose Memcached",
			Explanation:       &expl,
			ResolutionNote:    &note,
			ResolvedAt:        now,
		}
		m := compactResolution(r)

		assert.Equal(t, "architecture", m["decision_type"])
		assert.Equal(t, winID, m["winning_decision_id"])
		assert.Equal(t, "planner", m["winning_agent"])
		assert.Equal(t, "chose Redis", m["winning_outcome"])
		assert.Equal(t, "coder", m["losing_agent"])
		assert.Equal(t, "chose Memcached", m["losing_outcome"])
		assert.Equal(t, expl, m["explanation"])
		assert.Equal(t, note, m["resolution_note"])
		assert.Equal(t, now, m["resolved_at"])
	})

	t.Run("nil optional fields omitted", func(t *testing.T) {
		r := model.ConflictResolution{
			DecisionType:      "security",
			WinningDecisionID: winID,
			WinningAgent:      "auditor",
			WinningOutcome:    "enforce mTLS",
			LosingAgent:       "developer",
			LosingOutcome:     "skip mTLS",
			ResolvedAt:        now,
		}
		m := compactResolution(r)

		_, hasExpl := m["explanation"]
		_, hasNote := m["resolution_note"]
		assert.False(t, hasExpl, "nil explanation should be omitted")
		assert.False(t, hasNote, "nil resolution_note should be omitted")
	})

	t.Run("empty string optional fields omitted", func(t *testing.T) {
		empty := ""
		r := model.ConflictResolution{
			DecisionType:      "deploy",
			WinningDecisionID: winID,
			WinningAgent:      "a",
			WinningOutcome:    "o",
			LosingAgent:       "b",
			LosingOutcome:     "p",
			Explanation:       &empty,
			ResolutionNote:    &empty,
			ResolvedAt:        now,
		}
		m := compactResolution(r)

		_, hasExpl := m["explanation"]
		_, hasNote := m["resolution_note"]
		assert.False(t, hasExpl, "empty explanation should be omitted")
		assert.False(t, hasNote, "empty resolution_note should be omitted")
	})

	t.Run("long outcomes are truncated", func(t *testing.T) {
		longOutcome := strings.Repeat("a", 300)
		r := model.ConflictResolution{
			DecisionType:      "test",
			WinningDecisionID: winID,
			WinningAgent:      "a",
			WinningOutcome:    longOutcome,
			LosingAgent:       "b",
			LosingOutcome:     longOutcome,
			ResolvedAt:        now,
		}
		m := compactResolution(r)

		winOut := m["winning_outcome"].(string)
		loseOut := m["losing_outcome"].(string)
		assert.True(t, strings.HasSuffix(winOut, "..."), "winning outcome should be truncated")
		assert.True(t, strings.HasSuffix(loseOut, "..."), "losing outcome should be truncated")
		assert.LessOrEqual(t, len(winOut), maxCompactReasoning+3)
	})
}

// ---------- compactDecision consensus_weight tests ----------

func TestCompactDecision_ConsensusWeight(t *testing.T) {
	t.Run("no consensus data omits weight", func(t *testing.T) {
		d := model.Decision{
			ID: uuid.New(), AgentID: "a", DecisionType: "t", Outcome: "o",
			AgreementCount: 0, ConflictCount: 0,
		}
		m := compactDecision(d)
		_, has := m["consensus_weight"]
		assert.False(t, has, "consensus_weight should be omitted when total is 0")
	})

	t.Run("all agreements gives weight 1.0", func(t *testing.T) {
		d := model.Decision{
			ID: uuid.New(), AgentID: "a", DecisionType: "t", Outcome: "o",
			AgreementCount: 5, ConflictCount: 0,
		}
		m := compactDecision(d)
		assert.Equal(t, 1.0, m["consensus_weight"])
	})

	t.Run("all conflicts gives weight 0.5", func(t *testing.T) {
		d := model.Decision{
			ID: uuid.New(), AgentID: "a", DecisionType: "t", Outcome: "o",
			AgreementCount: 0, ConflictCount: 3,
		}
		m := compactDecision(d)
		assert.Equal(t, 0.5, m["consensus_weight"])
	})
}

// ---------- compactDecision assessment_summary tests ----------

func TestCompactDecision_AssessmentSummary(t *testing.T) {
	t.Run("nil assessment omits field", func(t *testing.T) {
		d := model.Decision{
			ID: uuid.New(), AgentID: "a", DecisionType: "t", Outcome: "o",
		}
		m := compactDecision(d)
		_, has := m["assessment_summary"]
		assert.False(t, has)
	})

	t.Run("zero total assessment omits field", func(t *testing.T) {
		d := model.Decision{
			ID: uuid.New(), AgentID: "a", DecisionType: "t", Outcome: "o",
			AssessmentSummary: &model.AssessmentSummary{Total: 0},
		}
		m := compactDecision(d)
		_, has := m["assessment_summary"]
		assert.False(t, has)
	})

	t.Run("populated assessment included", func(t *testing.T) {
		d := model.Decision{
			ID: uuid.New(), AgentID: "a", DecisionType: "t", Outcome: "o",
			AssessmentSummary: &model.AssessmentSummary{Total: 5, Correct: 3, Incorrect: 1, PartiallyCorrect: 1},
		}
		m := compactDecision(d)
		as, ok := m["assessment_summary"].(map[string]any)
		assert.True(t, ok)
		assert.Equal(t, 5, as["total"])
		assert.Equal(t, 3, as["correct"])
		assert.Equal(t, 1, as["incorrect"])
		assert.Equal(t, 1, as["partially_correct"])
	})
}

// ---------- compactConflict with consensus note ----------

func TestCompactConflict_WithConsensusNote(t *testing.T) {
	c := model.DecisionConflict{
		ID:       uuid.New(),
		AgentA:   "a",
		AgentB:   "b",
		OutcomeA: "x",
		OutcomeB: "y",
		Status:   "open",
	}
	note := "Decision A has 5 corroborating decision(s)."
	m := compactConflict(c, note)
	assert.Equal(t, note, m["consensus_note"])
}

func TestCompactConflict_WinningDecisionID(t *testing.T) {
	winID := uuid.New()
	c := model.DecisionConflict{
		ID:                uuid.New(),
		AgentA:            "a",
		AgentB:            "b",
		OutcomeA:          "x",
		OutcomeB:          "y",
		Status:            "resolved",
		WinningDecisionID: &winID,
	}
	m := compactConflict(c, "")
	assert.Equal(t, winID, m["winning_decision_id"])
}

func TestCompactConflict_NilOptionalFields(t *testing.T) {
	c := model.DecisionConflict{
		ID:       uuid.New(),
		AgentA:   "a",
		AgentB:   "b",
		OutcomeA: "x",
		OutcomeB: "y",
		Status:   "open",
	}
	m := compactConflict(c, "")

	_, hasCat := m["category"]
	_, hasSev := m["severity"]
	_, hasExpl := m["explanation"]
	_, hasWin := m["winning_decision_id"]
	_, hasNote := m["consensus_note"]
	assert.False(t, hasCat)
	assert.False(t, hasSev)
	assert.False(t, hasExpl)
	assert.False(t, hasWin)
	assert.False(t, hasNote)
}

// ---------- generateCheckSummary edge cases ----------

func TestGenerateCheckSummary_MultipleTypes(t *testing.T) {
	decs := []model.Decision{
		{Outcome: "chose Redis", Confidence: 0.85, DecisionType: "architecture"},
		{Outcome: "enforce mTLS", Confidence: 0.9, DecisionType: "security"},
	}
	s := generateCheckSummary(decs, nil)
	assert.Contains(t, s, "2 prior decisions across 2 types")
}

func TestGenerateCheckSummary_ResolvedConflicts(t *testing.T) {
	decs := []model.Decision{{Outcome: "x", Confidence: 0.8, DecisionType: "t"}}
	winID := uuid.New()

	t.Run("resolved with winner", func(t *testing.T) {
		conflicts := []model.DecisionConflict{
			{Status: "resolved", WinningDecisionID: &winID},
		}
		s := generateCheckSummary(decs, conflicts)
		assert.Contains(t, s, "1 conflict(s) resolved with winner declared")
	})

	t.Run("resolved without winner", func(t *testing.T) {
		conflicts := []model.DecisionConflict{
			{Status: "resolved"},
		}
		s := generateCheckSummary(decs, conflicts)
		assert.Contains(t, s, "1 conflict(s) resolved.")
	})

	t.Run("false_positive counts as resolved", func(t *testing.T) {
		conflicts := []model.DecisionConflict{
			{Status: "false_positive"},
		}
		s := generateCheckSummary(decs, conflicts)
		assert.Contains(t, s, "1 conflict(s) resolved.")
	})
}

func ptrFloat64(f float64) *float64 { return &f }

// TestGenerateCheckSummary_OutcomeSignals verifies that outcome signals are
// appended to the most recent decision summary line.
func TestGenerateCheckSummary_OutcomeSignals(t *testing.T) {
	correct := 3
	decs := []model.Decision{
		{
			Outcome:                "chose Redis",
			Confidence:             0.85,
			DecisionType:           "architecture",
			PrecedentCitationCount: 3,
			AssessmentSummary:      &model.AssessmentSummary{Total: 5, Correct: correct},
		},
	}
	s := generateCheckSummary(decs, nil)
	assert.Contains(t, s, "cited 3 times", "should include citation count")
	assert.Contains(t, s, "assessed correct 3/5", "should include assessment summary")
	assert.Contains(t, s, "never superseded", "should note never superseded")
}

// TestGenerateCheckSummary_SupersededDecision verifies that superseded decisions
// do NOT get the "never superseded" signal.
func TestGenerateCheckSummary_SupersededDecision(t *testing.T) {
	vel := float64(48)
	decs := []model.Decision{
		{
			Outcome:                   "chose Redis",
			Confidence:                0.85,
			DecisionType:              "architecture",
			SupersessionVelocityHours: &vel,
		},
	}
	s := generateCheckSummary(decs, nil)
	assert.NotContains(t, s, "never superseded",
		"superseded decisions should not get the 'never superseded' signal")
}

// TestGenerateCheckSummary_OpenAndResolvedMixed verifies that when both open and
// resolved conflicts exist, the open ones take priority in the summary.
func TestGenerateCheckSummary_OpenAndResolvedMixed(t *testing.T) {
	sev := "critical"
	winID := uuid.New()
	decs := []model.Decision{
		{Outcome: "x", Confidence: 0.8, DecisionType: "t"},
	}
	conflicts := []model.DecisionConflict{
		{Status: "open", Severity: &sev},
		{Status: "resolved", WinningDecisionID: &winID},
	}
	s := generateCheckSummary(decs, conflicts)
	assert.Contains(t, s, "1 open conflict(s)")
	assert.Contains(t, s, "critical")
	assert.NotContains(t, s, "resolved with winner",
		"open conflicts should take priority over resolved in summary")
}

// TestCompactDecision_ContextNote verifies that context_note is included when
// the decision has relevant outcome signals.
func TestCompactDecision_ContextNote(t *testing.T) {
	d := model.Decision{
		ID:                     uuid.New(),
		AgentID:                "agent",
		DecisionType:           "architecture",
		Outcome:                "chose Redis",
		Confidence:             0.9,
		PrecedentCitationCount: 3,
		// vel is nil → "never superseded" rule fires
	}
	m := compactDecision(d)
	note, ok := m["context_note"]
	assert.True(t, ok, "context_note should be present for decisions with citations")
	assert.Contains(t, note, "Cited as precedent 3 times")
}

// TestCompactDecision_TaskIncludedWhenSet verifies task is surfaced from AgentContext.
func TestCompactDecision_TaskIncludedWhenSet(t *testing.T) {
	d := model.Decision{
		ID:           uuid.New(),
		AgentID:      "coder",
		DecisionType: "architecture",
		Outcome:      "chose Redis",
		AgentContext: map[string]any{"task": "codebase review"},
	}
	m := compactDecision(d)
	assert.Equal(t, "codebase review", m["task"])
}

// TestCompactDecision_TaskOmittedWhenAbsent verifies task is not in output when not in AgentContext.
func TestCompactDecision_TaskOmittedWhenAbsent(t *testing.T) {
	d := model.Decision{
		ID:           uuid.New(),
		AgentID:      "coder",
		DecisionType: "architecture",
		Outcome:      "chose Redis",
		AgentContext: map[string]any{"tool": "claude-code"},
	}
	m := compactDecision(d)
	_, hasTask := m["task"]
	assert.False(t, hasTask, "task should be omitted when not in agent_context")
}

// TestCompactDecision_NoContextNoteWhenNoSignals verifies that context_note is
// omitted when no outcome signal rules fire.
func TestCompactDecision_NoContextNoteWhenNoSignals(t *testing.T) {
	d := model.Decision{
		ID:           uuid.New(),
		AgentID:      "agent",
		DecisionType: "architecture",
		Outcome:      "chose Redis",
		Confidence:   0.9,
	}
	m := compactDecision(d)
	_, ok := m["context_note"]
	assert.False(t, ok, "context_note should be omitted when no rules fire")
}

// TestActionNeeded_NilSeverity verifies that conflicts with nil severity
// do not trigger action_needed.
func TestActionNeeded_NilSeverity(t *testing.T) {
	conflicts := []model.DecisionConflict{
		{Status: "open", Severity: nil},
	}
	assert.False(t, actionNeeded(conflicts), "nil severity should not trigger action_needed")
}

// TestActionNeeded_LowSeverity verifies that low-severity open conflicts
// do not trigger action_needed.
func TestActionNeeded_LowSeverity(t *testing.T) {
	low := "low"
	conflicts := []model.DecisionConflict{
		{Status: "open", Severity: &low},
	}
	assert.False(t, actionNeeded(conflicts), "low severity should not trigger action_needed")
}

// ---------- rootsCache tests ----------

func TestRootsCache_GetSetRoundTrip(t *testing.T) {
	rc := newRootsCache()
	require.NotNil(t, rc)

	// Get before set returns nil and false.
	roots, ok := rc.Get("session-1")
	assert.False(t, ok)
	assert.Nil(t, roots)

	// Set and retrieve.
	expected := []mcplib.Root{
		{URI: "file:///home/user/project"},
		{URI: "file:///home/user/other"},
	}
	rc.Set("session-1", expected)

	roots, ok = rc.Get("session-1")
	assert.True(t, ok)
	assert.Equal(t, expected, roots)
}

func TestRootsCache_EmptySliceDistinctFromMissing(t *testing.T) {
	rc := newRootsCache()

	// Cache an empty slice (client doesn't support roots).
	rc.Set("session-2", []mcplib.Root{})

	roots, ok := rc.Get("session-2")
	assert.True(t, ok, "empty slice should be cached and distinguishable from missing")
	assert.Empty(t, roots)
}

func TestRootsCache_MultipleSessionsIndependent(t *testing.T) {
	rc := newRootsCache()

	rc.Set("s1", []mcplib.Root{{URI: "file:///a"}})
	rc.Set("s2", []mcplib.Root{{URI: "file:///b"}})

	r1, _ := rc.Get("s1")
	r2, _ := rc.Get("s2")
	assert.Equal(t, "file:///a", r1[0].URI)
	assert.Equal(t, "file:///b", r2[0].URI)
}

// ---------- inferProjectFromRoots edge cases ----------

func TestInferProjectFromRoots_NonFileScheme(t *testing.T) {
	roots := []mcplib.Root{
		{URI: "https://github.com/org/repo"},
		{URI: "file:///home/user/project"},
	}
	result := inferProjectFromRoots(roots)
	assert.Equal(t, "project", result, "should skip non-file URIs and use the file:// root")
}

func TestInferProjectFromRoots_EmptyRoots(t *testing.T) {
	assert.Equal(t, "", inferProjectFromRoots(nil))
	assert.Equal(t, "", inferProjectFromRoots([]mcplib.Root{}))
}

func TestInferProjectFromRoots_RootPath(t *testing.T) {
	roots := []mcplib.Root{{URI: "file:///"}}
	assert.Equal(t, "", inferProjectFromRoots(roots), "root path should return empty")
}

func TestInferProjectFromRoots_InvalidURI(t *testing.T) {
	roots := []mcplib.Root{{URI: "file://\x00invalid"}}
	// Should not panic; returns empty string.
	result := inferProjectFromRoots(roots)
	assert.Equal(t, "", result)
}

// ---------- rootURIs tests ----------

func TestRootURIs_EmptyReturnsNil(t *testing.T) {
	assert.Nil(t, rootURIs(nil))
	assert.Nil(t, rootURIs([]mcplib.Root{}))
}

func TestRootURIs_ExtractsURIs(t *testing.T) {
	roots := []mcplib.Root{
		{URI: "file:///a"},
		{URI: "file:///b"},
		{URI: "https://example.com"},
	}
	uris := rootURIs(roots)
	require.Len(t, uris, 3)
	assert.Equal(t, "file:///a", uris[0])
	assert.Equal(t, "file:///b", uris[1])
	assert.Equal(t, "https://example.com", uris[2])
}

// ---------- inferProjectFromRootsWithGit tests ----------

func TestInferProjectFromRootsWithGit_NonGitDirReturnsEmpty(t *testing.T) {
	// Use a path that is definitely not a git repo.
	tmpDir := t.TempDir()
	roots := []mcplib.Root{
		{URI: "file://" + tmpDir},
	}
	result := inferProjectFromRootsWithGit(roots)
	// Must NOT fall back to directory basename — that leaks workspace names.
	assert.Empty(t, result)
}

func TestInferProjectFromRootsWithGit_EmptyRoots(t *testing.T) {
	assert.Equal(t, "", inferProjectFromRootsWithGit(nil))
}

func TestInferProjectFromRootsWithGit_SkipsNonFileRoots(t *testing.T) {
	roots := []mcplib.Root{
		{URI: "https://example.com/repo"},
	}
	assert.Equal(t, "", inferProjectFromRootsWithGit(roots))
}

// ---------- gitRepoName tests ----------

func TestGitRepoName_NonGitDir(t *testing.T) {
	// A temp dir with no git repo should return empty.
	assert.Equal(t, "", gitRepoName(t.TempDir()))
}

func TestGitRepoName_NonExistentDir(t *testing.T) {
	assert.Equal(t, "", gitRepoName("/nonexistent/path/that/does/not/exist"))
}

func TestGitRepoName_RealGitRepo(t *testing.T) {
	// This test runs inside the akashi repo, which is a git repo with a remote.
	// The gitRepoName function should extract "kyoto" or the actual repo name
	// from the remote URL.
	name := gitRepoName(".")
	// We're in a git repo, so name should be non-empty.
	assert.NotEmpty(t, name, "gitRepoName should extract a name from the current git repo")
}

func TestInferProjectFromRootsWithGit_RealGitRepo(t *testing.T) {
	// Use the current working directory (which is inside a git repo) to test
	// the git detection path rather than the fallback-to-basename path.
	cwd, err := os.Getwd()
	require.NoError(t, err)
	roots := []mcplib.Root{
		{URI: "file://" + cwd},
	}
	result := inferProjectFromRootsWithGit(roots)
	assert.NotEmpty(t, result, "should detect project name from git repo")
}

func TestInferProjectFromRootsWithGit_RootPathIsSlash(t *testing.T) {
	// A file:// URI pointing to "/" should be skipped (path == "/" guard).
	roots := []mcplib.Root{
		{URI: "file:///"},
	}
	result := inferProjectFromRootsWithGit(roots)
	assert.Equal(t, "", result, "root path '/' should be skipped")
}
