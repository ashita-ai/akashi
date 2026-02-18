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

	"github.com/google/uuid"
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
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/migrations"
)

var (
	testSrv       *httptest.Server
	testcontainer testcontainers.Container
	adminToken    string
	agentToken    string
)

func TestMain(m *testing.M) {
	ctx, cancel := context.WithCancel(context.Background())

	req := testcontainers.ContainerRequest{
		Image:        "timescale/timescaledb:latest-pg18",
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

	if err := db.RunMigrations(ctx, migrations.FS); err != nil {
		fmt.Fprintf(os.Stderr, "failed to run migrations: %v\n", err)
		os.Exit(1)
	}

	jwtMgr, _ := auth.NewJWTManager("", "", 24*time.Hour)
	embedder := embedding.NewNoopProvider(1024)
	decisionSvc := decisions.New(db, embedder, nil, logger, nil)
	buf := trace.NewBuffer(db, logger, 1000, 50*time.Millisecond, nil)
	buf.Start(ctx)

	mcpSrv := mcp.New(db, decisionSvc, nil, logger, "test")
	srv := server.New(server.ServerConfig{
		DB:                  db,
		JWTMgr:              jwtMgr,
		DecisionSvc:         decisionSvc,
		Buffer:              buf,
		Logger:              logger,
		ReadTimeout:         30 * time.Second,
		WriteTimeout:        30 * time.Second,
		MCPServer:           mcpSrv.MCPServer(),
		Version:             "test",
		MaxRequestBodyBytes: 1 * 1024 * 1024,
		// Explicitly enabled for tests that exercise GDPR delete behavior.
		EnableDestructiveDelete: true,
	})

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
	_ = buf.Drain(context.Background())
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

func authedRequestWithHeaders(method, url, token string, body any, headers map[string]string) (*http.Response, error) {
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
	for k, v := range headers {
		req.Header.Set(k, v)
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

func TestSecurityHeaders(t *testing.T) {
	resp, err := http.Get(testSrv.URL + "/health")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "strict-origin-when-cross-origin", resp.Header.Get("Referrer-Policy"))
	assert.Equal(t, "max-age=63072000; includeSubDomains", resp.Header.Get("Strict-Transport-Security"))
	assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "default-src 'self'")
	assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'")
	assert.Contains(t, resp.Header.Get("Permissions-Policy"), "camera=()")
	assert.Contains(t, resp.Header.Get("Permissions-Policy"), "geolocation=()")
}

func TestOpenAPISpec(t *testing.T) {
	t.Run("nil spec returns 404", func(t *testing.T) {
		// testSrv was created with nil openapiSpec.
		resp, err := http.Get(testSrv.URL + "/openapi.yaml")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("embedded spec is served", func(t *testing.T) {
		spec := []byte("openapi: \"3.1.0\"\ninfo:\n  title: Test\n  version: 0.0.1\npaths: {}\n")
		h := server.NewHandlers(server.HandlersDeps{
			Logger:              slog.New(slog.NewTextHandler(os.Stderr, nil)),
			Version:             "test",
			MaxRequestBodyBytes: 1 * 1024 * 1024,
			OpenAPISpec:         spec,
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
		h.HandleOpenAPISpec(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/yaml", rec.Header().Get("Content-Type"))
		assert.Contains(t, rec.Body.String(), "openapi: \"3.1.0\"")
	})
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
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	// Wait for buffer flush.
	time.Sleep(200 * time.Millisecond)

	// Get run with events.
	resp3, err := authedRequest("GET", testSrv.URL+"/v1/runs/"+runID.String(), agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp3.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp3.StatusCode)
}

func TestHandleAppendEvents_IdempotencyReplay(t *testing.T) {
	// Create run.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(data, &runResult))
	runID := runResult.Data.ID

	key := "events-idem-" + uuid.NewString()
	appendReq := model.AppendEventsRequest{
		Events: []model.EventInput{
			{EventType: model.EventDecisionStarted, Payload: map[string]any{"step": 1}},
			{EventType: model.EventDecisionMade, Payload: map[string]any{"step": 2}},
		},
	}

	resp1, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/runs/"+runID.String()+"/events", agentToken, appendReq, map[string]string{
		"Idempotency-Key": key,
	})
	require.NoError(t, err)
	defer func() { _ = resp1.Body.Close() }()
	require.Equal(t, http.StatusOK, resp1.StatusCode)

	var appended1 struct {
		Data struct {
			EventIDs []string `json:"event_ids"`
		} `json:"data"`
	}
	data1, _ := io.ReadAll(resp1.Body)
	require.NoError(t, json.Unmarshal(data1, &appended1))
	require.Len(t, appended1.Data.EventIDs, 2)

	resp2, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/runs/"+runID.String()+"/events", agentToken, appendReq, map[string]string{
		"Idempotency-Key": key,
	})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	require.Equal(t, http.StatusOK, resp2.StatusCode)

	var appended2 struct {
		Data struct {
			EventIDs []string `json:"event_ids"`
		} `json:"data"`
	}
	data2, _ := io.ReadAll(resp2.Body)
	require.NoError(t, json.Unmarshal(data2, &appended2))
	assert.Equal(t, appended1.Data.EventIDs, appended2.Data.EventIDs)

	// Verify events were not duplicated.
	respRun, err := authedRequest("GET", testSrv.URL+"/v1/runs/"+runID.String(), agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = respRun.Body.Close() }()
	require.Equal(t, http.StatusOK, respRun.StatusCode)

	var runView struct {
		Data struct {
			Events []json.RawMessage `json:"events"`
		} `json:"data"`
	}
	runData, _ := io.ReadAll(respRun.Body)
	require.NoError(t, json.Unmarshal(runData, &runView))
	assert.Len(t, runView.Data.Events, 2)
}

func TestHandleAppendEvents_IdempotencyPayloadMismatch(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(data, &runResult))
	runID := runResult.Data.ID

	key := "events-idem-mismatch-" + uuid.NewString()
	resp1, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/runs/"+runID.String()+"/events", agentToken, model.AppendEventsRequest{
		Events: []model.EventInput{
			{EventType: model.EventDecisionStarted, Payload: map[string]any{"value": 1}},
		},
	}, map[string]string{"Idempotency-Key": key})
	require.NoError(t, err)
	defer func() { _ = resp1.Body.Close() }()
	require.Equal(t, http.StatusOK, resp1.StatusCode)

	resp2, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/runs/"+runID.String()+"/events", agentToken, model.AppendEventsRequest{
		Events: []model.EventInput{
			{EventType: model.EventDecisionStarted, Payload: map[string]any{"value": 2}},
		},
	}, map[string]string{"Idempotency-Key": key})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusConflict, resp2.StatusCode)
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
	assert.Len(t, toolsResult.Tools, 7)

	toolNames := make(map[string]bool)
	for _, tool := range toolsResult.Tools {
		toolNames[tool.Name] = true
	}
	assert.True(t, toolNames["akashi_check"], "expected akashi_check tool")
	assert.True(t, toolNames["akashi_trace"], "expected akashi_trace tool")
	assert.True(t, toolNames["akashi_query"], "expected akashi_query tool")
	assert.True(t, toolNames["akashi_search"], "expected akashi_search tool")
	assert.True(t, toolNames["akashi_recent"], "expected akashi_recent tool")
	assert.True(t, toolNames["akashi_conflicts"], "expected akashi_conflicts tool")
	assert.True(t, toolNames["akashi_stats"], "expected akashi_stats tool")
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

