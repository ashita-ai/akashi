package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/ashita-ai/kyoyu/internal/auth"
	"github.com/ashita-ai/kyoyu/internal/model"
	"github.com/ashita-ai/kyoyu/internal/service/embedding"
	"github.com/ashita-ai/kyoyu/internal/service/trace"
	"github.com/ashita-ai/kyoyu/internal/storage"
)

// Handlers holds HTTP handler dependencies.
type Handlers struct {
	db       *storage.DB
	jwtMgr   *auth.JWTManager
	embedder embedding.Provider
	buffer   *trace.Buffer
	logger   *slog.Logger
	startedAt time.Time
}

// NewHandlers creates a new Handlers with all dependencies.
func NewHandlers(db *storage.DB, jwtMgr *auth.JWTManager, embedder embedding.Provider, buffer *trace.Buffer, logger *slog.Logger) *Handlers {
	return &Handlers{
		db:        db,
		jwtMgr:    jwtMgr,
		embedder:  embedder,
		buffer:    buffer,
		logger:    logger,
		startedAt: time.Now(),
	}
}

// HandleAuthToken handles POST /auth/token.
func (h *Handlers) HandleAuthToken(w http.ResponseWriter, r *http.Request) {
	var req model.AuthTokenRequest
	if err := decodeJSON(r, &req); err != nil {
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

// HandleCreateAgent handles POST /v1/agents (admin-only).
func (h *Handlers) HandleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req model.CreateAgentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	if req.AgentID == "" || req.Name == "" || req.APIKey == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "agent_id, name, and api_key are required")
		return
	}

	if req.Role == "" {
		req.Role = model.RoleAgent
	}

	hash, err := auth.HashAPIKey(req.APIKey)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to hash api key")
		return
	}

	agent, err := h.db.CreateAgent(r.Context(), model.Agent{
		AgentID:    req.AgentID,
		Name:       req.Name,
		Role:       req.Role,
		APIKeyHash: &hash,
		Metadata:   req.Metadata,
	})
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			writeError(w, r, http.StatusConflict, model.ErrCodeConflict, "agent_id already exists")
			return
		}
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to create agent")
		return
	}

	writeJSON(w, r, http.StatusCreated, agent)
}

// HandleListAgents handles GET /v1/agents (admin-only).
func (h *Handlers) HandleListAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := h.db.ListAgents(r.Context())
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to list agents")
		return
	}
	writeJSON(w, r, http.StatusOK, agents)
}

// HandleCreateRun handles POST /v1/runs.
func (h *Handlers) HandleCreateRun(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())

	var req model.CreateRunRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	// Agents can only create runs for themselves.
	if claims.Role != model.RoleAdmin && req.AgentID != claims.AgentID {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "can only create runs for your own agent_id")
		return
	}

	run, err := h.db.CreateRun(r.Context(), req)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to create run")
		return
	}

	writeJSON(w, r, http.StatusCreated, run)
}

// HandleAppendEvents handles POST /v1/runs/{run_id}/events.
func (h *Handlers) HandleAppendEvents(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	runID, err := parseRunID(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

	// Verify run exists and agent has access.
	run, err := h.db.GetRun(r.Context(), runID)
	if err != nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "run not found")
		return
	}

	if claims.Role != model.RoleAdmin && run.AgentID != claims.AgentID {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "not your run")
		return
	}

	var req model.AppendEventsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	events, err := h.buffer.Append(r.Context(), runID, run.AgentID, req.Events)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to buffer events")
		return
	}

	eventIDs := make([]uuid.UUID, len(events))
	for i, e := range events {
		eventIDs[i] = e.ID
	}

	writeJSON(w, r, http.StatusCreated, map[string]any{
		"accepted":  len(events),
		"event_ids": eventIDs,
	})
}

// HandleCompleteRun handles POST /v1/runs/{run_id}/complete.
func (h *Handlers) HandleCompleteRun(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	runID, err := parseRunID(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

	run, err := h.db.GetRun(r.Context(), runID)
	if err != nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "run not found")
		return
	}
	if claims.Role != model.RoleAdmin && run.AgentID != claims.AgentID {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "not your run")
		return
	}

	var req model.CompleteRunRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	status := model.RunStatusCompleted
	if req.Status == "failed" {
		status = model.RunStatusFailed
	}

	if err := h.db.CompleteRun(r.Context(), runID, status, req.Metadata); err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to complete run")
		return
	}

	updated, _ := h.db.GetRun(r.Context(), runID)
	writeJSON(w, r, http.StatusOK, updated)
}

