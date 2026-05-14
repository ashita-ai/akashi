package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/authz"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/pendingassess"
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

// HandleListPendingAssessments handles GET /v1/decisions/pending-assessment (reader+).
//
// Surfaces decisions that are past their per-type assessment window and have
// no recorded assessment from any source (manual or auto). Default scope is
// the caller's own agent_id — the issue #716 use case is "agents follow up
// on decisions they traced." Pass agent_id=* to expand to all accessible
// agents (subject to authz filtering).
//
// Query params:
//   - agent_id      Optional. Defaults to caller. Pass "*" for org-wide
//     (still filtered by the caller's grant set).
//   - decision_type Optional. Narrows to a single type. Returns empty if
//     the type has no configured window (i.e. opted out).
//   - project       Optional. Restricts to one project.
//   - limit         Optional. Clamped to [1, 100]; default from config.
func (h *Handlers) HandleListPendingAssessments(w http.ResponseWriter, r *http.Request) {
	if h.pendingAssessSvc == nil {
		// Route should not be registered when the service is nil — defensive
		// guard in case wiring changes in the future.
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "pending-assessment is not configured on this server")
		return
	}
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	q := r.URL.Query()
	requested := q.Get("agent_id")
	decisionType := strings.ToLower(strings.TrimSpace(q.Get("decision_type")))
	project := q.Get("project")
	// Parse limit directly rather than via queryLimit: a missing param must
	// flow through as 0 so the service applies its configured default. The
	// shared queryLimit helper clamps 0→1, which would silently cap MCP/HTTP
	// callers at one result whenever they omit limit.
	limit := queryInt(r, "limit", 0)
	if limit < 0 {
		limit = 0
	}
	if limit > 100 {
		limit = 100
	}

	agentIDs, err := resolvePendingAssessAgentScope(r.Context(), h.db, claims, h.grantCache, requested)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}

	in := pendingassess.ListInput{
		AgentIDs:     agentIDs,
		DecisionType: decisionType,
		Limit:        limit,
	}
	if project != "" {
		in.Project = &project
	}

	rows, err := h.pendingAssessSvc.ListPending(r.Context(), orgID, in)
	if err != nil {
		h.writeInternalError(w, r, "failed to list pending assessments", err)
		return
	}
	if rows == nil {
		rows = []model.PendingAssessment{}
	}
	writeJSON(w, r, http.StatusOK, model.PendingAssessmentListResponse{
		Decisions: rows,
		Count:     len(rows),
	})
}

// resolvePendingAssessAgentScope translates the agent_id query parameter into
// the agent-set passed to storage.
//
// Behavior:
//   - empty (default)   → caller's own agent_id only
//   - "*"               → all agents the caller can access. For admin+ this
//     returns nil (no filter); for others, the granted set
//   - "<specific>"      → that agent if the caller can access it; empty slice
//     otherwise (which yields zero rows without DB churn)
//
// Returning ([], nil) is the deny-all signal; returning (nil, nil) is the
// unrestricted signal — same contract as authz.LoadGrantedSet.
func resolvePendingAssessAgentScope(ctx context.Context, db storage.Store, claims *auth.Claims, cache *authz.GrantCache, requested string) ([]string, error) {
	if claims == nil {
		return []string{}, nil
	}
	if requested == "" {
		return []string{claims.AgentID}, nil
	}
	// "*" and a specific id both require the grant set, so load once.
	granted, err := authz.LoadGrantedSet(ctx, db, claims, cache)
	if err != nil {
		return nil, err
	}
	if requested == "*" {
		if granted == nil {
			return nil, nil // admin: unrestricted
		}
		ids := make([]string, 0, len(granted))
		for id := range granted {
			ids = append(ids, id)
		}
		return ids, nil
	}
	// Specific agent_id.
	if granted == nil || granted[requested] {
		return []string{requested}, nil
	}
	return []string{}, nil
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