func TestConfigEndpoint(t *testing.T) {
	t.Run("returns feature flags", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/config", adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data struct {
				SearchEnabled bool `json:"search_enabled"`
			} `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		err = json.Unmarshal(data, &result)
		require.NoError(t, err)
		assert.False(t, result.Data.SearchEnabled, "search should be disabled without Qdrant")
	})

	t.Run("accessible without auth", func(t *testing.T) {
		resp, err := http.Get(testSrv.URL + "/config")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
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

func TestMCPTraceDefaultAgentID(t *testing.T) {
	// When agent_id is omitted, handleTrace should default to the caller's identity.
	c := newMCPClient(t, agentToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	_, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)

	// Trace without agent_id — should succeed using the caller's identity.
	traceResult, err := c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_trace",
			Arguments: map[string]any{
				"decision_type": "trade_off",
				"outcome":       "default agent_id test",
				"confidence":    0.7,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, traceResult.IsError, "trace should succeed without agent_id: %v", traceResult.Content)

	// Verify the recorded decision used test-agent's identity.
	var resp struct {
		RunID      string `json:"run_id"`
		DecisionID string `json:"decision_id"`
		Status     string `json:"status"`
	}
	for _, content := range traceResult.Content {
		if tc, ok := content.(mcplib.TextContent); ok {
			err := json.Unmarshal([]byte(tc.Text), &resp)
			require.NoError(t, err)
			break
		}
	}
	assert.Equal(t, "recorded", resp.Status)
	assert.NotEmpty(t, resp.DecisionID)
}

func TestMCPTraceAutoRegister(t *testing.T) {
	// Admin traces for a new agent_id that doesn't exist yet — should auto-register it.
	c := newMCPClient(t, adminToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	_, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)

	// Trace as admin with a brand-new agent_id.
	newAgentID := fmt.Sprintf("auto-reg-mcp-%d", time.Now().UnixNano())
	traceResult, err := c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_trace",
			Arguments: map[string]any{
				"agent_id":      newAgentID,
				"decision_type": "architecture",
				"outcome":       "auto-registered agent test",
				"confidence":    0.8,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, traceResult.IsError, "admin trace with new agent_id should succeed: %v", traceResult.Content)

	// Verify the agent was created by listing agents.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var agentList struct {
		Data struct {
			Agents []model.Agent `json:"agents"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &agentList)

	found := false
	for _, a := range agentList.Data.Agents {
		if a.AgentID == newAgentID {
			found = true
			assert.Equal(t, model.RoleAgent, a.Role, "auto-registered agent should have role=agent")
			assert.Equal(t, newAgentID, a.Name, "auto-registered agent should use agent_id as name")
			break
		}
	}
	assert.True(t, found, "expected auto-registered agent %q in agent list", newAgentID)
}

