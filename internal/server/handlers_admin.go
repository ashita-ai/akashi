package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/billing"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// HandleCreateAgent handles POST /v1/agents (admin-only).
func (h *Handlers) HandleCreateAgent(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	// Check agent quota before creating.
	if h.billingSvc != nil && h.billingSvc.Enabled() {
		if err := h.billingSvc.CheckAgentQuota(r.Context(), orgID); err != nil {
			if errors.Is(err, billing.ErrAgentLimitExceeded) {
				writeError(w, r, http.StatusTooManyRequests, model.ErrCodeQuotaExceeded, err.Error())
				return
			}
			h.logger.Error("agent quota check failed", "error", err, "org_id", orgID)
			writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "quota check failed")
			return
		}
	}

	var req model.CreateAgentRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	if req.AgentID == "" || req.Name == "" || req.APIKey == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "agent_id, name, and api_key are required")
		return
	}
	if err := model.ValidateAgentID(req.AgentID); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

	if req.Role == "" {
		req.Role = model.RoleAgent
	}

	hash, err := auth.HashAPIKey(req.APIKey)
	if err != nil {
		h.writeInternalError(w, r, "failed to hash api key", err)
		return
	}

	agent, err := h.db.CreateAgent(r.Context(), model.Agent{
		AgentID:    req.AgentID,
		OrgID:      orgID,
		Name:       req.Name,
		Role:       req.Role,
		APIKeyHash: &hash,
		Metadata:   req.Metadata,
	})
	if err != nil {
		if isDuplicateKeyError(err) {
			writeError(w, r, http.StatusConflict, model.ErrCodeConflict, "agent_id already exists")
			return
		}
		h.writeInternalError(w, r, "failed to create agent", err)
		return
	}

	writeJSON(w, r, http.StatusCreated, agent)
}

// HandleListAgents handles GET /v1/agents (admin-only).
func (h *Handlers) HandleListAgents(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())
	limit := queryLimit(r, 200)
	offset := queryInt(r, "offset", 0)

	agents, err := h.db.ListAgents(r.Context(), orgID, limit, offset)
	if err != nil {
		h.writeInternalError(w, r, "failed to list agents", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"agents": agents,
		"limit":  limit,
		"offset": offset,
	})
}

// HandleCreateGrant handles POST /v1/grants.
func (h *Handlers) HandleCreateGrant(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	var req model.CreateGrantRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	// Get grantor agent.
	grantor, err := h.db.GetAgentByAgentID(r.Context(), orgID, claims.AgentID)
	if err != nil {
		h.writeInternalError(w, r, "failed to get grantor", err)
		return
	}

	// Only admins and the owner of the resource can grant access.
	if !model.RoleAtLeast(claims.Role, model.RoleAdmin) {
		if req.ResourceID == nil || *req.ResourceID != claims.AgentID {
			writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "can only grant access to your own traces")
			return
		}
	}

	// Get grantee agent.
	grantee, err := h.db.GetAgentByAgentID(r.Context(), orgID, req.GranteeAgentID)
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
		OrgID:        orgID,
		GrantorID:    grantor.ID,
		GranteeID:    grantee.ID,
		ResourceType: req.ResourceType,
		ResourceID:   req.ResourceID,
		Permission:   req.Permission,
		ExpiresAt:    expiresAt,
	})
	if err != nil {
		if isDuplicateKeyError(err) {
			writeError(w, r, http.StatusConflict, model.ErrCodeConflict, "grant already exists")
			return
		}
		h.writeInternalError(w, r, "failed to create grant", err)
		return
	}

	writeJSON(w, r, http.StatusCreated, grant)
}

// HandleDeleteGrant handles DELETE /v1/grants/{grant_id}.
func (h *Handlers) HandleDeleteGrant(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	grantIDStr := r.PathValue("grant_id")
	grantID, err := uuid.Parse(grantIDStr)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid grant_id")
		return
	}

	// Verify the grant exists and belongs to the caller's org.
	grant, err := h.db.GetGrant(r.Context(), orgID, grantID)
	if err != nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "grant not found")
		return
	}

	// Only admins or the grantor can delete a grant.
	if !model.RoleAtLeast(claims.Role, model.RoleAdmin) {
		grantor, err := h.db.GetAgentByAgentID(r.Context(), orgID, claims.AgentID)
		if err != nil || grant.GrantorID != grantor.ID {
			writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "can only delete your own grants")
			return
		}
	}

	if err := h.db.DeleteGrant(r.Context(), grantID); err != nil {
		h.writeInternalError(w, r, "failed to delete grant", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleDeleteAgent handles DELETE /v1/agents/{agent_id} (admin-only).
// Deletes all data associated with the agent (GDPR right to erasure).
func (h *Handlers) HandleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())
	agentID := r.PathValue("agent_id")
	if err := model.ValidateAgentID(agentID); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

	// Prevent deleting the admin account to avoid lockout.
	if agentID == "admin" {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "cannot delete the admin agent")
		return
	}

	result, err := h.db.DeleteAgentData(r.Context(), orgID, agentID)
	if err != nil {
		if errors.Is(err, storage.ErrAgentNotFound) {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "agent not found")
			return
		}
		h.writeInternalError(w, r, "failed to delete agent data", err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"agent_id": agentID,
		"deleted":  result,
	})
}

// isDuplicateKeyError checks if a Postgres error is a unique_violation (23505).
func isDuplicateKeyError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
