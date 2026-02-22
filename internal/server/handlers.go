package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/authz"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/search"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/trace"
	"github.com/ashita-ai/akashi/internal/storage"
)

// Handlers holds HTTP handler dependencies.
type Handlers struct {
	db                      *storage.DB
	jwtMgr                  *auth.JWTManager
	decisionSvc             *decisions.Service
	buffer                  *trace.Buffer
	broker                  *Broker
	searcher                search.Searcher
	grantCache              *authz.GrantCache
	logger                  *slog.Logger
	startedAt               time.Time
	version                 string
	maxRequestBodyBytes     int64
	openapiSpec             []byte
	enableDestructiveDelete bool
	// decisionHooks are fired asynchronously after decision lifecycle events.
	// Nil or empty slice means no hooks registered.
	decisionHooks []DecisionHook
}

// HandlersDeps holds all dependencies for constructing Handlers.
// Optional (nil-safe): Broker, Searcher, GrantCache, OpenAPISpec, DecisionHooks.
type HandlersDeps struct {
	DB                      *storage.DB
	JWTMgr                  *auth.JWTManager
	DecisionSvc             *decisions.Service
	Buffer                  *trace.Buffer
	Broker                  *Broker
	Searcher                search.Searcher
	GrantCache              *authz.GrantCache
	Logger                  *slog.Logger
	Version                 string
	MaxRequestBodyBytes     int64
	OpenAPISpec             []byte
	EnableDestructiveDelete bool
	DecisionHooks           []DecisionHook
}

// NewHandlers creates a new Handlers with all dependencies.
func NewHandlers(d HandlersDeps) *Handlers {
	return &Handlers{
		db:                      d.DB,
		jwtMgr:                  d.JWTMgr,
		decisionSvc:             d.DecisionSvc,
		buffer:                  d.Buffer,
		broker:                  d.Broker,
		searcher:                d.Searcher,
		grantCache:              d.GrantCache,
		logger:                  d.Logger,
		startedAt:               time.Now(),
		version:                 d.Version,
		maxRequestBodyBytes:     d.MaxRequestBodyBytes,
		openapiSpec:             d.OpenAPISpec,
		enableDestructiveDelete: d.EnableDestructiveDelete,
		decisionHooks:           d.DecisionHooks,
	}
}

// HandleAuthToken handles POST /auth/token.
// Checks managed api_keys table first, falls back to agents.api_key_hash.
func (h *Handlers) HandleAuthToken(w http.ResponseWriter, r *http.Request) {
	var req model.AuthTokenRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
		return
	}

	// Phase 1: check managed api_keys table.
	var matched *model.Agent
	var matchedKeyID *uuid.UUID
	managedKeys, _ := h.db.GetActiveAPIKeysByAgentIDGlobal(r.Context(), req.AgentID)
	for _, k := range managedKeys {
		valid, verr := auth.VerifyAPIKey(req.APIKey, k.KeyHash)
		if verr != nil || !valid {
			continue
		}
		agent, err := h.db.GetAgentByAgentID(r.Context(), k.OrgID, k.AgentID)
		if err != nil {
			continue
		}
		matched = &agent
		kid := k.ID
		matchedKeyID = &kid
		break
	}

	// Phase 2: fall back to legacy agents.api_key_hash.
	if matched == nil {
		agents, err := h.db.GetAgentsByAgentIDGlobal(r.Context(), req.AgentID)
		if err != nil {
			if len(managedKeys) == 0 {
				auth.DummyVerify()
			}
			writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "invalid credentials")
			return
		}

		verified := len(managedKeys) > 0
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
		if !verified {
			auth.DummyVerify()
		}
	}

	if matched == nil {
		writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "invalid credentials")
		return
	}

	token, expiresAt, err := h.jwtMgr.IssueToken(*matched)
	if err != nil {
		h.writeInternalError(w, r, "failed to issue token", err)
		return
	}

	// Audit: record successful token issuance. Best-effort — failure to
	// audit must not block the token response.
	auditMeta := map[string]any{
		"ip":         r.RemoteAddr,
		"user_agent": r.UserAgent(),
		"token_exp":  expiresAt,
	}
	if matchedKeyID != nil {
		auditMeta["api_key_id"] = matchedKeyID.String()
	}
	if auditErr := h.recordMutationAuditBestEffort(r, matched.OrgID,
		"token_issued", "auth_token", matched.AgentID, nil, nil, auditMeta,
	); auditErr != nil {
		slog.Error("failed to audit token issuance",
			"agent_id", matched.AgentID, "org_id", matched.OrgID, "error", auditErr)
	}

	writeJSON(w, r, http.StatusOK, model.AuthTokenResponse{
		Token:     token,
		ExpiresAt: expiresAt,
	})
}

