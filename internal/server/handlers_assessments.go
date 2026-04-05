package server

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/quality"
	"github.com/ashita-ai/akashi/internal/storage"
)

// HandleAssessDecision handles POST /v1/decisions/{id}/assess (writer+).
// Records an outcome assessment for a decision. Assessments are append-only:
// each call creates a new row. An assessor changing their verdict over time
// is itself an auditable event — prior assessments are never overwritten.
// GetAssessmentSummary uses DISTINCT ON to count only each assessor's latest
// verdict when computing summary statistics.
func (h *Handlers) HandleAssessDecision(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	decisionID, err := parsePathUUID(r, "id")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid decision ID")
		return
	}

	// Verify the caller has access to the decision's agent before allowing assessment.
	d, err := h.db.GetDecision(r.Context(), orgID, decisionID, storage.GetDecisionOpts{})
	if err != nil {
		if isNotFoundError(err) {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "decision not found")
			return
		}
		h.writeInternalError(w, r, "failed to get decision", err)
		return
	}
	ok, err := canAccessAgent(r.Context(), h.db, claims, d.AgentID)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}
	if !ok {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "no access to this decision")
		return
	}

	var req model.AssessRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
		return
	}

	switch req.Outcome {
	case model.AssessmentCorrect, model.AssessmentIncorrect, model.AssessmentPartiallyCorrect:
		// valid
	default:
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
			"outcome must be one of: correct, incorrect, partially_correct")
		return
	}

	a := model.DecisionAssessment{
		DecisionID:      decisionID,
		OrgID:           orgID,
		AssessorAgentID: claims.AgentID,
		Outcome:         req.Outcome,
		Notes:           req.Notes,
	}

	result, err := h.db.CreateAssessment(r.Context(), orgID, a)
	if err != nil {
		if isNotFoundError(err) {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "decision not found")
			return
		}
		h.writeInternalError(w, r, "failed to save assessment", err)
		return
	}

	// Recompute outcome_score from the latest assessment summary.
	summary, err := h.db.GetAssessmentSummaryBatch(r.Context(), orgID, []uuid.UUID{decisionID})
	if err == nil {
		if s, ok := summary[decisionID]; ok {
			outcomeScore := quality.ComputeOutcomeScore(s)
			if updateErr := h.db.UpdateOutcomeScore(r.Context(), orgID, decisionID, outcomeScore); updateErr != nil {
				h.logger.Error("failed to update outcome score", "decision_id", decisionID, "err", updateErr)
			}
		}
	}

	_ = h.db.Notify(r.Context(), storage.ChannelDecisions,
		`{"source":"assess","decision_id":"`+decisionID.String()+`","org_id":"`+orgID.String()+`"}`)

	writeJSON(w, r, http.StatusOK, result)
}

// HandleListAssessments handles GET /v1/decisions/{id}/assessments (reader+).
// Returns all assessments for a decision, newest first.
func (h *Handlers) HandleListAssessments(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	decisionID, err := parsePathUUID(r, "id")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid decision ID")
		return
	}

	// Verify access to the decision itself before returning its assessments.
	d, err := h.db.GetDecision(r.Context(), orgID, decisionID, storage.GetDecisionOpts{})
	if err != nil {
		if isNotFoundError(err) {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "decision not found")
			return
		}
		h.writeInternalError(w, r, "failed to get decision", err)
		return
	}
	ok, err := canAccessAgent(r.Context(), h.db, claims, d.AgentID)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}
	if !ok {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "no access to this decision")
		return
	}

	assessments, err := h.db.ListAssessments(r.Context(), orgID, decisionID)
	if err != nil {
		h.writeInternalError(w, r, "failed to list assessments", err)
		return
	}

	summary, err := h.db.GetAssessmentSummary(r.Context(), orgID, decisionID)
	if err != nil {
		h.writeInternalError(w, r, "failed to get assessment summary", err)
		return
	}

	writeJSON(w, r, http.StatusOK, model.AssessmentListResponse{
		DecisionID:  decisionID,
		Summary:     summary,
		Assessments: assessments,
		Count:       len(assessments),
	})
}
