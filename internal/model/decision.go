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

	// Quality score (0.0-1.0) measuring trace completeness.
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

	// Joined data (populated by queries, not stored in decisions table).
	Alternatives []Alternative `json:"alternatives,omitempty"`
	Evidence     []Evidence    `json:"evidence,omitempty"`
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

// ConflictKind indicates whether a conflict is between agents or self-contradiction.
type ConflictKind string

const (
	ConflictKindCrossAgent        ConflictKind = "cross_agent"
	ConflictKindSelfContradiction ConflictKind = "self_contradiction"
)

// DecisionConflict represents a detected conflict between two decisions.
type DecisionConflict struct {
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
}