// HandleTrace handles POST /v1/trace (convenience endpoint).
func (h *Handlers) HandleTrace(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())

	var req model.TraceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	if claims.Role != model.RoleAdmin && req.AgentID != claims.AgentID {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "can only trace for your own agent_id")
		return
	}

	// 1. Create run.
	run, err := h.db.CreateRun(r.Context(), model.CreateRunRequest{
		AgentID:  req.AgentID,
		TraceID:  req.TraceID,
		Metadata: req.Metadata,
	})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to create run")
		return
	}

	// 2. Generate embedding for decision.
	embText := req.Decision.DecisionType + ": " + req.Decision.Outcome
	if req.Decision.Reasoning != nil {
		embText += " " + *req.Decision.Reasoning
	}
	var decisionEmb *pgvector.Vector
	emb, err := h.embedder.Embed(r.Context(), embText)
	if err != nil {
		h.logger.Warn("trace: embedding generation failed", "error", err)
	} else {
		decisionEmb = &emb
	}

	// 3. Create decision.
	decision, err := h.db.CreateDecision(r.Context(), model.Decision{
		RunID:        run.ID,
		AgentID:      req.AgentID,
		DecisionType: req.Decision.DecisionType,
		Outcome:      req.Decision.Outcome,
		Confidence:   req.Decision.Confidence,
		Reasoning:    req.Decision.Reasoning,
		Embedding:    decisionEmb,
	})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to create decision")
		return
	}

	// 4. Create alternatives.
	eventCount := 0
	if len(req.Decision.Alternatives) > 0 {
		alts := make([]model.Alternative, len(req.Decision.Alternatives))
		for i, a := range req.Decision.Alternatives {
			alts[i] = model.Alternative{
				DecisionID:      decision.ID,
				Label:           a.Label,
				Score:           a.Score,
				Selected:        a.Selected,
				RejectionReason: a.RejectionReason,
			}
		}
		if err := h.db.CreateAlternativesBatch(r.Context(), alts); err != nil {
			h.logger.Warn("trace: failed to create alternatives", "error", err)
		}
		eventCount += len(alts)
	}

	// 5. Create evidence with embeddings.
	if len(req.Decision.Evidence) > 0 {
		evs := make([]model.Evidence, len(req.Decision.Evidence))
		for i, e := range req.Decision.Evidence {
			var evEmb *pgvector.Vector
			if e.Content != "" {
				vec, err := h.embedder.Embed(r.Context(), e.Content)
				if err != nil {
					h.logger.Warn("trace: evidence embedding failed", "error", err)
				} else {
					evEmb = &vec
				}
			}
			evs[i] = model.Evidence{
				DecisionID:     decision.ID,
				SourceType:     model.SourceType(e.SourceType),
				SourceURI:      e.SourceURI,
				Content:        e.Content,
				RelevanceScore: e.RelevanceScore,
				Embedding:      evEmb,
			}
		}
		if err := h.db.CreateEvidenceBatch(r.Context(), evs); err != nil {
			h.logger.Warn("trace: failed to create evidence", "error", err)
		}
		eventCount += len(evs)
	}

	// 6. Complete run.
	_ = h.db.CompleteRun(r.Context(), run.ID, model.RunStatusCompleted, nil)

	// 7. Notify subscribers.
	notifyPayload, _ := json.Marshal(map[string]any{
		"decision_id": decision.ID,
		"agent_id":    req.AgentID,
		"outcome":     req.Decision.Outcome,
	})
	_ = h.db.Notify(r.Context(), storage.ChannelDecisions, string(notifyPayload))

	writeJSON(w, r, http.StatusCreated, map[string]any{
		"run_id":      run.ID,
		"decision_id": decision.ID,
		"event_count": eventCount + 1, // +1 for the decision itself
	})
}

// HandleQuery handles POST /v1/query.
func (h *Handlers) HandleQuery(w http.ResponseWriter, r *http.Request) {
	var req model.QueryRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	decisions, total, err := h.db.QueryDecisions(r.Context(), req)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "query failed")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"decisions": decisions,
		"total":     total,
		"limit":     req.Limit,
		"offset":    req.Offset,
	})
}

// HandleTemporalQuery handles POST /v1/query/temporal.
func (h *Handlers) HandleTemporalQuery(w http.ResponseWriter, r *http.Request) {
	var req model.TemporalQueryRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	decisions, err := h.db.QueryDecisionsTemporal(r.Context(), req)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "temporal query failed")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"as_of":     req.AsOf,
		"decisions": decisions,
	})
}

