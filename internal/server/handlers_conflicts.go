package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/conflicts"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/storage"
)

// HandleListConflicts handles GET /v1/conflicts.
func (h *Handlers) HandleListConflicts(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	filters := parseConflictFilters(r)
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

	preFilterCount := len(conflicts)
	conflicts, err = filterConflictsByAccess(r.Context(), h.db, claims, conflicts, h.grantCache)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}

	ptotal, hasMore := computePagination(len(conflicts), preFilterCount, limit, offset, total)
	writeListJSON(w, r, conflicts, ptotal, hasMore, limit, offset)
}

// HandleListConflictGroups handles GET /v1/conflict-groups.
// Returns one entry per logical conflict cluster (same agents × decision-type) with
// the highest-significance representative conflict embedded. This is the deduplicated
// view that eliminates N×M pairwise noise from the raw conflicts list.
func (h *Handlers) HandleListConflictGroups(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	filters := storage.ConflictGroupFilters{}
	if dt := r.URL.Query().Get("decision_type"); dt != "" {
		filters.DecisionType = &dt
	}
	if aid := r.URL.Query().Get("agent_id"); aid != "" {
		filters.AgentID = &aid
	}
	if ck := r.URL.Query().Get("conflict_kind"); ck != "" {
		filters.ConflictKind = &ck
	}
	if st := r.URL.Query().Get("status"); st != "" {
		filters.Status = &st
	}

	limit := queryLimit(r, 25)
	offset := queryOffset(r)

	total, err := h.db.CountConflictGroups(r.Context(), orgID, filters)
	if err != nil {
		h.writeInternalError(w, r, "failed to count conflict groups", err)
		return
	}

	groups, err := h.db.ListConflictGroups(r.Context(), orgID, filters, limit, offset)
	if err != nil {
		h.writeInternalError(w, r, "failed to list conflict groups", err)
		return
	}

	preFilterCount := len(groups)
	groups, err = filterConflictGroupsByAccess(r.Context(), h.db, claims, groups, h.grantCache)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}

	ptotal, hasMore := computePagination(len(groups), preFilterCount, limit, offset, total)
	writeListJSON(w, r, groups, ptotal, hasMore, limit, offset)
}

// cascadeSimilarityThreshold is the minimum cosine similarity between a
// winning decision's outcome_embedding and a candidate conflict's side for
// the cascade to auto-resolve that conflict. 0.80 is conservative enough to
// avoid false matches while catching genuine variants of the same disagreement.
const cascadeSimilarityThreshold = 0.80

// validResolutionStatuses defines the allowed values for conflict status transitions.
// Used by both individual conflict patches and batch group resolution.
var validResolutionStatuses = map[string]bool{
	"resolved":       true,
	"false_positive": true,
}

// HandlePatchConflict handles PATCH /v1/conflicts/{id}.
// Transitions a conflict to a new lifecycle state.
func (h *Handlers) HandlePatchConflict(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	id, err := parsePathUUID(r, "id")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid conflict id")
		return
	}

	var req model.ConflictStatusUpdate
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
		return
	}

	if !validResolutionStatuses[req.Status] {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
			"status must be one of: resolved, false_positive")
		return
	}

	// winning_decision_id is only valid when resolving a conflict.
	if req.WinningDecisionID != nil && req.Status != "resolved" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
			"winning_decision_id can only be set when status is 'resolved'")
		return
	}

	// If a winner is declared, validate it belongs to this conflict before
	// touching the DB (avoids a silent no-op or cross-conflict winner reference).
	if req.WinningDecisionID != nil {
		conflict, cErr := h.db.GetConflict(r.Context(), id, orgID)
		if cErr != nil || conflict == nil {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "conflict not found")
			return
		}
		if *req.WinningDecisionID != conflict.DecisionAID && *req.WinningDecisionID != conflict.DecisionBID {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
				"winning_decision_id must be one of the two decisions in this conflict")
			return
		}
	}

	resolvedBy := claims.ActorID()

	fpLabel := storage.ComputeFPLabel(req.Status, req.FalsePositiveLabel)

	audit := h.buildAuditEntry(r, orgID,
		"conflict_status_changed", "conflict", id.String(),
		nil, nil,
		map[string]any{"new_status": req.Status, "resolved_by": resolvedBy},
	)
	if _, err := h.db.UpdateConflictStatusWithAudit(r.Context(), id, orgID, req.Status, resolvedBy, req.ResolutionNote, req.WinningDecisionID, fpLabel, audit); err != nil {
		if isNotFoundError(err) {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "conflict not found")
			return
		}
		if errors.Is(err, storage.ErrWinningDecisionNotInConflict) {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
				"winning_decision_id must be one of the two decisions in this conflict")
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

	if h.resolutionRecorder != nil {
		h.resolutionRecorder.RecordResolution(r.Context(), req.Status, string(conflict.ConflictKind), 1)
	}

	// Resolution cascade: when a conflict is resolved with a winner and belongs
	// to a group, auto-resolve other open conflicts in the same group whose
	// outcome embeddings align with the winning decision.
	if req.Status == "resolved" && req.WinningDecisionID != nil {
		h.executeCascadeResolution(r, orgID, *conflict, *req.WinningDecisionID)
	}

	writeJSON(w, r, http.StatusOK, conflict)
}

