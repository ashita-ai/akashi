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
	if fromTime, err := queryTime(r, "from"); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	} else if fromTime != nil {
		filters.TimeRange = &model.TimeRange{From: fromTime}
	}
	if toTime, err := queryTime(r, "to"); err != nil {
		writeError(w, r, http.StatusBadRequest, model.ErrCodeInvalidInput, err.Error())
		return
	} else if toTime != nil {
		if filters.TimeRange == nil {
			filters.TimeRange = &model.TimeRange{}
		}
		filters.TimeRange.To = toTime
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
			if cursor == nil {
				// Headers not yet sent — we can still return a proper error response.
				h.writeInternalError(w, r, "export failed", err)
			} else {
				h.logger.Error("export failed mid-stream",
					"error", err,
					"method", r.Method,
					"path", r.URL.Path,
					"request_id", RequestIDFromContext(r.Context()))
				// Headers already sent — write an error sentinel as the last NDJSON line
				// so consumers can detect the truncation instead of silently accepting
				// a partial export as complete.
				_ = encoder.Encode(map[string]any{
					"__error":  true,
					"message":  "export terminated due to internal error",
					"exported": cursor != nil,
				})
				if flusher != nil {
					flusher.Flush()
				}
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