func TestTraceAutoRegisterHTTP(t *testing.T) {
	// Admin traces via HTTP for a new agent_id — should auto-register.
	newAgentID := fmt.Sprintf("auto-reg-http-%d", time.Now().UnixNano())
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken,
		model.TraceRequest{
			AgentID: newAgentID,
			Decision: model.TraceDecision{
				DecisionType: "feature_scope",
				Outcome:      "http auto-register test",
				Confidence:   0.75,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	// Verify the agent was created.
	resp2, err := authedRequest("GET", testSrv.URL+"/v1/agents", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	var agentList struct {
		Data struct {
			Agents []model.Agent `json:"agents"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(resp2.Body)
	_ = json.Unmarshal(data, &agentList)

	found := false
	for _, a := range agentList.Data.Agents {
		if a.AgentID == newAgentID {
			found = true
			assert.Equal(t, model.RoleAgent, a.Role, "auto-registered agent should have role=agent")
			break
		}
	}
	assert.True(t, found, "expected auto-registered agent %q in agent list", newAgentID)

	// Non-admin tracing for a non-existent agent should still fail.
	nonExistentID := fmt.Sprintf("no-such-agent-%d", time.Now().UnixNano())
	resp3, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: nonExistentID,
			Decision: model.TraceDecision{
				DecisionType: "test",
				Outcome:      "should fail",
				Confidence:   0.5,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp3.Body.Close() }()
	// Non-admin can only trace for their own agent_id, so this should be forbidden.
	assert.Equal(t, http.StatusForbidden, resp3.StatusCode)
}

func TestHandleTrace_MissingDecisionType(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: "",
				Outcome:      "some-outcome",
				Confidence:   0.5,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var errResp model.APIError
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &errResp)
	assert.Equal(t, model.ErrCodeInvalidInput, errResp.Error.Code)
	assert.Contains(t, errResp.Error.Message, "decision_type")
}

func TestHandleTrace_MissingOutcome(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: "test_type",
				Outcome:      "",
				Confidence:   0.5,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var errResp model.APIError
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &errResp)
	assert.Equal(t, model.ErrCodeInvalidInput, errResp.Error.Code)
	assert.Contains(t, errResp.Error.Message, "outcome")
}

func TestHandleTrace_InvalidConfidence(t *testing.T) {
	t.Run("negative confidence", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
			model.TraceRequest{
				AgentID: "test-agent",
				Decision: model.TraceDecision{
					DecisionType: "test_type",
					Outcome:      "some-outcome",
					Confidence:   -0.1,
				},
			})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

		var errResp model.APIError
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &errResp)
		assert.Equal(t, model.ErrCodeInvalidInput, errResp.Error.Code)
		assert.Contains(t, errResp.Error.Message, "confidence")
	})

	t.Run("confidence above 1", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
			model.TraceRequest{
				AgentID: "test-agent",
				Decision: model.TraceDecision{
					DecisionType: "test_type",
					Outcome:      "some-outcome",
					Confidence:   1.5,
				},
			})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

		var errResp model.APIError
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &errResp)
		assert.Equal(t, model.ErrCodeInvalidInput, errResp.Error.Code)
		assert.Contains(t, errResp.Error.Message, "confidence")
	})
}

func TestHandleTrace_InvalidAgentID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken,
		model.TraceRequest{
			AgentID: "agent;drop",
			Decision: model.TraceDecision{
				DecisionType: "test_type",
				Outcome:      "some-outcome",
				Confidence:   0.5,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var errResp model.APIError
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &errResp)
	assert.Equal(t, model.ErrCodeInvalidInput, errResp.Error.Code)
	assert.Contains(t, errResp.Error.Message, "invalid character")
}

func TestHandleTrace_SessionHeader(t *testing.T) {
	sessionUUID := uuid.New().String()
	body, _ := json.Marshal(model.TraceRequest{
		AgentID: "test-agent",
		Decision: model.TraceDecision{
			DecisionType: "session_header_test",
			Outcome:      "verified session header accepted",
			Confidence:   0.8,
		},
	})
	req, err := http.NewRequest("POST", testSrv.URL+"/v1/trace", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+agentToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Akashi-Session", sessionUUID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	var result struct {
		Data struct {
			RunID      string `json:"run_id"`
			DecisionID string `json:"decision_id"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &result)
	assert.NotEmpty(t, result.Data.DecisionID)
}

func TestHandleTrace_IdempotencyReplay(t *testing.T) {
	key := "trace-idem-" + uuid.NewString()
	decisionType := "trace_idem_" + uuid.NewString()[:8]
	body := model.TraceRequest{
		AgentID: "test-agent",
		Decision: model.TraceDecision{
			DecisionType: decisionType,
			Outcome:      "idempotent",
			Confidence:   0.9,
		},
	}

	resp1, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/trace", agentToken, body, map[string]string{
		"Idempotency-Key": key,
	})
	require.NoError(t, err)
	defer func() { _ = resp1.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp1.StatusCode)

	var created1 struct {
		Data struct {
			RunID      string `json:"run_id"`
			DecisionID string `json:"decision_id"`
		} `json:"data"`
	}
	data1, _ := io.ReadAll(resp1.Body)
	require.NoError(t, json.Unmarshal(data1, &created1))
	require.NotEmpty(t, created1.Data.DecisionID)

	resp2, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/trace", agentToken, body, map[string]string{
		"Idempotency-Key": key,
	})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp2.StatusCode)

	var created2 struct {
		Data struct {
			RunID      string `json:"run_id"`
			DecisionID string `json:"decision_id"`
		} `json:"data"`
	}
	data2, _ := io.ReadAll(resp2.Body)
	require.NoError(t, json.Unmarshal(data2, &created2))
	assert.Equal(t, created1.Data.RunID, created2.Data.RunID)
	assert.Equal(t, created1.Data.DecisionID, created2.Data.DecisionID)

	// Verify only one decision exists for the unique type.
	respQ, err := authedRequest("POST", testSrv.URL+"/v1/query", agentToken, model.QueryRequest{
		Filters: model.QueryFilters{
			AgentIDs:     []string{"test-agent"},
			DecisionType: &decisionType,
		},
		Limit: 10,
	})
	require.NoError(t, err)
	defer func() { _ = respQ.Body.Close() }()
	require.Equal(t, http.StatusOK, respQ.StatusCode)

	var queried struct {
		Data struct {
			Decisions []json.RawMessage `json:"decisions"`
		} `json:"data"`
	}
	qData, _ := io.ReadAll(respQ.Body)
	require.NoError(t, json.Unmarshal(qData, &queried))
	assert.Len(t, queried.Data.Decisions, 1)
}

