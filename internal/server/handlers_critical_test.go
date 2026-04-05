//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/ratelimit"
	"github.com/ashita-ai/akashi/internal/server"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/service/trace"
)

// ---------------------------------------------------------------------------
// Helper: build a second server with different config for isolated tests.
// ---------------------------------------------------------------------------

// criticalTestServer creates a test server with customizable config. The
// returned server shares testDB (and therefore the same Postgres), so seeded
// data from TestMain is visible. The caller can override specific config fields
// via the opts callback.
func criticalTestServer(t *testing.T, opts func(*server.ServerConfig)) *httptest.Server {
	t.Helper()
	jwtMgr, err := auth.NewJWTManager("", "", 24*time.Hour)
	require.NoError(t, err)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	embedder := embedding.NewNoopProvider(1024)
	decisionSvc := decisions.New(testDB, embedder, nil, logger, nil)
	buf := trace.NewBuffer(testDB, logger, 1000, 50*time.Millisecond, nil)

	cfg := server.ServerConfig{
		DB:                      testDB,
		JWTMgr:                  jwtMgr,
		DecisionSvc:             decisionSvc,
		Buffer:                  buf,
		Logger:                  logger,
		ReadTimeout:             30 * time.Second,
		WriteTimeout:            30 * time.Second,
		Version:                 "test",
		MaxRequestBodyBytes:     1 * 1024 * 1024,
		EnableDestructiveDelete: true,
	}
	if opts != nil {
		opts(&cfg)
	}

	srv := server.New(cfg)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// traceDecisionCritical traces a decision and returns its ID.
func traceDecisionCritical(t *testing.T, baseURL, token, agentID, decisionType, outcome string) uuid.UUID {
	t.Helper()
	resp, err := authedRequest("POST", baseURL+"/v1/trace", token, model.TraceRequest{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: decisionType,
			Outcome:      outcome,
			Confidence:   0.6,
		},
		Context: map[string]any{"project": "test-project"},
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var result struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	require.NotEqual(t, uuid.Nil, result.Data.DecisionID)
	return result.Data.DecisionID
}

// signupAndGetTokenCritical creates a new org via signup and returns the admin token.
func signupAndGetTokenCritical(t *testing.T, baseURL, orgName, agentID, email string) string {
	t.Helper()
	body, _ := json.Marshal(model.SignupRequest{
		OrgName: orgName,
		AgentID: agentID,
		Email:   email,
	})
	resp, err := http.Post(baseURL+"/auth/signup", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	data, _ := io.ReadAll(resp.Body)
	var result struct {
		Data model.SignupResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(data, &result))
	return getToken(baseURL, agentID, result.Data.APIKey)
}

// ===========================================================================
// 1. Retention/Purge — legal holds prevent deletion
// ===========================================================================

func TestHandlersCritical_PurgeRespectsLegalHolds(t *testing.T) {
	uniqueType := "purge-hold-test-" + uuid.NewString()[:8]
	agentIDForHold := "purge-hold-agent-" + uuid.NewString()[:8]

	// Create a dedicated agent for this test.
	createAgent(testSrv.URL, adminToken, agentIDForHold, "Purge Hold Agent", "agent", "purge-hold-key-"+uuid.NewString()[:8])

	// Trace a decision for this agent (uses admin token to trace on behalf).
	decisionID := traceDecisionCritical(t, testSrv.URL, adminToken, agentIDForHold, uniqueType, "should survive purge")

	// Verify the decision exists.
	getResp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+decisionID.String(), adminToken, nil)
	require.NoError(t, err)
	_ = getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode)

	// Create a legal hold covering this agent from the past to the future.
	holdResp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken, map[string]any{
		"reason":    "critical test hold",
		"from":      time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
		"to":        time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		"agent_ids": []string{agentIDForHold},
	})
	require.NoError(t, err)
	defer func() { _ = holdResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, holdResp.StatusCode)

	// Purge with a cutoff in the future — without holds, this would delete everything.
	purgeResp, err := authedRequest("POST", testSrv.URL+"/v1/retention/purge", adminToken, map[string]any{
		"before":  time.Now().Add(time.Hour).Format(time.RFC3339),
		"dry_run": false,
	})
	require.NoError(t, err)
	defer func() { _ = purgeResp.Body.Close() }()
	require.Equal(t, http.StatusOK, purgeResp.StatusCode)

	// The held decision must survive the purge.
	verifyResp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+decisionID.String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = verifyResp.Body.Close() }()
	assert.Equal(t, http.StatusOK, verifyResp.StatusCode, "decision covered by legal hold must survive purge")
}

