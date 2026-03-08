package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/auth"
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
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
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
	if err := model.ValidateTraceDecision(req.Decision); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

	if !model.RoleAtLeast(claims.Role, model.RoleAdmin) && req.AgentID != claims.AgentID {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "can only trace for your own agent_id")
		return
	}

	// Verify the agent exists within the caller's org, auto-registering if the
	// caller is admin+ and the agent is new (reduces friction for first-time traces).
	// The returned agent is reused below for operator enrichment, avoiding a second DB fetch.
	autoRegAudit := h.buildAuditEntry(r, orgID, "", "agent", req.AgentID, nil, nil, nil)
	resolvedAgent, err := h.decisionSvc.ResolveOrCreateAgent(r.Context(), orgID, req.AgentID, claims.Role, &autoRegAudit)
	if err != nil {
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

	agentContext := h.buildTraceAgentContext(r, orgID, claims, req, resolvedAgent)

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

	// Fire OnDecisionTraced hooks asynchronously. Hook failures are logged
	// but never fail the request — the decision is already durably stored.
	if len(h.decisionHooks) > 0 {
		decision := result.Decision
		hooks := h.decisionHooks
		logger := h.logger
		go func() {
			hookCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, hook := range hooks {
				if err := hook.OnDecisionTraced(hookCtx, decision); err != nil {
					logger.Warn("event hook OnDecisionTraced failed", "error", err)
				}
			}
		}()
	}

	resp := map[string]any{
		"run_id":      result.RunID,
		"decision_id": result.DecisionID,
		"event_count": result.EventCount,
	}
	h.completeIdempotentWriteBestEffort(r, orgID, idem, http.StatusCreated, resp)
	writeJSON(w, r, http.StatusCreated, resp)
}

// buildTraceAgentContext constructs the namespaced agent_context map for a
// trace request. It merges three sources:
//   - "client": caller-supplied context from the request body (self-reported).
//   - "server": server-verified values extracted from HTTP headers and JWT claims
//     (tool name from User-Agent, API key prefix, operator display name).
//
// The server namespace is omitted when empty so traces from plain HTTP callers
// (no User-Agent prefix, no API key, anonymous context) produce a compact JSON payload.
func (h *Handlers) buildTraceAgentContext(
	r *http.Request,
	orgID uuid.UUID,
	claims *auth.Claims,
	req model.TraceRequest,
	resolvedAgent model.Agent,
) map[string]any {
	serverCtx := map[string]any{}
	clientCtx := map[string]any{}

	// Client-reported context from request body (backward compat: flat → client).
	for k, v := range req.Context {
		clientCtx[k] = v
	}

	// Tool from User-Agent header (SDKs send "akashi-go/0.1.0" etc).
	if ua := r.Header.Get("User-Agent"); ua != "" && strings.HasPrefix(ua, "akashi-") {
		parts := strings.SplitN(ua, "/", 2)
		serverCtx["tool"] = parts[0]
		if len(parts) > 1 {
			serverCtx["tool_version"] = parts[1]
		}
	}

	// API key prefix for server-verified attribution.
	if claims.APIKeyID != nil {
		key, keyErr := h.db.GetAPIKeyByID(r.Context(), orgID, *claims.APIKeyID)
		if keyErr == nil && key.Prefix != "" {
			serverCtx["api_key_prefix"] = key.Prefix
		}
	}

	// Operator: the agent's display name when it differs from agent_id.
	// If the calling agent is the same as the traced agent, reuse the already-fetched
	// record. Otherwise fetch the caller separately (admin tracing on behalf of another agent).
	if claims != nil {
		var callerAgent model.Agent
		if claims.AgentID == req.AgentID {
			callerAgent = resolvedAgent
		} else {
			callerAgent, _ = h.db.GetAgentByAgentID(r.Context(), orgID, claims.AgentID)
		}
		if callerAgent.Name != "" && callerAgent.Name != callerAgent.AgentID {
			clientCtx["operator"] = callerAgent.Name
		}
	}

	agentContext := map[string]any{}
	if len(serverCtx) > 0 {
		agentContext["server"] = serverCtx
	}
	if len(clientCtx) > 0 {
		agentContext["client"] = clientCtx
	}
	return agentContext
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

	// Populate consensus scores and outcome signals (computed at query time, not stored).
	agreementCount, conflictCount, err := h.decisionSvc.ConsensusScores(r.Context(), d.ID, orgID)
	if err == nil {
		d.AgreementCount = agreementCount
		d.ConflictCount = conflictCount
	}

	signals, err := h.db.GetDecisionOutcomeSignals(r.Context(), d.ID, orgID)
	if err == nil {
		d.SupersessionVelocityHours = signals.SupersessionVelocityHours
		d.PrecedentCitationCount = signals.PrecedentCitationCount
		d.ConflictFate = signals.ConflictFate
	}

	summary, err := h.db.GetAssessmentSummary(r.Context(), orgID, d.ID)
	if err == nil {
		d.AssessmentSummary = &summary
	}

	writeJSON(w, r, http.StatusOK, d)
}

