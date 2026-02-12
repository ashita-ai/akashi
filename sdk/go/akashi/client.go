package akashi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Config holds the settings needed to construct a Client.
type Config struct {
	// BaseURL is the root URL of the Akashi server (e.g. "http://localhost:8080").
	BaseURL string

	// AgentID identifies this agent for authentication and tracing.
	AgentID string

	// APIKey is the secret used to obtain a JWT token.
	APIKey string

	// HTTPClient is an optional custom HTTP client. If nil, a default client
	// with a 30-second timeout is used.
	HTTPClient *http.Client

	// Timeout applies to individual API requests. Defaults to 30 seconds.
	Timeout time.Duration
}

// Client is an HTTP client for the Akashi decision-tracing API.
// All methods are safe for concurrent use.
type Client struct {
	baseURL  string
	agentID  string
	client   *http.Client
	tokenMgr *tokenManager
}

// NewClient creates a Client from the given configuration.
// Returns an error if BaseURL, AgentID, or APIKey is empty.
func NewClient(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("akashi: BaseURL is required")
	}
	if cfg.AgentID == "" {
		return nil, fmt.Errorf("akashi: AgentID is required")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("akashi: APIKey is required")
	}

	baseURL := strings.TrimRight(cfg.BaseURL, "/")

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		timeout := cfg.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		httpClient = &http.Client{Timeout: timeout}
	}

	return &Client{
		baseURL:  baseURL,
		agentID:  cfg.AgentID,
		client:   httpClient,
		tokenMgr: newTokenManager(baseURL, cfg.AgentID, cfg.APIKey, httpClient),
	}, nil
}

