package server

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/model"
)

// HandleCreateKey handles POST /v1/keys (admin-only).
// Mints a new API key for the specified agent and returns the raw key
// exactly once. After this response, only the prefix is available.
func (h *Handlers) HandleCreateKey(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	var req model.CreateKeyRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	if req.AgentID == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "agent_id is required")
		return
	}
	if err := model.ValidateAgentID(req.AgentID); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}
	if err := model.ValidateKeyLabel(req.Label); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

	// Verify the target agent exists in this org.
	if _, err := h.db.GetAgentByAgentID(r.Context(), orgID, req.AgentID); err != nil {
		if isNotFoundError(err) {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "agent not found")
			return
		}
		h.writeInternalError(w, r, "failed to verify agent", err)
		return
	}

	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid expires_at format (expected RFC3339)")
			return
		}
		if t.Before(time.Now()) {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "expires_at must be in the future")
			return
		}
		expiresAt = &t
	}

	rawKey, prefix, err := model.GenerateRawKey()
	if err != nil {
		h.writeInternalError(w, r, "failed to generate api key", err)
		return
	}

	hash, err := auth.HashAPIKey(rawKey)
	if err != nil {
		h.writeInternalError(w, r, "failed to hash api key", err)
		return
	}

	apiKey := model.APIKey{
		Prefix:    prefix,
		KeyHash:   hash,
		AgentID:   req.AgentID,
		OrgID:     orgID,
		Label:     req.Label,
		CreatedBy: claims.AgentID,
		ExpiresAt: expiresAt,
	}

	audit := h.buildAuditEntry(r, orgID, "create_api_key", "api_key", "", nil, nil, nil)
	created, err := h.db.CreateAPIKeyWithAudit(r.Context(), apiKey, audit)
	if err != nil {
		h.writeInternalError(w, r, "failed to create api key", err)
		return
	}

	writeJSON(w, r, http.StatusCreated, model.APIKeyWithRawKey{
		APIKey: created,
		RawKey: rawKey,
	})
}

// HandleListKeys handles GET /v1/keys (admin-only).
// Returns all keys for the org. Key hashes are never exposed.
func (h *Handlers) HandleListKeys(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())
	limit := queryLimit(r, 50)
	offset := queryOffset(r)

	keys, total, err := h.db.ListAPIKeys(r.Context(), orgID, limit, offset)
	if err != nil {
		h.writeInternalError(w, r, "failed to list api keys", err)
		return
	}
	if keys == nil {
		keys = []model.APIKey{}
	}

	writeJSON(w, r, http.StatusOK, model.APIKeyResponse{
		Keys:    keys,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
		HasMore: offset+len(keys) < total,
	})
}

// HandleRevokeKey handles DELETE /v1/keys/{id} (admin-only).
// Revokes a key by setting revoked_at. The key immediately stops working.
func (h *Handlers) HandleRevokeKey(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	keyIDStr := r.PathValue("id")
	keyID, err := uuid.Parse(keyIDStr)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid key id")
		return
	}

	audit := h.buildAuditEntry(r, orgID, "revoke_api_key", "api_key", keyIDStr, nil, nil, nil)
	if err := h.db.RevokeAPIKeyWithAudit(r.Context(), orgID, keyID, audit); err != nil {
		if isNotFoundError(err) {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "api key not found")
			return
		}
		h.writeInternalError(w, r, "failed to revoke api key", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleRotateKey handles POST /v1/keys/{id}/rotate (admin-only).
// Atomically revokes the old key and creates a new one with the same
// agent_id and label. Returns the new raw key exactly once.
func (h *Handlers) HandleRotateKey(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	oldKeyIDStr := r.PathValue("id")
	oldKeyID, err := uuid.Parse(oldKeyIDStr)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid key id")
		return
	}

	// Fetch the old key to inherit agent_id and label.
	oldKey, err := h.db.GetAPIKeyByID(r.Context(), orgID, oldKeyID)
	if err != nil {
		if isNotFoundError(err) {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "api key not found")
			return
		}
		h.writeInternalError(w, r, "failed to get api key", err)
		return
	}
	if oldKey.RevokedAt != nil {
		writeError(w, r, http.StatusConflict, model.ErrCodeConflict, "key is already revoked")
		return
	}

	rawKey, prefix, err := model.GenerateRawKey()
	if err != nil {
		h.writeInternalError(w, r, "failed to generate api key", err)
		return
	}

	hash, err := auth.HashAPIKey(rawKey)
	if err != nil {
		h.writeInternalError(w, r, "failed to hash api key", err)
		return
	}

	newKey := model.APIKey{
		Prefix:    prefix,
		KeyHash:   hash,
		AgentID:   oldKey.AgentID,
		OrgID:     orgID,
		Label:     oldKey.Label,
		CreatedBy: claims.AgentID,
		ExpiresAt: oldKey.ExpiresAt, // Inherit expiration.
	}

	audit := h.buildAuditEntry(r, orgID, "rotate_api_key", "api_key", oldKeyIDStr, nil, nil, nil)
	created, err := h.db.RotateAPIKeyWithAudit(r.Context(), orgID, oldKeyID, newKey, audit)
	if err != nil {
		if isNotFoundError(err) {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "api key not found or already revoked")
			return
		}
		h.writeInternalError(w, r, "failed to rotate api key", err)
		return
	}

	writeJSON(w, r, http.StatusOK, model.RotateKeyResponse{
		NewKey: model.APIKeyWithRawKey{
			APIKey: created,
			RawKey: rawKey,
		},
		RevokedKeyID: oldKeyID,
	})
}

