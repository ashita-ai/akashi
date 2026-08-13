package akashi

import (
	"context"
	"encoding/json"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/conflicts"
	"github.com/ashita-ai/akashi/internal/search"
	"github.com/ashita-ai/akashi/internal/storage"
)

// runLoop runs fn on every tick of interval until ctx is cancelled.
// It recovers from panics in fn and logs them so a single bad tick
// cannot kill a background goroutine.
func (a *App) runLoop(ctx context.Context, name string, interval time.Duration, fn func(ctx context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.safeRun(ctx, name, fn)
		}
	}
}

// safeRun executes fn with panic recovery. Extracted from runLoop so that a
// one-shot pass performed outside the ticker (such as the conflict backfill's
// startup sweep) carries the same protection a ticked pass does — otherwise a
// panic on the startup path takes the process down where the identical panic on
// the periodic path would only be logged.
func (a *App) safeRun(ctx context.Context, name string, fn func(ctx context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			a.logger.Error("panic in background loop",
				"loop", name,
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}()
	fn(ctx)
}

func (a *App) conflictBackfillLoop(ctx context.Context) {
	// Warm up the Ollama model before the backfill starts. Without this the
	// first backfill request pays the full cold-start cost (model load from
	// disk), which can exceed the per-call timeout on CPU hardware.
	if v, ok := a.conflictScorer.Validator().(*conflicts.OllamaValidator); ok {
		a.logger.Info("conflict backfill: warming up ollama model")
		if err := v.Warmup(ctx); err != nil {
			a.logger.Warn("conflict backfill: ollama warmup failed (will proceed anyway)", "error", err)
		} else {
			a.logger.Info("conflict backfill: ollama model ready")
		}
	}
	// Drain whatever is already pending, then keep sweeping. This ran exactly
	// once with a hardcoded batch of 500 until #745: the function was registered
	// alongside the genuine ticker loops and named like one, but scored a single
	// batch and returned. Any backlog larger than one batch — an operator-forced
	// rescore being the obvious case, which resets every decision's scored mark —
	// silently stopped at 500 and never resumed without a process restart.
	a.safeRun(ctx, "conflictBackfill", a.sweepConflictBackfill)
	a.runLoop(ctx, "conflictBackfill", a.cfg.ConflictBackfillInterval, a.sweepConflictBackfill)
}

// conflictBackfillMaxBatches bounds a single backfill sweep. BackfillScoring
// marks each decision scored as it goes, so the drain normally ends when a short
// batch comes back; this bound only matters if a decision is somehow never
// marked, which would otherwise re-fetch the same rows forever and spin the LLM
// judge hot. It is deliberately high enough that a real backlog drains in one
// sweep (200 * the default 500 = 100k decisions).
const conflictBackfillMaxBatches = 200

// sweepConflictBackfill drains the pending-scoring queue and reports what it did.
func (a *App) sweepConflictBackfill(ctx context.Context) {
	n := drainConflictBackfill(ctx, a.conflictScorer, a.cfg.ConflictBackfillBatchSize,
		conflictBackfillMaxBatches, a.logger)
	if n > 0 {
		a.logger.Info("conflict scoring backfill complete", "decisions_scored", n)
	}
}

// backfillScorer is the slice of the conflict scorer the drain needs, narrowed to
// an interface so the drain logic is testable without a database or an LLM.
type backfillScorer interface {
	BackfillScoring(ctx context.Context, batchSize int) (int, error)
}

// drainConflictBackfill scores pending decisions batch by batch until the queue
// is empty, returning the total number of decisions processed.
//
// Termination rests on BackfillScoring's contract: it processes at most
// batchSize decisions and marks each one scored, so a batch shorter than
// batchSize means nothing is left to score. TestBackfillScoring_MarksDecisionsScored
// pins that invariant — a second call over the same data returns 0.
//
// A batch error stops the sweep rather than retrying in place: the next tick
// retries from the same position, and continuing past an error risks hammering a
// failing database or judge. The error is logged with the count achieved so far
// so a partial sweep is never reported as a clean one.
func drainConflictBackfill(ctx context.Context, s backfillScorer, batchSize, maxBatches int, logger *slog.Logger) int {
	total := 0
	for range maxBatches {
		if ctx.Err() != nil {
			return total
		}
		n, err := s.BackfillScoring(ctx, batchSize)
		total += n
		if err != nil {
			logger.Warn("conflict scoring backfill failed",
				"error", err, "decisions_scored", total)
			return total
		}
		if n < batchSize {
			return total
		}
	}
	logger.Warn("conflict scoring backfill hit its per-sweep batch bound — remaining decisions wait for the next sweep",
		"max_batches", maxBatches, "batch_size", batchSize, "decisions_scored", total)
	return total
}

func (a *App) conflictRefreshLoop(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.ConflictRefreshInterval)
	defer ticker.Stop()

	lastNotifiedAt := make(map[uuid.UUID]time.Time)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			opCtx, cancel := context.WithTimeout(ctx, conflictRefreshTimeout(a.cfg.ConflictRefreshInterval))
			if err := a.db.RefreshConflicts(opCtx); err != nil {
				cancel()
				a.logger.Warn("conflict refresh failed", "error", err)
				continue
			}
			if err := a.db.RefreshAgentState(opCtx); err != nil {
				a.logger.Warn("agent state refresh failed", "error", err)
			}

			orgIDs, err := a.db.ListOrganizationIDs(opCtx)
			if err != nil {
				cancel()
				a.logger.Warn("conflict org list failed", "error", err)
				continue
			}

			var totalNotified int
			for _, orgID := range orgIDs {
				since, ok := lastNotifiedAt[orgID]
				if !ok {
					since = time.Now().UTC()
					lastNotifiedAt[orgID] = since
				}

				newConflicts, err := a.db.NewConflictsSinceByOrg(opCtx, orgID, since, 1000)
				if err != nil {
					a.logger.Warn("new conflicts query failed", "error", err, "org_id", orgID)
					continue
				}

				for _, c := range newConflicts {
					// Fire OnConflictDetected hooks asynchronously.
					if len(a.decisionHooks) > 0 {
						conflict := c
						hooks := a.decisionHooks
						logger := a.logger
						go func() {
							hookCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							defer cancel()
							for _, h := range hooks {
								if err := h.OnConflictDetected(hookCtx, conflict); err != nil {
									logger.Warn("event hook OnConflictDetected failed", "error", err)
								}
							}
						}()
					}

					payload, err := json.Marshal(map[string]any{
						"org_id":        c.OrgID,
						"conflict_kind": c.ConflictKind,
						"decision_a_id": c.DecisionAID,
						"decision_b_id": c.DecisionBID,
						"agent_a":       c.AgentA,
						"agent_b":       c.AgentB,
						"decision_type": c.DecisionType,
					})
					if err != nil {
						a.logger.Warn("conflict notify marshal failed", "error", err)
						continue
					}
					if err := a.db.Notify(opCtx, storage.ChannelConflicts, string(payload)); err != nil {
						a.logger.Warn("conflict notify failed", "error", err)
					}
					if c.DetectedAt.After(lastNotifiedAt[orgID]) {
						lastNotifiedAt[orgID] = c.DetectedAt
					}
					totalNotified++
				}
			}
			cancel()

			if totalNotified > 0 {
				a.logger.Info("conflict notifications sent", "count", totalNotified)
			}
		}
	}
}