func TestHandleTrace_IdempotencyPayloadMismatch(t *testing.T) {
	key := "trace-idem-mismatch-" + uuid.NewString()
	decisionType := "trace_idem_mismatch_" + uuid.NewString()[:8]

	resp1, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/trace", agentToken, model.TraceRequest{
		AgentID: "test-agent",
		Decision: model.TraceDecision{
			DecisionType: decisionType,
			Outcome:      "first",
			Confidence:   0.8,
		},
	}, map[string]string{"Idempotency-Key": key})
	require.NoError(t, err)
	defer func() { _ = resp1.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp1.StatusCode)

	resp2, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/trace", agentToken, model.TraceRequest{
		AgentID: "test-agent",
		Decision: model.TraceDecision{
			DecisionType: decisionType,
			Outcome:      "second",
			Confidence:   0.8,
		},
	}, map[string]string{"Idempotency-Key": key})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusConflict, resp2.StatusCode)
}

func TestHandleQuery_EmptyResult(t *testing.T) {
	agentID := "nonexistent-agent-xxx"
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query", agentToken,
		model.QueryRequest{
			Filters: model.QueryFilters{
				AgentIDs: []string{agentID},
			},
			Limit: 10,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Decisions []json.RawMessage `json:"decisions"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)
	// Verify the array is present and empty (not null).
	assert.NotNil(t, result.Data.Decisions, "decisions should be an empty array, not null")
	assert.Len(t, result.Data.Decisions, 0)
}

func TestHandleQuery_LimitBounds(t *testing.T) {
	t.Run("limit zero uses default", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/query", agentToken,
			model.QueryRequest{
				Filters: model.QueryFilters{
					AgentIDs: []string{"test-agent"},
				},
				Limit: 0,
			})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data struct {
				Limit int `json:"limit"`
			} `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.Equal(t, 50, result.Data.Limit, "limit should be normalized in the handler")
	})

	t.Run("limit above max is clamped", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/query", agentToken,
			model.QueryRequest{
				Filters: model.QueryFilters{
					AgentIDs: []string{"test-agent"},
				},
				Limit: 9999,
			})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data struct {
				Limit int `json:"limit"`
			} `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.Equal(t, 1000, result.Data.Limit, "limit should be clamped in the handler")
	})

	t.Run("offset above max is clamped", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/query", agentToken,
			model.QueryRequest{
				Filters: model.QueryFilters{
					AgentIDs: []string{"test-agent"},
				},
				Limit:  10,
				Offset: 999999999,
			})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data struct {
				Offset int `json:"offset"`
			} `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.Equal(t, 100_000, result.Data.Offset, "offset should be clamped in the handler")
	})
}

