package server

import (
	"net/http"

	"github.com/ashita-ai/akashi/internal/model"
)

// HandleCreateProjectLink handles POST /v1/project-links (admin-only).
func (h *Handlers) HandleCreateProjectLink(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	var req model.CreateProjectLinkRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
		return
	}

	if req.ProjectA == "" || req.ProjectB == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "project_a and project_b are required")
		return
	}
	if req.ProjectA == req.ProjectB {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "project_a and project_b must be different")
		return
	}

	linkType := req.LinkType
	if linkType == "" {
		linkType = "conflict_scope"
	}

	// Normalize order so (A,B) and (B,A) produce the same unique constraint key.
	projectA, projectB := req.ProjectA, req.ProjectB
	if projectA > projectB {
		projectA, projectB = projectB, projectA
	}

	audit := h.buildAuditEntry(r, orgID, "create_project_link", "project_link", "", nil, nil, nil)
	pl, err := h.db.CreateProjectLinkWithAudit(r.Context(), model.ProjectLink{
		OrgID:     orgID,
		ProjectA:  projectA,
		ProjectB:  projectB,
		LinkType:  linkType,
		CreatedBy: claims.AgentID,
	}, audit)
	if err != nil {
		if isDuplicateKeyError(err) {
			writeError(w, r, http.StatusConflict, model.ErrCodeConflict, "project link already exists")
			return
		}
		h.writeInternalError(w, r, "failed to create project link", err)
		return
	}

	writeJSON(w, r, http.StatusCreated, pl)
}

// HandleListProjectLinks handles GET /v1/project-links (admin-only).
func (h *Handlers) HandleListProjectLinks(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())
	limit := queryLimit(r, 50)
	offset := queryOffset(r)

	links, total, err := h.db.ListProjectLinks(r.Context(), orgID, limit, offset)
	if err != nil {
		h.writeInternalError(w, r, "failed to list project links", err)
		return
	}

	ptotal := total
	writeListJSON(w, r, links, &ptotal, offset+len(links) < total, limit, offset)
}

// HandleDeleteProjectLink handles DELETE /v1/project-links/{id} (admin-only).
func (h *Handlers) HandleDeleteProjectLink(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	id, err := parsePathUUID(r, "id")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid id")
		return
	}

	// Verify the link exists before deleting (for audit before_data).
	pl, err := h.db.GetProjectLink(r.Context(), orgID, id)
	if err != nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "project link not found")
		return
	}

	audit := h.buildAuditEntry(r, orgID, "delete_project_link", "project_link", pl.ID.String(), pl, nil, nil)
	if err := h.db.DeleteProjectLinkWithAudit(r.Context(), orgID, id, audit); err != nil {
		h.writeInternalError(w, r, "failed to delete project link", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleGrantAllProjectLinks handles POST /v1/project-links/grant-all (admin-only).
// Creates bidirectional conflict_scope links between all distinct projects in the org.
func (h *Handlers) HandleGrantAllProjectLinks(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	var req model.GrantAllProjectLinksRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
		return
	}

	linkType := req.LinkType
	if linkType == "" {
		linkType = "conflict_scope"
	}

	audit := h.buildAuditEntry(r, orgID, "grant_all_project_links", "project_link", "", nil, nil, nil)
	created, err := h.db.GrantAllProjectLinks(r.Context(), orgID, claims.AgentID, linkType, audit)
	if err != nil {
		h.writeInternalError(w, r, "failed to grant all project links", err)
		return
	}

	writeJSON(w, r, http.StatusOK, model.GrantAllProjectLinksResponse{
		LinksCreated: created,
	})
}
