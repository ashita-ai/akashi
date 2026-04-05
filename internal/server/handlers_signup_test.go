//go:build integration

package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/ratelimit"
	"github.com/ashita-ai/akashi/internal/server"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/service/trace"
)

// signupServer returns a test server with signup enabled. It reuses the shared
// testDB from TestMain but constructs its own server instance so the signup
// route is registered.
func signupServer(t *testing.T) *httptest.Server {
	t.Helper()
	jwtMgr, err := auth.NewJWTManager("", "", 24*time.Hour)
	require.NoError(t, err)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	embedder := embedding.NewNoopProvider(1024)
	decisionSvc := decisions.New(testDB, embedder, nil, logger, nil)
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
		SignupEnabled:       true,
		SignupRateLimiter:   ratelimit.NewMemoryLimiter(1000, 1000), // permissive for tests
	})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestSignup(t *testing.T) {
	ts := signupServer(t)

	t.Run("happy path", func(t *testing.T) {
		body, _ := json.Marshal(model.SignupRequest{
			OrgName: "Acme AI",
			AgentID: "alice",
			Email:   "alice@acme.com",
		})
		resp, err := http.Post(ts.URL+"/auth/signup", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusCreated, resp.StatusCode)

		data, _ := io.ReadAll(resp.Body)
		var result struct {
			Data model.SignupResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(data, &result))

		assert.NotEmpty(t, result.Data.OrgID)
		assert.Equal(t, "acme-ai", result.Data.OrgSlug)
		assert.Equal(t, "alice", result.Data.AgentID)
		assert.True(t, len(result.Data.APIKey) > 10, "api_key should be a managed key")
		assert.Contains(t, result.Data.APIKey, "ak_")
		assert.Contains(t, result.Data.MCPConfig.URL, "/mcp")
		assert.Contains(t, result.Data.MCPConfig.Header, "ApiKey alice:")
	})

	t.Run("duplicate org slug returns 409", func(t *testing.T) {
		body, _ := json.Marshal(model.SignupRequest{
			OrgName: "Acme AI", // same slug as happy path
			AgentID: "bob",
			Email:   "bob@acme.com",
		})
		resp, err := http.Post(ts.URL+"/auth/signup", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusConflict, resp.StatusCode)
	})

	t.Run("duplicate email returns 409", func(t *testing.T) {
		body, _ := json.Marshal(model.SignupRequest{
			OrgName: "Different Org",
			AgentID: "charlie",
			Email:   "alice@acme.com", // same email as happy path
		})
		resp, err := http.Post(ts.URL+"/auth/signup", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusConflict, resp.StatusCode)
	})

	t.Run("missing org_name returns 400", func(t *testing.T) {
		body, _ := json.Marshal(model.SignupRequest{
			AgentID: "dave",
			Email:   "dave@test.com",
		})
		resp, err := http.Post(ts.URL+"/auth/signup", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("reserved agent_id returns 400", func(t *testing.T) {
		body, _ := json.Marshal(model.SignupRequest{
			OrgName: "Reserved Org",
			AgentID: "admin",
			Email:   "admin@test.com",
		})
		resp, err := http.Post(ts.URL+"/auth/signup", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("invalid email returns 400", func(t *testing.T) {
		body, _ := json.Marshal(model.SignupRequest{
			OrgName: "Bad Email Org",
			AgentID: "eve",
			Email:   "not-an-email",
		})
		resp, err := http.Post(ts.URL+"/auth/signup", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("issued key can authenticate", func(t *testing.T) {
		body, _ := json.Marshal(model.SignupRequest{
			OrgName: "Auth Test Org",
			AgentID: "frank",
			Email:   "frank@test.com",
		})
		resp, err := http.Post(ts.URL+"/auth/signup", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusCreated, resp.StatusCode)

		data, _ := io.ReadAll(resp.Body)
		var result struct {
			Data model.SignupResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(data, &result))

		// Use the returned key to get a token.
		tokenBody, _ := json.Marshal(model.AuthTokenRequest{
			AgentID: "frank",
			APIKey:  result.Data.APIKey,
		})
		tokenResp, err := http.Post(ts.URL+"/auth/token", "application/json", bytes.NewReader(tokenBody))
		require.NoError(t, err)
		defer func() { _ = tokenResp.Body.Close() }()

		assert.Equal(t, http.StatusOK, tokenResp.StatusCode)
	})
}

func TestSignupDisabledByDefault(t *testing.T) {
	// The shared testSrv does NOT have SignupEnabled, so /auth/signup should 404.
	body, _ := json.Marshal(model.SignupRequest{
		OrgName: "Should Not Work",
		AgentID: "ghost",
		Email:   "ghost@test.com",
	})
	resp, err := http.Post(testSrv.URL+"/auth/signup", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	// Go's default mux returns 405 Method Not Allowed when path exists for other methods,
	// or 404 when no route matches. Since POST /auth/signup isn't registered, expect 404.
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
