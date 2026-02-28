package akashi

import (
	"time"

	"github.com/google/uuid"
)

// Decision mirrors the server's model.Decision for API consumers.
// It omits the Embedding field (internal to the server) and uses
// standard Go types instead of pgvector.
type Decision struct {
	ID           uuid.UUID      `json:"id"`
	RunID        uuid.UUID      `json:"run_id"`
	AgentID      string         `json:"agent_id"`
	OrgID        uuid.UUID      `json:"org_id"`
	DecisionType string         `json:"decision_type"`
	Outcome      string         `json:"outcome"`
	Confidence   float32        `json:"confidence"`
	Reasoning    *string        `json:"reasoning,omitempty"`
	Metadata     map[string]any `json:"metadata"`
	CompletenessScore float64 `json:"completeness_score"`
	PrecedentRef *uuid.UUID     `json:"precedent_ref,omitempty"`
	SupersedesID *uuid.UUID     `json:"supersedes_id,omitempty"`
	ContentHash  string         `json:"content_hash,omitempty"`
	Tags         []string       `json:"tags,omitempty"`

	// Composite agent identity: session and runtime context from the calling agent.
	SessionID    *uuid.UUID     `json:"session_id,omitempty"`
	AgentContext map[string]any `json:"agent_context,omitempty"`

	// Bi-temporal columns.
	ValidFrom       time.Time  `json:"valid_from"`
	ValidTo         *time.Time `json:"valid_to,omitempty"`
	TransactionTime time.Time  `json:"transaction_time"`

	CreatedAt time.Time `json:"created_at"`

	// Joined data (populated by queries that request includes).
	Alternatives []Alternative `json:"alternatives,omitempty"`
	Evidence     []Evidence    `json:"evidence,omitempty"`
}

// Alternative represents an option considered for a decision.
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

// Evidence represents supporting information for a decision.
type Evidence struct {
	ID             uuid.UUID      `json:"id"`
	DecisionID     uuid.UUID      `json:"decision_id"`
	SourceType     string         `json:"source_type"`
	SourceURI      *string        `json:"source_uri,omitempty"`
	Content        string         `json:"content"`
	RelevanceScore *float32       `json:"relevance_score,omitempty"`
	Metadata       map[string]any `json:"metadata"`
	CreatedAt      time.Time      `json:"created_at"`
}

// ConflictKind indicates whether a conflict is between agents or self-contradiction.
type ConflictKind string

const (
	ConflictKindCrossAgent       ConflictKind = "cross_agent"
	ConflictKindSelfContradiction ConflictKind = "self_contradiction"
)

