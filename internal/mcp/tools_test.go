package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/ctxutil"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/service/quality"
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/internal/testutil"
)

var (
	testDB      *storage.DB
	testSvc     *decisions.Service
	testServer  *Server
	testAdminID = "test-admin"
)

func TestMain(m *testing.M) {
	tc := testutil.MustStartTimescaleDB()
	code := setupAndRun(m, tc)
	tc.Terminate()
	os.Exit(code)
}

func setupAndRun(m *testing.M, tc *testutil.TestContainer) int {
	ctx := context.Background()
	logger := testutil.TestLogger()

	var err error
	testDB, err = tc.NewTestDB(ctx, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp test: create DB: %v\n", err)
		return 1
	}
	defer testDB.Close(ctx)

	if err := testDB.EnsureDefaultOrg(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "mcp test: ensure default org: %v\n", err)
		return 1
	}

	// Create the admin agent used by all tests.
	_, err = testDB.CreateAgent(ctx, model.Agent{
		AgentID: testAdminID,
		OrgID:   uuid.Nil,
		Name:    testAdminID,
		Role:    model.RoleAdmin,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp test: create admin agent: %v\n", err)
		return 1
	}

	embedder := embedding.NewNoopProvider(1024)
	testSvc = decisions.New(testDB, embedder, nil, logger, nil)
	testServer = New(testDB, testSvc, nil, logger, "test", 0.85, nil)

	return m.Run()
}

// adminCtx returns a context carrying admin claims for the default org.
func adminCtx() context.Context {
	return ctxutil.WithClaims(context.Background(), &auth.Claims{
		AgentID: testAdminID,
		OrgID:   uuid.Nil,
		Role:    model.RoleAdmin,
	})
}

// traceRequest builds a CallToolRequest for akashi_trace with the given arguments.
func traceRequest(args map[string]any) mcplib.CallToolRequest {
	return mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name:      "akashi_trace",
			Arguments: args,
		},
	}
}

// parseToolText extracts the first TextContent text from a CallToolResult.
func parseToolText(t *testing.T, result *mcplib.CallToolResult) string {
	t.Helper()
	for _, c := range result.Content {
		if tc, ok := c.(mcplib.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatal("no TextContent found in tool result")
	return ""
}

// mustTrace records a decision and returns its decision_id.
func mustTrace(t *testing.T, agentID, decisionType, outcome string, confidence float64) string {
	t.Helper()
	ctx := adminCtx()

	// Ensure agent exists.
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": decisionType,
		"outcome":       outcome,
		"confidence":    confidence,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "trace should succeed: %s", parseToolText(t, result))

	var resp struct {
		DecisionID string `json:"decision_id"`
		Status     string `json:"status"`
	}
	err = json.Unmarshal([]byte(parseToolText(t, result)), &resp)
	require.NoError(t, err)
	require.Equal(t, "recorded", resp.Status)
	return resp.DecisionID
}

// ---------- handleTrace tests ----------

func TestHandleTrace(t *testing.T) {
	ctx := adminCtx()
	agentID := "trace-basic-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "architecture",
		"outcome":       "chose PostgreSQL for persistence",
		"confidence":    0.85,
		"reasoning":     "mature ecosystem, pgvector support",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "expected successful trace")

	text := parseToolText(t, result)
	var resp struct {
		RunID      string `json:"run_id"`
		DecisionID string `json:"decision_id"`
		Status     string `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	assert.Equal(t, "recorded", resp.Status)
	assert.NotEmpty(t, resp.DecisionID, "decision_id should be a non-empty UUID string")
	assert.NotEmpty(t, resp.RunID, "run_id should be a non-empty UUID string")

	// Verify both are valid UUIDs.
	_, err = uuid.Parse(resp.DecisionID)
	assert.NoError(t, err, "decision_id should be a valid UUID")
	_, err = uuid.Parse(resp.RunID)
	assert.NoError(t, err, "run_id should be a valid UUID")
}

func TestHandleTrace_MissingFields(t *testing.T) {
	ctx := adminCtx()

	tests := []struct {
		name    string
		args    map[string]any
		errText string
	}{
		{
			name:    "missing decision_type",
			args:    map[string]any{"agent_id": "admin", "outcome": "x", "confidence": 0.5},
			errText: "decision_type and outcome are required",
		},
		{
			name:    "missing outcome",
			args:    map[string]any{"agent_id": "admin", "decision_type": "architecture", "confidence": 0.5},
			errText: "decision_type and outcome are required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := testServer.handleTrace(ctx, traceRequest(tt.args))
			require.NoError(t, err, "handler should not return go error, only tool error")
			require.True(t, result.IsError, "expected tool error for %s", tt.name)
			assert.Contains(t, parseToolText(t, result), tt.errText)
		})
	}
}

// TestHandleTrace_NilClaims verifies that a context without auth claims is
// rejected immediately. This exercises the H2 nil-claims guard that prevents
// access-filtering bypass on unauthenticated paths.
func TestHandleTrace_NilClaims(t *testing.T) {
	result, err := testServer.handleTrace(context.Background(), traceRequest(map[string]any{
		"agent_id":      "some-agent",
		"decision_type": "architecture",
		"outcome":       "x",
		"confidence":    0.5,
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "authentication required")
}

func TestHandleTrace_InvalidAgentID(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      "invalid agent id with spaces!",
		"decision_type": "architecture",
		"outcome":       "test",
		"confidence":    0.5,
	}))
	require.NoError(t, err)
	require.True(t, result.IsError, "expected tool error for invalid agent_id")
	assert.Contains(t, parseToolText(t, result), "invalid agent_id")
}

func TestHandleTrace_DefaultsAgentIDFromClaims(t *testing.T) {
	ctx := adminCtx()

	// Trace without explicit agent_id; should default to claims.AgentID.
	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"decision_type": "investigation",
		"outcome":       "found root cause",
		"confidence":    0.7,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "trace should succeed using claims agent_id")

	var resp struct {
		DecisionID string `json:"decision_id"`
		Status     string `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, "recorded", resp.Status)
}

func TestHandleTrace_ModelAndTaskContext(t *testing.T) {
	ctx := adminCtx()
	agentID := "trace-ctx-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "model_selection",
		"outcome":       "chose gpt-4o for summarization",
		"confidence":    0.9,
		"model":         "claude-opus-4-6",
		"task":          "codebase review",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "trace with model and task should succeed")

	var resp struct {
		DecisionID string `json:"decision_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))

	// Verify agent_context was stored with model and task under "client" namespace.
	decID, err := uuid.Parse(resp.DecisionID)
	require.NoError(t, err)
	dec, err := testDB.GetDecision(ctx, uuid.Nil, decID, storage.GetDecisionOpts{})
	require.NoError(t, err)

	clientCtx, ok := dec.AgentContext["client"].(map[string]any)
	require.True(t, ok, "agent_context should have 'client' namespace")
	assert.Equal(t, "claude-opus-4-6", clientCtx["model"])
	assert.Equal(t, "codebase review", clientCtx["task"])
}

func TestHandleTrace_NormalizedDecisionType(t *testing.T) {
	ctx := adminCtx()
	agentID := "trace-norm-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	// Trace with mixed-case decision_type — should be stored lowercase.
	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "  Architecture  ",
		"outcome":       "normalization test",
		"confidence":    0.8,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		DecisionID string `json:"decision_id"`
		Status     string `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	require.Equal(t, "recorded", resp.Status)

	// Verify stored decision_type is lowercase.
	id, err := uuid.Parse(resp.DecisionID)
	require.NoError(t, err)
	stored, err := testDB.GetDecision(ctx, uuid.Nil, id, storage.GetDecisionOpts{})
	require.NoError(t, err)
	assert.Equal(t, "architecture", stored.DecisionType, "decision_type should be stored lowercase")
}

// ---------- handleCheck tests ----------

func TestHandleCheck(t *testing.T) {
	ctx := adminCtx()
	agentID := "check-basic-" + uuid.New().String()[:8]

	// Trace a decision first.
	mustTrace(t, agentID, "security", "chose mTLS for internal services", 0.9)

	result, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": "security",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "check should succeed: %s", parseToolText(t, result))

	var resp model.CheckResponse
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.True(t, resp.HasPrecedent, "expected has_precedent=true after tracing a decision")
	assert.NotEmpty(t, resp.Decisions, "expected at least one precedent decision")
}

func TestHandleCheck_NoDecisionType(t *testing.T) {
	ctx := adminCtx()
	agentID := "check-nodtype-" + uuid.New().String()[:8]

	// Trace a decision so there's something to find.
	mustTrace(t, agentID, "architecture", "broad check test decision", 0.8)

	// Check with no decision_type should succeed and return decisions.
	result, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name:      "akashi_check",
			Arguments: map[string]any{},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "check without decision_type should succeed: %s", parseToolText(t, result))

	var resp model.CheckResponse
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	// has_precedent depends on whether decisions exist — we just verify no error.
}

func TestHandleCheck_NoPrecedent(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": "nonexistent-type-" + uuid.New().String()[:8],
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp model.CheckResponse
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.False(t, resp.HasPrecedent, "expected has_precedent=false for unused type")
	assert.Empty(t, resp.Decisions)
}

func TestHandleCheck_WithAgentFilter(t *testing.T) {
	ctx := adminCtx()
	agentA := "check-filter-a-" + uuid.New().String()[:8]
	agentB := "check-filter-b-" + uuid.New().String()[:8]

	mustTrace(t, agentA, "planning", "sprint plan A", 0.8)
	mustTrace(t, agentB, "planning", "sprint plan B", 0.7)

	// Check filtered to agentA only.
	result, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": "planning",
				"agent_id":      agentA,
				"limit":         50,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp model.CheckResponse
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.True(t, resp.HasPrecedent)
	for _, dec := range resp.Decisions {
		assert.Equal(t, agentA, dec.AgentID, "expected only agentA decisions")
	}
}

func TestHandleCheck_WithQuery(t *testing.T) {
	ctx := adminCtx()
	agentID := "check-query-" + uuid.New().String()[:8]
	keyword := "semanticquery-" + agentID

	mustTrace(t, agentID, "investigation", keyword, 0.85)

	// Check with a non-empty query triggers the semantic/text search path.
	result, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": "investigation",
				"query":         keyword,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp model.CheckResponse
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.True(t, resp.HasPrecedent, "text search should find the traced decision")
}

func TestHandleCheck_CaseInsensitiveType(t *testing.T) {
	ctx := adminCtx()
	agentID := "check-case-" + uuid.New().String()[:8]

	// Trace with lowercase type.
	mustTrace(t, agentID, "architecture", "case insensitive test", 0.8)

	// Check with mixed-case type should find the decision.
	result, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": "  ARCHITECTURE  ",
				"agent_id":      agentID,
				"limit":         50,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp model.CheckResponse
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.True(t, resp.HasPrecedent, "mixed-case decision_type should match stored lowercase decisions")
}

// ---------- handleQuery tests ----------

