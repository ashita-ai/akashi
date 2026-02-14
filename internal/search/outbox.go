package search

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"go.opentelemetry.io/otel/metric"

	"github.com/ashita-ai/akashi/internal/telemetry"
)

// outboxEntry represents a single row from the search_outbox table.
type outboxEntry struct {
	ID         int64
	DecisionID uuid.UUID
	OrgID      uuid.UUID
	Operation  string
	Attempts   int
}

// DecisionForIndex holds the fields needed to build a Qdrant point.
// Populated by the outbox worker from Postgres.
type DecisionForIndex struct {
	ID           uuid.UUID
	OrgID        uuid.UUID
	AgentID      string
	DecisionType string
	Confidence   float32
	QualityScore float32
	ValidFrom    time.Time
	Embedding    []float32
	SessionID    *uuid.UUID
	AgentContext map[string]any
}

// OutboxWorker polls the search_outbox table and syncs changes to Qdrant.
type OutboxWorker struct {
	pool         *pgxpool.Pool
	index        *QdrantIndex
	logger       *slog.Logger
	pollInterval time.Duration
	batchSize    int

	started     atomic.Bool
	cancelLoop  context.CancelFunc
	done        chan struct{}
	once        sync.Once // guards close(done)
	drainOnce   sync.Once // guards Drain to prevent double-drain panics
	lastCleanup time.Time
	drainCh     chan context.Context // carries the drain context to pollLoop for the final poll
}

// NewOutboxWorker creates a new outbox worker.
func NewOutboxWorker(pool *pgxpool.Pool, index *QdrantIndex, logger *slog.Logger, pollInterval time.Duration, batchSize int) *OutboxWorker {
	return &OutboxWorker{
		pool:         pool,
		index:        index,
		logger:       logger,
		pollInterval: pollInterval,
		batchSize:    batchSize,
		done:         make(chan struct{}),
		drainCh:      make(chan context.Context, 1),
	}
}

// Start begins the background poll loop. It is safe to call only once;
// subsequent calls are no-ops and log a warning.
func (w *OutboxWorker) Start(ctx context.Context) {
	if !w.started.CompareAndSwap(false, true) {
		w.logger.Warn("search outbox: Start called more than once, ignoring")
		return
	}
	w.registerMetrics()
	loopCtx, cancel := context.WithCancel(ctx)
	w.cancelLoop = cancel
	go w.pollLoop(loopCtx)
}

// Drain signals the poll loop to stop, processes remaining entries, and blocks
// until done or the context expires. The ctx parameter is passed to the final
// poll so it respects the caller's deadline. Safe to call multiple times;
// only the first call triggers the drain.
func (w *OutboxWorker) Drain(ctx context.Context) {
	w.drainOnce.Do(func() {
		// Send the drain context to pollLoop via channel (race-free).
		// Must be sent before cancelLoop so pollLoop can receive it on ctx.Done().
		// Use a short timeout to avoid blocking if the channel is unexpectedly full.
		sendCtx, sendCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		select {
		case w.drainCh <- ctx:
		case <-sendCtx.Done():
			w.logger.Warn("search outbox: drain context channel busy, final poll will use fallback timeout")
		}
		sendCancel()
		if w.cancelLoop != nil {
			w.cancelLoop()
		}
	})
	select {
	case <-w.done:
	case <-ctx.Done():
		w.logger.Warn("search outbox: drain timed out")
	}
}

func (w *OutboxWorker) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final drain: prefer the drain context (sent by Drain via channel)
			// so the final poll respects the caller's deadline.
			var drainCtx context.Context
			select {
			case drainCtx = <-w.drainCh:
			default:
			}
			if drainCtx != nil {
				w.processBatch(drainCtx)
			} else {
				// Fallback for direct cancellation without Drain (e.g., tests).
				fallbackCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				w.processBatch(fallbackCtx)
				cancel()
			}
			w.once.Do(func() { close(w.done) })
			return
		case <-ticker.C:
			batchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			w.processBatch(batchCtx)
			cancel()
		}
	}
}

// maxOutboxAttempts must match the partial index predicate in migration 023
// (WHERE attempts < 10). Changing this value requires a new migration.
const maxOutboxAttempts = 10

