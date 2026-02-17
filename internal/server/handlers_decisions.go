package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/integrity"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/tracehealth"
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

	// Verify the agent exists within the caller's org, auto-registering if the
	// caller is admin+ and the agent is new (reduces friction for first-time traces).
	autoRegAudit := h.buildAuditEntry(r, orgID, "", "agent", req.AgentID, nil, nil, nil)
	if err := h.decisionSvc.ResolveOrCreateAgent(r.Context(), orgID, req.AgentID, claims.Role, &autoRegAudit); err != nil {
		if errors.Is(err, decisions.ErrAgentNotFound) {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
			return
		}
		h.writeInternalError(w, r, "failed to resolve agent", err)
		return
	}

	// Session from header.
	var sessionID *uuid.UUID
	sessionHeader := ""
	if sh := r.Header.Get("X-Akashi-Session"); sh != "" {
		sessionHeader = sh
		if sid, parseErr := uuid.Parse(sh); parseErr == nil {
			sessionID = &sid
		}
	}

	// Agent context from request body + headers.
	agentContext := map[string]any{}
	for k, v := range req.Context {
		agentContext[k] = v
	}

	// Tool from User-Agent header (SDKs send "akashi-go/0.1.0" etc).
	if ua := r.Header.Get("User-Agent"); ua != "" && strings.HasPrefix(ua, "akashi-") {
		parts := strings.SplitN(ua, "/", 2)
		agentContext["tool"] = parts[0]
		if len(parts) > 1 {
			agentContext["tool_version"] = parts[1]
		}
	}

	// Operator from JWT claims: use the agent's display name if distinct from agent_id.
	if claims != nil {
		agent, agentErr := h.db.GetAgentByAgentID(r.Context(), orgID, claims.AgentID)
		if agentErr == nil && agent.Name != "" && agent.Name != agent.AgentID {
			agentContext["operator"] = agent.Name
		}
	}

	idemPayload := struct {
		Request       model.TraceRequest `json:"request"`
		SessionHeader string             `json:"session_header,omitempty"`
		UserAgent     string             `json:"user_agent,omitempty"`
	}{
		Request:       req,
		SessionHeader: sessionHeader,
		UserAgent:     r.Header.Get("User-Agent"),
	}
	idem, proceed := h.beginIdempotentWrite(w, r, orgID, req.AgentID, "POST:/v1/trace", idemPayload)
	if !proceed {
		return
	}

	result, err := h.decisionSvc.Trace(r.Context(), orgID, decisions.TraceInput{
		AgentID:      req.AgentID,
		TraceID:      req.TraceID,
		Metadata:     req.Metadata,
		Decision:     req.Decision,
		PrecedentRef: req.PrecedentRef,
		SessionID:    sessionID,
		AgentContext: agentContext,
		APIKeyID:     claims.APIKeyID,
		AuditMeta:    h.buildAuditMeta(r, orgID),
	})
	if err != nil {
		h.clearIdempotentWrite(r, orgID, idem)
		h.writeInternalError(w, r, "failed to create trace", err)
		return
	}

	resp := map[string]any{
		"run_id":      result.RunID,
		"decision_id": result.DecisionID,
		"event_count": result.EventCount,
	}
	h.completeIdempotentWriteBestEffort(r, orgID, idem, http.StatusCreated, resp)
	writeJSON(w, r, http.StatusCreated, resp)
}

