package akashi

import (
	"time"

	"github.com/google/uuid"
)

// Decision mirrors the server's model.Decision for API consumers.
// It omits the Embedding field (internal to the server) and uses
// standard Go types instead of pgvector.
type Decision struct {
	ID                uuid.UUID      `json:"id"`
	RunID             uuid.UUID      `json:"run_id"`
	AgentID           string         `json:"agent_id"`
	OrgID             uuid.UUID      `json:"org_id"`
	DecisionType      string         `json:"decision_type"`
	Outcome           string         `json:"outcome"`
	Confidence        float32        `json:"confidence"`
	Reasoning         *string        `json:"reasoning,omitempty"`
	Metadata          map[string]any `json:"metadata"`
	CompletenessScore float32        `json:"completeness_score"`
	OutcomeScore      *float32       `json:"outcome_score,omitempty"`
	PrecedentRef      *uuid.UUID     `json:"precedent_ref,omitempty"`
	PrecedentReason   *string        `json:"precedent_reason,omitempty"`
	SupersedesID      *uuid.UUID     `json:"supersedes_id,omitempty"`
	ContentHash       string         `json:"content_hash,omitempty"`

	// Composite agent identity: session and runtime context from the calling agent.
	SessionID    *uuid.UUID     `json:"session_id,omitempty"`
	AgentContext map[string]any `json:"agent_context,omitempty"`

	// First-class attribution columns (indexed fast-path).
	Tool    *string `json:"tool,omitempty"`
	Model   *string `json:"model,omitempty"`
	Project *string `json:"project,omitempty"`

	// API key attribution.
	APIKeyID *uuid.UUID `json:"api_key_id,omitempty"`

	// Bi-temporal columns.
	ValidFrom       time.Time  `json:"valid_from"`
	ValidTo         *time.Time `json:"valid_to,omitempty"`
	TransactionTime time.Time  `json:"transaction_time"`

	CreatedAt time.Time `json:"created_at"`

	// Joined data (populated by queries that request includes).
	Alternatives []Alternative `json:"alternatives,omitempty"`
	Evidence     []Evidence    `json:"evidence,omitempty"`

	// Consensus scoring: computed at query time from embedding similarity cluster.
	AgreementCount int `json:"agreement_count"`
	ConflictCount  int `json:"conflict_count"`

	// Outcome signals: temporal, graph, and fate signals computed at query time.
	SupersessionVelocityHours *float64     `json:"supersession_velocity_hours"`
	PrecedentCitationCount    int          `json:"precedent_citation_count"`
	ConflictFate              ConflictFate `json:"conflict_fate"`

	// Assessment summary: populated on single-decision GET; nil in list responses.
	AssessmentSummary *AssessmentSummary `json:"assessment_summary,omitempty"`
}

// Alternative represents an option considered for a decision.
type Alternative struct {
	ID              uuid.UUID      `json:"id"`
	DecisionID      uuid.UUID      `json:"decision_id"`
	Label           string         `json:"label"`
	RejectionReason *string        `json:"rejection_reason,omitempty"`
	Metadata        map[string]any `json:"metadata"`
	CreatedAt       time.Time      `json:"created_at"`
}

// ConflictFate tracks how a decision fared in resolved conflict pairs.
type ConflictFate struct {
	Won              int `json:"won"`
	Lost             int `json:"lost"`
	ResolvedNoWinner int `json:"resolved_no_winner"`
}

// AssessmentSummary is a precomputed count of assessments by outcome.
type AssessmentSummary struct {
	Total            int `json:"total"`
	Correct          int `json:"correct"`
	Incorrect        int `json:"incorrect"`
	PartiallyCorrect int `json:"partially_correct"`
}

