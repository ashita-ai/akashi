//go:build integration

package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

// ---------------------------------------------------------------------------
// Reader role: cannot create, delete, or list grants
// ---------------------------------------------------------------------------

func TestGrantEndpoints_ReaderCannotCreateGrant(t *testing.T) {
	readerID := fmt.Sprintf("reader-no-create-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, readerID, "Reader No Create", "reader", readerID+"-key")
	readerToken := getToken(testSrv.URL, readerID, readerID+"-key")

	resourceID := "test-agent"
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", readerToken,
		model.CreateGrantRequest{
			GranteeAgentID: "test-agent",
			ResourceType:   "agent_traces",
			ResourceID:     &resourceID,
			Permission:     "read",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"reader role is below writeRole and should not be able to create grants")
}

func TestGrantEndpoints_ReaderCannotDeleteGrant(t *testing.T) {
	readerID := fmt.Sprintf("reader-no-delete-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, readerID, "Reader No Delete", "reader", readerID+"-key")
	readerToken := getToken(testSrv.URL, readerID, readerID+"-key")

	// Create a grant as admin so there is something to try to delete.
	granteeID := fmt.Sprintf("delete-target-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, granteeID, "Delete Target", "reader", granteeID+"-key")

	resourceID := "test-agent"
	createResp, err := authedRequest("POST", testSrv.URL+"/v1/grants", adminToken,
		model.CreateGrantRequest{
			GranteeAgentID: granteeID,
			ResourceType:   "agent_traces",
			ResourceID:     &resourceID,
			Permission:     "read",
		})
	require.NoError(t, err)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(createResp.Body)
	_ = createResp.Body.Close()
	require.NoError(t, json.Unmarshal(body, &created))

	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/grants/"+created.Data.ID, readerToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"reader role is below writeRole and should not be able to delete grants")
}

func TestGrantEndpoints_ReaderCannotListGrants(t *testing.T) {
	readerID := fmt.Sprintf("reader-no-list-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, readerID, "Reader No List", "reader", readerID+"-key")
	readerToken := getToken(testSrv.URL, readerID, readerID+"-key")

	resp, err := authedRequest("GET", testSrv.URL+"/v1/grants", readerToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"listing grants is admin-only; reader should get 403")
}

// ---------------------------------------------------------------------------
// Agent role: cannot list grants (admin-only)
// ---------------------------------------------------------------------------

func TestGrantEndpoints_AgentCannotListGrants(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/grants", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"listing grants is admin-only; agent should get 403")
}

// ---------------------------------------------------------------------------
// Agent role: cannot delete another agent's grant
// ---------------------------------------------------------------------------

func TestGrantEndpoints_AgentCannotDeleteOthersGrant(t *testing.T) {
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	// Create a second agent that will be the grantor.
	grantor := "other-grantor-" + suffix
	createAgent(testSrv.URL, adminToken, grantor, "Other Grantor", "agent", grantor+"-key")
	grantorToken := getToken(testSrv.URL, grantor, grantor+"-key")

	// Grantee.
	grantee := "other-grantee-" + suffix
	createAgent(testSrv.URL, adminToken, grantee, "Other Grantee", "reader", grantee+"-key")

	// Grantor creates a grant on their own traces.
	resourceID := grantor
	createResp, err := authedRequest("POST", testSrv.URL+"/v1/grants", grantorToken,
		model.CreateGrantRequest{
			GranteeAgentID: grantee,
			ResourceType:   "agent_traces",
			ResourceID:     &resourceID,
			Permission:     "read",
		})
	require.NoError(t, err)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(createResp.Body)
	_ = createResp.Body.Close()
	require.Equal(t, http.StatusCreated, createResp.StatusCode, "grantor should create grant: %s", string(body))
	require.NoError(t, json.Unmarshal(body, &created))

	// test-agent (agentToken) tries to delete the other agent's grant.
	delResp, err := authedRequest("DELETE", testSrv.URL+"/v1/grants/"+created.Data.ID, agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = delResp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, delResp.StatusCode,
		"agent should not be able to delete another agent's grant")
}

// ---------------------------------------------------------------------------
// Admin can delete any agent's grant
// ---------------------------------------------------------------------------

func TestGrantEndpoints_AdminCanDeleteOthersGrant(t *testing.T) {
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	grantee := "admin-del-grantee-" + suffix
	createAgent(testSrv.URL, adminToken, grantee, "Admin Del Grantee", "reader", grantee+"-key")

	resourceID := "test-agent"
	createResp, err := authedRequest("POST", testSrv.URL+"/v1/grants", agentToken,
		model.CreateGrantRequest{
			GranteeAgentID: grantee,
			ResourceType:   "agent_traces",
			ResourceID:     &resourceID,
			Permission:     "read",
		})
	require.NoError(t, err)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(createResp.Body)
	_ = createResp.Body.Close()
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	require.NoError(t, json.Unmarshal(body, &created))

	// Admin deletes the agent's grant.
	delResp, err := authedRequest("DELETE", testSrv.URL+"/v1/grants/"+created.Data.ID, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = delResp.Body.Close() }()
	assert.Equal(t, http.StatusNoContent, delResp.StatusCode,
		"admin should be able to delete any grant")
}

// ---------------------------------------------------------------------------
// Full grant lifecycle: create → verify access → revoke → verify access lost
// ---------------------------------------------------------------------------

func TestGrantLifecycle_CreateVerifyRevokeVerify(t *testing.T) {
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	readerID := "lifecycle-reader-" + suffix
	createAgent(testSrv.URL, adminToken, readerID, "Lifecycle Reader", "reader", readerID+"-key")
	readerToken := getToken(testSrv.URL, readerID, readerID+"-key")

	// Seed a decision from test-agent so there is something to query.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: "lifecycle_test_" + suffix,
				Outcome:      "test outcome",
				Confidence:   0.8,
			},
		})
	require.NoError(t, err)
	_ = traceResp.Body.Close()

	// Step 1: Reader cannot see test-agent's history.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent/history", readerToken, nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"reader should not see history before grant")

	// Step 2: Admin creates a grant.
	resourceID := "test-agent"
	createResp, err := authedRequest("POST", testSrv.URL+"/v1/grants", adminToken,
		model.CreateGrantRequest{
			GranteeAgentID: readerID,
			ResourceType:   "agent_traces",
			ResourceID:     &resourceID,
			Permission:     "read",
		})
	require.NoError(t, err)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(createResp.Body)
	_ = createResp.Body.Close()
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	require.NoError(t, json.Unmarshal(body, &created))

	// Step 3: Reader can now see test-agent's history.
	resp, err = authedRequest("GET", testSrv.URL+"/v1/agents/test-agent/history", readerToken, nil)
	require.NoError(t, err)
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"reader should see history after grant, got: %s", string(respBody))

	// Step 4: Admin revokes the grant.
	delResp, err := authedRequest("DELETE", testSrv.URL+"/v1/grants/"+created.Data.ID, adminToken, nil)
	require.NoError(t, err)
	_ = delResp.Body.Close()
	require.Equal(t, http.StatusNoContent, delResp.StatusCode)

	// Step 5: Reader can no longer see test-agent's history.
	resp, err = authedRequest("GET", testSrv.URL+"/v1/agents/test-agent/history", readerToken, nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"reader should not see history after grant revocation")
}