// HandleGetDecision handles GET /v1/decisions/{id} (reader+).
// Returns a single decision by UUID with alternatives and evidence.
func (h *Handlers) HandleGetDecision(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid decision ID")
		return
	}

	d, err := h.db.GetDecision(r.Context(), orgID, id, storage.GetDecisionOpts{
		IncludeAlts:     true,
		IncludeEvidence: true,
	})
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

	writeJSON(w, r, http.StatusOK, d)
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
	if req.Limit <= 0 {
		req.Limit = 50
	} else if req.Limit > maxQueryLimit {
		req.Limit = maxQueryLimit
	}
	if req.Offset < 0 {
		req.Offset = 0
	}
	if req.Offset > maxQueryOffset {
		req.Offset = maxQueryOffset
	}

	decisions, total, err := h.decisionSvc.Query(r.Context(), orgID, req)
	if err != nil {
		h.writeInternalError(w, r, "query failed", err)
		return
	}

	preFilterCount := len(decisions)
	decisions, err = filterDecisionsByAccess(r.Context(), h.db, claims, decisions, h.grantCache)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}

	resp := map[string]any{
		"decisions": decisions,
		"count":     len(decisions),
		"limit":     req.Limit,
		"offset":    req.Offset,
	}
	if len(decisions) < preFilterCount {
		// Access filtering is active — the DB total counted decisions the caller
		// can't see, so it's unknowable without scanning all pages. Omit total
		// and use has_more instead.
		resp["has_more"] = len(decisions) == req.Limit
	} else {
		resp["total"] = total
		resp["has_more"] = req.Offset+len(decisions) < total
	}
	writeJSON(w, r, http.StatusOK, resp)
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

	decisions, err := h.decisionSvc.QueryTemporal(r.Context(), orgID, req)
	if err != nil {
		h.writeInternalError(w, r, "temporal query failed", err)
		return
	}

	decisions, err = filterDecisionsByAccess(r.Context(), h.db, claims, decisions, h.grantCache)
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
	offset := queryOffset(r)
	from, err := queryTime(r, "from")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}
	to, err := queryTime(r, "to")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

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
		"has_more":  offset+len(decisions) < total,
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

	if req.Limit <= 0 || req.Limit > 1000 {
		req.Limit = 100
	}

	results, err := h.decisionSvc.Search(r.Context(), orgID, req.Query, req.Semantic, req.Filters, req.Limit)
	if err != nil {
		h.writeInternalError(w, r, "search failed", err)
		return
	}

	results, err = filterSearchResultsByAccess(r.Context(), h.db, claims, results, h.grantCache)
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

	resp.Decisions, err = filterDecisionsByAccess(r.Context(), h.db, claims, resp.Decisions, h.grantCache)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}
	resp.Conflicts, err = filterConflictsByAccess(r.Context(), h.db, claims, resp.Conflicts, h.grantCache)
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
	offset := queryOffset(r)

	filters := model.QueryFilters{}
	if agentID := r.URL.Query().Get("agent_id"); agentID != "" {
		filters.AgentIDs = []string{agentID}
	}
	if dt := r.URL.Query().Get("decision_type"); dt != "" {
		filters.DecisionType = &dt
	}

	decisions, total, err := h.decisionSvc.Recent(r.Context(), orgID, filters, limit, offset)
	if err != nil {
		h.writeInternalError(w, r, "query failed", err)
		return
	}

	preFilterCount := len(decisions)
	decisions, err = filterDecisionsByAccess(r.Context(), h.db, claims, decisions, h.grantCache)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}

	resp := map[string]any{
		"decisions": decisions,
		"count":     len(decisions),
		"limit":     limit,
		"offset":    offset,
	}
	if len(decisions) < preFilterCount {
		resp["has_more"] = len(decisions) == limit
	} else {
		resp["total"] = total
		resp["has_more"] = offset+len(decisions) < total
	}
	writeJSON(w, r, http.StatusOK, resp)
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
	if ck := r.URL.Query().Get("conflict_kind"); ck != "" {
		filters.ConflictKind = &ck
	}
	if sev := r.URL.Query().Get("severity"); sev != "" {
		filters.Severity = &sev
	}
	if cat := r.URL.Query().Get("category"); cat != "" {
		filters.Category = &cat
	}
	// Default to open conflicts unless explicitly overridden.
	if st := r.URL.Query().Get("status"); st != "" {
		filters.Status = &st
	}
	limit := queryLimit(r, 25)
	offset := queryOffset(r)

	total, err := h.db.CountConflicts(r.Context(), orgID, filters)
	if err != nil {
		h.writeInternalError(w, r, "failed to count conflicts", err)
		return
	}

	conflicts, err := h.db.ListConflicts(r.Context(), orgID, filters, limit, offset)
	if err != nil {
		h.writeInternalError(w, r, "failed to list conflicts", err)
		return
	}

	conflicts, err = filterConflictsByAccess(r.Context(), h.db, claims, conflicts, h.grantCache)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}

	// Ensure JSON array, not null.
	if conflicts == nil {
		conflicts = []model.DecisionConflict{}
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"conflicts": conflicts,
		"total":     total,
		"count":     len(conflicts),
		"limit":     limit,
		"offset":    offset,
		"has_more":  offset+len(conflicts) < total,
	})
}

// validConflictStatuses defines the allowed values for conflict status transitions.
var validConflictStatuses = map[string]bool{
	"acknowledged": true,
	"resolved":     true,
	"wont_fix":     true,
}

