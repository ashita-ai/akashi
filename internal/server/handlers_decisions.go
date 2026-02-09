package server

import (
	"errors"
	"net/http"

	"github.com/ashita-ai/akashi/internal/billing"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/storage"
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

	if err := model.ValidateAgentID(req.AgentID); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
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

	// Verify the agent exists within the caller's org to prevent orphaned data.
	if _, err := h.db.GetAgentByAgentID(r.Context(), orgID, req.AgentID); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "agent_id not found in this organization")
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
		if errors.Is(err, billing.ErrQuotaExceeded) {
			writeError(w, r, http.StatusTooManyRequests, model.ErrCodeQuotaExceeded, err.Error())
			return
		}
		h.writeInternalError(w, r, "failed to create trace", err)
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

	decisions, _, err := h.decisionSvc.Query(r.Context(), orgID, req)
	if err != nil {
		h.writeInternalError(w, r, "query failed", err)
		return
	}

	decisions, err = filterDecisionsByAccess(r.Context(), h.db, claims, decisions)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"decisions": decisions,
		"total":     len(decisions),
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
		h.writeInternalError(w, r, "temporal query failed", err)
		return
	}

	decisions, err = filterDecisionsByAccess(r.Context(), h.db, claims, decisions)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
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
	if err := model.ValidateAgentID(agentID); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

	ok, err := canAccessAgent(r.Context(), h.db, claims, agentID)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
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
		h.writeInternalError(w, r, "failed to get history", err)
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

	results, err := h.decisionSvc.Search(r.Context(), orgID, req.Query, req.Semantic, req.Filters, req.Limit)
	if err != nil {
		h.writeInternalError(w, r, "search failed", err)
		return
	}

	results, err = filterSearchResultsByAccess(r.Context(), h.db, claims, results)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
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
		h.writeInternalError(w, r, "check failed", err)
		return
	}

	resp.Decisions, err = filterDecisionsByAccess(r.Context(), h.db, claims, resp.Decisions)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
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

	decisions, _, err := h.decisionSvc.Recent(r.Context(), orgID, filters, limit)
	if err != nil {
		h.writeInternalError(w, r, "query failed", err)
		return
	}

	decisions, err = filterDecisionsByAccess(r.Context(), h.db, claims, decisions)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"decisions": decisions,
		"total":     len(decisions),
		"count":     len(decisions),
		"limit":     limit,
	})
}

// HandleListConflicts handles GET /v1/conflicts.
func (h *Handlers) HandleListConflicts(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	filters := storage.ConflictFilters{}
	if dt := r.URL.Query().Get("decision_type"); dt != "" {
		filters.DecisionType = &dt
	}
	if aid := r.URL.Query().Get("agent_id"); aid != "" {
		filters.AgentID = &aid
	}
	limit := queryLimit(r, 25)
	offset := queryInt(r, "offset", 0)

	conflicts, err := h.db.ListConflicts(r.Context(), orgID, filters, limit, offset)
	if err != nil {
		h.writeInternalError(w, r, "failed to list conflicts", err)
		return
	}

	conflicts, err = filterConflictsByAccess(r.Context(), h.db, claims, conflicts)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"conflicts": conflicts,
		"total":     len(conflicts),
		"limit":     limit,
		"offset":    offset,
	})
}