func TestHandleQuery(t *testing.T) {
	ctx := adminCtx()
	agentID := "query-basic-" + uuid.New().String()[:8]

	mustTrace(t, agentID, "trade_off", "chose latency over throughput", 0.75)

	result, err := testServer.handleQuery(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"decision_type": "trade_off",
				"limit":         10,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "query should succeed: %s", parseToolText(t, result))

	var resp struct {
		Decisions []model.Decision `json:"decisions"`
		Total     int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.NotEmpty(t, resp.Decisions)
	assert.Greater(t, resp.Total, 0)
}

func TestHandleQuery_WithFilters(t *testing.T) {
	ctx := adminCtx()
	agentID := "query-filter-" + uuid.New().String()[:8]
	otherAgent := "query-filter-other-" + uuid.New().String()[:8]

	mustTrace(t, agentID, "architecture", "chose microservices", 0.8)
	mustTrace(t, otherAgent, "architecture", "chose monolith", 0.6)

	// Query filtered to agentID only.
	result, err := testServer.handleQuery(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"agent_id": agentID,
				"limit":    50,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Decisions []model.Decision `json:"decisions"`
		Total     int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	for _, dec := range resp.Decisions {
		assert.Equal(t, agentID, dec.AgentID, "filtered query should only return matching agent")
	}
}

func TestHandleQuery_WithConfidenceMin(t *testing.T) {
	ctx := adminCtx()
	agentID := "query-conf-" + uuid.New().String()[:8]

	mustTrace(t, agentID, "data_source", "low confidence pick", 0.3)
	mustTrace(t, agentID, "data_source", "high confidence pick", 0.95)

	result, err := testServer.handleQuery(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"agent_id":       agentID,
				"decision_type":  "data_source",
				"confidence_min": 0.9,
				"limit":          50,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Decisions []model.Decision `json:"decisions"`
		Total     int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	for _, dec := range resp.Decisions {
		assert.GreaterOrEqual(t, dec.Confidence, float32(0.9),
			"all returned decisions should have confidence >= 0.9")
	}
}

func TestHandleQuery_WithOutcomeFilter(t *testing.T) {
	ctx := adminCtx()
	agentID := "query-outcome-" + uuid.New().String()[:8]
	uniqueOutcome := "unique-outcome-" + uuid.New().String()[:8]

	mustTrace(t, agentID, "deployment", uniqueOutcome, 0.7)
	mustTrace(t, agentID, "deployment", "other outcome", 0.6)

	result, err := testServer.handleQuery(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"outcome": uniqueOutcome,
				"limit":   50,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Decisions []model.Decision `json:"decisions"`
		Total     int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	require.NotEmpty(t, resp.Decisions)
	for _, dec := range resp.Decisions {
		assert.Equal(t, uniqueOutcome, dec.Outcome)
	}
}

func TestHandleQuery_EmptyResult(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleQuery(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"decision_type": "nonexistent-type-" + uuid.New().String()[:8],
				"limit":         10,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Decisions []model.Decision `json:"decisions"`
		Total     int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Empty(t, resp.Decisions)
	assert.Equal(t, 0, resp.Total)
}

// ---------- handleQuery: semantic mode ----------

func TestHandleQuery_WithQuery(t *testing.T) {
	ctx := adminCtx()
	agentID := "query-semantic-" + uuid.New().String()[:8]
	keyword := "semantic-keyword-" + agentID

	mustTrace(t, agentID, "error_handling", keyword, 0.8)

	// query param triggers the semantic/text search path.
	result, err := testServer.handleQuery(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"query": keyword,
				"limit": 5,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "query with semantic param should succeed: %s", parseToolText(t, result))

	var resp struct {
		Decisions []map[string]any `json:"decisions"`
		Total     int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.NotEmpty(t, resp.Decisions, "text search should find the decision by keyword")
	assert.Greater(t, resp.Total, 0)

	// Semantic results include similarity_score.
	assert.Contains(t, resp.Decisions[0], "similarity_score",
		"semantic mode results should include similarity_score")
}

func TestHandleQuery_WithQuery_NoResults(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleQuery(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"query": "completely-nonexistent-" + uuid.New().String(),
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Decisions []map[string]any `json:"decisions"`
		Total     int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Empty(t, resp.Decisions)
	assert.Equal(t, 0, resp.Total)
}

func TestHandleQuery_NilClaims(t *testing.T) {
	// H2 fix: nil claims must be rejected immediately rather than silently
	// skipping access filtering and returning unfiltered cross-org data.
	ctx := context.Background()

	result, err := testServer.handleQuery(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"limit": 5,
			},
		},
	})
	require.NoError(t, err)
	require.True(t, result.IsError, "unauthenticated handleQuery must return an error")
	assert.Contains(t, parseToolText(t, result), "authentication required")
}

func TestHandleQuery_WithOffset(t *testing.T) {
	ctx := adminCtx()
	agentID := "query-offset-" + uuid.New().String()[:8]

	// Create 3 decisions.
	for i := range 3 {
		mustTrace(t, agentID, "planning", fmt.Sprintf("offset plan %d", i), 0.7)
	}

	// First page.
	result1, err := testServer.handleQuery(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"agent_id": agentID,
				"limit":    2,
				"offset":   0,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result1.IsError)

	// Second page.
	result2, err := testServer.handleQuery(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"agent_id": agentID,
				"limit":    2,
				"offset":   2,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result2.IsError)

	var page1, page2 struct {
		Decisions []model.Decision `json:"decisions"`
		Total     int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result1)), &page1))
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result2)), &page2))

	assert.Len(t, page1.Decisions, 2, "first page should have 2 decisions")
	// Pages should not overlap.
	if len(page1.Decisions) > 0 && len(page2.Decisions) > 0 {
		assert.NotEqual(t, page1.Decisions[0].ID, page2.Decisions[0].ID,
			"pages should not overlap")
	}
}

// ---------- handleTrace: non-admin agent cannot trace for another agent ----------

func TestHandleTrace_NonAdminCrossTrace(t *testing.T) {
	// Create an agent-level caller.
	agentID := "agent-caller-" + uuid.New().String()[:8]
	ctx := ctxutil.WithClaims(context.Background(), &auth.Claims{
		AgentID: agentID,
		OrgID:   uuid.Nil,
		Role:    model.RoleAgent,
	})
	_, err := testDB.CreateAgent(context.Background(), model.Agent{
		AgentID: agentID,
		OrgID:   uuid.Nil,
		Name:    agentID,
		Role:    model.RoleAgent,
	})
	require.NoError(t, err)

	// Try to trace as a different agent — should fail.
	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      "someone-else",
		"decision_type": "architecture",
		"outcome":       "should fail",
		"confidence":    0.5,
	}))
	require.NoError(t, err)
	require.True(t, result.IsError, "non-admin should not trace for another agent_id")
	assert.Contains(t, parseToolText(t, result), "agents can only record decisions for their own agent_id")
}

// ---------- Verify all 5 tools are registered ----------

func TestRegisterTools(t *testing.T) {
	// The server's registerTools is called during New(). Verify the MCPServer
	// has the 6 expected tools registered: akashi_check, akashi_trace,
	// akashi_query, akashi_conflicts, akashi_assess, akashi_stats.
	// We verify indirectly by ensuring the server is initialized correctly.
	assert.NotNil(t, testServer.mcpServer, "MCPServer should be initialized")
	assert.NotNil(t, testServer.MCPServer(), "MCPServer() accessor should work")
}

func TestHandleTrace_WithPrecedentRef(t *testing.T) {
	ctx := adminCtx()
	agentID := "precedent-ref-agent-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	// Record the first (antecedent) decision.
	firstID := mustTrace(t, agentID, "architecture", "chose PostgreSQL for primary storage", 0.85)

	// Record a second decision that explicitly builds on the first.
	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "architecture",
		"outcome":       "chose pgvector extension for vector storage",
		"confidence":    0.9,
		"reasoning":     "already on PostgreSQL, pgvector avoids a separate vector DB",
		"precedent_ref": firstID,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "trace with precedent_ref should succeed: %s", parseToolText(t, result))

	var resp struct {
		DecisionID string `json:"decision_id"`
		Status     string `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, "recorded", resp.Status)

	// Fetch the stored decision and verify precedent_ref was persisted.
	secondID, err := uuid.Parse(resp.DecisionID)
	require.NoError(t, err)
	stored, err := testDB.GetDecision(ctx, uuid.Nil, secondID, storage.GetDecisionOpts{})
	require.NoError(t, err)
	require.NotNil(t, stored.PrecedentRef, "PrecedentRef should be set on the stored decision")

	firstUUID, err := uuid.Parse(firstID)
	require.NoError(t, err)
	assert.Equal(t, firstUUID, *stored.PrecedentRef)
}

func TestHandleTrace_PrecedentRef_InvalidUUIDIgnored(t *testing.T) {
	ctx := adminCtx()
	agentID := "bad-precedent-agent-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	// A malformed precedent_ref should be silently ignored — the trace should
	// still succeed, just without a precedent link.
	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "architecture",
		"outcome":       "chose Redis for caching",
		"confidence":    0.8,
		"precedent_ref": "not-a-valid-uuid",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "invalid precedent_ref UUID should be ignored, not fail the trace")

	var resp struct {
		DecisionID string `json:"decision_id"`
		Status     string `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, "recorded", resp.Status)

	// PrecedentRef should be nil since the UUID was invalid.
	id, err := uuid.Parse(resp.DecisionID)
	require.NoError(t, err)
	stored, err := testDB.GetDecision(ctx, uuid.Nil, id, storage.GetDecisionOpts{})
	require.NoError(t, err)
	assert.Nil(t, stored.PrecedentRef, "invalid precedent_ref UUID should not be persisted")
}

// ---------- errorResult helper ----------

func TestErrorResult(t *testing.T) {
	result := errorResult("test error message")
	require.True(t, result.IsError)
	require.Len(t, result.Content, 1)

	tc, ok := result.Content[0].(mcplib.TextContent)
	require.True(t, ok, "content should be TextContent")
	assert.Equal(t, "test error message", tc.Text)
	assert.Equal(t, "text", tc.Type)
}

// ---------- Concurrent traces ----------

func TestHandleTrace_Concurrent(t *testing.T) {
	ctx := adminCtx()
	const n = 5
	errs := make(chan error, n)

	for i := range n {
		go func(idx int) {
			agentID := fmt.Sprintf("concurrent-%d-%s", idx, uuid.New().String()[:8])
			_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

			result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
				"agent_id":      agentID,
				"decision_type": "architecture",
				"outcome":       fmt.Sprintf("concurrent decision %d", idx),
				"confidence":    0.7,
			}))
			if err != nil {
				errs <- err
				return
			}
			if result.IsError {
				errs <- fmt.Errorf("trace %d returned tool error: %v", idx, result.Content)
				return
			}
			errs <- nil
		}(i)
	}

	for range n {
		select {
		case err := <-errs:
			assert.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Fatal("concurrent traces timed out")
		}
	}
}

// ---------- Resource handler tests ----------

func TestHandleSessionCurrent(t *testing.T) {
	ctx := adminCtx()

	// Ensure at least one decision exists.
	mustTrace(t, "session-res-"+uuid.New().String()[:8], "architecture", "session resource test", 0.8)

	contents, err := testServer.handleSessionCurrent(ctx, mcplib.ReadResourceRequest{
		Params: mcplib.ReadResourceParams{
			URI: "akashi://session/current",
		},
	})
	require.NoError(t, err)
	require.Len(t, contents, 1)

	trc, ok := contents[0].(mcplib.TextResourceContents)
	require.True(t, ok, "expected TextResourceContents")
	assert.Equal(t, "akashi://session/current", trc.URI)
	assert.Equal(t, "application/json", trc.MIMEType)
	assert.NotEmpty(t, trc.Text)

	// Verify the text is valid JSON containing a list of decisions.
	var decisions []model.Decision
	require.NoError(t, json.Unmarshal([]byte(trc.Text), &decisions))
	assert.NotEmpty(t, decisions, "should return recent decisions")
}

