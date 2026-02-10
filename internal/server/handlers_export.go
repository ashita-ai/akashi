package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// HandleExportDecisions handles GET /v1/export/decisions (admin-only).
// Streams decisions as NDJSON (one JSON object per line), including
// alternatives and evidence for each decision. Uses cursor-based
// pagination to avoid loading all results into memory.
func (h *Handlers) HandleExportDecisions(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFromContext(r.Context())
	q := r.URL.Query()

	filters := model.QueryFilters{}
	if agentID := q.Get("agent_id"); agentID != "" {
		filters.AgentIDs = []string{agentID}
	}
	if dt := q.Get("decision_type"); dt != "" {
		filters.DecisionType = &dt
	}
	if fromStr := q.Get("from"); fromStr != "" {
		if t, err := time.Parse(time.RFC3339, fromStr); err == nil {
			filters.TimeRange = &model.TimeRange{From: &t}
		}
	}
	if toStr := q.Get("to"); toStr != "" {
		if t, err := time.Parse(time.RFC3339, toStr); err == nil {
			if filters.TimeRange == nil {
				filters.TimeRange = &model.TimeRange{}
			}
			filters.TimeRange.To = &t
		}
	}

	// Filename with timestamp.
	filename := fmt.Sprintf("akashi-export-%s.ndjson", time.Now().UTC().Format("20060102-150405"))

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Cache-Control", "no-cache")

	// Stream in pages using keyset (cursor-based) pagination to avoid O(offset)
	// degradation. Each page uses (valid_from, id) > (last_seen) instead of OFFSET,
	// so every page is O(1) regardless of position in the result set.
	const pageSize = 100
	encoder := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	var cursor *storage.ExportCursor

	for {
		decisions, err := h.db.ExportDecisionsCursor(r.Context(), orgID, filters, cursor, pageSize)
		if err != nil {
			h.logger.Error("export failed", "error", err)
			if cursor == nil {
				writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "export failed")
			}
			return
		}

		for _, d := range decisions {
			if err := encoder.Encode(d); err != nil {
				return // Client disconnected.
			}
		}

		if flusher != nil {
			flusher.Flush()
		}

		if len(decisions) < pageSize {
			break // Last page.
		}

		// Advance cursor to the last row's position.
		last := decisions[len(decisions)-1]
		cursor = &storage.ExportCursor{ValidFrom: last.ValidFrom, ID: last.ID}
	}
}