// HandleGetUsage handles GET /v1/usage (admin-only).
// Returns decision counts grouped by API key for billing/metering.
// Accepts ?period=YYYY-MM query parameter (defaults to current month).
func (h *Handlers) HandleGetUsage(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	// Parse period (YYYY-MM format).
	periodStr := r.URL.Query().Get("period")
	var from, to time.Time
	if periodStr == "" {
		now := time.Now().UTC()
		from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		to = from.AddDate(0, 1, 0)
		periodStr = from.Format("2006-01")
	} else {
		parsed, err := time.Parse("2006-01", periodStr)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid period format (expected YYYY-MM)")
			return
		}
		from = parsed
		to = from.AddDate(0, 1, 0)
	}

	byKey, total, err := h.db.CountDecisionsByAPIKey(r.Context(), orgID, from, to)
	if err != nil {
		h.writeInternalError(w, r, "failed to get usage", err)
		return
	}

	// Enrich with key metadata using a single batch query to avoid N+1 round-trips.
	type keyUsage struct {
		KeyID   *uuid.UUID `json:"key_id"`
		Prefix  string     `json:"prefix"`
		Label   string     `json:"label"`
		AgentID string     `json:"agent_id"`
		Count   int        `json:"decisions"`
	}

	// Collect managed key IDs (skip uuid.Nil = legacy / unattributed).
	var keyIDs []uuid.UUID
	for keyID := range byKey {
		if keyID != uuid.Nil {
			keyIDs = append(keyIDs, keyID)
		}
	}

	// Batch-fetch key metadata in one round-trip.
	keysByID := make(map[uuid.UUID]model.APIKey, len(keyIDs))
	if len(keyIDs) > 0 {
		fetchedKeys, fetchErr := h.db.GetAPIKeysByIDs(r.Context(), orgID, keyIDs)
		if fetchErr == nil {
			for _, k := range fetchedKeys {
				keysByID[k.ID] = k
			}
		}
	}

	var keyUsages []keyUsage
	for keyID, count := range byKey {
		if keyID == uuid.Nil {
			keyUsages = append(keyUsages, keyUsage{
				KeyID:   nil,
				Prefix:  "",
				Label:   "(legacy)",
				AgentID: "",
				Count:   count,
			})
			continue
		}
		kid := keyID
		if k, ok := keysByID[keyID]; ok {
			keyUsages = append(keyUsages, keyUsage{
				KeyID:   &kid,
				Prefix:  k.Prefix,
				Label:   k.Label,
				AgentID: k.AgentID,
				Count:   count,
			})
		} else {
			// Key was deleted — show ID only. Best-effort.
			keyUsages = append(keyUsages, keyUsage{
				KeyID: &kid,
				Count: count,
			})
		}
	}

	// Aggregate by agent_id.
	agentCounts := make(map[string]int)
	for _, ku := range keyUsages {
		if ku.AgentID != "" {
			agentCounts[ku.AgentID] += ku.Count
		}
	}
	type agentUsage struct {
		AgentID   string `json:"agent_id"`
		Decisions int    `json:"decisions"`
	}
	var byAgent []agentUsage
	for aid, cnt := range agentCounts {
		byAgent = append(byAgent, agentUsage{AgentID: aid, Decisions: cnt})
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"org_id":          orgID,
		"period":          periodStr,
		"total_decisions": total,
		"by_key":          keyUsages,
		"by_agent":        byAgent,
	})
}