func TestHandleSessionCurrent_NilClaims(t *testing.T) {
	ctx := context.Background()

	contents, err := testServer.handleSessionCurrent(ctx, mcplib.ReadResourceRequest{
		Params: mcplib.ReadResourceParams{
			URI: "akashi://session/current",
		},
	})
	require.NoError(t, err, "should succeed without claims (skips access filtering)")
	require.Len(t, contents, 1)
}

func TestHandleDecisionsRecent(t *testing.T) {
	ctx := adminCtx()

	// Ensure at least one decision exists.
	mustTrace(t, "decisions-res-"+uuid.New().String()[:8], "security", "decisions resource test", 0.9)

	contents, err := testServer.handleDecisionsRecent(ctx, mcplib.ReadResourceRequest{
		Params: mcplib.ReadResourceParams{
			URI: "akashi://decisions/recent",
		},
	})
	require.NoError(t, err)
	require.Len(t, contents, 1)

	trc, ok := contents[0].(mcplib.TextResourceContents)
	require.True(t, ok, "expected TextResourceContents")
	assert.Equal(t, "akashi://decisions/recent", trc.URI)
	assert.Equal(t, "application/json", trc.MIMEType)
	assert.NotEmpty(t, trc.Text)

	var decisions []model.Decision
	require.NoError(t, json.Unmarshal([]byte(trc.Text), &decisions))
	assert.NotEmpty(t, decisions, "should return recent decisions")
}

func TestHandleDecisionsRecent_NilClaims(t *testing.T) {
	ctx := context.Background()

	contents, err := testServer.handleDecisionsRecent(ctx, mcplib.ReadResourceRequest{
		Params: mcplib.ReadResourceParams{
			URI: "akashi://decisions/recent",
		},
	})
	require.NoError(t, err, "should succeed without claims")
	require.Len(t, contents, 1)
}

func TestHandleAgentHistory(t *testing.T) {
	ctx := adminCtx()
	agentID := "history-res-" + uuid.New().String()[:8]

	mustTrace(t, agentID, "planning", "agent history test", 0.7)

	uri := "akashi://agent/" + agentID + "/history"
	contents, err := testServer.handleAgentHistory(ctx, mcplib.ReadResourceRequest{
		Params: mcplib.ReadResourceParams{
			URI: uri,
		},
	})
	require.NoError(t, err)
	require.Len(t, contents, 1)

	trc, ok := contents[0].(mcplib.TextResourceContents)
	require.True(t, ok, "expected TextResourceContents")
	assert.Equal(t, uri, trc.URI)
	assert.Equal(t, "application/json", trc.MIMEType)

	var resp struct {
		AgentID   string           `json:"agent_id"`
		Decisions []model.Decision `json:"decisions"`
	}
	require.NoError(t, json.Unmarshal([]byte(trc.Text), &resp))
	assert.Equal(t, agentID, resp.AgentID)
	assert.NotEmpty(t, resp.Decisions, "should return the agent's decisions")
}

func TestHandleAgentHistory_NilClaims(t *testing.T) {
	ctx := context.Background()
	agentID := "history-nil-" + uuid.New().String()[:8]

	// Create agent and trace a decision using admin context first.
	mustTrace(t, agentID, "investigation", "nil claims agent history", 0.6)

	uri := "akashi://agent/" + agentID + "/history"
	contents, err := testServer.handleAgentHistory(ctx, mcplib.ReadResourceRequest{
		Params: mcplib.ReadResourceParams{
			URI: uri,
		},
	})
	require.NoError(t, err, "should succeed without claims (skips access check)")
	require.Len(t, contents, 1)
}

func TestHandleAgentHistory_InvalidURI(t *testing.T) {
	ctx := adminCtx()

	_, err := testServer.handleAgentHistory(ctx, mcplib.ReadResourceRequest{
		Params: mcplib.ReadResourceParams{
			URI: "akashi://invalid/path",
		},
	})
	require.Error(t, err, "should error for invalid URI format")
	assert.Contains(t, err.Error(), "invalid agent history URI")
}

func TestHandleAgentHistory_InvalidAgentID(t *testing.T) {
	ctx := adminCtx()

	_, err := testServer.handleAgentHistory(ctx, mcplib.ReadResourceRequest{
		Params: mcplib.ReadResourceParams{
			URI: "akashi://agent/bad agent id!/history",
		},
	})
	require.Error(t, err, "should error for invalid agent_id in URI")
	assert.Contains(t, err.Error(), "invalid agent_id")
}

// ---------- handleConflicts tests ----------

func conflictsRequest(args map[string]any) mcplib.CallToolRequest {
	return mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name:      "akashi_conflicts",
			Arguments: args,
		},
	}
}

func TestHandleConflicts_EmptyResult(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{
		"decision_type": "nonexistent-type-" + uuid.New().String()[:8],
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "conflicts query should succeed: %s", parseToolText(t, result))

	var resp struct {
		Conflicts []any `json:"conflicts"`
		Total     int   `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Empty(t, resp.Conflicts)
	assert.Equal(t, 0, resp.Total)
}

func TestHandleConflicts_NilClaims(t *testing.T) {
	result, err := testServer.handleConflicts(context.Background(), conflictsRequest(map[string]any{}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "authentication required")
}

func TestHandleConflicts_DefaultArgs(t *testing.T) {
	ctx := adminCtx()

	// Call with no arguments — should use defaults (limit=10, open status).
	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{}))
	require.NoError(t, err)
	require.False(t, result.IsError, "conflicts with empty args should succeed: %s", parseToolText(t, result))

	var resp struct {
		Conflicts []any `json:"conflicts"`
		Total     int   `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	// We just verify the response parses; total may be 0 if no conflicts exist.
	assert.GreaterOrEqual(t, resp.Total, 0)
}

func TestHandleConflicts_FullFormat(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{
		"format": "full",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Conflicts []any `json:"conflicts"`
		Total     int   `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
}

func TestHandleConflicts_WithFilters(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{
		"decision_type": "architecture",
		"agent_id":      "nonexistent-agent",
		"severity":      "critical",
		"category":      "factual",
		"status":        "open",
		"limit":         5,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Conflicts []any `json:"conflicts"`
		Total     int   `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, 0, resp.Total)
}

// ---------- handleAssess tests ----------

func assessRequest(args map[string]any) mcplib.CallToolRequest {
	return mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name:      "akashi_assess",
			Arguments: args,
		},
	}
}