// ---------------------------------------------------------------------------
// Query filtering: reader only sees decisions from granted agents
// ---------------------------------------------------------------------------

func TestGrantEnforcement_QueryFiltering(t *testing.T) {
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	// Create two agents that will produce decisions.
	visibleAgent := "visible-agent-" + suffix
	hiddenAgent := "hidden-agent-" + suffix
	createAgent(testSrv.URL, adminToken, visibleAgent, "Visible", "agent", visibleAgent+"-key")
	createAgent(testSrv.URL, adminToken, hiddenAgent, "Hidden", "agent", hiddenAgent+"-key")
	visibleToken := getToken(testSrv.URL, visibleAgent, visibleAgent+"-key")
	hiddenToken := getToken(testSrv.URL, hiddenAgent, hiddenAgent+"-key")

	// Each agent traces a decision.
	decType := "query_filter_" + suffix
	for _, pair := range []struct {
		token, agentID string
	}{
		{visibleToken, visibleAgent},
		{hiddenToken, hiddenAgent},
	} {
		traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", pair.token,
			model.TraceRequest{
				AgentID: pair.agentID,
				Decision: model.TraceDecision{
					DecisionType: decType,
					Outcome:      "test",
					Confidence:   0.7,
				},
			})
		require.NoError(t, err)
		_ = traceResp.Body.Close()
	}

	// Create a reader with a grant only to visibleAgent.
	readerID := "query-reader-" + suffix
	createAgent(testSrv.URL, adminToken, readerID, "Query Reader", "reader", readerID+"-key")
	readerToken := getToken(testSrv.URL, readerID, readerID+"-key")

	resourceID := visibleAgent
	grantResp, err := authedRequest("POST", testSrv.URL+"/v1/grants", adminToken,
		model.CreateGrantRequest{
			GranteeAgentID: readerID,
			ResourceType:   "agent_traces",
			ResourceID:     &resourceID,
			Permission:     "read",
		})
	require.NoError(t, err)
	_ = grantResp.Body.Close()
	require.Equal(t, http.StatusCreated, grantResp.StatusCode)

	// Reader queries for both agents' decisions by type.
	queryResp, err := authedRequest("POST", testSrv.URL+"/v1/query", readerToken,
		model.QueryRequest{
			Filters: model.QueryFilters{
				AgentIDs:     []string{visibleAgent, hiddenAgent},
				DecisionType: &decType,
			},
			Limit: 50,
		})
	require.NoError(t, err)
	defer func() { _ = queryResp.Body.Close() }()
	assert.Equal(t, http.StatusOK, queryResp.StatusCode)

	var result struct {
		Data []model.Decision `json:"data"`
	}
	data, _ := io.ReadAll(queryResp.Body)
	require.NoError(t, json.Unmarshal(data, &result))

	// Reader should only see visibleAgent's decision (and own, but reader didn't trace).
	for _, d := range result.Data {
		assert.NotEqual(t, hiddenAgent, d.AgentID,
			"reader should not see decisions from ungrantee agent %s", hiddenAgent)
	}
}

