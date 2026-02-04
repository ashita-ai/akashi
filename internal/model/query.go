package model

import (
	"time"
)

// QueryFilters defines the filter parameters for structured decision queries.
type QueryFilters struct {
	AgentIDs      []string   `json:"agent_id,omitempty"`
	DecisionType  *string    `json:"decision_type,omitempty"`
	ConfidenceMin *float32   `json:"confidence_min,omitempty"`
	Outcome       *string    `json:"outcome,omitempty"`
	TimeRange     *TimeRange `json:"time_range,omitempty"`
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
}

// TemporalQueryRequest is the request body for POST /v1/query/temporal.
type TemporalQueryRequest struct {
	AsOf    time.Time    `json:"as_of"`
	Filters QueryFilters `json:"filters"`
}

// SearchRequest is the request body for POST /v1/search.
type SearchRequest struct {
	Query      string       `json:"query"`
	SearchType string       `json:"search_type,omitempty"`
	Filters    QueryFilters `json:"filters,omitempty"`
	Limit      int          `json:"limit,omitempty"`
}

// SearchResult wraps a decision with its similarity score.
type SearchResult struct {
	Decision        Decision `json:"decision"`
	SimilarityScore float32  `json:"similarity_score"`
}

// PagedResult wraps paginated query results.
type PagedResult[T any] struct {
	Items  []T `json:"items"`
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}
