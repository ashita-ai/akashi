package server

import (
	"net/http"

	"github.com/ashita-ai/akashi/internal/model"
)

// HandleSessionView handles GET /v1/sessions/{session_id}.
// Returns all decisions from a given MCP/HTTP session, with summary statistics.
func (h *Handlers) HandleSessionView(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	sid, err := parsePathUUID(r, "session_id")
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
		writeJSON(w, r, http.StatusOK, model.SessionViewResponse{
			SessionID:     sid,
			Decisions:     []model.Decision{},
			DecisionCount: 0,
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

	writeJSON(w, r, http.StatusOK, model.SessionViewResponse{
		SessionID:     sid,
		Decisions:     decs,
		DecisionCount: len(decs),
		Summary: &model.SessionViewSummary{
			StartedAt:     startedAt,
			EndedAt:       endedAt,
			DurationSecs:  duration,
			DecisionTypes: decisionTypes,
			AvgConfidence: avgConfidence,
		},
	})
}
