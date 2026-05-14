//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

// TestPendingAssessment_Endpoint_DefaultsToCallerAgent confirms that the
// HTTP surface's "agents follow up on what they traced" UX works end-to-end:
//   - admin sees nothing under their own agent_id (they didn't trace anything)
//   - an agent sees decisions they traced (when older than the window)
//   - the same agent does NOT see decisions traced by a different agent
//     unless they pass agent_id=*
//
// This is the load-bearing path from issue #716: the prompt loop is only
// useful if it surfaces the caller's own work by default.
func TestPendingAssessment_Endpoint_DefaultsToCallerAgent(t *testing.T) {
	ctx := context.Background()

	// Seed two decisions older than the 7-day architecture window: one by
	// test-agent, one by a fresh agent that no token here owns.
	otherAgentID := "pending-other-" + uuid.New().String()[:8]
	require.NoError(t, seedAgent(ctx, otherAgentID))

	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	myDecision := seedPendingDecision(t, ctx, "test-agent", "architecture", old)
	_ = seedPendingDecision(t, ctx, otherAgentID, "architecture", old)

	// Default scope: only the caller's own decision should appear.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/pending-assessment", agentToken, nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body := decodePendingResponse(t, resp.Body)
	ids := pendingDecisionIDs(body.Decisions)
	assert.Contains(t, ids, myDecision, "caller's decision should appear under default scope")
	for _, p := range body.Decisions {
		assert.Equal(t, "test-agent", p.AgentID,
			"default scope must restrict to caller; saw %s", p.AgentID)
	}
}

// TestPendingAssessment_Endpoint_AssessedAreHidden encodes the "any source
// counts as assessed" rule. A decision with a manual assessment must not
// reappear in the prompt list, even if it would otherwise be eligible.
func TestPendingAssessment_Endpoint_AssessedAreHidden(t *testing.T) {
	ctx := context.Background()
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)

	d := seedPendingDecision(t, ctx, "test-agent", "architecture", old)
	_, err := testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID:      d,
		AssessorAgentID: "test-agent",
		Outcome:         model.AssessmentCorrect,
		Source:          model.AssessmentSourceManual,
	})
	require.NoError(t, err)

	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/pending-assessment", agentToken, nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body := decodePendingResponse(t, resp.Body)
	for _, p := range body.Decisions {
		assert.NotEqual(t, d, p.DecisionID,
			"decision with an existing assessment must not be re-surfaced")
	}
}

// TestPendingAssessment_Endpoint_NormalizesDecisionType verifies the HTTP
// surface matches MCP/trace normalization. Operators and SDKs should not get
// an empty prompt list just because they send display casing or surrounding
// whitespace.
func TestPendingAssessment_Endpoint_NormalizesDecisionType(t *testing.T) {
	ctx := context.Background()
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	d := seedPendingDecision(t, ctx, "test-agent", "architecture", old)

	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/decisions/pending-assessment?decision_type=%20Architecture%20",
		agentToken, nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body := decodePendingResponse(t, resp.Body)
	assert.Contains(t, pendingDecisionIDs(body.Decisions), d,
		"HTTP decision_type filter should normalize case and whitespace")
}

// TestPendingAssessment_Endpoint_OptedOutType verifies decision_type=code_review
// (no configured window in the test harness) returns an empty list rather than
// a 400. An opt-out type is invisible, not an error.
func TestPendingAssessment_Endpoint_OptedOutType(t *testing.T) {
	ctx := context.Background()
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	_ = seedPendingDecision(t, ctx, "test-agent", "code_review", old)

	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/decisions/pending-assessment?decision_type=code_review",
		agentToken, nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body := decodePendingResponse(t, resp.Body)
	assert.Equal(t, 0, body.Count)
	assert.Empty(t, body.Decisions)
}

// TestPendingAssessment_Endpoint_AgentIDStar_AdminSeesAll: an admin caller
// with agent_id=* must see decisions from any agent because LoadGrantedSet
// returns nil (unrestricted) for admin+.
func TestPendingAssessment_Endpoint_AgentIDStar_AdminSeesAll(t *testing.T) {
	ctx := context.Background()
	otherAgentID := "pending-star-" + uuid.New().String()[:8]
	require.NoError(t, seedAgent(ctx, otherAgentID))

	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	d := seedPendingDecision(t, ctx, otherAgentID, "architecture", old)

	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/decisions/pending-assessment?agent_id=*",
		adminToken, nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body := decodePendingResponse(t, resp.Body)
	assert.Contains(t, pendingDecisionIDs(body.Decisions), d,
		"admin with agent_id=* should see decisions from any agent")
}

func seedAgent(ctx context.Context, agentID string) error {
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID,
		Name:    agentID,
		Role:    model.RoleAgent,
	})
	return err
}

func seedPendingDecision(t *testing.T, ctx context.Context, agentID, decisionType string, validFrom time.Time) uuid.UUID {
	t.Helper()
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)
	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: decisionType,
		Outcome:      "pending-fixture",
		Confidence:   0.7,
		ValidFrom:    validFrom,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)
	return d.ID
}

func decodePendingResponse(t *testing.T, r io.Reader) model.PendingAssessmentListResponse {
	t.Helper()
	// HTTP handlers wrap payloads in {data, meta}. Mirror that shape so we
	// decode the inner PendingAssessmentListResponse correctly rather than
	// passing through the envelope's structurally-similar but empty top
	// level.
	var envelope struct {
		Data model.PendingAssessmentListResponse `json:"data"`
	}
	require.NoError(t, json.NewDecoder(r).Decode(&envelope))
	return envelope.Data
}

func pendingDecisionIDs(rows []model.PendingAssessment) []uuid.UUID {
	out := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		out[i] = r.DecisionID
	}
	return out
}