// Check looks up existing decisions before making a new one.
// If query is non-empty, a semantic search is performed; otherwise a
// structured lookup by decision type is used. Conflicts on the decision
// type are always included.
func (c *Client) Check(ctx context.Context, req CheckRequest) (*CheckResponse, error) {
	if req.Limit <= 0 {
		req.Limit = 5
	}
	var resp CheckResponse
	if err := c.post(ctx, "/v1/check", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Trace records a decision so other agents can learn from it.
// The client's AgentID is automatically included in the request body.
func (c *Client) Trace(ctx context.Context, req TraceRequest) (*TraceResponse, error) {
	body := buildTraceBody(c.agentID, req)
	var resp TraceResponse
	if err := c.post(ctx, "/v1/trace", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Query retrieves past decisions with structured filters and pagination.
// Nil opts are replaced with sensible defaults.
func (c *Client) Query(ctx context.Context, filters *QueryFilters, opts *QueryOptions) (*QueryResponse, error) {
	body := buildQueryBody(filters, opts)
	var resp QueryResponse
	if err := c.post(ctx, "/v1/query", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Search performs a semantic similarity search over decision history.
func (c *Client) Search(ctx context.Context, query string, limit int) (*SearchResponse, error) {
	if limit <= 0 {
		limit = 5
	}
	body := map[string]any{"query": query, "limit": limit}
	var resp SearchResponse
	if err := c.post(ctx, "/v1/search", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// RecentOptions are optional filters for the Recent method.
type RecentOptions struct {
	Limit        int
	AgentID      string
	DecisionType string
}

// Recent returns the most recent decisions, optionally filtered.
func (c *Client) Recent(ctx context.Context, opts *RecentOptions) ([]Decision, error) {
	params := url.Values{}
	if opts != nil {
		if opts.Limit > 0 {
			params.Set("limit", strconv.Itoa(opts.Limit))
		}
		if opts.AgentID != "" {
			params.Set("agent_id", opts.AgentID)
		}
		if opts.DecisionType != "" {
			params.Set("decision_type", opts.DecisionType)
		}
	}
	if params.Get("limit") == "" {
		params.Set("limit", "10")
	}

	path := "/v1/decisions/recent?" + params.Encode()
	var resp recentResponse
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	return resp.Decisions, nil
}

// ---------------------------------------------------------------------------
// Run lifecycle
// ---------------------------------------------------------------------------

// CreateRun starts a new agent run.
func (c *Client) CreateRun(ctx context.Context, req CreateRunRequest) (*AgentRun, error) {
	var resp AgentRun
	if err := c.post(ctx, "/v1/runs", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// AppendEvents appends events to an existing run.
func (c *Client) AppendEvents(ctx context.Context, runID uuid.UUID, events []EventInput) (*AppendEventsResponse, error) {
	body := map[string]any{"events": events}
	var resp AppendEventsResponse
	if err := c.post(ctx, "/v1/runs/"+runID.String()+"/events", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CompleteRun marks a run as completed or failed.
func (c *Client) CompleteRun(ctx context.Context, runID uuid.UUID, status string, metadata map[string]any) (*AgentRun, error) {
	body := CompleteRunRequest{Status: status, Metadata: metadata}
	var resp AgentRun
	if err := c.post(ctx, "/v1/runs/"+runID.String()+"/complete", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetRun retrieves a run with its events and decisions.
func (c *Client) GetRun(ctx context.Context, runID uuid.UUID) (*GetRunResponse, error) {
	var resp GetRunResponse
	if err := c.get(ctx, "/v1/runs/"+runID.String(), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Agents (admin-only)
// ---------------------------------------------------------------------------

// CreateAgent creates a new agent identity. Requires admin role.
func (c *Client) CreateAgent(ctx context.Context, req CreateAgentRequest) (*Agent, error) {
	var resp Agent
	if err := c.post(ctx, "/v1/agents", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListAgents lists all agents in the caller's organization. Requires admin role.
func (c *Client) ListAgents(ctx context.Context) ([]Agent, error) {
	var resp []Agent
	if err := c.get(ctx, "/v1/agents", &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// DeleteAgent deletes an agent and all associated data. Requires admin role.
func (c *Client) DeleteAgent(ctx context.Context, agentID string) (*DeleteAgentResponse, error) {
	var resp DeleteAgentResponse
	if err := c.doDelete(ctx, "/v1/agents/"+agentID, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Temporal queries
// ---------------------------------------------------------------------------

// TemporalQuery retrieves decisions as they existed at a specific point in time.
func (c *Client) TemporalQuery(ctx context.Context, asOf time.Time, filters *QueryFilters) (*TemporalQueryResponse, error) {
	body := TemporalQueryRequest{AsOf: asOf}
	if filters != nil {
		body.Filters = *filters
	}
	var resp TemporalQueryResponse
	if err := c.post(ctx, "/v1/query/temporal", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// AgentHistory retrieves the decision history for a specific agent.
func (c *Client) AgentHistory(ctx context.Context, agentID string, limit int) (*AgentHistoryResponse, error) {
	if limit <= 0 {
		limit = 50
	}
	params := url.Values{}
	params.Set("limit", strconv.Itoa(limit))

	path := "/v1/agents/" + agentID + "/history?" + params.Encode()
	var resp AgentHistoryResponse
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Grants
// ---------------------------------------------------------------------------

// CreateGrant creates a fine-grained access grant between agents.
func (c *Client) CreateGrant(ctx context.Context, req CreateGrantRequest) (*Grant, error) {
	var resp Grant
	if err := c.post(ctx, "/v1/grants", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteGrant revokes an access grant. Returns nil on success (204 No Content).
func (c *Client) DeleteGrant(ctx context.Context, grantID uuid.UUID) error {
	return c.doDelete(ctx, "/v1/grants/"+grantID.String(), nil)
}

// ---------------------------------------------------------------------------
// Integrity
// ---------------------------------------------------------------------------

// GetDecisionRevisions retrieves the full revision chain for a decision.
func (c *Client) GetDecisionRevisions(ctx context.Context, decisionID uuid.UUID) (*RevisionsResponse, error) {
	var resp RevisionsResponse
	if err := c.get(ctx, "/v1/decisions/"+decisionID.String()+"/revisions", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// VerifyDecision recomputes the SHA-256 content hash for a decision and
// compares it to the stored hash. Returns whether the decision is intact.
func (c *Client) VerifyDecision(ctx context.Context, decisionID uuid.UUID) (*VerifyResponse, error) {
	var resp VerifyResponse
	if err := c.get(ctx, "/v1/verify/"+decisionID.String(), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Agent tags
// ---------------------------------------------------------------------------

// UpdateAgentTags replaces the tags for an agent. Requires admin role.
func (c *Client) UpdateAgentTags(ctx context.Context, agentID string, tags []string) (*Agent, error) {
	body := map[string]any{"tags": tags}
	var resp Agent
	if err := c.patch(ctx, "/v1/agents/"+agentID+"/tags", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Conflicts, usage, and health
// ---------------------------------------------------------------------------

// ListConflicts retrieves detected decision conflicts, optionally filtered.
func (c *Client) ListConflicts(ctx context.Context, opts *ConflictOptions) (*ConflictsResponse, error) {
	params := url.Values{}
	if opts != nil {
		if opts.DecisionType != "" {
			params.Set("decision_type", opts.DecisionType)
		}
		if opts.AgentID != "" {
			params.Set("agent_id", opts.AgentID)
		}
		if opts.Limit > 0 {
			params.Set("limit", strconv.Itoa(opts.Limit))
		}
		if opts.Offset > 0 {
			params.Set("offset", strconv.Itoa(opts.Offset))
		}
	}

	path := "/v1/conflicts"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var resp ConflictsResponse
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Health checks the server's health status. This endpoint does not require
// authentication and will work even if the client has invalid credentials.
func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	var resp HealthResponse
	if err := c.getNoAuth(ctx, "/health", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Wire-format body builders
// ---------------------------------------------------------------------------

// traceBody is the wire format for POST /v1/trace. The server expects a
// nested "decision" object rather than flat fields.
type traceBody struct {
	AgentID      string         `json:"agent_id"`
	Decision     traceDecision  `json:"decision"`
	PrecedentRef *uuid.UUID     `json:"precedent_ref,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

type traceDecision struct {
	DecisionType string             `json:"decision_type"`
	Outcome      string             `json:"outcome"`
	Confidence   float32            `json:"confidence"`
	Reasoning    *string            `json:"reasoning,omitempty"`
	Alternatives []TraceAlternative `json:"alternatives,omitempty"`
	Evidence     []TraceEvidence    `json:"evidence,omitempty"`
}

func buildTraceBody(agentID string, req TraceRequest) traceBody {
	return traceBody{
		AgentID: agentID,
		Decision: traceDecision{
			DecisionType: req.DecisionType,
			Outcome:      req.Outcome,
			Confidence:   req.Confidence,
			Reasoning:    req.Reasoning,
			Alternatives: req.Alternatives,
			Evidence:     req.Evidence,
		},
		PrecedentRef: req.PrecedentRef,
		Metadata:     req.Metadata,
	}
}

// queryBody is the wire format for POST /v1/query.
type queryBody struct {
	Filters  QueryFilters `json:"filters"`
	Limit    int          `json:"limit"`
	Offset   int          `json:"offset"`
	OrderBy  string       `json:"order_by"`
	OrderDir string       `json:"order_dir"`
}

func buildQueryBody(filters *QueryFilters, opts *QueryOptions) queryBody {
	b := queryBody{
		Limit:    50,
		Offset:   0,
		OrderBy:  "valid_from",
		OrderDir: "desc",
	}
	if filters != nil {
		b.Filters = *filters
	}
	if opts != nil {
		if opts.Limit > 0 {
			b.Limit = opts.Limit
		}
		if opts.Offset > 0 {
			b.Offset = opts.Offset
		}
		if opts.OrderBy != "" {
			b.OrderBy = opts.OrderBy
		}
		if opts.OrderDir != "" {
			b.OrderDir = opts.OrderDir
		}
	}
	return b
}

type recentResponse struct {
	Decisions []Decision `json:"decisions"`
}

// ---------------------------------------------------------------------------
// HTTP transport
// ---------------------------------------------------------------------------

// apiEnvelope is the server's standard response wrapper.
type apiEnvelope struct {
	Data json.RawMessage `json:"data"`
}

// apiErrorEnvelope is the server's standard error response wrapper.
type apiErrorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) post(ctx context.Context, path string, body any, dest any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("akashi: marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("akashi: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	return c.doRequest(ctx, req, dest)
}

func (c *Client) get(ctx context.Context, path string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("akashi: create request: %w", err)
	}

	return c.doRequest(ctx, req, dest)
}

func (c *Client) doDelete(ctx context.Context, path string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("akashi: create request: %w", err)
	}

	return c.doRequest(ctx, req, dest)
}

func (c *Client) patch(ctx context.Context, path string, body any, dest any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("akashi: marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("akashi: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	return c.doRequest(ctx, req, dest)
}

func (c *Client) getNoAuth(ctx context.Context, path string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("akashi: create request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("akashi: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	return handleResponse(resp, dest)
}

func (c *Client) doRequest(ctx context.Context, req *http.Request, dest any) error {
	token, err := c.tokenMgr.getToken(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("akashi: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	return handleResponse(resp, dest)
}

func handleResponse(resp *http.Response, dest any) error {
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("akashi: read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		return parseErrorResponse(resp.StatusCode, bodyBytes)
	}

	// 204 No Content — nothing to decode.
	if resp.StatusCode == http.StatusNoContent || dest == nil {
		return nil
	}

	// Unwrap the server's { "data": ... } envelope.
	var envelope apiEnvelope
	if err := json.Unmarshal(bodyBytes, &envelope); err != nil {
		return fmt.Errorf("akashi: decode response envelope: %w", err)
	}

	if envelope.Data == nil {
		// Fallback: some endpoints may not wrap in "data".
		return json.Unmarshal(bodyBytes, dest)
	}

	return json.Unmarshal(envelope.Data, dest)
}

func parseErrorResponse(statusCode int, body []byte) *Error {
	apiErr := &Error{StatusCode: statusCode}

	var envelope apiErrorEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Message != "" {
		apiErr.Code = envelope.Error.Code
		apiErr.Message = envelope.Error.Message
	} else {
		apiErr.Code = http.StatusText(statusCode)
		apiErr.Message = string(body)
	}

	return apiErr
}