// DecisionConflict represents a detected conflict between two decisions.
type DecisionConflict struct {
	ConflictKind      ConflictKind `json:"conflict_kind"`
	DecisionAID       uuid.UUID    `json:"decision_a_id"`
	DecisionBID       uuid.UUID    `json:"decision_b_id"`
	OrgID             uuid.UUID    `json:"org_id"`
	AgentA            string       `json:"agent_a"`
	AgentB            string       `json:"agent_b"`
	RunA              uuid.UUID    `json:"run_a"`
	RunB              uuid.UUID    `json:"run_b"`
	DecisionType      string       `json:"decision_type"`
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

// --- Request types ---

// CheckRequest is the input for Client.Check.
type CheckRequest struct {
	DecisionType string `json:"decision_type"`
	Query        string `json:"query,omitempty"`
	AgentID      string `json:"agent_id,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

// TraceRequest is the input for Client.Trace.
type TraceRequest struct {
	DecisionType string             `json:"decision_type"`
	Outcome      string             `json:"outcome"`
	Confidence   float32            `json:"confidence"`
	Reasoning    *string            `json:"reasoning,omitempty"`
	PrecedentRef *uuid.UUID         `json:"precedent_ref,omitempty"`
	Alternatives []TraceAlternative `json:"alternatives,omitempty"`
	Evidence     []TraceEvidence    `json:"evidence,omitempty"`
	Metadata     map[string]any     `json:"metadata,omitempty"`
	Context      map[string]any     `json:"context,omitempty"`
}

// TraceAlternative is an alternative in a trace request.
type TraceAlternative struct {
	Label           string   `json:"label"`
	Score           *float32 `json:"score,omitempty"`
	Selected        bool     `json:"selected"`
	RejectionReason *string  `json:"rejection_reason,omitempty"`
}

// TraceEvidence is evidence in a trace request.
type TraceEvidence struct {
	SourceType     string   `json:"source_type"`
	SourceURI      *string  `json:"source_uri,omitempty"`
	Content        string   `json:"content"`
	RelevanceScore *float32 `json:"relevance_score,omitempty"`
}

// QueryFilters are structured filters for decision queries.
type QueryFilters struct {
	AgentIDs      []string `json:"agent_id,omitempty"`
	DecisionType  *string  `json:"decision_type,omitempty"`
	ConfidenceMin *float32 `json:"confidence_min,omitempty"`
	Outcome       *string  `json:"outcome,omitempty"`

	// Filters for composite agent identity fields (session, tool, model, project).
	SessionID *string `json:"session_id,omitempty"`
	Tool      *string `json:"tool,omitempty"`
	Model     *string `json:"model,omitempty"`
	Project   *string `json:"project,omitempty"`
}

// QueryOptions control ordering and pagination for Client.Query.
type QueryOptions struct {
	Limit    int    `json:"limit,omitempty"`
	Offset   int    `json:"offset,omitempty"`
	OrderBy  string `json:"order_by,omitempty"`
	OrderDir string `json:"order_dir,omitempty"`
}

// --- Response types ---

// CheckResponse is the output of Client.Check.
type CheckResponse struct {
	HasPrecedent bool               `json:"has_precedent"`
	Decisions    []Decision         `json:"decisions"`
	Conflicts    []DecisionConflict `json:"conflicts,omitempty"`
}

// TraceResponse is the output of Client.Trace.
type TraceResponse struct {
	RunID      uuid.UUID `json:"run_id"`
	DecisionID uuid.UUID `json:"decision_id"`
	EventCount int       `json:"event_count"`
}

// QueryResponse is the output of Client.Query.
type QueryResponse struct {
	Decisions []Decision `json:"decisions"`
	Total     int        `json:"total"`
	HasMore   bool       `json:"has_more"`
	Limit     int        `json:"limit"`
	Offset    int        `json:"offset"`
}

// SearchResult wraps a decision with its similarity score.
type SearchResult struct {
	Decision        Decision `json:"decision"`
	SimilarityScore float32  `json:"similarity_score"`
}

// SearchResponse is the output of Client.Search.
type SearchResponse struct {
	Results []SearchResult `json:"results"`
	Total   int            `json:"total"`
}

// --- Run types ---

// RunStatus represents the lifecycle state of an agent run.
type RunStatus string

const (
	RunStatusRunning   RunStatus = "running"
	RunStatusCompleted RunStatus = "completed"
	RunStatusFailed    RunStatus = "failed"
)

// AgentRun is the top-level execution context for an agent.
type AgentRun struct {
	ID          uuid.UUID      `json:"id"`
	AgentID     string         `json:"agent_id"`
	OrgID       uuid.UUID      `json:"org_id"`
	TraceID     *string        `json:"trace_id,omitempty"`
	ParentRunID *uuid.UUID     `json:"parent_run_id,omitempty"`
	Status      RunStatus      `json:"status"`
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	Metadata    map[string]any `json:"metadata"`
	CreatedAt   time.Time      `json:"created_at"`
}

// EventType represents the category of an agent event.
type EventType string

const (
	EventAgentRunStarted        EventType = "AgentRunStarted"
	EventAgentRunCompleted      EventType = "AgentRunCompleted"
	EventAgentRunFailed         EventType = "AgentRunFailed"
	EventDecisionStarted        EventType = "DecisionStarted"
	EventAlternativeConsidered  EventType = "AlternativeConsidered"
	EventEvidenceGathered       EventType = "EvidenceGathered"
	EventReasoningStepCompleted EventType = "ReasoningStepCompleted"
	EventDecisionMade           EventType = "DecisionMade"
	EventDecisionRevised        EventType = "DecisionRevised"
	EventToolCallStarted        EventType = "ToolCallStarted"
	EventToolCallCompleted      EventType = "ToolCallCompleted"
	EventAgentHandoff           EventType = "AgentHandoff"
	EventConsensusRequested     EventType = "ConsensusRequested"
	EventConflictDetected       EventType = "ConflictDetected"
)

// AgentEvent is an append-only event in the event log.
type AgentEvent struct {
	ID          uuid.UUID      `json:"id"`
	RunID       uuid.UUID      `json:"run_id"`
	OrgID       uuid.UUID      `json:"org_id"`
	EventType   EventType      `json:"event_type"`
	SequenceNum int64          `json:"sequence_num"`
	OccurredAt  time.Time      `json:"occurred_at"`
	AgentID     string         `json:"agent_id"`
	TraceID     string         `json:"trace_id,omitempty"`
	SpanID      string         `json:"span_id,omitempty"`
	Payload     map[string]any `json:"payload"`
	CreatedAt   time.Time      `json:"created_at"`
}

// --- Agent types ---

// AgentRole represents the RBAC role assigned to an agent.
type AgentRole string

const (
	RolePlatformAdmin AgentRole = "platform_admin"
	RoleOrgOwner      AgentRole = "org_owner"
	RoleAdmin         AgentRole = "admin"
	RoleAgent         AgentRole = "agent"
	RoleReader        AgentRole = "reader"
)

// Agent represents an agent identity with role assignment.
type Agent struct {
	ID        uuid.UUID      `json:"id"`
	AgentID   string         `json:"agent_id"`
	OrgID     uuid.UUID      `json:"org_id"`
	Name      string         `json:"name"`
	Role      AgentRole      `json:"role"`
	Tags      []string       `json:"tags"`
	Metadata  map[string]any `json:"metadata"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// --- Grant types ---

// Grant represents a fine-grained access grant between agents.
type Grant struct {
	ID           uuid.UUID  `json:"id"`
	OrgID        uuid.UUID  `json:"org_id"`
	GrantorID    uuid.UUID  `json:"grantor_id"`
	GranteeID    uuid.UUID  `json:"grantee_id"`
	ResourceType string     `json:"resource_type"`
	ResourceID   *string    `json:"resource_id,omitempty"`
	Permission   string     `json:"permission"`
	GrantedAt    time.Time  `json:"granted_at"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

// --- Health and usage ---

// HealthResponse is the output of Client.Health.
type HealthResponse struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	Postgres      string `json:"postgres"`
	Qdrant        string `json:"qdrant,omitempty"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// --- Request types for new endpoints ---

// CreateRunRequest is the input for Client.CreateRun.
type CreateRunRequest struct {
	AgentID     string         `json:"agent_id"`
	TraceID     *string        `json:"trace_id,omitempty"`
	ParentRunID *uuid.UUID     `json:"parent_run_id,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// EventInput is a single event to append to a run.
type EventInput struct {
	EventType  EventType      `json:"event_type"`
	OccurredAt *time.Time     `json:"occurred_at,omitempty"`
	Payload    map[string]any `json:"payload"`
}

// CompleteRunRequest is the input for Client.CompleteRun.
type CompleteRunRequest struct {
	Status   string         `json:"status"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// TemporalQueryRequest is the input for Client.TemporalQuery.
type TemporalQueryRequest struct {
	AsOf    time.Time    `json:"as_of"`
	Filters QueryFilters `json:"filters"`
}

// CreateAgentRequest is the input for Client.CreateAgent.
type CreateAgentRequest struct {
	AgentID  string         `json:"agent_id"`
	Name     string         `json:"name"`
	Role     AgentRole      `json:"role"`
	APIKey   string         `json:"api_key"`
	Tags     []string       `json:"tags,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// CreateGrantRequest is the input for Client.CreateGrant.
type CreateGrantRequest struct {
	GranteeAgentID string  `json:"grantee_agent_id"`
	ResourceType   string  `json:"resource_type"`
	ResourceID     *string `json:"resource_id,omitempty"`
	Permission     string  `json:"permission"`
	ExpiresAt      *string `json:"expires_at,omitempty"`
}

// --- Response types for new endpoints ---

// AppendEventsResponse is the output of Client.AppendEvents.
type AppendEventsResponse struct {
	Accepted int         `json:"accepted"`
	EventIDs []uuid.UUID `json:"event_ids"`
}

// GetRunResponse is the output of Client.GetRun.
type GetRunResponse struct {
	Run       AgentRun     `json:"run"`
	Events    []AgentEvent `json:"events"`
	Decisions []Decision   `json:"decisions"`
}

// TemporalQueryResponse is the output of Client.TemporalQuery.
type TemporalQueryResponse struct {
	AsOf      time.Time  `json:"as_of"`
	Decisions []Decision `json:"decisions"`
}

// AgentHistoryResponse is the output of Client.AgentHistory.
type AgentHistoryResponse struct {
	AgentID   string     `json:"agent_id"`
	Decisions []Decision `json:"decisions"`
	Total     int        `json:"total"`
	HasMore   bool       `json:"has_more"`
	Limit     int        `json:"limit"`
	Offset    int        `json:"offset"`
}

// DeleteAgentResponse is the output of Client.DeleteAgent.
type DeleteAgentResponse struct {
	AgentID string         `json:"agent_id"`
	Deleted map[string]any `json:"deleted"`
}

// ConflictsResponse is the output of Client.ListConflicts.
type ConflictsResponse struct {
	Conflicts []DecisionConflict `json:"conflicts"`
	Total     int                `json:"total"`
	HasMore   bool               `json:"has_more"`
	Limit     int                `json:"limit"`
	Offset    int                `json:"offset"`
}

// VerifyResponse is the output of Client.VerifyDecision.
type VerifyResponse struct {
	DecisionID  uuid.UUID `json:"decision_id"`
	Valid       bool      `json:"valid"`
	StoredHash  string    `json:"stored_hash"`
	ComputedHash string   `json:"computed_hash"`
}

// RevisionsResponse is the output of Client.GetDecisionRevisions.
type RevisionsResponse struct {
	DecisionID uuid.UUID  `json:"decision_id"`
	Revisions  []Decision `json:"revisions"`
	Count      int        `json:"count"`
}

// ConflictOptions are optional filters for the ListConflicts method.
type ConflictOptions struct {
	DecisionType  string
	AgentID       string
	ConflictKind  string // "cross_agent" or "self_contradiction"
	Limit         int
	Offset        int
}