// HandleQuery handles POST /v1/query.
func (h *Handlers) HandleQuery(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	var req model.QueryRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
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

	ptotal, hasMore := computePagination(len(decisions), preFilterCount, req.Limit, req.Offset, total)
	writeListJSON(w, r, decisions, ptotal, hasMore, req.Limit, req.Offset)
}

// HandleTemporalQuery handles POST /v1/query/temporal.
func (h *Handlers) HandleTemporalQuery(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	var req model.TemporalQueryRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
		return
	}

	// Reject future timestamps with a 1-minute tolerance for clock skew.
	// A future as_of produces empty or misleading results with no signal to the caller.
	if req.AsOf.After(time.Now().Add(time.Minute)) {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "as_of must not be in the future")
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

	ptotal := total
	writeListJSON(w, r, decisions, &ptotal, offset+len(decisions) < total, limit, offset)
}

// HandleSearch handles POST /v1/search.
func (h *Handlers) HandleSearch(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	var req model.SearchRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
		return
	}

	if req.Query == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "query is required")
		return
	}

	if req.Limit <= 0 || req.Limit > 1000 {
		req.Limit = 100
	}

	// Detect whether Qdrant is reachable before the search. If the searcher is
	// absent or unhealthy, the service falls back to text search — we signal this
	// to the caller via X-Search-Backend so they know results are not semantic.
	searchBackend := "qdrant"
	if h.searcher == nil || h.searcher.Healthy(r.Context()) != nil {
		searchBackend = "text"
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

	total := len(results)
	w.Header().Set("X-Search-Backend", searchBackend)
	writeListJSON(w, r, results, &total, false, len(results), 0)
}