// ===========================================================================
// 2. Purge creates deletion_log entries
// ===========================================================================

func TestHandlersCritical_PurgeCreatesDeletionLog(t *testing.T) {
	ctx := context.Background()

	// Count deletion_log rows before the purge.
	var beforeCount int
	err := testDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM deletion_log WHERE org_id = $1 AND trigger = 'manual'`,
		uuid.Nil, // testSrv's default org
	).Scan(&beforeCount)
	require.NoError(t, err)

	// Execute a real (non-dry-run) purge with an old cutoff that won't actually delete data.
	purgeResp, err := authedRequest("POST", testSrv.URL+"/v1/retention/purge", adminToken, map[string]any{
		"before":  time.Now().Add(-100 * 365 * 24 * time.Hour).Format(time.RFC3339),
		"dry_run": false,
	})
	require.NoError(t, err)
	defer func() { _ = purgeResp.Body.Close() }()
	require.Equal(t, http.StatusOK, purgeResp.StatusCode)

	// Verify a new deletion_log row was created.
	var afterCount int
	err = testDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM deletion_log WHERE org_id = $1 AND trigger = 'manual'`,
		uuid.Nil,
	).Scan(&afterCount)
	require.NoError(t, err)
	assert.Greater(t, afterCount, beforeCount, "purge should create a deletion_log entry")
}

// ===========================================================================
// 3. Purge dry_run returns correct count structure
// ===========================================================================

func TestHandlersCritical_PurgeDryRunReturnsCounts(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/purge", adminToken, map[string]any{
		"before":  time.Now().Add(time.Hour).Format(time.RFC3339),
		"dry_run": true,
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			DryRun      bool `json:"dry_run"`
			WouldDelete struct {
				Decisions    int `json:"decisions"`
				Alternatives int `json:"alternatives"`
				Evidence     int `json:"evidence"`
				Claims       int `json:"claims"`
				Events       int `json:"events"`
			} `json:"would_delete"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.True(t, result.Data.DryRun)
	assert.GreaterOrEqual(t, result.Data.WouldDelete.Decisions, 0,
		"would_delete.decisions must be non-negative")
}

// ===========================================================================
// 4. API Key — CreateKey scoped to caller's org
// ===========================================================================

func TestHandlersCritical_CreateKeyForOwnOrgAgent(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken,
		model.CreateKeyRequest{AgentID: "test-agent", Label: "critical-test-key"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	var result struct {
		Data struct {
			RawKey string `json:"raw_key"`
			OrgID  string `json:"org_id"`
			Prefix string `json:"prefix"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.NotEmpty(t, result.Data.RawKey, "raw key must be returned exactly once")
	assert.Contains(t, result.Data.RawKey, "ak_", "key should use managed key format")
}

// ===========================================================================
// 5. API Key — CreateKey rejects agents not in caller's org
// ===========================================================================

func TestHandlersCritical_CreateKeyRejectsNonexistentAgent(t *testing.T) {
	// Requesting a key for an agent_id that doesn't exist in the caller's org
	// is functionally equivalent to cross-org rejection: the handler verifies the
	// agent exists in the caller's org via GetAgentByAgentID(orgID, agentID).
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken,
		model.CreateKeyRequest{AgentID: "agent-from-another-dimension", Label: "should-fail"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "agent not found")
}

// ===========================================================================
// 6. API Key — RevokeKey returns 204
// ===========================================================================

