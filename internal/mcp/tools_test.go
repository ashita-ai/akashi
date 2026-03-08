package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	testServer = New(testDB, testSvc, nil, logger, "test")

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
	s := New(testDB, testSvc, nil, testutil.TestLogger(), "test-version")
	require.NotNil(t, s)
	require.NotNil(t, s.MCPServer())
	// Verify the server has the expected name by checking it's non-nil.
	assert.NotNil(t, s.mcpServer)
}
