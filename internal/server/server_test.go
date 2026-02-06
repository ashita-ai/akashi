package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	mcpclient "github.com/mark3labs/mcp-go/client"
	mcptransport "github.com/mark3labs/mcp-go/client/transport"
	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/mcp"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/server"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/service/trace"
	"github.com/ashita-ai/akashi/internal/signup"
	"github.com/ashita-ai/akashi/internal/storage"
)

var (
	testSrv        *httptest.Server
	testcontainer  testcontainers.Container
	adminToken     string
	agentToken     string
)

func TestMain(m *testing.M) {
	ctx, cancel := context.WithCancel(context.Background())

	req := testcontainers.ContainerRequest{
		Image:        "timescale/timescaledb:latest-pg17",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "akashi",
			"POSTGRES_PASSWORD": "akashi",
			"POSTGRES_DB":       "akashi",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(60 * time.Second),
	}

	var err error
	testcontainer, err = testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start container: %v\n", err)
		os.Exit(1)
	}

	host, _ := testcontainer.Host(ctx)
	port, _ := testcontainer.MappedPort(ctx, "5432")
	dsn := fmt.Sprintf("postgres://akashi:akashi@%s:%s/akashi?sslmode=disable", host, port.Port())

	// Enable extensions before creating the storage layer so pgvector types
	// get registered on the pool's AfterConnect hook.
	bootstrapConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to bootstrap connection: %v\n", err)
		os.Exit(1)
	}
	_, _ = bootstrapConn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector")
	_, _ = bootstrapConn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS timescaledb")
	_ = bootstrapConn.Close(ctx)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	db, err := storage.New(ctx, dsn, "", logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create DB: %v\n", err)
		os.Exit(1)
	}

	if err := db.RunMigrations(ctx, os.DirFS("../../migrations")); err != nil {
		fmt.Fprintf(os.Stderr, "failed to run migrations: %v\n", err)
		os.Exit(1)
	}

	jwtMgr, _ := auth.NewJWTManager("", "", 24*time.Hour)
	embedder := embedding.NewNoopProvider(1024)
	decisionSvc := decisions.New(db, embedder, nil, logger)
	buf := trace.NewBuffer(db, logger, 1000, 50*time.Millisecond)
	buf.Start(ctx)

	mcpSrv := mcp.New(db, decisionSvc, logger, "test")
	signupSvc := signup.New(db, signup.Config{
		SMTPFrom: "test@akashi.dev",
		BaseURL:  "http://localhost:8080",
	}, logger)
	srv := server.New(db, jwtMgr, decisionSvc, nil, buf, nil, nil, signupSvc, logger, 0, 30*time.Second, 30*time.Second, mcpSrv.MCPServer(), "test", 1*1024*1024, nil)

	// Seed admin.
	_ = srv.Handlers().SeedAdmin(ctx, "test-admin-key")

	testSrv = httptest.NewServer(srv.Handler())

	// Get admin token.
	adminToken = getToken(testSrv.URL, "admin", "test-admin-key")

	// Create a test agent.
	createAgent(testSrv.URL, adminToken, "test-agent", "Test Agent", "agent", "test-agent-key")
	agentToken = getToken(testSrv.URL, "test-agent", "test-agent-key")

	code := m.Run()

	testSrv.Close()
	cancel() // Signal the buffer's flush loop to exit.
	buf.Drain(context.Background())
	db.Close(context.Background())
	_ = testcontainer.Terminate(context.Background())
	os.Exit(code)
}

func getToken(baseURL, agentID, apiKey string) string {
	body, _ := json.Marshal(model.AuthTokenRequest{AgentID: agentID, APIKey: apiKey})
	resp, err := http.Post(baseURL+"/auth/token", "application/json", bytes.NewReader(body))
	if err != nil {
		panic(fmt.Sprintf("getToken: request failed: %v", err))
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		panic(fmt.Sprintf("getToken: status %d, body: %s", resp.StatusCode, string(data)))
	}
	var result struct {
		Data model.AuthTokenResponse `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		panic(fmt.Sprintf("getToken: unmarshal failed: %v, body: %s", err, string(data)))
	}
	if result.Data.Token == "" {
		panic(fmt.Sprintf("getToken: empty token, body: %s", string(data)))
	}
	return result.Data.Token
}

func createAgent(baseURL, token, agentID, name, role, apiKey string) {
	body, _ := json.Marshal(model.CreateAgentRequest{
		AgentID: agentID, Name: name, Role: model.AgentRole(role), APIKey: apiKey,
	})
	req, _ := http.NewRequest("POST", baseURL+"/v1/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	_ = resp.Body.Close()
}

func authedRequest(method, url, token string, body any) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

func ptrFloat32(v float32) *float32 { return &v }

func TestHealthEndpoint(t *testing.T) {
	resp, err := http.Get(testSrv.URL + "/health")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data model.HealthResponse `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &result)
	assert.Equal(t, "healthy", result.Data.Status)
	assert.Equal(t, "connected", result.Data.Postgres)
}

