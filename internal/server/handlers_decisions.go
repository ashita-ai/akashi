package server

import (
	"net/http"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/decisions"
)

// HandleTrace handles POST /v1/trace (convenience endpoint).
func (h *Handlers) HandleTrace(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	var req model.TraceRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	if req.AgentID == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "agent_id is required")
		return
	}
	if req.Decision.DecisionType == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "decision.decision_type is required")
		return
	}
	if req.Decision.Outcome == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "decision.outcome is required")
		return
	}
	if req.Decision.Confidence < 0 || req.Decision.Confidence > 1 {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "decision.confidence must be between 0 and 1")
		return
	}

	if !model.RoleAtLeast(claims.Role, model.RoleAdmin) && req.AgentID != claims.AgentID {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "can only trace for your own agent_id")
		return
	}

	result, err := h.decisionSvc.Trace(r.Context(), orgID, decisions.TraceInput{
		AgentID:      req.AgentID,
		TraceID:      req.TraceID,
		Metadata:     req.Metadata,
		Decision:     req.Decision,
		PrecedentRef: req.PrecedentRef,
	})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to create trace")
		return
	}

	writeJSON(w, r, http.StatusCreated, map[string]any{
		"run_id":      result.RunID,
		"decision_id": result.DecisionID,
		"event_count": result.EventCount,
	})
}

// HandleQuery handles POST /v1/query.
func (h *Handlers) HandleQuery(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	var req model.QueryRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	decisions, total, err := h.decisionSvc.Query(r.Context(), orgID, req)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "query failed")
		return
	}

	decisions, err = filterDecisionsByAccess(r.Context(), h.db, claims, decisions)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "authorization check failed")
		return
	}

	// Note: total reflects DB count before access filtering.
	// For non-admin users, the actual accessible count may be lower.
	writeJSON(w, r, http.StatusOK, map[string]any{
		"decisions": decisions,
		"total":     total,
		"count":     len(decisions),
		"limit":     req.Limit,
		"offset":    req.Offset,
	})
}

// HandleTemporalQuery handles POST /v1/query/temporal.
func (h *Handlers) HandleTemporalQuery(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	var req model.TemporalQueryRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	decisions, err := h.db.QueryDecisionsTemporal(r.Context(), orgID, req)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "temporal query failed")
		return
	}

	decisions, err = filterDecisionsByAccess(r.Context(), h.db, claims, decisions)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "authorization check failed")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"as_of":     req.AsOf,
		"decisions": decisions,
	})
}

// HandleAgentHistory handles GET /v1/agents/{agent_id}/history.
func (h *Handlers) HandleAgentHistory(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())
	agentID := r.PathValue("agent_id")
	if agentID == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "agent_id is required")
		return
	}

	ok, err := canAccessAgent(r.Context(), h.db, claims, agentID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "authorization check failed")
		return
	}
	if !ok {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "no access to this agent's history")
		return
	}

	limit := queryLimit(r, 50)
	offset := queryInt(r, "offset", 0)
	from := queryTime(r, "from")
	to := queryTime(r, "to")

	decisions, total, err := h.db.GetDecisionsByAgent(r.Context(), orgID, agentID, limit, offset, from, to)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to get history")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"agent_id":  agentID,
		"decisions": decisions,
		"total":     total,
		"limit":     limit,
		"offset":    offset,
	})
}

// HandleSearch handles POST /v1/search.
func (h *Handlers) HandleSearch(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	var req model.SearchRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	if req.Query == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "query is required")
		return
	}

	results, err := h.decisionSvc.Search(r.Context(), orgID, req.Query, req.Filters, req.Limit)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "search failed")
		return
	}

	results, err = filterSearchResultsByAccess(r.Context(), h.db, claims, results)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "authorization check failed")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"results": results,
		"total":   len(results),
	})
}

// HandleCheck handles POST /v1/check.
func (h *Handlers) HandleCheck(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	var req model.CheckRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	if req.DecisionType == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "decision_type is required")
		return
	}

	resp, err := h.decisionSvc.Check(r.Context(), orgID, req.DecisionType, req.Query, req.AgentID, req.Limit)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "check failed")
		return
	}

	resp.Decisions, err = filterDecisionsByAccess(r.Context(), h.db, claims, resp.Decisions)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "authorization check failed")
		return
	}
	resp.HasPrecedent = len(resp.Decisions) > 0

	writeJSON(w, r, http.StatusOK, resp)
}

// HandleDecisionsRecent handles GET /v1/decisions/recent.
func (h *Handlers) HandleDecisionsRecent(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())
	limit := queryLimit(r, 10)

	filters := model.QueryFilters{}
	if agentID := r.URL.Query().Get("agent_id"); agentID != "" {
		filters.AgentIDs = []string{agentID}
	}
	if dt := r.URL.Query().Get("decision_type"); dt != "" {
		filters.DecisionType = &dt
	}

	decisions, total, err := h.decisionSvc.Recent(r.Context(), orgID, filters, limit)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "query failed")
		return
	}

	decisions, err = filterDecisionsByAccess(r.Context(), h.db, claims, decisions)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "authorization check failed")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"decisions": decisions,
		"total":     total,
		"count":     len(decisions),
		"limit":     limit,
	})
}

// HandleListConflicts handles GET /v1/conflicts.
func (h *Handlers) HandleListConflicts(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	var decisionType *string
	if dt := r.URL.Query().Get("decision_type"); dt != "" {
		decisionType = &dt
	}
	limit := queryLimit(r, 50)

	conflicts, err := h.db.ListConflicts(r.Context(), orgID, decisionType, limit)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to list conflicts")
		return
	}

	conflicts, err = filterConflictsByAccess(r.Context(), h.db, claims, conflicts)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "authorization check failed")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"conflicts": conflicts,
		"total":     len(conflicts),
	})
}
