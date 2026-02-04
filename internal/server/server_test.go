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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ashita-ai/kyoyu/internal/auth"
	"github.com/ashita-ai/kyoyu/internal/model"
	"github.com/ashita-ai/kyoyu/internal/server"
	"github.com/ashita-ai/kyoyu/internal/service/embedding"
	"github.com/ashita-ai/kyoyu/internal/service/trace"
	"github.com/ashita-ai/kyoyu/internal/storage"
)

var (
	testSrv    *httptest.Server
	adminToken string
	agentToken string
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "timescale/timescaledb:latest-pg17",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "kyoyu",
			"POSTGRES_PASSWORD": "kyoyu",
			"POSTGRES_DB":       "kyoyu",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(60 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start container: %v\n", err)
		os.Exit(1)
	}

	host, _ := container.Host(ctx)
	port, _ := container.MappedPort(ctx, "5432")
	dsn := fmt.Sprintf("postgres://kyoyu:kyoyu@%s:%s/kyoyu?sslmode=disable", host, port.Port())

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	db, err := storage.New(ctx, dsn, "", logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create DB: %v\n", err)
		os.Exit(1)
	}

	// Enable extensions and run migrations.
	pool := db.Pool()
	_, _ = pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector")
	_, _ = pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS timescaledb")

	if err := db.RunMigrations(ctx, os.DirFS("../../migrations")); err != nil {
		fmt.Fprintf(os.Stderr, "failed to run migrations: %v\n", err)
		os.Exit(1)
	}

	jwtMgr, _ := auth.NewJWTManager("", "", 24*time.Hour)
	embedder := embedding.NewNoopProvider(1536)
	buf := trace.NewBuffer(db, logger, 1000, 50*time.Millisecond)
	buf.Start(ctx)

	srv := server.New(db, jwtMgr, embedder, buf, logger, 0, 30*time.Second, 30*time.Second)

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
	buf.Drain(ctx)
	db.Close(ctx)
	_ = container.Terminate(ctx)
	os.Exit(code)
}

func getToken(baseURL, agentID, apiKey string) string {
	body, _ := json.Marshal(model.AuthTokenRequest{AgentID: agentID, APIKey: apiKey})
	resp, err := http.Post(baseURL+"/auth/token", "application/json", bytes.NewReader(body))
	if err != nil {
		panic(fmt.Sprintf("getToken: request failed: %v", err))
	}
	defer resp.Body.Close()
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
	resp.Body.Close()
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

func TestHealthEndpoint(t *testing.T) {
	resp, err := http.Get(testSrv.URL + "/health")
	require.NoError(t, err)
	defer resp.Body.Close()
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
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestUnauthenticatedAccess(t *testing.T) {
	resp, err := http.Get(testSrv.URL + "/v1/conflicts")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestCreateRunAndAppendEvents(t *testing.T) {
	// Create run.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/runs", agentToken,
		model.CreateRunRequest{AgentID: "test-agent"})
	require.NoError(t, err)
	defer resp.Body.Close()
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
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusCreated, resp2.StatusCode)

	// Wait for buffer flush.
	time.Sleep(200 * time.Millisecond)

	// Get run with events.
	resp3, err := authedRequest("GET", testSrv.URL+"/v1/runs/"+runID.String(), agentToken, nil)
	require.NoError(t, err)
	defer resp3.Body.Close()
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
	defer resp.Body.Close()
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
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestSearchEndpoint(t *testing.T) {
	resp, err := authedRequest("POST", testSrv.URL+"/v1/search", agentToken,
		model.SearchRequest{
			Query: "test decisions",
			Limit: 5,
		})
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAdminOnlyEndpoints(t *testing.T) {
	// Agent cannot create agents.
	resp, err := authedRequest("POST", testSrv.URL+"/v1/agents", agentToken,
		model.CreateAgentRequest{AgentID: "should-fail", Name: "Fail", APIKey: "key"})
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// Admin can list agents.
	resp2, err := authedRequest("GET", testSrv.URL+"/v1/agents", adminToken, nil)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
}

func TestConflictsEndpoint(t *testing.T) {
	resp, err := authedRequest("GET", testSrv.URL+"/v1/conflicts", adminToken, nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