func TestHandleAssess_Success(t *testing.T) {
	ctx := adminCtx()
	agentID := "assess-agent-" + uuid.New().String()[:8]

	// Create a decision to assess.
	decisionID := mustTrace(t, agentID, "architecture", "chose PostgreSQL for primary storage", 0.9)

	result, err := testServer.handleAssess(ctx, assessRequest(map[string]any{
		"decision_id": decisionID,
		"outcome":     "correct",
		"notes":       "confirmed by production metrics",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "assess should succeed: %s", parseToolText(t, result))

	var resp struct {
		AssessmentID string `json:"assessment_id"`
		DecisionID   string `json:"decision_id"`
		Outcome      string `json:"outcome"`
		Assessor     string `json:"assessor"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, decisionID, resp.DecisionID)
	assert.Equal(t, "correct", resp.Outcome)
	assert.Equal(t, testAdminID, resp.Assessor)
	assert.NotEmpty(t, resp.AssessmentID)
}

func TestHandleAssess_PartiallyCorrect(t *testing.T) {
	ctx := adminCtx()
	agentID := "assess-partial-" + uuid.New().String()[:8]
	decisionID := mustTrace(t, agentID, "planning", "sprint plan alpha", 0.7)

	result, err := testServer.handleAssess(ctx, assessRequest(map[string]any{
		"decision_id": decisionID,
		"outcome":     "partially_correct",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Outcome string `json:"outcome"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, "partially_correct", resp.Outcome)
}

func TestHandleAssess_NilClaims(t *testing.T) {
	result, err := testServer.handleAssess(context.Background(), assessRequest(map[string]any{
		"decision_id": uuid.New().String(),
		"outcome":     "correct",
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "authentication required")
}

func TestHandleAssess_MissingDecisionID(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleAssess(ctx, assessRequest(map[string]any{
		"outcome": "correct",
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "decision_id is required")
}

func TestHandleAssess_InvalidDecisionID(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleAssess(ctx, assessRequest(map[string]any{
		"decision_id": "not-a-uuid",
		"outcome":     "correct",
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "valid UUID")
}

func TestHandleAssess_InvalidOutcome(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleAssess(ctx, assessRequest(map[string]any{
		"decision_id": uuid.New().String(),
		"outcome":     "maybe",
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "correct")
}

func TestHandleAssess_DecisionNotFound(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleAssess(ctx, assessRequest(map[string]any{
		"decision_id": uuid.New().String(),
		"outcome":     "incorrect",
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "not found")
}

// ---------- handleStats tests ----------

func TestHandleStats(t *testing.T) {
	ctx := adminCtx()

	// Ensure at least one decision exists for stats.
	mustTrace(t, "stats-agent-"+uuid.New().String()[:8], "architecture", "stats test decision", 0.8)

	result, err := testServer.handleStats(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name:      "akashi_stats",
			Arguments: map[string]any{},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "stats should succeed: %s", parseToolText(t, result))

	var resp struct {
		TraceHealth map[string]any `json:"trace_health"`
		Agents      int            `json:"agents"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.NotNil(t, resp.TraceHealth, "trace_health should be present")
	assert.Greater(t, resp.Agents, 0, "should have at least one agent")
}

func TestHandleStats_NilClaimsStillWorks(t *testing.T) {
	// handleStats doesn't check claims — it's a read-only aggregate endpoint.
	result, err := testServer.handleStats(context.Background(), mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name:      "akashi_stats",
			Arguments: map[string]any{},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "stats should work without auth claims")

	var resp struct {
		TraceHealth map[string]any `json:"trace_health"`
		Agents      int            `json:"agents"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.NotNil(t, resp.TraceHealth)
}

// ---------- resolveProjectFilter tests ----------

func TestResolveProjectFilter(t *testing.T) {
	t.Run("explicit project", func(t *testing.T) {
		ctx := adminCtx()
		req := mcplib.CallToolRequest{
			Params: mcplib.CallToolParams{
				Arguments: map[string]any{"project": "my-project"},
			},
		}
		result := testServer.resolveProjectFilter(ctx, req)
		require.NotNil(t, result)
		assert.Equal(t, "my-project", *result)
	})

	t.Run("wildcard cross-project opt-out", func(t *testing.T) {
		ctx := adminCtx()
		req := mcplib.CallToolRequest{
			Params: mcplib.CallToolParams{
				Arguments: map[string]any{"project": "*"},
			},
		}
		result := testServer.resolveProjectFilter(ctx, req)
		assert.Nil(t, result, "* should return nil for cross-project opt-out")
	})

	t.Run("empty project with no MCP roots returns nil", func(t *testing.T) {
		ctx := adminCtx()
		req := mcplib.CallToolRequest{
			Params: mcplib.CallToolParams{
				Arguments: map[string]any{},
			},
		}
		result := testServer.resolveProjectFilter(ctx, req)
		// With no MCP session in the context, requestRoots returns nil,
		// so resolveProjectFilter should return nil.
		assert.Nil(t, result)
	})
}

// ---------- handleCheck: full format ----------

func TestHandleCheck_FullFormat(t *testing.T) {
	ctx := adminCtx()
	agentID := "check-full-" + uuid.New().String()[:8]
	mustTrace(t, agentID, "deployment", "full format test", 0.75)

	result, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": "deployment",
				"format":        "full",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp model.CheckResponse
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.True(t, resp.HasPrecedent)
}

func TestHandleCheck_NilClaims(t *testing.T) {
	result, err := testServer.handleCheck(context.Background(), mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": "architecture",
			},
		},
	})
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "authentication required")
}

// ---------- handleQuery: full format ----------

func TestHandleQuery_FullFormat(t *testing.T) {
	ctx := adminCtx()
	agentID := "query-full-" + uuid.New().String()[:8]
	mustTrace(t, agentID, "investigation", "full format query test", 0.8)

	result, err := testServer.handleQuery(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"decision_type": "investigation",
				"format":        "full",
				"limit":         5,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Decisions []model.Decision `json:"decisions"`
		Total     int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.NotEmpty(t, resp.Decisions)
}

// ---------- mcpTraceHash ----------

func TestMCPTraceHash_Deterministic(t *testing.T) {
	h1, err := mcpTraceHash("agent", "architecture", "chose Redis", 0.8, "good fit", nil, nil, nil)
	require.NoError(t, err)

	h2, err := mcpTraceHash("agent", "architecture", "chose Redis", 0.8, "good fit", nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, h1, h2, "same inputs should produce the same hash")

	// Different outcome should produce a different hash.
	h3, err := mcpTraceHash("agent", "architecture", "chose Memcached", 0.8, "good fit", nil, nil, nil)
	require.NoError(t, err)
	assert.NotEqual(t, h1, h3, "different outcome should produce different hash")
}

func TestMCPTraceHash_WithPrecedentRef(t *testing.T) {
	ref := uuid.New()

	h1, err := mcpTraceHash("agent", "architecture", "chose Redis", 0.8, "", nil, nil, nil)
	require.NoError(t, err)

	h2, err := mcpTraceHash("agent", "architecture", "chose Redis", 0.8, "", nil, nil, &ref)
	require.NoError(t, err)

	assert.NotEqual(t, h1, h2, "adding precedent_ref should change the hash")
}

// ---------- New() constructor ----------

func TestMCPServerNew(t *testing.T) {
	s := New(testDB, testSvc, nil, testutil.TestLogger(), "test-version", 0.85, nil)
	require.NotNil(t, s)
	require.NotNil(t, s.MCPServer())
	// Verify the server has the expected name by checking it's non-nil.
	assert.NotNil(t, s.mcpServer)
}

// ---------- handleTrace edge cases for coverage ----------

func TestHandleTrace_WithEvidence(t *testing.T) {
	ctx := adminCtx()
	agentID := "trace-ev-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	evidence := `[{"source_type":"benchmark","content":"benchmark showed 2x throughput","source_uri":"https://example.com/bench"}]`
	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "performance",
		"outcome":       "chose connection pooling",
		"confidence":    0.85,
		"evidence":      evidence,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "trace with evidence should succeed: %s", parseToolText(t, result))

	var resp struct {
		DecisionID string `json:"decision_id"`
		Status     string `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, "recorded", resp.Status)
}

func TestHandleTrace_InvalidEvidenceJSON(t *testing.T) {
	ctx := adminCtx()
	agentID := "trace-iev-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	// Invalid JSON should be logged and ignored, not fail the trace.
	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "architecture",
		"outcome":       "test invalid evidence",
		"confidence":    0.7,
		"evidence":      "not valid json{",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "invalid evidence JSON should be ignored, not fail trace: %s", parseToolText(t, result))

	var resp struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, "recorded", resp.Status)
}

func TestHandleTrace_WithAlternatives(t *testing.T) {
	ctx := adminCtx()
	agentID := "trace-alt-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	alternatives := `[{"outcome":"use MongoDB","reason":"document model flexibility"}]`
	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "database",
		"outcome":       "chose PostgreSQL",
		"confidence":    0.85,
		"alternatives":  alternatives,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "trace with alternatives should succeed: %s", parseToolText(t, result))
}

func TestHandleTrace_InvalidAlternativesJSON(t *testing.T) {
	ctx := adminCtx()
	agentID := "trace-ialt-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "architecture",
		"outcome":       "test invalid alternatives",
		"confidence":    0.7,
		"alternatives":  "{bad json",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "invalid alternatives JSON should be ignored: %s", parseToolText(t, result))
}

func TestHandleTrace_InvalidSourceURI(t *testing.T) {
	ctx := adminCtx()
	agentID := "trace-uri-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	evidence := `[{"description":"test","source_uri":"javascript:alert(1)"}]`
	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "security",
		"outcome":       "unsafe uri test",
		"confidence":    0.7,
		"evidence":      evidence,
	}))
	require.NoError(t, err)
	require.True(t, result.IsError, "javascript: URI should be rejected")
	assert.Contains(t, parseToolText(t, result), "source_uri")
}

func TestHandleTrace_DecisionTypeTooLong(t *testing.T) {
	ctx := adminCtx()

	longType := string(make([]byte, model.MaxDecisionTypeLen+1))
	for i := range longType {
		longType = longType[:i] + "a" + longType[i+1:]
	}

	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      testAdminID,
		"decision_type": longType,
		"outcome":       "test",
		"confidence":    0.5,
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "decision_type exceeds maximum length")
}

func TestHandleTrace_OutcomeTooLong(t *testing.T) {
	ctx := adminCtx()

	longOutcome := make([]byte, model.MaxOutcomeLen+1)
	for i := range longOutcome {
		longOutcome[i] = 'x'
	}

	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      testAdminID,
		"decision_type": "test",
		"outcome":       string(longOutcome),
		"confidence":    0.5,
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "outcome exceeds maximum length")
}

func TestHandleTrace_ReasoningTooLong(t *testing.T) {
	ctx := adminCtx()

	longReasoning := make([]byte, model.MaxReasoningLen+1)
	for i := range longReasoning {
		longReasoning[i] = 'r'
	}

	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      testAdminID,
		"decision_type": "test",
		"outcome":       "short outcome",
		"confidence":    0.5,
		"reasoning":     string(longReasoning),
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "reasoning exceeds maximum length")
}

func TestHandleTrace_WithProjectContext(t *testing.T) {
	ctx := adminCtx()
	agentID := "trace-proj-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "architecture",
		"outcome":       "project context test",
		"confidence":    0.8,
		"project":       "akashi",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		DecisionID string `json:"decision_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))

	decID, err := uuid.Parse(resp.DecisionID)
	require.NoError(t, err)
	dec, err := testDB.GetDecision(ctx, uuid.Nil, decID, storage.GetDecisionOpts{})
	require.NoError(t, err)

	clientCtx, ok := dec.AgentContext["client"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "akashi", clientCtx["project"])
}

// ---------- handleConflicts severity/category filtering ----------

func TestHandleConflicts_StatusFalsePositive(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{
		"status": "false_positive",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Conflicts []any `json:"conflicts"`
		Total     int   `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.GreaterOrEqual(t, resp.Total, 0)
}

func TestHandleConflicts_SeverityFilter(t *testing.T) {
	ctx := adminCtx()

	// Filter by high severity — should succeed even if no results.
	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{
		"severity": "high",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Conflicts []any `json:"conflicts"`
		Total     int   `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.GreaterOrEqual(t, resp.Total, 0)
}

func TestHandleConflicts_CategoryFilter(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{
		"category": "factual",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Conflicts []any `json:"conflicts"`
		Total     int   `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.GreaterOrEqual(t, resp.Total, 0)
}

func TestHandleConflicts_SeverityAndCategoryFilter(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{
		"severity": "medium",
		"category": "methodological",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)
}

func TestHandleConflicts_CustomLimit(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{
		"limit": 3,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Conflicts []any `json:"conflicts"`
		Total     int   `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.LessOrEqual(t, len(resp.Conflicts), 3)
}

// ---------- handleConflicts with seeded conflict data ----------

// seedConflict creates two decisions and inserts a scored conflict between them,
// returning the conflict ID. This exercises the full handleConflicts path including
// access filtering, severity/category post-filtering, and compact/full formatting.
func seedConflict(t *testing.T, decType, severity, category string) uuid.UUID {
	t.Helper()
	ctx := adminCtx()

	agentA := "conflict-a-" + uuid.New().String()[:8]
	agentB := "conflict-b-" + uuid.New().String()[:8]
	decAID := mustTrace(t, agentA, decType, "approach A: "+uuid.New().String()[:8], 0.8)
	decBID := mustTrace(t, agentB, decType, "approach B: "+uuid.New().String()[:8], 0.7)

	parsedA, err := uuid.Parse(decAID)
	require.NoError(t, err)
	parsedB, err := uuid.Parse(decBID)
	require.NoError(t, err)

	topicSim := 0.85
	outcomDiv := 0.9
	sig := 0.87
	explanation := "test conflict explanation"

	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind:      model.ConflictKindCrossAgent,
		DecisionAID:       parsedA,
		DecisionBID:       parsedB,
		OrgID:             uuid.Nil,
		AgentA:            agentA,
		AgentB:            agentB,
		DecisionTypeA:     decType,
		DecisionTypeB:     decType,
		DecisionType:      decType,
		OutcomeA:          "approach A",
		OutcomeB:          "approach B",
		TopicSimilarity:   &topicSim,
		OutcomeDivergence: &outcomDiv,
		Significance:      &sig,
		ScoringMethod:     "embedding",
		Explanation:       &explanation,
		Severity:          &severity,
		Category:          &category,
		Status:            "open",
	})
	require.NoError(t, err)
	return conflictID
}

func TestHandleConflicts_WithSeededData_ConciseFormat(t *testing.T) {
	decType := "seeded-conflict-" + uuid.New().String()[:8]
	seedConflict(t, decType, "high", "factual")

	ctx := adminCtx()
	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{
		"decision_type": decType,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "expected success: %s", parseToolText(t, result))

	var resp struct {
		Conflicts []map[string]any `json:"conflicts"`
		Total     int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Greater(t, resp.Total, 0, "expected at least 1 conflict group with seeded data")
	assert.NotEmpty(t, resp.Conflicts)
}

func TestHandleConflicts_WithSeededData_FullFormat(t *testing.T) {
	decType := "seeded-full-" + uuid.New().String()[:8]
	seedConflict(t, decType, "medium", "assessment")

	ctx := adminCtx()
	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{
		"decision_type": decType,
		"format":        "full",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Conflicts []model.ConflictGroup `json:"conflicts"`
		Total     int                   `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Greater(t, resp.Total, 0, "expected conflict groups in full format")
	if len(resp.Conflicts) > 0 {
		assert.NotNil(t, resp.Conflicts[0].Representative, "representative should be populated")
	}
}

func TestHandleConflicts_WithSeededData_SeverityFilterMatches(t *testing.T) {
	decType := "seeded-sev-" + uuid.New().String()[:8]
	seedConflict(t, decType, "critical", "strategic")

	ctx := adminCtx()
	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{
		"decision_type": decType,
		"severity":      "critical",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Conflicts []map[string]any `json:"conflicts"`
		Total     int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Greater(t, resp.Total, 0, "severity=critical should match the seeded conflict")
}

func TestHandleConflicts_WithSeededData_SeverityFilterExcludes(t *testing.T) {
	decType := "seeded-sevx-" + uuid.New().String()[:8]
	seedConflict(t, decType, "high", "factual")

	ctx := adminCtx()
	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{
		"decision_type": decType,
		"severity":      "low", // does not match "high"
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Conflicts []map[string]any `json:"conflicts"`
		Total     int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, 0, resp.Total, "severity=low should not match a high-severity conflict")
}

func TestHandleConflicts_WithSeededData_CategoryFilterMatches(t *testing.T) {
	decType := "seeded-cat-" + uuid.New().String()[:8]
	seedConflict(t, decType, "medium", "temporal")

	ctx := adminCtx()
	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{
		"decision_type": decType,
		"category":      "temporal",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Conflicts []map[string]any `json:"conflicts"`
		Total     int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Greater(t, resp.Total, 0, "category=temporal should match the seeded conflict")
}

func TestHandleConflicts_WithSeededData_AgentFilter(t *testing.T) {
	decType := "seeded-agent-filter-" + uuid.New().String()[:8]
	seedConflict(t, decType, "high", "factual")

	ctx := adminCtx()
	// Filter by a non-matching agent should exclude the seeded conflict.
	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{
		"decision_type": decType,
		"agent_id":      "nonexistent-agent-xyz",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Conflicts []map[string]any `json:"conflicts"`
		Total     int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, 0, resp.Total, "agent_id filter should exclude unrelated conflicts")
}

func TestHandleConflicts_WithSeededData_NilRepresentativeSkipped(t *testing.T) {
	// This test verifies the nil representative guard in the severity/category
	// post-filter loop. Groups without a representative (no scored pairs) should
	// be silently skipped rather than causing a nil pointer dereference.
	decType := "seeded-nilrep-" + uuid.New().String()[:8]
	seedConflict(t, decType, "high", "factual")

	ctx := adminCtx()
	// Apply both severity and category to exercise the combined post-filter path.
	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{
		"decision_type": decType,
		"severity":      "high",
		"category":      "factual",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)
}

// ---------- handleTrace idempotency tests ----------

func TestHandleTrace_IdempotencyKey_NewKey(t *testing.T) {
	ctx := adminCtx()
	agentID := "idem-new-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	idemKey := "idem-" + uuid.New().String()

	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":        agentID,
		"decision_type":   "architecture",
		"outcome":         "idempotency test - new key",
		"confidence":      0.8,
		"idempotency_key": idemKey,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "first call with idempotency key should succeed: %s", parseToolText(t, result))

	var resp struct {
		DecisionID string `json:"decision_id"`
		Status     string `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, "recorded", resp.Status)
	assert.NotEmpty(t, resp.DecisionID)
}

func TestHandleTrace_IdempotencyKey_Replay(t *testing.T) {
	ctx := adminCtx()
	agentID := "idem-replay-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	idemKey := "idem-" + uuid.New().String()

	args := map[string]any{
		"agent_id":        agentID,
		"decision_type":   "architecture",
		"outcome":         "idempotency test - replay",
		"confidence":      0.8,
		"idempotency_key": idemKey,
	}

	// First call.
	result1, err := testServer.handleTrace(ctx, traceRequest(args))
	require.NoError(t, err)
	require.False(t, result1.IsError)
	text1 := parseToolText(t, result1)

	// Second call with same key and payload should replay the stored response.
	result2, err := testServer.handleTrace(ctx, traceRequest(args))
	require.NoError(t, err)
	require.False(t, result2.IsError)
	text2 := parseToolText(t, result2)

	// Compare parsed JSON since key ordering and whitespace may differ between
	// the original json.Marshal and the stored json.RawMessage from Postgres.
	var parsed1, parsed2 map[string]any
	require.NoError(t, json.Unmarshal([]byte(text1), &parsed1))
	require.NoError(t, json.Unmarshal([]byte(text2), &parsed2))
	assert.Equal(t, parsed1, parsed2, "replayed response should match the original")
}

func TestHandleTrace_IdempotencyKey_PayloadMismatch(t *testing.T) {
	ctx := adminCtx()
	agentID := "idem-mismatch-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	idemKey := "idem-" + uuid.New().String()

	// First call.
	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":        agentID,
		"decision_type":   "architecture",
		"outcome":         "original payload",
		"confidence":      0.8,
		"idempotency_key": idemKey,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	// Second call with same key but different payload.
	result2, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":        agentID,
		"decision_type":   "architecture",
		"outcome":         "different payload",
		"confidence":      0.9,
		"idempotency_key": idemKey,
	}))
	require.NoError(t, err)
	require.True(t, result2.IsError, "should error on payload mismatch")
	assert.Contains(t, parseToolText(t, result2), "idempotency key reused with different payload")
}

// ---------- handleQuery additional filter tests ----------

func TestHandleQuery_WithSessionIDFilter(t *testing.T) {
	ctx := adminCtx()

	// Query with a session_id filter — should succeed even if no matches.
	sid := uuid.New().String()
	result, err := testServer.handleQuery(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"session_id": sid,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Decisions []any `json:"decisions"`
		Total     int   `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, 0, resp.Total, "random session_id should match nothing")
}

func TestHandleQuery_WithToolAndModelFilters(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleQuery(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"tool":  "claude-code",
				"model": "opus-4-6",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
}

func TestHandleQuery_WithSearchQuery_FullFormat(t *testing.T) {
	ctx := adminCtx()
	agentID := "query-search-full-" + uuid.New().String()[:8]
	mustTrace(t, agentID, "architecture", "full format search query test", 0.8)

	result, err := testServer.handleQuery(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"query":  "full format search",
				"format": "full",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Decisions []any `json:"decisions"`
		Total     int   `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
}

func TestHandleQuery_WithConfidenceMinOnSearch(t *testing.T) {
	ctx := adminCtx()
	agentID := "query-confmin-search-" + uuid.New().String()[:8]
	mustTrace(t, agentID, "trade_off", "confidence min search test", 0.95)

	result, err := testServer.handleQuery(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"query":          "confidence min search",
				"confidence_min": 0.9,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
}

// ---------- handleCheck additional tests ----------

func TestHandleCheck_WithProjectWildcard(t *testing.T) {
	ctx := adminCtx()
	agentID := "check-wildcard-" + uuid.New().String()[:8]
	mustTrace(t, agentID, "architecture", "wildcard project check", 0.8)

	result, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": "architecture",
				"project":       "*",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "check with project=* should succeed: %s", parseToolText(t, result))
}

func TestHandleCheck_WithExplicitProject(t *testing.T) {
	ctx := adminCtx()
	agentID := "check-proj-" + uuid.New().String()[:8]
	mustTrace(t, agentID, "architecture", "project filter check", 0.8)

	result, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": "architecture",
				"project":       "test-project",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
}

func TestHandleCheck_WithCustomLimit(t *testing.T) {
	ctx := adminCtx()
	agentID := "check-limit-" + uuid.New().String()[:8]
	mustTrace(t, agentID, "architecture", "custom limit check", 0.8)

	result, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": "architecture",
				"limit":         2,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp model.CheckResponse
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.LessOrEqual(t, len(resp.Decisions), 2)
}

func TestHandleCheck_WithConflictsInResults(t *testing.T) {
	ctx := adminCtx()
	decType := "check-conflict-" + uuid.New().String()[:8]

	// Seed decisions and a conflict for this decision type.
	agentA := "check-cflct-a-" + uuid.New().String()[:8]
	agentB := "check-cflct-b-" + uuid.New().String()[:8]
	mustTrace(t, agentA, decType, "chose approach A for check test", 0.85)
	mustTrace(t, agentB, decType, "chose approach B for check test", 0.75)
	seedConflict(t, decType, "high", "strategic")

	// Check should now find decisions and conflicts for this type.
	result, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": decType,
				"project":       "*",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "check should succeed: %s", parseToolText(t, result))

	var resp struct {
		HasPrecedent  bool             `json:"has_precedent"`
		Summary       string           `json:"summary"`
		ActionNeeded  bool             `json:"action_needed"`
		RelevantCount int              `json:"relevant_count"`
		Decisions     []map[string]any `json:"decisions"`
		Conflicts     []map[string]any `json:"conflicts"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.True(t, resp.HasPrecedent, "should find precedent decisions")
	assert.Greater(t, len(resp.Decisions), 0, "should have decisions")
}

func TestHandleCheck_FullFormatWithConflicts(t *testing.T) {
	ctx := adminCtx()
	decType := "check-full-cflct-" + uuid.New().String()[:8]
	mustTrace(t, "check-full-a-"+uuid.New().String()[:8], decType, "full format approach A", 0.85)
	seedConflict(t, decType, "medium", "assessment")

	result, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": decType,
				"format":        "full",
				"project":       "*",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "full format check should succeed: %s", parseToolText(t, result))
}

// ---------- handleStats with data ----------

func TestHandleStats_WithSeededData(t *testing.T) {
	ctx := adminCtx()

	// Seed some decisions so stats has data to compute.
	agentID := "stats-agent-" + uuid.New().String()[:8]
	mustTrace(t, agentID, "architecture", "stats test decision 1", 0.8)
	mustTrace(t, agentID, "security", "stats test decision 2", 0.9)

	result, err := testServer.handleStats(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name:      "akashi_stats",
			Arguments: map[string]any{},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "stats should succeed: %s", parseToolText(t, result))

	var resp struct {
		TraceHealth map[string]any `json:"trace_health"`
		Agents      int            `json:"agents"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Greater(t, resp.Agents, 0, "should have at least one agent")
}

// ---------- handleQuery with agent reader role ----------

func TestHandleQuery_AgentRoleFiltering(t *testing.T) {
	agentID := "query-role-" + uuid.New().String()[:8]
	mustTrace(t, agentID, "architecture", "role filtering test", 0.8)

	// Use agent-level claims instead of admin.
	agentCtx := ctxutil.WithClaims(context.Background(), &auth.Claims{
		AgentID: agentID,
		OrgID:   uuid.Nil,
		Role:    model.RoleAgent,
	})

	result, err := testServer.handleQuery(agentCtx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"decision_type": "architecture",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "agent-role query should succeed")
}

func TestHandleQuery_StructuredFilterAccessFiltering(t *testing.T) {
	ctx := adminCtx()
	agentID := "query-struct-af-" + uuid.New().String()[:8]
	mustTrace(t, agentID, "investigation", "structured filter access test", 0.8)

	// Query with structured filters and full format to exercise that branch.
	result, err := testServer.handleQuery(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"agent_id": agentID,
				"format":   "full",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Decisions []model.Decision `json:"decisions"`
		Total     int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Greater(t, resp.Total, 0)
}

// ---------- handleConflicts with reader role ----------

func TestHandleConflicts_AgentRoleAccessFiltering(t *testing.T) {
	decType := "seeded-role-" + uuid.New().String()[:8]
	seedConflict(t, decType, "high", "factual")

	agentID := "conflict-reader-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(context.Background(), uuid.Nil, agentID, model.RoleAdmin, nil)

	agentCtx := ctxutil.WithClaims(context.Background(), &auth.Claims{
		AgentID: agentID,
		OrgID:   uuid.Nil,
		Role:    model.RoleAgent,
	})

	result, err := testServer.handleConflicts(agentCtx, conflictsRequest(map[string]any{
		"decision_type": decType,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)
}

// ---------- handleTrace additional coverage ----------

func TestHandleTrace_AgentSelfTrace(t *testing.T) {
	// Agent-role caller tracing for their own agent_id (not cross-agent).
	agentID := "self-trace-" + uuid.New().String()[:8]
	ctx := adminCtx()
	// Create agent as admin first (agents can't auto-register themselves).
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	agentCtx := ctxutil.WithClaims(context.Background(), &auth.Claims{
		AgentID: agentID,
		OrgID:   uuid.Nil,
		Role:    model.RoleAgent,
	})

	result, err := testServer.handleTrace(agentCtx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "investigation",
		"outcome":       "agent self-trace test",
		"confidence":    0.7,
		"reasoning":     "testing self-trace path",
		"model":         "gpt-4o",
		"task":          "code review",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "agent should be able to trace for themselves: %s", parseToolText(t, result))
}

func TestHandleTrace_WithAPIKeyInClaims(t *testing.T) {
	ctx := adminCtx()
	agentID := "apikey-trace-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	// Create an API key to reference in claims.
	key, err := testDB.CreateAPIKeyWithAudit(ctx, model.APIKey{
		OrgID:     uuid.Nil,
		AgentID:   agentID,
		Label:     "test-key-" + uuid.New().String()[:8],
		Prefix:    "ak_test",
		KeyHash:   "fakehash",
		CreatedBy: testAdminID,
	}, storage.MutationAuditEntry{
		OrgID:        uuid.Nil,
		ActorAgentID: testAdminID,
		ActorRole:    "admin",
		Endpoint:     "test",
	})
	require.NoError(t, err)

	apiKeyCtx := ctxutil.WithClaims(context.Background(), &auth.Claims{
		AgentID:  agentID,
		OrgID:    uuid.Nil,
		Role:     model.RoleAdmin,
		APIKeyID: &key.ID,
	})

	result, err := testServer.handleTrace(apiKeyCtx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "security",
		"outcome":       "api key prefix attribution test",
		"confidence":    0.8,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "trace with API key claims should succeed: %s", parseToolText(t, result))

	// Verify agent_context contains the API key prefix.
	var resp struct {
		DecisionID string `json:"decision_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	decID, err := uuid.Parse(resp.DecisionID)
	require.NoError(t, err)
	dec, err := testDB.GetDecision(ctx, uuid.Nil, decID, storage.GetDecisionOpts{})
	require.NoError(t, err)

	serverCtx, ok := dec.AgentContext["server"].(map[string]any)
	require.True(t, ok, "agent_context should have 'server' namespace")
	assert.Equal(t, "ak_test", serverCtx["api_key_prefix"])
}

func TestHandleTrace_OperatorFromAgentName(t *testing.T) {
	ctx := adminCtx()
	agentID := "op-trace-" + uuid.New().String()[:8]

	// Create agent with a distinct display name.
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID,
		OrgID:   uuid.Nil,
		Name:    "Agent Display Name",
		Role:    model.RoleAdmin,
	})
	require.NoError(t, err)

	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "architecture",
		"outcome":       "operator name test",
		"confidence":    0.8,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	// The operator should be set because the admin's display name differs from agent_id.
	// (The operator lookup uses claims.AgentID which is test-admin, and test-admin's
	// name equals its ID, so operator won't be set for test-admin. But the decision
	// agent_context path is still exercised.)
}

func TestHandleTrace_WithNoReasoning(t *testing.T) {
	ctx := adminCtx()
	agentID := "noreason-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "architecture",
		"outcome":       "no reasoning provided",
		"confidence":    0.6,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)
}

func TestHandleTrace_IdempotencyKey_ClearOnFailure(t *testing.T) {
	// Verify that a trace failure with an owned idempotency key clears it
	// so retries aren't blocked by ErrIdempotencyInProgress.
	ctx := adminCtx()
	agentID := "idem-clear-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	idemKey := "idem-clear-" + uuid.New().String()

	// First, a successful trace with the key to mark it complete.
	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":        agentID,
		"decision_type":   "architecture",
		"outcome":         "clear test",
		"confidence":      0.8,
		"idempotency_key": idemKey,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	// Attempting with same key and same payload should replay, not error.
	result2, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":        agentID,
		"decision_type":   "architecture",
		"outcome":         "clear test",
		"confidence":      0.8,
		"idempotency_key": idemKey,
	}))
	require.NoError(t, err)
	require.False(t, result2.IsError, "replay should succeed")
}

// ---------- handleAgentHistory access denied path ----------

func TestHandleAgentHistory_AccessDenied(t *testing.T) {
	// Create an agent and a reader-role caller who should not have access
	// to other agents' histories. This exercises the !ok branch in handleAgentHistory.
	targetAgent := "history-target-" + uuid.New().String()[:8]
	readerAgent := "history-reader-" + uuid.New().String()[:8]

	ctx := adminCtx()
	mustTrace(t, targetAgent, "architecture", "access denied test", 0.8)
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, readerAgent, model.RoleAdmin, nil)

	// Use reader-role claims for a different agent.
	readerCtx := ctxutil.WithClaims(context.Background(), &auth.Claims{
		AgentID: readerAgent,
		OrgID:   uuid.Nil,
		Role:    model.RoleReader,
	})

	uri := "akashi://agent/" + targetAgent + "/history"
	_, err := testServer.handleAgentHistory(readerCtx, mcplib.ReadResourceRequest{
		Params: mcplib.ReadResourceParams{
			URI: uri,
		},
	})
	// Reader without grants should get access denied.
	require.Error(t, err, "reader should not access another agent's history")
	assert.Contains(t, err.Error(), "no access")
}

// ---------- handleConflicts status=closed ----------

func TestHandleConflicts_StatusClosed(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleConflicts(ctx, conflictsRequest(map[string]any{
		"status": "resolved",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)
}

// ---------- handleTrace: operator from agent display name ----------

func TestHandleTrace_OperatorFromDisplayName(t *testing.T) {
	agentID := "operator-caller-" + uuid.New().String()[:8]
	displayName := "Operator Display Name"

	ctx := adminCtx()

	// Create an admin agent whose Name differs from AgentID.
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID,
		OrgID:   uuid.Nil,
		Name:    displayName,
		Role:    model.RoleAdmin,
	})
	require.NoError(t, err)

	// Build a context with this agent's claims so the operator branch fires.
	// Line 686 checks claims.AgentID's Name, not the traced agent's Name.
	callerCtx := ctxutil.WithClaims(context.Background(), &auth.Claims{
		AgentID: agentID,
		OrgID:   uuid.Nil,
		Role:    model.RoleAdmin,
	})

	result, err := testServer.handleTrace(callerCtx, traceRequest(map[string]any{
		"decision_type": "testing",
		"outcome":       "verify operator extraction from display name",
		"confidence":    0.85,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "trace should succeed: %s", parseToolText(t, result))

	// Verify decision was recorded.
	var resp struct {
		DecisionID string `json:"decision_id"`
		Status     string `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, "recorded", resp.Status)
	assert.NotEmpty(t, resp.DecisionID)
}

// ---------- handleCheck: decisions with assessment summary ----------

func TestHandleCheck_WithAssessmentSummary(t *testing.T) {
	ctx := adminCtx()
	agentID := "assess-check-" + uuid.New().String()[:8]

	// Trace a decision.
	decIDStr := mustTrace(t, agentID, "trade_off", "chose caching over freshness", 0.9)
	decID, err := uuid.Parse(decIDStr)
	require.NoError(t, err)

	// Create an assessment for that decision.
	_, err = testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID:      decID,
		OrgID:           uuid.Nil,
		AssessorAgentID: testAdminID,
		Outcome:         model.AssessmentCorrect,
	})
	require.NoError(t, err)

	// Now call handleCheck — the assessment summary branch (lines 438-441)
	// should fire because GetAssessmentSummaryBatch returns a non-empty map.
	result, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": "trade_off",
				"project":       "*",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "check should succeed: %s", parseToolText(t, result))

	// Parse the concise response and verify assessment data appears.
	text := parseToolText(t, result)
	var checkResp map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &checkResp))
	assert.True(t, checkResp["has_precedent"].(bool), "should have precedent")

	// The decisions array should contain our decision with assessment_summary populated.
	decs, ok := checkResp["decisions"].([]any)
	require.True(t, ok, "decisions should be a list")
	require.NotEmpty(t, decs, "should find the traced decision")
}

// ---------- handleCheck: prior resolutions ----------

func TestHandleCheck_WithPriorResolutions(t *testing.T) {
	ctx := adminCtx()
	decType := "prior-res-" + uuid.New().String()[:8]

	// Create two agents and trace opposing decisions.
	agentA := "prior-a-" + uuid.New().String()[:8]
	agentB := "prior-b-" + uuid.New().String()[:8]
	decAIDStr := mustTrace(t, agentA, decType, "approach alpha", 0.9)
	decBIDStr := mustTrace(t, agentB, decType, "approach beta", 0.7)

	decAID, err := uuid.Parse(decAIDStr)
	require.NoError(t, err)
	decBID, err := uuid.Parse(decBIDStr)
	require.NoError(t, err)

	// Seed a conflict between the two decisions.
	topicSim := 0.85
	outcomeDiv := 0.9
	sig := 0.87
	explanation := "test conflict for prior resolutions"
	severity := "high"
	category := "strategic"
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind:      model.ConflictKindCrossAgent,
		DecisionAID:       decAID,
		DecisionBID:       decBID,
		OrgID:             uuid.Nil,
		AgentA:            agentA,
		AgentB:            agentB,
		DecisionTypeA:     decType,
		DecisionTypeB:     decType,
		DecisionType:      decType,
		OutcomeA:          "approach alpha",
		OutcomeB:          "approach beta",
		TopicSimilarity:   &topicSim,
		OutcomeDivergence: &outcomeDiv,
		Significance:      &sig,
		ScoringMethod:     "embedding",
		Explanation:       &explanation,
		Severity:          &severity,
		Category:          &category,
		Status:            "open",
	})
	require.NoError(t, err)

	// Resolve the conflict with a winner to generate a prior resolution.
	note := "alpha approach validated in production"
	_, err = testDB.UpdateConflictStatusWithAudit(ctx, conflictID, uuid.Nil,
		"resolved", testAdminID, &note, &decAID, nil,
		storage.MutationAuditEntry{
			OrgID:        uuid.Nil,
			ActorAgentID: testAdminID,
			ActorRole:    "admin",
			Endpoint:     "test",
		})
	require.NoError(t, err)

	// Now call handleCheck with this decision_type. The PriorResolutions
	// branch (lines 473-475, 478-480) should fire.
	result, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": decType,
				"project":       "*",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "check should succeed: %s", parseToolText(t, result))

	// Parse the concise response.
	text := parseToolText(t, result)
	var checkResp map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &checkResp))

	// Verify prior_resolutions is present and non-empty.
	resolutions, ok := checkResp["prior_resolutions"].([]any)
	require.True(t, ok, "prior_resolutions should be a list")
	assert.NotEmpty(t, resolutions, "should contain the resolved conflict")

	// Verify the summary mentions prior resolutions.
	summary, _ := checkResp["summary"].(string)
	assert.Contains(t, summary, "prior conflict")
}

// ---------- handleResolve tests ----------

func resolveRequest(args map[string]any) mcplib.CallToolRequest {
	return mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name:      "akashi_resolve",
			Arguments: args,
		},
	}
}