func (w *OutboxWorker) processBatch(ctx context.Context) {
	if w.pool == nil {
		w.logger.Warn("search outbox: skipping batch, pool is nil")
		return
	}
	if w.index == nil {
		w.logger.Warn("search outbox: skipping batch, index is nil")
		return
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		w.logger.Error("search outbox: begin tx", "error", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Select and lock pending entries.
	rows, err := tx.Query(ctx,
		`SELECT id, decision_id, org_id, operation, attempts
		 FROM search_outbox
		 WHERE (locked_until IS NULL OR locked_until < now())
		   AND attempts < $1
		 ORDER BY created_at ASC
		 LIMIT $2
		 FOR UPDATE SKIP LOCKED`,
		maxOutboxAttempts, w.batchSize,
	)
	if err != nil {
		w.logger.Error("search outbox: select pending", "error", err)
		return
	}

	entries, err := scanOutboxEntries(rows)
	if err != nil {
		w.logger.Error("search outbox: scan entries", "error", err)
		return
	}
	if len(entries) == 0 {
		return
	}

	// Lock the entries for 60 seconds (must exceed the 30s batchCtx timeout
	// to prevent a second worker from picking up entries whose lock expired
	// while the first worker is still processing).
	entryIDs := make([]int64, len(entries))
	for i, e := range entries {
		entryIDs[i] = e.ID
	}
	if _, err := tx.Exec(ctx,
		`UPDATE search_outbox SET locked_until = now() + interval '60 seconds' WHERE id = ANY($1)`,
		entryIDs,
	); err != nil {
		w.logger.Error("search outbox: lock entries", "error", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		w.logger.Error("search outbox: commit lock", "error", err)
		return
	}

	// Process upserts and deletes separately.
	var upserts []outboxEntry
	var deletes []outboxEntry
	for _, e := range entries {
		switch e.Operation {
		case "upsert":
			upserts = append(upserts, e)
		case "delete":
			deletes = append(deletes, e)
		}
	}

	if len(upserts) > 0 {
		w.processUpserts(ctx, upserts)
	}
	if len(deletes) > 0 {
		w.processDeletes(ctx, deletes)
	}

	// Periodically clean up dead-letter entries (attempts >= max, older than 7 days).
	if time.Since(w.lastCleanup) > time.Hour {
		w.cleanupDeadLetters(ctx)
		w.lastCleanup = time.Now()
	}
}

func (w *OutboxWorker) cleanupDeadLetters(ctx context.Context) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		w.logger.Error("search outbox: begin dead-letter cleanup tx failed", "error", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`WITH candidates AS (
		    SELECT id, decision_id, org_id, operation, attempts, last_error, created_at, locked_until
		    FROM search_outbox
		    WHERE attempts >= $1
		      AND (locked_until IS NULL OR locked_until < now())
		      AND created_at < now() - interval '7 days'
		    FOR UPDATE SKIP LOCKED
		)
		INSERT INTO search_outbox_dead_letters (
		    outbox_id, decision_id, org_id, operation, attempts, last_error, created_at, locked_until
		)
		SELECT id, decision_id, org_id, operation, attempts, last_error, created_at, locked_until
		FROM candidates
		ON CONFLICT (outbox_id) DO NOTHING`,
		maxOutboxAttempts,
	); err != nil {
		w.logger.Error("search outbox: archive dead-letters failed", "error", err)
		return
	}

	tag, err := tx.Exec(ctx,
		`DELETE FROM search_outbox s
		 WHERE s.attempts >= $1
		   AND (s.locked_until IS NULL OR s.locked_until < now())
		   AND s.created_at < now() - interval '7 days'
		   AND EXISTS (
		     SELECT 1
		     FROM search_outbox_dead_letters d
		     WHERE d.outbox_id = s.id
		   )`,
		maxOutboxAttempts,
	)
	if err != nil {
		w.logger.Error("search outbox: delete archived dead-letters failed", "error", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		w.logger.Error("search outbox: commit dead-letter cleanup failed", "error", err)
		return
	}

	if tag.RowsAffected() > 0 {
		w.logger.Info("search outbox: archived and cleaned dead-letter entries", "deleted", tag.RowsAffected())
	}
}

func (w *OutboxWorker) processUpserts(ctx context.Context, entries []outboxEntry) {
	// Fetch decision data from Postgres.
	decisionIDs := make([]uuid.UUID, len(entries))
	orgIDs := make([]uuid.UUID, len(entries))
	for i, e := range entries {
		decisionIDs[i] = e.DecisionID
		orgIDs[i] = e.OrgID
	}

	decisions, err := w.fetchDecisionsForIndex(ctx, decisionIDs, orgIDs)
	if err != nil {
		w.logger.Error("search outbox: fetch decisions", "error", err, "count", len(decisionIDs))
		w.failEntries(ctx, entries, err.Error())
		return
	}

	readyEntries, readyDecisions, pendingEntries := partitionUpsertEntries(entries, decisions)

	if len(readyEntries) > 0 {
		// Build Qdrant points from fetched decisions.
		points := make([]Point, 0, len(readyDecisions))
		for _, d := range readyDecisions {
			p := Point{
				ID:           d.ID,
				OrgID:        d.OrgID,
				AgentID:      d.AgentID,
				DecisionType: d.DecisionType,
				Confidence:   d.Confidence,
				QualityScore: d.QualityScore,
				ValidFrom:    d.ValidFrom,
				Embedding:    d.Embedding,
				SessionID:    d.SessionID,
			}
			if d.AgentContext != nil {
				if v, ok := d.AgentContext["tool"].(string); ok {
					p.Tool = v
				}
				if v, ok := d.AgentContext["model"].(string); ok {
					p.Model = v
				}
				if v, ok := d.AgentContext["repo"].(string); ok {
					p.Repo = v
				}
			}
			points = append(points, p)
		}

		if err := w.index.Upsert(ctx, points); err != nil {
			w.logger.Error("search outbox: qdrant upsert", "error", err, "count", len(points))
			w.failEntries(ctx, readyEntries, err.Error())
		} else {
			w.succeedEntries(ctx, readyEntries)
			w.logger.Info("search outbox: upserted", "count", len(points))
		}
	}

	if len(pendingEntries) > 0 {
		// Entries with no current embedding (or whose decision row is not visible yet).
		// Defer with incrementing attempts so we eventually dead-letter if backfill never runs.
		// 30-minute backoff gives backfill time; after 10 cycles we surface for ops investigation.
		var toDefer, toFail []outboxEntry
		for _, e := range pendingEntries {
			if e.Attempts >= maxOutboxAttempts-1 {
				toFail = append(toFail, e)
			} else {
				toDefer = append(toDefer, e)
			}
		}
		if len(toFail) > 0 {
			w.failEntries(ctx, toFail, "decision not ready after max defer cycles (missing embedding or not found)")
		}
		if len(toDefer) > 0 {
			w.deferPendingEntries(ctx, toDefer, "decision not ready for indexing (missing embedding or not found)")
		}
	}
}

func (w *OutboxWorker) processDeletes(ctx context.Context, entries []outboxEntry) {
	ids := make([]uuid.UUID, len(entries))
	for i, e := range entries {
		ids[i] = e.DecisionID
	}

	if err := w.index.DeleteByIDs(ctx, ids); err != nil {
		w.logger.Error("search outbox: qdrant delete", "error", err, "count", len(ids))
		w.failEntries(ctx, entries, err.Error())
		return
	}

	w.succeedEntries(ctx, entries)
	w.logger.Info("search outbox: deleted", "count", len(ids))
}

func (w *OutboxWorker) succeedEntries(ctx context.Context, entries []outboxEntry) {
	ids := make([]int64, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	if _, err := w.pool.Exec(ctx,
		`DELETE FROM search_outbox WHERE id = ANY($1)`, ids,
	); err != nil {
		w.logger.Error("search outbox: delete completed entries", "error", err)
	}
}

// deferPendingEntries defers entries, incrementing attempts and using a 30-minute
// backoff. Entries that exceed maxOutboxAttempts are routed to failEntries instead.
// This prevents infinite defer loops when backfill never runs (e.g. noop embedder).
func (w *OutboxWorker) deferPendingEntries(ctx context.Context, entries []outboxEntry, errMsg string) {
	ids := make([]int64, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	if _, err := w.pool.Exec(ctx,
		`UPDATE search_outbox
		 SET attempts = attempts + 1,
		     last_error = $1,
		     locked_until = now() + interval '30 minutes'
		 WHERE id = ANY($2)`,
		errMsg, ids,
	); err != nil {
		w.logger.Error("search outbox: defer pending entries", "error", err)
	}
}

func (w *OutboxWorker) failEntries(ctx context.Context, entries []outboxEntry, errMsg string) {
	ids := make([]int64, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	// Exponential backoff: locked_until = now() + 2^attempts seconds (capped at 5 minutes).
	// Each entry in the batch has the same attempt count (incremented atomically), so
	// the backoff is uniform per batch. This prevents tight retry loops during Qdrant outages.
	if _, err := w.pool.Exec(ctx,
		`UPDATE search_outbox
		 SET attempts = attempts + 1,
		     last_error = $1,
		     locked_until = now() + LEAST(POWER(2, attempts + 1), 300) * interval '1 second'
		 WHERE id = ANY($2)`,
		errMsg, ids,
	); err != nil {
		w.logger.Error("search outbox: update failed entries", "error", err)
	}

	// Log dead-letter entries (attempts >= maxOutboxAttempts after increment).
	for _, e := range entries {
		if e.Attempts+1 >= maxOutboxAttempts {
			w.logger.Warn("search outbox: dead-letter entry",
				"outbox_id", e.ID,
				"decision_id", e.DecisionID,
				"operation", e.Operation,
				"attempts", e.Attempts+1,
			)
		}
	}
}

func (w *OutboxWorker) fetchDecisionsForIndex(ctx context.Context, ids, orgIDs []uuid.UUID) ([]DecisionForIndex, error) {
	if len(ids) == 0 || len(orgIDs) == 0 || len(ids) != len(orgIDs) {
		return nil, nil
	}

	rows, err := w.pool.Query(ctx,
		`SELECT d.id, d.org_id, d.agent_id, d.decision_type, d.confidence, d.quality_score, d.valid_from, d.embedding,
		        d.session_id, d.agent_context
		 FROM decisions d
		 JOIN unnest($1::uuid[], $2::uuid[]) AS pair(did, oid)
		   ON d.id = pair.did AND d.org_id = pair.oid
		 WHERE d.valid_to IS NULL AND d.embedding IS NOT NULL`,
		ids, orgIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("search outbox: query decisions: %w", err)
	}
	defer rows.Close()

	var results []DecisionForIndex
	for rows.Next() {
		var d DecisionForIndex
		var emb pgvector.Vector
		if err := rows.Scan(
			&d.ID, &d.OrgID, &d.AgentID, &d.DecisionType,
			&d.Confidence, &d.QualityScore, &d.ValidFrom, &emb,
			&d.SessionID, &d.AgentContext,
		); err != nil {
			return nil, fmt.Errorf("search outbox: scan decision: %w", err)
		}
		d.Embedding = emb.Slice()
		results = append(results, d)
	}
	return results, rows.Err()
}

// registerMetrics registers observable OTEL gauges for outbox health monitoring.
func (w *OutboxWorker) registerMetrics() {
	meter := telemetry.Meter("akashi/outbox")

	_, _ = meter.Int64ObservableGauge("akashi.outbox.depth",
		metric.WithDescription("Estimated pending entries in the search outbox (via pg_class.reltuples)"),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			// Use pg_class.reltuples for an O(1) estimate instead of SELECT COUNT(*),
			// which requires a full table scan and becomes expensive under sustained
			// Qdrant outages when the outbox table grows.
			var estimate float64
			err := w.pool.QueryRow(ctx,
				`SELECT reltuples FROM pg_class WHERE relname = 'search_outbox'`,
			).Scan(&estimate)
			if err != nil {
				return nil // Non-fatal: just skip this observation.
			}
			// reltuples can be -1 before the first VACUUM/ANALYZE; treat as zero.
			if estimate < 0 {
				estimate = 0
			}
			o.Observe(int64(estimate))
			return nil
		}),
	)
}

func scanOutboxEntries(rows pgx.Rows) ([]outboxEntry, error) {
	defer rows.Close()
	var entries []outboxEntry
	for rows.Next() {
		var e outboxEntry
		if err := rows.Scan(&e.ID, &e.DecisionID, &e.OrgID, &e.Operation, &e.Attempts); err != nil {
			return nil, fmt.Errorf("search outbox: scan entry: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// partitionUpsertEntries splits outbox entries by whether the backing decision row
// is ready for indexing. Returns:
//   - readyEntries: outbox rows that have a matching decision with embedding
//   - readyDecisions: decisions in the same order as readyEntries
//   - pendingEntries: outbox rows with no matching decision (yet)
func partitionUpsertEntries(entries []outboxEntry, decisions []DecisionForIndex) ([]outboxEntry, []DecisionForIndex, []outboxEntry) {
	byID := make(map[uuid.UUID]DecisionForIndex, len(decisions))
	for _, d := range decisions {
		byID[d.ID] = d
	}

	readyEntries := make([]outboxEntry, 0, len(entries))
	readyDecisions := make([]DecisionForIndex, 0, len(entries))
	pendingEntries := make([]outboxEntry, 0)
	for _, e := range entries {
		d, ok := byID[e.DecisionID]
		if !ok {
			pendingEntries = append(pendingEntries, e)
			continue
		}
		readyEntries = append(readyEntries, e)
		readyDecisions = append(readyDecisions, d)
	}
	return readyEntries, readyDecisions, pendingEntries
}