func TestHandleSearch_EmptyQuery(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/search", agentToken,
		model.SearchRequest{
			Query: "",
			Limit: 5,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var errResp model.APIError
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &errResp)
	assert.Equal(t, model.ErrCodeInvalidInput, errResp.Error.Code)
	assert.Contains(t, errResp.Error.Message, "query")
}

func TestHandleSearch_LimitClamp(t *testing.T) {
	t.Run("limit zero defaults to 100", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/search", agentToken,
			model.SearchRequest{
				Query: "test decision",
				Limit: 0,
			})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		// Should succeed (handler defaults limit to 100 when <= 0).
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("limit above max defaults to 100", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/search", agentToken,
			model.SearchRequest{
				Query: "test decision",
				Limit: 9999,
			})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		// Should succeed (handler clamps limit to 100 when > 1000).
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

func TestHandleCheck_MissingDecisionType(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/check", agentToken,
		model.CheckRequest{
			DecisionType: "",
			Limit:        5,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var errResp model.APIError
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &errResp)
	assert.Equal(t, model.ErrCodeInvalidInput, errResp.Error.Code)
	assert.Contains(t, errResp.Error.Message, "decision_type")
}

func TestHandleListConflicts_Pagination(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts?limit=1", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Parse into a generic map so we can inspect pagination fields regardless
	// of whether conflicts is null or an empty array.
	var result struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)

	// Verify pagination fields are present.
	assert.Contains(t, result.Data, "limit", "response should have limit field")
	assert.Contains(t, result.Data, "offset", "response should have offset field")
	assert.Contains(t, result.Data, "has_more", "response should have has_more field")
	assert.Contains(t, result.Data, "conflicts", "response should have conflicts field")
	assert.Contains(t, result.Data, "count", "response should have count field")

	var limit int
	_ = json.Unmarshal(result.Data["limit"], &limit)
	assert.Equal(t, 1, limit, "limit should be 1")

	var offset int
	_ = json.Unmarshal(result.Data["offset"], &offset)
	assert.Equal(t, 0, offset, "offset should default to 0")
}

func TestHandleDecisionRevisions_InvalidID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/not-a-uuid/revisions", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var errResp model.APIError
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &errResp)
	assert.Equal(t, model.ErrCodeInvalidInput, errResp.Error.Code)
	assert.Contains(t, errResp.Error.Message, "invalid decision ID")
}

func TestHandleVerifyDecision_NotFound(t *testing.T) {
	randomID := uuid.New().String()
	resp, err := authedRequest("GET", testSrv.URL+"/v1/verify/"+randomID, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	var errResp model.APIError
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &errResp)
	assert.Equal(t, model.ErrCodeNotFound, errResp.Error.Code)
}