// HandleGetRun handles GET /v1/runs/{run_id}.
func (h *Handlers) HandleGetRun(w http.ResponseWriter, r *http.Request) {
	runID, err := parseRunID(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

	run, err := h.db.GetRun(r.Context(), runID)
	if err != nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "run not found")
		return
	}

	events, err := h.db.GetEventsByRun(r.Context(), runID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to get events")
		return
	}

	// Get decisions for this run by querying by agent and filtering to this run.
	var decisions []model.Decision
	runDecisions, _, err := h.db.QueryDecisions(r.Context(), model.QueryRequest{
		Filters: model.QueryFilters{
			AgentIDs: []string{run.AgentID},
		},
		Include: []string{"alternatives", "evidence"},
		Limit:   100,
	})
	if err == nil {
		for _, d := range runDecisions {
			if d.RunID == runID {
				decisions = append(decisions, d)
			}
		}
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"run":       run,
		"events":    events,
		"decisions": decisions,
	})
}

// HandleAgentHistory handles GET /v1/agents/{agent_id}/history.
func (h *Handlers) HandleAgentHistory(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agent_id")
	if agentID == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "agent_id is required")
		return
	}

	limit := queryInt(r, "limit", 50)
	offset := queryInt(r, "offset", 0)
	from := queryTime(r, "from")
	to := queryTime(r, "to")

	decisions, total, err := h.db.GetDecisionsByAgent(r.Context(), agentID, limit, offset, from, to)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to get history")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"agent_id":  agentID,
		"decisions": decisions,
		"total":     total,
		"limit":     limit,
		"offset":    offset,
	})
}

// HandleSearch handles POST /v1/search.
func (h *Handlers) HandleSearch(w http.ResponseWriter, r *http.Request) {
	var req model.SearchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	if req.Query == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "query is required")
		return
	}

	queryEmb, err := h.embedder.Embed(r.Context(), req.Query)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to generate query embedding")
		return
	}

	results, err := h.db.SearchDecisionsByEmbedding(r.Context(), queryEmb, req.Filters, req.Limit)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "search failed")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"results": results,
		"total":   len(results),
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

// HandleCreateGrant handles POST /v1/grants.
func (h *Handlers) HandleCreateGrant(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())

	var req model.CreateGrantRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	// Get grantor agent.
	grantor, err := h.db.GetAgentByAgentID(r.Context(), claims.AgentID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to get grantor")
		return
	}

	// Only admins and the owner of the resource can grant access.
	if claims.Role != model.RoleAdmin {
		if req.ResourceID == nil || *req.ResourceID != claims.AgentID {
			writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "can only grant access to your own traces")
			return
		}
	}

	// Get grantee agent.
	grantee, err := h.db.GetAgentByAgentID(r.Context(), req.GranteeAgentID)
	if err != nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "grantee agent not found")
		return
	}

	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid expires_at format")
			return
		}
		expiresAt = &t
	}

	grant, err := h.db.CreateGrant(r.Context(), model.AccessGrant{
		GrantorID:    grantor.ID,
		GranteeID:    grantee.ID,
		ResourceType: req.ResourceType,
		ResourceID:   req.ResourceID,
		Permission:   req.Permission,
		ExpiresAt:    expiresAt,
	})
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			writeError(w, r, http.StatusConflict, model.ErrCodeConflict, "grant already exists")
			return
		}
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to create grant")
		return
	}

	writeJSON(w, r, http.StatusCreated, grant)
}

// HandleDeleteGrant handles DELETE /v1/grants/{grant_id}.
func (h *Handlers) HandleDeleteGrant(w http.ResponseWriter, r *http.Request) {
	grantIDStr := r.PathValue("grant_id")
	grantID, err := uuid.Parse(grantIDStr)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid grant_id")
		return
	}

	if err := h.db.DeleteGrant(r.Context(), grantID); err != nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "grant not found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleListConflicts handles GET /v1/conflicts.
func (h *Handlers) HandleListConflicts(w http.ResponseWriter, r *http.Request) {
	var decisionType *string
	if dt := r.URL.Query().Get("decision_type"); dt != "" {
		decisionType = &dt
	}
	limit := queryInt(r, "limit", 50)

	conflicts, err := h.db.ListConflicts(r.Context(), decisionType, limit)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to list conflicts")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"conflicts": conflicts,
		"total":     len(conflicts),
	})
}

// HandleHealth handles GET /health.
func (h *Handlers) HandleHealth(w http.ResponseWriter, r *http.Request) {
	pgStatus := "connected"
	if err := h.db.Ping(r.Context()); err != nil {
		pgStatus = "disconnected"
	}

	writeJSON(w, r, http.StatusOK, model.HealthResponse{
		Status:   "healthy",
		Version:  "0.1.0",
		Postgres: pgStatus,
		Uptime:   int64(time.Since(h.startedAt).Seconds()),
	})
}

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