// HandleResolveConflictGroup handles PATCH /v1/conflict-groups/{id}/resolve.
// Batch-resolves all open conflicts in a conflict group.
func (h *Handlers) HandleResolveConflictGroup(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	groupID, err := parsePathUUID(r, "id")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid conflict group id")
		return
	}

	var req model.ConflictGroupResolveRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
		return
	}

	if !validResolutionStatuses[req.Status] {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
			"status must be one of: resolved, false_positive")
		return
	}

	if req.WinningAgent != nil && req.Status != "resolved" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
			"winning_agent can only be set when status is 'resolved'")
		return
	}

	resolvedBy := claims.ActorID()

	audit := h.buildAuditEntry(r, orgID,
		"conflict_group_resolved", "conflict_group", groupID.String(),
		nil, nil,
		map[string]any{"new_status": req.Status, "resolved_by": resolvedBy},
	)

	fpLabel := storage.ComputeFPLabel(req.Status, req.FalsePositiveLabel)

	affected, err := h.db.ResolveConflictGroup(r.Context(), groupID, orgID, req.Status, resolvedBy, req.ResolutionNote, req.WinningAgent, fpLabel, audit)
	if err != nil {
		if isNotFoundError(err) {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "conflict group not found")
			return
		}
		if errors.Is(err, storage.ErrWinningAgentNotInGroup) {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
				"winning_agent does not match either agent in this conflict group")
			return
		}
		if errors.Is(err, storage.ErrRevisedDecisions) {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
				"some conflicts in the group reference revised decisions and cannot be resolved with a winner")
			return
		}
		h.writeInternalError(w, r, "failed to resolve conflict group", err)
		return
	}

	if h.resolutionRecorder != nil && affected > 0 {
		groupKind, kindErr := h.db.GetConflictGroupKind(r.Context(), groupID, orgID)
		if kindErr == nil {
			h.resolutionRecorder.RecordResolution(r.Context(), req.Status, groupKind, affected)
		}
	}

	writeJSON(w, r, http.StatusOK, model.ConflictGroupResolveResult{
		GroupID:  groupID,
		Status:   req.Status,
		Resolved: affected,
	})
}

