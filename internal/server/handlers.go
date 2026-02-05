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
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/trace"
	"github.com/ashita-ai/akashi/internal/storage"
)

// Handlers holds HTTP handler dependencies.
type Handlers struct {
	db                  *storage.DB
	jwtMgr              *auth.JWTManager
	decisionSvc         *decisions.Service
	buffer              *trace.Buffer
	logger              *slog.Logger
	startedAt           time.Time
	version             string
	maxRequestBodyBytes int64
}

// NewHandlers creates a new Handlers with all dependencies.
func NewHandlers(db *storage.DB, jwtMgr *auth.JWTManager, decisionSvc *decisions.Service, buffer *trace.Buffer, logger *slog.Logger, version string, maxRequestBodyBytes int64) *Handlers {
	return &Handlers{
		db:                  db,
		jwtMgr:              jwtMgr,
		decisionSvc:         decisionSvc,
		buffer:              buffer,
		logger:              logger,
		startedAt:           time.Now(),
		version:             version,
		maxRequestBodyBytes: maxRequestBodyBytes,
	}
}

// HandleAuthToken handles POST /auth/token.
func (h *Handlers) HandleAuthToken(w http.ResponseWriter, r *http.Request) {
	var req model.AuthTokenRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	agent, err := h.db.GetAgentByAgentID(r.Context(), req.AgentID)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "invalid credentials")
		return
	}

	if agent.APIKeyHash == nil {
		writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "invalid credentials")
		return
	}

	valid, err := auth.VerifyAPIKey(req.APIKey, *agent.APIKeyHash)
	if err != nil || !valid {
		writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "invalid credentials")
		return
	}

	token, expiresAt, err := h.jwtMgr.IssueToken(agent)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to issue token")
		return
	}

	writeJSON(w, r, http.StatusOK, model.AuthTokenResponse{
		Token:     token,
		ExpiresAt: expiresAt,
	})
}

// HandleSubscribe handles GET /v1/subscribe (SSE).
func (h *Handlers) HandleSubscribe(w http.ResponseWriter, r *http.Request) {
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

	ctx := r.Context()

	// Subscribe to Postgres notifications via a polling approach
	// since we may not have a dedicated notify connection available.
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// In a full implementation, this would receive from a fan-out channel
			// connected to the LISTEN/NOTIFY connection.
			// For now, clients stay connected and will receive events
			// when the notification fan-out is wired up.
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

	writeJSON(w, r, httpStatus, model.HealthResponse{
		Status:   status,
		Version:  h.version,
		Postgres: pgStatus,
		Uptime:   int64(time.Since(h.startedAt).Seconds()),
	})
}

// SeedAdmin creates the initial admin agent if the agents table is empty.
func (h *Handlers) SeedAdmin(ctx context.Context, adminAPIKey string) error {
	if adminAPIKey == "" {
		h.logger.Info("no admin API key configured, skipping admin seed")
		return nil
	}

	count, err := h.db.CountAgents(ctx)
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

func queryInt(r *http.Request, key string, defaultVal int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}

func queryTime(r *http.Request, key string) *time.Time {
	if v := r.URL.Query().Get(key); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return &t
		}
	}
	return nil
}