func (a *App) idempotencyCleanupLoop(ctx context.Context) {
	a.runLoop(ctx, "idempotencyCleanup", a.cfg.IdempotencyCleanupInterval, func(ctx context.Context) {
		opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		deleted, err := a.db.CleanupIdempotencyKeys(opCtx, a.cfg.IdempotencyCompletedTTL, a.cfg.IdempotencyAbandonedTTL)
		if err != nil {
			a.logger.Warn("idempotency cleanup failed", "error", err)
			return
		}
		if deleted > 0 {
			a.logger.Info("idempotency cleanup deleted rows", "deleted", deleted)
		}
	})
}

func (a *App) hookCheckCleanupLoop(ctx context.Context) {
	a.runLoop(ctx, "hookCheckCleanup", 10*time.Minute, func(_ context.Context) {
		a.srv.Handlers().CleanupHookChecks()
	})
}

func (a *App) retentionLoop(ctx context.Context) {
	if a.cfg.RetentionInterval <= 0 {
		return
	}
	a.runLoop(ctx, "retention", a.cfg.RetentionInterval, func(ctx context.Context) {
		a.runRetention(ctx)
	})
}

func (a *App) claimEmbeddingRetryLoop(ctx context.Context) {
	if a.cfg.ClaimRetryInterval <= 0 {
		return
	}
	a.runLoop(ctx, "claimEmbeddingRetry", a.cfg.ClaimRetryInterval, func(ctx context.Context) {
		opCtx, cancel := context.WithTimeout(ctx, a.cfg.ClaimRetryInterval)
		defer cancel()
		n, err := a.decisionSvc.RetryFailedClaimEmbeddings(opCtx, 50, 3)
		if err != nil {
			a.logger.Warn("claim embedding retry failed", "error", err)
		} else if n > 0 {
			a.logger.Info("claim embedding retry complete", "retried", n)
		}
	})
}

// percentileRefreshLoop periodically recomputes signal percentile breakpoints for all orgs
// and stores them in the in-memory cache. ReScore uses these to normalize citation counts
// into distribution-aware [0,1] scores instead of using the hardcoded log saturation formula.
func (a *App) percentileRefreshLoop(ctx context.Context) {
	if a.cfg.PercentileRefreshInterval <= 0 {
		return
	}

	// Refresh immediately on startup to populate the cache before the first search.
	a.refreshPercentiles(ctx)

	ticker := time.NewTicker(a.cfg.PercentileRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.refreshPercentiles(ctx)
		}
	}
}

