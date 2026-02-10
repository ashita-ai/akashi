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
	once        sync.Once
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
// poll so it respects the caller's deadline.
func (w *OutboxWorker) Drain(ctx context.Context) {
	// Send the drain context to pollLoop via channel (race-free).
	// Must be sent before cancelLoop so pollLoop can receive it on ctx.Done().
	select {
	case w.drainCh <- ctx:
	default:
	}
	if w.cancelLoop != nil {
		w.cancelLoop()
	}
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

const maxOutboxAttempts = 10

func (w *OutboxWorker) processBatch(ctx context.Context) {
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
	tag, err := w.pool.Exec(ctx,
		`DELETE FROM search_outbox
		 WHERE attempts >= $1
		   AND created_at < now() - interval '7 days'`,
		maxOutboxAttempts,
	)
	if err != nil {
		w.logger.Error("search outbox: cleanup dead-letters failed", "error", err)
		return
	}
	if tag.RowsAffected() > 0 {
		w.logger.Info("search outbox: cleaned dead-letter entries", "deleted", tag.RowsAffected())
	}
}

func (w *OutboxWorker) processUpserts(ctx context.Context, entries []outboxEntry) {
	// Fetch decision data from Postgres.
	decisionIDs := make([]uuid.UUID, len(entries))
	for i, e := range entries {
		decisionIDs[i] = e.DecisionID
	}

	decisions, err := w.fetchDecisionsForIndex(ctx, decisionIDs)
	if err != nil {
		w.logger.Error("search outbox: fetch decisions", "error", err, "count", len(decisionIDs))
		w.failEntries(ctx, entries, err.Error())
		return
	}

	if len(decisions) == 0 {
		// All decisions deleted or have no embeddings — remove outbox entries.
		w.succeedEntries(ctx, entries)
		return
	}

	// Build Qdrant points.
	points := make([]Point, 0, len(decisions))
	for _, d := range decisions {
		points = append(points, Point(d))
	}

	if err := w.index.Upsert(ctx, points); err != nil {
		w.logger.Error("search outbox: qdrant upsert", "error", err, "count", len(points))
		w.failEntries(ctx, entries, err.Error())
		return
	}

	w.succeedEntries(ctx, entries)
	w.logger.Info("search outbox: upserted", "count", len(points))
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

func (w *OutboxWorker) fetchDecisionsForIndex(ctx context.Context, ids []uuid.UUID) ([]DecisionForIndex, error) {
	rows, err := w.pool.Query(ctx,
		`SELECT id, org_id, agent_id, decision_type, confidence, quality_score, valid_from, embedding
		 FROM decisions
		 WHERE id = ANY($1) AND valid_to IS NULL AND embedding IS NOT NULL`,
		ids,
	)
	if err != nil {
		return nil, fmt.Errorf("search outbox: query decisions: %w", err)
	}
	defer rows.Close()

	var results []DecisionForIndex
	for rows.Next() {
		var d DecisionForIndex
		var emb []float32
		if err := rows.Scan(
			&d.ID, &d.OrgID, &d.AgentID, &d.DecisionType,
			&d.Confidence, &d.QualityScore, &d.ValidFrom, &emb,
		); err != nil {
			return nil, fmt.Errorf("search outbox: scan decision: %w", err)
		}
		d.Embedding = emb
		results = append(results, d)
	}
	return results, rows.Err()
}

// registerMetrics registers observable OTEL gauges for outbox health monitoring.
func (w *OutboxWorker) registerMetrics() {
	meter := telemetry.Meter("akashi/outbox")

	_, _ = meter.Int64ObservableGauge("akashi.outbox.depth",
		metric.WithDescription("Number of pending entries in the search outbox"),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			var count int64
			err := w.pool.QueryRow(ctx, `SELECT COUNT(*) FROM search_outbox WHERE attempts < $1`, maxOutboxAttempts).Scan(&count)
			if err != nil {
				return nil // Non-fatal: just skip this observation.
			}
			o.Observe(count)
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
