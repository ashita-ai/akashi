package kyoyu

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
)

// Config holds the settings needed to construct a Client.
type Config struct {
	// BaseURL is the root URL of the Kyoyu server (e.g. "http://localhost:8080").
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

// Client is an HTTP client for the Kyoyu decision-tracing API.
// All methods are safe for concurrent use.
type Client struct {
	baseURL  string
	agentID  string
	client   *http.Client
	tokenMgr *tokenManager
}

// NewClient creates a Client from the given configuration.
func NewClient(cfg Config) *Client {
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
	}
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
// Wire-format body builders
// ---------------------------------------------------------------------------

// traceBody is the wire format for POST /v1/trace. The server expects a
// nested "decision" object rather than flat fields.
type traceBody struct {
	AgentID  string          `json:"agent_id"`
	Decision traceDecision   `json:"decision"`
	Metadata map[string]any  `json:"metadata,omitempty"`
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
		Metadata: req.Metadata,
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
		return fmt.Errorf("kyoyu: marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("kyoyu: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	return c.doRequest(ctx, req, dest)
}

func (c *Client) get(ctx context.Context, path string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("kyoyu: create request: %w", err)
	}

	return c.doRequest(ctx, req, dest)
}

func (c *Client) doRequest(ctx context.Context, req *http.Request, dest any) error {
	token, err := c.tokenMgr.getToken(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("kyoyu: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()

	return handleResponse(resp, dest)
}

func handleResponse(resp *http.Response, dest any) error {
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("kyoyu: read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		return parseErrorResponse(resp.StatusCode, bodyBytes)
	}

	// Unwrap the server's { "data": ... } envelope.
	var envelope apiEnvelope
	if err := json.Unmarshal(bodyBytes, &envelope); err != nil {
		return fmt.Errorf("kyoyu: decode response envelope: %w", err)
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