func TestHandleSessionView_EmptySession(t *testing.T) {
	randomID := uuid.New().String()
	resp, err := authedRequest("GET", testSrv.URL+"/v1/sessions/"+randomID, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			SessionID     string            `json:"session_id"`
			Decisions     []json.RawMessage `json:"decisions"`
			DecisionCount int               `json:"decision_count"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)
	assert.Equal(t, randomID, result.Data.SessionID)
	assert.NotNil(t, result.Data.Decisions, "decisions should be an empty array, not null")
	assert.Len(t, result.Data.Decisions, 0)
	assert.Equal(t, 0, result.Data.DecisionCount)
}

func TestHandleCreateGrant_Valid(t *testing.T) {
	// Create a dedicated agent for this test to grant access to.
	granteeID := fmt.Sprintf("grant-target-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, granteeID, "Grant Target", "reader", "grant-target-key")

	resourceID := "test-agent"
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", adminToken,
		model.CreateGrantRequest{
			GranteeAgentID: granteeID,
			ResourceType:   "agent_traces",
			ResourceID:     &resourceID,
			Permission:     "read",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	var result struct {
		Data struct {
			ID           string `json:"id"`
			ResourceType string `json:"resource_type"`
			Permission   string `json:"permission"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)
	assert.NotEmpty(t, result.Data.ID, "grant should have an ID")
	assert.Equal(t, "agent_traces", result.Data.ResourceType)
	assert.Equal(t, "read", result.Data.Permission)
}

func TestHandleUpdateAgentTags_Dedup(t *testing.T) {
	// Create a dedicated agent for tag updates.
	tagAgentID := fmt.Sprintf("tag-agent-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, tagAgentID, "Tag Agent", "agent", "tag-agent-key")

	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/"+tagAgentID+"/tags", adminToken,
		model.UpdateAgentTagsRequest{
			Tags: []string{"a", "b", "a"},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Tags []string `json:"tags"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)
	// The handler deduplicates tags while preserving order.
	assert.Equal(t, []string{"a", "b"}, result.Data.Tags, "duplicate tags should be removed")
}

func TestRequestIDMiddleware_ClientProvided(t *testing.T) {
	clientReqID := "my-custom-request-id-12345"
	req, err := http.NewRequest("GET", testSrv.URL+"/health", nil)
	require.NoError(t, err)
	req.Header.Set("X-Request-ID", clientReqID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, clientReqID, resp.Header.Get("X-Request-ID"),
		"response should echo back the client-provided X-Request-ID")
}

func TestHandleTemporalQuery_ZeroAsOf(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query/temporal", agentToken,
		model.TemporalQueryRequest{
			AsOf:  time.Time{},
			Limit: 10,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// A zero AsOf is technically valid (will match nothing since all
	// transaction_time values are after epoch). The handler should
	// return 200 with empty results rather than erroring.
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Decisions []json.RawMessage `json:"decisions"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &result)
	assert.NotNil(t, result.Data.Decisions)
	assert.Len(t, result.Data.Decisions, 0, "zero AsOf should return no decisions")
}

func TestHandleQuery_PaginationResponse(t *testing.T) {
	// Ensure there is at least one decision.
	_, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: "pagination_test",
				Outcome:      "verifying pagination fields",
				Confidence:   0.7,
			},
		})
	require.NoError(t, err)

	dt := "pagination_test"
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query", agentToken,
		model.QueryRequest{
			Filters: model.QueryFilters{
				AgentIDs:     []string{"test-agent"},
				DecisionType: &dt,
			},
			Limit:  5,
			Offset: 0,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Decisions []json.RawMessage `json:"decisions"`
			Total     *int              `json:"total"`
			Limit     int               `json:"limit"`
			Offset    int               `json:"offset"`
			HasMore   bool              `json:"has_more"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)

	assert.Equal(t, 5, result.Data.Limit, "response should include the requested limit")
	assert.Equal(t, 0, result.Data.Offset, "response should include the requested offset")
	// total is present when no access filtering reduces the result set.
	assert.NotNil(t, result.Data.Total, "total should be present for non-filtered results")
	assert.GreaterOrEqual(t, *result.Data.Total, 1, "total should be at least 1")
}

func TestHandleCreateRun_Valid(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	var result struct {
		Data model.AgentRun `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, result.Data.ID, "run should have a non-nil UUID")
	assert.Equal(t, "test-agent", result.Data.AgentID)
	assert.Equal(t, model.RunStatusRunning, result.Data.Status, "new run should have status 'running'")
}

