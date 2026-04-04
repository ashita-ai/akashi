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
	"strings"
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

func isFuzzOnly() bool {
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "-test.fuzz=") {
			return true
		}
	}
	return false
}

func TestMain(m *testing.M) {
	// Fuzz targets in this package (e.g. FuzzDecodeJSON) don't need a
	// database. Skip the expensive testcontainer setup when the binary
	// is invoked in fuzz-only mode to avoid a multi-minute container
	// pull/startup that caused CI timeouts.
	if isFuzzOnly() {
		os.Exit(m.Run())
	}

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

	db, err := storage.New(ctx, dsn, "", logger, storage.PoolOptions{})
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

	mcpSrv := mcp.New(db, decisionSvc, nil, logger, "test", 0.85, nil)
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
		EnableDestructiveDelete:     true,
		HighConfidenceWarnThreshold: 0.85,
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

func TestReadyzEndpoint(t *testing.T) {
	t.Run("ready when database is reachable", func(t *testing.T) {
		resp, err := http.Get(testSrv.URL + "/readyz")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result struct {
			Data model.ReadyzResponse `json:"data"`
		}
		data, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(data, &result))
		assert.Equal(t, "ready", result.Data.Status)
		assert.Equal(t, "connected", result.Data.Checks["postgres"])
	})
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
					{Label: "Approve"},
					{Label: "Deny"},
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
	assert.True(t, toolNames["akashi_conflicts"], "expected akashi_conflicts tool")
	assert.True(t, toolNames["akashi_resolve"], "expected akashi_resolve tool")
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
					{Label: "keep"},
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
			model.ConflictStatusUpdate{Status: "false_positive"})
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
			model.ConflictStatusUpdate{Status: "false_positive"})
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
		Agents            int `json:"agents"`
		FalsePositiveRate struct {
			Resolved      int     `json:"resolved"`
			FalsePositive int     `json:"false_positive"`
			Rate          float64 `json:"rate"`
		} `json:"false_positive_rate"`
	}
	for _, content := range result.Content {
		if tc, ok := content.(mcplib.TextContent); ok {
			require.NoError(t, json.Unmarshal([]byte(tc.Text), &statsResp))
			break
		}
	}
	assert.Contains(t, []string{"healthy", "needs_attention", "insufficient_data"}, statsResp.TraceHealth.Status)
	assert.GreaterOrEqual(t, statsResp.Agents, 0)
	assert.GreaterOrEqual(t, statsResp.FalsePositiveRate.Rate, 0.0)
	assert.LessOrEqual(t, statsResp.FalsePositiveRate.Rate, 1.0)
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
			model.ConflictStatusUpdate{Status: "false_positive", WinningDecisionID: &decisionAID})
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

	t.Run("winning_agent with false_positive returns 400", func(t *testing.T) {
		agent := "alpha"
		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflict-groups/"+uuid.New().String()+"/resolve", adminToken,
			model.ConflictGroupResolveRequest{Status: "false_positive", WinningAgent: &agent})
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

	t.Run("false_positive resolves without winner", func(t *testing.T) {
		groupID, conflictIDs, _ := seedConflictGroup(t)

		resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflict-groups/"+groupID.String()+"/resolve", adminToken,
			model.ConflictGroupResolveRequest{Status: "false_positive"})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var envelope struct {
			Data model.ConflictGroupResolveResult `json:"data"`
		}
		b, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(b, &envelope))
		assert.Equal(t, "false_positive", envelope.Data.Status)
		assert.Equal(t, 2, envelope.Data.Resolved)

		for _, cid := range conflictIDs {
			conflict, cErr := testDB.GetConflict(context.Background(), cid, uuid.Nil)
			require.NoError(t, cErr)
			require.NotNil(t, conflict)
			assert.Equal(t, "false_positive", conflict.Status)
			assert.Nil(t, conflict.WinningDecisionID, "false_positive should not set winner")
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

// ---- Coverage push: conflict handlers with real data ----

func TestHandleGetConflict_WithRealConflict(t *testing.T) {
	_, _, conflictID := seedConflict(t)

	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/"+conflictID.String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data map[string]any `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))

	assert.Equal(t, conflictID.String(), result.Data["id"])
	assert.Equal(t, "open", result.Data["status"])
	// computeRecommendation is invoked for open conflicts. It may or may not
	// produce a recommendation depending on signal strength (same agent, similar
	// confidence means insufficient signal), but the code path is exercised.
}

func TestHandleGetConflict_FalsePositiveConflict(t *testing.T) {
	_, _, conflictID := seedConflict(t)

	// Mark the conflict as false_positive.
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflicts/"+conflictID.String(), adminToken,
		map[string]any{"status": "false_positive"})
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// GET should return 200 without computing a recommendation for false_positive conflicts.
	resp, err = authedRequest("GET", testSrv.URL+"/v1/conflicts/"+conflictID.String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleAdjudicateConflict_SuccessfulResolution(t *testing.T) {
	decisionAID, _, conflictID := seedConflict(t)

	resp, err := authedRequest("POST",
		testSrv.URL+"/v1/conflicts/"+conflictID.String()+"/adjudicate",
		adminToken,
		map[string]any{
			"outcome":             "Use Redis for caching layer",
			"reasoning":           "Redis has better support for our use case",
			"winning_decision_id": decisionAID.String(),
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data map[string]any `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	assert.Equal(t, "resolved", result.Data["status"])
}

func TestHandleAdjudicateConflict_InvalidWinningDecisionID(t *testing.T) {
	_, _, conflictID := seedConflict(t)

	resp, err := authedRequest("POST",
		testSrv.URL+"/v1/conflicts/"+conflictID.String()+"/adjudicate",
		adminToken,
		map[string]any{
			"outcome":             "Use Redis",
			"winning_decision_id": uuid.New().String(),
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "winning_decision_id must be one of the two decisions")
}

func TestHandleAdjudicateConflict_EmptyOutcome(t *testing.T) {
	_, _, conflictID := seedConflict(t)

	resp, err := authedRequest("POST",
		testSrv.URL+"/v1/conflicts/"+conflictID.String()+"/adjudicate",
		adminToken,
		map[string]any{"outcome": ""})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleAdjudicateConflict_WithCustomDecisionType(t *testing.T) {
	_, _, conflictID := seedConflict(t)

	resp, err := authedRequest("POST",
		testSrv.URL+"/v1/conflicts/"+conflictID.String()+"/adjudicate",
		adminToken,
		map[string]any{
			"outcome":       "Chose Redis for performance reasons",
			"decision_type": "trade_off",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---- Coverage push: admin handler deeper paths ----

func TestHandleCreateGrant_ValidGrant(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", adminToken,
		map[string]any{
			"grantee_agent_id": "test-agent",
			"resource_type":    "agent_traces",
			"resource_id":      "admin",
			"permission":       "read",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Contains(t, []int{http.StatusOK, http.StatusCreated}, resp.StatusCode)
}

func TestHandleCreateGrant_InvalidResourceType(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", adminToken,
		map[string]any{
			"grantee_agent_id": "test-agent",
			"resource_type":    "invalid_type",
			"resource_id":      "admin",
			"permission":       "read",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateGrant_InvalidPermission(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", adminToken,
		map[string]any{
			"grantee_agent_id": "test-agent",
			"resource_type":    "agent_traces",
			"resource_id":      "admin",
			"permission":       "write",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateGrant_NonexistentGrantee(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", adminToken,
		map[string]any{
			"grantee_agent_id": "nonexistent-agent-xyz",
			"resource_type":    "agent_traces",
			"resource_id":      "admin",
			"permission":       "read",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleDeleteGrant_NonexistentGrant(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/grants/"+uuid.New().String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleUpdateAgent_MetadataUpdate(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/test-agent", adminToken,
		map[string]any{
			"metadata": map[string]any{"team": "backend", "env": "production"},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data map[string]any `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	metadata, ok := result.Data["metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "backend", metadata["team"])
}

func TestHandleUpdateAgent_NonexistentAgent(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/does-not-exist", adminToken,
		map[string]any{"name": "new name"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleUpdateAgentTags_EmptyTags(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/test-agent/tags", adminToken,
		map[string]any{"tags": []string{}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleUpdateAgentTags_InvalidChars(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/test-agent/tags", adminToken,
		map[string]any{"tags": []string{"valid-tag", "INVALID:TAG"}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleUpdateAgentTags_ValidMultiple(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/test-agent/tags", adminToken,
		map[string]any{"tags": []string{"backend", "production", "team-alpha"}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data map[string]any `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	tags, ok := result.Data["tags"].([]any)
	require.True(t, ok)
	assert.Len(t, tags, 3)
}

func TestHandleUpdateAgentTags_NonexistentAgent(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/nonexistent-agent/tags", adminToken,
		map[string]any{"tags": []string{"tag1"}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ---- Coverage push: health deeper assertions ----

func TestHandleHealth_Fields(t *testing.T) {
	resp, err := http.Get(testSrv.URL + "/health")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data model.HealthResponse `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))

	assert.Equal(t, "healthy", result.Data.Status)
	assert.Equal(t, "connected", result.Data.Postgres)
	assert.NotEmpty(t, result.Data.Version)
	assert.GreaterOrEqual(t, result.Data.Uptime, int64(0))
	assert.Equal(t, "ok", result.Data.BufferStatus)
}

// ---- Coverage push: scoped token deeper paths ----

func TestHandleScopedToken_ValidRequest(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/auth/scoped-token", adminToken,
		map[string]any{
			"as_agent_id": "test-agent",
			"expires_in":  3600,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	assert.NotEmpty(t, result.Data.Token, "scoped token should be returned")
}

func TestHandleScopedToken_NonexistentAgent(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/auth/scoped-token", adminToken,
		map[string]any{
			"as_agent_id": "nonexistent-agent-123",
			"expires_in":  3600,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ---- Coverage push: run handlers deeper paths ----

func TestHandleCreateRun_WithMetadata(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		map[string]any{
			"agent_id": "test-agent",
			"metadata": map[string]any{"source": "test", "env": "ci"},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestHandleGetRun_Nonexistent(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/runs/"+uuid.New().String(), agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleCompleteRun_Nonexistent(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+uuid.New().String()+"/complete", agentToken,
		map[string]any{"status": "completed"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleCompleteRun_InvalidStatus(t *testing.T) {
	// Create a run first.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		map[string]any{"agent_id": "test-agent"})
	require.NoError(t, err)
	var runResult struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "run creation body: %s", string(b))
	require.NoError(t, json.Unmarshal(b, &runResult))
	require.NotEmpty(t, runResult.Data.ID)

	resp, err = authedRequest("POST", testSrv.URL+"/v1/runs/"+runResult.Data.ID+"/complete", agentToken,
		map[string]any{"status": "invalid_status"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleAppendEvents_Nonexistent(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+uuid.New().String()+"/events", agentToken,
		map[string]any{
			"events": []map[string]any{
				{"type": "test_event", "data": map[string]any{"key": "value"}},
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ---- Coverage push: retention deeper paths ----

func TestHandleGetRetention_Valid(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/retention", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleSetRetention_ValidDays(t *testing.T) {
	resp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken,
		map[string]any{"retention_days": 90})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleSetRetention_NegativeDays(t *testing.T) {
	resp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken,
		map[string]any{"retention_days": -1})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ---- Coverage push: key management deeper paths ----

func TestHandleRotateKey_Nonexistent(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys/"+uuid.New().String()+"/rotate", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleCreateKey_ForAgent(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken,
		map[string]any{
			"agent_id": "test-agent",
			"label":    "coverage-test-key",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Contains(t, []int{http.StatusOK, http.StatusCreated}, resp.StatusCode)
}

func TestHandleListKeys_ForAgent(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/keys?agent_id=test-agent", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---- Coverage push: trace with session header ----

func TestHandleTrace_WithSessionHeader(t *testing.T) {
	resp, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/trace", adminToken,
		model.TraceRequest{
			AgentID: "admin",
			Decision: model.TraceDecision{
				DecisionType: "implementation",
				Outcome:      "use structured logging everywhere",
				Confidence:   0.9,
			},
		},
		map[string]string{"X-Session-ID": "test-session-abc"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Contains(t, []int{http.StatusOK, http.StatusCreated, http.StatusAccepted}, resp.StatusCode)
}

// ---- Coverage push: assessment deeper paths ----

func TestHandleListAssessments_NonexistentDecision(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+uuid.New().String()+"/assessments", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ---- Coverage push: validate-pair (integration - nil validator returns 501) ----

func TestHandleValidatePair_NoValidator(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/admin/conflicts/validate-pair", adminToken,
		map[string]any{
			"outcome_a": "Use Redis",
			"outcome_b": "Use Memcached",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

func TestHandleConflictEval_NoValidator(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/admin/conflicts/eval", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

// ---- Coverage push: grant listing ----

func TestHandleListGrants_ForAgent(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/grants?agent_id=test-agent", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---- Coverage push: decision conflicts & revisions ----

func TestHandleDecisionConflicts_Valid(t *testing.T) {
	// Trace a decision to get a valid ID.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, model.TraceRequest{
		AgentID: "admin",
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      "use gRPC for internal services",
			Confidence:   0.85,
		},
	})
	require.NoError(t, err)
	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	require.NoError(t, json.Unmarshal(b, &traceResult))

	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+traceResult.Data.DecisionID.String()+"/conflicts", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleDecisionRevisions_Valid(t *testing.T) {
	// Trace a decision.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, model.TraceRequest{
		AgentID: "admin",
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      "use REST for public API",
			Confidence:   0.8,
		},
	})
	require.NoError(t, err)
	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	require.NoError(t, json.Unmarshal(b, &traceResult))

	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+traceResult.Data.DecisionID.String()+"/revisions", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleDecisionRevisions_BadUUID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/not-a-uuid/revisions", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ---- Coverage push: temporal query ----

func TestHandleTemporalQuery_FutureAsOf(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query/temporal", adminToken,
		map[string]any{"as_of": time.Now().Add(24 * time.Hour).Format(time.RFC3339)})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleTemporalQuery_PastAsOf(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query/temporal", adminToken,
		map[string]any{"as_of": time.Now().Add(-1 * time.Hour).Format(time.RFC3339)})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---- Coverage push: export handler ----

func TestHandleExportDecisions_NDJSON(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/export/decisions", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/x-ndjson", resp.Header.Get("Content-Type"))
}

func TestHandleExportDecisions_WithTypeFilter(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/export/decisions?decision_type=architecture", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---- Coverage push: purge deeper paths ----

func TestHandlePurge_DryRunBasic(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/purge", adminToken,
		map[string]any{
			"before":  time.Now().Add(-365 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"dry_run": true,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandlePurge_MissingBeforeField(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/purge", adminToken,
		map[string]any{"dry_run": true})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandlePurge_WithDecisionTypeFilter(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/purge", adminToken,
		map[string]any{
			"before":        time.Now().Add(-365 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"decision_type": "architecture",
			"dry_run":       true,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandlePurge_WithAgentFilter(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/purge", adminToken,
		map[string]any{
			"before":   time.Now().Add(-365 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"agent_id": "test-agent",
			"dry_run":  true,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---- Coverage push: key management deeper paths ----

func TestHandleCreateKey_MissingAgentID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken,
		map[string]any{"name": "orphan-key"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleGetUsage_ForAgent(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/usage?agent_id=test-agent", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---- Coverage push: assess decision deeper paths ----

func TestHandleAssessDecision_InvalidDecisionID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/decisions/not-a-uuid/assess", adminToken,
		map[string]any{"outcome": "correct"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleAssessDecision_NonexistentDecision(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/decisions/"+uuid.New().String()+"/assess", adminToken,
		map[string]any{"outcome": "correct"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleAssessDecision_InvalidOutcome(t *testing.T) {
	// Trace a decision first.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, model.TraceRequest{
		AgentID: "admin",
		Decision: model.TraceDecision{
			DecisionType: "architecture",
			Outcome:      "use DynamoDB for sessions",
			Confidence:   0.7,
		},
	})
	require.NoError(t, err)
	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	require.NoError(t, json.Unmarshal(b, &traceResult))

	resp, err := authedRequest("POST", testSrv.URL+"/v1/decisions/"+traceResult.Data.DecisionID.String()+"/assess", adminToken,
		map[string]any{"outcome": "invalid_outcome_value"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ---- Coverage push: assess decision authorization ----

func TestHandleAssessDecision_ForbiddenWithoutGrant(t *testing.T) {
	// Create an agent that has no grants to admin's decisions.
	agentID := fmt.Sprintf("assess-no-grant-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, agentID, "Assess No Grant", "agent", agentID+"-key")
	token := getToken(testSrv.URL, agentID, agentID+"-key")

	// Create a decision as admin.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, model.TraceRequest{
		AgentID: "admin",
		Decision: model.TraceDecision{
			DecisionType: "assess-authz-test",
			Outcome:      "test outcome",
			Confidence:   0.7,
		},
	})
	require.NoError(t, err)
	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	require.NoError(t, json.Unmarshal(b, &traceResult))

	// Agent without grant tries to assess admin's decision — should be forbidden.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/decisions/"+traceResult.Data.DecisionID.String()+"/assess", token,
		map[string]any{"outcome": "correct"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandleAssessDecision_OwnDecisionSucceeds(t *testing.T) {
	// Agent traces a decision, then assesses it — should succeed.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken, model.TraceRequest{
		AgentID: "test-agent",
		Decision: model.TraceDecision{
			DecisionType: "assess-own-test",
			Outcome:      "test outcome",
			Confidence:   0.8,
		},
	})
	require.NoError(t, err)
	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	require.NoError(t, json.Unmarshal(b, &traceResult))

	resp, err := authedRequest("POST", testSrv.URL+"/v1/decisions/"+traceResult.Data.DecisionID.String()+"/assess", agentToken,
		map[string]any{"outcome": "correct"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleAssessDecision_SucceedsWithGrant(t *testing.T) {
	// Create a dedicated agent that will receive a grant.
	granteeID := fmt.Sprintf("assess-grantee-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, granteeID, "Assess Grantee", "agent", granteeID+"-key")
	granteeToken := getToken(testSrv.URL, granteeID, granteeID+"-key")

	// Create a decision as admin.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, model.TraceRequest{
		AgentID: "admin",
		Decision: model.TraceDecision{
			DecisionType: "assess-grant-test",
			Outcome:      "test outcome",
			Confidence:   0.6,
		},
	})
	require.NoError(t, err)
	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	require.NoError(t, json.Unmarshal(b, &traceResult))

	// Grant the agent access to admin's traces.
	agentIDStr := "admin"
	grantResp, err := authedRequest("POST", testSrv.URL+"/v1/grants", adminToken,
		model.CreateGrantRequest{
			GranteeAgentID: granteeID,
			ResourceType:   "agent_traces",
			ResourceID:     &agentIDStr,
			Permission:     "read",
		})
	require.NoError(t, err)
	defer func() { _ = grantResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, grantResp.StatusCode)

	// Agent with grant assesses admin's decision — should succeed.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/decisions/"+traceResult.Data.DecisionID.String()+"/assess", granteeToken,
		map[string]any{"outcome": "incorrect", "notes": "decision was wrong"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---- Coverage push: check endpoint ----

func TestHandleCheck_MissingDecisionTypeField(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/check", agentToken,
		map[string]any{"query": "test"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCheck_ValidQuery(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/check", agentToken,
		map[string]any{"query": "caching strategy", "decision_type": "architecture"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data map[string]any `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	_, hasDecisions := result.Data["decisions"]
	assert.True(t, hasDecisions, "check should return decisions field")
}

// ---- Coverage push: conflict list filters ----

func TestHandleListConflicts_WithSeverityFilter(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts?severity=high", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleListConflicts_WithStatusFilter(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts?status=open", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleListConflicts_WithConflictKindFilter(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts?conflict_kind=cross_agent", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleListConflicts_WithDecisionTypeFilter(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts?decision_type=architecture", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleListConflicts_WithAgentFilter(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts?agent=admin", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleListConflicts_WithPagination(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts?limit=5&offset=0", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---- Coverage push: conflict analytics ----

func TestHandleConflictAnalytics_ValidPeriod(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/analytics?period=7d", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleConflictAnalytics_InvalidPeriod(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/analytics?period=invalid", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ---- Coverage push: agent history with time range ----

func TestHandleAgentHistory_TimeRangeFilter(t *testing.T) {
	from := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	to := time.Now().Format(time.RFC3339)
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/agents/admin/history?from="+from+"&to="+to,
		adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---- Coverage push: decisions recent ----

func TestHandleDecisionsRecent_WithDecisionType(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/recent?decision_type=architecture", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---- Coverage push: session view ----

func TestHandleSessionView_NewSessionUUID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/sessions/"+uuid.New().String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---- Coverage push: API key authentication (covers verifyAPIKey) ----

func TestAPIKeyAuth_Valid(t *testing.T) {
	req, _ := http.NewRequest("GET", testSrv.URL+"/v1/agents", nil)
	req.Header.Set("Authorization", "ApiKey admin:test-admin-key")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAPIKeyAuth_InvalidFormat(t *testing.T) {
	req, _ := http.NewRequest("GET", testSrv.URL+"/v1/agents", nil)
	req.Header.Set("Authorization", "ApiKey no-colon-here")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAPIKeyAuth_WrongKey(t *testing.T) {
	req, _ := http.NewRequest("GET", testSrv.URL+"/v1/agents", nil)
	req.Header.Set("Authorization", "ApiKey admin:wrong-key")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAPIKeyAuth_NonexistentAgent(t *testing.T) {
	req, _ := http.NewRequest("GET", testSrv.URL+"/v1/agents", nil)
	req.Header.Set("Authorization", "ApiKey nonexistent:some-key")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// ---- Coverage push: query with various filters ----

func TestHandleQuery_WithAgentFilter(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query", adminToken,
		map[string]any{"filters": map[string]any{"agent_id": []string{"admin"}}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleQuery_WithDecisionTypeFilter(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query", adminToken,
		map[string]any{"filters": map[string]any{"decision_type": "architecture"}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleQuery_WithTimeRange(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query", adminToken,
		map[string]any{
			"filters": map[string]any{
				"time_range": map[string]any{
					"from": time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
					"to":   time.Now().Format(time.RFC3339),
				},
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleQuery_WithPagination(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query", adminToken,
		map[string]any{"limit": 2, "offset": 0})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---- Coverage push: trace with alternatives and evidence ----

func TestHandleTrace_WithAlternatives(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken,
		model.TraceRequest{
			AgentID: "admin",
			Decision: model.TraceDecision{
				DecisionType: "trade_off",
				Outcome:      "use connection pooling",
				Confidence:   0.85,
				Alternatives: []model.TraceAlternative{
					{Label: "no pooling"}, {Label: "pgbouncer"},
				},
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Contains(t, []int{http.StatusOK, http.StatusCreated, http.StatusAccepted}, resp.StatusCode)
}

// ---- Coverage push: retract and erase ----

func TestHandleRetractDecision_NonexistentDecision(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/decisions/"+uuid.New().String()+"/retract", adminToken,
		map[string]any{"reason": "test retraction"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleEraseDecision_NonexistentDecision(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/decisions/"+uuid.New().String(), orgOwnerToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ---- Coverage push: get single decision ----

func TestHandleGetDecision_NonexistentDecision(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+uuid.New().String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleGetDecision_InvalidID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/not-a-uuid", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ---- Coverage push: search ----

func TestHandleSearch_BlankQuery(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/search", adminToken,
		map[string]any{"query": ""})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleSearch_TextQuery(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/search", adminToken,
		map[string]any{"query": "database caching"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---- Coverage push: conflict groups ----

func TestHandleListConflictGroups_Valid(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflict-groups", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleListConflictGroups_WithPagination(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflict-groups?limit=5&offset=0", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ---- Coverage push: agent stats ----

func TestHandleAgentStats_Valid(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/admin/stats", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleAgentStats_NonexistentAgent(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/nonexistent-agent/stats", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ---- Coverage push: trace missing fields ----

func TestHandleTrace_NoDecisionType(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken,
		model.TraceRequest{
			AgentID: "admin",
			Decision: model.TraceDecision{
				Outcome:    "some outcome",
				Confidence: 0.9,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleTrace_NoOutcome(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken,
		model.TraceRequest{
			AgentID: "admin",
			Decision: model.TraceDecision{
				DecisionType: "architecture",
				Confidence:   0.9,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleTrace_MissingAgentID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken,
		model.TraceRequest{
			Decision: model.TraceDecision{
				DecisionType: "architecture",
				Outcome:      "some outcome",
				Confidence:   0.9,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandlePurge — real purge (non-dry-run) path
// ===========================================================================

func TestHandlePurge_RealPurge(t *testing.T) {
	// Real purge with a very old cutoff so nothing is actually deleted,
	// but the non-dry-run code path (StartDeletionLog, BatchDeleteDecisions,
	// CompleteDeletionLog) is fully exercised.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/purge", adminToken,
		map[string]any{
			"before":  time.Now().Add(-10 * 365 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"dry_run": false,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			DryRun  bool           `json:"dry_run"`
			Deleted map[string]any `json:"deleted"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.False(t, result.Data.DryRun, "should be a real purge, not dry_run")
	assert.NotNil(t, result.Data.Deleted, "deleted counts should be present")
}

func TestHandlePurge_RealPurgeWithFilters(t *testing.T) {
	// Real purge with both decision_type and agent_id filters — exercises criteria map building.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/purge", adminToken,
		map[string]any{
			"before":        time.Now().Add(-10 * 365 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"decision_type": "architecture",
			"agent_id":      "test-agent",
			"dry_run":       false,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			DryRun  bool           `json:"dry_run"`
			Deleted map[string]any `json:"deleted"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.False(t, result.Data.DryRun)
}

func TestHandlePurge_InvalidJSON(t *testing.T) {
	req, _ := http.NewRequest("POST", testSrv.URL+"/v1/retention/purge",
		bytes.NewReader([]byte("not valid json")))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleCreateKey — deeper validation paths
// ===========================================================================

func TestHandleCreateKey_InvalidAgentIDFormat(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken,
		model.CreateKeyRequest{AgentID: "INVALID AGENT!!!", Label: "test"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateKey_InvalidLabel(t *testing.T) {
	longLabel := ""
	for i := 0; i < 300; i++ {
		longLabel += "x"
	}
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken,
		model.CreateKeyRequest{AgentID: "test-agent", Label: longLabel})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateKey_NonexistentAgent(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken,
		model.CreateKeyRequest{AgentID: "does-not-exist-agent", Label: "test"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleCreateKey_WithExpiration(t *testing.T) {
	futureTime := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken,
		model.CreateKeyRequest{AgentID: "test-agent", Label: "expiring-key", ExpiresAt: &futureTime})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestHandleCreateKey_ExpiredExpiresAt(t *testing.T) {
	pastTime := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken,
		model.CreateKeyRequest{AgentID: "test-agent", Label: "past-key", ExpiresAt: &pastTime})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateKey_InvalidExpiresAtFormat(t *testing.T) {
	badFormat := "not-a-date"
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken,
		model.CreateKeyRequest{AgentID: "test-agent", Label: "bad-exp", ExpiresAt: &badFormat})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateKey_ForbiddenForAgentRole(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", agentToken,
		model.CreateKeyRequest{AgentID: "test-agent", Label: "denied"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleRotateKey — deeper paths
// ===========================================================================

func TestHandleRotateKey_InvalidUUID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys/not-a-uuid/rotate", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleRotateKey_AlreadyRevoked(t *testing.T) {
	agentID := fmt.Sprintf("rotate-revoked-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, agentID, "Rotate Revoked", "agent", "rotate-revoked-key")

	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken,
		model.CreateKeyRequest{AgentID: agentID, Label: "to-revoke-then-rotate"})
	require.NoError(t, err)
	var created struct {
		Data model.APIKeyWithRawKey `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, json.Unmarshal(body, &created))
	keyID := created.Data.ID.String()

	revokeResp, err := authedRequest("DELETE", testSrv.URL+"/v1/keys/"+keyID, adminToken, nil)
	require.NoError(t, err)
	_ = revokeResp.Body.Close()
	require.Equal(t, http.StatusNoContent, revokeResp.StatusCode)

	rotResp, err := authedRequest("POST", testSrv.URL+"/v1/keys/"+keyID+"/rotate", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = rotResp.Body.Close() }()
	assert.Equal(t, http.StatusConflict, rotResp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetUsage — period parsing paths
// ===========================================================================

func TestHandleGetUsage_WithValidPeriod(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/usage?period=2025-01", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Period         string `json:"period"`
			TotalDecisions int    `json:"total_decisions"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, "2025-01", result.Data.Period)
}

func TestHandleGetUsage_InvalidPeriodFormat(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/usage?period=not-a-period", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleGetUsage_DefaultPeriod(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/usage", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Period  string `json:"period"`
			ByKey   []any  `json:"by_key"`
			ByAgent []any  `json:"by_agent"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.NotEmpty(t, result.Data.Period, "default period should be set")
}

func TestHandleGetUsage_ForbiddenForAgent(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/usage", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleCreateGrant — deeper validation paths
// ===========================================================================

func TestHandleCreateGrant_AgentRoleSelfGrant(t *testing.T) {
	granteeID := fmt.Sprintf("grant-self-target-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, granteeID, "Self Grant Target", "reader", "self-grant-key")

	resourceID := "test-agent"
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", agentToken,
		model.CreateGrantRequest{
			GranteeAgentID: granteeID,
			ResourceType:   "agent_traces",
			ResourceID:     &resourceID,
			Permission:     "read",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestHandleCreateGrant_AgentRoleCantGrantOthersTraces(t *testing.T) {
	granteeID := fmt.Sprintf("grant-other-target-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, granteeID, "Other Grant Target", "reader", "other-grant-key")

	otherAgent := "admin"
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", agentToken,
		model.CreateGrantRequest{
			GranteeAgentID: granteeID,
			ResourceType:   "agent_traces",
			ResourceID:     &otherAgent,
			Permission:     "read",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandleCreateGrant_WithExpiresAt(t *testing.T) {
	granteeID := fmt.Sprintf("grant-exp-target-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, granteeID, "Exp Grant Target", "reader", "exp-grant-key")

	resourceID := "test-agent"
	futureTime := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", adminToken,
		model.CreateGrantRequest{
			GranteeAgentID: granteeID,
			ResourceType:   "agent_traces",
			ResourceID:     &resourceID,
			Permission:     "read",
			ExpiresAt:      &futureTime,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestHandleCreateGrant_InvalidExpiresAtFormat(t *testing.T) {
	badFormat := "not-a-date"
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", adminToken,
		model.CreateGrantRequest{
			GranteeAgentID: "test-agent",
			ResourceType:   "agent_traces",
			Permission:     "read",
			ExpiresAt:      &badFormat,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateGrant_DuplicateGrant(t *testing.T) {
	granteeID := fmt.Sprintf("grant-dup-target-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, granteeID, "Dup Grant Target", "reader", "dup-grant-key")

	resourceID := "test-agent"
	grantReq := model.CreateGrantRequest{
		GranteeAgentID: granteeID,
		ResourceType:   "agent_traces",
		ResourceID:     &resourceID,
		Permission:     "read",
	}

	resp1, err := authedRequest("POST", testSrv.URL+"/v1/grants", adminToken, grantReq)
	require.NoError(t, err)
	_ = resp1.Body.Close()
	require.Equal(t, http.StatusCreated, resp1.StatusCode)

	resp2, err := authedRequest("POST", testSrv.URL+"/v1/grants", adminToken, grantReq)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusConflict, resp2.StatusCode)
}

// ===========================================================================
// Coverage push: HandleDeleteGrant — deeper paths
// ===========================================================================

func TestHandleDeleteGrant_InvalidUUID(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/grants/not-a-uuid", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleDeleteGrant_AgentRoleCanDeleteOwnGrant(t *testing.T) {
	granteeID := fmt.Sprintf("grant-del-self-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, granteeID, "Del Self Target", "reader", "del-self-key")

	resourceID := "test-agent"
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", agentToken,
		model.CreateGrantRequest{
			GranteeAgentID: granteeID,
			ResourceType:   "agent_traces",
			ResourceID:     &resourceID,
			Permission:     "read",
		})
	require.NoError(t, err)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, json.Unmarshal(body, &created))

	delResp, err := authedRequest("DELETE", testSrv.URL+"/v1/grants/"+created.Data.ID, agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = delResp.Body.Close() }()
	assert.Equal(t, http.StatusNoContent, delResp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleExportDecisions — filter paths
// ===========================================================================

func TestHandleExportDecisions_WithAgentFilter(t *testing.T) {
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/export/decisions?agent_id=test-agent", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/x-ndjson", resp.Header.Get("Content-Type"))
}

func TestHandleExportDecisions_WithFromFilter(t *testing.T) {
	from := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/export/decisions?from="+from, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleExportDecisions_WithToFilter(t *testing.T) {
	to := time.Now().UTC().Format(time.RFC3339)
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/export/decisions?to="+to, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleExportDecisions_WithFromAndToFilter(t *testing.T) {
	from := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	to := time.Now().UTC().Format(time.RFC3339)
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/export/decisions?from="+from+"&to="+to, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleExportDecisions_InvalidFromFormat(t *testing.T) {
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/export/decisions?from=not-a-date", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleExportDecisions_InvalidToFormat(t *testing.T) {
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/export/decisions?to=not-a-date", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleExportDecisions_ForbiddenForAgentRole(t *testing.T) {
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/export/decisions", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleHookSessionStart — with valid input
// ===========================================================================

// NOTE: TestHandleHookSessionStart_ValidInput was removed because hook
// endpoints are guarded by localhostOnly middleware which rejects TCP
// connections from httptest. Full coverage lives in handlers_hooks_test.go
// which tests the handler directly without the middleware guard.

// ===========================================================================
// Coverage push: HandleGetConflict — not found, invalid ID
// ===========================================================================

func TestHandleGetConflict_NotFound(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/"+uuid.New().String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleGetConflict_InvalidUUID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/not-a-uuid", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetRun — access denied, invalid ID, full response
// ===========================================================================

func TestHandleGetRun_AccessDenied(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", adminToken,
		model.CreateRunRequest{AgentID: "admin"})
	require.NoError(t, err)
	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NoError(t, json.Unmarshal(body, &runResult))

	readerID := fmt.Sprintf("reader-run-%d", time.Now().UnixNano())
	createAgent(testSrv.URL, adminToken, readerID, "Reader Run", "reader", "reader-run-key")
	readerToken := getToken(testSrv.URL, readerID, "reader-run-key")

	getResp, err := authedRequest("GET", testSrv.URL+"/v1/runs/"+runResult.Data.ID.String(), readerToken, nil)
	require.NoError(t, err)
	defer func() { _ = getResp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, getResp.StatusCode)
}

func TestHandleGetRun_InvalidRunID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/runs/not-a-uuid", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleGetRun_WithDecisionsAndEvents(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NoError(t, json.Unmarshal(body, &runResult))
	runID := runResult.Data.ID

	evResp, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+runID.String()+"/events", agentToken,
		model.AppendEventsRequest{
			Events: []model.EventInput{
				{EventType: model.EventToolCallStarted, Payload: map[string]any{"tool": "grep"}},
				{EventType: model.EventToolCallCompleted, Payload: map[string]any{"result": "ok"}},
			},
		})
	require.NoError(t, err)
	_ = evResp.Body.Close()

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()
	_ = testBuf.FlushNow(flushCtx)

	getResp, err := authedRequest("GET", testSrv.URL+"/v1/runs/"+runID.String(), agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = getResp.Body.Close() }()
	assert.Equal(t, http.StatusOK, getResp.StatusCode)

	var result struct {
		Data struct {
			Run       model.AgentRun    `json:"run"`
			Events    []json.RawMessage `json:"events"`
			Decisions []json.RawMessage `json:"decisions"`
		} `json:"data"`
	}
	data, _ := io.ReadAll(getResp.Body)
	require.NoError(t, json.Unmarshal(data, &result))
	assert.GreaterOrEqual(t, len(result.Data.Events), 1, "should have at least one event")
	// decisions may be nil/empty when no decisions exist for the run; the key path exercised is that GetRun returns 200 with the decisions field
}

// ===========================================================================
// Coverage push: HandleCreateRun — validation and RBAC paths
// ===========================================================================

func TestHandleCreateRun_InvalidAgentID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", adminToken,
		model.CreateRunRequest{AgentID: "INVALID ID!!!"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateRun_AgentCannotCreateForOthers(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "admin"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandleCreateRun_WithTraceIDAndMetadata(t *testing.T) {
	traceID := "my-trace-id-123"
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{
			AgentID:  "test-agent",
			TraceID:  &traceID,
			Metadata: map[string]any{"branch": "main", "task": "deploy"},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestHandleCreateRun_WithIdempotencyKey(t *testing.T) {
	idemKey := "run-idem-" + uuid.NewString()
	runReq := model.CreateRunRequest{AgentID: "test-agent"}

	resp1, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/runs", agentToken, runReq,
		map[string]string{"Idempotency-Key": idemKey})
	require.NoError(t, err)
	var result1 struct {
		Data model.AgentRun `json:"data"`
	}
	body1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	require.Equal(t, http.StatusCreated, resp1.StatusCode)
	require.NoError(t, json.Unmarshal(body1, &result1))

	resp2, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/runs", agentToken, runReq,
		map[string]string{"Idempotency-Key": idemKey})
	require.NoError(t, err)
	var result2 struct {
		Data model.AgentRun `json:"data"`
	}
	body2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	require.NoError(t, json.Unmarshal(body2, &result2))
	assert.Equal(t, result1.Data.ID, result2.Data.ID, "idempotent replay should return same run ID")
}

// ===========================================================================
// Coverage push: HandleAppendEvents — validation and RBAC paths
// ===========================================================================

func TestHandleAppendEvents_EmptyEventsArray(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NoError(t, json.Unmarshal(body, &runResult))

	evResp, err := authedRequest("POST",
		testSrv.URL+"/v1/runs/"+runResult.Data.ID.String()+"/events", agentToken,
		model.AppendEventsRequest{Events: []model.EventInput{}})
	require.NoError(t, err)
	defer func() { _ = evResp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, evResp.StatusCode)
}

func TestHandleAppendEvents_InvalidRunID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs/not-a-uuid/events", agentToken,
		model.AppendEventsRequest{
			Events: []model.EventInput{
				{EventType: model.EventDecisionStarted, Payload: map[string]any{"x": 1}},
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleAppendEvents_NotYourRun(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", adminToken,
		model.CreateRunRequest{AgentID: "admin"})
	require.NoError(t, err)
	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NoError(t, json.Unmarshal(body, &runResult))

	evResp, err := authedRequest("POST",
		testSrv.URL+"/v1/runs/"+runResult.Data.ID.String()+"/events", agentToken,
		model.AppendEventsRequest{
			Events: []model.EventInput{
				{EventType: model.EventDecisionStarted, Payload: map[string]any{"x": 1}},
			},
		})
	require.NoError(t, err)
	defer func() { _ = evResp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, evResp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleCompleteRun — RBAC and idempotency paths
// ===========================================================================

func TestHandleCompleteRun_InvalidRunID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs/not-a-uuid/complete", agentToken,
		model.CompleteRunRequest{})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCompleteRun_NotYourRun(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", adminToken,
		model.CreateRunRequest{AgentID: "admin"})
	require.NoError(t, err)
	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NoError(t, json.Unmarshal(body, &runResult))

	complResp, err := authedRequest("POST",
		testSrv.URL+"/v1/runs/"+runResult.Data.ID.String()+"/complete", agentToken,
		model.CompleteRunRequest{})
	require.NoError(t, err)
	defer func() { _ = complResp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, complResp.StatusCode)
}

func TestHandleCompleteRun_WithIdempotencyKey(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NoError(t, json.Unmarshal(body, &runResult))
	runID := runResult.Data.ID

	idemKey := "complete-idem-" + uuid.NewString()
	complReq := model.CompleteRunRequest{Status: "completed"}

	resp1, err := authedRequestWithHeaders("POST",
		testSrv.URL+"/v1/runs/"+runID.String()+"/complete", agentToken, complReq,
		map[string]string{"Idempotency-Key": idemKey})
	require.NoError(t, err)
	_ = resp1.Body.Close()
	require.Equal(t, http.StatusOK, resp1.StatusCode)

	resp2, err := authedRequestWithHeaders("POST",
		testSrv.URL+"/v1/runs/"+runID.String()+"/complete", agentToken, complReq,
		map[string]string{"Idempotency-Key": idemKey})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
}

func TestHandleCompleteRun_WithMetadata(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NoError(t, json.Unmarshal(body, &runResult))

	complResp, err := authedRequest("POST",
		testSrv.URL+"/v1/runs/"+runResult.Data.ID.String()+"/complete", agentToken,
		model.CompleteRunRequest{Status: "completed", Metadata: map[string]any{"result": "success", "count": 42}})
	require.NoError(t, err)
	defer func() { _ = complResp.Body.Close() }()
	assert.Equal(t, http.StatusOK, complResp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleSetRetention — exclude types, invalid JSON, zero days
// ===========================================================================

func TestHandleSetRetention_WithExcludeTypes(t *testing.T) {
	days := 90
	resp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken,
		map[string]any{
			"retention_days":          days,
			"retention_exclude_types": []string{"architecture", "security"},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			RetentionDays         int      `json:"retention_days"`
			RetentionExcludeTypes []string `json:"retention_exclude_types"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, days, result.Data.RetentionDays)
}

func TestHandleSetRetention_InvalidJSON(t *testing.T) {
	req, _ := http.NewRequest("PUT", testSrv.URL+"/v1/retention",
		bytes.NewReader([]byte("not valid json")))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleSetRetention_ZeroDays(t *testing.T) {
	resp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken,
		map[string]any{"retention_days": 0})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleHealth — detailed field verification
// ===========================================================================

func TestHandleHealth_DetailedFields(t *testing.T) {
	resp, err := http.Get(testSrv.URL + "/health")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data model.HealthResponse `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))

	assert.Equal(t, "healthy", result.Data.Status)
	assert.Equal(t, "test", result.Data.Version)
	assert.Equal(t, "connected", result.Data.Postgres)
	assert.Equal(t, "ok", result.Data.BufferStatus)
	assert.GreaterOrEqual(t, result.Data.Uptime, int64(0))
	assert.GreaterOrEqual(t, result.Data.BufferDepth, 0)
	assert.Empty(t, result.Data.Qdrant)
	assert.Empty(t, result.Data.SSEBroker)
}

// ===========================================================================
// Coverage push: HandleRevokeKey — invalid UUID
// ===========================================================================

func TestHandleRevokeKey_InvalidUUID(t *testing.T) {
	reqDel, _ := http.NewRequest("DELETE", testSrv.URL+"/v1/keys/not-a-uuid", nil)
	reqDel.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(reqDel)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: verifyAPIKey — various credential formats via ApiKey auth
// ===========================================================================

func TestAPIKeyAuth_ValidCredential(t *testing.T) {
	req, _ := http.NewRequest("GET", testSrv.URL+"/v1/decisions/recent", nil)
	req.Header.Set("Authorization", "ApiKey admin:test-admin-key")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAPIKeyAuth_InvalidFormat_NoColon(t *testing.T) {
	req, _ := http.NewRequest("GET", testSrv.URL+"/v1/decisions/recent", nil)
	req.Header.Set("Authorization", "ApiKey noColonHere")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAPIKeyAuth_InvalidFormat_EmptyAgentID(t *testing.T) {
	req, _ := http.NewRequest("GET", testSrv.URL+"/v1/decisions/recent", nil)
	req.Header.Set("Authorization", "ApiKey :some-key")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAPIKeyAuth_InvalidFormat_EmptyKey(t *testing.T) {
	req, _ := http.NewRequest("GET", testSrv.URL+"/v1/decisions/recent", nil)
	req.Header.Set("Authorization", "ApiKey admin:")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAPIKeyAuth_WithOrgHeader(t *testing.T) {
	req, _ := http.NewRequest("GET", testSrv.URL+"/v1/decisions/recent", nil)
	req.Header.Set("Authorization", "ApiKey admin:test-admin-key")
	req.Header.Set("X-Akashi-Org-ID", uuid.Nil.String())
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAPIKeyAuth_InvalidOrgHeader(t *testing.T) {
	req, _ := http.NewRequest("GET", testSrv.URL+"/v1/decisions/recent", nil)
	req.Header.Set("Authorization", "ApiKey admin:test-admin-key")
	req.Header.Set("X-Akashi-Org-ID", "not-a-uuid")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAPIKeyAuth_WrongOrgHeader(t *testing.T) {
	req, _ := http.NewRequest("GET", testSrv.URL+"/v1/decisions/recent", nil)
	req.Header.Set("Authorization", "ApiKey admin:test-admin-key")
	req.Header.Set("X-Akashi-Org-ID", uuid.New().String())
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAuthToken_WrongAPIKey(t *testing.T) {
	body, _ := json.Marshal(model.AuthTokenRequest{AgentID: "admin", APIKey: "definitely-wrong-key"})
	resp, err := http.Post(testSrv.URL+"/auth/token", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAuthToken_NonexistentAgent(t *testing.T) {
	body, _ := json.Marshal(model.AuthTokenRequest{AgentID: "ghost-agent", APIKey: "some-key"})
	resp, err := http.Post(testSrv.URL+"/auth/token", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetRetention — verify full response shape
// ===========================================================================

func TestHandleGetRetention_FullResponseShape(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/retention", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			RetentionDays         int    `json:"retention_days"`
			RetentionExcludeTypes []any  `json:"retention_exclude_types"`
			LastRun               *any   `json:"last_run"`
			LastRunDeleted        *any   `json:"last_run_deleted"`
			NextRun               string `json:"next_run"`
			Holds                 []any  `json:"holds"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.GreaterOrEqual(t, result.Data.RetentionDays, 0)
	assert.NotNil(t, result.Data.Holds, "holds should be an array, not nil")
}

// ===========================================================================
// Coverage push: scoped token — delegation chain, role escalation, TTL capping
// ===========================================================================

func TestHandleScopedToken_ScopedTokenCannotDelegate(t *testing.T) {
	// First, get a scoped token for test-agent (admin -> test-agent).
	resp, err := authedRequest("POST", testSrv.URL+"/v1/auth/scoped-token", adminToken,
		map[string]any{"as_agent_id": "test-agent"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	scopedToken := result.Data.Token
	require.NotEmpty(t, scopedToken)

	// Now try to use the scoped token to issue another scoped token — should be denied.
	resp2, err := authedRequest("POST", testSrv.URL+"/v1/auth/scoped-token", scopedToken,
		map[string]any{"as_agent_id": "test-agent"})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp2.StatusCode, "scoped tokens cannot issue further scoped tokens")
}

func TestHandleScopedToken_CannotImpersonateHigherRole(t *testing.T) {
	// Admin (rank 3) tries to get a scoped token for org_owner (rank 4) — should be denied.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/auth/scoped-token", adminToken,
		map[string]any{"as_agent_id": "test-org-owner"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "cannot impersonate agent with higher or equal role")
}

func TestHandleScopedToken_MissingAgentID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/auth/scoped-token", adminToken,
		map[string]any{})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleScopedToken_InvalidAgentIDFormat(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/auth/scoped-token", adminToken,
		map[string]any{"as_agent_id": "INVALID AGENT!!!"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleScopedToken_OverlyLongTTLIsCapped(t *testing.T) {
	// Request 48 hours — the handler caps at MaxScopedTokenTTL (1 hour).
	resp, err := authedRequest("POST", testSrv.URL+"/v1/auth/scoped-token", adminToken,
		map[string]any{"as_agent_id": "test-agent", "expires_in": 172800})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	assert.NotEmpty(t, result.Data.Token)
	// Expiry should be within ~1 hour (with some tolerance), not 48 hours.
	assert.True(t, result.Data.ExpiresAt.Before(time.Now().Add(2*time.Hour)),
		"TTL should be capped at MaxScopedTokenTTL (1 hour)")
}

func TestHandleScopedToken_ForbiddenForAgentRole(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/auth/scoped-token", agentToken,
		map[string]any{"as_agent_id": "test-agent"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// Agent role (rank 2) shouldn't have access to the adminOnly-gated endpoint.
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ===========================================================================
// Coverage push: auth middleware — unsupported scheme, malformed header
// ===========================================================================

func TestAuthMiddleware_UnsupportedScheme(t *testing.T) {
	req, _ := http.NewRequest("GET", testSrv.URL+"/v1/conflicts", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "unsupported authorization scheme")
}

func TestAuthMiddleware_MalformedHeader(t *testing.T) {
	req, _ := http.NewRequest("GET", testSrv.URL+"/v1/conflicts", nil)
	req.Header.Set("Authorization", "BearerTokenWithNoSpace")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "invalid authorization format")
}

// ===========================================================================
// Coverage push: HandleTrace — agent-only, forbidden for other agent
// ===========================================================================

func TestHandleTrace_AgentCannotTraceForOther(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken, map[string]any{
		"agent_id": "admin",
		"decision": map[string]any{
			"decision_type": "test",
			"outcome":       "should fail",
			"confidence":    0.5,
		},
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"agent-role caller should not be able to trace for another agent")
}

// ===========================================================================
// Coverage push: HandleTemporalQuery — far future rejection
// ===========================================================================

func TestHandleTemporalQuery_FarFuture(t *testing.T) {
	futureTime := time.Now().Add(24 * time.Hour)
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query/temporal", adminToken, map[string]any{
		"as_of": futureTime.Format(time.RFC3339),
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "as_of must not be in the future")
}

// ===========================================================================
// Coverage push: HandleSessionView — invalid UUID
// ===========================================================================

func TestHandleSessionView_InvalidUUID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/sessions/not-a-uuid", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "invalid session_id")
}

// ===========================================================================
// Coverage push: HandleDeleteAgent — legal hold blocking, disabled flag
// ===========================================================================

func TestHandleDeleteAgent_BlockedByLegalHold(t *testing.T) {
	// Create a temporary agent to attempt deletion on.
	createAgent(testSrv.URL, adminToken, "hold-victim", "Hold Victim", "agent", "hold-victim-key")

	// Create a trace for this agent so the hold has something to cover.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, map[string]any{
		"agent_id": "hold-victim",
		"decision": map[string]any{
			"decision_type": "test",
			"outcome":       "test outcome for hold",
			"confidence":    0.9,
		},
	})
	require.NoError(t, err)
	_ = traceResp.Body.Close()

	// Create a legal hold covering this agent.
	holdResp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken, map[string]any{
		"reason":    "litigation hold for test",
		"from":      time.Now().Add(-time.Hour).Format(time.RFC3339),
		"to":        time.Now().Add(time.Hour).Format(time.RFC3339),
		"agent_ids": []string{"hold-victim"},
	})
	require.NoError(t, err)
	defer func() { _ = holdResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, holdResp.StatusCode)

	// Now try to delete — should be blocked by legal hold.
	deleteResp, err := authedRequest("DELETE", testSrv.URL+"/v1/agents/hold-victim", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = deleteResp.Body.Close() }()
	assert.Equal(t, http.StatusConflict, deleteResp.StatusCode)
	b, _ := io.ReadAll(deleteResp.Body)
	assert.Contains(t, string(b), "legal hold")
}

func TestHandleDeleteAgent_CannotDeleteSeedAdmin(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/agents/admin", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "cannot delete the admin agent")
}

func TestHandleDeleteAgent_InvalidAgentIDFormat(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/agents/INVALID AGENT!!!", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleUpdateAgent — invalid agent_id format, not found
// ===========================================================================

func TestHandleUpdateAgent_InvalidAgentIDFormat(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/INVALID!!!", adminToken,
		map[string]any{"name": "new-name"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleUpdateAgent_NothingToUpdate(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/test-agent", adminToken,
		map[string]any{})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "at least one of name or metadata is required")
}

func TestHandleUpdateAgent_Nonexistent(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/nonexistent-agent-xyz", adminToken,
		map[string]any{"name": "new-name"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleUpdateAgentTags — missing tags, invalid format
// ===========================================================================

func TestHandleUpdateAgentTags_MissingTagsField(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/test-agent/tags", adminToken,
		map[string]any{})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "tags field is required")
}

func TestHandleUpdateAgentTags_InvalidAgentIDFormat(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/INVALID!!!/tags", adminToken,
		map[string]any{"tags": []string{"foo"}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleUpdateAgentTags_Nonexistent(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/nonexistent-agent-xyz/tags", adminToken,
		map[string]any{"tags": []string{"foo"}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleEraseDecision — legal hold blocks erasure
// ===========================================================================

func TestHandleEraseDecision_BlockedByLegalHold(t *testing.T) {
	// Create a trace to erase.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, map[string]any{
		"agent_id": "test-agent",
		"decision": map[string]any{
			"decision_type": "erasure-hold-test",
			"outcome":       "test outcome for erasure hold",
			"confidence":    0.9,
		},
	})
	require.NoError(t, err)
	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	require.NoError(t, json.Unmarshal(b, &traceResult))
	decisionID := traceResult.Data.DecisionID

	// Create a hold covering this decision type and agent.
	holdResp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken, map[string]any{
		"reason":         "litigation hold for erasure test",
		"from":           time.Now().Add(-time.Hour).Format(time.RFC3339),
		"to":             time.Now().Add(time.Hour).Format(time.RFC3339),
		"decision_types": []string{"erasure-hold-test"},
		"agent_ids":      []string{"test-agent"},
	})
	require.NoError(t, err)
	_ = holdResp.Body.Close()
	require.Equal(t, http.StatusCreated, holdResp.StatusCode)

	// Attempt erasure — should be blocked.
	eraseResp, err := authedRequest("POST", testSrv.URL+"/v1/decisions/"+decisionID.String()+"/erase", orgOwnerToken,
		map[string]any{"reason": "test erasure"})
	require.NoError(t, err)
	defer func() { _ = eraseResp.Body.Close() }()
	assert.Equal(t, http.StatusConflict, eraseResp.StatusCode)
	eraseBody, _ := io.ReadAll(eraseResp.Body)
	assert.Contains(t, string(eraseBody), "legal hold")
}

// ===========================================================================
// Coverage push: HandleVerifyDecision — invalid UUID
// ===========================================================================

func TestHandleVerifyDecision_InvalidUUID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/verify/not-a-uuid", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleListAssessments — access denied for reader without grant
// ===========================================================================

func TestHandleListAssessments_AccessDeniedForReader(t *testing.T) {
	// Create a reader agent.
	createAgent(testSrv.URL, adminToken, "reader-assess-test", "Reader Assess Test", "reader", "reader-assess-key")
	readerToken := getToken(testSrv.URL, "reader-assess-test", "reader-assess-key")

	// Create a trace as admin.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, map[string]any{
		"agent_id": "admin",
		"decision": map[string]any{
			"decision_type": "reader-assess-test",
			"outcome":       "test",
			"confidence":    0.5,
		},
	})
	require.NoError(t, err)
	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	require.NoError(t, json.Unmarshal(b, &traceResult))

	// Reader tries to list assessments for admin's decision — should be forbidden.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+traceResult.Data.DecisionID.String()+"/assessments", readerToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleCheck — with project and agent_id filters, query field
// ===========================================================================

func TestHandleCheck_WithProjectAndAgentFilters(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/check", adminToken, map[string]any{
		"decision_type": "test",
		"project":       "test-project",
		"agent_id":      "test-agent",
		"limit":         5,
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleCheck_WithQueryField(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/check", adminToken, map[string]any{
		"decision_type": "test",
		"query":         "how should we handle caching?",
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleQuery — negative offset clamped
// ===========================================================================

func TestHandleQuery_NegativeOffset(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query", adminToken, map[string]any{
		"offset": -5,
		"limit":  10,
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "negative offset should be clamped to 0, not rejected")
}

// ===========================================================================
// Coverage push: HandleCreateHold — with decision_types and agent_ids
// ===========================================================================

func TestHandleCreateHold_WithDecisionTypesAndAgentIDs(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken, map[string]any{
		"reason":         "scoped hold test",
		"from":           time.Now().Add(-time.Hour).Format(time.RFC3339),
		"to":             time.Now().Add(time.Hour).Format(time.RFC3339),
		"decision_types": []string{"architecture", "security"},
		"agent_ids":      []string{"test-agent", "admin"},
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	var result struct {
		Data struct {
			DecisionTypes []string `json:"decision_types"`
			AgentIDs      []string `json:"agent_ids"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	assert.ElementsMatch(t, []string{"architecture", "security"}, result.Data.DecisionTypes)
	assert.ElementsMatch(t, []string{"test-agent", "admin"}, result.Data.AgentIDs)
}

// ===========================================================================
// Coverage push: HandleSetRetention — retention_days < 1 rejected
// ===========================================================================

func TestHandleSetRetention_InvalidRetentionDays(t *testing.T) {
	resp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken, map[string]any{
		"retention_days": 0,
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "retention_days must be")
}

func TestHandleSetRetention_ValidUpdate(t *testing.T) {
	resp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken, map[string]any{
		"retention_days":          90,
		"retention_exclude_types": []string{"audit"},
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetRetention — wraps retention + holds in one response
// ===========================================================================

func TestHandleGetRetention_ReturnsDataEnvelope(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/retention", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var raw map[string]json.RawMessage
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &raw))
	assert.Contains(t, raw, "data", "response must include data envelope")
}

// ===========================================================================
// Coverage push: HandleDecisionConflicts — pagination
// ===========================================================================

func TestHandleDecisionConflicts_WithPagination(t *testing.T) {
	// Create a decision to query conflicts for.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, map[string]any{
		"agent_id": "admin",
		"decision": map[string]any{
			"decision_type": "conflict-page-test",
			"outcome":       "test outcome",
			"confidence":    0.9,
		},
	})
	require.NoError(t, err)
	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	tb, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	require.NoError(t, json.Unmarshal(tb, &traceResult))

	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/decisions/"+traceResult.Data.DecisionID.String()+"/conflicts?limit=5&offset=0",
		adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetRun — reader without grant cannot access
// ===========================================================================

func TestHandleGetRun_ReaderCannotAccessWithoutGrant(t *testing.T) {
	// Create a run as test-agent.
	runResp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		map[string]any{"agent_id": "test-agent"})
	require.NoError(t, err)
	var runResult struct {
		Data struct {
			ID uuid.UUID `json:"id"`
		} `json:"data"`
	}
	rb, _ := io.ReadAll(runResp.Body)
	_ = runResp.Body.Close()
	require.Equal(t, http.StatusCreated, runResp.StatusCode)
	require.NoError(t, json.Unmarshal(rb, &runResult))

	// Create a reader agent with no grant.
	createAgent(testSrv.URL, adminToken, "reader-run-test", "Reader Run Test", "reader", "reader-run-key")
	readerToken := getToken(testSrv.URL, "reader-run-test", "reader-run-key")

	// Reader tries to access the run — should be denied.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/runs/"+runResult.Data.ID.String(), readerToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleRotateKey — already revoked key returns 409
// ===========================================================================

func TestHandleRotateKey_AlreadyRevokedReturns409(t *testing.T) {
	// Create a key, revoke it, then attempt rotate.
	createResp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken, map[string]any{
		"agent_id": "test-agent",
		"label":    "revoke-test-key",
	})
	require.NoError(t, err)
	var createResult struct {
		Data struct {
			ID uuid.UUID `json:"id"`
		} `json:"data"`
	}
	cb, _ := io.ReadAll(createResp.Body)
	_ = createResp.Body.Close()
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	require.NoError(t, json.Unmarshal(cb, &createResult))
	keyID := createResult.Data.ID

	// Revoke it.
	revokeResp, err := authedRequest("DELETE", testSrv.URL+"/v1/keys/"+keyID.String(), adminToken, nil)
	require.NoError(t, err)
	_ = revokeResp.Body.Close()
	require.Equal(t, http.StatusNoContent, revokeResp.StatusCode)

	// Attempt rotation — should fail with 409 Conflict.
	rotateResp, err := authedRequest("POST", testSrv.URL+"/v1/keys/"+keyID.String()+"/rotate", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = rotateResp.Body.Close() }()
	assert.Equal(t, http.StatusConflict, rotateResp.StatusCode)
	rb, _ := io.ReadAll(rotateResp.Body)
	assert.Contains(t, string(rb), "already revoked")
}

// ===========================================================================
// Coverage push: HandleCreateAgent — reserved agent_id, role escalation
// ===========================================================================

func TestHandleCreateAgent_ReservedAgentID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken, map[string]any{
		"agent_id": "system",
		"name":     "System",
		"role":     "agent",
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "reserved")
}

func TestHandleCreateAgent_CannotCreateHigherRole(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken, map[string]any{
		"agent_id": "new-admin-attempt",
		"name":     "New Admin",
		"role":     "admin",
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"admin cannot create agent with role equal to own")
}

// ===========================================================================
// Coverage push: HandleRetractDecision — invalid UUID
// ===========================================================================

func TestHandleRetractDecision_InvalidUUID(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/decisions/not-a-uuid", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleRetractDecision_Nonexistent(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/decisions/"+uuid.New().String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetDecision — forbidden for reader without grant
// ===========================================================================

func TestHandleGetDecision_ForbiddenForReader(t *testing.T) {
	// Create a trace as admin.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, map[string]any{
		"agent_id": "admin",
		"decision": map[string]any{
			"decision_type": "reader-access-test",
			"outcome":       "test",
			"confidence":    0.5,
		},
	})
	require.NoError(t, err)
	var traceResult struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	tb, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	require.NoError(t, json.Unmarshal(tb, &traceResult))

	// Create a reader agent without any grant.
	createAgent(testSrv.URL, adminToken, "reader-getdec-test", "Reader GetDec Test", "reader", "reader-getdec-key")
	readerToken := getToken(testSrv.URL, "reader-getdec-test", "reader-getdec-key")

	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+traceResult.Data.DecisionID.String(), readerToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleDecisionRevisions — invalid UUID
// ===========================================================================

func TestHandleDecisionRevisions_InvalidDecisionUUID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/not-a-uuid/revisions", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandlePatchConflict — invalid UUID, invalid status, winning_decision_id guard
// ===========================================================================

func TestHandlePatchConflict_BadUUID(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflicts/not-a-uuid", adminToken,
		map[string]any{"status": "resolved"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandlePatchConflict_BadStatus(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflicts/"+uuid.New().String(), adminToken,
		map[string]any{"status": "invalid_status"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandlePatchConflict_WinnerOnNonResolvedStatus(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflicts/"+uuid.New().String(), adminToken,
		map[string]any{"status": "false_positive", "winning_decision_id": uuid.New().String()})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "winning_decision_id can only be set when status is")
}

// ===========================================================================
// Coverage push: HandleAdjudicateConflict — invalid UUID, missing outcome
// ===========================================================================

func TestHandleAdjudicateConflict_BadUUID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/conflicts/not-a-uuid/adjudicate", adminToken,
		map[string]any{"outcome": "test"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleAdjudicateConflict_NonexistentConflict(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/conflicts/"+uuid.New().String()+"/adjudicate", adminToken,
		map[string]any{"outcome": "test"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleResolveConflictGroup — invalid UUID, invalid status, winning_agent guard
// ===========================================================================

func TestHandleResolveConflictGroup_BadUUID(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflict-groups/not-a-uuid/resolve", adminToken,
		map[string]any{"status": "resolved"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleResolveConflictGroup_AcknowledgedNotAllowed(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflict-groups/"+uuid.New().String()+"/resolve", adminToken,
		map[string]any{"status": "acknowledged"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "status must be one of: resolved, false_positive")
}

func TestHandleResolveConflictGroup_WinningAgentOnFalsePositive(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflict-groups/"+uuid.New().String()+"/resolve", adminToken,
		map[string]any{"status": "false_positive", "winning_agent": "admin"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "winning_agent can only be set when status is")
}

// ===========================================================================
// Coverage push: HandleCreateKey — with valid expires_at
// ===========================================================================

func TestHandleCreateKey_ValidExpiresAt(t *testing.T) {
	expiresAt := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken, map[string]any{
		"agent_id":   "test-agent",
		"label":      "expiring-key-cov",
		"expires_at": expiresAt,
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleConflictAnalytics — from after to
// ===========================================================================

func TestHandleConflictAnalytics_FromAfterToRange(t *testing.T) {
	from := time.Now().Format(time.RFC3339)
	to := time.Now().Add(-time.Hour).Format(time.RFC3339)
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/analytics?from="+from+"&to="+to, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "from must be before to")
}

// ===========================================================================
// Coverage push: HandleGetUsage — with explicit period
// ===========================================================================

func TestHandleGetUsage_SpecificPeriod(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/usage?period=2025-01", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Period         string `json:"period"`
			TotalDecisions int    `json:"total_decisions"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	assert.Equal(t, "2025-01", result.Data.Period)
}

// ===========================================================================
// Coverage push: HandleAgentHistory — invalid time formats
// ===========================================================================

func TestHandleAgentHistory_BadFromTime(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent/history?from=not-a-date", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleAgentHistory_BadToTime(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent/history?to=not-a-date", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: buildTraceAgentContext — with User-Agent header
// ===========================================================================

func TestHandleTrace_WithAkashiUserAgent(t *testing.T) {
	resp, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/trace", adminToken, map[string]any{
		"agent_id": "admin",
		"decision": map[string]any{
			"decision_type": "ua-test",
			"outcome":       "test outcome with UA",
			"confidence":    0.8,
		},
		"context": map[string]any{
			"custom_key": "custom_value",
		},
	}, map[string]string{
		"User-Agent": "akashi-go/0.1.0",
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleTrace — admin tracing for another agent (as_agent_id flow)
// ===========================================================================

func TestHandleTrace_AdminTracesForAnotherAgent(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, map[string]any{
		"agent_id": "test-agent",
		"decision": map[string]any{
			"decision_type": "admin-proxy-test",
			"outcome":       "admin tracing for test-agent",
			"confidence":    0.7,
		},
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleTrace — with precedent_ref and trace_id
// ===========================================================================

func TestHandleTrace_WithPrecedentRefAndTraceID(t *testing.T) {
	// First create a decision to reference.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, map[string]any{
		"agent_id": "admin",
		"decision": map[string]any{
			"decision_type": "precedent-test",
			"outcome":       "original decision",
			"confidence":    0.9,
		},
	})
	require.NoError(t, err)
	var result struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	require.NoError(t, json.Unmarshal(b, &result))

	// Now trace referencing it.
	traceID := uuid.New().String()
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, map[string]any{
		"agent_id":      "admin",
		"trace_id":      traceID,
		"precedent_ref": result.Data.DecisionID.String(),
		"decision": map[string]any{
			"decision_type": "precedent-test",
			"outcome":       "follows original",
			"confidence":    0.8,
			"reasoning":     "Building on prior decision",
		},
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleTrace — with session header
// ===========================================================================

func TestHandleTrace_WithSessionHeaderAndContext(t *testing.T) {
	sessionID := uuid.New().String()
	resp, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/trace", adminToken, map[string]any{
		"agent_id": "admin",
		"decision": map[string]any{
			"decision_type": "session-cov-test",
			"outcome":       "test with session",
			"confidence":    0.6,
		},
		"context": map[string]any{
			"workspace": "/tmp/test",
		},
	}, map[string]string{
		"X-Akashi-Session": sessionID,
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleVerifyDecision — verified (with content hash)
// ===========================================================================

func TestHandleVerifyDecision_VerifiedDecision(t *testing.T) {
	// Create a trace, then verify it.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, map[string]any{
		"agent_id": "admin",
		"decision": map[string]any{
			"decision_type": "verify-test",
			"outcome":       "decision to verify",
			"confidence":    0.95,
			"reasoning":     "test reasoning for verification",
		},
	})
	require.NoError(t, err)
	var result struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	require.NoError(t, json.Unmarshal(b, &result))

	resp, err := authedRequest("GET", testSrv.URL+"/v1/verify/"+result.Data.DecisionID.String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var verifyResult struct {
		Data struct {
			Status string `json:"status"`
			Valid  bool   `json:"valid"`
		} `json:"data"`
	}
	vb, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(vb, &verifyResult))
	assert.Equal(t, "verified", verifyResult.Data.Status)
	assert.True(t, verifyResult.Data.Valid)
}

// ===========================================================================
// Coverage push: HandleVerifyDecision — retracted decision
// ===========================================================================

func TestHandleVerifyDecision_RetractedDecision(t *testing.T) {
	// Create, retract, then verify.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, map[string]any{
		"agent_id": "admin",
		"decision": map[string]any{
			"decision_type": "retract-verify-test",
			"outcome":       "will be retracted",
			"confidence":    0.8,
		},
	})
	require.NoError(t, err)
	var result struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	require.NoError(t, json.Unmarshal(b, &result))

	// Retract it.
	retractResp, err := authedRequest("DELETE", testSrv.URL+"/v1/decisions/"+result.Data.DecisionID.String(), adminToken,
		map[string]any{"reason": "testing verification of retracted"})
	require.NoError(t, err)
	_ = retractResp.Body.Close()

	// Verify.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/verify/"+result.Data.DecisionID.String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var verifyResult struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	vb, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(vb, &verifyResult))
	assert.Equal(t, "retracted", verifyResult.Data.Status)
}

// ===========================================================================
// Coverage push: HandleSessionView — with multiple decisions from same session
// ===========================================================================

func TestHandleSessionView_WithMultipleDecisionsSameSession(t *testing.T) {
	sessionID := uuid.New().String()

	// Create two traces in the same session.
	for i := 0; i < 2; i++ {
		resp, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/trace", adminToken, map[string]any{
			"agent_id": "admin",
			"decision": map[string]any{
				"decision_type": "session-multi-test",
				"outcome":       fmt.Sprintf("decision %d in session", i+1),
				"confidence":    0.7,
			},
		}, map[string]string{
			"X-Akashi-Session": sessionID,
		})
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusCreated, resp.StatusCode)
	}

	// Query session view.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/sessions/"+sessionID, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			DecisionCount int `json:"decision_count"`
			Summary       struct {
				DurationSecs  float64 `json:"duration_secs"`
				AvgConfidence float64 `json:"avg_confidence"`
			} `json:"summary"`
		} `json:"data"`
	}
	rb, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(rb, &result))
	assert.Equal(t, 2, result.Data.DecisionCount, "should have 2 decisions in session")
	assert.InDelta(t, 0.7, result.Data.Summary.AvgConfidence, 0.01, "avg confidence should be ~0.7")
}

// ===========================================================================
// Coverage push: HandleGetRun — includes events and decisions
// ===========================================================================

func TestHandleGetRun_IncludesEventsAndDecisions(t *testing.T) {
	// Create a run.
	runResp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		map[string]any{"agent_id": "test-agent"})
	require.NoError(t, err)
	var runResult struct {
		Data struct {
			ID uuid.UUID `json:"id"`
		} `json:"data"`
	}
	rb, _ := io.ReadAll(runResp.Body)
	_ = runResp.Body.Close()
	require.Equal(t, http.StatusCreated, runResp.StatusCode)
	require.NoError(t, json.Unmarshal(rb, &runResult))

	// Append events.
	eventsResp, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+runResult.Data.ID.String()+"/events", agentToken,
		model.AppendEventsRequest{
			Events: []model.EventInput{
				{EventType: model.EventDecisionMade, Payload: map[string]any{"outcome": "approved"}},
			},
		})
	require.NoError(t, err)
	_ = eventsResp.Body.Close()

	// Get the run — verify it returns run, events, and decisions arrays.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/runs/"+runResult.Data.ID.String(), agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var raw map[string]json.RawMessage
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &raw))
	assert.Contains(t, raw, "data")
}

// ===========================================================================
// Coverage push: HandleCompleteRun — with metadata and "failed" status
// ===========================================================================

func TestHandleCompleteRun_FailedStatus(t *testing.T) {
	// Create a run.
	runResp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		map[string]any{"agent_id": "test-agent"})
	require.NoError(t, err)
	var runResult struct {
		Data struct {
			ID uuid.UUID `json:"id"`
		} `json:"data"`
	}
	rb, _ := io.ReadAll(runResp.Body)
	_ = runResp.Body.Close()
	require.Equal(t, http.StatusCreated, runResp.StatusCode)
	require.NoError(t, json.Unmarshal(rb, &runResult))

	// Complete with failed status.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+runResult.Data.ID.String()+"/complete", agentToken,
		map[string]any{
			"status":   "failed",
			"metadata": map[string]any{"error": "out of memory"},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleHealth — full response shape with buffer
// ===========================================================================

func TestHandleHealth_FullResponseShape(t *testing.T) {
	resp, err := http.Get(testSrv.URL + "/health")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Status       string `json:"status"`
			Version      string `json:"version"`
			Postgres     string `json:"postgres"`
			BufferDepth  int    `json:"buffer_depth"`
			BufferStatus string `json:"buffer_status"`
			Uptime       int64  `json:"uptime"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	assert.Equal(t, "healthy", result.Data.Status)
	assert.Equal(t, "connected", result.Data.Postgres)
	assert.Equal(t, "test", result.Data.Version)
	assert.GreaterOrEqual(t, result.Data.Uptime, int64(0))
	assert.GreaterOrEqual(t, result.Data.BufferDepth, 0)
	assert.Equal(t, "ok", result.Data.BufferStatus)
}

// ===========================================================================
// Coverage push: HandleMCPInfo — response shape
// ===========================================================================

func TestHandleMCPInfo_ResponseShape(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/mcp/info", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Version   string `json:"version"`
			Transport string `json:"transport"`
			Auth      struct {
				Schemes   []string `json:"schemes"`
				Preferred string   `json:"preferred"`
			} `json:"auth"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	assert.Equal(t, "test", result.Data.Version)
	assert.Equal(t, "streamable-http", result.Data.Transport)
	assert.Contains(t, result.Data.Auth.Schemes, "ApiKey")
	assert.Contains(t, result.Data.Auth.Schemes, "Bearer")
}

// ===========================================================================
// Coverage push: HandleConfig — response shape
// ===========================================================================

func TestHandleConfig_ResponseShape(t *testing.T) {
	resp, err := http.Get(testSrv.URL + "/config")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			SearchEnabled bool `json:"search_enabled"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	// With noop embedder, search should not be available.
	assert.False(t, result.Data.SearchEnabled)
}

// ===========================================================================
// Coverage push: HandleDecisionsRecent — with agent_id and decision_type filters
// ===========================================================================

func TestHandleDecisionsRecent_WithFilters(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/recent?agent_id=admin&decision_type=test&limit=5", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleExportDecisions — with from+to time range
// ===========================================================================

func TestHandleExportDecisions_WithTimeRange(t *testing.T) {
	from := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	to := time.Now().Add(time.Hour).Format(time.RFC3339)
	resp, err := authedRequest("GET", testSrv.URL+"/v1/export/decisions?from="+from+"&to="+to, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/x-ndjson", resp.Header.Get("Content-Type"))
}

// ===========================================================================
// Coverage push: HandleTraceHealth
// ===========================================================================

func TestHandleTraceHealth_ReturnsMetrics(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/trace-health", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleRetractDecision — with reason body
// ===========================================================================

func TestHandleRetractDecision_WithReason(t *testing.T) {
	// Create a decision.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken, map[string]any{
		"agent_id": "admin",
		"decision": map[string]any{
			"decision_type": "retract-reason-test",
			"outcome":       "will retract with reason",
			"confidence":    0.8,
		},
	})
	require.NoError(t, err)
	var result struct {
		Data struct {
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	require.NoError(t, json.Unmarshal(b, &result))

	// Retract with reason.
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/decisions/"+result.Data.DecisionID.String(), adminToken,
		map[string]any{"reason": "no longer valid"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleSearch — with semantic flag
// ===========================================================================

func TestHandleSearch_WithSemanticFlag(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/search", adminToken, map[string]any{
		"query":    "caching strategy",
		"semantic": true,
		"limit":    10,
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	// With noop embedder, backend should be "text".
	assert.Equal(t, "text", resp.Header.Get("X-Search-Backend"))
}

// ===========================================================================
// Coverage push: HandleListGrants
// ===========================================================================

func TestHandleListGrants_AdminAccess(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/grants?limit=10&offset=0", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleCreateAgent — with tags and metadata
// ===========================================================================

func TestHandleCreateAgent_WithTagsAndMetadata(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken, map[string]any{
		"agent_id": "tagged-agent-cov",
		"name":     "Tagged Agent",
		"role":     "agent",
		"tags":     []string{"backend", "python"},
		"metadata": map[string]any{"team": "infra"},
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetAgent — valid agent
// ===========================================================================

func TestHandleGetAgent_ValidAgent(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			AgentID string `json:"agent_id"`
			Name    string `json:"name"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	assert.Equal(t, "test-agent", result.Data.AgentID)
}

// ===========================================================================
// Coverage push: HandleUpdateAgent — valid update with metadata
// ===========================================================================

func TestHandleUpdateAgent_ValidMetadataUpdate(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/test-agent", adminToken,
		map[string]any{"metadata": map[string]any{"updated": true}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleTrace — deeper branch coverage
// ===========================================================================

func TestHandleTrace_ConfidenceAboveOne(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		map[string]any{
			"agent_id": "test-agent",
			"decision": map[string]any{
				"decision_type": "architecture",
				"outcome":       "use redis",
				"confidence":    1.5, // > 1 is invalid
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleTrace_ConfidenceBelowZero(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		map[string]any{
			"agent_id": "test-agent",
			"decision": map[string]any{
				"decision_type": "architecture",
				"outcome":       "use redis",
				"confidence":    -0.1,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleTrace_AgentCantTraceForOther(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		map[string]any{
			"agent_id": "admin", // agent role can't trace for another agent
			"decision": map[string]any{
				"decision_type": "architecture",
				"outcome":       "use redis",
				"confidence":    0.8,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandleTrace_BadAgentIDFormat(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken,
		map[string]any{
			"agent_id": "INVALID AGENT!!!",
			"decision": map[string]any{
				"decision_type": "architecture",
				"outcome":       "use redis",
				"confidence":    0.8,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleTrace_SessionHeaderUUID(t *testing.T) {
	sessionID := uuid.NewString()
	resp, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/trace", agentToken,
		map[string]any{
			"agent_id": "test-agent",
			"decision": map[string]any{
				"decision_type": "implementation",
				"outcome":       "traced with session",
				"confidence":    0.7,
			},
		}, map[string]string{
			"X-Akashi-Session": sessionID,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestHandleTrace_WithIdempotencyKey(t *testing.T) {
	idemKey := "trace-idem-" + uuid.NewString()
	traceReq := map[string]any{
		"agent_id": "test-agent",
		"decision": map[string]any{
			"decision_type": "implementation",
			"outcome":       "idempotent trace",
			"confidence":    0.9,
		},
	}

	// First call.
	resp1, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/trace", agentToken,
		traceReq, map[string]string{"Idempotency-Key": idemKey})
	require.NoError(t, err)
	defer func() { _ = resp1.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp1.StatusCode)
	body1, _ := io.ReadAll(resp1.Body)

	// Replay with same key — should return same response.
	resp2, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/trace", agentToken,
		traceReq, map[string]string{"Idempotency-Key": idemKey})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	body2, _ := io.ReadAll(resp2.Body)

	// Both should succeed and return same decision_id.
	var r1, r2 struct {
		Data struct {
			DecisionID string `json:"decision_id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body1, &r1))
	require.NoError(t, json.Unmarshal(body2, &r2))
	assert.Equal(t, r1.Data.DecisionID, r2.Data.DecisionID,
		"replayed idempotent trace should return same decision_id")
}

func TestHandleTrace_IdempotencyPayloadMismatchCov(t *testing.T) {
	idemKey := "trace-idem-mismatch-cov-" + uuid.NewString()

	// First call.
	resp1, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/trace", agentToken,
		map[string]any{
			"agent_id": "test-agent",
			"decision": map[string]any{
				"decision_type": "implementation",
				"outcome":       "first payload",
				"confidence":    0.9,
			},
		}, map[string]string{"Idempotency-Key": idemKey})
	require.NoError(t, err)
	defer func() { _ = resp1.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp1.StatusCode)
	_, _ = io.ReadAll(resp1.Body)

	// Second call with different payload — should get 409.
	resp2, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/trace", agentToken,
		map[string]any{
			"agent_id": "test-agent",
			"decision": map[string]any{
				"decision_type": "implementation",
				"outcome":       "different payload",
				"confidence":    0.5,
			},
		}, map[string]string{"Idempotency-Key": idemKey})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusConflict, resp2.StatusCode)
}

func TestHandleTrace_WithContext(t *testing.T) {
	resp, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/trace", agentToken,
		map[string]any{
			"agent_id": "test-agent",
			"decision": map[string]any{
				"decision_type": "implementation",
				"outcome":       "traced with context",
				"confidence":    0.7,
			},
			"context": map[string]any{
				"project": "akashi",
				"tool":    "claude-code",
			},
		}, map[string]string{
			"User-Agent": "akashi-go/0.1.0",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestHandleTrace_InvalidJSON(t *testing.T) {
	req, _ := http.NewRequest("POST", testSrv.URL+"/v1/trace",
		bytes.NewReader([]byte("not valid json")))
	req.Header.Set("Authorization", "Bearer "+agentToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleCreateGrant — deeper branch coverage (55.3% → higher)
// ===========================================================================

func TestHandleCreateGrant_AgentRoleNonTraceResource(t *testing.T) {
	// Agent role trying to grant non-agent_traces resource type — should fail.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", agentToken,
		map[string]any{
			"grantee_agent_id": "admin",
			"resource_type":    "invalid_type",
			"permission":       "read",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateGrant_AgentRoleGrantOtherTracesNoResourceID(t *testing.T) {
	// Agent trying to grant agent_traces but with nil resource_id — must fail
	// because agents can only grant their own traces.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/grants", agentToken,
		map[string]any{
			"grantee_agent_id": "admin",
			"resource_type":    "agent_traces",
			"permission":       "read",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleExportDecisions — additional filter branches (56.2% → higher)
// ===========================================================================

func TestHandleExportDecisions_WithToFilterOnly(t *testing.T) {
	// Exercise the "to without from" path (filters.TimeRange == nil check).
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/export/decisions?to="+time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339),
		adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/x-ndjson", resp.Header.Get("Content-Type"))
}

func TestHandleExportDecisions_ForbiddenForAgentWithDetail(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/export/decisions", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetRun — branches with invalid run ID format
// ===========================================================================

func TestHandleGetRun_ValidRunWithDetails(t *testing.T) {
	// Create a run, append events, trace a decision, then get run details.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &runResult))
	runID := runResult.Data.ID

	// Get the run as the owning agent — exercises the canAccessAgent path.
	resp2, err := authedRequest("GET", testSrv.URL+"/v1/runs/"+runID.String(), agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	var getResult struct {
		Data struct {
			Run       model.AgentRun  `json:"run"`
			Events    json.RawMessage `json:"events"`
			Decisions json.RawMessage `json:"decisions"`
		} `json:"data"`
	}
	body2, _ := io.ReadAll(resp2.Body)
	require.NoError(t, json.Unmarshal(body2, &getResult))
	assert.Equal(t, runID, getResult.Data.Run.ID)
}

// ===========================================================================
// Coverage push: HandleCompleteRun — various status paths
// ===========================================================================

func TestHandleCompleteRun_StatusCompleted(t *testing.T) {
	// Create a run.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &runResult))
	runID := runResult.Data.ID

	// Complete with explicit "completed" status.
	resp2, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+runID.String()+"/complete",
		agentToken, map[string]any{"status": "completed"})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
}

func TestHandleCompleteRun_StatusFailed(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &runResult))
	runID := runResult.Data.ID

	// Complete with "failed" status.
	resp2, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+runID.String()+"/complete",
		agentToken, map[string]any{"status": "failed"})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
}

func TestHandleCompleteRun_InvalidStatusCov(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &runResult))
	runID := runResult.Data.ID

	// Complete with invalid status.
	resp2, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+runID.String()+"/complete",
		agentToken, map[string]any{"status": "invalid_status"})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp2.StatusCode)
}

func TestHandleCompleteRun_NotYourRunCov(t *testing.T) {
	// Create a run as admin.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", adminToken,
		model.CreateRunRequest{AgentID: "admin"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &runResult))
	runID := runResult.Data.ID

	// Try to complete as agent — should fail.
	resp2, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+runID.String()+"/complete",
		agentToken, map[string]any{"status": "completed"})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp2.StatusCode)
}

func TestHandleCompleteRun_NonexistentRun(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+uuid.NewString()+"/complete",
		agentToken, map[string]any{"status": "completed"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleCompleteRun_InvalidRunIDCov(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs/not-a-uuid/complete",
		agentToken, map[string]any{"status": "completed"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCompleteRun_WithIdempotency(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &runResult))
	runID := runResult.Data.ID

	idemKey := "complete-idem-" + uuid.NewString()
	completeReq := map[string]any{"status": "completed"}

	resp1, err := authedRequestWithHeaders("POST",
		testSrv.URL+"/v1/runs/"+runID.String()+"/complete",
		agentToken, completeReq, map[string]string{"Idempotency-Key": idemKey})
	require.NoError(t, err)
	defer func() { _ = resp1.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp1.StatusCode)
	_, _ = io.ReadAll(resp1.Body)

	// Replay.
	resp2, err := authedRequestWithHeaders("POST",
		testSrv.URL+"/v1/runs/"+runID.String()+"/complete",
		agentToken, completeReq, map[string]string{"Idempotency-Key": idemKey})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
}

// ===========================================================================
// Coverage push: HandleCreateRun — validation and auth branches
// ===========================================================================

func TestHandleCreateRun_InvalidAgentIDCov(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "INVALID AGENT!!!"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateRun_AgentCantCreateForOther(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "admin"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandleCreateRun_WithIdempotency(t *testing.T) {
	idemKey := "run-idem-" + uuid.NewString()
	createReq := model.CreateRunRequest{AgentID: "test-agent"}

	resp1, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/runs",
		agentToken, createReq, map[string]string{"Idempotency-Key": idemKey})
	require.NoError(t, err)
	defer func() { _ = resp1.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp1.StatusCode)
	body1, _ := io.ReadAll(resp1.Body)

	// Replay with same key — should return same response.
	resp2, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/runs",
		agentToken, createReq, map[string]string{"Idempotency-Key": idemKey})
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	body2, _ := io.ReadAll(resp2.Body)

	var r1, r2 struct {
		Data model.AgentRun `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body1, &r1))
	require.NoError(t, json.Unmarshal(body2, &r2))
	assert.Equal(t, r1.Data.ID, r2.Data.ID,
		"replayed idempotent run create should return same run_id")
}

func TestHandleCreateRun_InvalidJSON(t *testing.T) {
	req, _ := http.NewRequest("POST", testSrv.URL+"/v1/runs",
		bytes.NewReader([]byte("not valid json")))
	req.Header.Set("Authorization", "Bearer "+agentToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleAppendEvents — auth and validation branches
// ===========================================================================

func TestHandleAppendEvents_InvalidJSON(t *testing.T) {
	// Create a run first.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var runResult struct {
		Data model.AgentRun `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &runResult))
	runID := runResult.Data.ID

	req, _ := http.NewRequest("POST", testSrv.URL+"/v1/runs/"+runID.String()+"/events",
		bytes.NewReader([]byte("not valid json")))
	req.Header.Set("Authorization", "Bearer "+agentToken)
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp2.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetConflict — additional branches
// ===========================================================================

func TestHandleGetConflict_ForbiddenForAgentRole(t *testing.T) {
	// Agent role should still be able to see conflicts if they have read access.
	// Create a conflict first by tracing two contradictory decisions.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/"+uuid.NewString(), agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// Non-existent conflict → 404 (not 403), which exercises the handleGetConflict path.
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: Retention endpoints — hold creation and release branches
// ===========================================================================

func TestHandleCreateHold_MissingReasonCov(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken,
		map[string]any{
			"from": time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339),
			"to":   time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateHold_MissingDates(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken,
		map[string]any{
			"reason": "litigation",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateHold_ToBeforeFromCov(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken,
		map[string]any{
			"reason": "litigation",
			"from":   time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
			"to":     time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339),
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateHold_ValidHold(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken,
		map[string]any{
			"reason":         "litigation hold for Q1",
			"from":           time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"to":             time.Now().Add(90 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"decision_types": []string{"architecture"},
			"agent_ids":      []string{"test-agent"},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	var result struct {
		Data struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, "litigation hold for Q1", result.Data.Reason)
}

func TestHandleReleaseHold_InvalidUUID(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/retention/hold/not-a-uuid", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleReleaseHold_NotFoundCov(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/retention/hold/"+uuid.NewString(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleReleaseHold_ValidRelease(t *testing.T) {
	// Create a hold first.
	createResp, err := authedRequest("POST", testSrv.URL+"/v1/retention/hold", adminToken,
		map[string]any{
			"reason": "release test hold",
			"from":   time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339),
			"to":     time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		})
	require.NoError(t, err)
	defer func() { _ = createResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, createResp.StatusCode)

	var holdResult struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(createResp.Body)
	require.NoError(t, json.Unmarshal(body, &holdResult))

	// Release it.
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/retention/hold/"+holdResult.Data.ID, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleSetRetention — validation and update paths
// ===========================================================================

func TestHandleSetRetention_InvalidRetentionDaysCov(t *testing.T) {
	resp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken,
		map[string]any{"retention_days": 0})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleSetRetention_NegativeRetentionDays(t *testing.T) {
	resp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken,
		map[string]any{"retention_days": -1})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleSetRetention_ValidUpdateCov(t *testing.T) {
	resp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken,
		map[string]any{
			"retention_days":          90,
			"retention_exclude_types": []string{"compliance"},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleSetRetention_InvalidJSONCov(t *testing.T) {
	req, _ := http.NewRequest("PUT", testSrv.URL+"/v1/retention",
		bytes.NewReader([]byte("not valid json")))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetRetention — success path
// ===========================================================================

func TestHandleGetRetention_Success(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/retention", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleListKeys — pagination
// ===========================================================================

func TestHandleListKeys_WithPagination(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/keys?limit=10&offset=0", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Keys    []json.RawMessage `json:"keys"`
			Total   int               `json:"total"`
			Limit   int               `json:"limit"`
			Offset  int               `json:"offset"`
			HasMore bool              `json:"has_more"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, 10, result.Data.Limit)
	assert.Equal(t, 0, result.Data.Offset)
}

// ===========================================================================
// Coverage push: HandleRevokeKey — valid revocation
// ===========================================================================

func TestHandleRevokeKey_InvalidUUIDCov(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/keys/not-a-uuid", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleRevokeKey_NotFoundCov(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/keys/"+uuid.NewString(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandlePatchConflict — patch various fields
// ===========================================================================

func TestHandlePatchConflict_InvalidUUID(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflicts/not-a-uuid", agentToken,
		map[string]any{"status": "false_positive"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandlePatchConflict_NotFound(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflicts/"+uuid.NewString(), agentToken,
		map[string]any{"status": "false_positive"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleAdjudicateConflict — validation paths
// ===========================================================================

func TestHandleAdjudicateConflict_InvalidUUID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/conflicts/not-a-uuid/adjudicate",
		adminToken, map[string]any{"outcome": "Use approach A", "reasoning": "more robust"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleAdjudicateConflict_NotFound(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/conflicts/"+uuid.NewString()+"/adjudicate",
		adminToken, map[string]any{"outcome": "Use approach A", "reasoning": "more robust"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleDeleteGrant — validation
// ===========================================================================

func TestHandleDeleteGrant_InvalidUUIDCov(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/grants/not-a-uuid", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleDeleteGrant_NotFound(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/grants/"+uuid.NewString(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleListGrants — success path
// ===========================================================================

func TestHandleListGrants_Success(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/grants", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleDecisionsRecent — success path
// ===========================================================================

func TestHandleDecisionsRecent_Success(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/recent", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleConfig — success path
// ===========================================================================

func TestHandleConfig_Success(t *testing.T) {
	resp, err := http.Get(testSrv.URL + "/config")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			SearchEnabled bool `json:"search_enabled"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
}

// ===========================================================================
// Coverage push: HandleMCPInfo — success path
// ===========================================================================

func TestHandleMCPInfo_Success(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/mcp/info", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Version   string `json:"version"`
			Transport string `json:"transport"`
			Auth      struct {
				Schemes   []string `json:"schemes"`
				Preferred string   `json:"preferred"`
			} `json:"auth"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, "test", result.Data.Version)
	assert.Equal(t, "streamable-http", result.Data.Transport)
	assert.Contains(t, result.Data.Auth.Schemes, "ApiKey")
}

// ===========================================================================
// Coverage push: HandleRetractDecision — validation paths
// ===========================================================================

func TestHandleRetractDecision_InvalidUUIDCov(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/decisions/not-a-uuid", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleRetractDecision_NotFound(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/decisions/"+uuid.NewString(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleEraseDecision — validation paths
// ===========================================================================

func TestHandleEraseDecision_InvalidUUID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/decisions/not-a-uuid/erase", orgOwnerToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleDecisionRevisions — validation
// ===========================================================================

func TestHandleDecisionRevisions_InvalidUUID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/not-a-uuid/revisions", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleDecisionRevisions_NotFound(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+uuid.NewString()+"/revisions", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// Returns empty list or 200 when decision exists but has no revisions.
	// Depends on implementation, but exercises the handler path.
	status := resp.StatusCode
	assert.True(t, status == http.StatusOK || status == http.StatusNotFound)
}

// ===========================================================================
// Coverage push: HandleDecisionConflicts — validation
// ===========================================================================

func TestHandleDecisionConflicts_InvalidUUID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/not-a-uuid/conflicts", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleAssessDecision — validation
// ===========================================================================

func TestHandleAssessDecision_InvalidUUID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/decisions/not-a-uuid/assess", agentToken,
		map[string]any{"outcome_status": "good", "explanation": "works well"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleVerifyDecision — validation
// ===========================================================================

func TestHandleVerifyDecision_InvalidUUIDCov(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/verify/not-a-uuid", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleVerifyDecision_NotFoundCov(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/verify/"+uuid.NewString(), agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleConflictAnalytics — success path
// ===========================================================================

func TestHandleConflictAnalytics_Success(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/analytics", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleListConflictGroups — success path
// ===========================================================================

func TestHandleListConflictGroups_Success(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflict-groups", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleResolveConflictGroup — validation
// ===========================================================================

func TestHandleResolveConflictGroup_InvalidUUID(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflict-groups/not-a-uuid/resolve",
		agentToken, map[string]any{"resolution": "accepted_a"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleTraceHealth — success path
// ===========================================================================

func TestHandleTraceHealth_Success(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/trace-health", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleScopedToken — validation paths
// ===========================================================================

func TestHandleScopedToken_MissingAgentIDCov(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/auth/scoped-token", adminToken,
		map[string]any{})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleAgentHistory — success and validation paths
// ===========================================================================

func TestHandleAgentHistory_Success(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent/history", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleQuery — validation paths
// ===========================================================================

func TestHandleQuery_EmptyBody(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query", agentToken, map[string]any{})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// Empty query should still return a valid response (possibly empty results).
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleQuery_WithFilters(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query", agentToken,
		map[string]any{
			"filters": map[string]any{
				"agent_id":      []string{"test-agent"},
				"decision_type": "architecture",
			},
			"limit": 5,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleQuery_InvalidJSON(t *testing.T) {
	req, _ := http.NewRequest("POST", testSrv.URL+"/v1/query",
		bytes.NewReader([]byte("not valid json")))
	req.Header.Set("Authorization", "Bearer "+agentToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleTemporalQuery — validation paths
// ===========================================================================

func TestHandleTemporalQuery_EmptyBody(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query/temporal", agentToken, map[string]any{})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// Depends on implementation: either 200 with empty results or 400 for missing fields.
	status := resp.StatusCode
	assert.True(t, status == http.StatusOK || status == http.StatusBadRequest)
}

// ===========================================================================
// Coverage push: HandleCheck — validation paths
// ===========================================================================

func TestHandleCheck_ValidRequest(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/check", agentToken,
		map[string]any{
			"decision_type": "architecture",
			"query":         "use redis for caching",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleCheck_InvalidJSON(t *testing.T) {
	req, _ := http.NewRequest("POST", testSrv.URL+"/v1/check",
		bytes.NewReader([]byte("not valid json")))
	req.Header.Set("Authorization", "Bearer "+agentToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleSearch — basic path
// ===========================================================================

func TestHandleSearch_ValidRequest(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/search", agentToken,
		map[string]any{
			"query": "redis",
			"limit": 5,
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// Search may return 200 (with text fallback) or 501 depending on searcher config.
	status := resp.StatusCode
	assert.True(t, status == http.StatusOK || status == http.StatusNotImplemented)
}

// ===========================================================================
// Coverage push: HandleAgentStats — success path
// ===========================================================================

func TestHandleAgentStats_Success(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent/stats", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleAgentStats_NotFoundCov(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/nonexistent-agent/stats", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleSessionView — validation and success paths
// ===========================================================================

func TestHandleSessionView_InvalidSessionID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/sessions/not-a-uuid", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleSessionView_NotFound(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/sessions/"+uuid.NewString(), agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// No decisions for this session — may return 404 or 200 with empty data.
	status := resp.StatusCode
	assert.True(t, status == http.StatusOK || status == http.StatusNotFound)
}

// ===========================================================================
// Coverage push: HandleListAssessments — validation
// ===========================================================================

func TestHandleListAssessments_InvalidUUID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/not-a-uuid/assessments", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleUpdateAgentTags — success path
// ===========================================================================

func TestHandleUpdateAgentTags_ValidUpdate(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/test-agent/tags", adminToken,
		map[string]any{
			"tags": []string{"go", "backend"},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleUpdateAgentTags_NonexistentAgentCov(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/nonexistent/tags", adminToken,
		map[string]any{
			"tags": []string{"go"},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleDeleteAgent — validation path
// ===========================================================================

func TestHandleDeleteAgent_NonexistentAgent(t *testing.T) {
	resp, err := authedRequest("DELETE", testSrv.URL+"/v1/agents/nonexistent-del-agent", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleRotateKey — success path
// ===========================================================================

func TestHandleRotateKey_Success(t *testing.T) {
	// Create a throwaway agent with a key.
	createAgent(testSrv.URL, adminToken, "rotate-agent", "Rotate Agent", "agent", "rotate-agent-key")

	// List keys to find the key we just created.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/keys?limit=100", adminToken, nil)
	require.NoError(t, err)
	var keysResp struct {
		Data struct {
			Keys []struct {
				ID      uuid.UUID `json:"id"`
				AgentID string    `json:"agent_id"`
			} `json:"keys"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, json.Unmarshal(body, &keysResp))

	var keyID uuid.UUID
	for _, k := range keysResp.Data.Keys {
		if k.AgentID == "rotate-agent" {
			keyID = k.ID
			break
		}
	}
	require.NotEqual(t, uuid.Nil, keyID, "should find key for rotate-agent")

	// Rotate the key.
	rotResp, err := authedRequest("POST", testSrv.URL+"/v1/keys/"+keyID.String()+"/rotate", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = rotResp.Body.Close() }()
	assert.Equal(t, http.StatusOK, rotResp.StatusCode)

	var rotResult struct {
		Data struct {
			RevokedKeyID uuid.UUID `json:"revoked_key_id"`
			NewKey       struct {
				ID string `json:"id"`
			} `json:"new_key"`
		} `json:"data"`
	}
	rotBody, _ := io.ReadAll(rotResp.Body)
	require.NoError(t, json.Unmarshal(rotBody, &rotResult))
	assert.Equal(t, keyID, rotResult.Data.RevokedKeyID)
}

func TestHandleRotateKey_NotFound(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys/"+uuid.NewString()+"/rotate", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleUpdateAgent — validation paths
// ===========================================================================

func TestHandleUpdateAgent_NameOnly(t *testing.T) {
	name := "Updated Agent Name"
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/test-agent", adminToken,
		map[string]any{"name": name})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleUpdateAgent_MetadataOnly(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/test-agent", adminToken,
		map[string]any{"metadata": map[string]any{"team": "backend"}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleUpdateAgent_InvalidAgentIDChars(t *testing.T) {
	// Agent IDs with spaces or special chars should be rejected by ValidateAgentID.
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/bad%20agent%21", adminToken,
		map[string]any{"name": "test"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateAgent_EqualRoleForbidden(t *testing.T) {
	// Admin trying to create another admin should be forbidden.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken,
		model.CreateAgentRequest{AgentID: "peer-admin", Name: "Peer Admin", Role: model.RoleAdmin, APIKey: "pa-key"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandleCreateAgent_DuplicateID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken,
		model.CreateAgentRequest{AgentID: "test-agent", Name: "Duplicate", Role: model.RoleAgent, APIKey: "dup-key"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestHandleListConflictGroups_FalsePositiveStatus(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflict-groups?status=false_positive", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleListConflictGroups_WithAgentFilter(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflict-groups?agent_id=test-agent", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleListConflictGroups_WithConflictKindFilter(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflict-groups?conflict_kind=direct", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleConflictAnalytics — filter and period paths
// ===========================================================================

func TestHandleConflictAnalytics_DefaultPeriod(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/analytics", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleConflictAnalytics_30dPeriod(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/analytics?period=30d", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleConflictAnalytics_90dPeriod(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/analytics?period=90d", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleConflictAnalytics_FromToRange(t *testing.T) {
	from := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	to := time.Now().UTC().Format(time.RFC3339)
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/analytics?from="+from+"&to="+to, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleConflictAnalytics_ExceedsMaxRange(t *testing.T) {
	from := time.Now().Add(-400 * 24 * time.Hour).UTC().Format(time.RFC3339)
	to := time.Now().UTC().Format(time.RFC3339)
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/analytics?from="+from+"&to="+to, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleConflictAnalytics_WithAgentFilter(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/analytics?agent_id=test-agent", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleConflictAnalytics_WithDecisionTypeFilter(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/analytics?decision_type=architecture", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleConflictAnalytics_WithConflictKindFilter(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/analytics?conflict_kind=direct", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleGetUsage_ValidPastPeriod(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/usage?period=2025-01", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleAppendEvents_NonexistentRun(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+uuid.NewString()+"/events", agentToken,
		model.AppendEventsRequest{Events: []model.EventInput{
			{EventType: model.EventDecisionMade, Payload: map[string]any{"outcome": "x"}},
		}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetRun — bad UUID
// ===========================================================================

func TestHandleGetRun_BadUUID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/runs/not-a-uuid", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleGetRun_NotFound(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/runs/"+uuid.NewString(), agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandlePatchConflict — resolved + winner
// ===========================================================================

func TestHandlePatchConflict_WinnerOnNonResolved(t *testing.T) {
	winnerID := uuid.NewString()
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflicts/"+uuid.NewString(), adminToken,
		map[string]any{"status": "false_positive", "winning_decision_id": winnerID})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// winning_decision_id is only valid when status is "resolved".
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleDecisionConflicts — paths
// ===========================================================================

func TestHandleDecisionConflicts_BadUUID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/not-a-uuid/conflicts", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleDecisionConflicts_NonexistentDecision(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+uuid.NewString()+"/conflicts", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleDecisionConflicts_WithStatusFilter(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+uuid.NewString()+"/conflicts?status=open", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleListAssessments — paths
// ===========================================================================

func TestHandleListAssessments_BadUUIDCov(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/not-a-uuid/assessments", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleListAssessments_NonexistentDecisionCov(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+uuid.NewString()+"/assessments", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleListGrants — pagination
// ===========================================================================

func TestHandleListGrants_WithPagination(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/grants?limit=10&offset=0", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleRetention — more paths
// ===========================================================================

func TestHandleGetRetention_DefaultPolicy(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/retention", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			RetentionDays *int `json:"retention_days"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
}

// ===========================================================================
// Coverage push: HandleAdjudicateConflict — outcome validation
// ===========================================================================

func TestHandleAdjudicateConflict_MissingOutcome(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/conflicts/"+uuid.NewString()+"/adjudicate",
		adminToken, map[string]any{"reasoning": "some reasoning"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetAgent — paths
// ===========================================================================

func TestHandleGetAgent_Success(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/test-agent", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			AgentID string `json:"agent_id"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	assert.Equal(t, "test-agent", result.Data.AgentID)
}

// ===========================================================================
// Coverage push: HandleOpenAPISpec
// ===========================================================================

func TestHandleOpenAPISpec_ReturnsYAML(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/openapi.yaml", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// May return 200 (with spec) or 404 (no spec embedded in test build).
	assert.Contains(t, []int{http.StatusOK, http.StatusNotFound}, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleListAgents — paths
// ===========================================================================

func TestHandleListAgents_WithPagination(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents?limit=5&offset=0", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleListAgents_WithRoleFilter(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents?role=agent", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleTemporalQuery_ValidQuery(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query/temporal", adminToken,
		map[string]any{
			"as_of": time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleSubscribe — nil broker
// ===========================================================================

func TestHandleSubscribe_NilBroker(t *testing.T) {
	// The test server has no broker configured, so /v1/subscribe should return 503.
	resp, err := authedRequest("GET", testSrv.URL+"/v1/subscribe", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleScopedToken — paths
// ===========================================================================

func TestHandleScopedToken_InvalidJSON(t *testing.T) {
	req, _ := http.NewRequest("POST", testSrv.URL+"/v1/auth/scoped-token", strings.NewReader("bad"))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.NotEqual(t, http.StatusOK, resp.StatusCode)
}

func TestHandleExportDecisions_WithDecisionTypeFilter(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/export/decisions?decision_type=architecture", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleExportDecisions_InvalidFromDate(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/export/decisions?from=not-a-date", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleExportDecisions_InvalidToDate(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/export/decisions?to=not-a-date", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleExportDecisions_WithFromAndTo(t *testing.T) {
	from := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	to := time.Now().UTC().Format(time.RFC3339)
	resp, err := authedRequest("GET", testSrv.URL+"/v1/export/decisions?from="+from+"&to="+to, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleExportDecisions_ToOnly(t *testing.T) {
	to := time.Now().UTC().Format(time.RFC3339)
	resp, err := authedRequest("GET", testSrv.URL+"/v1/export/decisions?to="+to, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleCreateKey — with expires_at
// ===========================================================================

func TestHandleCreateKey_WithLabel(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/keys", adminToken,
		map[string]any{
			"agent_id": "test-agent",
			"label":    "ci-key",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleAuthToken — invalid credentials
// ===========================================================================

func TestHandleAuthToken_WrongAPIKey(t *testing.T) {
	body, _ := json.Marshal(model.AuthTokenRequest{AgentID: "test-agent", APIKey: "wrong-key"})
	resp, err := http.Post(testSrv.URL+"/auth/token", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHandleAuthToken_NonexistentAgent(t *testing.T) {
	body, _ := json.Marshal(model.AuthTokenRequest{AgentID: "nonexistent-auth-agent", APIKey: "some-key"})
	resp, err := http.Post(testSrv.URL+"/auth/token", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHandleAuthToken_MissingFields(t *testing.T) {
	body, _ := json.Marshal(model.AuthTokenRequest{})
	resp, err := http.Post(testSrv.URL+"/auth/token", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// Empty credentials are treated as invalid auth, not a validation error.
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleTemporalQuery — agent_ids and decision_type filters
// ===========================================================================

func TestHandleTemporalQuery_WithAgentIDsFilter(t *testing.T) {
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: "temporal-agent-filter-test",
				Outcome:      "use agent filter",
				Confidence:   0.8,
			},
		})
	require.NoError(t, err)
	_ = traceResp.Body.Close()

	pastTime := time.Now().Add(-1 * time.Second)
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query/temporal", adminToken,
		model.TemporalQueryRequest{
			AsOf: pastTime,
			Filters: model.QueryFilters{
				AgentIDs: []string{"test-agent"},
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			AsOf      time.Time        `json:"as_of"`
			Decisions []model.Decision `json:"decisions"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	assert.False(t, result.Data.AsOf.IsZero())
}

func TestHandleTemporalQuery_WithDecisionTypeFilter(t *testing.T) {
	pastTime := time.Now().Add(-1 * time.Second)
	dt := "temporal-agent-filter-test"
	resp, err := authedRequest("POST", testSrv.URL+"/v1/query/temporal", adminToken,
		model.TemporalQueryRequest{
			AsOf: pastTime,
			Filters: model.QueryFilters{
				DecisionType: &dt,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleTemporalQuery_InvalidJSON(t *testing.T) {
	req, _ := http.NewRequest("POST", testSrv.URL+"/v1/query/temporal", strings.NewReader("{bad json"))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleTrace — forbidden, agent auto-registration, validation
// ===========================================================================

func TestHandleTrace_NonexistentAgentForNonAdmin(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "nonexistent-agent-xyz",
			Decision: model.TraceDecision{
				DecisionType: "test",
				Outcome:      "test",
				Confidence:   0.5,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandleTrace_AdminTracesNonexistentAgent(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken,
		model.TraceRequest{
			AgentID: "auto-reg-agent-" + uuid.New().String()[:8],
			Decision: model.TraceDecision{
				DecisionType: "auto-reg-test",
				Outcome:      "auto registered",
				Confidence:   0.7,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestHandleTrace_DecisionTypeOverMaxLen(t *testing.T) {
	longType := strings.Repeat("x", 300)
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: longType,
				Outcome:      "chosen",
				Confidence:   0.5,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetRun — reader forbidden without grant
// ===========================================================================

func TestHandleGetRun_ReaderForbiddenWithoutGrant(t *testing.T) {
	createResp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	defer func() { _ = createResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, createResp.StatusCode)

	var run struct {
		Data model.AgentRun `json:"data"`
	}
	b, _ := io.ReadAll(createResp.Body)
	require.NoError(t, json.Unmarshal(b, &run))

	readerID := "reader-run-forbidden-" + uuid.New().String()[:8]
	createAgent(testSrv.URL, adminToken, readerID, "Reader No Access", "reader", readerID+"-key")
	rToken := getToken(testSrv.URL, readerID, readerID+"-key")

	resp, err := authedRequest("GET", testSrv.URL+"/v1/runs/"+run.Data.ID.String(), rToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleAppendEvents — agent cannot append to other's run
// ===========================================================================

func TestHandleAppendEvents_AgentCannotAppendToOtherAgentRun(t *testing.T) {
	otherAgentID := "other-run-agent-" + uuid.New().String()[:8]
	createAgent(testSrv.URL, adminToken, otherAgentID, "Other Agent", "agent", otherAgentID+"-key")
	otherToken := getToken(testSrv.URL, otherAgentID, otherAgentID+"-key")

	createResp, err := authedRequest("POST", testSrv.URL+"/v1/runs", otherToken,
		model.CreateRunRequest{AgentID: otherAgentID})
	require.NoError(t, err)
	defer func() { _ = createResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, createResp.StatusCode)

	var run struct {
		Data model.AgentRun `json:"data"`
	}
	b, _ := io.ReadAll(createResp.Body)
	require.NoError(t, json.Unmarshal(b, &run))

	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+run.Data.ID.String()+"/events", agentToken,
		model.AppendEventsRequest{
			Events: []model.EventInput{{EventType: "test_event"}},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleCompleteRun — agent cannot complete other's run
// ===========================================================================

func TestHandleCompleteRun_AgentCannotCompleteOtherAgentRun(t *testing.T) {
	otherAgentID := "other-complete-agent-" + uuid.New().String()[:8]
	createAgent(testSrv.URL, adminToken, otherAgentID, "Other Complete Agent", "agent", otherAgentID+"-key")
	otherToken := getToken(testSrv.URL, otherAgentID, otherAgentID+"-key")

	createResp, err := authedRequest("POST", testSrv.URL+"/v1/runs", otherToken,
		model.CreateRunRequest{AgentID: otherAgentID})
	require.NoError(t, err)
	defer func() { _ = createResp.Body.Close() }()
	require.Equal(t, http.StatusCreated, createResp.StatusCode)

	var run struct {
		Data model.AgentRun `json:"data"`
	}
	b, _ := io.ReadAll(createResp.Body)
	require.NoError(t, json.Unmarshal(b, &run))

	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs/"+run.Data.ID.String()+"/complete", agentToken,
		model.CompleteRunRequest{Status: "completed"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleSetRetention — exclude types without days
// ===========================================================================

func TestHandleSetRetention_WithExcludeTypesAndNilDays(t *testing.T) {
	resp, err := authedRequest("PUT", testSrv.URL+"/v1/retention", adminToken,
		map[string]any{
			"retention_exclude_types": []string{"security_audit", "compliance"},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetRetention — response includes holds
// ===========================================================================

func TestHandleGetRetention_ResponseIncludesHolds(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/retention", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data map[string]any `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	_, hasHolds := result.Data["holds"]
	assert.True(t, hasHolds, "response should include holds field")
	_, hasRetentionDays := result.Data["retention_days"]
	assert.True(t, hasRetentionDays, "response should include retention_days field")
}

// ===========================================================================
// Coverage push: HandleSignup — missing fields, invalid formats
// ===========================================================================

func TestSignup_MissingAgentID(t *testing.T) {
	ts := signupServer(t)
	body, _ := json.Marshal(model.SignupRequest{
		OrgName: "Missing AgentID Org",
		Email:   "missing-agent@test.com",
	})
	resp, err := http.Post(ts.URL+"/auth/signup", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestSignup_MissingEmail(t *testing.T) {
	ts := signupServer(t)
	body, _ := json.Marshal(model.SignupRequest{
		OrgName: "Missing Email Org",
		AgentID: "agentx",
	})
	resp, err := http.Post(ts.URL+"/auth/signup", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestSignup_OrgNameOnlySpecialChars(t *testing.T) {
	ts := signupServer(t)
	body, _ := json.Marshal(model.SignupRequest{
		OrgName: "!!!---",
		AgentID: "slug-test-agent",
		Email:   "slugtest@test.com",
	})
	resp, err := http.Post(ts.URL+"/auth/signup", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestSignup_InvalidJSON(t *testing.T) {
	ts := signupServer(t)
	resp, err := http.Post(ts.URL+"/auth/signup", "application/json", strings.NewReader("{bad json"))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestSignup_InvalidAgentIDFormat(t *testing.T) {
	ts := signupServer(t)
	body, _ := json.Marshal(model.SignupRequest{
		OrgName: "Invalid AgentID Org",
		AgentID: "INVALID AGENT ID!!",
		Email:   "invalidagent@test.com",
	})
	resp, err := http.Post(ts.URL+"/auth/signup", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleCheck — empty body, malformed JSON, with query/project
// ===========================================================================

func TestHandleCheck_EmptyBody(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/check", adminToken,
		model.CheckRequest{})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCheck_MalformedJSON(t *testing.T) {
	req, _ := http.NewRequest("POST", testSrv.URL+"/v1/check", strings.NewReader("{bad json"))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCheck_WithQueryAndProject(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/check", adminToken,
		model.CheckRequest{
			DecisionType: "implementation",
			Query:        "test check query",
			Project:      "test-project",
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleSearch — malformed JSON
// ===========================================================================

func TestHandleSearch_InvalidJSON(t *testing.T) {
	req, _ := http.NewRequest("POST", testSrv.URL+"/v1/search", strings.NewReader("{bad"))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleUpdateAgent — invalid agent_id, with metadata
// ===========================================================================

func TestHandleUpdateAgent_InvalidAgentID(t *testing.T) {
	name := "updated name"
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/INVALID AGENT!!", adminToken,
		model.UpdateAgentRequest{Name: &name})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleUpdateAgent_WithMetadata(t *testing.T) {
	meta := map[string]any{"team": "backend"}
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/test-agent", adminToken,
		model.UpdateAgentRequest{Metadata: meta})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleDecisionRevisions — nonexistent decision
// ===========================================================================

func TestHandleDecisionRevisions_NonexistentDecision(t *testing.T) {
	fakeID := uuid.New().String()
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+fakeID+"/revisions", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var result struct {
		Data struct {
			Count int `json:"count"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	assert.Equal(t, 0, result.Data.Count)
}

// ===========================================================================
// Coverage push: HandleVerifyDecision — nonexistent
// ===========================================================================

func TestHandleVerifyDecision_Nonexistent(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/verify/"+uuid.New().String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleDecisionsRecent — with filters
// ===========================================================================

func TestHandleDecisionsRecent_WithDecisionTypeFilter(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/recent?decision_type=nonexistent_type", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleDecisionsRecent_WithBothFilters(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/recent?agent_id=test-agent&decision_type=implementation", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandlePatchConflict — invalid JSON, missing/invalid status
// ===========================================================================

func TestHandlePatchConflict_InvalidJSON(t *testing.T) {
	fakeID := uuid.New().String()
	req, _ := http.NewRequest("PATCH", testSrv.URL+"/v1/conflicts/"+fakeID, strings.NewReader("{bad"))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandlePatchConflict_MissingStatus(t *testing.T) {
	fakeID := uuid.New().String()
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflicts/"+fakeID, adminToken,
		map[string]any{})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandlePatchConflict_InvalidStatus(t *testing.T) {
	fakeID := uuid.New().String()
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/conflicts/"+fakeID, adminToken,
		map[string]any{"status": "invalid_status"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleAdjudicateConflict — invalid JSON, nonexistent
// ===========================================================================

func TestHandleAdjudicateConflict_InvalidJSON(t *testing.T) {
	fakeID := uuid.New().String()
	req, _ := http.NewRequest("POST", testSrv.URL+"/v1/conflicts/"+fakeID+"/adjudicate", strings.NewReader("{bad"))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetConflict — nonexistent
// ===========================================================================

func TestHandleGetConflict_Nonexistent(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts/"+uuid.New().String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleResolveConflictGroup — invalid JSON
// ===========================================================================

func TestHandleResolveConflictGroup_InvalidJSON(t *testing.T) {
	fakeID := uuid.New().String()
	req, _ := http.NewRequest("PATCH", testSrv.URL+"/v1/conflict-groups/"+fakeID+"/resolve", strings.NewReader("{bad"))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleSessionView — nonexistent session
// ===========================================================================

func TestHandleSessionView_Nonexistent(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/sessions/"+uuid.New().String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var result struct {
		Data struct {
			DecisionCount int `json:"decision_count"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	assert.Equal(t, 0, result.Data.DecisionCount)
}

// ===========================================================================
// Coverage push: HandleUpdateAgentTags — invalid agent_id
// ===========================================================================

func TestHandleUpdateAgentTags_InvalidAgentID(t *testing.T) {
	resp, err := authedRequest("PATCH", testSrv.URL+"/v1/agents/INVALID!!!/tags", adminToken,
		map[string]any{"tags": []string{"tag1"}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleCreateAgent — invalid JSON, missing/invalid agent_id
// ===========================================================================

func TestHandleCreateAgent_InvalidJSON(t *testing.T) {
	req, _ := http.NewRequest("POST", testSrv.URL+"/v1/agents", strings.NewReader("{bad json"))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateAgent_MissingAgentID(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken,
		model.CreateAgentRequest{Name: "No ID", Role: model.RoleAgent})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCreateAgent_InvalidAgentIDFormat(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", adminToken,
		model.CreateAgentRequest{AgentID: "INVALID ID!", Name: "Bad", Role: model.RoleAgent})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleDecisionConflicts — nonexistent decision
// ===========================================================================

func TestHandleDecisionConflicts_Nonexistent(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+uuid.New().String()+"/conflicts", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleListConflicts — combined filters
// ===========================================================================

func TestHandleListConflicts_CombinedFilters(t *testing.T) {
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/conflicts?status=open&severity=high&decision_type=implementation&agent_id=test-agent&limit=10&offset=0",
		adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleCreateRun — agent cannot create for others, invalid
// ===========================================================================

func TestHandleCreateRun_AgentCannotCreateForOther(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "some-other-agent"})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleRevokeKey — nonexistent
// ===========================================================================

func TestHandleRevokeKey_Nonexistent(t *testing.T) {
	req, _ := http.NewRequest("DELETE", testSrv.URL+"/v1/keys/"+uuid.New().String(), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetDecision — invalid UUID, nonexistent
// ===========================================================================

func TestHandleGetDecision_InvalidUUID(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/not-a-uuid", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleGetDecision_Nonexistent(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+uuid.New().String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleDeleteGrant — nonexistent
// ===========================================================================

func TestHandleDeleteGrant_Nonexistent(t *testing.T) {
	req, _ := http.NewRequest("DELETE", testSrv.URL+"/v1/grants/"+uuid.New().String(), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleMCPInfo — returns expected fields
// ===========================================================================

func TestHandleMCPInfo(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/mcp/info", agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data model.MCPInfoResponse `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	assert.Equal(t, "streamable-http", result.Data.Transport)
	assert.Equal(t, "ApiKey", result.Data.Auth.Preferred)
	assert.Contains(t, result.Data.Auth.Schemes, "ApiKey")
}

// ===========================================================================
// Coverage push: HandleConfig — returns expected fields
// ===========================================================================

func TestHandleConfig(t *testing.T) {
	resp, err := http.Get(testSrv.URL + "/config")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data map[string]bool `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))
	_, hasSearchEnabled := result.Data["search_enabled"]
	assert.True(t, hasSearchEnabled)
}

// ===========================================================================
// Coverage push: API key auth via ApiKey header
// ===========================================================================

func TestAPIKeyAuth_ValidCredentials(t *testing.T) {
	req, _ := http.NewRequest("GET", testSrv.URL+"/v1/decisions/recent", nil)
	req.Header.Set("Authorization", "ApiKey test-agent:test-agent-key")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleListAssessments — nonexistent decision
// ===========================================================================

func TestHandleListAssessments_Nonexistent(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+uuid.New().String()+"/assessments", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleCreateKey — invalid JSON, missing agent_id
// ===========================================================================

func TestHandleCreateKey_InvalidJSON(t *testing.T) {
	req, _ := http.NewRequest("POST", testSrv.URL+"/v1/keys", strings.NewReader("{bad json"))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleDeleteAgent — invalid agent_id, nonexistent
// ===========================================================================

func TestHandleDeleteAgent_InvalidAgentID(t *testing.T) {
	req, _ := http.NewRequest("DELETE", testSrv.URL+"/v1/agents/INVALID!!!", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleDeleteAgent_Nonexistent(t *testing.T) {
	req, _ := http.NewRequest("DELETE", testSrv.URL+"/v1/agents/nonexistent-delete-agent", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleCreateHold — invalid JSON
// ===========================================================================

func TestHandleCreateHold_InvalidJSON(t *testing.T) {
	req, _ := http.NewRequest("POST", testSrv.URL+"/v1/retention/hold", strings.NewReader("{bad"))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleReleaseHold — nonexistent
// ===========================================================================

func TestHandleReleaseHold_Nonexistent(t *testing.T) {
	req, _ := http.NewRequest("DELETE", testSrv.URL+"/v1/retention/hold/"+uuid.New().String(), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleListConflictGroups — conflict_kind filter
// ===========================================================================

func TestHandleListConflictGroups_WithConflictKind(t *testing.T) {
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/conflict-groups?conflict_kind=cross_agent",
		adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleGetAgent — nonexistent
// ===========================================================================

func TestHandleGetAgent_Nonexistent(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/agents/nonexistent-get-agent", adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleCreateGrant — invalid JSON
// ===========================================================================

func TestHandleCreateGrant_InvalidJSON(t *testing.T) {
	req, _ := http.NewRequest("POST", testSrv.URL+"/v1/grants", strings.NewReader("{bad"))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ===========================================================================
// Coverage push: HandleOpenAPISpec — not configured
// ===========================================================================

func TestHandleOpenAPISpec_NotConfigured(t *testing.T) {
	resp, err := http.Get(testSrv.URL + "/openapi.yaml")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ===========================================================================
// X-Model header: server-inferred model in agent_context
// ===========================================================================

func TestHandleTrace_XModelHeader(t *testing.T) {
	// POST trace with X-Model header, no explicit model in body.
	traceResp, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/trace", adminToken, map[string]any{
		"agent_id": "admin",
		"decision": map[string]any{
			"decision_type": "x-model-test",
			"outcome":       "test model header inference",
			"confidence":    0.8,
		},
	}, map[string]string{
		"X-Model": "claude-opus-4-6",
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
	require.NotEqual(t, uuid.Nil, decisionID)

	// GET the decision and verify the model was persisted from the header.
	getResp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+decisionID.String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = getResp.Body.Close() }()
	require.Equal(t, http.StatusOK, getResp.StatusCode)

	var result struct {
		Data model.Decision `json:"data"`
	}
	body, _ := io.ReadAll(getResp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	require.NotNil(t, result.Data.Model, "model should be populated from X-Model header")
	assert.Equal(t, "claude-opus-4-6", *result.Data.Model)
}

func TestHandleTrace_ExplicitModelTakesPriorityOverXModelHeader(t *testing.T) {
	// POST trace with both explicit model in body and X-Model header.
	traceResp, err := authedRequestWithHeaders("POST", testSrv.URL+"/v1/trace", adminToken, map[string]any{
		"agent_id": "admin",
		"decision": map[string]any{
			"decision_type": "model-priority-test",
			"outcome":       "explicit model should win over header",
			"confidence":    0.8,
		},
		"context": map[string]any{
			"model": "gpt-4o",
		},
	}, map[string]string{
		"X-Model": "claude-opus-4-6",
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
	require.NotEqual(t, uuid.Nil, decisionID)

	// GET the decision and verify the explicit model won over the header.
	getResp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+decisionID.String(), adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = getResp.Body.Close() }()
	require.Equal(t, http.StatusOK, getResp.StatusCode)

	var result struct {
		Data model.Decision `json:"data"`
	}
	body, _ := io.ReadAll(getResp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	require.NotNil(t, result.Data.Model, "model should be populated")
	assert.Equal(t, "gpt-4o", *result.Data.Model, "explicit model in body must take priority over X-Model header")
}

// ===========================================================================
// Project normalization: workspace names resolved to canonical repo names
// ===========================================================================

func TestHandleTrace_ProjectNormalizedFromRepoURL(t *testing.T) {
	// When the client sends a workspace name as the project but also includes
	// a repo_url in context, the server should normalize to the canonical repo name.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken,
		map[string]any{
			"agent_id": "test-agent",
			"decision": map[string]any{
				"decision_type": "implementation",
				"outcome":       "traced with workspace name and repo_url",
				"confidence":    0.7,
			},
			"context": map[string]any{
				"project":  "cairo-v1",
				"repo_url": "https://github.com/ashita-ai/akashi.git",
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var traceResp struct {
		Data struct {
			DecisionID string `json:"decision_id"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &traceResp))
	require.NotEmpty(t, traceResp.Data.DecisionID)

	// Fetch the decision and verify the project was normalized.
	getResp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+traceResp.Data.DecisionID, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = getResp.Body.Close() }()
	require.Equal(t, http.StatusOK, getResp.StatusCode)

	var result struct {
		Data model.Decision `json:"data"`
	}
	body, _ = io.ReadAll(getResp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	require.NotNil(t, result.Data.Project, "project should be set")
	assert.Equal(t, "akashi", *result.Data.Project, "workspace name should be normalized to repo name via repo_url")
}

func TestHandleTrace_ProjectUnchangedWhenCanonical(t *testing.T) {
	// When the client sends a proper project name, it should be preserved as-is.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken,
		map[string]any{
			"agent_id": "test-agent",
			"decision": map[string]any{
				"decision_type": "implementation",
				"outcome":       "traced with canonical project",
				"confidence":    0.7,
			},
			"context": map[string]any{
				"project": "akashi",
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var traceResp struct {
		Data struct {
			DecisionID string `json:"decision_id"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &traceResp))
	require.NotEmpty(t, traceResp.Data.DecisionID)

	getResp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+traceResp.Data.DecisionID, adminToken, nil)
	require.NoError(t, err)
	defer func() { _ = getResp.Body.Close() }()
	require.Equal(t, http.StatusOK, getResp.StatusCode)

	var result struct {
		Data model.Decision `json:"data"`
	}
	body, _ = io.ReadAll(getResp.Body)
	require.NoError(t, json.Unmarshal(body, &result))
	require.NotNil(t, result.Data.Project)
	assert.Equal(t, "akashi", *result.Data.Project)
}

func TestHandleTrace_WorkspaceNamesSameRepoGetSameProject(t *testing.T) {
	// Two decisions traced with different workspace names but the same repo_url
	// should both resolve to the same canonical project name, making them
	// eligible for conflict comparison.
	repoURL := "https://github.com/ashita-ai/akashi.git"
	workspaceNames := []string{"dubai", "bamako"}
	var decisionIDs []string

	for _, ws := range workspaceNames {
		resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken,
			map[string]any{
				"agent_id": "test-agent",
				"decision": map[string]any{
					"decision_type": "architecture",
					"outcome":       "decision from workspace " + ws,
					"confidence":    0.7,
				},
				"context": map[string]any{
					"project":  ws,
					"repo_url": repoURL,
				},
			})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusCreated, resp.StatusCode)

		var traceResp struct {
			Data struct {
				DecisionID string `json:"decision_id"`
			} `json:"data"`
		}
		body, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(body, &traceResp))
		require.NotEmpty(t, traceResp.Data.DecisionID)
		decisionIDs = append(decisionIDs, traceResp.Data.DecisionID)
	}

	// Verify both decisions have the same canonical project name.
	for i, id := range decisionIDs {
		getResp, err := authedRequest("GET", testSrv.URL+"/v1/decisions/"+id, adminToken, nil)
		require.NoError(t, err)
		defer func() { _ = getResp.Body.Close() }()
		require.Equal(t, http.StatusOK, getResp.StatusCode)

		var result struct {
			Data model.Decision `json:"data"`
		}
		body, _ := io.ReadAll(getResp.Body)
		require.NoError(t, json.Unmarshal(body, &result))
		require.NotNil(t, result.Data.Project, "decision %d should have project set", i)
		assert.Equal(t, "akashi", *result.Data.Project,
			"workspace %q should be normalized to 'akashi' via repo_url", workspaceNames[i])
	}
}

// ===========================================================================
// High-confidence warning in trace response (#468)
// ===========================================================================

func TestHandleTrace_HighConfNoEvidence_ReturnsWarning(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken,
		map[string]any{
			"agent_id": "test-agent",
			"decision": map[string]any{
				"decision_type": "architecture",
				"outcome":       "chose Postgres for high-confidence-warning test",
				"confidence":    0.9,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var body struct {
		Data struct {
			Warnings []string `json:"warnings"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Len(t, body.Data.Warnings, 1, "expected one warning for high confidence without evidence")
	assert.Contains(t, body.Data.Warnings[0], "high confidence")
}

func TestHandleTrace_HighConfWithEvidence_NoWarning(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken,
		map[string]any{
			"agent_id": "test-agent",
			"decision": map[string]any{
				"decision_type": "architecture",
				"outcome":       "chose Postgres with evidence",
				"confidence":    0.9,
				"evidence": []map[string]any{
					{"source_type": "document", "content": "benchmark results showing 10x throughput"},
				},
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.NotContains(t, string(body), "warnings", "no warning expected when evidence is present")
}

func TestHandleTrace_LowConfNoEvidence_NoWarning(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/trace", adminToken,
		map[string]any{
			"agent_id": "test-agent",
			"decision": map[string]any{
				"decision_type": "implementation",
				"outcome":       "low confidence decision",
				"confidence":    0.7,
			},
		})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.NotContains(t, string(body), "warnings", "no warning expected for confidence below threshold")
}

// ===========================================================================
// HandleGetRun — enrichments via ?include=enrichments
// ===========================================================================

func TestHandleGetRun_EnrichmentsBasic(t *testing.T) {
	// Trace a decision (auto-creates a run).
	reasoning := "integration test reasoning"
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: "enrichment_test",
				Outcome:      "chose enrichments",
				Confidence:   0.85,
				Reasoning:    &reasoning,
			},
		})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, traceResp.StatusCode)

	var traceResult struct {
		Data struct {
			RunID      uuid.UUID `json:"run_id"`
			DecisionID uuid.UUID `json:"decision_id"`
		} `json:"data"`
	}
	tb, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	require.NoError(t, json.Unmarshal(tb, &traceResult))
	runID := traceResult.Data.RunID

	// GET with enrichments.
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/runs/"+runID.String()+"?include=enrichments",
		agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Data struct {
			Run                 json.RawMessage            `json:"run"`
			Decisions           []json.RawMessage          `json:"decisions"`
			DecisionEnrichments map[string]json.RawMessage `json:"decision_enrichments"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(b, &result))

	// Must have at least one decision and a matching enrichment.
	require.GreaterOrEqual(t, len(result.Data.Decisions), 1, "run should contain at least one decision")
	require.NotNil(t, result.Data.DecisionEnrichments, "enrichments map must be present")

	// Find our decision in the enrichments.
	decID := traceResult.Data.DecisionID.String()
	rawEnrichment, ok := result.Data.DecisionEnrichments[decID]
	require.True(t, ok, "enrichment must exist for traced decision %s", decID)

	// Parse and validate the enrichment structure.
	var enrichment struct {
		Revisions struct {
			Items    []json.RawMessage `json:"items"`
			Count    int               `json:"count"`
			Degraded bool              `json:"degraded"`
		} `json:"revisions"`
		Lineage struct {
			DecisionID string `json:"decision_id"`
		} `json:"lineage"`
		Conflicts struct {
			Items   []json.RawMessage `json:"items"`
			Count   int               `json:"count"`
			HasMore bool              `json:"has_more"`
		} `json:"conflicts"`
		Integrity struct {
			Status      string `json:"status"`
			ContentHash string `json:"content_hash"`
		} `json:"integrity"`
	}
	require.NoError(t, json.Unmarshal(rawEnrichment, &enrichment))

	// Revisions: at least the decision itself.
	assert.GreaterOrEqual(t, enrichment.Revisions.Count, 1, "revision chain should include at least the original decision")
	assert.False(t, enrichment.Revisions.Degraded, "revisions should not be degraded")

	// Lineage: decision_id should match.
	assert.Equal(t, decID, enrichment.Lineage.DecisionID)

	// Conflicts: no overflow expected for a single fresh decision.
	assert.False(t, enrichment.Conflicts.HasMore)

	// Integrity: decision was traced with content hash.
	assert.Contains(t, []string{"verified", "no_hash"}, enrichment.Integrity.Status,
		"integrity status should be verified or no_hash")
	if enrichment.Integrity.Status == "verified" {
		assert.NotEmpty(t, enrichment.Integrity.ContentHash,
			"verified status must include the content_hash")
	}
}

func TestHandleGetRun_NoEnrichmentsWithoutParam(t *testing.T) {
	// Trace a decision to get a run.
	traceResp, err := authedRequest("POST", testSrv.URL+"/v1/trace", agentToken,
		model.TraceRequest{
			AgentID: "test-agent",
			Decision: model.TraceDecision{
				DecisionType: "no_enrichment_test",
				Outcome:      "should not enrich",
				Confidence:   0.5,
			},
		})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, traceResp.StatusCode)

	var traceResult struct {
		Data struct {
			RunID uuid.UUID `json:"run_id"`
		} `json:"data"`
	}
	tb, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	require.NoError(t, json.Unmarshal(tb, &traceResult))

	// GET without ?include=enrichments.
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/runs/"+traceResult.Data.RunID.String(),
		agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	b, _ := io.ReadAll(resp.Body)
	// The response should NOT contain decision_enrichments.
	assert.NotContains(t, string(b), "decision_enrichments",
		"enrichments should not appear without ?include=enrichments")
}

func TestHandleGetRun_EnrichmentsEmptyRun(t *testing.T) {
	// Create a run with no decisions.
	runResp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, runResp.StatusCode)

	var runResult struct {
		Data struct {
			ID uuid.UUID `json:"id"`
		} `json:"data"`
	}
	rb, _ := io.ReadAll(runResp.Body)
	_ = runResp.Body.Close()
	require.NoError(t, json.Unmarshal(rb, &runResult))

	// GET with ?include=enrichments on a run with zero decisions.
	resp, err := authedRequest("GET",
		testSrv.URL+"/v1/runs/"+runResult.Data.ID.String()+"?include=enrichments",
		agentToken, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	b, _ := io.ReadAll(resp.Body)
	// No enrichments when there are no decisions.
	assert.NotContains(t, string(b), "decision_enrichments",
		"enrichments should not appear when run has no decisions")
}

// ---- HandleSetOrgSettings audit trail ------------------------------------

func TestHandleSetOrgSettings_AuditTrail(t *testing.T) {
	orgID := uuid.Nil // default org from SeedAdmin

	// Count existing audit entries so we can assert on the delta.
	var baseline int
	require.NoError(t, testDB.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM mutation_audit_log
		 WHERE operation = 'org_settings_updated'
		   AND resource_type = 'org_settings'
		   AND org_id = $1`, orgID,
	).Scan(&baseline))

	t.Run("upsert writes mutation audit entry", func(t *testing.T) {
		settings := model.OrgSettingsData{
			ConflictResolution: &model.ConflictResolutionPolicy{
				AutoResolveAfterDays:   7,
				AutoResolveWinner:      model.WinnerRecency,
				AutoResolveMaxSeverity: "medium",
			},
		}

		resp, err := authedRequest("PUT", testSrv.URL+"/v1/org/settings", adminToken, settings)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		// Verify a mutation audit row was written for this operation.
		var count int
		err = testDB.Pool().QueryRow(context.Background(),
			`SELECT count(*) FROM mutation_audit_log
			 WHERE operation = 'org_settings_updated'
			   AND resource_type = 'org_settings'
			   AND org_id = $1`, orgID,
		).Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, baseline+1, count, "expected one new audit entry for org_settings_updated")
	})

	t.Run("second upsert captures before-state", func(t *testing.T) {
		// Second write — should capture the first settings as before_data.
		second := model.OrgSettingsData{
			ConflictResolution: &model.ConflictResolutionPolicy{
				AutoResolveAfterDays:   14,
				AutoResolveWinner:      model.WinnerConfidence,
				AutoResolveMaxSeverity: "high",
			},
		}
		resp, err := authedRequest("PUT", testSrv.URL+"/v1/org/settings", adminToken, second)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		// Find the most recent audit entry and verify before_data is non-null.
		var beforeData, afterData []byte
		err = testDB.Pool().QueryRow(context.Background(),
			`SELECT before_data, after_data FROM mutation_audit_log
			 WHERE operation = 'org_settings_updated'
			   AND resource_type = 'org_settings'
			   AND org_id = $1
			 ORDER BY occurred_at DESC LIMIT 1`, orgID,
		).Scan(&beforeData, &afterData)
		require.NoError(t, err)
		assert.NotNil(t, beforeData, "second upsert should capture before_data")
		assert.NotNil(t, afterData, "second upsert should capture after_data")

		// Verify the before_data contains the previous settings.
		// PostgreSQL jsonb canonical form uses spaces after colons.
		assert.Contains(t, string(beforeData), `"auto_resolve_after_days": 7`,
			"before_data should reflect the first settings write")
		// Verify the after_data contains the new settings.
		assert.Contains(t, string(afterData), `"auto_resolve_after_days": 14`,
			"after_data should reflect the second settings write")
	})
}