// HandlePatchConflict handles PATCH /v1/conflicts/{id}.
// Transitions a conflict to a new lifecycle state.
func (h *Handlers) HandlePatchConflict(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid conflict id")
		return
	}

	var req model.ConflictResolution
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	if !validConflictStatuses[req.Status] {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
			"status must be one of: acknowledged, resolved, wont_fix")
		return
	}

	resolvedBy := claims.AgentID
	if resolvedBy == "" {
		resolvedBy = claims.Subject
	}

	audit := h.buildAuditEntry(r, orgID,
		"conflict_status_changed", "conflict", id.String(),
		nil, nil,
		map[string]any{"new_status": req.Status, "resolved_by": resolvedBy},
	)
	if _, err := h.db.UpdateConflictStatusWithAudit(r.Context(), id, orgID, req.Status, resolvedBy, req.ResolutionNote, audit); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "conflict not found")
			return
		}
		h.writeInternalError(w, r, "failed to update conflict", err)
		return
	}

	conflict, err := h.db.GetConflict(r.Context(), id, orgID)
	if err != nil || conflict == nil {
		// Update succeeded but re-fetch failed — return 204 rather than error.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writeJSON(w, r, http.StatusOK, conflict)
}

// HandleResolveConflict handles POST /v1/conflicts/{id}/resolve.
// Creates a resolution decision trace and links it to the conflict.
func (h *Handlers) HandleResolveConflict(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid conflict id")
		return
	}

	var req struct {
		Outcome      string  `json:"outcome"`
		Reasoning    *string `json:"reasoning,omitempty"`
		DecisionType string  `json:"decision_type,omitempty"`
	}
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}
	if req.Outcome == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "outcome is required")
		return
	}
	if req.DecisionType == "" {
		req.DecisionType = "conflict_resolution"
	}

	// Verify the conflict exists and belongs to this org.
	conflict, err := h.db.GetConflict(r.Context(), id, orgID)
	if err != nil || conflict == nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "conflict not found")
		return
	}

	resolverAgent := claims.AgentID
	if resolverAgent == "" {
		resolverAgent = claims.Subject
	}

	// Ensure the resolver agent exists (auto-create if admin+).
	autoRegAudit := h.buildAuditEntry(r, orgID, "", "agent", resolverAgent, nil, nil, nil)
	if err := h.decisionSvc.ResolveOrCreateAgent(r.Context(), orgID, resolverAgent, claims.Role, &autoRegAudit); err != nil {
		if errors.Is(err, decisions.ErrAgentNotFound) {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
			return
		}
		h.writeInternalError(w, r, "failed to resolve agent", err)
		return
	}

	// Create a resolution decision trace AND resolve the conflict atomically.
	// A single transaction prevents the failure mode where a resolution decision
	// exists but the conflict remains unresolved.
	note := "Resolved by decision trace"
	conflictAudit := h.buildAuditEntry(r, orgID,
		"conflict_resolved_with_decision", "conflict", id.String(),
		nil, nil,
		map[string]any{"resolved_by": resolverAgent},
	)
	result, err := h.decisionSvc.ResolveConflictWithTrace(r.Context(), orgID, decisions.TraceInput{
		AgentID: resolverAgent,
		Decision: model.TraceDecision{
			DecisionType: req.DecisionType,
			Outcome:      req.Outcome,
			Confidence:   1.0, // Resolution decisions are definitive.
			Reasoning:    req.Reasoning,
		},
		APIKeyID:  claims.APIKeyID,
		AuditMeta: h.buildAuditMeta(r, orgID),
	}, storage.ResolveConflictInTraceParams{
		ConflictID: id,
		ResolvedBy: resolverAgent,
		ResNote:    &note,
		Audit:      conflictAudit,
	})
	if err != nil {
		if strings.Contains(err.Error(), "conflict not found") {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "conflict not found")
			return
		}
		h.writeInternalError(w, r, "failed to resolve conflict", err)
		return
	}

	// Return the updated conflict.
	updated, err := h.db.GetConflict(r.Context(), id, orgID)
	if err != nil || updated == nil {
		// Resolution succeeded but re-fetch failed — return decision info.
		writeJSON(w, r, http.StatusOK, map[string]any{
			"conflict_id": id,
			"decision_id": result.DecisionID,
			"status":      "resolved",
		})
		return
	}

	writeJSON(w, r, http.StatusOK, updated)
}

// HandleDecisionRevisions handles GET /v1/decisions/{id}/revisions.
// Returns the full revision chain for a decision (all versions, ordered by valid_from).
func (h *Handlers) HandleDecisionRevisions(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid decision ID")
		return
	}

	revisions, err := h.db.GetDecisionRevisions(r.Context(), orgID, id)
	if err != nil {
		h.writeInternalError(w, r, "failed to get revisions", err)
		return
	}

	revisions, err = filterDecisionsByAccess(r.Context(), h.db, claims, revisions, h.grantCache)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"decision_id": id,
		"revisions":   revisions,
		"count":       len(revisions),
	})
}

