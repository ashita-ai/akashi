package model

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Field length limits for TraceDecision fields.
// These prevent a single oversized field from exhausting the embedding
// pipeline, triggering quadratic conflict extraction cost, or filling
// Postgres TEXT columns with caller-controlled garbage.
const (
	MaxDecisionTypeLen = 200
	MaxOutcomeLen      = 32 * 1024 // 32 KB
	MaxReasoningLen    = 64 * 1024 // 64 KB
)

// privateIPRanges is the set of CIDR blocks considered non-public.
// Populated once at package init; used by ValidateSourceURI.
var privateIPRanges []*net.IPNet

func init() {
	for _, cidr := range []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16", // link-local
		"::1/128",
		"fc00::/7",  // unique-local IPv6
		"fe80::/10", // link-local IPv6
	} {
		_, network, err := net.ParseCIDR(cidr)
		if err == nil {
			privateIPRanges = append(privateIPRanges, network)
		}
	}
}

// ValidateTraceDecision checks per-field length limits on the fields that flow
// into the embedding pipeline and Postgres TEXT columns.
func ValidateTraceDecision(d TraceDecision) error {
	if len(d.DecisionType) > MaxDecisionTypeLen {
		return fmt.Errorf("decision_type exceeds maximum length of %d characters", MaxDecisionTypeLen)
	}
	if len(d.Outcome) > MaxOutcomeLen {
		return fmt.Errorf("outcome exceeds maximum length of %d bytes", MaxOutcomeLen)
	}
	if d.Reasoning != nil && len(*d.Reasoning) > MaxReasoningLen {
		return fmt.Errorf("reasoning exceeds maximum length of %d bytes", MaxReasoningLen)
	}
	for i, ev := range d.Evidence {
		if ev.SourceURI != nil {
			if err := ValidateSourceURI(*ev.SourceURI); err != nil {
				return fmt.Errorf("evidence[%d].source_uri: %w", i, err)
			}
		}
	}
	return nil
}

// ValidateSourceURI ensures a source_uri is a safe, publicly-routable http/https URL.
// Rejects javascript: and file: schemes (XSS via UI), credentials embedded in
// the URL, and private/loopback addresses (future SSRF surface).
func ValidateSourceURI(rawURI string) error {
	u, err := url.Parse(rawURI)
	if err != nil {
		return fmt.Errorf("invalid URI: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("source_uri must use http or https scheme (got %q)", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("source_uri must not include credentials")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("source_uri must include a host")
	}
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("source_uri must not point to localhost")
	}
	if ip := net.ParseIP(host); ip != nil {
		for _, r := range privateIPRanges {
			if r.Contains(ip) {
				return fmt.Errorf("source_uri must not point to a private or loopback address")
			}
		}
	}
	return nil
}

// APIResponse is the standard response envelope for all HTTP API responses.
type APIResponse struct {
	Data any          `json:"data,omitempty"`
	Meta ResponseMeta `json:"meta"`
}

// ListResponse is the standard envelope for paginated list endpoints.
// The array of items is in Data; Total is omitted when access-filtering
// makes the DB total unreliable (i.e., some rows were hidden by grants).
type ListResponse struct {
	Data    any          `json:"data"`
	Total   *int         `json:"total,omitempty"`
	HasMore bool         `json:"has_more"`
	Limit   int          `json:"limit"`
	Offset  int          `json:"offset"`
	Meta    ResponseMeta `json:"meta"`
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

// ScopedTokenRequest is the request body for POST /auth/scoped-token.
type ScopedTokenRequest struct {
	AsAgentID string `json:"as_agent_id"`
	ExpiresIn int    `json:"expires_in,omitempty"` // seconds; defaults to 300, capped at 3600
}

// ScopedTokenResponse is the response for POST /auth/scoped-token.
type ScopedTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	AsAgentID string    `json:"as_agent_id"`
	ScopedBy  string    `json:"scoped_by"`
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