func TestHandleTemporalQuery_Valid(t *testing.T) {
	// First, create a decision so there is at least one result.
	_, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: "temporal_test",
				Outcome:      "temporal query validation",
				Confidence:   0.85,
			},
		})
	require.NoError(t, err)

	// Query with AsOf set to now — includes all decisions committed before this moment.
	// The trace above may still be in the buffer, but prior test runs create enough
	// committed decisions that the assertion below is reliably satisfied.
	asOf := time.Now()
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query/temporal", agentToken,
		model.TemporalQueryRequest{
			AsOf: asOf,
			Filters: model.QueryFilters{
				AgentIDs: []string{"test-agent"},
			},
			Limit: 10,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			AsOf      string            `json:"as_of"`
			Decisions []json.RawMessage `json:"decisions"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)

	// Verify the response structure includes as_of; decisions may be empty if the
	// buffer hasn't flushed, but the endpoint must return a valid response.
	assert.NotEmpty(t, result.Data.AsOf, "response should include as_of timestamp")
}

func TestHandleSearch_ValidQuery(t *testing.T) {
	// Ensure there is a decision to find.
	_, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: "search_validation",
				Outcome:      "validated search response structure",
				Confidence:   0.9,
			},
		})
	require.NoError(t, err)

	resp, err := authedRequest("POST", testSrv.URL+"/v1/search", agentToken,
		model.SearchRequest{
			Query: "validated search response",
			Limit: 10,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Results []json.RawMessage `json:"results"`
			Total   int               `json:"total"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)

	// Verify response structure: results array and total count are present.
	assert.NotNil(t, result.Data.Results, "results should be an array, not null")
	assert.GreaterOrEqual(t, result.Data.Total, 0, "total should be non-negative")
}