// Evidence represents supporting information for a decision.
type Evidence struct {
	ID             uuid.UUID          `json:"id"`
	DecisionID     uuid.UUID          `json:"decision_id"`
	OrgID          uuid.UUID          `json:"org_id"`
	SourceType     string             `json:"source_type"`
	SourceURI      *string            `json:"source_uri,omitempty"`
	Content        string             `json:"content"`
	RelevanceScore *float32           `json:"relevance_score,omitempty"`
	Metrics        map[string]float64 `json:"metrics,omitempty"`
	Metadata       map[string]any     `json:"metadata"`
	CreatedAt      time.Time          `json:"created_at"`
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
	Explanation       *string      `json:"explanation,omitempty"`

	// Conflict lifecycle: category, severity, and resolution state.
	Category       *string    `json:"category,omitempty"`
	Severity       *string    `json:"severity,omitempty"`
	Status         string     `json:"status"`
	ResolvedBy     *string    `json:"resolved_by,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	ResolutionNote *string    `json:"resolution_note,omitempty"`

	// Precision fields.
	Relationship         *string    `json:"relationship,omitempty"`
	ConfidenceWeight     *float64   `json:"confidence_weight,omitempty"`
	TemporalDecay        *float64   `json:"temporal_decay,omitempty"`
	ResolutionDecisionID *uuid.UUID `json:"resolution_decision_id,omitempty"`

	// Winner: which of the two decisions prevailed in resolution.
	WinningDecisionID *uuid.UUID `json:"winning_decision_id,omitempty"`

	// Group: canonical conflict group this pair belongs to.
	GroupID *uuid.UUID `json:"group_id,omitempty"`

	// Claim fragments from claim-level scoring.
	ClaimTextA *string `json:"claim_text_a,omitempty"`
	ClaimTextB *string `json:"claim_text_b,omitempty"`

	// Links to prior resolved conflict this one contradicts.
	ReopensResolutionID *uuid.UUID `json:"reopens_resolution_id,omitempty"`

	// Denormalized project names for project-scoped queries.
	ProjectA *string `json:"project_a,omitempty"`
	ProjectB *string `json:"project_b,omitempty"`
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
	SupersedesID *uuid.UUID         `json:"supersedes_id,omitempty"`
	Alternatives []TraceAlternative `json:"alternatives,omitempty"`
	Evidence     []TraceEvidence    `json:"evidence,omitempty"`
	Metadata     map[string]any     `json:"metadata,omitempty"`
	Context      map[string]any     `json:"context,omitempty"`

	// IdempotencyKey is an optional client-provided key for safe retries.
	// If empty, a random UUID is generated automatically. Sent as the
	// X-Idempotency-Key header.
	IdempotencyKey string `json:"-"`
}

// TraceAlternative is an alternative in a trace request.
type TraceAlternative struct {
	Label           string  `json:"label"`
	RejectionReason *string `json:"rejection_reason,omitempty"`
}

// TraceEvidence is evidence in a trace request.
type TraceEvidence struct {
	SourceType     string             `json:"source_type"`
	SourceURI      *string            `json:"source_uri,omitempty"`
	Content        string             `json:"content"`
	RelevanceScore *float32           `json:"relevance_score,omitempty"`
	Metrics        map[string]float64 `json:"metrics,omitempty"`
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
	HasPrecedent         bool               `json:"has_precedent"`
	Decisions            []Decision         `json:"decisions"`
	Conflicts            []DecisionConflict `json:"conflicts,omitempty"`
	ConflictsUnavailable bool               `json:"conflicts_unavailable,omitempty"`
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
	EventDecisionSuperseded     EventType = "DecisionSuperseded"
	EventDecisionRetracted      EventType = "DecisionRetracted"
	EventDecisionErased         EventType = "DecisionErased"
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
	BufferDepth   int    `json:"buffer_depth"`
	BufferStatus  string `json:"buffer_status"`
	SSEBroker     string `json:"sse_broker,omitempty"`
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
	DecisionID   uuid.UUID `json:"decision_id"`
	Valid        bool      `json:"valid"`
	StoredHash   string    `json:"stored_hash"`
	ComputedHash string    `json:"computed_hash"`
}

// RevisionsResponse is the output of Client.GetDecisionRevisions.
type RevisionsResponse struct {
	DecisionID uuid.UUID  `json:"decision_id"`
	Revisions  []Decision `json:"revisions"`
	Count      int        `json:"count"`
}

// ConflictOptions are optional filters for the ListConflicts method.
type ConflictOptions struct {
	DecisionType string
	AgentID      string
	ConflictKind string // "cross_agent" or "self_contradiction"
	Limit        int
	Offset       int
}

// --- Assessment types ---

// AssessOutcome is the verdict an agent records for a prior decision.
type AssessOutcome string

const (
	AssessCorrect          AssessOutcome = "correct"
	AssessIncorrect        AssessOutcome = "incorrect"
	AssessPartiallyCorrect AssessOutcome = "partially_correct"
)

// AssessRequest is the input for Client.Assess.
type AssessRequest struct {
	// Outcome is required. Must be "correct", "incorrect", or "partially_correct".
	Outcome AssessOutcome `json:"outcome"`
	// Notes is optional free-text explanation.
	Notes string `json:"notes,omitempty"`
}

// AssessResponse is the output of Client.Assess and an element of Client.ListAssessments.
type AssessResponse struct {
	ID              uuid.UUID     `json:"id"`
	DecisionID      uuid.UUID     `json:"decision_id"`
	OrgID           uuid.UUID     `json:"org_id"`
	AssessorAgentID string        `json:"assessor_agent_id"`
	Outcome         AssessOutcome `json:"outcome"`
	Notes           string        `json:"notes,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
}

