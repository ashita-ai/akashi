package server

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// HandleGetRetention handles GET /v1/retention.
// Returns the org's current retention policy and last-run metadata.
func (h *Handlers) HandleGetRetention(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())
	policy, err := h.db.GetRetentionPolicy(r.Context(), orgID, h.retentionInterval)
	if err != nil {
		h.writeInternalError(w, r, "failed to get retention policy", err)
		return
	}

	holds, err := h.db.ListHolds(r.Context(), orgID)
	if err != nil {
		h.writeInternalError(w, r, "failed to list holds", err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"retention_days":          policy.RetentionDays,
		"retention_exclude_types": policy.RetentionExcludeTypes,
		"last_run":                policy.LastRunAt,
		"last_run_deleted":        policy.LastRunDeleted,
		"next_run":                policy.NextRunAt,
		"holds":                   holds,
	})
}

// retentionPolicyRequest is the body for PUT /v1/retention.
type retentionPolicyRequest struct {
	RetentionDays         *int     `json:"retention_days"`
	RetentionExcludeTypes []string `json:"retention_exclude_types"`
}

// HandleSetRetention handles PUT /v1/retention.
// Sets the org's retention policy. Requires admin role.
func (h *Handlers) HandleSetRetention(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	var req retentionPolicyRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
		return
	}

	if req.RetentionDays != nil && *req.RetentionDays < 1 {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
			"retention_days must be >= 1")
		return
	}

	if err := h.db.SetRetentionPolicy(r.Context(), orgID, req.RetentionDays, req.RetentionExcludeTypes); err != nil {
		h.writeInternalError(w, r, "failed to set retention policy", err)
		return
	}

	policy, err := h.db.GetRetentionPolicy(r.Context(), orgID, h.retentionInterval)
	if err != nil {
		h.writeInternalError(w, r, "failed to read retention policy after update", err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"retention_days":          policy.RetentionDays,
		"retention_exclude_types": policy.RetentionExcludeTypes,
		"last_run":                policy.LastRunAt,
		"last_run_deleted":        policy.LastRunDeleted,
		"next_run":                policy.NextRunAt,
	})
}

// purgeRequest is the body for POST /v1/retention/purge.
type purgeRequest struct {
	Before       time.Time `json:"before"`
	DecisionType *string   `json:"decision_type,omitempty"`
	AgentID      *string   `json:"agent_id,omitempty"`
	DryRun       bool      `json:"dry_run"`
}

// HandlePurge handles POST /v1/retention/purge.
// Deletes (or dry-runs) decisions older than before. Requires admin role.
func (h *Handlers) HandlePurge(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())
	claims := ClaimsFromContext(r.Context())

	var req purgeRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
		return
	}

	if req.Before.IsZero() {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
			"before is required")
		return
	}

	if req.DryRun {
		counts, err := h.db.CountEligibleDecisions(r.Context(), orgID, req.Before, req.DecisionType, req.AgentID)
		if err != nil {
			h.writeInternalError(w, r, "failed to count eligible decisions", err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{
			"dry_run":      true,
			"would_delete": counts,
		})
		return
	}

	// Real purge: log it, run it, complete the log.
	initiatedBy := claims.AgentID
	if initiatedBy == "" {
		initiatedBy = claims.Subject
	}
	criteria := map[string]any{"before": req.Before}
	if req.DecisionType != nil {
		criteria["decision_type"] = *req.DecisionType
	}
	if req.AgentID != nil {
		criteria["agent_id"] = *req.AgentID
	}

	logID, err := h.db.StartDeletionLog(r.Context(), orgID, "manual", initiatedBy, criteria)
	if err != nil {
		h.writeInternalError(w, r, "failed to start deletion log", err)
		return
	}

	counts, err := h.db.BatchDeleteDecisions(r.Context(), orgID, req.Before, req.DecisionType, req.AgentID, nil, 1000)
	if err != nil {
		h.writeInternalError(w, r, "purge failed", err)
		return
	}

	countMap := map[string]any{
		"decisions":    counts.Decisions,
		"alternatives": counts.Alternatives,
		"evidence":     counts.Evidence,
		"claims":       counts.Claims,
		"events":       counts.Events,
	}
	_ = h.db.CompleteDeletionLog(r.Context(), logID, countMap)

	writeJSON(w, r, http.StatusOK, map[string]any{
		"dry_run": false,
		"deleted": countMap,
	})
}

// holdRequest is the body for POST /v1/retention/hold.
type holdRequest struct {
	Reason        string    `json:"reason"`
	From          time.Time `json:"from"`
	To            time.Time `json:"to"`
	DecisionTypes []string  `json:"decision_types,omitempty"`
	AgentIDs      []string  `json:"agent_ids,omitempty"`
}

// HandleCreateHold handles POST /v1/retention/hold.
// Creates a legal hold that prevents automated deletion. Requires admin role.
func (h *Handlers) HandleCreateHold(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())
	claims := ClaimsFromContext(r.Context())

	var req holdRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
		return
	}

	if req.Reason == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "reason is required")
		return
	}
	if req.From.IsZero() || req.To.IsZero() {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "from and to are required")
		return
	}
	if !req.To.After(req.From) {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "to must be after from")
		return
	}

	createdBy := claims.AgentID
	if createdBy == "" {
		createdBy = claims.Subject
	}

	hold, err := h.db.CreateHold(r.Context(), storage.RetentionHold{
		OrgID:         orgID,
		Reason:        req.Reason,
		HoldFrom:      req.From,
		HoldTo:        req.To,
		DecisionTypes: req.DecisionTypes,
		AgentIDs:      req.AgentIDs,
		CreatedBy:     createdBy,
	})
	if err != nil {
		h.writeInternalError(w, r, "failed to create hold", err)
		return
	}

	writeJSON(w, r, http.StatusCreated, hold)
}

// HandleReleaseHold handles DELETE /v1/retention/hold/{id}.
// Releases (deactivates) a legal hold. Requires admin role.
func (h *Handlers) HandleReleaseHold(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid hold id")
		return
	}

	released, err := h.db.ReleaseHold(r.Context(), id, orgID)
	if err != nil {
		h.writeInternalError(w, r, "failed to release hold", err)
		return
	}
	if !released {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "hold not found or already released")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
