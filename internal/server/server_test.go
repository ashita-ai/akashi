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
	testDB        *storage.DB   // exposed so tests can seed data not reachable via HTTP
	testBuf       *trace.Buffer // exposed so tests can flush the buffer before seeding conflicts
	testcontainer testcontainers.Container
	adminToken    string
	agentToken    string
	orgOwnerToken string
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
	testDB = db

	if err := db.RunMigrations(ctx, migrations.FS); err != nil {
		fmt.Fprintf(os.Stderr, "failed to run migrations: %v\n", err)
		os.Exit(1)
	}

	jwtMgr, _ := auth.NewJWTManager("", "", 24*time.Hour)
	embedder := embedding.NewNoopProvider(1024)
	decisionSvc := decisions.New(db, embedder, nil, logger, nil)
	buf := trace.NewBuffer(db, logger, 1000, 50*time.Millisecond, nil)
	buf.Start(ctx)
	testBuf = buf

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

	// Create an org_owner agent for GDPR erasure tests.
	// The seeded admin (rank 3) cannot create org_owner (rank 4) via HTTP because
	// HandleCreateAgent requires the caller to strictly outrank the requested role.
	// Insert directly via the storage layer instead.
	{
		ownerKeyHash, hashErr := auth.HashAPIKey("test-org-owner-key")
		if hashErr != nil {
			fmt.Fprintf(os.Stderr, "failed to hash org owner key: %v\n", hashErr)
			os.Exit(1)
		}
		if _, dbErr := db.CreateAgent(ctx, model.Agent{
			AgentID:    "test-org-owner",
			OrgID:      uuid.Nil,
			Name:       "Test Org Owner",
			Role:       model.RoleOrgOwner,
			APIKeyHash: &ownerKeyHash,
		}); dbErr != nil {
			fmt.Fprintf(os.Stderr, "failed to create org owner agent: %v\n", dbErr)
			os.Exit(1)
		}
		orgOwnerToken = getToken(testSrv.URL, "test-org-owner", "test-org-owner-key")
	}

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

	// Flush the event buffer so events are visible in the database.
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()
	require.NoError(t, testBuf.FlushNow(flushCtx))

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
	assert.Len(t, toolsResult.Tools, 6)

	toolNames := make(map[string]bool)
	for _, tool := range toolsResult.Tools {
		toolNames[tool.Name] = true
	}
	assert.True(t, toolNames["akashi_check"], "expected akashi_check tool")
	assert.True(t, toolNames["akashi_trace"], "expected akashi_trace tool")
	assert.True(t, toolNames["akashi_query"], "expected akashi_query tool")
	assert.True(t, toolNames["akashi_conflicts"], "expected akashi_conflicts tool")
	assert.True(t, toolNames["akashi_stats"], "expected akashi_stats tool")
	assert.True(t, toolNames["akashi_assess"], "expected akashi_assess tool")
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

func TestMCPQueryRecentDecisions(t *testing.T) {
	c := newMCPClient(t, agentToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	_, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)

	// Record a decision so there's at least one to retrieve.
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

	// akashi_query with no filters returns recent decisions (replaces akashi_recent).
	queryResult, err := c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_query",
			Arguments: map[string]any{
				"limit": 5,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, queryResult.IsError, "query tool returned error: %v", queryResult.Content)

	// Parse and verify we got results.
	var queryResp struct {
		Decisions []model.Decision `json:"decisions"`
		Total     int              `json:"total"`
	}
	for _, content := range queryResult.Content {
		if tc, ok := content.(mcplib.TextContent); ok {
			err := json.Unmarshal([]byte(tc.Text), &queryResp)
			require.NoError(t, err)
			break
		}
	}
	assert.NotEmpty(t, queryResp.Decisions, "expected at least one decision")
	assert.Greater(t, queryResp.Total, 0, "expected total > 0")
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
		Data  []model.Decision `json:"data"`
		Total int              `json:"total"`
		Limit int              `json:"limit"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)
	assert.NotEmpty(t, result.Data, "expected at least one recent decision")
	assert.Equal(t, 10, result.Limit, "expected default limit of 10")

	// GET with agent_id filter.
	resp2, err := authedRequest("GET", testSrv.URL+"/v1/decisions/recent?agent_id=test-agent&limit=3", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	var result2 struct {
		Data  []model.Decision `json:"data"`
		Limit int              `json:"limit"`
	}
	data2, _ := io.ReadAll(resp2.Body)
	err = json.Unmarshal(data2, &result2)
	require.NoError(t, err)
	assert.Equal(t, 3, result2.Limit)
	for _, d := range result2.Data {
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
		Data []model.Decision `json:"data"`
	}
	data, _ := io.ReadAll(resp2.Body)
	_ = json.Unmarshal(data, &hist)
	assert.NotEmpty(t, hist.Data, "agent should have decisions before deletion")

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
			Data []model.Decision `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.Empty(t, result.Data, "reader should see no decisions without a grant")
	})

	t.Run("reader gets empty recent decisions", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/recent?agent_id=test-agent", readerToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data []model.Decision `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.Empty(t, result.Data, "reader should see no recent decisions without a grant")
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
			Data []model.Decision `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.NotEmpty(t, result.Data, "reader should see decisions after grant")
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
			Data []model.Decision `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.NotEmpty(t, result.Data, "reader should see decisions after grant")
	})

	t.Run("admin sees everything regardless", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent/history", adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data []model.Decision `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.NotEmpty(t, result.Data, "admin should always see decisions")
	})

	t.Run("agent can see own data", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent/history", agentToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data []model.Decision `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.NotEmpty(t, result.Data, "agent should see own decisions")
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
		Data []model.Agent `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &agentList)

	found := false
	for _, a := range agentList.Data {
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
		Data []model.Agent `json:"data"`
	}
	data, _ := io.ReadAll(resp2.Body)
	_ = json.Unmarshal(data, &agentList)

	found := false
	for _, a := range agentList.Data {
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
		Data []json.RawMessage `json:"data"`
	}
	qData, _ := io.ReadAll(respQ.Body)
	require.NoError(t, json.Unmarshal(qData, &queried))
	assert.Len(t, queried.Data, 1)
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
		Data []json.RawMessage `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)
	// Verify the array is present and empty (not null).
	assert.NotNil(t, result.Data, "decisions should be an empty array, not null")
	assert.Len(t, result.Data, 0)
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
			Limit int `json:"limit"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.Equal(t, 50, result.Limit, "limit should be normalized in the handler")
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
			Limit int `json:"limit"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.Equal(t, 1000, result.Limit, "limit should be clamped in the handler")
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
			Offset int `json:"offset"`
		}
		data, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(data, &result)
		assert.Equal(t, 100_000, result.Offset, "offset should be clamped in the handler")
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

	// Parse the flat list envelope: data is the array, pagination fields are top-level.
	var result struct {
		Data    []json.RawMessage `json:"data"`
		Limit   int               `json:"limit"`
		Offset  int               `json:"offset"`
		HasMore bool              `json:"has_more"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)

	// Verify pagination fields are present at the top level.
	assert.NotNil(t, result.Data, "response data should be an array")
	assert.Equal(t, 1, result.Limit, "limit should be 1")
	assert.Equal(t, 0, result.Offset, "offset should default to 0")
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

func TestHandleVerifyDecision_Active(t *testing.T) {
	// Trace a decision so it gets a content hash.
	dt := "verify_active_" + uuid.NewString()[:8]
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: dt,
				Outcome:      "active decision for verify test",
				Confidence:   0.9,
				Reasoning:    ptrStr("testing verify on active decision"),
			},
		})
	require.NoError(t, err)
	defer func() { _ = traceResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, traceResp.StatusCode)

	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	traceBody, _ := io.ReadAll(traceResp.Body)
	require.NoError(t, json.Unmarshal(traceBody, &traceResult))
	decisionID := traceResult.Data.DecisionID

	resp, err := authedRequest("GET", testSrv.URL+"/v1/verify/"+decisionID.String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]any
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))

	data, ok := result["data"].(map[string]any)
	require.True(t, ok, "expected data wrapper in response")
	assert.Equal(t, "verified", data["status"])
	assert.Equal(t, true, data["valid"])
	assert.NotEmpty(t, data["content_hash"])
	assert.Nil(t, data["retracted_at"], "active decision must not have retracted_at")
}

func TestHandleVerifyDecision_Retracted(t *testing.T) {
	// Trace a decision, retract it, then verify it.
	dt := "verify_retracted_" + uuid.NewString()[:8]
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: dt,
				Outcome:      "will be retracted then verified",
				Confidence:   0.75,
				Reasoning:    ptrStr("testing verify on retracted decision"),
			},
		})
	require.NoError(t, err)
	defer func() { _ = traceResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, traceResp.StatusCode)

	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	traceBody, _ := io.ReadAll(traceResp.Body)
	require.NoError(t, json.Unmarshal(traceBody, &traceResult))
	decisionID := traceResult.Data.DecisionID

	// Retract the decision.
	retractResp, err := authedRequest("DELETE", testSrv.URL+"/v1/decisions/"+decisionID.String(), adminToken,
		map[string]string{"reason": "verify retraction test"})
	require.NoError(t, err)
	defer func() { _ = retractResp.Body.Close() }()
	require.Equal(t, http.StatusOK, retractResp.StatusCode)

	// Verify the retracted decision.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/verify/"+decisionID.String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]any
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))

	data, ok := result["data"].(map[string]any)
	require.True(t, ok, "expected data wrapper in response")
	assert.Equal(t, "retracted", data["status"])
	assert.NotEmpty(t, data["retracted_at"], "retracted decision must have retracted_at")
	assert.Equal(t, true, data["verified"], "hash should still verify for retracted decision")
	assert.NotEmpty(t, data["content_hash"])

	// Confirm retracted_at parses as a valid timestamp.
	_, parseErr := time.Parse(time.RFC3339Nano, data["retracted_at"].(string))
	assert.NoError(t, parseErr, "retracted_at must be a valid RFC3339Nano timestamp")
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

func TestHandleCreateAndListKeys(t *testing.T) {
	// Create a dedicated agent for this test to own the key.
	agentID := fmt.Sprintf("key-agent-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, agentID, "Key Agent", "agent", "key-agent-init")

	// Create key
	req := model.CreateKeyRequest{AgentID: agentID, Label: "test-key"}
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken, req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	var created struct {
		Data model.APIKeyWithRawKey `json:"data"`
	}
	data, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(data, &created))
	assert.NotEmpty(t, created.Data.RawKey)
	assert.NotEqual(t, uuid.Nil, created.Data.ID)

	// List keys and ensure the created key's prefix appears (raw key not returned)
	resp2, err := authedRequest("GET", testSrv.URL+"/v1/keys", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	var list struct {
		Data model.APIKeyResponse `json:"data"`
	}
	listData, _ := io.ReadAll(resp2.Body)
	require.NoError(t, json.Unmarshal(listData, &list))
	found := false
	for _, k := range list.Data.Keys {
		if k.ID == created.Data.ID {
			found = true
			break
		}
	}
	assert.True(t, found, "created key should appear in list")
}

func TestHandleRevokeKey_ValidAndRevokedKeyReturns401(t *testing.T) {
	// Create agent + key
	tagent := fmt.Sprintf("revoke-agent-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, tagent, "Revoke Agent", "agent", "revoke-init-key")

	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken, model.CreateKeyRequest{AgentID: tagent, Label: "revoke-test"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var created struct {
		Data model.APIKeyWithRawKey `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &created))

	// Verify the raw key works before revocation: get a token successfully.
	pre := getToken(testSrv.URL, tagent, created.Data.RawKey)
	assert.NotEmpty(t, pre)

	// Revoke the key.
	reqDel, _ := http.NewRequest("DELETE", testSrv.URL+"/v1/keys/"+created.Data.ID.String(), nil)
	reqDel.Header.Set("Authorization", "Bearer "+adminToken)
	respDel, err := http.DefaultClient.Do(reqDel)
	require.NoError(t, err)
	defer func() { _ = respDel.Body.Close() }()
	assert.Equal(t, http.StatusNoContent, respDel.StatusCode)

	// After revocation, requesting a NEW token with the old key must return 401.
	// (JWTs already issued are stateless and stay valid until expiry.)
	authBody, _ := json.Marshal(model.AuthTokenRequest{AgentID: tagent, APIKey: created.Data.RawKey})
	respNew, err := http.Post(testSrv.URL+"/auth/token", "application/json", bytes.NewReader(authBody))
	require.NoError(t, err)
	defer func() { _ = respNew.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, respNew.StatusCode, "revoked key must not issue new tokens")
}

