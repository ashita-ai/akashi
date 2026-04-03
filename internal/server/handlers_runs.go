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

	"github.com/ashita-ai/akashi/internal/auth"
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
	Count    int              `json:"count"` // Number of accessible items returned.
	Total    int              `json:"total"` // Total accessible revisions (post access filtering).
	Degraded bool             `json:"degraded,omitempty"`
}

// enrichmentConflicts holds conflicts for a single decision.
// Count is the number of items returned (post-RBAC, post-truncation).
// Total is the pre-RBAC count from the storage layer; omitted (zero) when
// equal to Count so consumers can detect when RBAC or truncation hid conflicts.
type enrichmentConflicts struct {
	Items    []model.DecisionConflict `json:"items"`
	Count    int                      `json:"count"`    // Number of accessible items returned (may be capped).
	Total    int                      `json:"total"`    // Pre-RBAC conflict count; 0 when equal to Count.
	HasMore  bool                     `json:"has_more"` // True when pre-RBAC count exceeds the cap.
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
	runID, err := parsePathUUID(r, "run_id")
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

	// AppendWithAudit buffers events and their audit entry under a single lock
	// acquisition, closing the race where a background flush could drain events
	// before the audit entry was buffered (see issue #608).
	events, err := h.buffer.AppendWithAudit(
		r.Context(), runID, run.AgentID, run.OrgID, req.Events,
		func(evts []model.AgentEvent) storage.MutationAuditEntry {
			eventIDs := make([]uuid.UUID, len(evts))
			for i, e := range evts {
				eventIDs[i] = e.ID
			}
			return h.buildAuditEntry(
				r, orgID,
				"append_events", "agent_run", runID.String(),
				nil,
				map[string]any{
					"accepted":  len(evts),
					"event_ids": eventIDs,
					"status":    "persisted",
					"message":   "events durably persisted",
				},
				map[string]any{"agent_id": run.AgentID, "event_count": len(evts)},
			)
		},
	)
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

	// After AppendWithAudit succeeds, events and audit are in the buffer and
	// WILL be persisted by the background flush loop (barring process crash).
	// From this point we must NEVER clear the idempotency key — doing so
	// allows retries to create duplicate events. See issue #65.

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
	h.completeIdempotentWriteBestEffort(r, orgID, idem, statusCode, resp)
	writeJSON(w, r, statusCode, resp)
}

