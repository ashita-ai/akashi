//go:build integration

package server_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/server"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/service/quality"
	"github.com/ashita-ai/akashi/internal/service/trace"
)

// gateTestServer builds an isolated httptest.Server whose decision service has
// the supplied completeness gate configured. The server reuses the shared
// testDB (so the "admin" agent and its API key seeded by TestMain are visible),
// but uses its own JWT manager and decision service so the gate cannot leak
// into other tests' servers.
func gateTestServer(t *testing.T, gate quality.CompletenessGate) (*httptest.Server, string, string) {
	t.Helper()

	jwtMgr, err := auth.NewJWTManager("", "", 24*time.Hour)
	require.NoError(t, err)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	embedder := embedding.NewNoopProvider(1024)
	decisionSvc := decisions.New(testDB, embedder, nil, logger, nil)
	decisionSvc.SetCompletenessGate(gate)
	buf := trace.NewBuffer(testDB, logger, 1000, 50*time.Millisecond, nil)

	srv := server.New(server.ServerConfig{
		DB:                  testDB,
		JWTMgr:              jwtMgr,
		DecisionSvc:         decisionSvc,
		Buffer:              buf,
		Logger:              logger,
		ReadTimeout:         30 * time.Second,
		WriteTimeout:        30 * time.Second,
		Version:             "test",
		MaxRequestBodyBytes: 1 * 1024 * 1024,
	})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Reuse the admin agent already seeded by TestMain in server_test.go.
	// The DB row exists with the hashed "test-admin-key"; we just need a
	// fresh JWT against this server's JWT manager.
	adminTok := getToken(ts.URL, "admin", "test-admin-key")

	// Create a per-test agent so concurrent test runs don't collide on agent_id.
	agentID := "gate-handler-" + uuid.New().String()[:8]
	agentKey := "gate-handler-key-" + uuid.New().String()[:8]
	createAgent(ts.URL, adminTok, agentID, "Gate Test Agent", "agent", agentKey)
	agentTok := getToken(ts.URL, agentID, agentKey)
	return ts, agentTok, agentID
}

func TestTraceHandler_CompletenessGateReject_Returns422(t *testing.T) {
	ts, agentTok, agentID := gateTestServer(t, quality.CompletenessGate{
		Mode:      quality.GateModeReject,
		Threshold: 0.50, // high enough that a terse trace fails
	})

	resp, err := authedRequest("POST", ts.URL+"/v1/trace", agentTok, model.TraceRequest{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "code_review",
			Outcome:      "approved",
			Confidence:   0.5,
		},
		Context: map[string]any{"project": "test-project"},
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode,
		"reject mode must surface as 422 Unprocessable Entity")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var envelope struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	assert.Equal(t, model.ErrCodeCompletenessBelowThreshold, envelope.Error.Code)
	require.NotNil(t, envelope.Error.Details, "details must surface threshold/score/type for actionable client errors")
	assert.Equal(t, "code_review", envelope.Error.Details["decision_type"])
	// JSON numbers decode to float64. Threshold should match the configured 0.50.
	if v, ok := envelope.Error.Details["required_min"].(float64); assert.True(t, ok, "required_min must be a number") {
		assert.InDelta(t, 0.50, v, 0.001)
	}
}

func TestTraceHandler_CompletenessGateWarn_Returns201WithWarning(t *testing.T) {
	ts, agentTok, agentID := gateTestServer(t, quality.CompletenessGate{
		Mode:      quality.GateModeWarn,
		Threshold: 0.50,
	})

	resp, err := authedRequest("POST", ts.URL+"/v1/trace", agentTok, model.TraceRequest{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "code_review",
			Outcome:      "approved",
			Confidence:   0.5,
		},
		Context: map[string]any{"project": "test-project"},
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusCreated, resp.StatusCode,
		"warn mode must still accept the trace (201 Created)")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var envelope struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
			Warnings   []string  `json:"warnings"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	assert.NotEqual(t, uuid.Nil, envelope.Data.DecisionID, "decision must be persisted in warn mode")
	require.NotEmpty(t, envelope.Data.Warnings, "warn mode must attach a warning to the response")

	// The completeness warning must appear (HighConfidenceWarnings is also possible
	// but with confidence=0.5 it won't fire). Verify the gate's wording reached the client.
	foundGateWarning := false
	for _, w := range envelope.Data.Warnings {
		if strings.HasPrefix(w, "completeness") {
			foundGateWarning = true
			break
		}
	}
	assert.True(t, foundGateWarning, "gate warning must be surfaced via the response Warnings array")
}