func TestHandleRevokeKey_NotFound(t *testing.T) {
	// Attempt to revoke a random UUID
	randID := uuid.New()
	reqDel, _ := http.NewRequest("DELETE", testSrv.URL+"/v1/keys/"+randID.String(), nil)
	reqDel.Header.Set("Authorization", "Bearer "+adminToken)
	respDel, err := http.DefaultClient.Do(reqDel)
	require.NoError(t, err)
	defer func() { _ = respDel.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, respDel.StatusCode)
}

func TestHandleRotateKey_Valid(t *testing.T) {
	// Create agent + key
	rAgent := fmt.Sprintf("rotate-agent-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, rAgent, "Rotate Agent", "agent", "rotate-init-key")

	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken, model.CreateKeyRequest{AgentID: rAgent, Label: "rotate-test"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var created struct {
		Data model.APIKeyWithRawKey `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &created))

	// Rotate
	respRot, err := authedRequest("POST", testSrv.URL+"/v1/keys/"+created.Data.ID.String()+"/rotate", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = respRot.Body.Close() }()
	require.Equal(t, http.StatusOK, respRot.StatusCode)

	var rot struct {
		Data model.RotateKeyResponse `json:"data"`
	}
	rotBody, _ := io.ReadAll(respRot.Body)
	require.NoError(t, json.Unmarshal(rotBody, &rot))
	assert.NotEmpty(t, rot.Data.NewKey.RawKey)
	assert.Equal(t, created.Data.ID, rot.Data.RevokedKeyID)

	// After rotation, the OLD raw key must no longer issue new tokens.
	oldAuthBody, _ := json.Marshal(model.AuthTokenRequest{AgentID: rAgent, APIKey: created.Data.RawKey})
	respOld, err := http.Post(testSrv.URL+"/auth/token", "application/json", bytes.NewReader(oldAuthBody))
	require.NoError(t, err)
	defer func() { _ = respOld.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, respOld.StatusCode, "rotated (revoked) key must not issue new tokens")

	// New raw key should obtain a fresh token.
	newToken := getToken(testSrv.URL, rAgent, rot.Data.NewKey.RawKey)
	assert.NotEmpty(t, newToken)
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
		Data    []json.RawMessage `json:"data"`
		Total   *int              `json:"total"`
		Limit   int               `json:"limit"`
		Offset  int               `json:"offset"`
		HasMore bool              `json:"has_more"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)

	assert.Equal(t, 5, result.Limit, "response should include the requested limit")
	assert.Equal(t, 0, result.Offset, "response should include the requested offset")
	// total is present when no access filtering reduces the result set.
	assert.NotNil(t, result.Total, "total should be present for non-filtered results")
	assert.GreaterOrEqual(t, *result.Total, 1, "total should be at least 1")
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
		Data  []json.RawMessage `json:"data"`
		Total int               `json:"total"`
	}
	data, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)

	// Verify response structure: results array and total count are present.
	assert.NotNil(t, result.Data, "results should be an array, not null")
	assert.GreaterOrEqual(t, result.Total, 0, "total should be non-negative")
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

// ---- HandleGetDecision ---------------------------------------------------

func TestHandleGetDecision(t *testing.T) {
	// Trace a decision to get a valid ID.
	dt := "get_decision_test_" + uuid.NewString()[:8]
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: dt,
				Outcome:      "retrieved ok",
				Confidence:   0.8,
				Reasoning:    ptrStr("for get decision test"),
			},
		})
	require.NoError(t, err)
	defer func() { _ = traceResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, traceResp.StatusCode)

	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(traceResp.Body)
	require.NoError(t, json.Unmarshal(data, &traceResult))
	decisionID := traceResult.Data.DecisionID
	require.NotEqual(t, uuid.Nil, decisionID)

	t.Run("happy path returns decision", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+decisionID.String(), agentToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data model.Decision `json:"data"`
		}
		body, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(body, &result))
		assert.Equal(t, decisionID, result.Data.ID)
		assert.Equal(t, dt, result.Data.DecisionType)
		assert.Equal(t, "retrieved ok", result.Data.Outcome)
	})

	t.Run("invalid UUID returns 400", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/not-a-uuid", agentToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("nonexistent ID returns 404", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+uuid.New().String(), agentToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

// ---- HandleTraceHealth (exercises tracehealth.New + tracehealth.Compute) -

func TestHandleTraceHealth(t *testing.T) {
	t.Run("returns valid metrics structure", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/trace-health", adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data struct {
				Status       string `json:"status"`
				Completeness *struct {
					TotalDecisions int `json:"total_decisions"`
				} `json:"completeness"`
				Gaps []string `json:"gaps"`
			} `json:"data"`
		}
		body, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(body, &result))
		assert.Contains(t, []string{"healthy", "needs_attention", "insufficient_data"}, result.Data.Status)
		assert.NotNil(t, result.Data.Completeness)
		assert.NotNil(t, result.Data.Gaps)
	})

	t.Run("requires authentication", func(t *testing.T) {
		req, _ := http.NewRequest("GET", testSrv.URL+"/v1/trace-health", nil)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
}

// ---- HandlePatchConflict -------------------------------------------------

func TestHandlePatchConflict(t *testing.T) {
	t.Run("invalid UUID returns 400", func(t *testing.T) {
		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflicts/not-a-uuid", adminToken,
			model.ConflictStatusUpdate{Status: "acknowledged"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("invalid status returns 400", func(t *testing.T) {
		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflicts/"+uuid.New().String(), adminToken,
			model.ConflictStatusUpdate{Status: "invalid_status"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

		var errResp model.APIError
		body, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(body, &errResp)
		assert.Equal(t, model.ErrCodeInvalidInput, errResp.Error.Code)
	})

	t.Run("nonexistent conflict returns 404", func(t *testing.T) {
		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflicts/"+uuid.New().String(), adminToken,
			model.ConflictStatusUpdate{Status: "acknowledged"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

// ---- HandleAdjudicateConflict --------------------------------------------

func TestHandleAdjudicateConflict(t *testing.T) {
	t.Run("invalid UUID returns 400", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/conflicts/not-a-uuid/adjudicate", adminToken,
			map[string]any{"outcome": "agent-a is correct"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("missing outcome returns 400", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/conflicts/"+uuid.New().String()+"/adjudicate", adminToken,
			map[string]any{"outcome": ""})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

		var errResp model.APIError
		body, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(body, &errResp)
		assert.Equal(t, model.ErrCodeInvalidInput, errResp.Error.Code)
	})

	t.Run("nonexistent conflict returns 404", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/conflicts/"+uuid.New().String()+"/adjudicate", adminToken,
			map[string]any{"outcome": "agent-a is correct"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

// ---- MCP: akashi_conflicts -----------------------------------------------

func TestMCPConflictsTool(t *testing.T) {
	c := newMCPClient(t, agentToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	_, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)

	result, err := c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name:      "akashi_conflicts",
			Arguments: map[string]any{"limit": 10},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "akashi_conflicts returned error: %v", result.Content)

	// Response must include a "conflicts" array (may be empty).
	var resp struct {
		Conflicts []any `json:"conflicts"`
		Total     int   `json:"total"`
	}
	for _, content := range result.Content {
		if tc, ok := content.(mcplib.TextContent); ok {
			require.NoError(t, json.Unmarshal([]byte(tc.Text), &resp))
			break
		}
	}
	assert.NotNil(t, resp.Conflicts, "conflicts field should be present")
}

func TestMCPConflictsTool_WithStatusFilter(t *testing.T) {
	c := newMCPClient(t, agentToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	_, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)

	result, err := c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name:      "akashi_conflicts",
			Arguments: map[string]any{"status": "resolved", "limit": 5},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "akashi_conflicts with status filter returned error: %v", result.Content)
}

// ---- MCP: akashi_assess --------------------------------------------------

func TestMCPAssessTool(t *testing.T) {
	c := newMCPClient(t, agentToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	_, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)

	// Trace a decision to assess.
	traceResult, err := c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name: "akashi_trace",
			Arguments: map[string]any{
				"agent_id":      "test-agent",
				"decision_type": "assess_tool_test",
				"outcome":       "chose the right approach",
				"confidence":    0.9,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, traceResult.IsError, "trace returned error: %v", traceResult.Content)

	var traceResp struct {
		DecisionID string `json:"decision_id"`
	}
	for _, content := range traceResult.Content {
		if tc, ok := content.(mcplib.TextContent); ok {
			require.NoError(t, json.Unmarshal([]byte(tc.Text), &traceResp))
			break
		}
	}
	require.NotEmpty(t, traceResp.DecisionID)

	t.Run("valid assessment is recorded", func(t *testing.T) {
		result, err := c.CallTool(ctx, mcplib.CallToolRequest{
			Params: mcplib.CallToolParams{
				Name: "akashi_assess",
				Arguments: map[string]any{
					"decision_id": traceResp.DecisionID,
					"outcome":     "correct",
					"notes":       "verified in production",
				},
			},
		})
		require.NoError(t, err)
		require.False(t, result.IsError, "akashi_assess returned error: %v", result.Content)

		var assessResp struct {
			AssessmentID string `json:"assessment_id"`
			DecisionID   string `json:"decision_id"`
			Outcome      string `json:"outcome"`
		}
		for _, content := range result.Content {
			if tc, ok := content.(mcplib.TextContent); ok {
				require.NoError(t, json.Unmarshal([]byte(tc.Text), &assessResp))
				break
			}
		}
		assert.NotEmpty(t, assessResp.AssessmentID)
		assert.Equal(t, traceResp.DecisionID, assessResp.DecisionID)
		assert.Equal(t, "correct", assessResp.Outcome)
	})

	t.Run("missing decision_id returns tool error", func(t *testing.T) {
		result, err := c.CallTool(ctx, mcplib.CallToolRequest{
			Params: mcplib.CallToolParams{
				Name:      "akashi_assess",
				Arguments: map[string]any{"outcome": "correct"},
			},
		})
		require.NoError(t, err)
		assert.True(t, result.IsError, "missing decision_id should produce tool error")
	})

	t.Run("invalid outcome returns tool error", func(t *testing.T) {
		result, err := c.CallTool(ctx, mcplib.CallToolRequest{
			Params: mcplib.CallToolParams{
				Name: "akashi_assess",
				Arguments: map[string]any{
					"decision_id": traceResp.DecisionID,
					"outcome":     "definitely_not_a_valid_outcome",
				},
			},
		})
		require.NoError(t, err)
		assert.True(t, result.IsError, "invalid outcome should produce tool error")
	})
}

// ---- MCP: akashi_stats ---------------------------------------------------

func TestMCPStatsTool(t *testing.T) {
	c := newMCPClient(t, agentToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	_, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)

	result, err := c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name:      "akashi_stats",
			Arguments: map[string]any{},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "akashi_stats returned error: %v", result.Content)

	var statsResp struct {
		TraceHealth struct {
			Status string `json:"status"`
		} `json:"trace_health"`
		Agents int `json:"agents"`
	}
	for _, content := range result.Content {
		if tc, ok := content.(mcplib.TextContent); ok {
			require.NoError(t, json.Unmarshal([]byte(tc.Text), &statsResp))
			break
		}
	}
	assert.Contains(t, []string{"healthy", "needs_attention", "insufficient_data"}, statsResp.TraceHealth.Status)
	assert.GreaterOrEqual(t, statsResp.Agents, 0)
}

// ---- MCP: akashi_trace with idempotency_key (exercises mcpTraceHash) -----

func TestMCPTraceIdempotencyKey(t *testing.T) {
	c := newMCPClient(t, agentToken)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	_, err := c.Initialize(ctx, mcplib.InitializeRequest{
		Params: mcplib.InitializeParams{
			ClientInfo: mcplib.Implementation{Name: "test-client", Version: "1.0"},
		},
	})
	require.NoError(t, err)

	idemKey := "mcp-idem-" + uuid.NewString()
	args := map[string]any{
		"agent_id":        "test-agent",
		"decision_type":   "mcp_idempotency_test",
		"outcome":         "chose postgres",
		"confidence":      0.85,
		"idempotency_key": idemKey,
	}

	// First call — should record and return a decision_id.
	r1, err := c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{Name: "akashi_trace", Arguments: args},
	})
	require.NoError(t, err)
	require.False(t, r1.IsError, "first trace with idempotency_key failed: %v", r1.Content)

	var resp1 struct {
		DecisionID string `json:"decision_id"`
		Status     string `json:"status"`
	}
	for _, content := range r1.Content {
		if tc, ok := content.(mcplib.TextContent); ok {
			require.NoError(t, json.Unmarshal([]byte(tc.Text), &resp1))
			break
		}
	}
	require.NotEmpty(t, resp1.DecisionID)
	assert.Equal(t, "recorded", resp1.Status)

	// Second call with the same key and payload — must replay the original decision_id.
	r2, err := c.CallTool(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{Name: "akashi_trace", Arguments: args},
	})
	require.NoError(t, err)
	require.False(t, r2.IsError, "idempotency replay failed: %v", r2.Content)

	var resp2 struct {
		DecisionID string `json:"decision_id"`
	}
	for _, content := range r2.Content {
		if tc, ok := content.(mcplib.TextContent); ok {
			require.NoError(t, json.Unmarshal([]byte(tc.Text), &resp2))
			break
		}
	}
	assert.Equal(t, resp1.DecisionID, resp2.DecisionID, "idempotency replay must return the same decision_id")
}

// ptrStr is a test convenience helper for *string literals.
func ptrStr(s string) *string { return &s }

// ---- Spec-34: winning_decision_id on PATCH /v1/conflicts/{id} ------------

// seedConflict traces two decisions via HTTP and inserts a scored conflict
// referencing them directly via testDB. Returns the two decision IDs and the
// conflict ID so callers can drive conflict resolution scenarios.
func seedConflict(t *testing.T) (decisionAID, decisionBID, conflictID uuid.UUID) {
	t.Helper()
	orgID := uuid.Nil // default org from SeedAdmin

	traceDecision := func(outcome string, confidence float32) uuid.UUID {
		resp, tErr := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, model.TraceRequest{
			AgentID: "admin",
			Decision: model.TraceDecision{
				DecisionType: "architecture",
				Outcome:      outcome,
				Confidence:   confidence,
			},
		})
		require.NoError(t, tErr)
		defer func() { _ = resp.Body.Close() }()
		require.Contains(t, []int{http.StatusOK, http.StatusCreated, http.StatusAccepted}, resp.StatusCode, "trace %q", outcome)
		var result struct {
			Data struct {
				DecisionID uuid.UUID `json:"decision_id"`
			} `json:"data"`
		}
		b, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(b, &result))
		require.NotEqual(t, uuid.Nil, result.Data.DecisionID)
		return result.Data.DecisionID
	}

	decisionAID = traceDecision("spec-34 side A: use Redis", 0.9)
	decisionBID = traceDecision("spec-34 side B: use Memcached", 0.8)

	// Flush the event buffer so the decisions are committed before we insert
	// the conflict (which has FKs to decision_a_id and decision_b_id).
	require.NoError(t, testBuf.FlushNow(context.Background()))

	// Insert scored conflict directly — embedding pipeline won't produce one
	// with the noop embedder (zero vectors have undefined cosine similarity).
	// InsertScoredConflict ignores c.ID; the database auto-generates it.
	// Capture the returned UUID so callers can reference the actual row.
	c := model.DecisionConflict{
		OrgID:         orgID,
		ConflictKind:  model.ConflictKindCrossAgent,
		DecisionAID:   decisionAID,
		DecisionBID:   decisionBID,
		AgentA:        "admin",
		AgentB:        "admin",
		DecisionTypeA: "architecture",
		DecisionTypeB: "architecture",
		OutcomeA:      "spec-34 side A: use Redis",
		OutcomeB:      "spec-34 side B: use Memcached",
		Status:        "open",
	}
	var err error
	conflictID, err = testDB.InsertScoredConflict(context.Background(), c)
	require.NoError(t, err)
	return decisionAID, decisionBID, conflictID
}

func TestHandlePatchConflict_WinningDecisionID(t *testing.T) {
	decisionAID, decisionBID, conflictID := seedConflict(t)

	t.Run("winning_decision_id without resolved status returns 400", func(t *testing.T) {
		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflicts/"+conflictID.String(), adminToken,
			model.ConflictStatusUpdate{Status: "acknowledged", WinningDecisionID: &decisionAID})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		var errResp model.APIError
		body, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(body, &errResp)
		assert.Equal(t, model.ErrCodeInvalidInput, errResp.Error.Code)
	})

	t.Run("winning_decision_id not in conflict returns 400", func(t *testing.T) {
		stranger := uuid.New()
		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflicts/"+conflictID.String(), adminToken,
			model.ConflictStatusUpdate{Status: "resolved", WinningDecisionID: &stranger})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		var errResp model.APIError
		body, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(body, &errResp)
		assert.Equal(t, model.ErrCodeInvalidInput, errResp.Error.Code)
	})

	t.Run("resolved with winning_decision_id persists winner", func(t *testing.T) {
		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflicts/"+conflictID.String(), adminToken,
			model.ConflictStatusUpdate{Status: "resolved", WinningDecisionID: &decisionAID})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var envelope struct {
			Data struct {
				WinningDecisionID *uuid.UUID `json:"winning_decision_id"`
				Status            string     `json:"status"`
			} `json:"data"`
		}
		body, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(body, &envelope))
		result := envelope.Data
		assert.Equal(t, "resolved", result.Status)
		require.NotNil(t, result.WinningDecisionID)
		assert.Equal(t, decisionAID, *result.WinningDecisionID)
	})

	t.Run("resolved without winning_decision_id leaves winner nil", func(t *testing.T) {
		_, _, noWinnerConflict := seedConflict(t)

		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflicts/"+noWinnerConflict.String(), adminToken,
			model.ConflictStatusUpdate{Status: "resolved"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var envelope struct {
			Data struct {
				WinningDecisionID *uuid.UUID `json:"winning_decision_id"`
				Status            string     `json:"status"`
			} `json:"data"`
		}
		body, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(body, &envelope))
		result := envelope.Data
		assert.Equal(t, "resolved", result.Status)
		assert.Nil(t, result.WinningDecisionID, "no winner declared → field must be absent/null")
	})

	// Silence unused variable warning: decisionBID is set by seedConflict
	// and exists to make conflict-side validation meaningful.
	_ = decisionBID
}