// ---------------------------------------------------------------------------
// Agent cannot grant access to another agent's traces (only own)
// ---------------------------------------------------------------------------

func TestGrantEndpoints_AgentCannotGrantOtherAgentsTracesResourceID(t *testing.T) {
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	grantee := "cant-grant-other-" + suffix
	createAgent(testSrv.URL, adminToken, grantee, "Cant Grant Other", "reader", grantee+"-key")

	// agentToken belongs to "test-agent". Try to grant access to "admin" traces.
	otherAgent := "admin"
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", agentToken,
		model.CreateGrantRequest{
			GranteeAgentID: grantee,
			ResourceType:   "agent_traces",
			ResourceID:     &otherAgent,
			Permission:     "read",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"agent should not be able to grant access to another agent's traces")
}

func TestGrantEndpoints_AgentCannotGrantWithNilResourceID(t *testing.T) {
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	grantee := "nil-resource-" + suffix
	createAgent(testSrv.URL, adminToken, grantee, "Nil Resource", "reader", grantee+"-key")

	// No resource_id = wildcard, which non-admins shouldn't be able to create.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", agentToken,
		model.CreateGrantRequest{
			GranteeAgentID: grantee,
			ResourceType:   "agent_traces",
			Permission:     "read",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"agent should not be able to create wildcard grants (nil resource_id)")
}

// ---------------------------------------------------------------------------
// Admin can create grants for any agent's traces
// ---------------------------------------------------------------------------

func TestGrantEndpoints_AdminCanGrantAnyAgentsTraces(t *testing.T) {
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	grantee := "admin-grant-any-" + suffix
	createAgent(testSrv.URL, adminToken, grantee, "Admin Grant Any", "reader", grantee+"-key")

	// Admin grants access to test-agent's traces (admin is not test-agent).
	resourceID := "test-agent"
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", adminToken,
		model.CreateGrantRequest{
			GranteeAgentID: grantee,
			ResourceType:   "agent_traces",
			ResourceID:     &resourceID,
			Permission:     "read",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode,
		"admin should be able to grant access to any agent's traces")
}

// ---------------------------------------------------------------------------
// Unauthenticated requests: 401
// ---------------------------------------------------------------------------

func TestGrantEndpoints_UnauthenticatedDenied(t *testing.T) {
	tests := []struct {
		method string
		path   string
	}{
		{"GET", "/v1/grants"},
		{"POST", "/v1/grants"},
		{"DELETE", "/v1/grants/00000000-0000-0000-0000-000000000000"},
	}

	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, testSrv.URL+tc.path, nil)
			require.NoError(t, err)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
				"unauthenticated request should get 401")
		})
	}
}
