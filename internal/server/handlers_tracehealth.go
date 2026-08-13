package server

import (
	"net/http"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/tracehealth"
)

// HandleTraceHealth handles GET /v1/trace-health.
// Returns aggregate health metrics for the caller's organization.
// Optional query params: from, to (RFC3339) scope metrics to a time window.
func (h *Handlers) HandleTraceHealth(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())

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

	svc := tracehealth.New(h.db)
	metrics, err := svc.Compute(r.Context(), orgID, from, to)
	if err != nil {
		h.writeInternalError(w, r, "failed to compute trace health", err)
		return
	}

	writeJSON(w, r, http.StatusOK, metrics)
}