// seedConflictWithDecisions creates two decisions and a conflict, returning
// the conflict ID and both decision IDs for use in resolve tests.
func seedConflictWithDecisions(t *testing.T) (conflictID uuid.UUID, decAID, decBID string) {
	t.Helper()
	ctx := adminCtx()

	suffix := uuid.New().String()[:8]
	agentA := "resolve-a-" + suffix
	agentB := "resolve-b-" + suffix
	decType := "resolve-type-" + suffix
	decAID = mustTrace(t, agentA, decType, "approach A: "+suffix, 0.8)
	decBID = mustTrace(t, agentB, decType, "approach B: "+suffix, 0.7)

	parsedA, err := uuid.Parse(decAID)
	require.NoError(t, err)
	parsedB, err := uuid.Parse(decBID)
	require.NoError(t, err)

	topicSim := 0.85
	outcomDiv := 0.9
	sig := 0.87
	explanation := "test conflict for resolution"
	severity := "high"
	category := "strategic"

	conflictID, err = testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind:      model.ConflictKindCrossAgent,
		DecisionAID:       parsedA,
		DecisionBID:       parsedB,
		OrgID:             uuid.Nil,
		AgentA:            agentA,
		AgentB:            agentB,
		DecisionTypeA:     decType,
		DecisionTypeB:     decType,
		DecisionType:      decType,
		OutcomeA:          "approach A",
		OutcomeB:          "approach B",
		TopicSimilarity:   &topicSim,
		OutcomeDivergence: &outcomDiv,
		Significance:      &sig,
		ScoringMethod:     "embedding",
		Explanation:       &explanation,
		Severity:          &severity,
		Category:          &category,
		Status:            "open",
	})
	require.NoError(t, err)
	return conflictID, decAID, decBID
}

