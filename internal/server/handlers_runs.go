package server

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ashita-ai/akashi/internal/model"
)

// HandleCreateRun handles POST /v1/runs.
func (h *Handlers) HandleCreateRun(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	var req model.CreateRunRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	if err := model.ValidateAgentID(req.AgentID); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

	// Agents can only create runs for themselves.
	if !model.RoleAtLeast(claims.Role, model.RoleAdmin) && req.AgentID != claims.AgentID {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "can only create runs for your own agent_id")
		return
	}

	// Set OTEL span attributes for trace correlation.
	span := trace.SpanFromContext(r.Context())
	span.SetAttributes(attribute.String("akashi.agent_id", req.AgentID))
	if req.TraceID != nil {
		span.SetAttributes(attribute.String("akashi.trace_id", *req.TraceID))
	}

	idem, proceed := h.beginIdempotentWrite(w, r, orgID, req.AgentID, "POST:/v1/runs", req)
	if !proceed {
		return
	}

	req.OrgID = orgID
	run, err := h.db.CreateRun(r.Context(), req)
	if err != nil {
		h.clearIdempotentWrite(r, orgID, idem)
		h.writeInternalError(w, r, "failed to create run", err)
		return
	}

	if err := h.completeIdempotentWrite(r, orgID, idem, http.StatusCreated, run); err != nil {
		h.writeInternalError(w, r, "failed to finalize idempotency record", err)
		return
	}
	writeJSON(w, r, http.StatusCreated, run)
}

// HandleAppendEvents handles POST /v1/runs/{run_id}/events.
func (h *Handlers) HandleAppendEvents(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())
	runID, err := parseRunID(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

	// Verify run exists within the caller's org and agent has access.
	run, err := h.db.GetRun(r.Context(), orgID, runID)
	if err != nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "run not found")
		return
	}

	if !model.RoleAtLeast(claims.Role, model.RoleAdmin) && run.AgentID != claims.AgentID {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "not your run")
		return
	}

	var req model.AppendEventsRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	if len(req.Events) == 0 {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "events array must not be empty")
		return
	}

	idem, proceed := h.beginIdempotentWrite(w, r, orgID, run.AgentID, appendEventsEndpoint(runID), req)
	if !proceed {
		return
	}

	events, err := h.buffer.Append(r.Context(), runID, run.AgentID, run.OrgID, req.Events)
	if err != nil {
		h.clearIdempotentWrite(r, orgID, idem)
		msg := err.Error()
		switch {
		case strings.Contains(msg, "buffer at capacity"):
			writeError(w, r, http.StatusServiceUnavailable, model.ErrCodeConflict, "event buffer is full, retry shortly")
		case strings.Contains(msg, "buffer is draining"):
			writeError(w, r, http.StatusServiceUnavailable, model.ErrCodeConflict, "server is shutting down, retry on another instance")
		default:
			h.writeInternalError(w, r, "failed to buffer events", err)
		}
		return
	}
	if err := h.buffer.FlushNow(r.Context()); err != nil {
		h.clearIdempotentWrite(r, orgID, idem)
		h.writeInternalError(w, r, "failed to persist buffered events", err)
		return
	}

	eventIDs := make([]uuid.UUID, len(events))
	for i, e := range events {
		eventIDs[i] = e.ID
	}

	resp := map[string]any{
		"accepted":  len(events),
		"event_ids": eventIDs,
		"status":    "persisted",
		"message":   "events durably persisted",
	}
	if err := h.completeIdempotentWrite(r, orgID, idem, http.StatusOK, resp); err != nil {
		h.writeInternalError(w, r, "failed to finalize idempotency record", err)
		return
	}
	writeJSON(w, r, http.StatusOK, resp)
}

// HandleCompleteRun handles POST /v1/runs/{run_id}/complete.
func (h *Handlers) HandleCompleteRun(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())
	runID, err := parseRunID(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

	run, err := h.db.GetRun(r.Context(), orgID, runID)
	if err != nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "run not found")
		return
	}
	if !model.RoleAtLeast(claims.Role, model.RoleAdmin) && run.AgentID != claims.AgentID {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "not your run")
		return
	}

	var req model.CompleteRunRequest
	if err := decodeJSON(r, &req, h.maxRequestBodyBytes); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "invalid request body")
		return
	}

	var status model.RunStatus
	switch req.Status {
	case "completed", "":
		status = model.RunStatusCompleted
	case "failed":
		status = model.RunStatusFailed
	default:
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, "status must be 'completed' or 'failed'")
		return
	}

	if err := h.db.CompleteRun(r.Context(), orgID, runID, status, req.Metadata); err != nil {
		h.writeInternalError(w, r, "failed to complete run", err)
		return
	}

	updated, err := h.db.GetRun(r.Context(), orgID, runID)
	if err != nil {
		h.logger.Warn("complete run: read-back failed", "error", err, "run_id", runID)
		writeJSON(w, r, http.StatusOK, map[string]any{"run_id": runID, "status": string(status)})
		return
	}
	writeJSON(w, r, http.StatusOK, updated)
}

// HandleGetRun handles GET /v1/runs/{run_id}.
func (h *Handlers) HandleGetRun(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())
	runID, err := parseRunID(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	}

	run, err := h.db.GetRun(r.Context(), orgID, runID)
	if err != nil {
		writeError(w, r, http.StatusNotFound, model.ErrCodeNotFound, "run not found")
		return
	}

	ok, err := canAccessAgent(r.Context(), h.db, claims, run.AgentID)
	if err != nil {
		h.writeInternalError(w, r, "authorization check failed", err)
		return
	}
	if !ok {
		writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "no access to this run")
		return
	}

	const maxRunEvents = 10_000
	events, err := h.db.GetEventsByRun(r.Context(), orgID, runID, maxRunEvents)
	if err != nil {
		h.writeInternalError(w, r, "failed to get events", err)
		return
	}

	// Get decisions for this run. Use a high ceiling rather than an arbitrary
	// limit — an audit endpoint must not silently drop records. If the ceiling
	// is hit, signal truncation so the caller knows the data is incomplete.
	const maxRunDecisions = 10_000
	decisions, total, err := h.db.QueryDecisions(r.Context(), orgID, model.QueryRequest{
		Filters: model.QueryFilters{
			RunID: &runID,
		},
		Include: []string{"alternatives", "evidence"},
		Limit:   maxRunDecisions,
	})
	if err != nil {
		h.writeInternalError(w, r, "failed to get decisions", err)
		return
	}

	resp := map[string]any{
		"run":       run,
		"events":    events,
		"decisions": decisions,
	}
	truncated := false
	if len(events) >= maxRunEvents {
		resp["truncated_events"] = true
		truncated = true
	}
	if total > maxRunDecisions {
		resp["truncated_decisions"] = true
		resp["total_decisions"] = total
		truncated = true
	}
	if truncated {
		resp["truncated"] = true
	}
	writeJSON(w, r, http.StatusOK, resp)
}
