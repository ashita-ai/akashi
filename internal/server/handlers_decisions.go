package server

import (
	"encoding/json"
	"net/http"

	"github.com/pgvector/pgvector-go"

	"github.com/ashita-ai/kyoyu/internal/model"
	"github.com/ashita-ai/kyoyu/internal/storage"
)

// HandleTrace handles POST /v1/trace (convenience endpoint).
func (h *Handlers) HandleTrace(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())

	var req model.TraceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	if claims.Role != model.RoleAdmin && req.AgentID != claims.AgentID {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "can only trace for your own agent_id")
		return
	}

	// 1. Create run.
	run, err := h.db.CreateRun(r.Context(), model.CreateRunRequest{
		AgentID:  req.AgentID,
		TraceID:  req.TraceID,
		Metadata: req.Metadata,
	})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to create run")
		return
	}

	// 2. Generate embedding for decision.
	embText := req.Decision.DecisionType + ": " + req.Decision.Outcome
	if req.Decision.Reasoning != nil {
		embText += " " + *req.Decision.Reasoning
	}
	var decisionEmb *pgvector.Vector
	emb, err := h.embedder.Embed(r.Context(), embText)
	if err != nil {
		h.logger.Warn("trace: embedding generation failed", "error", err)
	} else {
		decisionEmb = &emb
	}

	// 3. Create decision.
	decision, err := h.db.CreateDecision(r.Context(), model.Decision{
		RunID:        run.ID,
		AgentID:      req.AgentID,
		DecisionType: req.Decision.DecisionType,
		Outcome:      req.Decision.Outcome,
		Confidence:   req.Decision.Confidence,
		Reasoning:    req.Decision.Reasoning,
		Embedding:    decisionEmb,
	})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to create decision")
		return
	}

	// 4. Create alternatives.
	eventCount := 0
	if len(req.Decision.Alternatives) > 0 {
		alts := make([]model.Alternative, len(req.Decision.Alternatives))
		for i, a := range req.Decision.Alternatives {
			alts[i] = model.Alternative{
				DecisionID:      decision.ID,
				Label:           a.Label,
				Score:           a.Score,
				Selected:        a.Selected,
				RejectionReason: a.RejectionReason,
			}
		}
		if err := h.db.CreateAlternativesBatch(r.Context(), alts); err != nil {
			h.logger.Warn("trace: failed to create alternatives", "error", err)
		}
		eventCount += len(alts)
	}

	// 5. Create evidence with embeddings.
	if len(req.Decision.Evidence) > 0 {
		evs := make([]model.Evidence, len(req.Decision.Evidence))
		for i, e := range req.Decision.Evidence {
			var evEmb *pgvector.Vector
			if e.Content != "" {
				vec, err := h.embedder.Embed(r.Context(), e.Content)
				if err != nil {
					h.logger.Warn("trace: evidence embedding failed", "error", err)
				} else {
					evEmb = &vec
				}
			}
			evs[i] = model.Evidence{
				DecisionID:     decision.ID,
				SourceType:     model.SourceType(e.SourceType),
				SourceURI:      e.SourceURI,
				Content:        e.Content,
				RelevanceScore: e.RelevanceScore,
				Embedding:      evEmb,
			}
		}
		if err := h.db.CreateEvidenceBatch(r.Context(), evs); err != nil {
			h.logger.Warn("trace: failed to create evidence", "error", err)
		}
		eventCount += len(evs)
	}

	// 6. Complete run.
	_ = h.db.CompleteRun(r.Context(), run.ID, model.RunStatusCompleted, nil)

	// 7. Notify subscribers.
	notifyPayload, _ := json.Marshal(map[string]any{
		"decision_id": decision.ID,
		"agent_id":    req.AgentID,
		"outcome":     req.Decision.Outcome,
	})
	_ = h.db.Notify(r.Context(), storage.ChannelDecisions, string(notifyPayload))

	writeJSON(w, r, http.StatusCreated, map[string]any{
		"run_id":      run.ID,
		"decision_id": decision.ID,
		"event_count": eventCount + 1, // +1 for the decision itself
	})
}

// HandleQuery handles POST /v1/query.
func (h *Handlers) HandleQuery(w http.ResponseWriter, r *http.Request) {
	var req model.QueryRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	decisions, total, err := h.db.QueryDecisions(r.Context(), req)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "query failed")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"decisions": decisions,
		"total":     total,
		"limit":     req.Limit,
		"offset":    req.Offset,
	})
}

