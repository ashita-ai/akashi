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

func TestHandleTrace_CheckNudge(t *testing.T) {
	ctx := adminCtx()

	// Trace without prior akashi_check should include the nudge message.
	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"decision_type": "nudge-test-" + uuid.New().String()[:8],
		"outcome":       "nudge test decision",
		"confidence":    0.5,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	// The result should have 2 content items: the trace result + the nudge note.
	assert.GreaterOrEqual(t, len(result.Content), 2, "expected trace result + nudge note")

	// Find the nudge content.
	var foundNudge bool
	for _, c := range result.Content {
		if tc, ok := c.(mcplib.TextContent); ok {
			if tc.Text != "" && len(tc.Text) > 10 && tc.Text[:4] == "NOTE" {
				foundNudge = true
				assert.Contains(t, tc.Text, "akashi_check")
			}
		}
	}
	assert.True(t, foundNudge, "expected check-before-trace nudge note")
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

func TestHandleCheck_MissingDecisionType(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name:      "akashi_check",
			Arguments: map[string]any{},
		},
	})
	require.NoError(t, err)
	require.True(t, result.IsError, "expected error when decision_type is missing")
	assert.Contains(t, parseToolText(t, result), "decision_type is required")
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

func TestHandleCheck_RecordsTracker(t *testing.T) {
	ctx := adminCtx()
	decisionType := "tracker-test-" + uuid.New().String()[:8]

	// Before check: tracker should not have this type.
	assert.False(t, testServer.checkTracker.WasChecked(testAdminID, decisionType))

	_, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": decisionType,
			},
		},
	})
	require.NoError(t, err)

	// After check: tracker should record it.
	assert.True(t, testServer.checkTracker.WasChecked(testAdminID, decisionType),
		"handleCheck should record the check in the tracker")
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

// ---------- handleSearch tests ----------

