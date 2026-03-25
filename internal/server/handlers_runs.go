package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"github.com/ashita-ai/akashi/internal/integrity"
	"github.com/ashita-ai/akashi/internal/model"
	tracesvc "github.com/ashita-ai/akashi/internal/service/trace"
	"github.com/ashita-ai/akashi/internal/storage"
)

// ---------------------------------------------------------------------------
// Typed enrichment response structs for GET /v1/runs/{run_id}?include=enrichments
// ---------------------------------------------------------------------------

// enrichmentRevisions holds the revision chain for a single decision.
// Count is the number of items returned (post-access-filter). Total is the
// number of revisions that exist before filtering; it is omitted when equal
// to Count so consumers can detect hidden revisions.
type enrichmentRevisions struct {
	Items    []model.Decision `json:"items"`
	Count    int              `json:"count"`
	Total    int              `json:"total,omitempty"`
	Degraded bool             `json:"degraded,omitempty"`
}

// enrichmentConflicts holds conflicts for a single decision.
// Count is the number of items returned (post-access-filter, post-truncation).
// Total is the pre-filter count; omitted when equal to Count.
type enrichmentConflicts struct {
	Items    []model.DecisionConflict `json:"items"`
	Count    int                      `json:"count"`
	Total    int                      `json:"total,omitempty"`
	HasMore  bool                     `json:"has_more"`
	Degraded bool                     `json:"degraded,omitempty"`
}

// enrichmentIntegrity holds the integrity verification result.
type enrichmentIntegrity struct {
	Status      string `json:"status"`
	ContentHash string `json:"content_hash,omitempty"`
}

// decisionEnrichment holds all enrichment data for a single decision.
type decisionEnrichment struct {
	Revisions enrichmentRevisions     `json:"revisions"`
	Lineage   storage.DecisionLineage `json:"lineage"`
	Conflicts enrichmentConflicts     `json:"conflicts"`
	Integrity enrichmentIntegrity     `json:"integrity"`
	Degraded  bool                    `json:"degraded,omitempty"`
}

// getRunResponse is the typed response for GET /v1/runs/{run_id}.
type getRunResponse struct {
	Run                  model.AgentRun                `json:"run"`
	Events               []model.AgentEvent            `json:"events"`
	Decisions            []model.Decision              `json:"decisions"`
	DecisionEnrichments  map[string]decisionEnrichment `json:"decision_enrichments,omitempty"`
	Truncated            bool                          `json:"truncated,omitempty"`
	TruncatedEvents      bool                          `json:"truncated_events,omitempty"`
	TruncatedDecisions   bool                          `json:"truncated_decisions,omitempty"`
	TotalDecisions       int                           `json:"total_decisions,omitempty"`
	TruncatedEnrichments bool                          `json:"truncated_enrichments,omitempty"`
	EnrichedCount        int                           `json:"enriched_count,omitempty"`
}

