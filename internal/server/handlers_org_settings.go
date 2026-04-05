package server

import (
	"net/http"

	"github.com/ashita-ai/akashi/internal/model"
)

// HandleGetOrgSettings handles GET /v1/org/settings.
// Returns the org's current settings.
func (h *Handlers) HandleGetOrgSettings(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())
	settings, err := h.db.GetOrgSettings(r.Context(), orgID)
	if err != nil {
		h.writeInternalError(w, r, "failed to get org settings", err)
		return
	}
	writeJSON(w, r, http.StatusOK, settings.Settings)
}

// HandleSetOrgSettings handles PUT /v1/org/settings.
// Updates the org's settings. Requires admin role.
func (h *Handlers) HandleSetOrgSettings(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())
	claims := ClaimsFromContext(r.Context())

	var req model.OrgSettingsData
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
		return
	}

	if req.ConflictResolution != nil {
		if err := req.ConflictResolution.Validate(); err != nil {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
			return
		}
	}

	updatedBy := claims.ActorID()

	audit := h.buildAuditEntry(r, orgID,
		"org_settings_updated", "org_settings", orgID.String(),
		nil, nil, // before/after populated inside the transaction
		map[string]any{"updated_by": updatedBy},
	)

	if err := h.db.UpsertOrgSettingsWithAudit(r.Context(), orgID, req, updatedBy, audit); err != nil {
		h.writeInternalError(w, r, "failed to update org settings", err)
		return
	}

	// Read back the settings to include updated_at.
	settings, err := h.db.GetOrgSettings(r.Context(), orgID)
	if err != nil {
		h.writeInternalError(w, r, "failed to read org settings after update", err)
		return
	}
	writeJSON(w, r, http.StatusOK, settings.Settings)
}