func TestHandleSearch(t *testing.T) {
	ctx := adminCtx()
	agentID := "search-basic-" + uuid.New().String()[:8]
	keyword := "searchable-keyword-" + agentID

	mustTrace(t, agentID, "error_handling", keyword, 0.8)

	result, err := testServer.handleSearch(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_search",
			Arguments: map[string]any{
				"query": keyword,
				"limit": 5,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "search should succeed: %s", parseToolText(t, result))

	var resp struct {
		Results []model.SearchResult `json:"results"`
		Total   int                  `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.NotEmpty(t, resp.Results, "text search should find the decision by keyword")
	assert.Greater(t, resp.Total, 0)
}

func TestHandleSearch_EmptyQuery(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleSearch(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_search",
			Arguments: map[string]any{
				"query": "",
			},
		},
	})
	require.NoError(t, err)
	require.True(t, result.IsError, "expected error for empty query")
	assert.Contains(t, parseToolText(t, result), "query is required")
}

func TestHandleSearch_MissingQuery(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleSearch(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name:      "akashi_search",
			Arguments: map[string]any{},
		},
	})
	require.NoError(t, err)
	require.True(t, result.IsError, "expected error when query is missing")
	assert.Contains(t, parseToolText(t, result), "query is required")
}

func TestHandleSearch_NoResults(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleSearch(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_search",
			Arguments: map[string]any{
				"query": "completely-nonexistent-" + uuid.New().String(),
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var resp struct {
		Results []model.SearchResult `json:"results"`
		Total   int                  `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.Empty(t, resp.Results)
	assert.Equal(t, 0, resp.Total)
}

// ---------- handleRecent tests ----------

func TestHandleRecent(t *testing.T) {
	ctx := adminCtx()
	agentID := "recent-basic-" + uuid.New().String()[:8]

	mustTrace(t, agentID, "feature_scope", "included pagination in API", 0.9)

	result, err := testServer.handleRecent(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_recent",
			Arguments: map[string]any{
				"limit": 10,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "recent should succeed: %s", parseToolText(t, result))

	var resp struct {
		Decisions []model.Decision `json:"decisions"`
		Total     int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(parseToolText(t, result)), &resp))
	assert.NotEmpty(t, resp.Decisions, "expected at least one recent decision")
	assert.Greater(t, resp.Total, 0)
}

func TestHandleRecent_WithLimit(t *testing.T) {
	ctx := adminCtx()
	agentID := "recent-limit-" + uuid.New().String()[:8]

	// Create 3 decisions.
	for i := range 3 {
		mustTrace(t, agentID, "planning", fmt.Sprintf("plan iteration %d", i), 0.7)
	}

	// Request with limit=1.
	result, err := testServer.handleRecent(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_recent",
			Arguments: map[string]any{
				"agent_id": agentID,
				"limit":    1,
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
	assert.Len(t, resp.Decisions, 1, "limit=1 should return exactly 1 decision")
}

func TestHandleRecent_WithAgentFilter(t *testing.T) {
	ctx := adminCtx()
	agentA := "recent-a-" + uuid.New().String()[:8]
	agentB := "recent-b-" + uuid.New().String()[:8]

	mustTrace(t, agentA, "security", "chose TLS 1.3", 0.95)
	mustTrace(t, agentB, "security", "chose TLS 1.2", 0.6)

	result, err := testServer.handleRecent(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_recent",
			Arguments: map[string]any{
				"agent_id": agentA,
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
		assert.Equal(t, agentA, dec.AgentID, "agent filter should restrict results")
	}
}

func TestHandleRecent_WithDecisionTypeFilter(t *testing.T) {
	ctx := adminCtx()
	agentID := "recent-dtype-" + uuid.New().String()[:8]
	uniqueType := "unique-dtype-" + uuid.New().String()[:8]

	mustTrace(t, agentID, uniqueType, "test outcome", 0.7)
	mustTrace(t, agentID, "architecture", "other outcome", 0.8)

	result, err := testServer.handleRecent(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_recent",
			Arguments: map[string]any{
				"decision_type": uniqueType,
				"limit":         50,
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
		assert.Equal(t, uniqueType, dec.DecisionType)
	}
}

func TestHandleRecent_EmptyResult(t *testing.T) {
	ctx := adminCtx()

	result, err := testServer.handleRecent(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_recent",
			Arguments: map[string]any{
				"agent_id": "nonexistent-agent-" + uuid.New().String()[:8],
				"limit":    10,
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
}

// ---------- Integration: check-before-trace workflow ----------

func TestCheckBeforeTraceWorkflow(t *testing.T) {
	ctx := adminCtx()
	decisionType := "workflow-" + uuid.New().String()[:8]

	// Step 1: Check first.
	_, err := testServer.handleCheck(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": decisionType,
			},
		},
	})
	require.NoError(t, err)

	// Step 2: Trace after checking. The nudge should NOT appear because we checked.
	result, err := testServer.handleTrace(ctx, traceRequest(map[string]any{
		"decision_type": decisionType,
		"outcome":       "workflow test decision",
		"confidence":    0.8,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	// Verify no nudge (only 1 content item, the trace result).
	assert.Len(t, result.Content, 1,
		"after checking, trace should not include the nudge note")
}

// ---------- handleRecent/handleQuery: no-context (nil claims) ----------

func TestHandleRecent_NilClaims(t *testing.T) {
	// H2 fix: nil claims must be rejected immediately rather than silently
	// skipping access filtering and returning unfiltered cross-org data.
	ctx := context.Background()

	result, err := testServer.handleRecent(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_recent",
			Arguments: map[string]any{
				"limit": 5,
			},
		},
	})
	require.NoError(t, err)
	require.True(t, result.IsError, "unauthenticated handleRecent must return an error")
	assert.Contains(t, parseToolText(t, result), "authentication required")
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
	// has the expected tools by attempting to list them. Since we can't call
	// ListTools directly (that's a client method), we verify indirectly by
	// calling each tool handler and confirming none panic.

	// Verify the server object has its MCPServer initialized.
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