// HandleCreateRun handles POST /v1/runs.
func (h *Handlers) HandleCreateRun(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())

	var req model.CreateRunRequest
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
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
	audit := h.buildAuditEntry(r, orgID, "create_run", "agent_run", "", nil, nil,
		map[string]any{"agent_id": req.AgentID})
	run, err := h.db.CreateRunWithAudit(r.Context(), req, audit)
	if err != nil {
		h.clearIdempotentWrite(r, orgID, idem)
		h.writeInternalError(w, r, "failed to create run", err)
		return
	}

	h.completeIdempotentWriteBestEffort(r, orgID, idem, http.StatusCreated, run)
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
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
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
		switch {
		case errors.Is(err, tracesvc.ErrBufferAtCapacity):
			writeError(w, r, http.StatusServiceUnavailable, model.ErrCodeConflict, "event buffer is full, retry shortly")
		case errors.Is(err, tracesvc.ErrBufferDraining):
			writeError(w, r, http.StatusServiceUnavailable, model.ErrCodeConflict, "server is shutting down, retry on another instance")
		default:
			h.writeInternalError(w, r, "failed to buffer events", err)
		}
		return
	}

	// After Append succeeds, events are in the buffer and WILL be persisted by
	// the background flush loop (barring process crash). From this point we must
	// NEVER clear the idempotency key — doing so allows retries to create
	// duplicate events. See issue #65.

	eventIDs := make([]uuid.UUID, len(events))
	for i, e := range events {
		eventIDs[i] = e.ID
	}

	statusCode := http.StatusOK
	resp := map[string]any{
		"accepted":  len(events),
		"event_ids": eventIDs,
		"status":    "persisted",
		"message":   "events durably persisted",
	}

	if err := h.buffer.FlushNow(r.Context()); err != nil {
		// Events are in the buffer and will be flushed by the background loop.
		// Return 202 to signal they are accepted but not yet confirmed durable.
		// Do NOT clear the idempotency key — that would allow duplicate writes.
		h.logger.Warn("flush after append failed, events buffered for background persistence",
			"error", err,
			"run_id", runID,
			"event_count", len(events),
			"request_id", RequestIDFromContext(r.Context()))
		statusCode = http.StatusAccepted
		resp["status"] = "buffered"
		resp["message"] = "events accepted, will be persisted by background flush"
	}

	if err := h.recordMutationAuditBestEffort(
		r,
		orgID,
		"append_events",
		"agent_run",
		runID.String(),
		nil,
		resp,
		map[string]any{"agent_id": run.AgentID, "event_count": len(events)},
	); err != nil {
		h.logger.Error("failed to record mutation audit after committed append_events",
			"error", err,
			"run_id", runID,
			"org_id", orgID,
			"request_id", RequestIDFromContext(r.Context()))
	}
	h.completeIdempotentWriteBestEffort(r, orgID, idem, statusCode, resp)
	writeJSON(w, r, statusCode, resp)
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
	if err := decodeJSON(w, r, &req, h.maxRequestBodyBytes); err != nil {
		handleDecodeError(w, r, err)
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

	idem, proceed := h.beginIdempotentWrite(w, r, orgID, run.AgentID, completeRunEndpoint(runID), req)
	if !proceed {
		return
	}

	audit := h.buildAuditEntry(r, orgID, "complete_run", "agent_run", "",
		nil, nil, map[string]any{"agent_id": run.AgentID})
	if err := h.db.CompleteRunWithAudit(r.Context(), orgID, runID, status, req.Metadata, audit); err != nil {
		h.clearIdempotentWrite(r, orgID, idem)
		h.writeInternalError(w, r, "failed to complete run", err)
		return
	}

	updated, err := h.db.GetRun(r.Context(), orgID, runID)
	if err != nil {
		h.logger.Warn("complete run: read-back failed", "error", err, "run_id", runID)
		resp := map[string]any{"run_id": runID, "status": string(status)}
		h.completeIdempotentWriteBestEffort(r, orgID, idem, http.StatusOK, resp)
		writeJSON(w, r, http.StatusOK, resp)
		return
	}
	h.completeIdempotentWriteBestEffort(r, orgID, idem, http.StatusOK, updated)
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

	// When ?include=enrichments is set, fetch revisions, lineage, conflicts,
	// and integrity status for every decision in the run.
	//
	// Lineage and conflicts are fetched via batch queries (3 total queries
	// instead of N*3). Revisions still use per-decision recursive CTEs but
	// run concurrently with a concurrency cap. Integrity is CPU-only (hash
	// recompute) and runs inline.
	//
	// Cap the number of enriched decisions to avoid unbounded fan-out.
	const maxEnrichedDecisions = 200
	const maxEnrichmentConflicts = 50

	var enrichments map[string]decisionEnrichment
	var enrichmentsTruncated bool
	// TODO: if more include options are added, switch to comma-split or
	// repeated ?include= params (e.g. "enrichments,metrics") instead of
	// equality check — the current form won't compose.
	if r.URL.Query().Get("include") == "enrichments" && len(decisions) > 0 {
		toEnrich := decisions
		if len(toEnrich) > maxEnrichedDecisions {
			toEnrich = toEnrich[:maxEnrichedDecisions]
			enrichmentsTruncated = true
		}

		decIDs := make([]uuid.UUID, len(toEnrich))
		for i, d := range toEnrich {
			decIDs[i] = d.ID
		}

		// Phase 1: Batch-fetch lineage and conflicts concurrently.
		// These replace N*3 individual queries with 3+1 total queries.
		var batchLineage map[uuid.UUID]storage.DecisionLineage
		var batchConflicts map[uuid.UUID][]model.DecisionConflict
		var batchConflictTotals map[uuid.UUID]int // pre-RBAC-filter counts
		var lineageDegraded, conflictsDegraded atomic.Bool

		g, gctx := errgroup.WithContext(r.Context())

		g.Go(func() error {
			var err error
			batchLineage, err = h.db.GetDecisionLineageBatch(gctx, decIDs, orgID, 20)
			if err != nil {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					h.logger.Warn("enrichment: batch lineage failed", "error", err)
				}
				lineageDegraded.Store(true)
			}
			return nil
		})

		g.Go(func() error {
			batchResult, err := h.db.ListConflictsByDecisionIDs(gctx, orgID, decIDs, maxEnrichmentConflicts)
			if err != nil {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					h.logger.Warn("enrichment: batch conflicts failed", "error", err)
				}
				conflictsDegraded.Store(true)
				return nil
			}
			if batchResult.GlobalTruncated {
				h.logger.Warn("enrichment: batch conflicts hit global row cap — some decisions may have incomplete conflict lists",
					"decision_count", len(decIDs))
				conflictsDegraded.Store(true)
			}
			raw := batchResult.ByDecision
			// Track pre-filter totals, then apply access filtering.
			totals := make(map[uuid.UUID]int, len(raw))
			for decID, conflicts := range raw {
				totals[decID] = len(conflicts)
				filtered, filterErr := filterConflictsByAccess(gctx, h.db, claims, conflicts, h.grantCache)
				if filterErr != nil {
					if !errors.Is(filterErr, context.Canceled) && !errors.Is(filterErr, context.DeadlineExceeded) {
						h.logger.Warn("enrichment: access filter failed for batch conflicts",
							"decision_id", decID, "error", filterErr)
					}
					conflictsDegraded.Store(true)
					continue
				}
				raw[decID] = filtered
			}
			batchConflictTotals = totals
			batchConflicts = raw
			return nil
		})

		_ = g.Wait()

		// Phase 2: Fetch revisions concurrently (recursive CTE, per-decision).
		enrichments = make(map[string]decisionEnrichment, len(toEnrich))
		var mu sync.Mutex

		g2, gctx2 := errgroup.WithContext(r.Context())
		g2.SetLimit(8)

		for _, d := range toEnrich {
			g2.Go(func() error {
				// Bail early if the client disconnected or the request context
				// was cancelled — avoids spurious DB calls and warning logs
				// when the client hangs up mid-enrichment.
				if gctx2.Err() != nil {
					return nil
				}

				decID := d.ID
				var entry decisionEnrichment

				// isCtxErr returns true for context cancellation/deadline
				// errors, which should not be logged as warnings since they
				// simply mean the client went away.
				isCtxErr := func(err error) bool {
					return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
				}

				// --- Revisions (per-decision recursive CTE) ---
				revisions, err := h.db.GetDecisionRevisions(gctx2, orgID, decID)
				if err != nil {
					if isCtxErr(err) {
						return nil
					}
					if !isNotFoundError(err) {
						h.logger.Warn("enrichment: failed to get revisions",
							"decision_id", decID, "error", err)
						entry.Revisions = enrichmentRevisions{Items: []model.Decision{}, Degraded: true}
						entry.Degraded = true
					} else {
						entry.Revisions = enrichmentRevisions{Items: []model.Decision{}}
					}
				} else {
					totalRevisions := len(revisions)
					revisions, filterErr := filterDecisionsByAccess(gctx2, h.db, claims, revisions, h.grantCache)
					if filterErr != nil {
						if isCtxErr(filterErr) {
							return nil
						}
						h.logger.Warn("enrichment: access filter failed for revisions",
							"decision_id", decID, "error", filterErr)
						entry.Revisions = enrichmentRevisions{Items: []model.Decision{}, Degraded: true}
						entry.Degraded = true
					} else {
						er := enrichmentRevisions{Items: revisions, Count: len(revisions)}
						if totalRevisions != len(revisions) {
							er.Total = totalRevisions
						}
						entry.Revisions = er
					}
				}

				// --- Lineage (from batch result) ---
				switch {
				case lineageDegraded.Load():
					entry.Lineage = storage.DecisionLineage{DecisionID: decID}
					entry.Degraded = true
				case batchLineage != nil:
					if l, ok := batchLineage[decID]; ok {
						entry.Lineage = l
					} else {
						entry.Lineage = storage.DecisionLineage{DecisionID: decID}
					}
				default:
					entry.Lineage = storage.DecisionLineage{DecisionID: decID}
				}

				// --- Conflicts (from batch result) ---
				switch {
				case conflictsDegraded.Load():
					entry.Conflicts = enrichmentConflicts{Items: []model.DecisionConflict{}, Degraded: true}
					entry.Degraded = true
				case batchConflicts != nil:
					conflicts := batchConflicts[decID] // already RBAC-filtered in Phase 1
					if conflicts == nil {
						conflicts = []model.DecisionConflict{}
					}
					// Pre-filter total from Phase 1 (before RBAC).
					preFilterTotal := batchConflictTotals[decID]
					hasMore := preFilterTotal > maxEnrichmentConflicts
					if len(conflicts) > maxEnrichmentConflicts {
						conflicts = conflicts[:maxEnrichmentConflicts]
					}
					ec := enrichmentConflicts{
						Items:   conflicts,
						Count:   len(conflicts),
						HasMore: hasMore,
					}
					if preFilterTotal != len(conflicts) {
						ec.Total = preFilterTotal
					}
					entry.Conflicts = ec
				default:
					entry.Conflicts = enrichmentConflicts{Items: []model.DecisionConflict{}}
				}

				// --- Integrity (CPU-only, no DB call) ---
				if d.ContentHash == "" {
					entry.Integrity = enrichmentIntegrity{Status: "no_hash"}
				} else {
					valid := integrity.VerifyContentHash(d.ContentHash, d.ID, d.DecisionType, d.Outcome, d.Confidence, d.Reasoning, d.ValidFrom)
					status := "tampered"
					if valid {
						status = "verified"
					}
					entry.Integrity = enrichmentIntegrity{Status: status, ContentHash: d.ContentHash}
				}

				mu.Lock()
				enrichments[decID.String()] = entry
				mu.Unlock()
				return nil
			})
		}

		// Invariant: every goroutine in this errgroup returns nil — errors are
		// logged and surfaced via per-decision degraded flags inside the closure,
		// not propagated. We must still call Wait() to ensure all goroutines
		// complete before reading enrichments. Do not add "return err" to the
		// closure without also adding a response-level degraded signal, because
		// errgroup.WithContext cancels gctx on the first non-nil error.
		_ = g2.Wait()

		// If the request was cancelled mid-flight, some enrichments may be
		// incomplete. Insert fully-degraded stubs for any decision that never
		// got an entry, so absence in the map is unambiguous (= not requested)
		// rather than ambiguous (= cancelled? errored? empty?).
		if r.Context().Err() != nil {
			enrichmentsTruncated = true
			for _, d := range toEnrich {
				key := d.ID.String()
				if _, exists := enrichments[key]; !exists {
					enrichments[key] = decisionEnrichment{
						Revisions: enrichmentRevisions{Items: []model.Decision{}, Degraded: true},
						Lineage:   storage.DecisionLineage{DecisionID: d.ID},
						Conflicts: enrichmentConflicts{Items: []model.DecisionConflict{}, Degraded: true},
						Integrity: enrichmentIntegrity{Status: "no_hash"},
						Degraded:  true,
					}
				}
			}
		}
	}

	resp := getRunResponse{
		Run:       run,
		Events:    events,
		Decisions: decisions,
	}
	if len(events) >= maxRunEvents {
		resp.TruncatedEvents = true
		resp.Truncated = true
	}
	// Compare against len(decisions), NOT the requested limit. The storage
	// layer may silently cap the limit (e.g. QueryDecisions caps at 1000),
	// so comparing against maxRunDecisions would miss truncation whenever
	// the actual row count falls between the storage cap and our ceiling.
	if total > len(decisions) {
		resp.TruncatedDecisions = true
		resp.TotalDecisions = total
		resp.Truncated = true
	}
	if enrichments != nil {
		resp.DecisionEnrichments = enrichments
		if enrichmentsTruncated {
			resp.TruncatedEnrichments = true
			resp.EnrichedCount = len(enrichments)
			resp.Truncated = true
		}
	}
	writeJSON(w, r, http.StatusOK, resp)
}