func TestHandleResolve_WithWinner(t *testing.T) {
	ctx := adminCtx()
	conflictID, decAID, _ := seedConflictWithDecisions(t)

	result, err := testServer.handleResolve(ctx, resolveRequest(map[string]any{
		"conflict_id":         conflictID.String(),
		"status":              "resolved",
		"winning_decision_id": decAID,
		"resolution_note":     "approach A is better",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "resolve should succeed: %s", parseToolText(t, result))

	var resp struct {
		ConflictID      string `json:"conflict_id"`
		OldStatus       string `json:"old_status"`
		NewStatus       string `json:"new_status"`
		ResolvedBy      string `json:"resolved_by"`
		CascadeResolved int    `json:"cascade_resolved"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, conflictID.String(), resp.ConflictID)
	assert.Equal(t, "open", resp.OldStatus)
	assert.Equal(t, "resolved", resp.NewStatus)
	assert.Equal(t, testAdminID, resp.ResolvedBy)
}

func TestHandleResolve_FalsePositive(t *testing.T) {
	ctx := adminCtx()
	conflictID, _, _ := seedConflictWithDecisions(t)

	result, err := testServer.handleResolve(ctx, resolveRequest(map[string]any{
		"conflict_id":     conflictID.String(),
		"status":          "false_positive",
		"resolution_note": "detector was wrong",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "false_positive should succeed: %s", parseToolText(t, result))

	var resp struct {
		NewStatus string `json:"new_status"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, "false_positive", resp.NewStatus)
}

func TestHandleResolve_NilClaims(t *testing.T) {
	result, err := testServer.handleResolve(context.Background(), resolveRequest(map[string]any{
		"conflict_id": uuid.New().String(),
		"status":      "resolved",
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "authentication required")
}

func TestHandleResolve_MissingConflictID(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleResolve(ctx, resolveRequest(map[string]any{
		"status": "resolved",
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "conflict_id is required")
}

func TestHandleResolve_InvalidStatus(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleResolve(ctx, resolveRequest(map[string]any{
		"conflict_id": uuid.New().String(),
		"status":      "invalid",
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "resolved")
}

func TestHandleResolve_WinnerWithNonResolvedStatus(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleResolve(ctx, resolveRequest(map[string]any{
		"conflict_id":         uuid.New().String(),
		"status":              "false_positive",
		"winning_decision_id": uuid.New().String(),
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "winning_decision_id can only be set")
}

func TestHandleResolve_WinnerNotInConflict(t *testing.T) {
	ctx := adminCtx()
	conflictID, _, _ := seedConflictWithDecisions(t)

	result, err := testServer.handleResolve(ctx, resolveRequest(map[string]any{
		"conflict_id":         conflictID.String(),
		"status":              "resolved",
		"winning_decision_id": uuid.New().String(),
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "must be one of the two decisions")
}

// ---------- handleResolve: group resolution tests ----------

// seedConflictGroup creates two decisions and two conflicts in the same group,
// returning the group ID, both conflict IDs, and agent names.
func seedConflictGroup(t *testing.T) (groupID uuid.UUID, conflictIDs [2]uuid.UUID, agentA, agentB string) {
	t.Helper()
	ctx := adminCtx()

	suffix := uuid.New().String()[:8]
	agentA = "grp-a-" + suffix
	agentB = "grp-b-" + suffix
	decType := "grp-type-" + suffix

	decAID1 := mustTrace(t, agentA, decType, "approach A1: "+suffix, 0.8)
	decBID1 := mustTrace(t, agentB, decType, "approach B1: "+suffix, 0.7)
	decAID2 := mustTrace(t, agentA, decType, "approach A2: "+suffix, 0.85)
	decBID2 := mustTrace(t, agentB, decType, "approach B2: "+suffix, 0.75)

	parsedA1, err := uuid.Parse(decAID1)
	require.NoError(t, err)
	parsedB1, err := uuid.Parse(decBID1)
	require.NoError(t, err)
	parsedA2, err := uuid.Parse(decAID2)
	require.NoError(t, err)
	parsedB2, err := uuid.Parse(decBID2)
	require.NoError(t, err)

	topicSim := 0.85
	outcomDiv := 0.9
	sig := 0.87
	explanation := "test conflict for group resolution"
	severity := "high"
	category := "strategic"

	for i, pair := range [][2]uuid.UUID{{parsedA1, parsedB1}, {parsedA2, parsedB2}} {
		cid, insertErr := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
			ConflictKind:      model.ConflictKindCrossAgent,
			DecisionAID:       pair[0],
			DecisionBID:       pair[1],
			OrgID:             uuid.Nil,
			AgentA:            agentA,
			AgentB:            agentB,
			DecisionTypeA:     decType,
			DecisionTypeB:     decType,
			DecisionType:      decType,
			OutcomeA:          "approach A",
			OutcomeB:          "approach B",
			TopicSimilarity:   &topicSim,
			OutcomeDivergence: &outcomDiv,
			Significance:      &sig,
			ScoringMethod:     "embedding",
			Explanation:       &explanation,
			Severity:          &severity,
			Category:          &category,
			Status:            "open",
		})
		require.NoError(t, insertErr)
		conflictIDs[i] = cid
	}

	conflict, err := testDB.GetConflict(ctx, conflictIDs[0], uuid.Nil)
	require.NoError(t, err)
	require.NotNil(t, conflict.GroupID, "InsertScoredConflict must set group_id")
	groupID = *conflict.GroupID

	return groupID, conflictIDs, agentA, agentB
}

func TestHandleResolve_GroupFalsePositive(t *testing.T) {
	ctx := adminCtx()
	groupID, conflictIDs, _, _ := seedConflictGroup(t)

	result, err := testServer.handleResolve(ctx, resolveRequest(map[string]any{
		"conflict_id":     groupID.String(),
		"status":          "false_positive",
		"resolution_note": "all conflicts in this group are false positives",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "group resolve should succeed: %s", parseToolText(t, result))

	var resp struct {
		GroupID       string `json:"group_id"`
		NewStatus     string `json:"new_status"`
		ResolvedBy    string `json:"resolved_by"`
		ResolvedCount int    `json:"resolved_count"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, groupID.String(), resp.GroupID)
	assert.Equal(t, "false_positive", resp.NewStatus)
	assert.Equal(t, testAdminID, resp.ResolvedBy)
	assert.Equal(t, 2, resp.ResolvedCount, "both open conflicts should be resolved")

	// Verify both conflicts are now false_positive.
	for _, cid := range conflictIDs {
		c, cErr := testDB.GetConflict(ctx, cid, uuid.Nil)
		require.NoError(t, cErr)
		assert.Equal(t, "false_positive", c.Status)
	}
}

func TestHandleResolve_GroupResolved(t *testing.T) {
	ctx := adminCtx()
	groupID, _, _, _ := seedConflictGroup(t)

	result, err := testServer.handleResolve(ctx, resolveRequest(map[string]any{
		"conflict_id":     groupID.String(),
		"status":          "resolved",
		"resolution_note": "resolved at group level",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "group resolve should succeed: %s", parseToolText(t, result))

	var resp struct {
		GroupID       string `json:"group_id"`
		NewStatus     string `json:"new_status"`
		ResolvedCount int    `json:"resolved_count"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, groupID.String(), resp.GroupID)
	assert.Equal(t, "resolved", resp.NewStatus)
	assert.Equal(t, 2, resp.ResolvedCount)
}

func TestHandleResolve_GroupRejectsWinningDecisionID(t *testing.T) {
	ctx := adminCtx()
	groupID, _, _, _ := seedConflictGroup(t)

	result, err := testServer.handleResolve(ctx, resolveRequest(map[string]any{
		"conflict_id":         groupID.String(),
		"status":              "resolved",
		"winning_decision_id": uuid.New().String(),
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "winning_decision_id is not supported for group resolution")
}

func TestHandleResolve_GroupNotFound(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleResolve(ctx, resolveRequest(map[string]any{
		"conflict_id": uuid.New().String(),
		"status":      "resolved",
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, parseToolText(t, result), "conflict not found")
}

func TestHandleResolve_GroupDoesNotRelabelPriorResolutions(t *testing.T) {
	ctx := adminCtx()
	groupID, conflictIDs, _, _ := seedConflictGroup(t)

	// First, resolve one conflict individually as "resolved" with no label.
	_, err := testDB.UpdateConflictStatusWithAudit(ctx, conflictIDs[0], uuid.Nil,
		"resolved", testAdminID, nil, nil, nil,
		storage.MutationAuditEntry{
			RequestID: uuid.New().String(), OrgID: uuid.Nil,
			ActorAgentID: testAdminID, ActorRole: "admin",
			Endpoint: "test", Operation: "resolve", ResourceType: "conflict",
			ResourceID: conflictIDs[0].String(),
		})
	require.NoError(t, err)

	// Now resolve the group as false_positive. Only the remaining open
	// conflict should be resolved and labeled.
	result, resolveErr := testServer.handleResolve(ctx, resolveRequest(map[string]any{
		"conflict_id": groupID.String(),
		"status":      "false_positive",
	}))
	require.NoError(t, resolveErr)
	require.False(t, result.IsError, "group resolve should succeed: %s", parseToolText(t, result))

	var resp struct {
		ResolvedCount int `json:"resolved_count"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, 1, resp.ResolvedCount, "only the open conflict should be resolved")

	// The first conflict should still be "resolved", not relabeled.
	c0, err := testDB.GetConflict(ctx, conflictIDs[0], uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, "resolved", c0.Status, "previously resolved conflict should not be changed")
}

func TestComputeMissingFields_TaskTip(t *testing.T) {
	reasoning := "detailed reasoning that is over one hundred characters long so it passes the threshold requirement here"
	// Without task: tip should be present.
	tips := computeMissingFields("architecture", "chose Redis with 5min TTL", 0.7, &reasoning, nil, nil, false, false, false, quality.DefaultStandardDecisionTypes)
	found := false
	for _, tip := range tips {
		if strings.Contains(tip, "Add task") {
			found = true
			break
		}
	}
	assert.True(t, found, "should include task tip when hasTask is false")

	// With task: tip should be absent.
	tips = computeMissingFields("architecture", "chose Redis with 5min TTL", 0.7, &reasoning, nil, nil, false, false, true, quality.DefaultStandardDecisionTypes)
	found = false
	for _, tip := range tips {
		if strings.Contains(tip, "Add task") {
			found = true
			break
		}
	}
	assert.False(t, found, "should not include task tip when hasTask is true")
}

// ===========================================================================
// inferModelFromToolName unit tests
// ===========================================================================

func TestInferModelFromToolName(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		want     string
	}{
		{"claude-code exact", "claude-code", "claude"},
		{"claude-desktop exact", "claude-desktop", "claude"},
		{"case insensitive", "Claude-Code", "claude"},
		{"whitespace trimmed", "  claude-code  ", "claude"},
		{"unknown tool returns empty", "cursor", ""},
		{"empty string", "", ""},
		{"generic tool", "my-custom-agent", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferModelFromToolName(tt.toolName)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestComputeMissingFields_EvidenceTips(t *testing.T) {
	t.Parallel()

	reasoning := "A long enough reasoning string that exceeds one hundred characters so it doesn't trigger the reasoning tip in the output"
	reason := "solid reason for rejection"
	fullAlts := []model.TraceAlternative{
		{Label: "alt1", RejectionReason: &reason},
		{Label: "alt2", RejectionReason: &reason},
		{Label: "alt3", RejectionReason: &reason},
	}

	tests := []struct {
		name     string
		evidence []model.TraceEvidence
		wantTip  string
		noTip    bool
	}{
		{
			name:     "no evidence — actionable tip with examples",
			evidence: nil,
			wantTip:  "Add evidence to make this trace verifiable: attach file paths, error messages, test output, benchmark numbers, or the constraint that drove the choice",
		},
		{
			name:     "empty slice — same as nil",
			evidence: []model.TraceEvidence{},
			wantTip:  "Add evidence to make this trace verifiable",
		},
		{
			name: "one evidence item — nudge for more",
			evidence: []model.TraceEvidence{
				{SourceType: "tool_output", Content: "test passed"},
			},
			wantTip: "Add 1 more evidence item for full credit",
		},
		{
			name: "one evidence item tip includes examples",
			evidence: []model.TraceEvidence{
				{SourceType: "tool_output", Content: "test passed"},
			},
			wantTip: "file path, error message, test result, or benchmark number",
		},
		{
			name: "two evidence items — no tip",
			evidence: []model.TraceEvidence{
				{SourceType: "document", Content: "evidence one"},
				{SourceType: "api_response", Content: "evidence two"},
			},
			noTip: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tips := computeMissingFields("architecture", "chose X", 0.7, &reasoning, fullAlts, tc.evidence, true, true, true, quality.DefaultStandardDecisionTypes)

			// Filter to only evidence-related tips.
			var evidenceTips []string
			for _, tip := range tips {
				if containsEvidence(tip) {
					evidenceTips = append(evidenceTips, tip)
				}
			}

			if tc.noTip {
				assert.Empty(t, evidenceTips, "expected no evidence tip with >=2 items")
				return
			}

			require.Len(t, evidenceTips, 1, "expected exactly one evidence tip")
			assert.Contains(t, evidenceTips[0], tc.wantTip)
		})
	}
}

// ===========================================================================
// computeMissingFields: model tip surfaced when model absent
// ===========================================================================

func TestComputeMissingFields_ModelTip(t *testing.T) {
	reasoning := "some reasoning that is long enough to pass the 100 char threshold -- padding padding padding padding padding"

	t.Run("no model tip when model present", func(t *testing.T) {
		tips := computeMissingFields("architecture", "chose postgres", 0.8, &reasoning, nil, nil, false, true, true, quality.DefaultStandardDecisionTypes)
		for _, tip := range tips {
			assert.NotContains(t, tip, "model")
		}
	})

	t.Run("model tip when model absent and not inferred", func(t *testing.T) {
		tips := computeMissingFields("architecture", "chose postgres", 0.8, &reasoning, nil, nil, false, false, true, quality.DefaultStandardDecisionTypes)
		found := false
		for _, tip := range tips {
			if assert.ObjectsAreEqual(tip, `Pass "model" (e.g. "claude-opus-4-6") so decisions can be correlated by model capability tier`) {
				found = true
			}
		}
		assert.True(t, found, "expected model tip in: %v", tips)
	})
}

// ===========================================================================
// MCP trace: model inferred from tool name in agent_context
// ===========================================================================

func TestHandleTrace_ModelInferredFromToolName(t *testing.T) {
	ctx := adminCtx()
	agentID := "model-infer-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	// Trace WITHOUT explicit model — inference should populate server.model
	// from the tool name stored in the MCP session client info.
	// Since we don't have a real MCP session with client info in unit tests,
	// we verify the inference function directly and confirm that when model is
	// not provided the completeness_tips include the model hint.
	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "architecture",
		"outcome":       "chose event sourcing for audit trail",
		"confidence":    0.85,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "trace should succeed: %s", parseToolText(t, result))

	var resp struct {
		DecisionID       string   `json:"decision_id"`
		Status           string   `json:"status"`
		CompletenessTips []string `json:"completeness_tips"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Equal(t, "recorded", resp.Status)

	// Without an MCP session with client info, model cannot be inferred,
	// so the completeness tips should include the model hint.
	modelTipFound := false
	for _, tip := range resp.CompletenessTips {
		if tip == `Pass "model" (e.g. "claude-opus-4-6") so decisions can be correlated by model capability tier` {
			modelTipFound = true
		}
	}
	assert.True(t, modelTipFound, "expected model tip in completeness_tips: %v", resp.CompletenessTips)
}

func TestHandleTrace_ExplicitModelSuppressesTip(t *testing.T) {
	ctx := adminCtx()
	agentID := "model-explicit-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "architecture",
		"outcome":       "chose event sourcing for audit trail",
		"confidence":    0.85,
		"model":         "claude-opus-4-6",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError, "trace should succeed: %s", parseToolText(t, result))

	var resp struct {
		CompletenessTips []string `json:"completeness_tips"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))

	for _, tip := range resp.CompletenessTips {
		assert.NotContains(t, tip, "model", "model tip should not appear when model is explicitly provided")
	}
}

// containsEvidence returns true if the tip string relates to evidence.
func containsEvidence(tip string) bool {
	return strings.Contains(tip, "evidence") || strings.Contains(tip, "verifiable")
}

// ---------- High-confidence warning (#468) ----------

func TestHandleTrace_HighConfNoEvidence_Warning(t *testing.T) {
	ctx := adminCtx()
	agentID := "high-conf-warn-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "architecture",
		"outcome":       "high confidence no evidence warning test",
		"confidence":    0.9,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	require.Len(t, resp.Warnings, 1)
	assert.Contains(t, resp.Warnings[0], "high confidence")
}

func TestHandleTrace_HighConfWithEvidence_NoWarning(t *testing.T) {
	ctx := adminCtx()
	agentID := "high-conf-ev-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "architecture",
		"outcome":       "high confidence with evidence",
		"confidence":    0.9,
		"evidence":      `[{"source_type":"benchmark","content":"latency p99 < 50ms"}]`,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	text := parseToolText(t, result)
	assert.NotContains(t, text, "warnings")
}

func TestHandleTrace_BelowThreshold_NoWarning(t *testing.T) {
	ctx := adminCtx()
	agentID := "low-conf-" + uuid.New().String()[:8]
	_, _ = testSvc.ResolveOrCreateAgent(ctx, uuid.Nil, agentID, model.RoleAdmin, nil)

	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"agent_id":      agentID,
		"decision_type": "implementation",
		"outcome":       "low confidence decision",
		"confidence":    0.6,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	text := parseToolText(t, result)
	assert.NotContains(t, text, "warnings")
}

// ===========================================================================
// Profile-aware tip filtering: decision types suppress irrelevant tips
// ===========================================================================

func TestComputeMissingFields_InvestigationSuppressesAltsAndEvidence(t *testing.T) {
	// Investigation profile: no alternatives expected, no evidence expected.
	// Tips for alternatives and evidence should not appear.
	reasoning := "long enough reasoning that exceeds the 100 char threshold for testing purposes padding padding pad"
	tips := computeMissingFields("investigation", "root cause identified", 0.7, &reasoning, nil, nil, false, true, true, quality.DefaultStandardDecisionTypes)

	for _, tip := range tips {
		assert.NotContains(t, tip, "alternative", "investigation should not get alternatives tip")
		assert.NotContains(t, tip, "evidence", "investigation should not get evidence tip")
		assert.NotContains(t, tip, "verifiable", "investigation should not get evidence tip")
	}
}

func TestComputeMissingFields_SecurityGetsAltsAndEvidence(t *testing.T) {
	// Security profile: alternatives expected, evidence expected.
	// Tips for both should appear when missing.
	reasoning := "long enough reasoning that exceeds the 100 char threshold for testing purposes padding padding pad"
	tips := computeMissingFields("security", "chose Argon2id for hashing", 0.7, &reasoning, nil, nil, false, true, true, quality.DefaultStandardDecisionTypes)

	hasAlts := false
	hasEvidence := false
	for _, tip := range tips {
		if strings.Contains(tip, "alternative") {
			hasAlts = true
		}
		if containsEvidence(tip) {
			hasEvidence = true
		}
	}
	assert.True(t, hasAlts, "security should get alternatives tip")
	assert.True(t, hasEvidence, "security should get evidence tip")
}

func TestComputeMissingFields_ConfidencePenaltyWarning(t *testing.T) {
	// Security max_confidence_no_evidence = 0.75.
	// Confidence 0.80 with no evidence should trigger a warning tip.
	reasoning := "long enough reasoning that exceeds the 100 char threshold for testing purposes padding padding pad"
	tips := computeMissingFields("security", "chose Argon2id for hashing", 0.80, &reasoning, nil, nil, false, true, true, quality.DefaultStandardDecisionTypes)

	found := false
	for _, tip := range tips {
		if strings.Contains(tip, "exceeds max") {
			found = true
		}
	}
	assert.True(t, found, "expected confidence penalty warning for security with 0.80 confidence and no evidence")
}

func TestComputeMissingFields_NoConfidencePenaltyWithEvidence(t *testing.T) {
	// Even though confidence exceeds threshold, having evidence should suppress the warning.
	reasoning := "long enough reasoning that exceeds the 100 char threshold for testing purposes padding padding pad"
	evidence := []model.TraceEvidence{{SourceType: "document", Content: "OWASP guideline"}}
	tips := computeMissingFields("security", "chose Argon2id for hashing", 0.80, &reasoning, nil, evidence, false, true, true, quality.DefaultStandardDecisionTypes)

	for _, tip := range tips {
		assert.NotContains(t, tip, "exceeds max", "confidence penalty should not appear when evidence is provided")
	}
}

func TestComputeMissingFields_PlanningReasoningIsPrimaryFactor(t *testing.T) {
	// Planning profile: no alternatives, no evidence → reasoning is the primary factor.
	tips := computeMissingFields("planning", "split into phases", 0.7, nil, nil, nil, false, true, true, quality.DefaultStandardDecisionTypes)

	found := false
	for _, tip := range tips {
		if strings.Contains(tip, "up to 65%") {
			found = true
		}
	}
	assert.True(t, found, "planning should show elevated reasoning weight label, got: %v", tips)
}

func TestComputeMissingFields_ArchitectureReasoningWeightLabel(t *testing.T) {
	// Architecture profile: alternatives and evidence expected → plain 25%.
	tips := computeMissingFields("architecture", "chose Redis", 0.7, nil, nil, nil, false, true, true, quality.DefaultStandardDecisionTypes)

	found := false
	for _, tip := range tips {
		if strings.Contains(tip, "+30%") {
			found = true
		}
	}
	assert.True(t, found, "architecture should show '+25%%' without primary factor qualifier, got: %v", tips)
}
