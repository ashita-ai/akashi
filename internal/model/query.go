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
	Repo          *string    `json:"repo,omitempty"`
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
}

// CheckRequest is the request body for POST /v1/check.
// It supports a lightweight precedent lookup before making a decision.
type CheckRequest struct {
	DecisionType string `json:"decision_type"`
	Query        string `json:"query,omitempty"`
	AgentID      string `json:"agent_id,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

// CheckResponse is the response for POST /v1/check.
type CheckResponse struct {
	HasPrecedent bool               `json:"has_precedent"`
	Decisions    []Decision         `json:"decisions"`
	Conflicts    []DecisionConflict `json:"conflicts,omitempty"`
}
