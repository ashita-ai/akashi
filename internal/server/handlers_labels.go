package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/akashi/internal/storage"
)

type upsertLabelRequest struct {
	Label string  `json:"label"`
	Notes *string `json:"notes,omitempty"`
}

type labelResponse struct {
	ScoredConflictID uuid.UUID `json:"scored_conflict_id"`
	OrgID            uuid.UUID `json:"org_id"`
	Label            string    `json:"label"`
	LabeledBy        string    `json:"labeled_by"`
	LabeledAt        string    `json:"labeled_at"`
	Notes            *string   `json:"notes,omitempty"`
}

func toLabelResponse(cl storage.ConflictLabel) labelResponse {
	return labelResponse{
		ScoredConflictID: cl.ScoredConflictID,
		OrgID:            cl.OrgID,
		Label:            cl.Label,
		LabeledBy:        cl.LabeledBy,
		LabeledAt:        cl.LabeledAt.Format(time.RFC3339),
		Notes:            cl.Notes,
	}
}

var validLabels = map[string]bool{
	"genuine":                   true,
	"related_not_contradicting": true,
	"unrelated_false_positive":  true,
}

// HandleUpsertConflictLabel handles PUT /v1/admin/conflicts/{id}/label.
func (h *Handlers) HandleUpsertConflictLabel(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())
	claims := ClaimsFromContext(r.Context())

	conflictID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid conflict id"})
		return
	}

	var req upsertLabelRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		return
	}
	if !validLabels[req.Label] {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error": "label must be one of: genuine, related_not_contradicting, unrelated_false_positive",
		})
		return
	}

	cl := storage.ConflictLabel{
		ScoredConflictID: conflictID,
		OrgID:            orgID,
		Label:            req.Label,
		LabeledBy:        claims.AgentID,
		LabeledAt:        time.Now(),
		Notes:            req.Notes,
	}
	if err := h.db.UpsertConflictLabel(r.Context(), cl); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, r, http.StatusConflict, map[string]string{"error": "conflict belongs to a different organization"})
			return
		}
		h.writeInternalError(w, r, "failed to upsert conflict label", err)
		return
	}

	writeJSON(w, r, http.StatusOK, toLabelResponse(cl))
}

// HandleGetConflictLabel handles GET /v1/admin/conflicts/{id}/label.
func (h *Handlers) HandleGetConflictLabel(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	conflictID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid conflict id"})
		return
	}

	cl, err := h.db.GetConflictLabel(r.Context(), conflictID, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, r, http.StatusNotFound, map[string]string{"error": "label not found"})
			return
		}
		h.writeInternalError(w, r, "failed to get conflict label", err)
		return
	}

	writeJSON(w, r, http.StatusOK, toLabelResponse(cl))
}

// HandleDeleteConflictLabel handles DELETE /v1/admin/conflicts/{id}/label.
func (h *Handlers) HandleDeleteConflictLabel(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	conflictID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid conflict id"})
		return
	}

	if err := h.db.DeleteConflictLabel(r.Context(), conflictID, orgID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, r, http.StatusNotFound, map[string]string{"error": "label not found"})
			return
		}
		h.writeInternalError(w, r, "failed to delete conflict label", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type listLabelsResponse struct {
	Labels []labelResponse             `json:"labels"`
	Counts storage.ConflictLabelCounts `json:"counts"`
}

// HandleListConflictLabels handles GET /v1/admin/conflict-labels.
func (h *Handlers) HandleListConflictLabels(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	labels, err := h.db.ListConflictLabels(r.Context(), orgID)
	if err != nil {
		h.writeInternalError(w, r, "failed to list conflict labels", err)
		return
	}

	counts, err := h.db.GetConflictLabelCounts(r.Context(), orgID)
	if err != nil {
		h.writeInternalError(w, r, "failed to get label counts", err)
		return
	}

	resp := listLabelsResponse{Counts: counts}
	resp.Labels = make([]labelResponse, 0, len(labels))
	for _, cl := range labels {
		resp.Labels = append(resp.Labels, toLabelResponse(cl))
	}

	writeJSON(w, r, http.StatusOK, resp)
}

type scorerEvalResponse struct {
	Precision      float64 `json:"precision"`
	TruePositives  int     `json:"true_positives"`
	FalsePositives int     `json:"false_positives"`
	TotalLabeled   int     `json:"total_labeled"`
	Message        string  `json:"message,omitempty"`
}

// HandleScorerEval handles POST /v1/admin/scorer-eval.
// Computes precision from ground truth labels on existing scored conflicts.
// Precision = genuine / (genuine + related_not_contradicting + unrelated_false_positive).
// All labeled conflicts were detected by the scorer, so every label contributes
// to precision: genuine = TP, everything else = FP.
func (h *Handlers) HandleScorerEval(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	counts, err := h.db.GetConflictLabelCounts(r.Context(), orgID)
	if err != nil {
		h.writeInternalError(w, r, "failed to get label counts", err)
		return
	}

	if counts.Total == 0 {
		writeJSON(w, r, http.StatusOK, scorerEvalResponse{
			Message: "no labeled conflicts — label some via PUT /v1/admin/conflicts/{id}/label first",
		})
		return
	}

	tp := counts.Genuine
	fp := counts.RelatedNotContradicting + counts.UnrelatedFalsePositive
	var precision float64
	if tp+fp > 0 {
		precision = float64(tp) / float64(tp+fp)
	}

	writeJSON(w, r, http.StatusOK, scorerEvalResponse{
		Precision:      precision,
		TruePositives:  tp,
		FalsePositives: fp,
		TotalLabeled:   counts.Total,
	})
}
