package akashi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Phase 2: Decision details
// ---------------------------------------------------------------------------

// GetDecision retrieves a single decision by ID with enrichments.
func (c *Client) GetDecision(ctx context.Context, decisionID uuid.UUID) (*Decision, error) {
	var resp Decision
	if err := c.get(ctx, "/v1/decisions/"+decisionID.String(), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetDecisionConflicts retrieves conflicts involving a specific decision.
func (c *Client) GetDecisionConflicts(ctx context.Context, decisionID uuid.UUID, opts *DecisionConflictsOptions) (*DecisionConflictsResponse, error) {
	params := url.Values{}
	if opts != nil {
		if opts.Status != "" {
			params.Set("status", opts.Status)
		}
		if opts.Limit > 0 {
			params.Set("limit", strconv.Itoa(opts.Limit))
		}
		if opts.Offset > 0 {
			params.Set("offset", strconv.Itoa(opts.Offset))
		}
	}
	path := "/v1/decisions/" + decisionID.String() + "/conflicts"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var items []DecisionConflict
	env, err := c.doGetList(ctx, path, &items)
	if err != nil {
		return nil, err
	}
	total := 0
	if env.Total != nil {
		total = *env.Total
	}
	return &DecisionConflictsResponse{
		Conflicts: items,
		Total:     total,
		HasMore:   env.HasMore,
		Limit:     env.Limit,
		Offset:    env.Offset,
	}, nil
}

// GetDecisionLineage retrieves the precedent chain for a decision.
func (c *Client) GetDecisionLineage(ctx context.Context, decisionID uuid.UUID) (*LineageResponse, error) {
	var resp LineageResponse
	if err := c.get(ctx, "/v1/decisions/"+decisionID.String()+"/lineage", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetDecisionTimeline returns decisions aggregated into time buckets.
func (c *Client) GetDecisionTimeline(ctx context.Context, opts *TimelineOptions) (*TimelineResponse, error) {
	params := url.Values{}
	if opts != nil {
		if opts.Granularity != "" {
			params.Set("granularity", opts.Granularity)
		}
		if opts.From != nil {
			params.Set("from", opts.From.Format("2006-01-02T15:04:05Z07:00"))
		}
		if opts.To != nil {
			params.Set("to", opts.To.Format("2006-01-02T15:04:05Z07:00"))
		}
		if opts.AgentID != "" {
			params.Set("agent_id", opts.AgentID)
		}
		if opts.Project != "" {
			params.Set("project", opts.Project)
		}
	}
	path := "/v1/decisions/timeline"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var resp TimelineResponse
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetDecisionFacets returns the distinct decision types and projects.
func (c *Client) GetDecisionFacets(ctx context.Context) (*FacetsResponse, error) {
	var resp FacetsResponse
	if err := c.get(ctx, "/v1/decisions/facets", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// RetractDecision soft-deletes a decision by setting valid_to.
// Requires admin role.
func (c *Client) RetractDecision(ctx context.Context, decisionID uuid.UUID, reason string) (*Decision, error) {
	body := RetractDecisionRequest{Reason: reason}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("akashi: marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/v1/decisions/"+decisionID.String(), bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("akashi: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	var resp Decision
	if err := c.doRequest(ctx, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PatchDecision updates mutable metadata on a decision.
// Currently supports updating the project field. Requires admin role.
func (c *Client) PatchDecision(ctx context.Context, decisionID uuid.UUID, patch PatchDecisionRequest) (*Decision, error) {
	encoded, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("akashi: marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL+"/v1/decisions/"+decisionID.String(), bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("akashi: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	var resp Decision
	if err := c.doRequest(ctx, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// EraseDecision permanently erases a decision's content (GDPR).
// Requires org_owner or higher role.
func (c *Client) EraseDecision(ctx context.Context, decisionID uuid.UUID, reason string) (*EraseDecisionResponse, error) {
	body := EraseDecisionRequest{Reason: reason}
	var resp EraseDecisionResponse
	if err := c.post(ctx, "/v1/decisions/"+decisionID.String()+"/erase", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Phase 2: Conflict management
// ---------------------------------------------------------------------------

// GetConflict retrieves a single conflict with recommendation.
func (c *Client) GetConflict(ctx context.Context, conflictID uuid.UUID) (*ConflictDetail, error) {
	var resp ConflictDetail
	if err := c.get(ctx, "/v1/conflicts/"+conflictID.String(), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// AdjudicateConflict resolves a conflict by recording a decision.
func (c *Client) AdjudicateConflict(ctx context.Context, conflictID uuid.UUID, req AdjudicateConflictRequest) (*ConflictDetail, error) {
	var resp ConflictDetail
	if err := c.post(ctx, "/v1/conflicts/"+conflictID.String()+"/adjudicate", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PatchConflict updates the status of a conflict (resolve or mark as false positive).
func (c *Client) PatchConflict(ctx context.Context, conflictID uuid.UUID, req ConflictStatusUpdate) (*DecisionConflict, error) {
	var resp DecisionConflict
	if err := c.patch(ctx, "/v1/conflicts/"+conflictID.String(), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListConflictGroups retrieves groups of related conflicts.
func (c *Client) ListConflictGroups(ctx context.Context, opts *ConflictGroupOptions) (*ConflictGroupsResponse, error) {
	params := url.Values{}
	if opts != nil {
		if opts.DecisionType != "" {
			params.Set("decision_type", opts.DecisionType)
		}
		if opts.AgentID != "" {
			params.Set("agent_id", opts.AgentID)
		}
		if opts.ConflictKind != "" {
			params.Set("conflict_kind", opts.ConflictKind)
		}
		if opts.Status != "" {
			params.Set("status", opts.Status)
		}
		if opts.Limit > 0 {
			params.Set("limit", strconv.Itoa(opts.Limit))
		}
		if opts.Offset > 0 {
			params.Set("offset", strconv.Itoa(opts.Offset))
		}
	}
	path := "/v1/conflict-groups"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var items []ConflictGroup
	env, err := c.doGetList(ctx, path, &items)
	if err != nil {
		return nil, err
	}
	total := 0
	if env.Total != nil {
		total = *env.Total
	}
	return &ConflictGroupsResponse{
		Groups:  items,
		Total:   total,
		HasMore: env.HasMore,
		Limit:   env.Limit,
		Offset:  env.Offset,
	}, nil
}

// ResolveConflictGroup resolves all conflicts in a group.
func (c *Client) ResolveConflictGroup(ctx context.Context, groupID uuid.UUID, req ResolveConflictGroupRequest) (*ResolveConflictGroupResponse, error) {
	var resp ResolveConflictGroupResponse
	if err := c.patch(ctx, "/v1/conflict-groups/"+groupID.String()+"/resolve", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetConflictAnalytics retrieves aggregate conflict metrics.
func (c *Client) GetConflictAnalytics(ctx context.Context, opts *ConflictAnalyticsOptions) (*ConflictAnalyticsResponse, error) {
	params := url.Values{}
	if opts != nil {
		if opts.Period != "" {
			params.Set("period", opts.Period)
		}
		if opts.From != nil {
			params.Set("from", opts.From.Format("2006-01-02T15:04:05Z07:00"))
		}
		if opts.To != nil {
			params.Set("to", opts.To.Format("2006-01-02T15:04:05Z07:00"))
		}
		if opts.AgentID != "" {
			params.Set("agent_id", opts.AgentID)
		}
		if opts.DecisionType != "" {
			params.Set("decision_type", opts.DecisionType)
		}
		if opts.ConflictKind != "" {
			params.Set("conflict_kind", opts.ConflictKind)
		}
	}
	path := "/v1/conflicts/analytics"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var resp ConflictAnalyticsResponse
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Phase 3: API key management (admin-only)
// ---------------------------------------------------------------------------

// CreateKey creates a new API key. Requires admin role.
func (c *Client) CreateKey(ctx context.Context, req CreateKeyRequest) (*APIKeyWithRawKey, error) {
	var resp APIKeyWithRawKey
	if err := c.post(ctx, "/v1/keys", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListKeys lists all API keys in the organization. Requires admin role.
func (c *Client) ListKeys(ctx context.Context, limit, offset int) ([]APIKey, error) {
	params := url.Values{}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		params.Set("offset", strconv.Itoa(offset))
	}
	path := "/v1/keys"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var items []APIKey
	if _, err := c.doGetList(ctx, path, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// RevokeKey revokes an API key. Requires admin role.
func (c *Client) RevokeKey(ctx context.Context, keyID uuid.UUID) error {
	return c.doDelete(ctx, "/v1/keys/"+keyID.String(), nil)
}

// RotateKey rotates an API key, revoking the old one and returning a new one.
// Requires admin role.
func (c *Client) RotateKey(ctx context.Context, keyID uuid.UUID) (*RotateKeyResponse, error) {
	var resp RotateKeyResponse
	if err := c.post(ctx, "/v1/keys/"+keyID.String()+"/rotate", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Phase 3: Org settings
// ---------------------------------------------------------------------------

// GetOrgSettings retrieves the organization's settings.
func (c *Client) GetOrgSettings(ctx context.Context) (*OrgSettingsData, error) {
	var resp OrgSettingsData
	if err := c.get(ctx, "/v1/org/settings", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SetOrgSettings updates the organization's settings. Requires admin role.
func (c *Client) SetOrgSettings(ctx context.Context, req OrgSettingsData) (*OrgSettingsData, error) {
	var resp OrgSettingsData
	if err := c.put(ctx, "/v1/org/settings", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Phase 3: Retention & legal holds
// ---------------------------------------------------------------------------

// GetRetention retrieves the retention policy and active holds.
func (c *Client) GetRetention(ctx context.Context) (*RetentionPolicy, error) {
	var resp RetentionPolicy
	if err := c.get(ctx, "/v1/retention", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SetRetention updates the retention policy. Requires admin role.
func (c *Client) SetRetention(ctx context.Context, req SetRetentionRequest) (*RetentionPolicy, error) {
	var resp RetentionPolicy
	if err := c.put(ctx, "/v1/retention", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PurgeDecisions deletes decisions older than the given timestamp.
// Requires admin role. Set DryRun to true to preview without deleting.
func (c *Client) PurgeDecisions(ctx context.Context, req PurgeRequest) (*PurgeResponse, error) {
	var resp PurgeResponse
	if err := c.post(ctx, "/v1/retention/purge", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CreateHold creates a retention hold to prevent purging.
// Requires admin role.
func (c *Client) CreateHold(ctx context.Context, req CreateHoldRequest) (*RetentionHold, error) {
	var resp RetentionHold
	if err := c.post(ctx, "/v1/retention/hold", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ReleaseHold releases a retention hold. Requires admin role.
func (c *Client) ReleaseHold(ctx context.Context, holdID uuid.UUID) error {
	return c.doDelete(ctx, "/v1/retention/hold/"+holdID.String(), nil)
}

// ---------------------------------------------------------------------------
// Phase 3: Project links
// ---------------------------------------------------------------------------

// CreateProjectLink creates a link between two projects. Requires admin role.
func (c *Client) CreateProjectLink(ctx context.Context, req CreateProjectLinkRequest) (*ProjectLink, error) {
	var resp ProjectLink
	if err := c.post(ctx, "/v1/project-links", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListProjectLinks lists all project links. Requires admin role.
func (c *Client) ListProjectLinks(ctx context.Context, limit, offset int) ([]ProjectLink, error) {
	params := url.Values{}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		params.Set("offset", strconv.Itoa(offset))
	}
	path := "/v1/project-links"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var items []ProjectLink
	if _, err := c.doGetList(ctx, path, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// DeleteProjectLink deletes a project link. Requires admin role.
func (c *Client) DeleteProjectLink(ctx context.Context, linkID uuid.UUID) error {
	return c.doDelete(ctx, "/v1/project-links/"+linkID.String(), nil)
}

// GrantAllProjectLinks creates conflict-scope links between all projects.
// Requires admin role.
func (c *Client) GrantAllProjectLinks(ctx context.Context, linkType string) (*GrantAllProjectLinksResponse, error) {
	body := GrantAllProjectLinksRequest{LinkType: linkType}
	var resp GrantAllProjectLinksResponse
	if err := c.post(ctx, "/v1/project-links/grant-all", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Phase 3: Export, integrity, trace health, usage
// ---------------------------------------------------------------------------

// ExportDecisions streams decisions as NDJSON. Returns channels for decisions
// and errors. The caller must call cancel when done to release the HTTP connection.
func (c *Client) ExportDecisions(ctx context.Context, opts *ExportOptions) (decisions <-chan Decision, errs <-chan error, cancel func()) {
	params := url.Values{}
	if opts != nil {
		if opts.AgentID != "" {
			params.Set("agent_id", opts.AgentID)
		}
		if opts.DecisionType != "" {
			params.Set("decision_type", opts.DecisionType)
		}
		if opts.From != nil {
			params.Set("from", opts.From.Format("2006-01-02T15:04:05Z07:00"))
		}
		if opts.To != nil {
			params.Set("to", opts.To.Format("2006-01-02T15:04:05Z07:00"))
		}
	}
	path := "/v1/export/decisions"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	ctx, cancelFn := context.WithCancel(ctx)
	dch := make(chan Decision, 64)
	ech := make(chan error, 1)

	go func() {
		defer close(dch)
		defer close(ech)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
		if err != nil {
			ech <- fmt.Errorf("akashi: create request: %w", err)
			return
		}
		resp, err := c.execRequest(ctx, req)
		if err != nil {
			ech <- err
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
			ech <- parseErrorResponse(resp.StatusCode, body)
			return
		}

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var raw map[string]any
			if jsonErr := json.Unmarshal(line, &raw); jsonErr != nil {
				ech <- fmt.Errorf("akashi: decode export line: %w", jsonErr)
				return
			}
			if _, ok := raw["__error"]; ok {
				msg, _ := raw["message"].(string)
				if msg == "" {
					msg = "export terminated due to internal error"
				}
				ech <- &Error{StatusCode: 500, Code: "export_error", Message: msg}
				return
			}
			var d Decision
			if jsonErr := json.Unmarshal(line, &d); jsonErr != nil {
				ech <- fmt.Errorf("akashi: decode export line: %w", jsonErr)
				return
			}
			select {
			case dch <- d:
			case <-ctx.Done():
				return
			}
		}
		if scanErr := scanner.Err(); scanErr != nil {
			ech <- fmt.Errorf("akashi: read export stream: %w", scanErr)
		}
	}()

	return dch, ech, cancelFn
}

// Subscribe opens an SSE connection to /v1/subscribe and returns a channel
// of real-time decision and conflict events scoped to the caller's org.
// The caller must call the returned cancel function when done to close the
// connection and release resources.
//
// The SSE connection uses a dedicated HTTP client with no timeout so that
// long-lived idle connections are not killed. Keepalive comments from the
// server are silently consumed.
func (c *Client) Subscribe(ctx context.Context) (events <-chan SubscriptionEvent, errs <-chan error, cancel func()) {
	ctx, cancelFn := context.WithCancel(ctx)
	ech := make(chan SubscriptionEvent, 64)
	errch := make(chan error, 1)

	go func() {
		defer close(ech)
		defer close(errch)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/subscribe", nil)
		if err != nil {
			errch <- fmt.Errorf("akashi: create subscribe request: %w", err)
			return
		}
		req.Header.Set("Accept", "text/event-stream")

		// Use execRequest for auth headers, but override the response handling
		// since SSE is a long-lived stream rather than a single response.
		resp, err := c.execRequest(ctx, req)
		if err != nil {
			errch <- err
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
			errch <- parseErrorResponse(resp.StatusCode, body)
			return
		}

		scanner := bufio.NewScanner(resp.Body)
		var eventType string
		var dataBuf bytes.Buffer

		for scanner.Scan() {
			line := scanner.Text()

			// SSE comment (keepalive).
			if strings.HasPrefix(line, ":") {
				continue
			}

			// Empty line = end of event.
			if line == "" {
				if eventType != "" && dataBuf.Len() > 0 {
					var data map[string]any
					if jsonErr := json.Unmarshal(dataBuf.Bytes(), &data); jsonErr != nil {
						errch <- fmt.Errorf("akashi: decode SSE data: %w", jsonErr)
						return
					}
					evt := SubscriptionEvent{
						EventType: eventType,
						Data:      data,
					}
					select {
					case ech <- evt:
					case <-ctx.Done():
						return
					}
				}
				eventType = ""
				dataBuf.Reset()
				continue
			}

			if strings.HasPrefix(line, "event: ") {
				eventType = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				if dataBuf.Len() > 0 {
					dataBuf.WriteByte('\n')
				}
				dataBuf.WriteString(strings.TrimPrefix(line, "data: "))
			}
		}
		if scanErr := scanner.Err(); scanErr != nil {
			// context cancellation is expected — don't report it.
			if ctx.Err() == nil {
				errch <- fmt.Errorf("akashi: read SSE stream: %w", scanErr)
			}
		}
	}()

	return ech, errch, cancelFn
}

// ListIntegrityViolations retrieves detected hash mismatches.
// Requires admin role.
func (c *Client) ListIntegrityViolations(ctx context.Context, limit int) (*IntegrityViolationsResponse, error) {
	params := url.Values{}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/integrity/violations"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var resp IntegrityViolationsResponse
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetTraceHealth retrieves aggregate health metrics for the decision pipeline.
// Requires admin role.
func (c *Client) GetTraceHealth(ctx context.Context, opts *TraceHealthOptions) (*TraceHealthResponse, error) {
	params := url.Values{}
	if opts != nil {
		if opts.From != nil {
			params.Set("from", opts.From.Format("2006-01-02T15:04:05Z07:00"))
		}
		if opts.To != nil {
			params.Set("to", opts.To.Format("2006-01-02T15:04:05Z07:00"))
		}
	}
	path := "/v1/trace-health"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var resp TraceHealthResponse
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetUsage retrieves usage metrics for the organization.
// Requires admin role. If period is empty, defaults to the current month.
func (c *Client) GetUsage(ctx context.Context, period string) (*UsageResponse, error) {
	params := url.Values{}
	if period != "" {
		params.Set("period", period)
	}
	path := "/v1/usage"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var resp UsageResponse
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Phase 3: Auth helpers
// ---------------------------------------------------------------------------

// ScopedToken creates a short-lived token scoped to a different agent.
// Requires admin role.
func (c *Client) ScopedToken(ctx context.Context, req ScopedTokenRequest) (*ScopedTokenResponse, error) {
	var resp ScopedTokenResponse
	if err := c.post(ctx, "/v1/auth/scoped-token", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Signup creates a new organization and initial agent. This endpoint does
// not require authentication.
func (c *Client) Signup(ctx context.Context, req SignupRequest) (*SignupResponse, error) {
	var resp SignupResponse
	if err := c.postNoAuth(ctx, "/auth/signup", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetConfig retrieves the server's public configuration (search_enabled, etc.).
// This endpoint does not require authentication.
func (c *Client) GetConfig(ctx context.Context) (*ConfigResponse, error) {
	var resp ConfigResponse
	if err := c.getNoAuth(ctx, "/config", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Phase 4: Agent management (additional endpoints)
// ---------------------------------------------------------------------------

// GetAgent retrieves a single agent by agent_id. Requires admin role.
func (c *Client) GetAgent(ctx context.Context, agentID string) (*Agent, error) {
	var resp Agent
	if err := c.get(ctx, "/v1/agents/"+agentID, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// UpdateAgent updates an agent's name and/or metadata. Requires admin role.
func (c *Client) UpdateAgent(ctx context.Context, agentID string, req UpdateAgentRequest) (*Agent, error) {
	var resp Agent
	if err := c.patch(ctx, "/v1/agents/"+agentID, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetAgentStats retrieves aggregate metrics for a specific agent. Requires admin role.
func (c *Client) GetAgentStats(ctx context.Context, agentID string) (*AgentStatsResponse, error) {
	var resp AgentStatsResponse
	if err := c.get(ctx, "/v1/agents/"+agentID+"/stats", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Phase 4: Grants (list)
// ---------------------------------------------------------------------------

// ListGrants lists all access grants in the organization. Requires admin role.
func (c *Client) ListGrants(ctx context.Context, opts *ListGrantsOptions) (*GrantsResponse, error) {
	params := url.Values{}
	if opts != nil {
		if opts.Limit > 0 {
			params.Set("limit", strconv.Itoa(opts.Limit))
		}
		if opts.Offset > 0 {
			params.Set("offset", strconv.Itoa(opts.Offset))
		}
	}
	path := "/v1/grants"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var items []Grant
	env, err := c.doGetList(ctx, path, &items)
	if err != nil {
		return nil, err
	}
	total := 0
	if env.Total != nil {
		total = *env.Total
	}
	return &GrantsResponse{
		Grants:  items,
		Total:   total,
		HasMore: env.HasMore,
		Limit:   env.Limit,
		Offset:  env.Offset,
	}, nil
}

// ---------------------------------------------------------------------------
// Phase 4: Sessions
// ---------------------------------------------------------------------------

// GetSessionView retrieves all decisions for a session with summary.
func (c *Client) GetSessionView(ctx context.Context, sessionID uuid.UUID) (*SessionViewResponse, error) {
	var resp SessionViewResponse
	if err := c.get(ctx, "/v1/sessions/"+sessionID.String(), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Admin: conflict validation, evaluation, and labels
// ---------------------------------------------------------------------------

// ValidatePair asks the server's conflict validator to classify the
// relationship between two decision outcomes. Requires admin role.
func (c *Client) ValidatePair(ctx context.Context, req ValidatePairRequest) (*ValidatePairResponse, error) {
	var resp ValidatePairResponse
	if err := c.post(ctx, "/v1/admin/conflicts/validate-pair", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ConflictEval runs the conflict evaluation suite against all labeled
// conflicts and returns precision/recall metrics. Requires admin role.
func (c *Client) ConflictEval(ctx context.Context) (*ConflictEvalResponse, error) {
	var resp ConflictEvalResponse
	if err := c.post(ctx, "/v1/admin/conflicts/eval", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// UpsertConflictLabel creates or updates a human label on a scored conflict.
// Requires admin role.
func (c *Client) UpsertConflictLabel(ctx context.Context, conflictID uuid.UUID, req UpsertConflictLabelRequest) (*ConflictLabel, error) {
	var resp ConflictLabel
	if err := c.put(ctx, "/v1/admin/conflicts/"+conflictID.String()+"/label", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetConflictLabel retrieves the human label for a scored conflict.
// Requires admin role.
func (c *Client) GetConflictLabel(ctx context.Context, conflictID uuid.UUID) (*ConflictLabel, error) {
	var resp ConflictLabel
	if err := c.get(ctx, "/v1/admin/conflicts/"+conflictID.String()+"/label", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteConflictLabel removes the human label from a scored conflict.
// Requires admin role.
func (c *Client) DeleteConflictLabel(ctx context.Context, conflictID uuid.UUID) error {
	return c.doDelete(ctx, "/v1/admin/conflicts/"+conflictID.String()+"/label", nil)
}

// ListConflictLabels retrieves all conflict labels with aggregate counts.
// Requires admin role.
func (c *Client) ListConflictLabels(ctx context.Context) (*ListConflictLabelsResponse, error) {
	var resp ListConflictLabelsResponse
	if err := c.get(ctx, "/v1/admin/conflict-labels", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ScorerEval evaluates the conflict scorer's precision using human labels.
// Requires admin role.
func (c *Client) ScorerEval(ctx context.Context) (*ScorerEvalResponse, error) {
	var resp ScorerEvalResponse
	if err := c.post(ctx, "/v1/admin/scorer-eval", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Additional transport helpers needed by new endpoints
// ---------------------------------------------------------------------------

func (c *Client) put(ctx context.Context, path string, body any, dest any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("akashi: marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("akashi: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	return c.doRequest(ctx, req, dest)
}

func (c *Client) postNoAuth(ctx context.Context, path string, body any, dest any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("akashi: marshal request body: %w", err)
	}

	var lastErr error
	for attempt := range c.maxRetries + 1 {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
		if reqErr != nil {
			return fmt.Errorf("akashi: create request: %w", reqErr)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)

		resp, doErr := c.client.Do(req)
		if doErr != nil {
			lastErr = fmt.Errorf("akashi: %s %s: %w", req.Method, req.URL.Path, doErr)
			if attempt < c.maxRetries {
				if waitErr := retrySleep(ctx, attempt, c.retryBaseDelay, 0); waitErr != nil {
					return waitErr
				}
				continue
			}
			return lastErr
		}

		if isRetryableStatus(resp.StatusCode) && attempt < c.maxRetries {
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if waitErr := retrySleep(ctx, attempt, c.retryBaseDelay, retryAfter); waitErr != nil {
				return waitErr
			}
			continue
		}

		defer func() { _ = resp.Body.Close() }()
		return handleResponse(resp, dest)
	}

	return lastErr
}