// HandleAdjudicateConflict handles POST /v1/conflicts/{id}/adjudicate.
// Creates an adjudication decision trace and links it to the conflict.
func (h *Handlers) HandleAdjudicateConflict(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	id, err := parsePathUUID(r, "id")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid conflict id")
		return
	}

	var req struct {
		Outcome           string     `json:"outcome"`
		Reasoning         *string    `json:"reasoning,omitempty"`
		DecisionType      string     `json:"decision_type,omitempty"`
		WinningDecisionID *uuid.UUID `json:"winning_decision_id,omitempty"`
	}
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
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

	// Validate winning_decision_id: if provided, must be one of the two conflict sides.
	if req.WinningDecisionID != nil {
		if *req.WinningDecisionID != conflict.DecisionAID && *req.WinningDecisionID != conflict.DecisionBID {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
				"winning_decision_id must be one of the two decisions in this conflict")
			return
		}
	}

	resolverAgent := claims.ActorID()

	// Ensure the resolver agent exists (auto-create if admin+).
	autoRegAudit := h.buildAuditEntry(r, orgID, "", "agent", resolverAgent, nil, nil, nil)
	if _, err := h.decisionSvc.ResolveOrCreateAgent(r.Context(), orgID, resolverAgent, claims.Role, &autoRegAudit); err != nil {
		if errors.Is(err, decisions.ErrAgentNotFound) {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
			return
		}
		h.writeInternalError(w, r, "failed to resolve agent", err)
		return
	}

	// Create an adjudication decision trace AND resolve the conflict atomically.
	// A single transaction prevents the failure mode where an adjudication decision
	// exists but the conflict remains unresolved.
	note := "Resolved by adjudication trace"
	conflictAudit := h.buildAuditEntry(r, orgID,
		"conflict_adjudicated_with_decision", "conflict", id.String(),
		nil, nil,
		map[string]any{"resolved_by": resolverAgent},
	)
	result, err := h.decisionSvc.AdjudicateConflictWithTrace(r.Context(), orgID, decisions.TraceInput{
		AgentID: resolverAgent,
		Decision: model.TraceDecision{
			DecisionType: req.DecisionType,
			Outcome:      req.Outcome,
			Confidence:   1.0, // Adjudication decisions are definitive.
			Reasoning:    req.Reasoning,
		},
		APIKeyID:  claims.APIKeyID,
		AuditMeta: h.buildAuditMeta(r, orgID),
	}, storage.AdjudicateConflictInTraceParams{
		ConflictID:        id,
		ResolvedBy:        resolverAgent,
		ResNote:           &note,
		Audit:             conflictAudit,
		WinningDecisionID: req.WinningDecisionID,
	})
	if err != nil {
		if isNotFoundError(err) {
			writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "conflict not found")
			return
		}
		h.writeInternalError(w, r, "failed to adjudicate conflict", err)
		return
	}

	if h.resolutionRecorder != nil {
		h.resolutionRecorder.RecordResolution(r.Context(), "resolved", string(conflict.ConflictKind), 1)
	}

	// Resolution cascade: auto-resolve related conflicts in the same group.
	if req.WinningDecisionID != nil {
		h.executeCascadeResolution(r, orgID, *conflict, *req.WinningDecisionID)
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

// HandleGetConflict handles GET /v1/conflicts/{id}.
// Returns a single conflict with a lazily-computed resolution recommendation.
func (h *Handlers) HandleGetConflict(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	id, err := parsePathUUID(r, "id")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid conflict id")
		return
	}

	conflict, err := h.db.GetConflict(r.Context(), id, orgID)
	if err != nil {
		h.writeInternalError(w, r, "failed to get conflict", err)
		return
	}
	if conflict == nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "conflict not found")
		return
	}

	// RBAC: verify the caller can see both decisions in this conflict.
	filtered, err := filterConflictsByAccess(r.Context(), h.db, claims, []model.DecisionConflict{*conflict}, h.grantCache)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}
	if len(filtered) == 0 {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "conflict not found")
		return
	}

	detail := model.ConflictDetail{DecisionConflict: *conflict}

	// Compute recommendation for open conflicts only.
	if conflict.Status == "open" {
		detail.Recommendation = h.computeRecommendation(r.Context(), *conflict, orgID)
	}

	// Hydrate reopened resolution details when this conflict reopens a prior one.
	if conflict.ReopensResolutionID != nil {
		res, resErr := h.db.GetConflictResolution(r.Context(), *conflict.ReopensResolutionID, orgID)
		if resErr != nil {
			h.logger.Warn("failed to hydrate reopens_resolution", "error", resErr,
				"conflict_id", id, "reopens_resolution_id", conflict.ReopensResolutionID)
		} else if res != nil {
			detail.ReopensResolution = res
		}
	}

	writeJSON(w, r, http.StatusOK, detail)
}

// computeRecommendation gathers signals and computes a resolution recommendation.
// Errors from signal queries are logged and treated as zero-signal rather than
// failing the request — a partial recommendation is better than none.
func (h *Handlers) computeRecommendation(ctx context.Context, c model.DecisionConflict, orgID uuid.UUID) *model.Recommendation {
	agents := []string{c.AgentA, c.AgentB}
	winRates, err := h.db.GetAgentWinRates(ctx, orgID, agents, c.DecisionType)
	if err != nil {
		h.logger.Warn("recommendation: failed to get agent win rates", "error", err)
		winRates = map[string]storage.AgentWinRate{}
	}

	// Fetch revision depths concurrently — they're independent queries.
	var depthA, depthB int
	var errA, errB error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		depthA, errA = h.db.GetRevisionDepth(ctx, c.DecisionAID, orgID)
	}()
	go func() {
		defer wg.Done()
		depthB, errB = h.db.GetRevisionDepth(ctx, c.DecisionBID, orgID)
	}()
	wg.Wait()

	if errA != nil {
		h.logger.Warn("recommendation: failed to get revision depth for decision A", "error", errA)
	}
	if errB != nil {
		h.logger.Warn("recommendation: failed to get revision depth for decision B", "error", errB)
	}

	return conflicts.Recommend(conflicts.RecommendationInput{
		Conflict:       c,
		WinRateA:       winRates[c.AgentA].WinRate,
		WinRateB:       winRates[c.AgentB].WinRate,
		WinCountA:      winRates[c.AgentA].Total,
		WinCountB:      winRates[c.AgentB].Total,
		RevisionDepthA: depthA,
		RevisionDepthB: depthB,
	})
}

// validAnalyticsPeriods maps convenience period strings to durations.
var validAnalyticsPeriods = map[string]time.Duration{
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
	"90d": 90 * 24 * time.Hour,
}

const maxAnalyticsRangeDays = 365