// HandleScopedToken handles POST /auth/scoped-token (admin-only).
// Issues a short-lived JWT that acts as the target agent, with the issuing
// admin's agent_id recorded in the ScopedBy claim. Useful for testing access
// control and debugging what a specific agent can see without needing its key.
func (h *Handlers) HandleScopedToken(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	// Scoped tokens cannot issue further scoped tokens — no delegation chains.
	if claims.ScopedBy != "" {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden,
			"scoped tokens cannot issue further scoped tokens")
		return
	}

	var req model.ScopedTokenRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
		return
	}
	if req.AsAgentID == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "as_agent_id is required")
		return
	}
	if err := model.ValidateAgentID(req.AsAgentID); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

	ttl := 5 * time.Minute
	if req.ExpiresIn > 0 {
		ttl = time.Duration(req.ExpiresIn) * time.Second
	}
	// Cap is enforced inside IssueScopedToken, but clamp the value used for
	// the audit log so it reflects what was actually issued.
	if ttl > auth.MaxScopedTokenTTL {
		ttl = auth.MaxScopedTokenTTL
	}

	target, err := h.db.GetAgentByAgentID(r.Context(), orgID, req.AsAgentID)
	if err != nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound,
			"agent not found: "+req.AsAgentID)
		return
	}

	// Privilege escalation guard: callers can only impersonate agents whose role
	// is strictly below their own. Without this check an admin-role caller could
	// issue a scoped token for org_owner or platform_admin.
	if model.RoleRank(claims.Role) <= model.RoleRank(target.Role) {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden,
			"cannot issue scoped token for agent with role equal to or higher than your own")
		return
	}

	token, expiresAt, err := h.jwtMgr.IssueScopedToken(claims.AgentID, target, ttl)
	if err != nil {
		h.writeInternalError(w, r, "failed to issue scoped token", err)
		return
	}

	slog.Info("scoped token issued",
		"issuer", claims.AgentID,
		"as_agent_id", target.AgentID,
		"as_role", target.Role,
		"ttl_seconds", int(ttl.Seconds()),
		"request_id", RequestIDFromContext(r.Context()),
	)

	if auditErr := h.recordMutationAuditBestEffort(r, orgID,
		"scoped_token_issued", "auth_token", target.AgentID, nil, nil,
		map[string]any{
			"issuer":      claims.AgentID,
			"as_role":     string(target.Role),
			"ttl_seconds": int(ttl.Seconds()),
			"token_exp":   expiresAt,
		},
	); auditErr != nil {
		slog.Error("failed to audit scoped token issuance",
			"issuer", claims.AgentID, "as_agent_id", target.AgentID, "error", auditErr)
	}

	writeJSON(w, r, http.StatusOK, model.ScopedTokenResponse{
		Token:     token,
		ExpiresAt: expiresAt,
		AsAgentID: target.AgentID,
		ScopedBy:  claims.AgentID,
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

	// Disable the server's WriteTimeout for this long-lived connection.
	// Without this, idle SSE connections are killed after WriteTimeout (default 30s).
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	orgID := OrgIDFromContext(r.Context())
	ch := h.broker.Subscribe(orgID)
	defer h.broker.Unsubscribe(ch)

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if _, err := w.Write([]byte(":keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
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
		totalAgents, err := h.db.CountAgentsGlobal(ctx)
		if err != nil {
			return fmt.Errorf("seed admin: count global agents: %w", err)
		}
		if totalAgents == 0 {
			return fmt.Errorf("seed admin: AKASHI_ADMIN_API_KEY is empty and no agents exist; set AKASHI_ADMIN_API_KEY to bootstrap initial admin access")
		}
		h.logger.Info("no admin API key configured, skipping admin seed", "existing_agents", totalAgents)
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
// can adapt to optional capabilities. No auth required.
// search_enabled is true only when semantic search works (Qdrant + real embedder).
func (h *Handlers) HandleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]bool{
		"search_enabled": h.decisionSvc.SemanticSearchAvailable(),
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