// HandleVerifyDecision handles GET /v1/verify/{id}.
// Recomputes the SHA-256 content hash from stored fields and compares to the stored hash.
func (h *Handlers) HandleVerifyDecision(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid decision ID")
		return
	}

	claims := ClaimsFromContext(r.Context())

	d, err := h.db.GetDecision(r.Context(), orgID, id, storage.GetDecisionOpts{})
	if err != nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "decision not found")
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

	resp := map[string]any{"decision_id": id}

	if d.ContentHash == "" {
		// Pre-migration decisions have no hash — don't report them as tampered.
		resp["status"] = "no_hash"
		resp["message"] = "this decision was created before content hashing was enabled"
	} else {
		valid := integrity.VerifyContentHash(d.ContentHash, d.ID, d.DecisionType, d.Outcome, d.Confidence, d.Reasoning, d.ValidFrom)
		resp["valid"] = valid
		if valid {
			resp["status"] = "verified"
		} else {
			resp["status"] = "tampered"
		}
		resp["content_hash"] = d.ContentHash
	}

	writeJSON(w, r, http.StatusOK, resp)
}

// HandleTraceHealth handles GET /v1/trace-health.
// Returns aggregate health metrics for the caller's organization.
func (h *Handlers) HandleTraceHealth(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	svc := tracehealth.New(h.db)
	metrics, err := svc.Compute(r.Context(), orgID)
	if err != nil {
		h.writeInternalError(w, r, "failed to compute trace health", err)
		return
	}

	writeJSON(w, r, http.StatusOK, metrics)
}

// HandleSessionView handles GET /v1/sessions/{session_id}.
// Returns all decisions from a given MCP/HTTP session, with summary statistics.
func (h *Handlers) HandleSessionView(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	sidStr := r.PathValue("session_id")
	sid, err := uuid.Parse(sidStr)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid session_id")
		return
	}

	decs, err := h.db.GetSessionDecisions(r.Context(), orgID, sid)
	if err != nil {
		h.writeInternalError(w, r, "failed to get session decisions", err)
		return
	}

	decs, err = filterDecisionsByAccess(r.Context(), h.db, claims, decs, h.grantCache)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}

	if len(decs) == 0 {
		writeJSON(w, r, http.StatusOK, map[string]any{
			"session_id":     sid,
			"decisions":      []any{},
			"decision_count": 0,
		})
		return
	}

	// Compute summary: use min/max of valid_from to avoid ordering edge cases
	// (multiple decisions can share the same valid_from in revision chains).
	startedAt := decs[0].ValidFrom
	endedAt := decs[0].ValidFrom
	for _, d := range decs[1:] {
		if d.ValidFrom.Before(startedAt) {
			startedAt = d.ValidFrom
		}
		if d.ValidFrom.After(endedAt) {
			endedAt = d.ValidFrom
		}
	}
	duration := endedAt.Sub(startedAt).Seconds()
	if duration < 0 {
		duration = 0
	}

	decisionTypes := map[string]int{}
	var totalConf float64
	for _, d := range decs {
		decisionTypes[d.DecisionType]++
		totalConf += float64(d.Confidence)
	}
	avgConfidence := totalConf / float64(len(decs))

	writeJSON(w, r, http.StatusOK, map[string]any{
		"session_id":     sid,
		"decisions":      decs,
		"decision_count": len(decs),
		"summary": map[string]any{
			"started_at":     startedAt,
			"ended_at":       endedAt,
			"duration_secs":  duration,
			"decision_types": decisionTypes,
			"avg_confidence": avgConfidence,
		},
	})
}

// HandleDecisionConflicts handles GET /v1/decisions/{id}/conflicts.
// Returns all conflicts involving a specific decision (as A or B side).
func (h *Handlers) HandleDecisionConflicts(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	idStr := r.PathValue("id")
	decisionID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_INPUT", "invalid decision ID")
		return
	}

	filters := storage.ConflictFilters{DecisionID: &decisionID}
	if st := r.URL.Query().Get("status"); st != "" {
		filters.Status = &st
	}

	conflicts, err := h.db.ListConflicts(r.Context(), orgID, filters, 100, 0)
	if err != nil {
		h.writeInternalError(w, r, "failed to list decision conflicts", err)
		return
	}

	conflicts, err = filterConflictsByAccess(r.Context(), h.db, claims, conflicts, h.grantCache)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}

	if conflicts == nil {
		conflicts = []model.DecisionConflict{}
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"decision_id": decisionID,
		"conflicts":   conflicts,
		"total":       len(conflicts),
	})
}