// ---------------------------------------------------------------------------
// Phase 2: Decision & conflict management types
// ---------------------------------------------------------------------------

// ConflictDetail is the enriched output of Client.GetConflict.
type ConflictDetail struct {
	DecisionConflict DecisionConflict        `json:"decision_conflict"`
	Recommendation   *ConflictRecommendation `json:"recommendation,omitempty"`
}

// ConflictRecommendation is the server's suggested resolution for a conflict.
type ConflictRecommendation struct {
	SuggestedWinner uuid.UUID `json:"suggested_winner"`
	Reasons         []string  `json:"reasons"`
	Confidence      float64   `json:"confidence"`
}

// LineageResponse is the output of Client.GetDecisionLineage.
type LineageResponse struct {
	DecisionID   uuid.UUID      `json:"decision_id"`
	PrecedentRef *uuid.UUID     `json:"precedent_ref,omitempty"`
	Precedent    *LineageEntry  `json:"precedent,omitempty"`
	Cites        []LineageEntry `json:"cites"`
}

// LineageEntry is a minimal decision summary in a lineage chain.
type LineageEntry struct {
	ID              uuid.UUID `json:"id"`
	DecisionType    string    `json:"decision_type"`
	Outcome         string    `json:"outcome"`
	Confidence      float32   `json:"confidence"`
	AgentID         string    `json:"agent_id"`
	ValidFrom       time.Time `json:"valid_from"`
	PrecedentReason *string   `json:"precedent_reason,omitempty"`
}

// TimelineResponse is the output of Client.GetDecisionTimeline.
type TimelineResponse struct {
	Granularity string           `json:"granularity"`
	Buckets     []TimelineBucket `json:"buckets"`
	Projects    []string         `json:"projects"`
}

// TimelineBucket is one time period in a decision timeline.
type TimelineBucket struct {
	Bucket        string                    `json:"bucket"`
	DecisionCount int                       `json:"decision_count"`
	AvgConfidence float64                   `json:"avg_confidence"`
	DecisionTypes map[string]int            `json:"decision_types"`
	Agents        map[string]int            `json:"agents"`
	ConflictCount int                       `json:"conflict_count"`
	TopDecisions  []TimelineDecisionSummary `json:"top_decisions,omitempty"`
}