func (a *App) refreshPercentiles(ctx context.Context) {
	opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	orgIDs, err := a.db.ListOrganizationIDs(opCtx)
	if err != nil {
		a.logger.Warn("percentile refresh: list orgs", "error", err)
		return
	}

	refreshed := 0
	for _, orgID := range orgIDs {
		bp, err := a.db.GetCitationPercentilesForOrg(opCtx, orgID)
		if err != nil {
			a.logger.Warn("percentile refresh: org query failed", "org_id", orgID, "error", err)
			continue
		}
		a.percentileCache.Set(orgID, search.OrgPercentiles{
			CitationBreakpoints: bp,
			RefreshedAt:         time.Now(),
		})
		refreshed++
	}

	if refreshed > 0 {
		a.logger.Debug("percentile refresh complete", "orgs", refreshed)
	}
}

// autoResolveLoop periodically runs auto-resolution of conflicts based on org policies.
func (a *App) autoResolveLoop(ctx context.Context) {
	if a.cfg.AutoResolveInterval <= 0 {
		return
	}
	a.runLoop(ctx, "autoResolve", a.cfg.AutoResolveInterval, func(ctx context.Context) {
		if err := a.autoResolver.RunOnce(ctx); err != nil {
			a.logger.Warn("auto-resolve loop failed", "error", err)
		}
	})
}

// runRetention processes data retention policies for all orgs that have a
// retention_days set. Each org gets its own deletion_log entry. A global
// pass also prunes detector-inferred supersedes suggestions older than
// SupersedesSuggestionTTL — these are operational hints, not user data, and
// are pruned on a single global TTL regardless of per-org policy.
func (a *App) runRetention(ctx context.Context) {
	opCtx, cancel := context.WithTimeout(ctx, a.cfg.RetentionInterval/2)
	defer cancel()

	a.pruneStaleSupersedesSuggestions(opCtx)

	orgs, err := a.db.GetOrgsWithRetention(opCtx)
	if err != nil {
		a.logger.Warn("retention: failed to list orgs with policy", "error", err)
		return
	}
	if len(orgs) == 0 {
		return
	}

	for _, org := range orgs {
		cutoff := time.Now().UTC().AddDate(0, 0, -org.RetentionDays)
		criteria := map[string]any{"before": cutoff, "retention_days": org.RetentionDays}
		if len(org.RetentionExcludeTypes) > 0 {
			criteria["exclude_types"] = org.RetentionExcludeTypes
		}

		logID, err := a.db.StartDeletionLog(opCtx, org.OrgID, "policy", "", criteria)
		if err != nil {
			a.logger.Warn("retention: failed to start deletion log", "org_id", org.OrgID, "error", err)
			continue
		}

		counts, err := a.db.BatchDeleteDecisions(opCtx, org.OrgID, cutoff, nil, nil, org.RetentionExcludeTypes, 1000)
		if err != nil {
			a.logger.Warn("retention: batch delete failed", "org_id", org.OrgID, "error", err)
			// Still complete the log even on partial failure so the run is recorded.
		}

		countMap := map[string]any{
			"decisions":    counts.Decisions,
			"alternatives": counts.Alternatives,
			"evidence":     counts.Evidence,
			"claims":       counts.Claims,
			"events":       counts.Events,
		}
		if cerr := a.db.CompleteDeletionLog(opCtx, org.OrgID, logID, countMap); cerr != nil {
			a.logger.Warn("retention: failed to complete deletion log", "org_id", org.OrgID, "error", cerr)
		}

		if counts.Decisions > 0 || counts.Events > 0 {
			a.logger.Info("retention: deleted stale records",
				"org_id", org.OrgID,
				"decisions", counts.Decisions,
				"events", counts.Events,
				"cutoff", cutoff,
			)
		}
	}
}

// pruneStaleSupersedesSuggestions removes detector-inferred supersedes
// suggestions that an agent never confirmed. Suggestions confirmed by a
// re-trace are retired by the migration-106 trigger at confirm time; this
// global TTL pass cleans up the long tail. Failures are warn-logged but do
// not abort the retention run — the next tick will retry.
func (a *App) pruneStaleSupersedesSuggestions(ctx context.Context) {
	if a.cfg.SupersedesSuggestionTTL <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-a.cfg.SupersedesSuggestionTTL)
	n, err := a.db.DeleteOldSupersedesSuggestions(ctx, cutoff)
	if err != nil {
		a.logger.Warn("retention: prune supersedes suggestions failed", "error", err, "cutoff", cutoff)
		return
	}
	if n > 0 {
		a.logger.Info("retention: pruned stale supersedes suggestions", "count", n, "cutoff", cutoff)
	}
}

func contextWithOptionalTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

func conflictRefreshTimeout(interval time.Duration) time.Duration {
	const max = 15 * time.Second
	if interval < max {
		return interval
	}
	return max
}