// ---- HandleRetractDecision ------------------------------------------------

func TestHandleRetractDecision(t *testing.T) {
	// Trace a decision to retract.
	dt := "retract_test_" + uuid.NewString()[:8]
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: dt,
				Outcome:      "will be retracted",
				Confidence:   0.85,
				Reasoning:    ptrStr("retraction test"),
			},
		})
	require.NoError(t, err)
	defer func() { _ = traceResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, traceResp.StatusCode)

	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
			RunID      uuid.UUID `json:"run_id"`
		} `json:"data"`
	}
	traceBody, _ := io.ReadAll(traceResp.Body)
	require.NoError(t, json.Unmarshal(traceBody, &traceResult))
	decisionID := traceResult.Data.DecisionID
	runID := traceResult.Data.RunID
	require.NotEqual(t, uuid.Nil, decisionID)
	require.NotEqual(t, uuid.Nil, runID)

	t.Run("non-admin gets 403", func(t *testing.T) {
		resp, err := authedRequest("DELETE", testSrv.URL+"/v1/decisions/"+decisionID.String(), agentToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("invalid UUID returns 400", func(t *testing.T) {
		resp, err := authedRequest("DELETE", testSrv.URL+"/v1/decisions/not-a-uuid", adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("nonexistent ID returns 404", func(t *testing.T) {
		resp, err := authedRequest("DELETE", testSrv.URL+"/v1/decisions/"+uuid.New().String(), adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("successful retraction with reason", func(t *testing.T) {
		resp, err := authedRequest("DELETE", testSrv.URL+"/v1/decisions/"+decisionID.String(), adminToken,
			map[string]string{"reason": "inaccurate assessment"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data model.Decision `json:"data"`
		}
		body, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(body, &result))
		assert.Equal(t, decisionID, result.Data.ID)
		assert.NotNil(t, result.Data.ValidTo, "valid_to must be set after retraction")
	})

	t.Run("retracted decision hidden from current-only GET", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+decisionID.String(), agentToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		// HandleGetDecision does not use CurrentOnly by default, so the decision
		// is still retrievable. But it should have valid_to set.
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		var result struct {
			Data model.Decision `json:"data"`
		}
		body, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(body, &result))
		assert.NotNil(t, result.Data.ValidTo, "retracted decision must have valid_to set")
	})

	t.Run("DecisionRetracted event recorded", func(t *testing.T) {
		// Query events for the run that contained the retracted decision.
		orgID := uuid.Nil // default org from SeedAdmin
		events, err := testDB.GetEventsByRun(context.Background(), orgID, runID, 100)
		require.NoError(t, err)

		var found bool
		for _, ev := range events {
			if ev.EventType == model.EventDecisionRetracted {
				found = true
				assert.Equal(t, decisionID.String(), ev.Payload["decision_id"])
				assert.Equal(t, "inaccurate assessment", ev.Payload["reason"])
				assert.NotEmpty(t, ev.Payload["retracted_by"])
				break
			}
		}
		assert.True(t, found, "expected a DecisionRetracted event in the run's event log")
	})

	t.Run("double retract returns 404", func(t *testing.T) {
		resp, err := authedRequest("DELETE", testSrv.URL+"/v1/decisions/"+decisionID.String(), adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

// ---- HandleResolveConflictGroup ------------------------------------------

// seedConflictGroup creates two decisions and two open conflicts sharing the
// same conflict group. Returns the group ID, two conflict IDs, and the two
// decision IDs (A1, B1 for the first pair; A2, B2 for the second). Agent
// names are "alpha" (side A) and "beta" (side B).
func seedConflictGroup(t *testing.T) (groupID uuid.UUID, conflictIDs [2]uuid.UUID, decisionIDs [4]uuid.UUID) {
	t.Helper()
	orgID := uuid.Nil

	traceDecision := func(agentID, outcome string, confidence float32) uuid.UUID {
		resp, tErr := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, model.TraceRequest{
			AgentID: agentID,
			Decision: model.TraceDecision{
				DecisionType: "architecture",
				Outcome:      outcome,
				Confidence:   confidence,
			},
		})
		require.NoError(t, tErr)
		defer func() { _ = resp.Body.Close() }()
		require.Contains(t, []int{http.StatusOK, http.StatusCreated, http.StatusAccepted}, resp.StatusCode)
		var result struct {
			Data struct {
				DecisionID uuid.UUID `json:"decision_id"`
			} `json:"data"`
		}
		b, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(b, &result))
		require.NotEqual(t, uuid.Nil, result.Data.DecisionID)
		return result.Data.DecisionID
	}

	// Create 4 decisions: two per side
	decisionIDs[0] = traceDecision("alpha", "use Redis for caching", 0.9)
	decisionIDs[1] = traceDecision("beta", "use Memcached for caching", 0.8)
	decisionIDs[2] = traceDecision("alpha", "Redis cluster mode", 0.85)
	decisionIDs[3] = traceDecision("beta", "Memcached with mcrouter", 0.75)

	require.NoError(t, testBuf.FlushNow(context.Background()))

	// Insert two conflicts — InsertScoredConflict auto-creates and links the group.
	for i, pair := range [][2]int{{0, 1}, {2, 3}} {
		c := model.DecisionConflict{
			OrgID:         orgID,
			ConflictKind:  model.ConflictKindCrossAgent,
			DecisionAID:   decisionIDs[pair[0]],
			DecisionBID:   decisionIDs[pair[1]],
			AgentA:        "alpha",
			AgentB:        "beta",
			DecisionTypeA: "architecture",
			DecisionTypeB: "architecture",
			OutcomeA:      "alpha outcome",
			OutcomeB:      "beta outcome",
			Status:        "open",
		}
		var err error
		conflictIDs[i], err = testDB.InsertScoredConflict(context.Background(), c)
		require.NoError(t, err)
	}

	// Retrieve the group_id that was auto-created.
	conflict, err := testDB.GetConflict(context.Background(), conflictIDs[0], orgID)
	require.NoError(t, err)
	require.NotNil(t, conflict.GroupID, "InsertScoredConflict must set group_id")
	groupID = *conflict.GroupID

	return groupID, conflictIDs, decisionIDs
}

func TestHandleResolveConflictGroup(t *testing.T) {
	t.Run("invalid UUID returns 400", func(t *testing.T) {
		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflict-groups/not-a-uuid/resolve", adminToken,
			model.ConflictGroupResolveRequest{Status: "resolved"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("invalid status returns 400", func(t *testing.T) {
		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflict-groups/"+uuid.New().String()+"/resolve", adminToken,
			model.ConflictGroupResolveRequest{Status: "acknowledged"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

		var errResp model.APIError
		body, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(body, &errResp)
		assert.Equal(t, model.ErrCodeInvalidInput, errResp.Error.Code)
	})

	t.Run("winning_agent with wont_fix returns 400", func(t *testing.T) {
		agent := "alpha"
		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflict-groups/"+uuid.New().String()+"/resolve", adminToken,
			model.ConflictGroupResolveRequest{Status: "wont_fix", WinningAgent: &agent})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("nonexistent group returns 404", func(t *testing.T) {
		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflict-groups/"+uuid.New().String()+"/resolve", adminToken,
			model.ConflictGroupResolveRequest{Status: "resolved"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("resolved without winning_agent resolves all open conflicts", func(t *testing.T) {
		groupID, _, _ := seedConflictGroup(t)
		note := "all caching decisions settled"

		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflict-groups/"+groupID.String()+"/resolve", adminToken,
			model.ConflictGroupResolveRequest{Status: "resolved", ResolutionNote: &note})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var envelope struct {
			Data model.ConflictGroupResolveResult `json:"data"`
		}
		b, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(b, &envelope))
		assert.Equal(t, groupID, envelope.Data.GroupID)
		assert.Equal(t, "resolved", envelope.Data.Status)
		assert.Equal(t, 2, envelope.Data.Resolved, "both open conflicts should be resolved")
	})

	t.Run("resolved with winning_agent sets winning_decision_id", func(t *testing.T) {
		groupID, conflictIDs, _ := seedConflictGroup(t)
		agent := "alpha"
		note := "alpha is authoritative"

		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflict-groups/"+groupID.String()+"/resolve", adminToken,
			model.ConflictGroupResolveRequest{Status: "resolved", WinningAgent: &agent, ResolutionNote: &note})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var envelope struct {
			Data model.ConflictGroupResolveResult `json:"data"`
		}
		b, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(b, &envelope))
		assert.Equal(t, 2, envelope.Data.Resolved)

		// Verify winning_decision_id was set on each conflict.
		for _, cid := range conflictIDs {
			conflict, cErr := testDB.GetConflict(context.Background(), cid, uuid.Nil)
			require.NoError(t, cErr)
			require.NotNil(t, conflict)
			assert.Equal(t, "resolved", conflict.Status)
			require.NotNil(t, conflict.WinningDecisionID, "winning_decision_id should be set")
		}
	})

	t.Run("wont_fix resolves without winner", func(t *testing.T) {
		groupID, conflictIDs, _ := seedConflictGroup(t)

		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflict-groups/"+groupID.String()+"/resolve", adminToken,
			model.ConflictGroupResolveRequest{Status: "wont_fix"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var envelope struct {
			Data model.ConflictGroupResolveResult `json:"data"`
		}
		b, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(b, &envelope))
		assert.Equal(t, "wont_fix", envelope.Data.Status)
		assert.Equal(t, 2, envelope.Data.Resolved)

		for _, cid := range conflictIDs {
			conflict, cErr := testDB.GetConflict(context.Background(), cid, uuid.Nil)
			require.NoError(t, cErr)
			require.NotNil(t, conflict)
			assert.Equal(t, "wont_fix", conflict.Status)
			assert.Nil(t, conflict.WinningDecisionID, "wont_fix should not set winner")
		}
	})

	t.Run("idempotent re-resolve returns 0 affected", func(t *testing.T) {
		groupID, _, _ := seedConflictGroup(t)

		// Resolve first time.
		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflict-groups/"+groupID.String()+"/resolve", adminToken,
			model.ConflictGroupResolveRequest{Status: "resolved"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		// Resolve second time — no open conflicts remain.
		resp2, err := authedRequest("PATCH", testSrv.URL+"/v1/conflict-groups/"+groupID.String()+"/resolve", adminToken,
			model.ConflictGroupResolveRequest{Status: "resolved"})
		require.NoError(t, err)
		defer func() { _ = resp2.Body.Close() }()
		require.Equal(t, http.StatusOK, resp2.StatusCode)

		var envelope struct {
			Data model.ConflictGroupResolveResult `json:"data"`
		}
		b, _ := io.ReadAll(resp2.Body)
		require.NoError(t, json.Unmarshal(b, &envelope))
		assert.Equal(t, 0, envelope.Data.Resolved, "no open conflicts to resolve on second call")
	})
}

// ---- HandleEraseDecision (GDPR tombstone erasure) -------------------------

func TestHandleEraseDecision(t *testing.T) {
	// Trace a decision with alternatives and evidence to erase.
	dt := "erasure_test_" + uuid.NewString()[:8]
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: dt,
				Outcome:      "sensitive PII outcome",
				Confidence:   0.85,
				Reasoning:    ptrStr("contains PII reasoning"),
				Alternatives: []model.TraceAlternative{
					{Label: "alt with PII", RejectionReason: ptrStr("PII rejection reason")},
				},
				Evidence: []model.TraceEvidence{
					{SourceType: "document", Content: "PII evidence content", SourceURI: ptrStr("https://example.com/pii")},
				},
			},
		})
	require.NoError(t, err)
	defer func() { _ = traceResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, traceResp.StatusCode)

	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
			RunID      uuid.UUID `json:"run_id"`
		} `json:"data"`
	}
	traceBody, _ := io.ReadAll(traceResp.Body)
	require.NoError(t, json.Unmarshal(traceBody, &traceResult))
	decisionID := traceResult.Data.DecisionID
	runID := traceResult.Data.RunID
	require.NotEqual(t, uuid.Nil, decisionID)

	t.Run("pre-erasure verify returns verified", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/verify/"+decisionID.String(), adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		var result struct {
			Data map[string]any `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(data, &result))
		assert.Equal(t, "verified", result.Data["status"])
	})

	t.Run("agent role gets 403", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/decisions/"+decisionID.String()+"/erase", agentToken,
			map[string]string{"reason": "GDPR request"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("admin role gets 403", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/decisions/"+decisionID.String()+"/erase", adminToken,
			map[string]string{"reason": "GDPR request"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("invalid UUID returns 400", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/decisions/not-a-uuid/erase", orgOwnerToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("nonexistent ID returns 404", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/decisions/"+uuid.New().String()+"/erase", orgOwnerToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("successful erasure", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/decisions/"+decisionID.String()+"/erase", orgOwnerToken,
			map[string]string{"reason": "GDPR data subject request #42"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data map[string]any `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(data, &result))
		assert.Equal(t, decisionID.String(), result.Data["decision_id"])
		assert.NotEmpty(t, result.Data["erased_at"])
		assert.NotEmpty(t, result.Data["original_hash"])
		assert.NotEmpty(t, result.Data["erased_hash"])
		assert.NotEqual(t, result.Data["original_hash"], result.Data["erased_hash"])
	})

	t.Run("decision still retrievable but scrubbed", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+decisionID.String(), agentToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data model.Decision `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(data, &result))
		assert.Equal(t, "[erased]", result.Data.Outcome)
		assert.NotNil(t, result.Data.Reasoning)
		assert.Equal(t, "[erased]", *result.Data.Reasoning)
		assert.Nil(t, result.Data.ValidTo, "erasure must NOT set valid_to")
	})

	t.Run("verify returns erased status", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/verify/"+decisionID.String(), adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data map[string]any `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(data, &result))
		assert.Equal(t, "erased", result.Data["status"])
		assert.Equal(t, true, result.Data["valid"])
		assert.NotEmpty(t, result.Data["original_hash"])
		assert.NotEmpty(t, result.Data["erased_at"])
		assert.NotEmpty(t, result.Data["erased_by"])
	})

	t.Run("double erasure returns 409", func(t *testing.T) {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/decisions/"+decisionID.String()+"/erase", orgOwnerToken,
			map[string]string{"reason": "second attempt"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusConflict, resp.StatusCode)
	})

	t.Run("DecisionErased event recorded", func(t *testing.T) {
		orgID := uuid.Nil // default org from SeedAdmin
		events, err := testDB.GetEventsByRun(context.Background(), orgID, runID, 100)
		require.NoError(t, err)

		var found bool
		for _, ev := range events {
			if ev.EventType == model.EventDecisionErased {
				found = true
				assert.Equal(t, decisionID.String(), ev.Payload["decision_id"])
				assert.NotEmpty(t, ev.Payload["erased_by"])
				assert.NotEmpty(t, ev.Payload["original_hash"])
				break
			}
		}
		assert.True(t, found, "expected a DecisionErased event in the run's event log")
	})
}

func TestConflictAnalytics(t *testing.T) {
	orgID := uuid.Nil

	// Trace four decisions so we can create conflicts referencing them.
	traceDecision := func(agentID, outcome string, confidence float32) uuid.UUID {
		t.Helper()
		resp, tErr := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, model.TraceRequest{
			AgentID: agentID,
			Decision: model.TraceDecision{
				DecisionType: "architecture",
				Outcome:      outcome,
				Confidence:   confidence,
			},
		})
		require.NoError(t, tErr)
		defer func() { _ = resp.Body.Close() }()
		require.Contains(t, []int{http.StatusOK, http.StatusCreated, http.StatusAccepted}, resp.StatusCode)
		var result struct {
			Data struct {
				DecisionID uuid.UUID `json:"decision_id"`
			} `json:"data"`
		}
		b, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(b, &result))
		return result.Data.DecisionID
	}

	d1 := traceDecision("alpha", "analytics-test: use Postgres", 0.9)
	d2 := traceDecision("beta", "analytics-test: use MySQL", 0.8)
	d3 := traceDecision("alpha", "analytics-test: use Redis", 0.7)
	d4 := traceDecision("gamma", "analytics-test: use Memcached", 0.6)
	require.NoError(t, testBuf.FlushNow(context.Background()))

	now := time.Now().UTC()
	threeDaysAgo := now.Add(-3 * 24 * time.Hour)
	fiveDaysAgo := now.Add(-5 * 24 * time.Hour)

	severity := func(s string) *string { return &s }

	// Insert two conflicts with different agents, severities, and dates.
	c1 := model.DecisionConflict{
		OrgID:         orgID,
		ConflictKind:  model.ConflictKindCrossAgent,
		DecisionAID:   d1,
		DecisionBID:   d2,
		AgentA:        "alpha",
		AgentB:        "beta",
		DecisionTypeA: "architecture",
		DecisionTypeB: "architecture",
		OutcomeA:      "analytics-test: use Postgres",
		OutcomeB:      "analytics-test: use MySQL",
		Status:        "open",
		Severity:      severity("high"),
	}
	id1, err := testDB.InsertScoredConflict(context.Background(), c1)
	require.NoError(t, err)

	c2 := model.DecisionConflict{
		OrgID:         orgID,
		ConflictKind:  model.ConflictKindCrossAgent,
		DecisionAID:   d3,
		DecisionBID:   d4,
		AgentA:        "alpha",
		AgentB:        "gamma",
		DecisionTypeA: "architecture",
		DecisionTypeB: "architecture",
		OutcomeA:      "analytics-test: use Redis",
		OutcomeB:      "analytics-test: use Memcached",
		Status:        "resolved",
		Severity:      severity("medium"),
	}
	id2, err := testDB.InsertScoredConflict(context.Background(), c2)
	require.NoError(t, err)

	// Backdate detected_at and set resolved_at for the second conflict.
	_, err = testDB.Pool().Exec(context.Background(),
		`UPDATE scored_conflicts SET detected_at = $1 WHERE id = $2`, fiveDaysAgo, id1)
	require.NoError(t, err)
	_, err = testDB.Pool().Exec(context.Background(),
		`UPDATE scored_conflicts SET detected_at = $1, resolved_at = $2, resolved_by = 'test', status = 'resolved' WHERE id = $3`,
		threeDaysAgo, now, id2)
	require.NoError(t, err)

	t.Run("default period returns analytics", func(t *testing.T) {
		resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/analytics", adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data model.ConflictAnalytics `json:"data"`
		}
		body, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(body, &result))
		analytics := result.Data

		assert.False(t, analytics.Period.Start.IsZero())
		assert.False(t, analytics.Period.End.IsZero())
		// Both conflicts are within the default 7d window.
		assert.GreaterOrEqual(t, analytics.Summary.TotalDetected, 2)
		assert.GreaterOrEqual(t, analytics.Summary.TotalResolved, 1)
		assert.NotNil(t, analytics.ByAgentPair)
		assert.NotNil(t, analytics.BySeverity)
		assert.NotNil(t, analytics.ByDecisionType)
		assert.NotNil(t, analytics.Trend)
		// Trend should have 7 entries for a 7d period (one per day).
		assert.Len(t, analytics.Trend, 7)
	})

	t.Run("explicit from/to", func(t *testing.T) {
		from := fiveDaysAgo.Add(-1 * time.Hour).Format(time.RFC3339)
		to := now.Add(1 * time.Hour).Format(time.RFC3339)
		resp, err := authedRequest("GET",
			testSrv.URL+"/v1/conflicts/analytics?from="+from+"&to="+to, adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("agent_id filter", func(t *testing.T) {
		resp, err := authedRequest("GET",
			testSrv.URL+"/v1/conflicts/analytics?agent_id=alpha", adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data model.ConflictAnalytics `json:"data"`
		}
		body, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(body, &result))
		// alpha appears in both conflicts.
		assert.GreaterOrEqual(t, result.Data.Summary.TotalDetected, 2)
	})

	t.Run("invalid period returns 400", func(t *testing.T) {
		resp, err := authedRequest("GET",
			testSrv.URL+"/v1/conflicts/analytics?period=999d", adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("from after to returns 400", func(t *testing.T) {
		from := now.Format(time.RFC3339)
		to := fiveDaysAgo.Format(time.RFC3339)
		resp, err := authedRequest("GET",
			testSrv.URL+"/v1/conflicts/analytics?from="+from+"&to="+to, adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

// ---------------------------------------------------------------------------
// Retention & Legal Hold endpoint tests (issue #265)
// ---------------------------------------------------------------------------

func TestHandleGetRetention_Default(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/retention", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			RetentionDays         *int            `json:"retention_days"`
			RetentionExcludeTypes []string        `json:"retention_exclude_types"`
			LastRun               *time.Time      `json:"last_run"`
			LastRunDeleted        *int            `json:"last_run_deleted"`
			NextRun               *time.Time      `json:"next_run"`
			Holds                 json.RawMessage `json:"holds"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))

	// Default org has no retention policy set, so retention_days should be nil.
	assert.Nil(t, result.Data.RetentionDays, "default org should have nil retention_days")
	assert.Nil(t, result.Data.LastRun, "no prior run expected")
}

func TestHandleGetRetention_ForbiddenUnauthenticated(t *testing.T) {
	// Unauthenticated request should be rejected.
	resp, err := http.Get(testSrv.URL + "/v1/retention")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHandleSetRetention_Valid(t *testing.T) {
	days := 30
	resp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken, map[string]any{
		"retention_days":          days,
		"retention_exclude_types": []string{"security", "compliance"},
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			RetentionDays         *int     `json:"retention_days"`
			RetentionExcludeTypes []string `json:"retention_exclude_types"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	require.NotNil(t, result.Data.RetentionDays)
	assert.Equal(t, 30, *result.Data.RetentionDays)
	assert.Equal(t, []string{"security", "compliance"}, result.Data.RetentionExcludeTypes)

	// Clean up: reset to nil so we don't affect other tests.
	resetResp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken, map[string]any{
		"retention_days":          nil,
		"retention_exclude_types": nil,
	})
	require.NoError(t, err)
	defer func() { _ = resetResp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resetResp.StatusCode)
}

func TestHandleSetRetention_InvalidDays(t *testing.T) {
	// Zero days should fail validation (must be >= 1).
	resp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken, map[string]any{
		"retention_days": 0,
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// Negative days should also fail.
	resp2, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken, map[string]any{
		"retention_days": -5,
	})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp2.StatusCode)
}

func TestHandleSetRetention_ForbiddenForAgent(t *testing.T) {
	resp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", agentToken, map[string]any{
		"retention_days": 60,
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandleCreateHold_Valid(t *testing.T) {
	from := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	to := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken, map[string]any{
		"reason": "Legal review Q1",
		"from":   from.Format(time.RFC3339),
		"to":     to.Format(time.RFC3339),
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	var result struct {
		Data struct {
			ID        string `json:"id"`
			Reason    string `json:"reason"`
			CreatedBy string `json:"created_by"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.NotEmpty(t, result.Data.ID, "hold should have an ID")
	assert.Equal(t, "Legal review Q1", result.Data.Reason)
	assert.NotEmpty(t, result.Data.CreatedBy)
}

func TestHandleCreateHold_MissingReason(t *testing.T) {
	from := time.Now().Add(-24 * time.Hour).UTC()
	to := time.Now().Add(24 * time.Hour).UTC()

	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken, map[string]any{
		"reason": "",
		"from":   from.Format(time.RFC3339),
		"to":     to.Format(time.RFC3339),
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateHold_MissingTimeRange(t *testing.T) {
	// Missing both from and to.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken, map[string]any{
		"reason": "Missing times",
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateHold_ToBeforeFrom(t *testing.T) {
	from := time.Now().Add(24 * time.Hour).UTC()
	to := time.Now().Add(-24 * time.Hour).UTC()

	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken, map[string]any{
		"reason": "Backwards range",
		"from":   from.Format(time.RFC3339),
		"to":     to.Format(time.RFC3339),
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateHold_ForbiddenForAgent(t *testing.T) {
	from := time.Now().Add(-24 * time.Hour).UTC()
	to := time.Now().Add(24 * time.Hour).UTC()

	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", agentToken, map[string]any{
		"reason": "Should fail",
		"from":   from.Format(time.RFC3339),
		"to":     to.Format(time.RFC3339),
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandleReleaseHold_Valid(t *testing.T) {
	from := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	to := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

	// Create a hold to release.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken, map[string]any{
		"reason": "To be released",
		"from":   from.Format(time.RFC3339),
		"to":     to.Format(time.RFC3339),
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var createResult struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &createResult))
	holdID := createResult.Data.ID
	require.NotEmpty(t, holdID)

	// Release the hold.
	resp2, err := authedRequest("DELETE", testSrv.URL+"/v1/retention/hold/"+holdID, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusNoContent, resp2.StatusCode)

	// Releasing the same hold again should return 404.
	resp3, err := authedRequest("DELETE", testSrv.URL+"/v1/retention/hold/"+holdID, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp3.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp3.StatusCode)
}

func TestHandleReleaseHold_NotFound(t *testing.T) {
	unknownID := uuid.New().String()
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/retention/hold/"+unknownID, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleReleaseHold_InvalidID(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/retention/hold/not-a-uuid", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandlePurge_DryRun(t *testing.T) {
	before := time.Now().Add(-365 * 24 * time.Hour).UTC()

	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/purge", adminToken, map[string]any{
		"before":  before.Format(time.RFC3339),
		"dry_run": true,
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			DryRun      bool `json:"dry_run"`
			WouldDelete struct {
				Decisions    int64 `json:"decisions"`
				Alternatives int64 `json:"alternatives"`
				Evidence     int64 `json:"evidence"`
				Claims       int64 `json:"claims"`
				Events       int64 `json:"events"`
			} `json:"would_delete"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.True(t, result.Data.DryRun, "response should indicate dry_run=true")
	// Count should be >= 0 (we don't care about exact value, just valid response).
	assert.GreaterOrEqual(t, result.Data.WouldDelete.Decisions, int64(0))
}

func TestHandlePurge_MissingBefore(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/purge", adminToken, map[string]any{
		"dry_run": true,
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandlePurge_ForbiddenForAgent(t *testing.T) {
	before := time.Now().Add(-365 * 24 * time.Hour).UTC()

	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/purge", agentToken, map[string]any{
		"before":  before.Format(time.RFC3339),
		"dry_run": true,
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandleGetRetention_ShowsHoldsAfterCreate(t *testing.T) {
	from := time.Now().Add(-12 * time.Hour).UTC().Truncate(time.Second)
	to := time.Now().Add(12 * time.Hour).UTC().Truncate(time.Second)

	// Create a hold.
	createResp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken, map[string]any{
		"reason": "Visible in GET",
		"from":   from.Format(time.RFC3339),
		"to":     to.Format(time.RFC3339),
	})
	require.NoError(t, err)
	defer func() { _ = createResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, createResp.StatusCode)

	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	createBody, _ := io.ReadAll(createResp.Body)
	require.NoError(t, json.Unmarshal(createBody, &created))
	holdID := created.Data.ID

	// GET /v1/retention should include the hold we just created.
	getResp, err := authedRequest("GET", testSrv.URL+"/v1/retention", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = getResp.Body.Close() }()
	require.Equal(t, http.StatusOK, getResp.StatusCode)

	var getResult struct {
		Data struct {
			Holds []struct {
				ID     string `json:"id"`
				Reason string `json:"reason"`
			} `json:"holds"`
		} `json:"data"`
	}
	getBody, _ := io.ReadAll(getResp.Body)
	require.NoError(t, json.Unmarshal(getBody, &getResult))

	found := false
	for _, h := range getResult.Data.Holds {
		if h.ID == holdID {
			found = true
			assert.Equal(t, "Visible in GET", h.Reason)
			break
		}
	}
	assert.True(t, found, "newly created hold should appear in GET /v1/retention holds list")
}

// ---------------------------------------------------------------------------
// HandleCreateAgent tests
// ---------------------------------------------------------------------------

func TestHandleCreateAgent_HappyPath(t *testing.T) {
	agentID := "create-agent-" + uuid.New().String()[:8]
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken,
		model.CreateAgentRequest{
			AgentID: agentID,
			Name:    "Test Create Agent",
			Role:    model.RoleAgent,
			APIKey:  "test-key-" + agentID,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	var result struct {
		Data struct {
			Agent  model.Agent `json:"agent"`
			APIKey struct {
				ID     uuid.UUID `json:"id"`
				Prefix string    `json:"prefix"`
			} `json:"api_key"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, agentID, result.Data.Agent.AgentID)
	assert.Equal(t, "Test Create Agent", result.Data.Agent.Name)
	assert.Equal(t, model.RoleAgent, result.Data.Agent.Role)
}

func TestHandleCreateAgent_ServerGeneratedKey(t *testing.T) {
	agentID := "create-srvkey-" + uuid.New().String()[:8]
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken,
		model.CreateAgentRequest{
			AgentID: agentID,
			Name:    "Server Key Agent",
			// No APIKey — server generates one.
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	var result struct {
		Data struct {
			RawKey string `json:"raw_key"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.NotEmpty(t, result.Data.RawKey, "server-generated key should be returned once")
}

func TestHandleCreateAgent_MissingFields(t *testing.T) {
	tests := []struct {
		name string
		req  model.CreateAgentRequest
	}{
		{name: "missing agent_id", req: model.CreateAgentRequest{Name: "Agent"}},
		{name: "missing name", req: model.CreateAgentRequest{AgentID: "some-id"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken, tt.req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

func TestHandleCreateAgent_DuplicateAgentID(t *testing.T) {
	agentID := "dup-agent-" + uuid.New().String()[:8]
	// Create first.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken,
		model.CreateAgentRequest{AgentID: agentID, Name: "First", APIKey: "key-" + agentID})
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	// Create duplicate.
	resp2, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken,
		model.CreateAgentRequest{AgentID: agentID, Name: "Second", APIKey: "key2-" + agentID})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusConflict, resp2.StatusCode)
}

func TestHandleCreateAgent_InvalidRole(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken,
		model.CreateAgentRequest{
			AgentID: "invalid-role-agent",
			Name:    "Bad Role",
			Role:    "superadmin",
			APIKey:  "key-invalid-role",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateAgent_CannotEscalateRole(t *testing.T) {
	// Admin (rank 3) cannot create platform_admin (rank 5).
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken,
		model.CreateAgentRequest{
			AgentID: "escalation-attempt",
			Name:    "Escalation",
			Role:    model.RolePlatformAdmin,
			APIKey:  "key-escalation",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandleCreateAgent_ForbiddenForAgentRole(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", agentToken,
		model.CreateAgentRequest{AgentID: "agent-attempt", Name: "Attempt", APIKey: "k"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandleCreateAgent_ReservedID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken,
		model.CreateAgentRequest{AgentID: "admin", Name: "Sneaky Admin", APIKey: "k"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateAgent_WithTags(t *testing.T) {
	agentID := "tagged-agent-" + uuid.New().String()[:8]
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken,
		model.CreateAgentRequest{
			AgentID: agentID,
			Name:    "Tagged Agent",
			Tags:    []string{"backend", "production"},
			APIKey:  "key-" + agentID,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	var result struct {
		Data struct {
			Agent model.Agent `json:"agent"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, []string{"backend", "production"}, result.Data.Agent.Tags)
}

// ---------------------------------------------------------------------------
// HandleListAgents tests
// ---------------------------------------------------------------------------

func TestHandleListAgents_HappyPath(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data  []model.Agent `json:"data"`
		Total *int          `json:"total"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Greater(t, len(result.Data), 0)
	require.NotNil(t, result.Total)
	assert.Greater(t, *result.Total, 0)
}

func TestHandleListAgents_WithStats(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents?include=stats", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data []struct {
			AgentID       string `json:"agent_id"`
			DecisionCount int    `json:"decision_count"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Greater(t, len(result.Data), 0)
}

func TestHandleListAgents_Pagination(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents?limit=1&offset=0", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data    []model.Agent `json:"data"`
		Total   *int          `json:"total"`
		HasMore bool          `json:"has_more"`
		Limit   int           `json:"limit"`
		Offset  int           `json:"offset"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Len(t, result.Data, 1)
	assert.Equal(t, 1, result.Limit)
	assert.Equal(t, 0, result.Offset)
	assert.True(t, result.HasMore, "there are multiple agents so has_more should be true")
}

func TestHandleListAgents_ForbiddenForAgentRole(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// HandleGetAgent tests
// ---------------------------------------------------------------------------

func TestHandleGetAgent_HappyPath(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data model.Agent `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, "test-agent", result.Data.AgentID)
	assert.Equal(t, "Test Agent", result.Data.Name)
}

func TestHandleGetAgent_NotFound(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/nonexistent-agent-xyz", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleGetAgent_InvalidAgentID(t *testing.T) {
	// Agent IDs must match a pattern; try one with invalid characters.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/INVALID%20AGENT!", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleGetAgent_ForbiddenForAgentRole(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// HandleUpdateAgent tests
// ---------------------------------------------------------------------------

func TestHandleUpdateAgent_UpdateName(t *testing.T) {
	// Create a dedicated agent to update.
	agentID := "update-name-" + uuid.New().String()[:8]
	createAgent(testSrv.URL, adminToken, agentID, "Original Name", "agent", "key-"+agentID)

	newName := "Updated Name"
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/"+agentID, adminToken,
		model.UpdateAgentRequest{Name: &newName})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data model.Agent `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, "Updated Name", result.Data.Name)
}

func TestHandleUpdateAgent_UpdateMetadata(t *testing.T) {
	agentID := "update-meta-" + uuid.New().String()[:8]
	createAgent(testSrv.URL, adminToken, agentID, "Meta Agent", "agent", "key-"+agentID)

	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/"+agentID, adminToken,
		model.UpdateAgentRequest{Metadata: map[string]any{"team": "platform"}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data model.Agent `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, "platform", result.Data.Metadata["team"])
}

func TestHandleUpdateAgent_NoFieldsProvided(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/test-agent", adminToken,
		model.UpdateAgentRequest{})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleUpdateAgent_EmptyName(t *testing.T) {
	empty := ""
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/test-agent", adminToken,
		model.UpdateAgentRequest{Name: &empty})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleUpdateAgent_NotFound(t *testing.T) {
	newName := "Some Name"
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/nonexistent-agent-xyz", adminToken,
		model.UpdateAgentRequest{Name: &newName})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleUpdateAgent_ForbiddenForAgentRole(t *testing.T) {
	newName := "Hacked Name"
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/test-agent", agentToken,
		model.UpdateAgentRequest{Name: &newName})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// HandleAgentStats tests
// ---------------------------------------------------------------------------

func TestHandleAgentStats_HappyPath(t *testing.T) {
	// test-agent has decisions from earlier test runs.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent/stats", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			AgentID string `json:"agent_id"`
			Stats   struct {
				DecisionCount int `json:"decision_count"`
			} `json:"stats"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, "test-agent", result.Data.AgentID)
	assert.GreaterOrEqual(t, result.Data.Stats.DecisionCount, 0)
}

func TestHandleAgentStats_NotFound(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/nonexistent-agent-xyz/stats", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleAgentStats_InvalidAgentID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/INVALID%20AGENT!/stats", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleAgentStats_ForbiddenForAgentRole(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent/stats", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// HandleListAssessments tests
// ---------------------------------------------------------------------------

func TestHandleListAssessments_HappyPath(t *testing.T) {
	// Create a decision via trace, then assess it, then list assessments.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: "assessment_list_test",
				Outcome:      "approved",
				Confidence:   0.85,
			},
		})
	require.NoError(t, err)
	defer func() { _ = traceResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, traceResp.StatusCode)

	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	traceBody, _ := io.ReadAll(traceResp.Body)
	require.NoError(t, json.Unmarshal(traceBody, &traceResult))
	decisionID := traceResult.Data.DecisionID

	// Assess the decision.
	assessResp, err := authedRequest("POST",
		testSrv.URL+"/v1/decisions/"+decisionID.String()+"/assess", agentToken,
		model.AssessRequest{Outcome: model.AssessmentCorrect})
	require.NoError(t, err)
	defer func() { _ = assessResp.Body.Close() }()
	require.Equal(t, http.StatusOK, assessResp.StatusCode)

	// List assessments.
	listResp, err := authedRequest("GET",
		testSrv.URL+"/v1/decisions/"+decisionID.String()+"/assessments", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = listResp.Body.Close() }()
	assert.Equal(t, http.StatusOK, listResp.StatusCode)

	var listResult struct {
		Data struct {
			DecisionID  uuid.UUID                  `json:"decision_id"`
			Assessments []model.DecisionAssessment `json:"assessments"`
			Count       int                        `json:"count"`
			Summary     model.AssessmentSummary    `json:"summary"`
		} `json:"data"`
	}
	listBody, _ := io.ReadAll(listResp.Body)
	require.NoError(t, json.Unmarshal(listBody, &listResult))
	assert.Equal(t, decisionID, listResult.Data.DecisionID)
	assert.GreaterOrEqual(t, listResult.Data.Count, 1)
	assert.GreaterOrEqual(t, len(listResult.Data.Assessments), 1)
	assert.GreaterOrEqual(t, listResult.Data.Summary.Correct, 1)
}

func TestHandleListAssessments_DecisionNotFound(t *testing.T) {
	fakeID := uuid.New()
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/decisions/"+fakeID.String()+"/assessments", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleListAssessments_InvalidDecisionID(t *testing.T) {
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/decisions/not-a-uuid/assessments", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleListAssessments_EmptyList(t *testing.T) {
	// Create a decision but do NOT assess it.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: "assessment_empty_test",
				Outcome:      "denied",
				Confidence:   0.5,
			},
		})
	require.NoError(t, err)
	defer func() { _ = traceResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, traceResp.StatusCode)

	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	traceBody, _ := io.ReadAll(traceResp.Body)
	require.NoError(t, json.Unmarshal(traceBody, &traceResult))

	listResp, err := authedRequest("GET",
		testSrv.URL+"/v1/decisions/"+traceResult.Data.DecisionID.String()+"/assessments", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = listResp.Body.Close() }()
	assert.Equal(t, http.StatusOK, listResp.StatusCode)

	var listResult struct {
		Data struct {
			Count       int                        `json:"count"`
			Assessments []model.DecisionAssessment `json:"assessments"`
		} `json:"data"`
	}
	listBody, _ := io.ReadAll(listResp.Body)
	require.NoError(t, json.Unmarshal(listBody, &listResult))
	assert.Equal(t, 0, listResult.Data.Count)
	assert.Empty(t, listResult.Data.Assessments)
}

// ---------------------------------------------------------------------------
// HandleListConflictGroups tests
// ---------------------------------------------------------------------------

func TestHandleListConflictGroups_HappyPath(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflict-groups", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data    []model.ConflictGroup `json:"data"`
		Total   *int                  `json:"total"`
		HasMore bool                  `json:"has_more"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	// Should not be nil; handler ensures JSON array.
	assert.NotNil(t, result.Data)
}

func TestHandleListConflictGroups_WithFilters(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "status=open", query: "?status=open"},
		{name: "decision_type filter", query: "?decision_type=architecture"},
		{name: "agent_id filter", query: "?agent_id=alpha"},
		{name: "conflict_kind filter", query: "?conflict_kind=cross_agent"},
		{name: "combined filters", query: "?status=open&decision_type=architecture&limit=5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := authedRequest("GET", testSrv.URL+"/v1/conflict-groups"+tt.query, adminToken, nil)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusOK, resp.StatusCode)
		})
	}
}

func TestHandleListConflictGroups_Pagination(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflict-groups?limit=1&offset=0", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, 1, result.Limit)
	assert.Equal(t, 0, result.Offset)
}

func TestHandleListConflictGroups_UnauthenticatedReturns401(t *testing.T) {
	resp, err := http.Get(testSrv.URL + "/v1/conflict-groups")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// HandleConflictAnalytics additional edge-case tests
// ---------------------------------------------------------------------------

func TestHandleConflictAnalytics_ValidPeriods(t *testing.T) {
	for _, period := range []string{"7d", "30d", "90d"} {
		t.Run("period="+period, func(t *testing.T) {
			resp, err := authedRequest("GET",
				testSrv.URL+"/v1/conflicts/analytics?period="+period, adminToken, nil)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusOK, resp.StatusCode)
		})
	}
}

func TestHandleConflictAnalytics_UnauthenticatedReturns401(t *testing.T) {
	resp, err := http.Get(testSrv.URL + "/v1/conflicts/analytics")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHandleConflictAnalytics_ConflictKindFilter(t *testing.T) {
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/conflicts/analytics?conflict_kind=cross_agent", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleConflictAnalytics_DecisionTypeFilter(t *testing.T) {
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/conflicts/analytics?decision_type=architecture", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// HandleAgentHistory tests
// ---------------------------------------------------------------------------

func TestHandleAgentHistory_HappyPath(t *testing.T) {
	// Create a fresh decision so the agent definitely has history.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: "history_test",
				Outcome:      "approved",
				Confidence:   0.8,
			},
		})
	require.NoError(t, err)
	_ = traceResp.Body.Close()
	require.Equal(t, http.StatusCreated, traceResp.StatusCode)

	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/agents/test-agent/history", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data  []model.Decision `json:"data"`
		Total *int             `json:"total"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Greater(t, len(result.Data), 0)
	require.NotNil(t, result.Total)
	assert.Greater(t, *result.Total, 0)
}

func TestHandleAgentHistory_Pagination(t *testing.T) {
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/agents/test-agent/history?limit=1&offset=0", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data   []model.Decision `json:"data"`
		Limit  int              `json:"limit"`
		Offset int              `json:"offset"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.LessOrEqual(t, len(result.Data), 1)
	assert.Equal(t, 1, result.Limit)
	assert.Equal(t, 0, result.Offset)
}

func TestHandleAgentHistory_WithTimeRange(t *testing.T) {
	now := time.Now().UTC()
	from := now.Add(-24 * time.Hour).Format(time.RFC3339)
	to := now.Add(1 * time.Hour).Format(time.RFC3339)

	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/agents/test-agent/history?from="+from+"&to="+to,
		agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleAgentHistory_InvalidAgentID(t *testing.T) {
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/agents/INVALID%20AGENT!/history", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleAgentHistory_ForbiddenForUnrelatedAgent(t *testing.T) {
	// Create a second agent and try to access a different agent's history
	// without a grant. The agent role can access its own history.
	otherAgentID := "history-other-" + uuid.New().String()[:8]
	createAgent(testSrv.URL, adminToken, otherAgentID, "Other Agent", "agent", "key-"+otherAgentID)
	otherToken := getToken(testSrv.URL, otherAgentID, "key-"+otherAgentID)

	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/agents/test-agent/history", otherToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandleAgentHistory_AdminCanViewAnyAgent(t *testing.T) {
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/agents/test-agent/history", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleAgentHistory_InvalidTimeFormat(t *testing.T) {
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/agents/test-agent/history?from=not-a-time", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// HandleValidatePair tests
// ---------------------------------------------------------------------------

func TestHandleValidatePair_NoValidatorReturns501(t *testing.T) {
	// The test server has no conflict validator configured, so it should return 501.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/admin/conflicts/validate-pair", adminToken,
		map[string]any{
			"outcome_a": "use Postgres",
			"outcome_b": "use MySQL",
			"type_a":    "architecture",
			"type_b":    "architecture",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

func TestHandleValidatePair_ForbiddenForAgentRole(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/admin/conflicts/validate-pair", agentToken,
		map[string]any{
			"outcome_a": "use Postgres",
			"outcome_b": "use MySQL",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// HandleMCPInfo tests
// ---------------------------------------------------------------------------

func TestHandleMCPInfo_HappyPath(t *testing.T) {
	// /mcp/info requires auth because /mcp is an authenticated prefix.
	resp, err := authedRequest("GET", testSrv.URL+"/mcp/info", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data model.MCPInfoResponse `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, "test", result.Data.Version)
	assert.Equal(t, "streamable-http", result.Data.Transport)
	assert.Contains(t, result.Data.Auth.Schemes, "ApiKey")
	assert.Contains(t, result.Data.Auth.Schemes, "Bearer")
	assert.Equal(t, "ApiKey", result.Data.Auth.Preferred)
	assert.NotEmpty(t, result.Data.Auth.Note)
}

func TestHandleMCPInfo_UnauthenticatedReturns401(t *testing.T) {
	// /mcp/info is under the /mcp authenticated prefix.
	resp, err := http.Get(testSrv.URL + "/mcp/info")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHandleMCPInfo_ResponseStructure(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/mcp/info", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	data, ok := raw["data"].(map[string]any)
	require.True(t, ok, "response should have a data field")
	assert.Contains(t, data, "version")
	assert.Contains(t, data, "transport")
	assert.Contains(t, data, "auth")

	authMap, ok := data["auth"].(map[string]any)
	require.True(t, ok, "auth should be an object")
	assert.Contains(t, authMap, "schemes")
	assert.Contains(t, authMap, "preferred")
	assert.Contains(t, authMap, "note")
}
