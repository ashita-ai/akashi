package model

import (
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
)

// Decision is a first-class decision entity with bi-temporal modeling.
// Created from DecisionMade events and revised via DecisionRevised events.
type Decision struct {
	ID               uuid.UUID        `json:"id"`
	RunID            uuid.UUID        `json:"run_id"`
	AgentID          string           `json:"agent_id"`
	OrgID            uuid.UUID        `json:"org_id"`
	DecisionType     string           `json:"decision_type"`
	Outcome          string           `json:"outcome"`
	Confidence       float32          `json:"confidence"`
	Reasoning        *string          `json:"reasoning,omitempty"`
	Embedding        *pgvector.Vector `json:"-"`
	OutcomeEmbedding *pgvector.Vector `json:"-"` // Outcome-only embedding for semantic conflict detection.
	Metadata         map[string]any   `json:"metadata"`

	// CompletenessScore (0.0-1.0) measures trace completeness at write time:
	// whether the agent provided reasoning, alternatives, evidence, etc.
	// It does NOT measure whether the decision was correct or adopted.
	CompletenessScore float32 `json:"completeness_score"`

	// QualityScore is a deprecated alias for CompletenessScore. It is emitted
	// alongside completeness_score for one release cycle to give API clients
	// time to migrate. Do not use in new code.
	//
	// Deprecated: use CompletenessScore.
	QualityScore float32 `json:"quality_score"`

	// Precedent reference: decision that influenced this one.
	PrecedentRef *uuid.UUID `json:"precedent_ref,omitempty"`

	// Revision chain: ID of the decision this one supersedes.
	SupersedesID *uuid.UUID `json:"supersedes_id,omitempty"`

	// Tamper-evident SHA-256 content hash of canonical decision fields.
	ContentHash string `json:"content_hash,omitempty"`

	// Bi-temporal columns.
	ValidFrom       time.Time  `json:"valid_from"`
	ValidTo         *time.Time `json:"valid_to,omitempty"`
	TransactionTime time.Time  `json:"transaction_time"`

	CreatedAt time.Time `json:"created_at"`

	// Composite agent identity (Spec 31): multi-dimensional trace attribution.
	SessionID    *uuid.UUID     `json:"session_id,omitempty"`
	AgentContext map[string]any `json:"agent_context,omitempty"`

	// First-class attribution columns (migration 048/052): indexed fast-path for
	// the three most-filtered context fields. Auto-computed from agent_context
	// by generated columns; nil when the context fields were not provided.
	Tool    *string `json:"tool,omitempty"`
	Model   *string `json:"model,omitempty"`
	Project *string `json:"project,omitempty"`

	// API key attribution: which managed key authenticated this decision.
	APIKeyID *uuid.UUID `json:"api_key_id,omitempty"`

	// Joined data (populated by queries, not stored in decisions table).
	Alternatives []Alternative `json:"alternatives,omitempty"`
	Evidence     []Evidence    `json:"evidence,omitempty"`

	// Consensus scoring (Spec 34): computed at query time from embedding similarity cluster.
	// Returns 0 for decisions without embeddings.
	AgreementCount int `json:"agreement_count"`
	ConflictCount  int `json:"conflict_count"`

	// Outcome signals (Spec 35): temporal, graph, and fate signals computed at query time.
	SupersessionVelocityHours *float64     `json:"supersession_velocity"`
	PrecedentCitationCount    int          `json:"precedent_citation_count"`
	ConflictFate              ConflictFate `json:"conflict_fate"`

	// Explicit outcome feedback (Spec 29): assessment counts from agents who
	// observed whether this decision turned out to be correct.
	// Populated on GET /v1/decisions/{id}; nil in list responses.
	AssessmentSummary *AssessmentSummary `json:"assessment_summary,omitempty"`
}

// Alternative represents an option considered for a decision. Immutable.
type Alternative struct {
	ID              uuid.UUID      `json:"id"`
	DecisionID      uuid.UUID      `json:"decision_id"`
	Label           string         `json:"label"`
	Score           *float32       `json:"score,omitempty"`
	Selected        bool           `json:"selected"`
	RejectionReason *string        `json:"rejection_reason,omitempty"`
	Metadata        map[string]any `json:"metadata"`
	CreatedAt       time.Time      `json:"created_at"`
}

// SourceType enumerates valid evidence source types.
type SourceType string

const (
	SourceDocument      SourceType = "document"
	SourceAPIResponse   SourceType = "api_response"
	SourceAgentOutput   SourceType = "agent_output"
	SourceUserInput     SourceType = "user_input"
	SourceSearchResult  SourceType = "search_result"
	SourceToolOutput    SourceType = "tool_output"
	SourceMemory        SourceType = "memory"
	SourceDatabaseQuery SourceType = "database_query"
)

// Evidence represents supporting information for a decision. Immutable.
type Evidence struct {
	ID             uuid.UUID        `json:"id"`
	DecisionID     uuid.UUID        `json:"decision_id"`
	OrgID          uuid.UUID        `json:"org_id"`
	SourceType     SourceType       `json:"source_type"`
	SourceURI      *string          `json:"source_uri,omitempty"`
	Content        string           `json:"content"`
	RelevanceScore *float32         `json:"relevance_score,omitempty"`
	Embedding      *pgvector.Vector `json:"-"`
	Metadata       map[string]any   `json:"metadata"`
	CreatedAt      time.Time        `json:"created_at"`
}

// ConflictFate tracks how a decision fared in resolved conflict pairs.
type ConflictFate struct {
	Won              int `json:"won"`
	Lost             int `json:"lost"`
	ResolvedNoWinner int `json:"resolved_no_winner"`
}