func TestHandlersCritical_RevokeKeyReturns204(t *testing.T) {
	revokeAgentID := "revoke-test-agent-" + uuid.NewString()[:8]
	createAgent(testSrv.URL, adminToken, revokeAgentID, "Revoke Test", "agent", "revoke-agent-key-"+uuid.NewString()[:8])

	// Create a key for this agent.
	createResp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken,
		model.CreateKeyRequest{AgentID: revokeAgentID, Label: "to-revoke"})
	require.NoError(t, err)
	defer func() { _ = createResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, createResp.StatusCode)

	var created struct {
		Data struct {
			ID uuid.UUID `json:"id"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(createResp.Body)
	require.NoError(t, json.Unmarshal(body, &created))
	require.NotEqual(t, uuid.Nil, created.Data.ID)

	// Revoke it.
	revokeResp, err := authedRequest("DELETE", testSrv.URL+"/v1/keys/"+created.Data.ID.String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = revokeResp.Body.Close() }()
	assert.Equal(t, http.StatusNoContent, revokeResp.StatusCode)
}

// ===========================================================================
// 7. API Key — RotateKey atomically revokes old and creates new
// ===========================================================================

func TestHandlersCritical_RotateKeyOldKeyStopsWorking(t *testing.T) {
	rotAgentID := "rotate-critical-" + uuid.NewString()[:8]
	originalKey := "rot-crit-key-" + uuid.NewString()[:8]
	createAgent(testSrv.URL, adminToken, rotAgentID, "Rotate Critical", "agent", originalKey)

	// Get a token with the original key to prove it works.
	origToken := getToken(testSrv.URL, rotAgentID, originalKey)
	require.NotEmpty(t, origToken)

	// Find the key ID.
	listResp, err := authedRequest("GET", testSrv.URL+"/v1/keys?limit=200", adminToken, nil)
	require.NoError(t, err)
	var keysResult struct {
		Data []struct {
			ID      uuid.UUID `json:"id"`
			AgentID string    `json:"agent_id"`
		} `json:"data"`
	}
	listBody, _ := io.ReadAll(listResp.Body)
	_ = listResp.Body.Close()
	require.NoError(t, json.Unmarshal(listBody, &keysResult))

	var keyID uuid.UUID
	for _, k := range keysResult.Data {
		if k.AgentID == rotAgentID {
			keyID = k.ID
			break
		}
	}
	require.NotEqual(t, uuid.Nil, keyID, "must find key for agent")

	// Rotate the key.
	rotResp, err := authedRequest("POST", testSrv.URL+"/v1/keys/"+keyID.String()+"/rotate", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = rotResp.Body.Close() }()
	require.Equal(t, http.StatusOK, rotResp.StatusCode)

	var rotResult struct {
		Data struct {
			RevokedKeyID uuid.UUID `json:"revoked_key_id"`
			NewKey       struct {
				RawKey string `json:"raw_key"`
			} `json:"new_key"`
		} `json:"data"`
	}
	rotBody, _ := io.ReadAll(rotResp.Body)
	require.NoError(t, json.Unmarshal(rotBody, &rotResult))
	assert.Equal(t, keyID, rotResult.Data.RevokedKeyID, "response must report the revoked key ID")
	assert.NotEmpty(t, rotResult.Data.NewKey.RawKey, "new raw key must be returned")

	// The old key should no longer authenticate.
	tokenBody, _ := json.Marshal(model.AuthTokenRequest{AgentID: rotAgentID, APIKey: originalKey})
	tokenResp, err := http.Post(testSrv.URL+"/auth/token", "application/json", bytes.NewReader(tokenBody))
	require.NoError(t, err)
	defer func() { _ = tokenResp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, tokenResp.StatusCode,
		"old key must be rejected after rotation")

	// The new key should work.
	newToken := getToken(testSrv.URL, rotAgentID, rotResult.Data.NewKey.RawKey)
	assert.NotEmpty(t, newToken, "new key must produce a valid token")
}

// ===========================================================================
// 8. Agent CRUD — cannot create agent with equal or higher role
// ===========================================================================

func TestHandlersCritical_CreateAgentRoleEscalation(t *testing.T) {
	t.Run("admin cannot create admin", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken,
			model.CreateAgentRequest{
				AgentID: "escalation-test-admin-" + uuid.NewString()[:8],
				Name:    "Should Fail",
				Role:    model.RoleAdmin,
			})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		body, _ := io.ReadAll(resp.Body)
		assert.Contains(t, string(body), "equal to or higher")
	})

	t.Run("admin cannot create org_owner", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken,
			model.CreateAgentRequest{
				AgentID: "escalation-test-owner-" + uuid.NewString()[:8],
				Name:    "Should Fail",
				Role:    model.RoleOrgOwner,
			})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("admin can create agent role", func(t *testing.T) {
		agentID := "escalation-test-ok-" + uuid.NewString()[:8]
		resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken,
			model.CreateAgentRequest{
				AgentID: agentID,
				Name:    "Should Succeed",
				Role:    model.RoleAgent,
			})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusCreated, resp.StatusCode)
	})

	t.Run("agent role cannot create agents at all", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", agentToken,
			model.CreateAgentRequest{
				AgentID: "should-never-exist-" + uuid.NewString()[:8],
				Name:    "Agent Creating Agent",
				Role:    model.RoleReader,
			})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode,
			"agent role lacks admin access to POST /v1/agents")
	})
}

// ===========================================================================
// 9. DeleteAgent — requires EnableDestructiveDelete
// ===========================================================================

func TestHandlersCritical_DeleteAgentDisabledByConfig(t *testing.T) {
	ts := criticalTestServer(t, func(cfg *server.ServerConfig) {
		cfg.EnableDestructiveDelete = false
		cfg.SignupEnabled = true
		cfg.SignupRateLimiter = ratelimit.NewMemoryLimiter(100, 100)
	})

	// Signup creates a fresh org+admin on this server (same DB).
	token := signupAndGetTokenCritical(t, ts.URL,
		"NoDelete Org "+uuid.NewString()[:4],
		"nodelete-admin",
		"nodelete-"+uuid.NewString()[:6]+"@test.com")

	// Try to delete an agent — should be blocked by config.
	resp, err := authedRequest("DELETE", ts.URL+"/v1/agents/nodelete-admin", token, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "destructive delete is disabled")
}

// ===========================================================================
// 10. DeleteAgent — creates deletion_log entries
// ===========================================================================

func TestHandlersCritical_DeleteAgentCreatesDeletionLog(t *testing.T) {
	ctx := context.Background()

	delAgentID := "del-log-agent-" + uuid.NewString()[:8]
	createAgent(testSrv.URL, adminToken, delAgentID, "Deletion Log Agent", "agent", "del-log-key-"+uuid.NewString()[:8])

	// Trace something so there's data to delete.
	traceDecisionCritical(t, testSrv.URL, adminToken, delAgentID, "del-log-test", "will be deleted")

	// Count deletion_log rows before.
	var beforeCount int
	err := testDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM deletion_log WHERE org_id = $1 AND trigger = 'gdpr'`,
		uuid.Nil,
	).Scan(&beforeCount)
	require.NoError(t, err)

	// Delete the agent.
	delResp, err := authedRequest("DELETE", testSrv.URL+"/v1/agents/"+delAgentID, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = delResp.Body.Close() }()
	require.Equal(t, http.StatusOK, delResp.StatusCode)

	var delResult struct {
		Data struct {
			AgentID string `json:"agent_id"`
			Deleted struct {
				Decisions int `json:"decisions"`
			} `json:"deleted"`
		} `json:"data"`
	}
	delBody, _ := io.ReadAll(delResp.Body)
	require.NoError(t, json.Unmarshal(delBody, &delResult))
	assert.Equal(t, delAgentID, delResult.Data.AgentID)

	// Verify deletion_log was written.
	var afterCount int
	err = testDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM deletion_log WHERE org_id = $1 AND trigger = 'gdpr'`,
		uuid.Nil,
	).Scan(&afterCount)
	require.NoError(t, err)
	assert.Greater(t, afterCount, beforeCount, "agent deletion must create a GDPR deletion_log entry")
}

// ===========================================================================
// 11. DeleteAgent — archives to deletion_audit_log
// ===========================================================================

func TestHandlersCritical_DeleteAgentCreatesAuditLog(t *testing.T) {
	ctx := context.Background()

	auditAgentID := "audit-del-agent-" + uuid.NewString()[:8]
	createAgent(testSrv.URL, adminToken, auditAgentID, "Audit Del Agent", "agent", "audit-del-key-"+uuid.NewString()[:8])
	traceDecisionCritical(t, testSrv.URL, adminToken, auditAgentID, "audit-del-test", "auditable deletion")

	// Count deletion_audit_log rows before deletion.
	var beforeCount int
	err := testDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM deletion_audit_log WHERE org_id = $1 AND agent_id = $2`,
		uuid.Nil, auditAgentID,
	).Scan(&beforeCount)
	require.NoError(t, err)

	delResp, err := authedRequest("DELETE", testSrv.URL+"/v1/agents/"+auditAgentID, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = delResp.Body.Close() }()
	require.Equal(t, http.StatusOK, delResp.StatusCode)

	// Verify rows were archived.
	var afterCount int
	err = testDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM deletion_audit_log WHERE org_id = $1 AND agent_id = $2`,
		uuid.Nil, auditAgentID,
	).Scan(&afterCount)
	require.NoError(t, err)
	assert.Greater(t, afterCount, beforeCount,
		"agent deletion must archive records to deletion_audit_log")
}

// ===========================================================================
// 12. GetDecision — cross-org isolation
// ===========================================================================

func TestHandlersCritical_GetDecisionCrossOrgReturns404(t *testing.T) {
	// Use the signup-enabled server so we can create two independent orgs
	// that share the same JWT issuer.
	ts := criticalTestServer(t, func(cfg *server.ServerConfig) {
		cfg.SignupEnabled = true
		cfg.SignupRateLimiter = ratelimit.NewMemoryLimiter(100, 100)
	})

	// Create org A and trace a decision.
	orgAToken := signupAndGetTokenCritical(t, ts.URL,
		"OrgA "+uuid.NewString()[:4],
		"org-a-agent",
		"org-a-"+uuid.NewString()[:6]+"@test.com")
	decisionID := traceDecisionCritical(t, ts.URL, orgAToken, "org-a-agent", "cross-org-test", "should be invisible")

	// Create org B.
	orgBToken := signupAndGetTokenCritical(t, ts.URL,
		"OrgB "+uuid.NewString()[:4],
		"org-b-agent",
		"org-b-"+uuid.NewString()[:6]+"@test.com")

	// Org B should NOT see org A's decision.
	resp, err := authedRequest("GET", ts.URL+"/v1/decisions/"+decisionID.String(), orgBToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// Could be 404 (not found in their org) or 403 (no access to that agent).
	// Either way, data must NOT leak.
	assert.True(t, resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden,
		"cross-org access must return 404 or 403, got %d", resp.StatusCode)
}

// ===========================================================================
// 13. PatchDecision — creates mutation audit entry
// ===========================================================================

func TestHandlersCritical_PatchDecisionCreatesAuditEntry(t *testing.T) {
	ctx := context.Background()

	// Trace a decision to patch.
	decisionID := traceDecisionCritical(t, testSrv.URL, adminToken, "test-agent", "patch-audit-test", "will be patched")

	// Count mutation_audit_log rows for this decision before the patch.
	var beforeCount int
	err := testDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM mutation_audit_log WHERE org_id = $1 AND resource_id = $2 AND operation = 'decision_project_updated'`,
		uuid.Nil, decisionID.String(),
	).Scan(&beforeCount)
	require.NoError(t, err)

	// Patch the decision's project.
	patchResp, err := authedRequest("PATCH", testSrv.URL+"/v1/decisions/"+decisionID.String(), adminToken,
		map[string]string{"project": "audited-project"})
	require.NoError(t, err)
	defer func() { _ = patchResp.Body.Close() }()
	require.Equal(t, http.StatusOK, patchResp.StatusCode)

	// Verify the audit log entry was created.
	var afterCount int
	err = testDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM mutation_audit_log WHERE org_id = $1 AND resource_id = $2 AND operation = 'decision_project_updated'`,
		uuid.Nil, decisionID.String(),
	).Scan(&afterCount)
	require.NoError(t, err)
	assert.Greater(t, afterCount, beforeCount,
		"PATCH /v1/decisions/{id} must create a mutation_audit_log entry")
}

// ===========================================================================
// 14. PatchDecision — adminOnly enforcement (agent role forbidden)
// ===========================================================================

func TestHandlersCritical_PatchDecisionForbiddenForAgent(t *testing.T) {
	decisionID := traceDecisionCritical(t, testSrv.URL, adminToken, "test-agent", "patch-authz-test", "will try to patch")

	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/decisions/"+decisionID.String(), agentToken,
		map[string]string{"project": "nope"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"agent role must not be able to PATCH decisions (admin-only route)")
}

// ===========================================================================
// 15. Retention hold validation
// ===========================================================================

func TestHandlersCritical_CreateHoldValidation(t *testing.T) {
	t.Run("missing reason returns 400", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken, map[string]any{
			"from": time.Now().Add(-time.Hour).Format(time.RFC3339),
			"to":   time.Now().Add(time.Hour).Format(time.RFC3339),
		})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("to before from returns 400", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken, map[string]any{
			"reason": "bad dates",
			"from":   time.Now().Add(time.Hour).Format(time.RFC3339),
			"to":     time.Now().Add(-time.Hour).Format(time.RFC3339),
		})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("agent role forbidden", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", agentToken, map[string]any{
			"reason": "agent trying to create hold",
			"from":   time.Now().Add(-time.Hour).Format(time.RFC3339),
			"to":     time.Now().Add(time.Hour).Format(time.RFC3339),
		})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

// ===========================================================================
// 16. Release hold — happy path and not found
// ===========================================================================

func TestHandlersCritical_ReleaseHold(t *testing.T) {
	// Create a hold, then release it.
	holdResp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken, map[string]any{
		"reason": "to be released",
		"from":   time.Now().Add(-time.Hour).Format(time.RFC3339),
		"to":     time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	require.NoError(t, err)
	defer func() { _ = holdResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, holdResp.StatusCode)

	var holdResult struct {
		Data struct {
			ID uuid.UUID `json:"id"`
		} `json:"data"`
	}
	holdBody, _ := io.ReadAll(holdResp.Body)
	require.NoError(t, json.Unmarshal(holdBody, &holdResult))

	t.Run("release existing hold returns 204", func(t *testing.T) {
		resp, err := authedRequest("DELETE", testSrv.URL+"/v1/retention/hold/"+holdResult.Data.ID.String(), adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("re-release returns 404", func(t *testing.T) {
		resp, err := authedRequest("DELETE", testSrv.URL+"/v1/retention/hold/"+holdResult.Data.ID.String(), adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("nonexistent hold returns 404", func(t *testing.T) {
		resp, err := authedRequest("DELETE", testSrv.URL+"/v1/retention/hold/"+uuid.New().String(), adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

// ===========================================================================
// 17. CreateKey — agent role forbidden (adminOnly middleware)
// ===========================================================================

func TestHandlersCritical_CreateKeyForbiddenForAgentRole(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", agentToken,
		model.CreateKeyRequest{AgentID: "test-agent", Label: "agent-created"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"agent role must not access adminOnly key management endpoint")
}

// ===========================================================================
// 18. SetRetention validation
// ===========================================================================

func TestHandlersCritical_SetRetentionValidation(t *testing.T) {
	t.Run("retention_days < 1 returns 400", func(t *testing.T) {
		resp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken, map[string]any{
			"retention_days": 0,
		})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("valid retention_days succeeds", func(t *testing.T) {
		resp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken, map[string]any{
			"retention_days": 90,
		})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data struct {
				RetentionDays int `json:"retention_days"`
			} `json:"data"`
		}
		body, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(body, &result))
		assert.Equal(t, 90, result.Data.RetentionDays)

		// Reset to nil so downstream tests (e.g. TestHandleGetRetention_Default)
		// that assert on a clean default org are not contaminated.
		t.Cleanup(func() {
			r, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken, map[string]any{})
			if err == nil {
				_ = r.Body.Close()
			}
		})
	})

	t.Run("agent role forbidden", func(t *testing.T) {
		resp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", agentToken, map[string]any{
			"retention_days": 30,
		})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}
