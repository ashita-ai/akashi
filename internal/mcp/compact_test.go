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
		AgentContext:      map[string]any{"tool": "claude-code", "model": "claude-opus-4-6", "operator": "System Admin"},
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

	// UTF-8 safety: CJK characters are multi-byte but should truncate at rune boundaries.
	assert.Equal(t, "日本語...", truncate("日本語テスト", 3))
	assert.Equal(t, "日本語テスト", truncate("日本語テスト", 10)) // under limit, returned as-is

	// Emoji: each emoji is a single rune (may be multi-byte).
	assert.Equal(t, "🎉🎊...", truncate("🎉🎊🎈🎁", 2))

	// Mixed ASCII and multi-byte.
	assert.Equal(t, "ab日...", truncate("ab日本語", 3))
}

// ---------- generateContextNote tests ----------

func TestGenerateContextNote(t *testing.T) {
	vel48 := float64(24)
	vel720 := float64(800)

	tests := []struct {
		name string
		d    model.Decision
		want string
	}{
		{
			name: "no signals fires no rule",
			d:    model.Decision{},
			want: "",
		},
		{
			name: "majority assessed correct",
			d: model.Decision{
				AssessmentSummary: &model.AssessmentSummary{Total: 3, Correct: 2, Incorrect: 1},
			},
			want: "Assessed correct by 2 of 3 agent(s).",
		},
		{
			name: "majority assessed incorrect",
			d: model.Decision{
				AssessmentSummary: &model.AssessmentSummary{Total: 4, Correct: 1, Incorrect: 3},
			},
			want: "Assessed incorrect by 3 of 4 agent(s) — review carefully.",
		},
		{
			name: "assessment total < 2 skipped",
			d: model.Decision{
				AssessmentSummary: &model.AssessmentSummary{Total: 1, Correct: 1},
			},
			want: "",
		},
		{
			name: "assessment tie skips to next rule",
			d: model.Decision{
				AssessmentSummary: &model.AssessmentSummary{Total: 4, Correct: 2, Incorrect: 2},
			},
			want: "",
		},
		{
			name: "revised within 48h and never cited",
			d: model.Decision{
				SupersessionVelocityHours: &vel48,
				PrecedentCitationCount:    0,
			},
			want: "Revised within 24h and never cited as precedent — treat with caution.",
		},
		{
			name: "never superseded and cited 2+ times",
			d: model.Decision{
				PrecedentCitationCount: 3,
			},
			want: "Never superseded. Cited as precedent 3 times.",
		},
		{
			name: "never superseded and won conflict",
			d: model.Decision{
				ConflictFate: model.ConflictFate{Won: 2},
			},
			want: "Never superseded. Won 2 conflict resolution(s).",
		},
		{
			name: "stood for 30+ days",
			d: model.Decision{
				SupersessionVelocityHours: &vel720,
			},
			want: "Stood for 33 days before revision.",
		},
		{
			name: "overridden in conflicts",
			d: model.Decision{
				ConflictFate: model.ConflictFate{Lost: 3, Won: 0},
			},
			want: "Overridden in 3 conflict resolution(s).",
		},
		{
			name: "assessment takes priority over velocity",
			d: model.Decision{
				AssessmentSummary:         &model.AssessmentSummary{Total: 2, Correct: 2},
				SupersessionVelocityHours: &vel48,
			},
			want: "Assessed correct by 2 of 2 agent(s).",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := generateContextNote(tt.d)
			assert.Equal(t, tt.want, got)
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
				Status:   "acknowledged",
			},
		}
		m := compactConflictGroup(g)
		_, hasSev := m["severity"]
		_, hasCat := m["category"]
		_, hasExpl := m["explanation"]
		assert.False(t, hasSev)
		assert.False(t, hasCat)
		assert.False(t, hasExpl)
		assert.Equal(t, "acknowledged", m["status"])
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

// ---------- buildOpenConflictSummary tests ----------

func TestBuildOpenConflictSummary(t *testing.T) {
	t.Run("basic without severity", func(t *testing.T) {
		got := buildOpenConflictSummary(3, "", nil, nil)
		assert.Equal(t, "3 open conflict(s).", got)
	})

	t.Run("with severity", func(t *testing.T) {
		got := buildOpenConflictSummary(2, "critical", nil, nil)
		assert.Equal(t, "2 open conflict(s), highest severity: critical.", got)
	})

	t.Run("with consensus asymmetry", func(t *testing.T) {
		idA := uuid.New()
		idB := uuid.New()

		decisions := []model.Decision{
			{ID: idA, AgreementCount: 5},
			{ID: idB, AgreementCount: 1},
		}
		conflicts := []model.DecisionConflict{
			{
				Status:      "open",
				DecisionAID: idA,
				DecisionBID: idB,
				OutcomeA:    "chose Redis",
				OutcomeB:    "chose Memcached",
			},
		}

		got := buildOpenConflictSummary(1, "", decisions, conflicts)
		assert.Contains(t, got, "5-to-1")
		assert.Contains(t, got, "chose Redis")
	})

	t.Run("no asymmetry falls back to base", func(t *testing.T) {
		idA := uuid.New()
		idB := uuid.New()

		decisions := []model.Decision{
			{ID: idA, AgreementCount: 2},
			{ID: idB, AgreementCount: 2},
		}
		conflicts := []model.DecisionConflict{
			{Status: "open", DecisionAID: idA, DecisionBID: idB},
		}

		got := buildOpenConflictSummary(1, "medium", decisions, conflicts)
		assert.Equal(t, "1 open conflict(s), highest severity: medium.", got)
	})
}

// ---------- decisionAgreementCount tests ----------

func TestDecisionAgreementCount(t *testing.T) {
	idA := uuid.New()
	idB := uuid.New()
	idMissing := uuid.New()

	decisions := []model.Decision{
		{ID: idA, AgreementCount: 7},
		{ID: idB, AgreementCount: 3},
	}

	tests := []struct {
		name string
		id   uuid.UUID
		want int
	}{
		{"found A", idA, 7},
		{"found B", idB, 3},
		{"not found", idMissing, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, decisionAgreementCount(decisions, tt.id))
		})
	}

	t.Run("nil slice", func(t *testing.T) {
		assert.Equal(t, 0, decisionAgreementCount(nil, idA))
	})

	t.Run("empty slice", func(t *testing.T) {
		assert.Equal(t, 0, decisionAgreementCount([]model.Decision{}, idA))
	})
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
	assert.Equal(t, &winID, m["winning_decision_id"])
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

	t.Run("wont_fix counts as resolved", func(t *testing.T) {
		conflicts := []model.DecisionConflict{
			{Status: "wont_fix"},
		}
		s := generateCheckSummary(decs, conflicts)
		assert.Contains(t, s, "1 conflict(s) resolved.")
	})
}

func ptrFloat64(f float64) *float64 { return &f }
