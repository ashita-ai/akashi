package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/billing"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/search"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/trace"
	"github.com/ashita-ai/akashi/internal/signup"
	"github.com/ashita-ai/akashi/internal/storage"
)

// Handlers holds HTTP handler dependencies.
type Handlers struct {
	db                  *storage.DB
	jwtMgr              *auth.JWTManager
	decisionSvc         *decisions.Service
	billingSvc          *billing.Service
	buffer              *trace.Buffer
	broker              *Broker
	signupSvc           *signup.Service
	searcher            search.Searcher
	logger              *slog.Logger
	startedAt           time.Time
	version             string
	maxRequestBodyBytes int64
	openapiSpec         []byte
}

// HandlersDeps holds all dependencies for constructing Handlers.
// Optional (nil-safe): BillingSvc, Broker, SignupSvc, Searcher, OpenAPISpec.
type HandlersDeps struct {
	DB                  *storage.DB
	JWTMgr              *auth.JWTManager
	DecisionSvc         *decisions.Service
	BillingSvc          *billing.Service
	Buffer              *trace.Buffer
	Broker              *Broker
	SignupSvc           *signup.Service
	Searcher            search.Searcher
	Logger              *slog.Logger
	Version             string
	MaxRequestBodyBytes int64
	OpenAPISpec         []byte
}

// NewHandlers creates a new Handlers with all dependencies.
func NewHandlers(d HandlersDeps) *Handlers {
	return &Handlers{
		db:                  d.DB,
		jwtMgr:              d.JWTMgr,
		decisionSvc:         d.DecisionSvc,
		billingSvc:          d.BillingSvc,
		buffer:              d.Buffer,
		broker:              d.Broker,
		signupSvc:           d.SignupSvc,
		searcher:            d.Searcher,
		logger:              d.Logger,
		startedAt:           time.Now(),
		version:             d.Version,
		maxRequestBodyBytes: d.MaxRequestBodyBytes,
		openapiSpec:         d.OpenAPISpec,
	}
}

// HandleAuthToken handles POST /auth/token.
func (h *Handlers) HandleAuthToken(w http.ResponseWriter, r *http.Request) {
	var req model.AuthTokenRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	agents, err := h.db.GetAgentsByAgentIDGlobal(r.Context(), req.AgentID)
	if err != nil {
		// Perform a dummy Argon2 hash to make the response time indistinguishable
		// from a valid agent_id with wrong credentials.
		auth.DummyVerify()
		writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "invalid credentials")
		return
	}

	// Iterate over all matching agents (agent_ids can collide across orgs)
	// and verify credentials against each one. Use the first match.
	var matched *model.Agent
	verified := false
	for i := range agents {
		a := &agents[i]
		if a.APIKeyHash == nil {
			continue
		}
		valid, verr := auth.VerifyAPIKey(req.APIKey, *a.APIKeyHash)
		verified = true
		if verr != nil || !valid {
			continue
		}
		matched = a
		break
	}
	// If no agent had a hash, do a dummy verify to prevent timing side-channel.
	if !verified {
		auth.DummyVerify()
	}
	if matched == nil {
		writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "invalid credentials")
		return
	}

	// Reject unverified orgs (skip for the default org used by pre-migration seed admin).
	if matched.OrgID != uuid.Nil {
		org, err := h.db.GetOrganization(r.Context(), matched.OrgID)
		if err != nil {
			h.writeInternalError(w, r, "failed to look up organization", err)
			return
		}
		if !org.EmailVerified {
			writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "email not verified — check your inbox or request a new verification link")
			return
		}
	}

	token, expiresAt, err := h.jwtMgr.IssueToken(*matched)
	if err != nil {
		h.writeInternalError(w, r, "failed to issue token", err)
		return
	}

	writeJSON(w, r, http.StatusOK, model.AuthTokenResponse{
		Token:     token,
		ExpiresAt: expiresAt,
	})
}

