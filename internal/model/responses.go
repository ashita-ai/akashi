package model

import (
	"time"

	"github.com/google/uuid"
)

// TraceResponse is the response for POST /v1/trace.
type TraceResponse struct {
	RunID      uuid.UUID `json:"run_id"`
	DecisionID uuid.UUID `json:"decision_id"`
	EventCount int       `json:"event_count"`

	EmbeddingSkipped   bool     `json:"embedding_skipped,omitempty"`
	Warnings           []string `json:"warnings,omitempty"`
	ConfidenceAdjusted bool     `json:"confidence_adjusted,omitempty"`
	OriginalConfidence float32  `json:"original_confidence,omitempty"`
	StoredConfidence   float32  `json:"stored_confidence,omitempty"`
	ConfidenceReasons  []string `json:"confidence_reasons,omitempty"`
}

// TemporalQueryResponse is the response for POST /v1/query/temporal.
type TemporalQueryResponse struct {
	AsOf      time.Time  `json:"as_of"`
	Decisions []Decision `json:"decisions"`
}

// DecisionRevisionsResponse is the response for GET /v1/decisions/{id}/revisions.
type DecisionRevisionsResponse struct {
	DecisionID uuid.UUID  `json:"decision_id"`
	Revisions  []Decision `json:"revisions"`
	Count      int        `json:"count"`
}

// VerifyDecisionResponse is the response for GET /v1/verify/{id}.
type VerifyDecisionResponse struct {
	DecisionID  uuid.UUID `json:"decision_id"`
	Status      string    `json:"status"`
	Verified    *bool     `json:"verified,omitempty"`
	Valid       *bool     `json:"valid,omitempty"`
	ContentHash string    `json:"content_hash,omitempty"`
	Message     string    `json:"message,omitempty"`
	RetractedAt string    `json:"retracted_at,omitempty"`

	// Erasure-specific fields.
	OriginalHash string     `json:"original_hash,omitempty"`
	ErasedAt     *time.Time `json:"erased_at,omitempty"`
	ErasedBy     string     `json:"erased_by,omitempty"`
}

// IntegrityViolationsResponse is the response for GET /v1/integrity/violations.
type IntegrityViolationsResponse struct {
	Violations any `json:"violations"`
	Count      int `json:"count"`
}

// SessionViewSummary contains aggregate stats for a session.
type SessionViewSummary struct {
	StartedAt     time.Time      `json:"started_at"`
	EndedAt       time.Time      `json:"ended_at"`
	DurationSecs  float64        `json:"duration_secs"`
	DecisionTypes map[string]int `json:"decision_types"`
	AvgConfidence float64        `json:"avg_confidence"`
}

// SessionViewResponse is the response for GET /v1/sessions/{session_id}.
type SessionViewResponse struct {
	SessionID     uuid.UUID           `json:"session_id"`
	Decisions     []Decision          `json:"decisions"`
	DecisionCount int                 `json:"decision_count"`
	Summary       *SessionViewSummary `json:"summary,omitempty"`
}

// EraseDecisionResponse is the response for POST /v1/decisions/{id}/erase.
type EraseDecisionResponse struct {
	DecisionID         uuid.UUID  `json:"decision_id"`
	ErasedAt           *time.Time `json:"erased_at"`
	OriginalHash       string     `json:"original_hash"`
	ErasedHash         string     `json:"erased_hash"`
	AlternativesErased int64      `json:"alternatives_erased"`
	EvidenceErased     int64      `json:"evidence_erased"`
	ClaimsErased       int64      `json:"claims_erased"`
}

// CreateAgentAPIKeyInfo is the nested api_key in the CreateAgentResponse.
type CreateAgentAPIKeyInfo struct {
	ID     uuid.UUID `json:"id"`
	Prefix string    `json:"prefix"`
}

// CreateAgentResponse is the response for POST /v1/agents.
type CreateAgentResponse struct {
	Agent  Agent                 `json:"agent"`
	APIKey CreateAgentAPIKeyInfo `json:"api_key"`
	RawKey string                `json:"raw_key,omitempty"`
}

// AgentStatsResponse is the response for GET /v1/agents/{agent_id}/stats.
type AgentStatsResponse struct {
	AgentID string `json:"agent_id"`
	Stats   any    `json:"stats"`
}

// DeleteAgentResponse is the response for DELETE /v1/agents/{agent_id}.
type DeleteAgentResponse struct {
	AgentID string `json:"agent_id"`
	Deleted any    `json:"deleted"`
}

// UsageByKey is a single API key's usage in the usage response.
type UsageByKey struct {
	KeyID   *uuid.UUID `json:"key_id"`
	Prefix  string     `json:"prefix"`
	Label   string     `json:"label"`
	AgentID string     `json:"agent_id"`
	Count   int        `json:"decisions"`
}

// UsageByAgent is a single agent's usage in the usage response.
type UsageByAgent struct {
	AgentID   string `json:"agent_id"`
	Decisions int    `json:"decisions"`
}

// GetUsageResponse is the response for GET /v1/usage.
type GetUsageResponse struct {
	OrgID          uuid.UUID      `json:"org_id"`
	Period         string         `json:"period"`
	TotalDecisions int            `json:"total_decisions"`
	ByKey          []UsageByKey   `json:"by_key"`
	ByAgent        []UsageByAgent `json:"by_agent"`
}

// RetentionPolicyResponse is the response for GET/PUT /v1/retention.
type RetentionPolicyResponse struct {
	RetentionDays         *int       `json:"retention_days"`
	RetentionExcludeTypes []string   `json:"retention_exclude_types"`
	LastRun               *time.Time `json:"last_run"`
	LastRunDeleted        *int       `json:"last_run_deleted"`
	NextRun               *time.Time `json:"next_run"`
	Holds                 any        `json:"holds,omitempty"`
}

// PurgeResponse is the response for POST /v1/retention/purge.
type PurgeResponse struct {
	DryRun      bool `json:"dry_run"`
	Deleted     any  `json:"deleted,omitempty"`
	WouldDelete any  `json:"would_delete,omitempty"`
}

// DecisionProofResponse is the response for GET /v1/integrity/proof/{id}.
type DecisionProofResponse struct {
	DecisionID  uuid.UUID `json:"decision_id"`
	ContentHash string    `json:"content_hash"`
	ProofID     uuid.UUID `json:"proof_id"`
	RootHash    string    `json:"root_hash"`
	BatchStart  time.Time `json:"batch_start"`
	BatchEnd    time.Time `json:"batch_end"`
	ProofPath   any       `json:"proof_path"`
	Verified    bool      `json:"verified"`
}

// AssessmentListResponse is the response for GET /v1/decisions/{id}/assessments.
type AssessmentListResponse struct {
	DecisionID  uuid.UUID            `json:"decision_id"`
	Summary     AssessmentSummary    `json:"summary"`
	Assessments []DecisionAssessment `json:"assessments"`
	Count       int                  `json:"count"`
}

// GrantAllProjectLinksResponse is the response for POST /v1/project-links/grant-all.
type GrantAllProjectLinksResponse struct {
	LinksCreated int `json:"links_created"`
}
