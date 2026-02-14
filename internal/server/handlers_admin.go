package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// HandleCreateAgent handles POST /v1/agents (admin-only).
func (h *Handlers) HandleCreateAgent(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

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

	// Validate role is known and caller outranks the requested role.
	if model.RoleRank(req.Role) == 0 {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
			"invalid role: must be one of platform_admin, org_owner, admin, agent, reader")
		return
	}
	callerRole := ClaimsFromContext(r.Context()).Role
	if model.RoleRank(callerRole) <= model.RoleRank(req.Role) {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden,
			"cannot create agent with role equal to or higher than your own")
		return
	}

	hash, err := auth.HashAPIKey(req.APIKey)
	if err != nil {
		h.writeInternalError(w, r, "failed to hash api key", err)
		return
	}

	// Validate tags if provided.
	for _, tag := range req.Tags {
		if err := model.ValidateTag(tag); err != nil {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
			return
		}
	}

	agent, err := h.db.CreateAgent(r.Context(), model.Agent{
		AgentID:    req.AgentID,
		OrgID:      orgID,
		Name:       req.Name,
		Role:       req.Role,
		APIKeyHash: &hash,
		Tags:       req.Tags,
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
	offset := queryOffset(r)

	agents, err := h.db.ListAgents(r.Context(), orgID, limit, offset)
	if err != nil {
		h.writeInternalError(w, r, "failed to list agents", err)
		return
	}
	total, err := h.db.CountAgents(r.Context(), orgID)
	if err != nil {
		h.writeInternalError(w, r, "failed to count agents", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"agents":   agents,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
		"has_more": offset+len(agents) < total,
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

	// Validate resource_type and permission against known constants.
	validResourceTypes := map[string]bool{
		string(model.ResourceAgentTraces): true,
	}
	validPermissions := map[string]bool{
		string(model.PermissionRead): true,
	}
	if !validResourceTypes[req.ResourceType] {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid resource_type")
		return
	}
	if !validPermissions[req.Permission] {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid permission")
		return
	}

	// Get grantor agent.
	grantor, err := h.db.GetAgentByAgentID(r.Context(), orgID, claims.AgentID)
	if err != nil {
		h.writeInternalError(w, r, "failed to get grantor", err)
		return
	}

	// Only admins and the owner of the resource can grant access.
	// Non-admin agents can only create grants for agent_traces on their own traces.
	if !model.RoleAtLeast(claims.Role, model.RoleAdmin) {
		if req.ResourceType != string(model.ResourceAgentTraces) {
			writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "agents can only grant access to their own traces")
			return
		}
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

	// Invalidate the grantee's cached access set so the new grant takes effect immediately.
	if h.grantCache != nil {
		h.grantCache.Invalidate(orgID.String() + ":" + grantee.ID.String())
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

	if err := h.db.DeleteGrant(r.Context(), orgID, grantID); err != nil {
		h.writeInternalError(w, r, "failed to delete grant", err)
		return
	}

	// Invalidate the grantee's cached access set so the revocation takes effect immediately.
	if h.grantCache != nil {
		h.grantCache.Invalidate(orgID.String() + ":" + grant.GranteeID.String())
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleDeleteAgent handles DELETE /v1/agents/{agent_id} (admin-only).
// Deletes all data associated with the agent (GDPR right to erasure).
func (h *Handlers) HandleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	if !h.enableDestructiveDelete {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden,
			"destructive delete is disabled; set AKASHI_ENABLE_DESTRUCTIVE_DELETE=true to enable")
		return
	}

	orgID := OrgIDFromContext(r.Context())
	agentID := r.PathValue("agent_id")
	if err := model.ValidateAgentID(agentID); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

	// Protect the seed admin (agent_id="admin") created during server startup.
	// Other admin-role agents are deletable by org_owner+ callers.
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

// HandleUpdateAgentTags handles PATCH /v1/agents/{agent_id}/tags (admin-only).
func (h *Handlers) HandleUpdateAgentTags(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())
	agentID := r.PathValue("agent_id")
	if err := model.ValidateAgentID(agentID); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

	var req model.UpdateAgentTagsRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	if req.Tags == nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "tags field is required")
		return
	}

	for _, tag := range req.Tags {
		if err := model.ValidateTag(tag); err != nil {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
			return
		}
	}

	// Deduplicate tags while preserving order.
	seen := make(map[string]struct{}, len(req.Tags))
	deduped := make([]string, 0, len(req.Tags))
	for _, tag := range req.Tags {
		if _, ok := seen[tag]; !ok {
			seen[tag] = struct{}{}
			deduped = append(deduped, tag)
		}
	}

	agent, err := h.db.UpdateAgentTags(r.Context(), orgID, agentID, deduped)
	if err != nil {
		if isNotFoundError(err) {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "agent not found")
			return
		}
		h.writeInternalError(w, r, "failed to update agent tags", err)
		return
	}

	writeJSON(w, r, http.StatusOK, agent)
}

// isDuplicateKeyError checks if a Postgres error is a unique_violation (23505).
func isDuplicateKeyError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isNotFoundError checks if the error indicates a missing resource.
// Uses sentinel error matching instead of fragile string comparison.
func isNotFoundError(err error) bool {
	return errors.Is(err, storage.ErrNotFound) || errors.Is(err, pgx.ErrNoRows)
}
