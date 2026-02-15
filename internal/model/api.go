package model

import (
	"time"

	"github.com/google/uuid"
)

// APIResponse is the standard response envelope for all HTTP API responses.
type APIResponse struct {
	Data any          `json:"data,omitempty"`
	Meta ResponseMeta `json:"meta"`
}

// APIError is the standard error response envelope.
type APIError struct {
	Error ErrorDetail  `json:"error"`
	Meta  ResponseMeta `json:"meta"`
}

// ResponseMeta contains request metadata included in every response.
type ResponseMeta struct {
	RequestID string    `json:"request_id"`
	Timestamp time.Time `json:"timestamp"`
}

// ErrorDetail describes an API error.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// ErrorCode constants for standard API error codes.
const (
	ErrCodeInvalidInput  = "INVALID_INPUT"
	ErrCodeUnauthorized  = "UNAUTHORIZED"
	ErrCodeForbidden     = "FORBIDDEN"
	ErrCodeNotFound      = "NOT_FOUND"
	ErrCodeConflict      = "CONFLICT"
	ErrCodeInternalError = "INTERNAL_ERROR"
	ErrCodeRateLimited   = "RATE_LIMITED"
)

// CreateRunRequest is the request body for POST /v1/runs.
type CreateRunRequest struct {
	AgentID     string         `json:"agent_id"`
	OrgID       uuid.UUID      `json:"-"` // Set from JWT claims, not from request body.
	TraceID     *string        `json:"trace_id,omitempty"`
	ParentRunID *uuid.UUID     `json:"parent_run_id,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// AppendEventsRequest is the request body for POST /v1/runs/{run_id}/events.
type AppendEventsRequest struct {
	Events []EventInput `json:"events"`
}

// EventInput is a single event in an append request.
type EventInput struct {
	EventType  EventType      `json:"event_type"`
	OccurredAt *time.Time     `json:"occurred_at,omitempty"`
	Payload    map[string]any `json:"payload"`
}

// CompleteRunRequest is the request body for POST /v1/runs/{run_id}/complete.
type CompleteRunRequest struct {
	Status   string         `json:"status"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// TraceRequest is the convenience request for POST /v1/trace.
type TraceRequest struct {
	AgentID      string         `json:"agent_id"`
	TraceID      *string        `json:"trace_id,omitempty"`
	Decision     TraceDecision  `json:"decision"`
	PrecedentRef *uuid.UUID     `json:"precedent_ref,omitempty"` // decision that influenced this one
	Metadata     map[string]any `json:"metadata,omitempty"`
	Context      map[string]any `json:"context,omitempty"` // Agent context (model, task, repo, branch).
}

// TraceDecision is the decision portion of a trace convenience request.
type TraceDecision struct {
	DecisionType string             `json:"decision_type"`
	Outcome      string             `json:"outcome"`
	Confidence   float32            `json:"confidence"`
	Reasoning    *string            `json:"reasoning,omitempty"`
	Alternatives []TraceAlternative `json:"alternatives,omitempty"`
	Evidence     []TraceEvidence    `json:"evidence,omitempty"`
}

// TraceAlternative is an alternative in a trace convenience request.
type TraceAlternative struct {
	Label           string   `json:"label"`
	Score           *float32 `json:"score,omitempty"`
	Selected        bool     `json:"selected"`
	RejectionReason *string  `json:"rejection_reason,omitempty"`
}

// TraceEvidence is evidence in a trace convenience request.
type TraceEvidence struct {
	SourceType     string   `json:"source_type"`
	SourceURI      *string  `json:"source_uri,omitempty"`
	Content        string   `json:"content"`
	RelevanceScore *float32 `json:"relevance_score,omitempty"`
}

// AuthTokenRequest is the request body for POST /auth/token.
type AuthTokenRequest struct {
	AgentID string `json:"agent_id"`
	APIKey  string `json:"api_key"`
}

// AuthTokenResponse is the response for POST /auth/token.
type AuthTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CreateAgentRequest is the request body for POST /v1/agents.
type CreateAgentRequest struct {
	AgentID  string         `json:"agent_id"`
	Name     string         `json:"name"`
	Role     AgentRole      `json:"role"`
	APIKey   string         `json:"api_key"`
	Tags     []string       `json:"tags,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// UpdateAgentRequest is the request body for PATCH /v1/agents/{agent_id}.
type UpdateAgentRequest struct {
	Name     *string        `json:"name,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// UpdateAgentTagsRequest is the request body for PATCH /v1/agents/{agent_id}/tags.
type UpdateAgentTagsRequest struct {
	Tags []string `json:"tags"`
}

// CreateGrantRequest is the request body for POST /v1/grants.
type CreateGrantRequest struct {
	GranteeAgentID string  `json:"grantee_agent_id"`
	ResourceType   string  `json:"resource_type"`
	ResourceID     *string `json:"resource_id,omitempty"`
	Permission     string  `json:"permission"`
	ExpiresAt      *string `json:"expires_at,omitempty"`
}

// HealthResponse is the response for GET /health.
type HealthResponse struct {
	Status       string `json:"status"`
	Version      string `json:"version"`
	Postgres     string `json:"postgres"`
	Qdrant       string `json:"qdrant,omitempty"`
	BufferDepth  int    `json:"buffer_depth"`
	BufferStatus string `json:"buffer_status"` // "ok", "high", "critical"
	SSEBroker    string `json:"sse_broker,omitempty"`
	Uptime       int64  `json:"uptime_seconds"`
}

// Organization represents a tenant in the multi-tenancy model.
type Organization struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Plan      string    `json:"plan"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