// HandleSubscribe handles GET /v1/subscribe (SSE).
func (h *Handlers) HandleSubscribe(w http.ResponseWriter, r *http.Request) {
	if h.broker == nil {
		writeError(w, r, http.StatusServiceUnavailable, model.ErrCodeInternalError,
			"SSE not available (LISTEN/NOTIFY not configured)")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	orgID := OrgIDFromContext(r.Context())
	ch := h.broker.Subscribe(orgID)
	defer h.broker.Unsubscribe(ch)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(event); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// HandleHealth handles GET /health.
func (h *Handlers) HandleHealth(w http.ResponseWriter, r *http.Request) {
	pgStatus := "connected"
	status := "healthy"
	httpStatus := http.StatusOK

	if err := h.db.Ping(r.Context()); err != nil {
		pgStatus = "disconnected"
		status = "unhealthy"
		httpStatus = http.StatusServiceUnavailable
	}

	// Buffer health: >50% capacity = high, >75% capacity = critical.
	bufDepth := 0
	bufStatus := "ok"
	if h.buffer != nil {
		bufDepth = h.buffer.Len()
		cap := h.buffer.Capacity()
		if bufDepth > cap*3/4 {
			bufStatus = "critical"
			if status == "healthy" {
				status = "degraded"
			}
		} else if bufDepth > cap/2 {
			bufStatus = "high"
		}
	}

	resp := model.HealthResponse{
		Status:       status,
		Version:      h.version,
		Postgres:     pgStatus,
		BufferDepth:  bufDepth,
		BufferStatus: bufStatus,
		Uptime:       int64(time.Since(h.startedAt).Seconds()),
	}

	if h.searcher != nil {
		if err := h.searcher.Healthy(r.Context()); err == nil {
			resp.Qdrant = "connected"
		} else {
			resp.Qdrant = "disconnected"
		}
	}

	if h.broker != nil {
		resp.SSEBroker = "running"
	}

	writeJSON(w, r, httpStatus, resp)
}

// HandleOpenAPISpec serves the embedded OpenAPI specification.
func (h *Handlers) HandleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	if len(h.openapiSpec) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(h.openapiSpec)
}

// SeedAdmin creates the initial admin agent if the agents table is empty.
func (h *Handlers) SeedAdmin(ctx context.Context, adminAPIKey string) error {
	if adminAPIKey == "" {
		h.logger.Info("no admin API key configured, skipping admin seed")
		return nil
	}

	// Default org UUID for the pre-migration seed admin.
	defaultOrgID := uuid.Nil

	// Ensure the default org exists so the agents FK is satisfied on fresh DBs.
	if err := h.db.EnsureDefaultOrg(ctx); err != nil {
		return fmt.Errorf("seed admin: ensure default org: %w", err)
	}

	count, err := h.db.CountAgents(ctx, defaultOrgID)
	if err != nil {
		return fmt.Errorf("seed admin: count agents: %w", err)
	}
	if count > 0 {
		h.logger.Info("agents table not empty, skipping admin seed")
		return nil
	}

	hash, err := auth.HashAPIKey(adminAPIKey)
	if err != nil {
		return fmt.Errorf("seed admin: hash key: %w", err)
	}

	_, err = h.db.CreateAgent(ctx, model.Agent{
		AgentID:    "admin",
		OrgID:      defaultOrgID,
		Name:       "System Admin",
		Role:       model.RoleAdmin,
		APIKeyHash: &hash,
	})
	if err != nil {
		return fmt.Errorf("seed admin: create agent: %w", err)
	}

	h.logger.Info("seeded initial admin agent")
	return nil
}

// HandleConfig returns feature flags for the current deployment so the UI
// can hide billing/search when they are not configured. No auth required.
func (h *Handlers) HandleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]bool{
		"billing_enabled": h.billingSvc != nil && h.billingSvc.Enabled(),
		"search_enabled":  h.searcher != nil,
	})
}

// HandleSignup handles POST /auth/signup.
func (h *Handlers) HandleSignup(w http.ResponseWriter, r *http.Request) {
	if h.signupSvc == nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "signup is not enabled")
		return
	}

	var req model.SignupRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	result, err := h.signupSvc.Signup(r.Context(), signup.SignupInput{
		Email:    req.Email,
		Password: req.Password,
		OrgName:  req.OrgName,
	})
	if err != nil {
		switch {
		case errors.Is(err, signup.ErrInvalidEmail),
			errors.Is(err, signup.ErrWeakPassword),
			errors.Is(err, signup.ErrOrgNameRequired):
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		default:
			h.logger.Error("signup failed", "error", err)
			writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "signup failed")
		}
		return
	}

	writeJSON(w, r, http.StatusCreated, result)
}

// HandleVerifyEmail handles POST /auth/verify.
// Accepts {"token": "..."} in the request body (not as a query parameter)
// to avoid token exposure in server/proxy logs and browser history.
func (h *Handlers) HandleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	if h.signupSvc == nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "signup is not enabled")
		return
	}

	var req struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}
	if req.Token == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "token is required")
		return
	}

	if err := h.signupSvc.Verify(r.Context(), req.Token); err != nil {
		h.logger.Warn("email verification failed", "error", err)
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid or expired verification token")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]string{
		"status":  "verified",
		"message": "email verified successfully — you can now authenticate",
	})
}

// --- Shared helpers ---

func parseRunID(r *http.Request) (uuid.UUID, error) {
	runIDStr := r.PathValue("run_id")
	if runIDStr == "" {
		return uuid.Nil, fmt.Errorf("run_id is required")
	}
	id, err := uuid.Parse(runIDStr)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid run_id: %s", runIDStr)
	}
	return id, nil
}

// maxQueryLimit is the maximum allowed value for limit query parameters.
const maxQueryLimit = 1000

func queryInt(r *http.Request, key string, defaultVal int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}

// maxQueryOffset prevents absurdly large offset values that cause expensive sequential scans.
const maxQueryOffset = 100_000

// queryOffset returns a bounded, non-negative offset from query params.
func queryOffset(r *http.Request) int {
	offset := queryInt(r, "offset", 0)
	if offset < 0 {
		return 0
	}
	if offset > maxQueryOffset {
		return maxQueryOffset
	}
	return offset
}

// queryLimit returns a bounded limit value from query params.
// Values are clamped to [1, maxQueryLimit].
func queryLimit(r *http.Request, defaultVal int) int {
	limit := queryInt(r, "limit", defaultVal)
	if limit < 1 {
		return 1
	}
	if limit > maxQueryLimit {
		return maxQueryLimit
	}
	return limit
}

func queryTime(r *http.Request, key string) (*time.Time, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: expected RFC3339 format (e.g. 2024-01-01T00:00:00Z)", key)
	}
	return &t, nil
}