// HandleCheck handles POST /v1/check.
func (h *Handlers) HandleCheck(w http.ResponseWriter, r *http.Request) {
	// Record that akashi_check was called so the IDE hook gate (PreToolUse for
	// Edit/Write) can confirm a check happened before edits. Recorded here
	// rather than in the PostToolUse hook because Claude Code does not reliably
	// fire PostToolUse hooks for MCP tool calls.
	h.hookChecks.Record("")
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	var req model.CheckRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
		return
	}

	if req.DecisionType == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "decision_type is required")
		return
	}

	resp, err := h.decisionSvc.Check(r.Context(), orgID, decisions.CheckInput{
		DecisionType: req.DecisionType,
		Query:        req.Query,
		AgentID:      req.AgentID,
		Project:      req.Project,
		Limit:        req.Limit,
	})
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

	ptotal, hasMore := computePagination(len(decisions), preFilterCount, limit, offset, total)
	writeListJSON(w, r, decisions, ptotal, hasMore, limit, offset)
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

	switch {
	case d.ValidTo != nil:
		// Decision was retracted — still verify hash integrity but report retracted status.
		resp["status"] = "retracted"
		resp["retracted_at"] = d.ValidTo.UTC().Format(time.RFC3339Nano)
		if d.ContentHash == "" {
			resp["verified"] = false
			resp["message"] = "this decision was created before content hashing was enabled"
		} else {
			valid := integrity.VerifyContentHash(d.ContentHash, d.ID, d.DecisionType, d.Outcome, d.Confidence, d.Reasoning, d.ValidFrom)
			resp["verified"] = valid
			resp["content_hash"] = d.ContentHash
		}
	case d.ContentHash == "":
		// Pre-migration decisions have no hash — don't report them as tampered.
		resp["status"] = "no_hash"
		resp["message"] = "this decision was created before content hashing was enabled"
	default:
		// Check for GDPR erasure before standard verification.
		erasure, erasureErr := h.db.GetDecisionErasure(r.Context(), orgID, id)
		switch {
		case erasureErr == nil:
			// Decision has been erased — verify the erased hash matches.
			valid := integrity.VerifyContentHash(d.ContentHash, d.ID, d.DecisionType, d.Outcome, d.Confidence, d.Reasoning, d.ValidFrom)
			resp["status"] = "erased"
			resp["valid"] = valid
			resp["content_hash"] = d.ContentHash
			resp["original_hash"] = erasure.OriginalHash
			resp["erased_at"] = erasure.ErasedAt
			resp["erased_by"] = erasure.ErasedBy
		case !isNotFoundError(erasureErr):
			h.writeInternalError(w, r, "failed to check erasure status", erasureErr)
			return
		default:
			// No erasure — standard verification.
			valid := integrity.VerifyContentHash(d.ContentHash, d.ID, d.DecisionType, d.Outcome, d.Confidence, d.Reasoning, d.ValidFrom)
			resp["valid"] = valid
			if valid {
				resp["status"] = "verified"
			} else {
				resp["status"] = "tampered"
			}
			resp["content_hash"] = d.ContentHash
		}
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

// HandleRetractDecision handles DELETE /v1/decisions/{id}.
// Soft-deletes a decision by setting valid_to and recording a DecisionRetracted event.
func (h *Handlers) HandleRetractDecision(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid decision id")
		return
	}

	// Decode optional body with reason.
	var req struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
			handleDecodeError(w, r, err)
			return
		}
	}

	claims := ClaimsFromContext(r.Context())
	retractedBy := claims.AgentID
	if retractedBy == "" {
		retractedBy = claims.Subject
	}

	audit := h.buildAuditEntry(r, orgID,
		"decision_retracted", "decision", id.String(),
		nil, nil,
		map[string]any{"retracted_by": retractedBy, "reason": req.Reason},
	)
	if err := h.db.RetractDecision(r.Context(), orgID, id, req.Reason, retractedBy, &audit); err != nil {
		if isNotFoundError(err) {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "decision not found")
			return
		}
		h.writeInternalError(w, r, "failed to retract decision", err)
		return
	}

	// Return the retracted decision (with valid_to set).
	decision, err := h.db.GetDecision(r.Context(), orgID, id, storage.GetDecisionOpts{CurrentOnly: false})
	if err != nil {
		// Retraction succeeded but re-fetch failed — return 204 rather than error.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writeJSON(w, r, http.StatusOK, decision)
}

// HandleEraseDecision handles POST /v1/decisions/{id}/erase.
// GDPR Art. 17 tombstone erasure: scrubs PII fields in-place without deleting
// the decision row, preserving the audit chain. Requires org_owner role.
func (h *Handlers) HandleEraseDecision(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid decision id")
		return
	}

	// Decode optional body with reason.
	var req struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
			handleDecodeError(w, r, err)
			return
		}
	}

	// Block erasure if an active legal hold covers this decision.
	holdActive, err := h.db.ActiveHoldsExistForDecision(r.Context(), orgID, id)
	if err != nil {
		h.writeInternalError(w, r, "failed to check legal holds", err)
		return
	}
	if holdActive {
		writeError(w, r, http.StatusConflict, model.ErrCodeConflict,
			"decision is covered by an active legal hold and cannot be erased")
		return
	}

	claims := ClaimsFromContext(r.Context())
	erasedBy := claims.AgentID
	if erasedBy == "" {
		erasedBy = claims.Subject
	}

	audit := h.buildAuditEntry(r, orgID,
		"decision_erased", "decision", id.String(),
		nil, nil,
		map[string]any{"erased_by": erasedBy, "reason": req.Reason},
	)
	result, err := h.db.EraseDecision(r.Context(), orgID, id, req.Reason, erasedBy, &audit)
	if err != nil {
		if isNotFoundError(err) {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "decision not found")
			return
		}
		if errors.Is(err, storage.ErrAlreadyErased) {
			writeError(w, r, http.StatusConflict, model.ErrCodeConflict, "decision has already been erased")
			return
		}
		h.writeInternalError(w, r, "failed to erase decision", err)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"decision_id":         id,
		"erased_at":           result.Erasure.ErasedAt,
		"original_hash":       result.Erasure.OriginalHash,
		"erased_hash":         result.Erasure.ErasedHash,
		"alternatives_erased": result.AlternativesErased,
		"evidence_erased":     result.EvidenceErased,
	})
}