// OutcomeSignals holds all computed outcome signal fields for a decision.
// None of these fields are stored — they are computed at query time.
type OutcomeSignals struct {
	SupersessionVelocityHours *float64
	PrecedentCitationCount    int
	ConflictFate              ConflictFate
	AgreementCount            int
	ConflictCount             int
}

// ConflictKind indicates whether a conflict is between agents or self-contradiction.
type ConflictKind string

const (
	ConflictKindCrossAgent        ConflictKind = "cross_agent"
	ConflictKindSelfContradiction ConflictKind = "self_contradiction"
)

// DecisionConflict represents a detected conflict between two decisions.
type DecisionConflict struct {
	ID                uuid.UUID    `json:"id"`
	ConflictKind      ConflictKind `json:"conflict_kind"` // cross_agent or self_contradiction
	DecisionAID       uuid.UUID    `json:"decision_a_id"`
	DecisionBID       uuid.UUID    `json:"decision_b_id"`
	OrgID             uuid.UUID    `json:"org_id"`
	AgentA            string       `json:"agent_a"`
	AgentB            string       `json:"agent_b"`
	RunA              uuid.UUID    `json:"run_a"`
	RunB              uuid.UUID    `json:"run_b"`
	DecisionType      string       `json:"decision_type"` // Primary for filtering; equals DecisionTypeA
	DecisionTypeA     string       `json:"decision_type_a"`
	DecisionTypeB     string       `json:"decision_type_b"`
	OutcomeA          string       `json:"outcome_a"`
	OutcomeB          string       `json:"outcome_b"`
	ConfidenceA       float32      `json:"confidence_a"`
	ConfidenceB       float32      `json:"confidence_b"`
	ReasoningA        *string      `json:"reasoning_a,omitempty"`
	ReasoningB        *string      `json:"reasoning_b,omitempty"`
	DecidedAtA        time.Time    `json:"decided_at_a"`
	DecidedAtB        time.Time    `json:"decided_at_b"`
	DetectedAt        time.Time    `json:"detected_at"`
	TopicSimilarity   *float64     `json:"topic_similarity,omitempty"`
	OutcomeDivergence *float64     `json:"outcome_divergence,omitempty"`
	Significance      *float64     `json:"significance,omitempty"`
	ScoringMethod     string       `json:"scoring_method,omitempty"`
	Explanation       *string      `json:"explanation,omitempty"`

	// Conflict lifecycle fields: category, severity, and resolution state.
	Category       *string    `json:"category,omitempty"` // factual, assessment, strategic, temporal
	Severity       *string    `json:"severity,omitempty"` // critical, high, medium, low
	Status         string     `json:"status"`             // open, acknowledged, resolved, wont_fix
	ResolvedBy     *string    `json:"resolved_by,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	ResolutionNote *string    `json:"resolution_note,omitempty"`

	// Precision fields (migration 038).
	Relationship         *string    `json:"relationship,omitempty"`           // contradiction, supersession, complementary, refinement, unrelated
	ConfidenceWeight     *float64   `json:"confidence_weight,omitempty"`      // sqrt(confA * confB) scaling factor
	TemporalDecay        *float64   `json:"temporal_decay,omitempty"`         // exp(-lambda * daysBetween) scaling factor
	ResolutionDecisionID *uuid.UUID `json:"resolution_decision_id,omitempty"` // decision that resolved this conflict

	// Winner (migration 046): which of the two decisions prevailed in resolution.
	WinningDecisionID *uuid.UUID `json:"winning_decision_id,omitempty"`
}

// ConflictStatusUpdate is the request body for PATCH /v1/conflicts/{id}.
type ConflictStatusUpdate struct {
	Status         string  `json:"status"` // acknowledged, resolved, wont_fix
	ResolutionNote *string `json:"resolution_note,omitempty"`
	// WinningDecisionID identifies which side prevailed. Only valid when status is
	// "resolved"; must be decision_a_id or decision_b_id of the conflict.
	WinningDecisionID *uuid.UUID `json:"winning_decision_id,omitempty"`
}

// AssessmentOutcome enumerates valid values for DecisionAssessment.Outcome.
type AssessmentOutcome string

const (
	AssessmentCorrect          AssessmentOutcome = "correct"
	AssessmentIncorrect        AssessmentOutcome = "incorrect"
	AssessmentPartiallyCorrect AssessmentOutcome = "partially_correct"
)

// DecisionAssessment is explicit outcome feedback from an agent that observed
// whether a prior decision turned out to be correct. Immutable once written;
// an assessor can revise by upserting (same decision_id + assessor_agent_id).
type DecisionAssessment struct {
	ID              uuid.UUID         `json:"id"`
	DecisionID      uuid.UUID         `json:"decision_id"`
	OrgID           uuid.UUID         `json:"org_id"`
	AssessorAgentID string            `json:"assessor_agent_id"`
	Outcome         AssessmentOutcome `json:"outcome"`
	Notes           *string           `json:"notes,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
}

// AssessmentSummary is a precomputed count of assessments by outcome.
// Returned inline on GET /v1/decisions/{id} and in akashi_check responses.
type AssessmentSummary struct {
	Total            int `json:"total"`
	Correct          int `json:"correct"`
	Incorrect        int `json:"incorrect"`
	PartiallyCorrect int `json:"partially_correct"`
}

// AssessRequest is the request body for POST /v1/decisions/{id}/assess.
type AssessRequest struct {
	Outcome AssessmentOutcome `json:"outcome"`
	Notes   *string           `json:"notes,omitempty"`
}