// HandleCompleteRun handles POST /v1/runs/{run_id}/complete.
func (h *Handlers) HandleCompleteRun(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFromContext(r.Context())
	orgID := OrgIDFromContext(r.Context())
	runID, err := parsePathUUID(r, "run_id")
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
	runID, err := parsePathUUID(r, "run_id")
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

	// TODO: if more include options are added, switch to comma-split or
	// repeated ?include= params (e.g. "enrichments,metrics") instead of
	// equality check — the current form won't compose.
	var enrichments map[string]decisionEnrichment
	var enrichmentsTruncated bool
	if r.URL.Query().Get("include") == "enrichments" && len(decisions) > 0 {
		enrichments, enrichmentsTruncated = h.buildDecisionEnrichments(r.Context(), orgID, claims, decisions)
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

// ---------------------------------------------------------------------------
// buildDecisionEnrichments fetches revisions, lineage, conflicts, and integrity
// status for the given decisions. Returns the enrichment map and whether the
// enrichment set was truncated (i.e. more decisions exist than the cap).
//
// Lineage and conflicts are batch-fetched (3+1 queries). Revisions use
// per-decision recursive CTEs with a concurrency cap. Integrity is CPU-only.
// ---------------------------------------------------------------------------

const (
	maxEnrichedDecisions    = 200
	maxEnrichmentConflicts  = 50
	enrichmentRevisionFanIn = 8
)

func (h *Handlers) buildDecisionEnrichments(
	ctx context.Context,
	orgID uuid.UUID,
	claims *auth.Claims,
	decisions []model.Decision,
) (map[string]decisionEnrichment, bool) {
	toEnrich := decisions
	truncated := false
	if len(toEnrich) > maxEnrichedDecisions {
		toEnrich = toEnrich[:maxEnrichedDecisions]
		truncated = true
	}

	decIDs := make([]uuid.UUID, len(toEnrich))
	for i, d := range toEnrich {
		decIDs[i] = d.ID
	}

	// Phase 1: Batch-fetch lineage and conflicts concurrently.
	var batchLineage map[uuid.UUID]storage.DecisionLineage
	var batchConflicts map[uuid.UUID][]model.DecisionConflict
	var batchConflictTotals map[uuid.UUID]int // pre-RBAC-filter counts
	var lineageDegraded, conflictsDegraded atomic.Bool

	g, gctx := errgroup.WithContext(ctx)

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

	// Phase 2: Per-decision enrichment (revisions, lineage RBAC, conflicts assembly, integrity).
	enrichments := make(map[string]decisionEnrichment, len(toEnrich))
	var mu sync.Mutex

	g2, gctx2 := errgroup.WithContext(ctx)
	g2.SetLimit(enrichmentRevisionFanIn)

	for _, d := range toEnrich {
		g2.Go(func() error {
			entry := h.buildSingleEnrichment(gctx2, orgID, claims, d,
				batchLineage, &lineageDegraded,
				batchConflicts, batchConflictTotals, &conflictsDegraded)
			mu.Lock()
			enrichments[d.ID.String()] = entry
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
	// incomplete. Insert fully-degraded stubs so absence in the map is
	// unambiguous (= not requested) rather than ambiguous (= cancelled?).
	if ctx.Err() != nil {
		truncated = true
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

	return enrichments, truncated
}

// buildSingleEnrichment assembles the enrichment for one decision from the
// batch-fetched lineage/conflict data and per-decision revision queries.
func (h *Handlers) buildSingleEnrichment(
	ctx context.Context,
	orgID uuid.UUID,
	claims *auth.Claims,
	d model.Decision,
	batchLineage map[uuid.UUID]storage.DecisionLineage,
	lineageDegraded *atomic.Bool,
	batchConflicts map[uuid.UUID][]model.DecisionConflict,
	batchConflictTotals map[uuid.UUID]int,
	conflictsDegraded *atomic.Bool,
) decisionEnrichment {
	decID := d.ID
	var entry decisionEnrichment

	// Bail early if the client disconnected or the request context
	// was cancelled — avoids spurious DB calls and warning logs
	// when the client hangs up mid-enrichment.
	if ctx.Err() != nil {
		return entry
	}

	// isCtxErr returns true for context cancellation/deadline errors,
	// which should not be logged as warnings since they simply mean the
	// client went away.
	isCtxErr := func(err error) bool {
		return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	}

	// --- Revisions (per-decision recursive CTE) ---
	revisions, err := h.db.GetDecisionRevisions(ctx, orgID, decID)
	if err != nil {
		if isCtxErr(err) {
			return entry
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
		revisions, filterErr := filterDecisionsByAccess(ctx, h.db, claims, revisions, h.grantCache)
		if filterErr != nil {
			if isCtxErr(filterErr) {
				return entry
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

	// --- Lineage (from batch result, then RBAC-filtered via FilterLineage) ---
	switch {
	case lineageDegraded.Load():
		entry.Lineage = storage.DecisionLineage{DecisionID: decID}
		entry.Degraded = true
	case batchLineage != nil:
		if lineage, ok := batchLineage[decID]; ok {
			filtered, fErr := filterLineageByAccess(ctx, h.db, claims, lineage, h.grantCache)
			if fErr != nil {
				if !isCtxErr(fErr) {
					h.logger.Warn("enrichment: access filter failed for lineage",
						"decision_id", decID, "error", fErr)
				}
				entry.Lineage = storage.DecisionLineage{DecisionID: decID}
				entry.Degraded = true
			} else {
				entry.Lineage = filtered
			}
		} else {
			entry.Lineage = storage.DecisionLineage{DecisionID: decID}
		}
	default:
		entry.Lineage = storage.DecisionLineage{DecisionID: decID}
	}

	// --- Conflicts (from batch result, already RBAC-filtered in Phase 1) ---
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

	return entry
}