// HandleConflictAnalytics handles GET /v1/conflicts/analytics.
// Returns aggregated conflict metrics over a time window: summary stats,
// breakdowns by agent pair / decision type / severity, and a daily trend.
func (h *Handlers) HandleConflictAnalytics(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

	// Determine time range from query parameters.
	var from, to time.Time
	now := time.Now().UTC()

	fromParam, err := queryTime(r, "from")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}
	toParam, err := queryTime(r, "to")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

	if fromParam != nil && toParam != nil {
		from = *fromParam
		to = *toParam
	} else {
		period := r.URL.Query().Get("period")
		if period == "" {
			period = "7d"
		}
		dur, ok := validAnalyticsPeriods[period]
		if !ok {
			writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
				"invalid period: must be one of 7d, 30d, 90d")
			return
		}
		from = now.Add(-dur)
		to = now
	}

	if !from.Before(to) {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
			"from must be before to")
		return
	}
	if to.Sub(from).Hours() > float64(maxAnalyticsRangeDays*24) {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput,
			"time range must not exceed 365 days")
		return
	}

	// Build filters.
	filters := storage.ConflictAnalyticsFilters{From: from, To: to}
	if v := r.URL.Query().Get("agent_id"); v != "" {
		filters.AgentID = &v
	}
	if v := r.URL.Query().Get("decision_type"); v != "" {
		filters.DecisionType = &v
	}
	if v := r.URL.Query().Get("conflict_kind"); v != "" {
		filters.ConflictKind = &v
	}

	analytics, err := h.db.GetConflictAnalytics(r.Context(), orgID, filters)
	if err != nil {
		h.writeInternalError(w, r, "failed to get conflict analytics", err)
		return
	}

	writeJSON(w, r, http.StatusOK, analytics)
}

// HandleDecisionConflicts handles GET /v1/decisions/{id}/conflicts.
// Returns conflicts involving a specific decision (as A or B side), paginated.
// Accepts ?limit, ?offset, and ?status query parameters.
func (h *Handlers) HandleDecisionConflicts(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	decisionID, err := parsePathUUID(r, "id")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid decision ID")
		return
	}

	limit := queryLimit(r, 50)
	if limit > 200 {
		limit = 200
	}
	offset := queryOffset(r)

	filters := storage.ConflictFilters{DecisionID: &decisionID}
	if st := r.URL.Query().Get("status"); st != "" {
		filters.Status = &st
	}

	total, err := h.db.CountConflicts(r.Context(), orgID, filters)
	if err != nil {
		h.writeInternalError(w, r, "failed to count decision conflicts", err)
		return
	}

	conflicts, err := h.db.ListConflicts(r.Context(), orgID, filters, limit, offset)
	if err != nil {
		h.writeInternalError(w, r, "failed to list decision conflicts", err)
		return
	}

	preFilterCount := len(conflicts)
	conflicts, err = filterConflictsByAccess(r.Context(), h.db, claims, conflicts, h.grantCache)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}

	ptotal, hasMore := computePagination(len(conflicts), preFilterCount, limit, offset, total)
	writeListJSON(w, r, conflicts, ptotal, hasMore, limit, offset)
}

// executeCascadeResolution auto-resolves related conflicts in the same group
// whose outcome embeddings align with the winning decision. Called after both
// manual resolution (PATCH) and adjudication (POST .../adjudicate) to propagate
// the winner across the conflict group. No-op when the conflict has no group.
func (h *Handlers) executeCascadeResolution(r *http.Request, orgID uuid.UUID, conflict model.DecisionConflict, winningDecisionID uuid.UUID) {
	if conflict.GroupID == nil {
		return
	}
	cascadeAudit := h.buildAuditEntry(r, orgID,
		"conflict_cascade_resolved", "conflict", conflict.ID.String(),
		nil, nil,
		map[string]any{
			"trigger_conflict_id": conflict.ID.String(),
			"winning_decision_id": winningDecisionID.String(),
		},
	)
	cascaded, cascadeErr := h.db.CascadeResolveByOutcome(
		r.Context(), orgID, *conflict.GroupID, winningDecisionID, conflict.ID,
		cascadeSimilarityThreshold, cascadeAudit,
	)
	if cascadeErr != nil {
		h.logger.Warn("resolution cascade failed", "conflict_id", conflict.ID, "error", cascadeErr)
	} else if cascaded > 0 {
		h.logger.Info("resolution cascade resolved conflicts",
			"trigger_conflict_id", conflict.ID,
			"group_id", conflict.GroupID,
			"cascade_resolved", cascaded,
		)
		if h.resolutionRecorder != nil {
			h.resolutionRecorder.RecordResolution(r.Context(), "resolved", string(conflict.ConflictKind), cascaded)
		}
	}
}

// parseConflictFilters extracts conflict filter parameters from the request query string.
func parseConflictFilters(r *http.Request) storage.ConflictFilters {
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
	if st := r.URL.Query().Get("status"); st != "" {
		filters.Status = &st
	}
	return filters
}
