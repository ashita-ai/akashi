package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

func TestHandleScopedToken(t *testing.T) {
	// test-agent (role: agent) and admin are created in TestMain.

	t.Run("admin issues scoped token for test-agent", func(t *testing.T) {
		body := `{"as_agent_id":"test-agent","expires_in":300}`
		req, _ := http.NewRequest("POST", testSrv.URL+"/v1/auth/scoped-token", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		raw, _ := io.ReadAll(resp.Body)
		var envelope struct {
			Data model.ScopedTokenResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(raw, &envelope))

		got := envelope.Data
		assert.NotEmpty(t, got.Token)
		assert.Equal(t, "test-agent", got.AsAgentID)
		assert.Equal(t, "admin", got.ScopedBy)
		assert.False(t, got.ExpiresAt.IsZero())

		// Validate that the scoped token is accepted by an authenticated endpoint.
		// GET /v1/decisions/recent requires readRole — test-agent has role=agent.
		meResp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/recent", got.Token, nil)
		require.NoError(t, err)
		defer func() { _ = meResp.Body.Close() }()
		assert.Equal(t, http.StatusOK, meResp.StatusCode)
	})

	t.Run("scoped token cannot issue another scoped token", func(t *testing.T) {
		// First get a scoped token.
		body := `{"as_agent_id":"test-agent","expires_in":300}`
		req, _ := http.NewRequest("POST", testSrv.URL+"/v1/auth/scoped-token", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		// Test-agent is not admin, so it can't reach /v1/auth/scoped-token.
		// Use the scoped token directly.
		raw, _ := io.ReadAll(resp.Body)
		var envelope struct {
			Data model.ScopedTokenResponse `json:"data"`
		}
		// Re-issue since body was already read.
		req2, _ := http.NewRequest("POST", testSrv.URL+"/v1/auth/scoped-token", strings.NewReader(body))
		req2.Header.Set("Authorization", "Bearer "+adminToken)
		req2.Header.Set("Content-Type", "application/json")
		resp2, err := http.DefaultClient.Do(req2)
		require.NoError(t, err)
		data2, _ := io.ReadAll(resp2.Body)
		_ = resp2.Body.Close()
		_ = json.Unmarshal(data2, &envelope)
		_ = raw

		scopedToken := envelope.Data.Token
		require.NotEmpty(t, scopedToken)

		// Attempt to issue another scoped token using the scoped token.
		// test-agent doesn't have admin role, so it should get 403 from requireRole.
		req3, _ := http.NewRequest("POST", testSrv.URL+"/v1/auth/scoped-token", strings.NewReader(`{"as_agent_id":"test-agent"}`))
		req3.Header.Set("Authorization", "Bearer "+scopedToken)
		req3.Header.Set("Content-Type", "application/json")
		resp3, err := http.DefaultClient.Do(req3)
		require.NoError(t, err)
		defer func() { _ = resp3.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp3.StatusCode)
	})

	t.Run("non-admin cannot call scoped-token endpoint", func(t *testing.T) {
		body := `{"as_agent_id":"admin"}`
		req, _ := http.NewRequest("POST", testSrv.URL+"/v1/auth/scoped-token", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+agentToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("unknown agent_id returns 404", func(t *testing.T) {
		body := `{"as_agent_id":"ghost-agent"}`
		req, _ := http.NewRequest("POST", testSrv.URL+"/v1/auth/scoped-token", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("missing as_agent_id returns 400", func(t *testing.T) {
		body := `{}`
		req, _ := http.NewRequest("POST", testSrv.URL+"/v1/auth/scoped-token", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("unauthenticated request returns 401", func(t *testing.T) {
		body := `{"as_agent_id":"test-agent"}`
		req, _ := http.NewRequest("POST", testSrv.URL+"/v1/auth/scoped-token", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
}