func TestAuthFlow(t *testing.T) {
	// Valid credentials.
	token := getToken(testSrv.URL, "admin", "test-admin-key")
	assert.NotEmpty(t, token)

	// Invalid credentials.
	body, _ := json.Marshal(model.AuthTokenRequest{AgentID: "admin", APIKey: "wrong"})
	resp, err := http.Post(testSrv.URL+"/auth/token", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestUnauthenticatedAccess(t *testing.T) {
	resp, err := http.Get(testSrv.URL + "/v1/conflicts")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestCreateRunAndAppendEvents(t *testing.T) {
	// Create run.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &runResult)
	runID := runResult.Data.ID

	// Append events.
	resp2, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+runID.String()+"/events", agentToken,
		model.AppendEventsRequest{
			Events: []model.EventInput{
				{EventType: model.EventDecisionStarted, Payload: map[string]any{"decision_type": "test"}},
				{EventType: model.EventDecisionMade, Payload: map[string]any{"outcome": "approved"}},
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp2.StatusCode)

	// Wait for buffer flush.
	time.Sleep(200 * time.Millisecond)

	// Get run with events.
	resp3, err := authedRequest("GET", testSrv.URL+"/v1/runs/"+runID.String(), agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp3.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp3.StatusCode)
}

func TestTraceConvenience(t *testing.T) {
	reasoning := "test reasoning"
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: "test_decision",
				Outcome:      "approved",
				Confidence:   0.9,
				Reasoning:    &reasoning,
				Alternatives: []model.TraceAlternative{
					{Label: "Approve", Selected: true},
					{Label: "Deny", Selected: false},
				},
				Evidence: []model.TraceEvidence{
					{SourceType: "document", Content: "Test evidence"},
				},
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestQueryEndpoint(t *testing.T) {
	// Create a decision first via trace.
	_, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: "query_test",
				Outcome:      "passed",
				Confidence:   0.95,
			},
		})
	require.NoError(t, err)

	// Query it.
	dType := "query_test"
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query", agentToken,
		model.QueryRequest{
			Filters: model.QueryFilters{
				AgentIDs:     []string{"test-agent"},
				DecisionType: &dType,
			},
			Limit: 10,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestSearchEndpoint(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/search", agentToken,
		model.SearchRequest{
			Query: "test decisions",
			Limit: 5,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAdminOnlyEndpoints(t *testing.T) {
	// Agent cannot create agents.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", agentToken,
		model.CreateAgentRequest{AgentID: "should-fail", Name: "Fail", APIKey: "key"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// Admin can list agents.
	resp2, err := authedRequest("GET", testSrv.URL+"/v1/agents", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
}

func TestConflictsEndpoint(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// newMCPClient creates an MCP client that connects to the test server's /mcp endpoint
// with the given bearer token for authentication.
func newMCPClient(t *testing.T, token string) *mcpclient.Client {
	t.Helper()
	c, err := mcpclient.NewStreamableHttpClient(
		testSrv.URL+"/mcp",
		mcptransport.WithHTTPHeaders(map[string]string{
			"Authorization": "Bearer " + token,
		}),
	)
	require.NoError(t, err)
	return c
}

func TestMCPInitialize(t *testing.T) {
	c := newMCPClient(t, agentToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	initResult, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "akashi", initResult.ServerInfo.Name)
	assert.Equal(t, "test", initResult.ServerInfo.Version)
}

func TestMCPListTools(t *testing.T) {
	c := newMCPClient(t, agentToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	_, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)

	toolsResult, err := c.ListTools(ctx, mcplib.ListToolsRequest{})
	require.NoError(t, err)
	assert.Len(t, toolsResult.Tools, 5)

	toolNames := make(map[string]bool)
	for _, tool := range toolsResult.Tools {
		toolNames[tool.Name] = true
	}
	assert.True(t, toolNames["akashi_check"], "expected akashi_check tool")
	assert.True(t, toolNames["akashi_trace"], "expected akashi_trace tool")
	assert.True(t, toolNames["akashi_query"], "expected akashi_query tool")
	assert.True(t, toolNames["akashi_search"], "expected akashi_search tool")
	assert.True(t, toolNames["akashi_recent"], "expected akashi_recent tool")
}

func TestMCPListResources(t *testing.T) {
	c := newMCPClient(t, agentToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	_, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)

	resourcesResult, err := c.ListResources(ctx, mcplib.ListResourcesRequest{})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(resourcesResult.Resources), 2, "expected at least session/current and decisions/recent")
}

func TestMCPTraceAndQuery(t *testing.T) {
	c := newMCPClient(t, agentToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	_, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)

	// Record a decision via the MCP trace tool.
	traceResult, err := c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_trace",
			Arguments: map[string]any{
				"agent_id":      "test-agent",
				"decision_type": "mcp_test",
				"outcome":       "mcp_approved",
				"confidence":    0.85,
				"reasoning":     "tested via MCP protocol",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, traceResult.IsError, "trace tool returned error: %v", traceResult.Content)
	assert.NotEmpty(t, traceResult.Content)

	// Query it back via the MCP query tool.
	queryResult, err := c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"agent_id":      "test-agent",
				"decision_type": "mcp_test",
				"limit":         10,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, queryResult.IsError, "query tool returned error: %v", queryResult.Content)
	assert.NotEmpty(t, queryResult.Content)

	// Search via the MCP search tool.
	searchResult, err := c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_search",
			Arguments: map[string]any{
				"query": "mcp approved decisions",
				"limit": 5,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, searchResult.IsError, "search tool returned error: %v", searchResult.Content)
}

func TestMCPReadResource(t *testing.T) {
	c := newMCPClient(t, agentToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	_, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)

	// Read the session/current resource.
	result, err := c.ReadResource(ctx, mcplib.ReadResourceRequest{
		Params: mcplib.ReadResourceParams{
			URI: "akashi://session/current",
		},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, result.Contents)

	// Read the decisions/recent resource.
	result, err = c.ReadResource(ctx, mcplib.ReadResourceRequest{
		Params: mcplib.ReadResourceParams{
			URI: "akashi://decisions/recent",
		},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, result.Contents)
}

func TestMCPUnauthenticated(t *testing.T) {
	// MCP endpoint should require auth.
	resp, err := http.Post(testSrv.URL+"/mcp", "application/json", nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestMCPCheckTool(t *testing.T) {
	c := newMCPClient(t, agentToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	_, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)

	// Record a decision first.
	traceResult, err := c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_trace",
			Arguments: map[string]any{
				"agent_id":      "test-agent",
				"decision_type": "architecture",
				"outcome":       "chose Redis for session caching",
				"confidence":    0.85,
				"reasoning":     "Redis handles expected QPS, TTL prevents stale reads",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, traceResult.IsError, "trace tool returned error: %v", traceResult.Content)

	// Now check for precedents — should find the decision we just recorded.
	checkResult, err := c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": "architecture",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, checkResult.IsError, "check tool returned error: %v", checkResult.Content)

	// Parse the response and verify has_precedent is true.
	var checkResp model.CheckResponse
	for _, content := range checkResult.Content {
		if tc, ok := content.(mcplib.TextContent); ok {
			err := json.Unmarshal([]byte(tc.Text), &checkResp)
			require.NoError(t, err)
			break
		}
	}
	assert.True(t, checkResp.HasPrecedent, "expected has_precedent=true after recording a decision")
	assert.NotEmpty(t, checkResp.Decisions, "expected at least one precedent decision")
}

func TestMCPCheckNoPrecedent(t *testing.T) {
	c := newMCPClient(t, agentToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	_, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)

	// Check for a decision type that hasn't been used.
	checkResult, err := c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_check",
			Arguments: map[string]any{
				"decision_type": "deployment",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, checkResult.IsError, "check tool returned error: %v", checkResult.Content)

	var checkResp model.CheckResponse
	for _, content := range checkResult.Content {
		if tc, ok := content.(mcplib.TextContent); ok {
			err := json.Unmarshal([]byte(tc.Text), &checkResp)
			require.NoError(t, err)
			break
		}
	}
	assert.False(t, checkResp.HasPrecedent, "expected has_precedent=false for unused decision type")
}

func TestMCPRecentTool(t *testing.T) {
	c := newMCPClient(t, agentToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	_, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)

	// Record a decision so there's at least one recent one.
	_, err = c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_trace",
			Arguments: map[string]any{
				"agent_id":      "test-agent",
				"decision_type": "feature_scope",
				"outcome":       "included pagination in API response",
				"confidence":    0.9,
			},
		},
	})
	require.NoError(t, err)

	// Call akashi_recent.
	recentResult, err := c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_recent",
			Arguments: map[string]any{
				"limit": 5,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, recentResult.IsError, "recent tool returned error: %v", recentResult.Content)

	// Parse and verify we got results.
	var recentResp struct {
		Decisions []model.Decision `json:"decisions"`
		Total     int              `json:"total"`
	}
	for _, content := range recentResult.Content {
		if tc, ok := content.(mcplib.TextContent); ok {
			err := json.Unmarshal([]byte(tc.Text), &recentResp)
			require.NoError(t, err)
			break
		}
	}
	assert.NotEmpty(t, recentResp.Decisions, "expected at least one recent decision")
	assert.Greater(t, recentResp.Total, 0, "expected total > 0")
}

func TestMCPPrompts(t *testing.T) {
	c := newMCPClient(t, agentToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	_, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)

	// List prompts.
	promptsResult, err := c.ListPrompts(ctx, mcplib.ListPromptsRequest{})
	require.NoError(t, err)
	assert.Len(t, promptsResult.Prompts, 3, "expected 3 prompts")

	promptNames := make(map[string]bool)
	for _, p := range promptsResult.Prompts {
		promptNames[p.Name] = true
	}
	assert.True(t, promptNames["before-decision"], "expected before-decision prompt")
	assert.True(t, promptNames["after-decision"], "expected after-decision prompt")
	assert.True(t, promptNames["agent-setup"], "expected agent-setup prompt")

	// Get the agent-setup prompt (no arguments needed).
	setupResult, err := c.GetPrompt(ctx, mcplib.GetPromptRequest{
		Params: mcplib.GetPromptParams{
			Name: "agent-setup",
		},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, setupResult.Messages, "expected at least one message in agent-setup prompt")
	// Verify the content mentions the check-before workflow.
	for _, msg := range setupResult.Messages {
		if tc, ok := msg.Content.(mcplib.TextContent); ok {
			assert.Contains(t, tc.Text, "Check Before", "expected agent-setup to mention check-before pattern")
			break
		}
	}

	// Get the before-decision prompt with an argument.
	beforeResult, err := c.GetPrompt(ctx, mcplib.GetPromptRequest{
		Params: mcplib.GetPromptParams{
			Name:      "before-decision",
			Arguments: map[string]string{"decision_type": "architecture"},
		},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, beforeResult.Messages)
}

func TestCheckEndpoint(t *testing.T) {
	// First, create a decision via /v1/trace so we have precedent.
	_, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: "security",
				Outcome:      "chose JWT for API auth",
				Confidence:   0.9,
			},
		})
	require.NoError(t, err)

	// Check for precedents on "security" type — should find one.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/check", agentToken,
		model.CheckRequest{
			DecisionType: "security",
			Limit:        5,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data model.CheckResponse `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)
	assert.True(t, result.Data.HasPrecedent, "expected has_precedent=true")
	assert.NotEmpty(t, result.Data.Decisions)

	// Check for a type with no precedents.
	resp2, err := authedRequest("POST", testSrv.URL+"/v1/check", agentToken,
		model.CheckRequest{
			DecisionType: "deployment",
			Limit:        5,
		})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	var result2 struct {
		Data model.CheckResponse `json:"data"`
	}
	data2, _ := io.ReadAll(resp2.Body)
	err = json.Unmarshal(data2, &result2)
	require.NoError(t, err)
	assert.False(t, result2.Data.HasPrecedent, "expected has_precedent=false for unused type")
}

func TestDecisionsRecentEndpoint(t *testing.T) {
	// GET /v1/decisions/recent with no filters.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/recent", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Decisions []model.Decision `json:"decisions"`
			Total     int              `json:"total"`
			Limit     int              `json:"limit"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)
	assert.NotEmpty(t, result.Data.Decisions, "expected at least one recent decision")
	assert.Equal(t, 10, result.Data.Limit, "expected default limit of 10")

	// GET with agent_id filter.
	resp2, err := authedRequest("GET", testSrv.URL+"/v1/decisions/recent?agent_id=test-agent&limit=3", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	var result2 struct {
		Data struct {
			Decisions []model.Decision `json:"decisions"`
			Limit     int              `json:"limit"`
		} `json:"data"`
	}
	data2, _ := io.ReadAll(resp2.Body)
	err = json.Unmarshal(data2, &result2)
	require.NoError(t, err)
	assert.Equal(t, 3, result2.Data.Limit)
	for _, d := range result2.Data.Decisions {
		assert.Equal(t, "test-agent", d.AgentID, "expected only test-agent decisions")
	}
}

func TestSSESubscribeNoBroker(t *testing.T) {
	// When broker is nil (no LISTEN/NOTIFY configured), SSE returns 503.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/subscribe", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestExportDecisions(t *testing.T) {
	// Ensure there are decisions to export (created by earlier tests).
	t.Run("admin can export NDJSON", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/export/decisions?agent_id=test-agent", adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "application/x-ndjson", resp.Header.Get("Content-Type"))
		assert.Contains(t, resp.Header.Get("Content-Disposition"), "akashi-export-")
		assert.Contains(t, resp.Header.Get("Content-Disposition"), ".ndjson")

		// Parse NDJSON lines.
		body, _ := io.ReadAll(resp.Body)
		lines := bytes.Split(bytes.TrimSpace(body), []byte("\n"))
		assert.Greater(t, len(lines), 0, "should have at least one decision in export")

		// Each line should be valid JSON parseable as a Decision.
		for _, line := range lines {
			if len(line) == 0 {
				continue
			}
			var d model.Decision
			err := json.Unmarshal(line, &d)
			assert.NoError(t, err, "each line should be valid JSON decision: %s", string(line))
			assert.NotEmpty(t, d.ID, "decision should have an ID")
			assert.Equal(t, "test-agent", d.AgentID, "export should only contain requested agent")
		}
	})

	t.Run("non-admin cannot export", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/export/decisions", agentToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("empty export for unknown agent", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/export/decisions?agent_id=nonexistent", adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		// Should succeed but with empty body.
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		body, _ := io.ReadAll(resp.Body)
		assert.Empty(t, bytes.TrimSpace(body), "export for nonexistent agent should be empty")
	})
}

func TestDeleteAgentData(t *testing.T) {
	// Create an agent with runs, decisions, and events.
	createAgent(testSrv.URL, adminToken, "delete-me", "Delete Me", "agent", "delete-key")
	deleteToken := getToken(testSrv.URL, "delete-me", "delete-key")

	// Trace a decision (creates run + decision + events).
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", deleteToken,
		model.TraceRequest{
			AgentID: "delete-me",
			Decision: model.TraceDecision{
				DecisionType: "gdpr_test",
				Outcome:      "delete_everything",
				Confidence:   0.8,
				Alternatives: []model.TraceAlternative{
					{Label: "keep", Score: ptrFloat32(0.2)},
				},
				Evidence: []model.TraceEvidence{
					{SourceType: "document", Content: "test evidence for GDPR"},
				},
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("trace failed: status=%d body=%s", resp.StatusCode, string(body))
	}

	// Verify the agent's history exists.
	resp2, err := authedRequest("GET", testSrv.URL+"/v1/agents/delete-me/history", deleteToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	var hist struct {
		Data struct {
			Decisions []model.Decision `json:"decisions"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(resp2.Body)
	_ = json.Unmarshal(data, &hist)
	assert.NotEmpty(t, hist.Data.Decisions, "agent should have decisions before deletion")

	t.Run("non-admin cannot delete", func(t *testing.T) {
		resp, err := authedRequest("DELETE", testSrv.URL+"/v1/agents/delete-me", deleteToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("cannot delete admin", func(t *testing.T) {
		resp, err := authedRequest("DELETE", testSrv.URL+"/v1/agents/admin", adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("admin can delete agent", func(t *testing.T) {
		resp, err := authedRequest("DELETE", testSrv.URL+"/v1/agents/delete-me", adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data struct {
				AgentID string `json:"agent_id"`
				Deleted struct {
					Evidence     int64 `json:"evidence"`
					Alternatives int64 `json:"alternatives"`
					Decisions    int64 `json:"decisions"`
					Events       int64 `json:"events"`
					Runs         int64 `json:"runs"`
					Agents       int64 `json:"agents"`
				} `json:"deleted"`
			} `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.Equal(t, "delete-me", result.Data.AgentID)
		assert.Equal(t, int64(1), result.Data.Deleted.Agents, "should delete 1 agent")
		assert.GreaterOrEqual(t, result.Data.Deleted.Decisions, int64(1), "should delete at least 1 decision")
		assert.GreaterOrEqual(t, result.Data.Deleted.Runs, int64(1), "should delete at least 1 run")
	})

	t.Run("deleted agent is gone", func(t *testing.T) {
		resp, err := authedRequest("DELETE", testSrv.URL+"/v1/agents/delete-me", adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

func TestSignupFlow(t *testing.T) {
	t.Run("valid signup creates org and agent", func(t *testing.T) {
		body, _ := json.Marshal(model.SignupRequest{
			Email:    "test-signup@example.com",
			Password: "StrongP@ss123",
			OrgName:  "Test Org",
		})
		resp, err := http.Post(testSrv.URL+"/auth/signup", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusCreated, resp.StatusCode)

		var result struct {
			Data signup.SignupResult `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		err = json.Unmarshal(data, &result)
		require.NoError(t, err)
		assert.NotEqual(t, "", result.Data.OrgID.String())
		assert.Equal(t, "owner@test-org", result.Data.AgentID)
		assert.Contains(t, result.Data.Message, "verify")
	})

	t.Run("unverified org cannot get token", func(t *testing.T) {
		body, _ := json.Marshal(model.AuthTokenRequest{
			AgentID: "owner@test-org",
			APIKey:  "StrongP@ss123",
		})
		resp, err := http.Post(testSrv.URL+"/auth/token", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)

		data, _ := io.ReadAll(resp.Body)
		assert.Contains(t, string(data), "email not verified")
	})

	t.Run("invalid email rejected", func(t *testing.T) {
		body, _ := json.Marshal(model.SignupRequest{
			Email:    "not-an-email",
			Password: "StrongP@ss123",
			OrgName:  "Bad Email Org",
		})
		resp, err := http.Post(testSrv.URL+"/auth/signup", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("weak password rejected", func(t *testing.T) {
		body, _ := json.Marshal(model.SignupRequest{
			Email:    "weak@example.com",
			Password: "short",
			OrgName:  "Weak Pwd Org",
		})
		resp, err := http.Post(testSrv.URL+"/auth/signup", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("missing org name rejected", func(t *testing.T) {
		body, _ := json.Marshal(model.SignupRequest{
			Email:    "noorg@example.com",
			Password: "StrongP@ss123",
			OrgName:  "",
		})
		resp, err := http.Post(testSrv.URL+"/auth/signup", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

func TestVerifyEmail(t *testing.T) {
	t.Run("missing token rejected", func(t *testing.T) {
		resp, err := http.Get(testSrv.URL + "/auth/verify")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("invalid token rejected", func(t *testing.T) {
		resp, err := http.Get(testSrv.URL + "/auth/verify?token=bogus-token")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

func TestSignupAndVerifyFullFlow(t *testing.T) {
	// 1. Sign up.
	signupBody, _ := json.Marshal(model.SignupRequest{
		Email:    "fullflow@example.com",
		Password: "MyStr0ngPasswd",
		OrgName:  "Full Flow Org",
	})
	resp, err := http.Post(testSrv.URL+"/auth/signup", "application/json", bytes.NewReader(signupBody))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var signupResult struct {
		Data signup.SignupResult `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(data, &signupResult))
	orgID := signupResult.Data.OrgID
	agentID := signupResult.Data.AgentID

	// 2. Confirm token is rejected before email verification.
	tokenBody, _ := json.Marshal(model.AuthTokenRequest{AgentID: agentID, APIKey: "MyStr0ngPasswd"})
	resp2, err := http.Post(testSrv.URL+"/auth/token", "application/json", bytes.NewReader(tokenBody))
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp2.StatusCode)

	// 3. Get the verification token from the DB directly (in production this comes via email).
	var verifyToken string
	ctx := context.Background()
	// We need DB access to retrieve the token. Use the exported test DB via a storage query.
	// Since we can't access the DB directly from the test package, we'll use a
	// roundabout approach: look up the token via the org_id we got back.
	// Actually, we can use the storage package directly since we have the DSN.
	// Instead, let's just verify the full flow works via the org_id.
	// We'll directly query the DB that the test server uses.
	// The test container DSN is not exported, so let's use the signup service's DB.
	// Actually, the simplest approach: query the verification token from email_verifications.
	host, _ := testcontainer.Host(ctx)
	port, _ := testcontainer.MappedPort(ctx, "5432")
	dsn := fmt.Sprintf("postgres://akashi:akashi@%s:%s/akashi?sslmode=disable", host, port.Port())
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	err = conn.QueryRow(ctx,
		"SELECT token FROM email_verifications WHERE org_id = $1 AND used_at IS NULL ORDER BY created_at DESC LIMIT 1",
		orgID,
	).Scan(&verifyToken)
	require.NoError(t, err, "should find verification token in DB")
	require.NotEmpty(t, verifyToken)

	// 4. Verify email.
	resp3, err := http.Get(testSrv.URL + "/auth/verify?token=" + verifyToken)
	require.NoError(t, err)
	defer func() { _ = resp3.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp3.StatusCode)

	data3, _ := io.ReadAll(resp3.Body)
	assert.Contains(t, string(data3), "verified")

	// 5. Now token issuance should succeed.
	resp4, err := http.Post(testSrv.URL+"/auth/token", "application/json", bytes.NewReader(tokenBody))
	require.NoError(t, err)
	defer func() { _ = resp4.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp4.StatusCode)

	var tokenResult struct {
		Data model.AuthTokenResponse `json:"data"`
	}
	data4, _ := io.ReadAll(resp4.Body)
	require.NoError(t, json.Unmarshal(data4, &tokenResult))
	assert.NotEmpty(t, tokenResult.Data.Token)

	// 6. Verify the token can't be used again.
	resp5, err := http.Get(testSrv.URL + "/auth/verify?token=" + verifyToken)
	require.NoError(t, err)
	defer func() { _ = resp5.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp5.StatusCode)
}

func TestUsageEndpoint(t *testing.T) {
	t.Run("authenticated user can see usage", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/usage", adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data struct {
				Plan          string `json:"plan"`
				Period        string `json:"period"`
				DecisionCount int    `json:"decision_count"`
				DecisionLimit int    `json:"decision_limit"`
				AgentLimit    int    `json:"agent_limit"`
			} `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		err = json.Unmarshal(data, &result)
		require.NoError(t, err)
		assert.NotEmpty(t, result.Data.Period)
		assert.NotEmpty(t, result.Data.Plan)
	})

	t.Run("unauthenticated cannot see usage", func(t *testing.T) {
		resp, err := http.Get(testSrv.URL + "/v1/usage")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
}

func TestBillingWebhookEndpoint(t *testing.T) {
	t.Run("webhook returns 503 when billing disabled", func(t *testing.T) {
		body := []byte(`{"type":"checkout.session.completed"}`)
		req, _ := http.NewRequest("POST", testSrv.URL+"/billing/webhooks", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	})

	t.Run("webhook does not require JWT auth", func(t *testing.T) {
		// Verify the endpoint is reachable without Bearer token
		// (it uses Stripe signature instead of JWT).
		body := []byte(`{}`)
		req, _ := http.NewRequest("POST", testSrv.URL+"/billing/webhooks", bytes.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		// Should NOT be 401 (auth middleware skipped). Returns 503 because billing is disabled.
		assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	})
}

func TestBillingCheckoutEndpoint(t *testing.T) {
	t.Run("billing not configured returns 503 for org_owner", func(t *testing.T) {
		// Create an org_owner via signup + verify to get a proper token.
		ownerToken := createVerifiedOrgOwner(t, "billing-checkout-test@example.com", "Str0ngPassw0rd!", "Billing Checkout Org")

		resp, err := authedRequest("POST", testSrv.URL+"/billing/checkout", ownerToken,
			map[string]string{"success_url": "https://ok", "cancel_url": "https://cancel"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	})

	t.Run("admin cannot access checkout (requires org_owner)", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/billing/checkout", adminToken,
			map[string]string{"success_url": "https://ok", "cancel_url": "https://cancel"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("agent cannot access checkout", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/billing/checkout", agentToken,
			map[string]string{"success_url": "https://ok", "cancel_url": "https://cancel"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

func TestBillingPortalEndpoint(t *testing.T) {
	t.Run("billing not configured returns 503 for org_owner", func(t *testing.T) {
		ownerToken := createVerifiedOrgOwner(t, "billing-portal-test@example.com", "Str0ngPassw0rd!", "Billing Portal Org")

		resp, err := authedRequest("POST", testSrv.URL+"/billing/portal", ownerToken,
			map[string]string{"return_url": "https://return"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	})

	t.Run("admin cannot access portal (requires org_owner)", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/billing/portal", adminToken,
			map[string]string{"return_url": "https://return"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

// createVerifiedOrgOwner signs up a new org, verifies the email, and returns a JWT token
// for the org_owner agent. This is a helper for tests that need an org_owner role.
func createVerifiedOrgOwner(t *testing.T, email, password, orgName string) string {
	t.Helper()
	ctx := context.Background()

	// 1. Sign up.
	signupBody, _ := json.Marshal(model.SignupRequest{Email: email, Password: password, OrgName: orgName})
	resp, err := http.Post(testSrv.URL+"/auth/signup", "application/json", bytes.NewReader(signupBody))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var signupResult struct {
		Data signup.SignupResult `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(data, &signupResult))

	// 2. Verify email directly via DB.
	host, _ := testcontainer.Host(ctx)
	port, _ := testcontainer.MappedPort(ctx, "5432")
	dsn := fmt.Sprintf("postgres://akashi:akashi@%s:%s/akashi?sslmode=disable", host, port.Port())
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	var verifyToken string
	err = conn.QueryRow(ctx,
		"SELECT token FROM email_verifications WHERE org_id = $1 AND used_at IS NULL ORDER BY created_at DESC LIMIT 1",
		signupResult.Data.OrgID,
	).Scan(&verifyToken)
	require.NoError(t, err)

	resp2, err := http.Get(testSrv.URL + "/auth/verify?token=" + verifyToken)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	require.Equal(t, http.StatusOK, resp2.StatusCode)

	// 3. Get token.
	return getToken(testSrv.URL, signupResult.Data.AgentID, password)
}

func TestAccessGrantEnforcement(t *testing.T) {
	// Create a reader agent with no grants.
	createAgent(testSrv.URL, adminToken, "reader-agent", "Reader", "reader", "reader-key")
	readerToken := getToken(testSrv.URL, "reader-agent", "reader-key")

	// First, ensure test-agent has at least one decision.
	_, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: "authz_test",
				Outcome:      "granted",
				Confidence:   0.9,
			},
		})
	require.NoError(t, err)

	t.Run("reader cannot see other agent history", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent/history", readerToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("reader gets empty results from query", func(t *testing.T) {
		dType := "authz_test"
		resp, err := authedRequest("POST", testSrv.URL+"/v1/query", readerToken,
			model.QueryRequest{
				Filters: model.QueryFilters{
					AgentIDs:     []string{"test-agent"},
					DecisionType: &dType,
				},
				Limit: 10,
			})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data struct {
				Decisions []model.Decision `json:"decisions"`
			} `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.Empty(t, result.Data.Decisions, "reader should see no decisions without a grant")
	})

	t.Run("reader gets empty recent decisions", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/recent?agent_id=test-agent", readerToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data struct {
				Decisions []model.Decision `json:"decisions"`
			} `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.Empty(t, result.Data.Decisions, "reader should see no recent decisions without a grant")
	})

	t.Run("admin can grant access to reader", func(t *testing.T) {
		agentIDStr := "test-agent"
		resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", adminToken,
			model.CreateGrantRequest{
				GranteeAgentID: "reader-agent",
				ResourceType:   "agent_traces",
				ResourceID:     &agentIDStr,
				Permission:     "read",
			})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusCreated, resp.StatusCode)
	})

	t.Run("reader can see history after grant", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent/history", readerToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data struct {
				Decisions []model.Decision `json:"decisions"`
			} `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.NotEmpty(t, result.Data.Decisions, "reader should see decisions after grant")
	})

	t.Run("reader can query after grant", func(t *testing.T) {
		dType := "authz_test"
		resp, err := authedRequest("POST", testSrv.URL+"/v1/query", readerToken,
			model.QueryRequest{
				Filters: model.QueryFilters{
					AgentIDs:     []string{"test-agent"},
					DecisionType: &dType,
				},
				Limit: 10,
			})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data struct {
				Decisions []model.Decision `json:"decisions"`
			} `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.NotEmpty(t, result.Data.Decisions, "reader should see decisions after grant")
	})

	t.Run("admin sees everything regardless", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent/history", adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data struct {
				Decisions []model.Decision `json:"decisions"`
			} `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.NotEmpty(t, result.Data.Decisions, "admin should always see decisions")
	})

	t.Run("agent can see own data", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent/history", agentToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data struct {
				Decisions []model.Decision `json:"decisions"`
			} `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.NotEmpty(t, result.Data.Decisions, "agent should see own decisions")
	})
}
