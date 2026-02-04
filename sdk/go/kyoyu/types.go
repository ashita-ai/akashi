package kyoyu

import (
	"time"

	"github.com/google/uuid"
)

// Decision mirrors the server's model.Decision for API consumers.
// It omits the Embedding field (internal to the server) and uses
// standard Go types instead of pgvector.
type Decision struct {
	ID              uuid.UUID      `json:"id"`
	RunID           uuid.UUID      `json:"run_id"`
	AgentID         string         `json:"agent_id"`
	DecisionType    string         `json:"decision_type"`
	Outcome         string         `json:"outcome"`
	Confidence      float32        `json:"confidence"`
	Reasoning       *string        `json:"reasoning,omitempty"`
	Metadata        map[string]any `json:"metadata"`
	ValidFrom       time.Time      `json:"valid_from"`
	ValidTo         *time.Time     `json:"valid_to,omitempty"`
	TransactionTime time.Time      `json:"transaction_time"`
	CreatedAt       time.Time      `json:"created_at"`
	Alternatives    []Alternative  `json:"alternatives,omitempty"`
	Evidence        []Evidence     `json:"evidence,omitempty"`
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

// DecisionConflict represents a detected conflict between two decisions.
type DecisionConflict struct {
	DecisionAID  uuid.UUID `json:"decision_a_id"`
	DecisionBID  uuid.UUID `json:"decision_b_id"`
	AgentA       string    `json:"agent_a"`
	AgentB       string    `json:"agent_b"`
	RunA         uuid.UUID `json:"run_a"`
	RunB         uuid.UUID `json:"run_b"`
	DecisionType string    `json:"decision_type"`
	OutcomeA     string    `json:"outcome_a"`
	OutcomeB     string    `json:"outcome_b"`
	ConfidenceA  float32   `json:"confidence_a"`
	ConfidenceB  float32   `json:"confidence_b"`
	DecidedAtA   time.Time `json:"decided_at_a"`
	DecidedAtB   time.Time `json:"decided_at_b"`
	DetectedAt   time.Time `json:"detected_at"`
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
	Alternatives []TraceAlternative `json:"alternatives,omitempty"`
	Evidence     []TraceEvidence    `json:"evidence,omitempty"`
	Metadata     map[string]any     `json:"metadata,omitempty"`
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
