package model

import (
	"time"

	"github.com/google/uuid"
)

// QueryFilters defines the filter parameters for structured decision queries.
type QueryFilters struct {
	AgentIDs      []string   `json:"agent_id,omitempty"`
	RunID         *uuid.UUID `json:"run_id,omitempty"`
	DecisionType  *string    `json:"decision_type,omitempty"`
	ConfidenceMin *float32   `json:"confidence_min,omitempty"`
	Outcome       *string    `json:"outcome,omitempty"`
	TimeRange     *TimeRange `json:"time_range,omitempty"`
	SessionID     *uuid.UUID `json:"session_id,omitempty"`
	Tool          *string    `json:"tool,omitempty"`
	Model         *string    `json:"model,omitempty"`
	Project       *string    `json:"project,omitempty"`
}

// TimeRange defines a time range for queries.
type TimeRange struct {
	From *time.Time `json:"from,omitempty"`
	To   *time.Time `json:"to,omitempty"`
}

// QueryRequest is the request body for POST /v1/query.
type QueryRequest struct {
	Filters  QueryFilters `json:"filters"`
	Include  []string     `json:"include,omitempty"`
	OrderBy  string       `json:"order_by,omitempty"`
	OrderDir string       `json:"order_dir,omitempty"`
	Limit    int          `json:"limit,omitempty"`
	Offset   int          `json:"offset,omitempty"`
	TraceID  *string      `json:"trace_id,omitempty"` // Filter by OTEL trace ID (matches agent_runs.trace_id).
}

// TemporalQueryRequest is the request body for POST /v1/query/temporal.
type TemporalQueryRequest struct {
	AsOf    time.Time    `json:"as_of"`
	Filters QueryFilters `json:"filters"`
	Limit   int          `json:"limit,omitempty"`
}

// SearchRequest is the request body for POST /v1/search.
type SearchRequest struct {
	Query    string       `json:"query"`
	Semantic bool         `json:"semantic"`
	Filters  QueryFilters `json:"filters,omitempty"`
	Limit    int          `json:"limit,omitempty"`
}

// SearchResult wraps a decision with its similarity score.
type SearchResult struct {
	Decision        Decision `json:"decision"`
	SimilarityScore float32  `json:"similarity_score"`
	QdrantRank      int      `json:"qdrant_rank,omitempty"` // 1-based position in Qdrant's ANN results; 0 for text-fallback results.
}

// TimelineBucket represents a single time period in the decision timeline summary.
type TimelineBucket struct {
	Bucket        string             `json:"bucket"` // ISO date string for the bucket start (e.g. "2026-03-10")
	DecisionCount int                `json:"decision_count"`
	AvgConfidence float64            `json:"avg_confidence"`
	DecisionTypes map[string]int     `json:"decision_types"`
	Agents        map[string]int     `json:"agents"`
	ConflictCount int                `json:"conflict_count"`
	TopDecisions  []TimelineDecision `json:"top_decisions"`
}

// TimelineDecision is a lightweight decision summary for the timeline view.
type TimelineDecision struct {
	ID           uuid.UUID `json:"id"`
	AgentID      string    `json:"agent_id"`
	DecisionType string    `json:"decision_type"`
	Outcome      string    `json:"outcome"`
	Confidence   float32   `json:"confidence"`
	Project      *string   `json:"project,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// TimelineResponse is the response for GET /v1/decisions/timeline.
type TimelineResponse struct {
	Granularity string           `json:"granularity"`
	Buckets     []TimelineBucket `json:"buckets"`
	Projects    []string         `json:"projects"`
}

// CheckRequest is the request body for POST /v1/check.
// It supports a lightweight precedent lookup before making a decision.
type CheckRequest struct {
	DecisionType string `json:"decision_type"`
	Query        string `json:"query,omitempty"`
	AgentID      string `json:"agent_id,omitempty"`
	Project      string `json:"project,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

// ConflictResolution summarises a resolved conflict for use in akashi_check responses.
// It tells an agent which approach prevailed on this decision type so they can avoid
// resurrecting the losing side of an already-resolved disagreement.
type ConflictResolution struct {
	ID                uuid.UUID `json:"id"`
	DecisionType      string    `json:"decision_type"`
	WinningDecisionID uuid.UUID `json:"winning_decision_id"`
	WinningAgent      string    `json:"winning_agent"`
	WinningOutcome    string    `json:"winning_outcome"`
	LosingAgent       string    `json:"losing_agent"`
	LosingOutcome     string    `json:"losing_outcome"`
	Explanation       *string   `json:"explanation,omitempty"`
	ResolutionNote    *string   `json:"resolution_note,omitempty"`
	ResolvedAt        time.Time `json:"resolved_at"`
}

// CheckResponse is the response for POST /v1/check.
type CheckResponse struct {
	HasPrecedent bool               `json:"has_precedent"`
	Decisions    []Decision         `json:"decisions"`
	Conflicts    []DecisionConflict `json:"conflicts,omitempty"`
	// PriorResolutions contains recently resolved conflicts for the requested
	// decision type. Each entry shows which approach was formally chosen
	// (winning_outcome / winning_agent) and which was rejected
	// (losing_outcome / losing_agent). Use winning_decision_id as precedent_ref
	// in akashi_trace to build on the validated approach.
	PriorResolutions []ConflictResolution `json:"prior_resolutions,omitempty"`
}
