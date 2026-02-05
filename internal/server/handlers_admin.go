package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/model"
)

// HandleCreateAgent handles POST /v1/agents (admin-only).
func (h *Handlers) HandleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req model.CreateAgentRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
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
		if isDuplicateKeyError(err) {
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

// HandleCreateGrant handles POST /v1/grants.
func (h *Handlers) HandleCreateGrant(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())

	var req model.CreateGrantRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
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
		if isDuplicateKeyError(err) {
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
	claims := ClaimsFromContext(r.Context())

	grantIDStr := r.PathValue("grant_id")
	grantID, err := uuid.Parse(grantIDStr)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid grant_id")
		return
	}

	// Verify the grant exists and the caller has permission to delete it.
	grant, err := h.db.GetGrant(r.Context(), grantID)
	if err != nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "grant not found")
		return
	}

	// Only admins or the grantor can delete a grant.
	if claims.Role != model.RoleAdmin {
		grantor, err := h.db.GetAgentByAgentID(r.Context(), claims.AgentID)
		if err != nil || grant.GrantorID != grantor.ID {
			writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "can only delete your own grants")
			return
		}
	}

	if err := h.db.DeleteGrant(r.Context(), grantID); err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to delete grant")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// isDuplicateKeyError checks if a Postgres error is a unique_violation (23505).
func isDuplicateKeyError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