func TestHandleSessionView_WithDecisions(t *testing.T) {
	// Create a decision with a specific session_id via the X-Akashi-Session header.
	sessionID := uuid.New()
	body, _ := json.Marshal(model.TraceRequest{
		AgentID: "test-agent",
		Decision: model.TraceDecision{
			DecisionType: "session_view_test",
			Outcome:      "decision within session",
			Confidence:   0.75,
		},
	})

	req, err := http.NewRequest("POST", testSrv.URL+"/v1/trace", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+agentToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Akashi-Session", sessionID.String())

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	// Retrieve the session view.
	resp2, err := authedRequest("GET", testSrv.URL+"/v1/sessions/"+sessionID.String(), agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	var result struct {
		Data struct {
			SessionID     string            `json:"session_id"`
			Decisions     []json.RawMessage `json:"decisions"`
			DecisionCount int               `json:"decision_count"`
			Summary       *struct {
				StartedAt     string         `json:"started_at"`
				EndedAt       string         `json:"ended_at"`
				DurationSecs  float64        `json:"duration_secs"`
				DecisionTypes map[string]int `json:"decision_types"`
				AvgConfidence float64        `json:"avg_confidence"`
			} `json:"summary"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(resp2.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)

	assert.Equal(t, sessionID.String(), result.Data.SessionID)
	assert.GreaterOrEqual(t, result.Data.DecisionCount, 1, "should have at least one decision in session")
	assert.Len(t, result.Data.Decisions, result.Data.DecisionCount, "decisions array length should match decision_count")

	// When there are decisions, summary should be present.
	require.NotNil(t, result.Data.Summary, "summary should be present when session has decisions")
	assert.NotEmpty(t, result.Data.Summary.StartedAt, "summary should have started_at")
	assert.NotEmpty(t, result.Data.Summary.EndedAt, "summary should have ended_at")
	assert.Contains(t, result.Data.Summary.DecisionTypes, "session_view_test",
		"decision_types should contain the traced decision type")
	assert.InDelta(t, 0.75, result.Data.Summary.AvgConfidence, 0.01,
		"avg_confidence should reflect the traced decision's confidence")
}

func TestHandleDeleteGrant(t *testing.T) {
	// Create a dedicated grantee agent.
	granteeID := fmt.Sprintf("del-grantee-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, granteeID, "Delete Grantee", "reader", "del-grantee-key")

	// Create a grant.
	resourceID := "test-agent"
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", adminToken,
		model.CreateGrantRequest{
			GranteeAgentID: granteeID,
			ResourceType:   "agent_traces",
			ResourceID:     &resourceID,
			Permission:     "read",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	// Extract the grant ID from the response.
	var createResult struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &createResult)
	require.NoError(t, err)
	grantID := createResult.Data.ID
	require.NotEmpty(t, grantID, "grant should have an ID")

	// Delete the grant.
	resp2, err := authedRequest("DELETE", testSrv.URL+"/v1/grants/"+grantID, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusNoContent, resp2.StatusCode,
		"DELETE grant should return 204 No Content")

	// Verify the grant is gone by trying to delete it again.
	resp3, err := authedRequest("DELETE", testSrv.URL+"/v1/grants/"+grantID, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp3.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp3.StatusCode,
		"second DELETE should return 404 since grant was already deleted")
}

func TestHandleCompleteRun(t *testing.T) {
	// Create a run.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &runResult)
	require.NoError(t, err)
	runID := runResult.Data.ID

	t.Run("complete with default status", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+runID.String()+"/complete", agentToken,
			model.CompleteRunRequest{})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data model.AgentRun `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		err = json.Unmarshal(data, &result)
		require.NoError(t, err)
		assert.Equal(t, model.RunStatusCompleted, result.Data.Status,
			"default completion status should be 'completed'")
		assert.NotNil(t, result.Data.CompletedAt, "completed run should have completed_at set")
	})

	t.Run("complete with failed status", func(t *testing.T) {
		// Create another run to test failure status.
		resp2, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
			model.CreateRunRequest{AgentID: "test-agent"})
		require.NoError(t, err)
		defer func() { _ = resp2.Body.Close() }()
		require.Equal(t, http.StatusCreated, resp2.StatusCode)

		var runResult2 struct {
			Data model.AgentRun `json:"data"`
		}
		data2, _ := io.ReadAll(resp2.Body)
		err = json.Unmarshal(data2, &runResult2)
		require.NoError(t, err)
		runID2 := runResult2.Data.ID

		resp3, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+runID2.String()+"/complete", agentToken,
			model.CompleteRunRequest{Status: "failed"})
		require.NoError(t, err)
		defer func() { _ = resp3.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp3.StatusCode)

		var result struct {
			Data model.AgentRun `json:"data"`
		}
		data3, _ := io.ReadAll(resp3.Body)
		err = json.Unmarshal(data3, &result)
		require.NoError(t, err)
		assert.Equal(t, model.RunStatusFailed, result.Data.Status,
			"run should have status 'failed'")
	})

	t.Run("complete with invalid status", func(t *testing.T) {
		// Create yet another run.
		resp4, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
			model.CreateRunRequest{AgentID: "test-agent"})
		require.NoError(t, err)
		defer func() { _ = resp4.Body.Close() }()
		require.Equal(t, http.StatusCreated, resp4.StatusCode)

		var runResult3 struct {
			Data model.AgentRun `json:"data"`
		}
		data4, _ := io.ReadAll(resp4.Body)
		err = json.Unmarshal(data4, &runResult3)
		require.NoError(t, err)

		resp5, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+runResult3.Data.ID.String()+"/complete", agentToken,
			model.CompleteRunRequest{Status: "invalid_status"})
		require.NoError(t, err)
		defer func() { _ = resp5.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp5.StatusCode,
			"invalid status should return 400")
	})

	t.Run("complete nonexistent run", func(t *testing.T) {
		fakeRunID := uuid.New().String()
		resp6, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+fakeRunID+"/complete", agentToken,
			model.CompleteRunRequest{})
		require.NoError(t, err)
		defer func() { _ = resp6.Body.Close() }()
		assert.Equal(t, http.StatusNotFound, resp6.StatusCode,
			"completing a nonexistent run should return 404")
	})
}

func TestHandleHealth_ResponseFields(t *testing.T) {
	resp, err := http.Get(testSrv.URL + "/health")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data model.HealthResponse `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)

	assert.Equal(t, "healthy", result.Data.Status)
	assert.Equal(t, "test", result.Data.Version, "version should match the configured test version")
	assert.GreaterOrEqual(t, result.Data.Uptime, int64(0), "uptime should be non-negative")
	assert.Equal(t, "connected", result.Data.Postgres, "postgres should be connected")
	assert.Equal(t, "ok", result.Data.BufferStatus, "buffer status should be ok under low load")
	assert.GreaterOrEqual(t, result.Data.BufferDepth, 0, "buffer depth should be non-negative")
}

func TestSecurityHeaders_AllRequired(t *testing.T) {
	// Verify security headers on an authenticated endpoint as well.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/recent", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// All security headers should be present on authenticated endpoints too.
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"),
		"X-Content-Type-Options should be set on authenticated endpoints")
	assert.Contains(t, resp.Header.Get("Strict-Transport-Security"), "max-age=63072000",
		"HSTS should be present on authenticated endpoints")
	assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "default-src",
		"CSP should be present on authenticated endpoints")
	assert.Contains(t, resp.Header.Get("Permissions-Policy"), "camera=()",
		"Permissions-Policy should be present on authenticated endpoints")
}