// HandleTemporalQuery handles POST /v1/query/temporal.
func (h *Handlers) HandleTemporalQuery(w http.ResponseWriter, r *http.Request) {
	var req model.TemporalQueryRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	decisions, err := h.db.QueryDecisionsTemporal(r.Context(), req)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "temporal query failed")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"as_of":     req.AsOf,
		"decisions": decisions,
	})
}

// HandleAgentHistory handles GET /v1/agents/{agent_id}/history.
func (h *Handlers) HandleAgentHistory(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agent_id")
	if agentID == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "agent_id is required")
		return
	}

	limit := queryInt(r, "limit", 50)
	offset := queryInt(r, "offset", 0)
	from := queryTime(r, "from")
	to := queryTime(r, "to")

	decisions, total, err := h.db.GetDecisionsByAgent(r.Context(), agentID, limit, offset, from, to)
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
	var req model.SearchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	if req.Query == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "query is required")
		return
	}

	queryEmb, err := h.embedder.Embed(r.Context(), req.Query)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to generate query embedding")
		return
	}

	results, err := h.db.SearchDecisionsByEmbedding(r.Context(), queryEmb, req.Filters, req.Limit)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "search failed")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"results": results,
		"total":   len(results),
	})
}

// HandleCheck handles POST /v1/check.
// It performs a lightweight precedent lookup: if a semantic query is provided,
// it searches by embedding similarity; otherwise it does a structured query
// by decision_type. Conflicts for the decision type are always included.
func (h *Handlers) HandleCheck(w http.ResponseWriter, r *http.Request) {
	var req model.CheckRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	if req.DecisionType == "" {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "decision_type is required")
		return
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 5
	}

	var decisions []model.Decision

	if req.Query != "" {
		// Semantic search path.
		queryEmb, err := h.embedder.Embed(r.Context(), req.Query)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to generate query embedding")
			return
		}

		filters := model.QueryFilters{
			DecisionType: &req.DecisionType,
		}
		if req.AgentID != "" {
			filters.AgentIDs = []string{req.AgentID}
		}

		results, err := h.db.SearchDecisionsByEmbedding(r.Context(), queryEmb, filters, limit)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "search failed")
			return
		}
		for _, sr := range results {
			decisions = append(decisions, sr.Decision)
		}
	} else {
		// Structured query path.
		filters := model.QueryFilters{
			DecisionType: &req.DecisionType,
		}
		if req.AgentID != "" {
			filters.AgentIDs = []string{req.AgentID}
		}

		queried, _, err := h.db.QueryDecisions(r.Context(), model.QueryRequest{
			Filters:  filters,
			Include:  []string{"alternatives"},
			OrderBy:  "valid_from",
			OrderDir: "desc",
			Limit:    limit,
		})
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "query failed")
			return
		}
		decisions = queried
	}

	// Always check for conflicts on this decision type.
	conflicts, err := h.db.ListConflicts(r.Context(), &req.DecisionType, limit)
	if err != nil {
		h.logger.Warn("check: failed to list conflicts", "error", err)
		// Non-fatal: return decisions without conflict data.
		conflicts = nil
	}

	writeJSON(w, r, http.StatusOK, model.CheckResponse{
		HasPrecedent: len(decisions) > 0,
		Decisions:    decisions,
		Conflicts:    conflicts,
	})
}

// HandleDecisionsRecent handles GET /v1/decisions/recent.
// It returns recent decisions with optional filters for agent_id, decision_type, and limit.
func (h *Handlers) HandleDecisionsRecent(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 10)

	filters := model.QueryFilters{}
	if agentID := r.URL.Query().Get("agent_id"); agentID != "" {
		filters.AgentIDs = []string{agentID}
	}
	if dt := r.URL.Query().Get("decision_type"); dt != "" {
		filters.DecisionType = &dt
	}

	decisions, total, err := h.db.QueryDecisions(r.Context(), model.QueryRequest{
		Filters:  filters,
		Include:  []string{"alternatives"},
		OrderBy:  "valid_from",
		OrderDir: "desc",
		Limit:    limit,
	})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "query failed")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"decisions": decisions,
		"total":     total,
		"limit":     limit,
	})
}

// HandleListConflicts handles GET /v1/conflicts.
func (h *Handlers) HandleListConflicts(w http.ResponseWriter, r *http.Request) {
	var decisionType *string
	if dt := r.URL.Query().Get("decision_type"); dt != "" {
		decisionType = &dt
	}
	limit := queryInt(r, "limit", 50)

	conflicts, err := h.db.ListConflicts(r.Context(), decisionType, limit)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to list conflicts")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"conflicts": conflicts,
		"total":     len(conflicts),
	})
}