// TimelineDecisionSummary is a compact decision in a timeline bucket.
type TimelineDecisionSummary struct {
	ID           uuid.UUID `json:"id"`
	AgentID      string    `json:"agent_id"`
	DecisionType string    `json:"decision_type"`
	Outcome      string    `json:"outcome"`
	Confidence   float32   `json:"confidence"`
	Project      string    `json:"project,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// TimelineOptions are optional filters for Client.GetDecisionTimeline.
type TimelineOptions struct {
	Granularity string // "day" or "week"
	From        *time.Time
	To          *time.Time
	AgentID     string
	Project     string
}

// FacetsResponse is the output of Client.GetDecisionFacets.
type FacetsResponse struct {
	Types    []string `json:"types"`
	Projects []string `json:"projects"`
}

// RetractDecisionRequest is the optional input for Client.RetractDecision.
type RetractDecisionRequest struct {
	Reason string `json:"reason,omitempty"`
}

// EraseDecisionRequest is the optional input for Client.EraseDecision.
type EraseDecisionRequest struct {
	Reason string `json:"reason,omitempty"`
}

// EraseDecisionResponse is the output of Client.EraseDecision.
type EraseDecisionResponse struct {
	DecisionID         uuid.UUID `json:"decision_id"`
	ErasedAt           time.Time `json:"erased_at"`
	OriginalHash       string    `json:"original_hash"`
	ErasedHash         string    `json:"erased_hash"`
	AlternativesErased int       `json:"alternatives_erased"`
	EvidenceErased     int       `json:"evidence_erased"`
	ClaimsErased       int       `json:"claims_erased"`
}

// AdjudicateConflictRequest is the input for Client.AdjudicateConflict.
type AdjudicateConflictRequest struct {
	Outcome           string     `json:"outcome"`
	Reasoning         string     `json:"reasoning,omitempty"`
	DecisionType      string     `json:"decision_type,omitempty"`
	WinningDecisionID *uuid.UUID `json:"winning_decision_id,omitempty"`
}

// ConflictStatusUpdate is the input for Client.PatchConflict.
type ConflictStatusUpdate struct {
	Status             string     `json:"status"` // "resolved" or "false_positive"
	ResolutionNote     string     `json:"resolution_note,omitempty"`
	WinningDecisionID  *uuid.UUID `json:"winning_decision_id,omitempty"`
	FalsePositiveLabel string     `json:"false_positive_label,omitempty"`
}

// ResolveConflictGroupRequest is the input for Client.ResolveConflictGroup.
type ResolveConflictGroupRequest struct {
	Status             string `json:"status"` // "resolved" or "false_positive"
	ResolutionNote     string `json:"resolution_note,omitempty"`
	WinningAgent       string `json:"winning_agent,omitempty"`
	FalsePositiveLabel string `json:"false_positive_label,omitempty"`
}

// ResolveConflictGroupResponse is the output of Client.ResolveConflictGroup.
type ResolveConflictGroupResponse struct {
	GroupID  uuid.UUID `json:"group_id"`
	Status   string    `json:"status"`
	Resolved int       `json:"resolved"`
}

// ConflictGroup represents a group of related conflicts.
type ConflictGroup struct {
	ID              uuid.UUID          `json:"id"`
	OrgID           uuid.UUID          `json:"org_id"`
	AgentA          string             `json:"agent_a"`
	AgentB          string             `json:"agent_b"`
	ConflictKind    ConflictKind       `json:"conflict_kind"`
	DecisionType    string             `json:"decision_type"`
	GroupTopic      string             `json:"group_topic,omitempty"`
	FirstDetectedAt time.Time          `json:"first_detected_at"`
	LastDetectedAt  time.Time          `json:"last_detected_at"`
	ConflictCount   int                `json:"conflict_count"`
	OpenCount       int                `json:"open_count"`
	TimesReopened   int                `json:"times_reopened"`
	Representative  *DecisionConflict  `json:"representative,omitempty"`
	OpenConflicts   []DecisionConflict `json:"open_conflicts,omitempty"`
}

// ConflictGroupOptions are optional filters for Client.ListConflictGroups.
type ConflictGroupOptions struct {
	DecisionType string
	AgentID      string
	ConflictKind string
	Status       string
	Limit        int
	Offset       int
}

// ConflictAnalyticsResponse is the output of Client.GetConflictAnalytics.
type ConflictAnalyticsResponse struct {
	Period         string                   `json:"period"`
	From           time.Time                `json:"from"`
	To             time.Time                `json:"to"`
	Summary        ConflictAnalyticsSummary `json:"summary"`
	ByAgentPair    []ConflictAgentPairStats `json:"by_agent_pair"`
	ByDecisionType []ConflictTypeStats      `json:"by_decision_type"`
	BySeverity     []ConflictSeverityStats  `json:"by_severity"`
	DailyTrend     []ConflictDailyTrend     `json:"daily_trend"`
}

// ConflictAnalyticsSummary holds aggregate conflict metrics.
type ConflictAnalyticsSummary struct {
	TotalConflicts      int     `json:"total_conflicts"`
	Open                int     `json:"open"`
	Resolved            int     `json:"resolved"`
	FalsePositives      int     `json:"false_positives"`
	AvgDaysToResolution float64 `json:"avg_days_to_resolution"`
}

// ConflictAgentPairStats holds conflict counts for an agent pair.
type ConflictAgentPairStats struct {
	AgentA         string `json:"agent_a"`
	AgentB         string `json:"agent_b"`
	Count          int    `json:"count"`
	Open           int    `json:"open"`
	Resolved       int    `json:"resolved"`
	FalsePositives int    `json:"false_positives"`
}

// ConflictTypeStats holds conflict counts for a decision type.
type ConflictTypeStats struct {
	DecisionType string `json:"decision_type"`
	Count        int    `json:"count"`
	Open         int    `json:"open"`
}

// ConflictSeverityStats holds conflict counts by severity level.
type ConflictSeverityStats struct {
	Severity string `json:"severity"`
	Count    int    `json:"count"`
	Open     int    `json:"open"`
}

// ConflictDailyTrend holds daily conflict detection/resolution counts.
type ConflictDailyTrend struct {
	Date     string `json:"date"`
	Detected int    `json:"detected"`
	Resolved int    `json:"resolved"`
}

// ConflictAnalyticsOptions are optional filters for Client.GetConflictAnalytics.
type ConflictAnalyticsOptions struct {
	Period       string // "7d", "30d", "90d"
	From         *time.Time
	To           *time.Time
	AgentID      string
	DecisionType string
	ConflictKind string
}

// ---------------------------------------------------------------------------
// Phase 3: Admin & configuration types
// ---------------------------------------------------------------------------

// APIKey represents an API key (without the raw secret).
type APIKey struct {
	ID        uuid.UUID  `json:"id"`
	Prefix    string     `json:"prefix"`
	AgentID   string     `json:"agent_id"`
	OrgID     uuid.UUID  `json:"org_id"`
	Label     string     `json:"label"`
	CreatedBy string     `json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// APIKeyWithRawKey is the response when creating or rotating a key.
// The raw key is returned exactly once.
type APIKeyWithRawKey struct {
	APIKey APIKey `json:"api_key"`
	RawKey string `json:"raw_key"`
}

// CreateKeyRequest is the input for Client.CreateKey.
type CreateKeyRequest struct {
	AgentID   string  `json:"agent_id"`
	Label     string  `json:"label,omitempty"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// RotateKeyResponse is the output of Client.RotateKey.
type RotateKeyResponse struct {
	NewKey       APIKeyWithRawKey `json:"new_key"`
	RevokedKeyID uuid.UUID        `json:"revoked_key_id"`
}

// OrgSettings represents organization-level configuration.
type OrgSettings struct {
	ConflictResolution ConflictResolutionSettings `json:"conflict_resolution"`
	UpdatedAt          time.Time                  `json:"updated_at"`
}

// ConflictResolutionSettings configures automatic conflict handling.
type ConflictResolutionSettings struct {
	AutoResolveThreshold       float64 `json:"auto_resolve_threshold"`
	EnableCascadeResolution    bool    `json:"enable_cascade_resolution"`
	CascadeSimilarityThreshold float64 `json:"cascade_similarity_threshold"`
}

// SetOrgSettingsRequest is the input for Client.SetOrgSettings.
type SetOrgSettingsRequest struct {
	ConflictResolution ConflictResolutionSettings `json:"conflict_resolution"`
}

// RetentionPolicy is the output of Client.GetRetention.
type RetentionPolicy struct {
	RetentionDays         int             `json:"retention_days"`
	RetentionExcludeTypes []string        `json:"retention_exclude_types"`
	LastRun               *time.Time      `json:"last_run,omitempty"`
	LastRunDeleted        int             `json:"last_run_deleted"`
	NextRun               *time.Time      `json:"next_run,omitempty"`
	Holds                 []RetentionHold `json:"holds"`
}

// RetentionHold prevents purging of matching decisions.
type RetentionHold struct {
	ID            uuid.UUID  `json:"id"`
	OrgID         uuid.UUID  `json:"org_id"`
	Reason        string     `json:"reason"`
	HoldFrom      time.Time  `json:"hold_from"`
	HoldTo        time.Time  `json:"hold_to"`
	DecisionTypes []string   `json:"decision_types,omitempty"`
	AgentIDs      []string   `json:"agent_ids,omitempty"`
	CreatedBy     string     `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	ReleasedAt    *time.Time `json:"released_at,omitempty"`
}

// SetRetentionRequest is the input for Client.SetRetention.
type SetRetentionRequest struct {
	RetentionDays         int      `json:"retention_days"`
	RetentionExcludeTypes []string `json:"retention_exclude_types,omitempty"`
}

// PurgeRequest is the input for Client.PurgeDecisions.
type PurgeRequest struct {
	Before       time.Time `json:"before"`
	DecisionType string    `json:"decision_type,omitempty"`
	AgentID      string    `json:"agent_id,omitempty"`
	DryRun       bool      `json:"dry_run"`
}

// PurgeCounts holds the per-entity counts affected by a purge.
type PurgeCounts struct {
	Decisions    int `json:"decisions"`
	Alternatives int `json:"alternatives"`
	Evidence     int `json:"evidence"`
	Claims       int `json:"claims"`
	Events       int `json:"events"`
}

// PurgeResponse is the output of Client.PurgeDecisions.
type PurgeResponse struct {
	DryRun      bool        `json:"dry_run"`
	WouldDelete PurgeCounts `json:"would_delete"`
	Deleted     PurgeCounts `json:"deleted"`
}

// CreateHoldRequest is the input for Client.CreateHold.
type CreateHoldRequest struct {
	Reason        string    `json:"reason"`
	From          time.Time `json:"from"`
	To            time.Time `json:"to"`
	DecisionTypes []string  `json:"decision_types,omitempty"`
	AgentIDs      []string  `json:"agent_ids,omitempty"`
}

// ProjectLink represents a connection between two projects.
type ProjectLink struct {
	ID        uuid.UUID `json:"id"`
	OrgID     uuid.UUID `json:"org_id"`
	ProjectA  string    `json:"project_a"`
	ProjectB  string    `json:"project_b"`
	LinkType  string    `json:"link_type"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateProjectLinkRequest is the input for Client.CreateProjectLink.
type CreateProjectLinkRequest struct {
	ProjectA string `json:"project_a"`
	ProjectB string `json:"project_b"`
	LinkType string `json:"link_type,omitempty"`
}

// GrantAllProjectLinksRequest is the input for Client.GrantAllProjectLinks.
type GrantAllProjectLinksRequest struct {
	LinkType string `json:"link_type,omitempty"`
}

// GrantAllProjectLinksResponse is the output of Client.GrantAllProjectLinks.
type GrantAllProjectLinksResponse struct {
	LinksCreated int `json:"links_created"`
}

// SubscriptionEvent represents a single event received from the SSE stream.
// EventType is the SSE event name (e.g. "akashi_decisions" or "akashi_conflicts").
// Data contains the parsed JSON payload.
type SubscriptionEvent struct {
	EventType string         `json:"event_type"`
	Data      map[string]any `json:"data"`
}

// ExportOptions are optional filters for Client.ExportDecisions.
type ExportOptions struct {
	AgentID      string
	DecisionType string
	From         *time.Time
	To           *time.Time
}

// IntegrityViolation represents a detected hash mismatch.
type IntegrityViolation struct {
	ID           uuid.UUID `json:"id"`
	DecisionID   uuid.UUID `json:"decision_id"`
	OrgID        uuid.UUID `json:"org_id"`
	ExpectedHash string    `json:"expected_hash"`
	ActualHash   string    `json:"actual_hash"`
	DetectedAt   time.Time `json:"detected_at"`
}

// IntegrityViolationsResponse is the output of Client.ListIntegrityViolations.
type IntegrityViolationsResponse struct {
	Violations []IntegrityViolation `json:"violations"`
	Count      int                  `json:"count"`
}

// TraceHealthResponse is the output of Client.GetTraceHealth.
type TraceHealthResponse struct {
	TotalDecisions   int     `json:"total_decisions"`
	TotalAssessments int     `json:"total_assessments"`
	TotalConflicts   int     `json:"total_conflicts"`
	AvgCompleteness  float64 `json:"avg_completeness"`
	AvgConfidence    float64 `json:"avg_confidence"`
	AssessmentRate   float64 `json:"assessment_rate"`
	ConflictRate     float64 `json:"conflict_rate"`
	ComplianceScore  float64 `json:"compliance_score"`
}

// TraceHealthOptions are optional time-range filters for Client.GetTraceHealth.
type TraceHealthOptions struct {
	From *time.Time
	To   *time.Time
}

// UsageResponse is the output of Client.GetUsage.
type UsageResponse struct {
	OrgID          uuid.UUID      `json:"org_id"`
	Period         string         `json:"period"`
	TotalDecisions int            `json:"total_decisions"`
	ByKey          []UsageByKey   `json:"by_key"`
	ByAgent        []UsageByAgent `json:"by_agent"`
}

// UsageByKey is a per-key breakdown in a usage response.
type UsageByKey struct {
	KeyID     uuid.UUID `json:"key_id"`
	Prefix    string    `json:"prefix"`
	Label     string    `json:"label"`
	AgentID   string    `json:"agent_id"`
	Decisions int       `json:"decisions"`
}

// UsageByAgent is a per-agent breakdown in a usage response.
type UsageByAgent struct {
	AgentID   string `json:"agent_id"`
	Decisions int    `json:"decisions"`
}

// ScopedTokenRequest is the input for Client.ScopedToken.
type ScopedTokenRequest struct {
	AsAgentID string `json:"as_agent_id"`
	ExpiresIn int    `json:"expires_in,omitempty"` // seconds
}

// ScopedTokenResponse is the output of Client.ScopedToken.
type ScopedTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	AsAgentID string    `json:"as_agent_id"`
	ScopedBy  string    `json:"scoped_by"`
}

// SignupRequest is the input for Client.Signup.
type SignupRequest struct {
	OrgName string `json:"org_name"`
	AgentID string `json:"agent_id"`
	Email   string `json:"email"`
}

// SignupResponse is the output of Client.Signup.
type SignupResponse struct {
	OrgID     uuid.UUID      `json:"org_id"`
	OrgSlug   string         `json:"org_slug"`
	AgentID   string         `json:"agent_id"`
	APIKey    string         `json:"api_key"`
	MCPConfig *MCPConfigInfo `json:"mcp_config,omitempty"`
}

// MCPConfigInfo holds MCP connection details returned on signup.
type MCPConfigInfo struct {
	URL    string `json:"url"`
	Header string `json:"header"`
}

// ConfigResponse is the output of Client.GetConfig.
type ConfigResponse struct {
	SearchEnabled bool `json:"search_enabled"`
}

// ---------------------------------------------------------------------------
// Phase 4: Agent, grant, and session types
// ---------------------------------------------------------------------------

// UpdateAgentRequest is the input for Client.UpdateAgent.
type UpdateAgentRequest struct {
	Name     *string        `json:"name,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// AgentStatsResponse is the output of Client.GetAgentStats.
type AgentStatsResponse struct {
	AgentID string     `json:"agent_id"`
	Stats   AgentStats `json:"stats"`
}

// AgentStats holds aggregate metrics for an agent.
type AgentStats struct {
	DecisionCount  int        `json:"decision_count"`
	LastDecisionAt *time.Time `json:"last_decision_at,omitempty"`
	AvgConfidence  float64    `json:"avg_confidence"`
	ConflictRate   float64    `json:"conflict_rate"`
}

// ListGrantsOptions are optional filters for Client.ListGrants.
type ListGrantsOptions struct {
	Limit  int
	Offset int
}

// GrantsResponse is the output of Client.ListGrants.
type GrantsResponse struct {
	Grants  []Grant `json:"grants"`
	Total   int     `json:"total"`
	HasMore bool    `json:"has_more"`
	Limit   int     `json:"limit"`
	Offset  int     `json:"offset"`
}

// SessionViewResponse is the output of Client.GetSessionView.
type SessionViewResponse struct {
	SessionID     uuid.UUID      `json:"session_id"`
	Decisions     []Decision     `json:"decisions"`
	DecisionCount int            `json:"decision_count"`
	Summary       SessionSummary `json:"summary"`
}

// SessionSummary holds aggregate metrics for a session.
type SessionSummary struct {
	StartedAt     *time.Time     `json:"started_at,omitempty"`
	EndedAt       *time.Time     `json:"ended_at,omitempty"`
	DurationSecs  float64        `json:"duration_secs"`
	DecisionTypes map[string]int `json:"decision_types"`
	AvgConfidence float64        `json:"avg_confidence"`
}

// DecisionConflictsResponse is the output of Client.GetDecisionConflicts.
type DecisionConflictsResponse struct {
	Conflicts []DecisionConflict `json:"conflicts"`
	Total     int                `json:"total"`
	HasMore   bool               `json:"has_more"`
	Limit     int                `json:"limit"`
	Offset    int                `json:"offset"`
}

// DecisionConflictsOptions are optional filters for Client.GetDecisionConflicts.
type DecisionConflictsOptions struct {
	Status string
	Limit  int
	Offset int
}

// ConflictGroupsResponse is the output of Client.ListConflictGroups.
type ConflictGroupsResponse struct {
	Groups  []ConflictGroup `json:"groups"`
	Total   int             `json:"total"`
	HasMore bool            `json:"has_more"`
	Limit   int             `json:"limit"`
	Offset  int             `json:"offset"`
}

// ---------------------------------------------------------------------------
// Admin: conflict validation, evaluation, and labels
// ---------------------------------------------------------------------------

// ValidatePairRequest is the input for Client.ValidatePair.
type ValidatePairRequest struct {
	OutcomeA        string  `json:"outcome_a"`
	OutcomeB        string  `json:"outcome_b"`
	TypeA           string  `json:"type_a,omitempty"`
	TypeB           string  `json:"type_b,omitempty"`
	AgentA          string  `json:"agent_a,omitempty"`
	AgentB          string  `json:"agent_b,omitempty"`
	ReasoningA      string  `json:"reasoning_a,omitempty"`
	ReasoningB      string  `json:"reasoning_b,omitempty"`
	ProjectA        string  `json:"project_a,omitempty"`
	ProjectB        string  `json:"project_b,omitempty"`
	TopicSimilarity float64 `json:"topic_similarity,omitempty"`
}

// ValidatePairResponse is the output of Client.ValidatePair.
type ValidatePairResponse struct {
	Relationship string `json:"relationship"` // contradiction, supersession, complementary, refinement, unrelated
	Category     string `json:"category"`     // factual, assessment, strategic, temporal
	Severity     string `json:"severity"`     // critical, high, medium, low
	Explanation  string `json:"explanation"`
}

// ConflictEvalMetrics holds aggregate metrics from a conflict evaluation run.
type ConflictEvalMetrics struct {
	TotalPairs           int     `json:"total_pairs"`
	Errors               int     `json:"errors"`
	RelationshipAccuracy float64 `json:"relationship_accuracy"`
	ConflictPrecision    float64 `json:"conflict_precision"`
	ConflictRecall       float64 `json:"conflict_recall"`
	ConflictF1           float64 `json:"conflict_f1"`
	TruePositives        int     `json:"true_positives"`
	FalsePositives       int     `json:"false_positives"`
	TrueNegatives        int     `json:"true_negatives"`
	FalseNegatives       int     `json:"false_negatives"`
	RelationshipHits     int     `json:"relationship_hits"`
}

// ConflictEvalResult holds per-pair results from a conflict evaluation run.
type ConflictEvalResult struct {
	Label                string `json:"label"`
	ExpectedRelationship string `json:"expected_relationship"`
	ActualRelationship   string `json:"actual_relationship"`
	Correct              bool   `json:"correct"`
	ConflictExpected     bool   `json:"conflict_expected"`
	ConflictActual       bool   `json:"conflict_actual"`
	Explanation          string `json:"explanation"`
	Error                string `json:"error,omitempty"`
}

// ConflictEvalResponse is the output of Client.ConflictEval.
type ConflictEvalResponse struct {
	Metrics ConflictEvalMetrics  `json:"metrics"`
	Results []ConflictEvalResult `json:"results"`
}

// UpsertConflictLabelRequest is the input for Client.UpsertConflictLabel.
type UpsertConflictLabelRequest struct {
	Label string `json:"label"` // genuine, related_not_contradicting, unrelated_false_positive
	Notes string `json:"notes,omitempty"`
}

// ConflictLabel represents a human label applied to a scored conflict.
type ConflictLabel struct {
	ScoredConflictID uuid.UUID `json:"scored_conflict_id"`
	OrgID            uuid.UUID `json:"org_id"`
	Label            string    `json:"label"`
	LabeledBy        string    `json:"labeled_by"`
	LabeledAt        time.Time `json:"labeled_at"`
	Notes            string    `json:"notes,omitempty"`
}

// ConflictLabelCounts holds aggregate label counts.
type ConflictLabelCounts struct {
	Genuine                 int `json:"genuine"`
	RelatedNotContradicting int `json:"related_not_contradicting"`
	UnrelatedFalsePositive  int `json:"unrelated_false_positive"`
	Total                   int `json:"total"`
}

// ListConflictLabelsResponse is the output of Client.ListConflictLabels.
type ListConflictLabelsResponse struct {
	Labels []ConflictLabel     `json:"labels"`
	Counts ConflictLabelCounts `json:"counts"`
}

// ScorerEvalResponse is the output of Client.ScorerEval.
type ScorerEvalResponse struct {
	Precision      float64 `json:"precision"`
	TruePositives  int     `json:"true_positives"`
	FalsePositives int     `json:"false_positives"`
	TotalLabeled   int     `json:"total_labeled"`
	Message        string  `json:"message,omitempty"`
}
