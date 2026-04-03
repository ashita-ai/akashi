// Package trace provides the event ingestion pipeline with buffered COPY-based writes.
package trace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/metric"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/internal/telemetry"
)

// maxBufferCapacity is the hard upper limit on buffered events to prevent OOM.
// When this limit is reached, Append applies backpressure by returning an error.
const maxBufferCapacity = 100_000

var (
	// ErrBufferDraining indicates the server is shutting down and no new events are accepted.
	ErrBufferDraining = errors.New("trace: buffer is draining")
	// ErrBufferAtCapacity indicates the in-memory buffer hit its hard cap.
	ErrBufferAtCapacity = errors.New("trace: buffer at capacity")
)

// Buffer accumulates events in memory and flushes to the database
// using COPY when either the buffer size or flush timeout is reached.
// When a WAL is configured, events are written to disk before being
// buffered in memory, providing crash durability.
type Buffer struct {
	db           *storage.DB
	logger       *slog.Logger
	maxSize      int
	flushTimeout time.Duration
	wal          *WAL // nil when WAL is disabled

	mu        sync.Mutex
	events    []model.AgentEvent
	audits    []storage.MutationAuditEntry // audit entries to flush atomically with events
	walMaxLSN uint64                       // highest WAL LSN for buffered events (valid only when wal != nil)

	droppedEvents atomic.Int64 // total events rejected (capacity or drain in progress)
	draining      atomic.Bool  // true after Drain is initiated; rejects new appends

	started    atomic.Bool // guards against double Start calls
	drainOnce  sync.Once
	flushCh    chan struct{}
	done       chan struct{}
	cancelLoop context.CancelFunc   // cancels the flushLoop goroutine
	drainCh    chan context.Context // carries the drain context to flushLoop for the final flush
}

// NewBuffer creates a new event buffer. Pass wal=nil to disable WAL (existing behavior).
func NewBuffer(db *storage.DB, logger *slog.Logger, maxSize int, flushTimeout time.Duration, wal *WAL) *Buffer {
	return &Buffer{
		db:           db,
		logger:       logger,
		maxSize:      maxSize,
		flushTimeout: flushTimeout,
		wal:          wal,
		flushCh:      make(chan struct{}, 1),
		done:         make(chan struct{}),
		drainCh:      make(chan context.Context, 1),
	}
}

// Start begins the background flush loop and registers OTEL metrics. Call Drain to stop.
// When WAL is enabled, recovers un-flushed events before starting the flush loop.
// It is safe to call only once; subsequent calls are no-ops and log a warning.
func (b *Buffer) Start(ctx context.Context) {
	if !b.started.CompareAndSwap(false, true) {
		b.logger.Warn("trace: buffer Start called more than once, ignoring")
		return
	}
	b.registerMetrics()

	// Recover un-flushed events from WAL before accepting new traffic.
	if b.wal != nil {
		recovered, maxLSN, err := b.wal.Recover()
		if err != nil {
			b.logger.Error("trace: wal recovery failed", "error", err)
			// Continue without recovered events. They are not lost — the WAL
			// files are intact and will be retried on next startup.
		} else if len(recovered) > 0 {
			// Flush recovered events through the idempotent path to handle
			// the case where events were already COPYed before the crash.
			inserted, err := b.db.InsertEventsIdempotent(ctx, recovered)
			if err != nil {
				b.logger.Error("trace: wal recovery flush failed, events remain in WAL for next startup",
					"error", err, "recovered_count", len(recovered))
			} else {
				b.logger.Info("trace: recovered events from WAL",
					"recovered", len(recovered), "new_inserts", inserted,
					"duplicates_skipped", int64(len(recovered))-inserted)
				// Advance the WAL checkpoint now that events are in Postgres.
				if err := b.wal.CheckpointLSN(maxLSN); err != nil {
					b.logger.Warn("trace: wal checkpoint after recovery failed (events are safe in postgres)",
						"error", err)
				}
			}
		}
	}

	loopCtx, cancel := context.WithCancel(ctx)
	b.cancelLoop = cancel
	go b.flushLoop(loopCtx)
}

// Append adds events to the buffer, assigning server-side sequence numbers.
// Returns the assigned events with populated IDs and sequence numbers.
// Returns an error if the buffer is at capacity (backpressure).
//
// When WAL is enabled, events are written to the WAL before buffering in memory.
// This makes events crash-durable at Append time.
//
// Holds the lock during ReserveSequenceNums to avoid sequence leaks: if we
// reserved sequences then failed a post-reserve capacity check (race with
// another goroutine), those sequences would be consumed but never used.
func (b *Buffer) Append(ctx context.Context, runID uuid.UUID, agentID string, orgID uuid.UUID, inputs []model.EventInput) ([]model.AgentEvent, error) {
	if b.draining.Load() {
		b.droppedEvents.Add(int64(len(inputs)))
		return nil, fmt.Errorf("%w: rejecting %d new events", ErrBufferDraining, len(inputs))
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.events)+len(inputs) > maxBufferCapacity {
		b.droppedEvents.Add(int64(len(inputs)))
		return nil, fmt.Errorf("%w (%d events), try again later", ErrBufferAtCapacity, len(b.events))
	}

	seqNums, err := b.db.ReserveSequenceNums(ctx, len(inputs))
	if err != nil {
		return nil, fmt.Errorf("trace: reserve sequence nums: %w", err)
	}

	now := time.Now().UTC()
	events := make([]model.AgentEvent, len(inputs))
	for i, input := range inputs {
		occurredAt := now
		if input.OccurredAt != nil {
			occurredAt = *input.OccurredAt
		}
		events[i] = model.AgentEvent{
			ID:          uuid.New(),
			RunID:       runID,
			OrgID:       orgID,
			EventType:   input.EventType,
			SequenceNum: seqNums[i],
			OccurredAt:  occurredAt,
			AgentID:     agentID,
			Payload:     input.Payload,
			CreatedAt:   now,
		}
	}

	// Write to WAL before buffering in memory for crash durability.
	if b.wal != nil {
		maxLSN, err := b.wal.Write(events)
		if err != nil {
			return nil, fmt.Errorf("trace: wal write: %w", err)
		}
		b.walMaxLSN = maxLSN
	}

	b.events = append(b.events, events...)

	if len(b.events) >= b.maxSize {
		select {
		case b.flushCh <- struct{}{}:
		default:
		}
	}

	return events, nil
}

// BufferAudit enqueues an audit entry to be flushed atomically with the next
// batch of events. This ensures event appends and their audit records are
// committed in the same transaction — eliminating the window where events
// persist but their audit trail is lost.
func (b *Buffer) BufferAudit(entry storage.MutationAuditEntry) {
	b.mu.Lock()
	b.audits = append(b.audits, entry)
	b.mu.Unlock()
}

// AppendWithAudit is like Append but atomically buffers an audit entry alongside
// the events under the same lock acquisition. The auditFn callback receives the
// created events and returns the audit entry to buffer. This eliminates the race
// window where a background flush could drain events before the audit is buffered.
func (b *Buffer) AppendWithAudit(
	ctx context.Context,
	runID uuid.UUID,
	agentID string,
	orgID uuid.UUID,
	inputs []model.EventInput,
	auditFn func(events []model.AgentEvent) storage.MutationAuditEntry,
) ([]model.AgentEvent, error) {
	if b.draining.Load() {
		b.droppedEvents.Add(int64(len(inputs)))
		return nil, fmt.Errorf("%w: rejecting %d new events", ErrBufferDraining, len(inputs))
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.events)+len(inputs) > maxBufferCapacity {
		b.droppedEvents.Add(int64(len(inputs)))
		return nil, fmt.Errorf("%w (%d events), try again later", ErrBufferAtCapacity, len(b.events))
	}

	seqNums, err := b.db.ReserveSequenceNums(ctx, len(inputs))
	if err != nil {
		return nil, fmt.Errorf("trace: reserve sequence nums: %w", err)
	}

	now := time.Now().UTC()
	events := make([]model.AgentEvent, len(inputs))
	for i, input := range inputs {
		occurredAt := now
		if input.OccurredAt != nil {
			occurredAt = *input.OccurredAt
		}
		events[i] = model.AgentEvent{
			ID:          uuid.New(),
			RunID:       runID,
			OrgID:       orgID,
			EventType:   input.EventType,
			SequenceNum: seqNums[i],
			OccurredAt:  occurredAt,
			AgentID:     agentID,
			Payload:     input.Payload,
			CreatedAt:   now,
		}
	}

	// Write to WAL before buffering in memory for crash durability.
	if b.wal != nil {
		maxLSN, walErr := b.wal.Write(events)
		if walErr != nil {
			return nil, fmt.Errorf("trace: wal write: %w", walErr)
		}
		b.walMaxLSN = maxLSN
	}

	b.events = append(b.events, events...)

	// Buffer the audit entry under the same lock hold — no background flush
	// can snapshot the events without also capturing this audit entry.
	if auditFn != nil {
		b.audits = append(b.audits, auditFn(events))
	}

	if len(b.events) >= b.maxSize {
		select {
		case b.flushCh <- struct{}{}:
		default:
		}
	}

	return events, nil
}

// HasWAL returns true if a write-ahead log is configured.
func (b *Buffer) HasWAL() bool {
	return b.wal != nil
}

func (b *Buffer) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(b.flushTimeout)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final flush: prefer the drain context (sent by Drain via channel)
			// so the final flush respects the caller's deadline.
			var drainCtx context.Context
			select {
			case drainCtx = <-b.drainCh:
			default:
			}
			if drainCtx != nil {
				if err := b.flushUntilEmpty(drainCtx); err != nil {
					b.logger.Warn("trace: final drain flush incomplete", "error", err, "remaining", b.Len())
				}
			} else {
				// Fallback for direct cancellation without Drain (e.g., tests).
				// 30s matches the outbox drain timeout — long enough to flush a
				// full buffer batch even under transient DB pressure.
				fallbackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := b.flushUntilEmpty(fallbackCtx); err != nil {
					b.logger.Warn("trace: fallback final flush incomplete", "error", err, "remaining", b.Len())
				}
				cancel()
			}
			close(b.done)
			return
		case <-ticker.C:
			b.flush(ctx)
		case <-b.flushCh:
			b.flush(ctx)
		}
	}
}

func (b *Buffer) flush(ctx context.Context) {
	_, _ = b.flushOnce(ctx)
}

// FlushNow blocks until buffered events are durably written or ctx expires.
func (b *Buffer) FlushNow(ctx context.Context) error {
	return b.flushUntilEmpty(ctx)
}

// flushUntilEmpty retries flushes until the buffer is empty or ctx expires.
// This is used during shutdown so we don't exit after a single transient write failure.
func (b *Buffer) flushUntilEmpty(ctx context.Context) error {
	const maxBackoff = 2 * time.Second
	backoff := 50 * time.Millisecond

	for {
		flushed, err := b.flushOnce(ctx)
		if err == nil {
			if !flushed {
				return nil // nothing left to flush
			}
			// Flushed at least one batch; continue immediately in case more are queued.
			backoff = 50 * time.Millisecond
			continue
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("trace: flush incomplete before deadline: %w", ctx.Err())
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// flushOnce attempts a single COPY flush.
// Returns (flushedAny, err): flushedAny=true when a batch was written successfully.
//
// When audit entries have been buffered via BufferAudit, events and audits are
// flushed in a single transaction — guaranteeing that event appends never
// persist without their audit trail.
func (b *Buffer) flushOnce(ctx context.Context) (bool, error) {
	b.mu.Lock()
	if len(b.events) == 0 && len(b.audits) == 0 {
		b.mu.Unlock()
		return false, nil
	}

	// Orphaned audit entries (no events): flush them in a single transaction
	// rather than waiting for an event batch that may never arrive. Using a
	// transaction ensures all-or-nothing semantics — no duplicates on retry.
	if len(b.events) == 0 {
		orphanedAudits := make([]storage.MutationAuditEntry, len(b.audits))
		copy(orphanedAudits, b.audits)
		b.mu.Unlock()

		if err := b.db.InsertMutationAuditBatch(ctx, orphanedAudits); err != nil {
			b.logger.Error("trace: orphaned audit flush failed", "error", err)
			return false, err
		}

		b.mu.Lock()
		if len(b.audits) >= len(orphanedAudits) {
			b.audits = b.audits[len(orphanedAudits):]
		} else {
			b.audits = nil
		}
		b.mu.Unlock()

		b.logger.Info("trace: orphaned audits flushed", "audit_entries", len(orphanedAudits))
		return true, nil
	}
	// Keep events in memory until the flush succeeds.
	// This avoids data loss on transient flush failures.
	batch := make([]model.AgentEvent, len(b.events))
	copy(batch, b.events)
	var auditBatch []storage.MutationAuditEntry
	if len(b.audits) > 0 {
		auditBatch = make([]storage.MutationAuditEntry, len(b.audits))
		copy(auditBatch, b.audits)
	}
	batchWALLSN := b.walMaxLSN // snapshot the highest LSN for this batch
	b.mu.Unlock()

	start := time.Now()
	count, err := b.db.InsertEventsWithAudit(ctx, batch, auditBatch)
	duration := time.Since(start)

	if err != nil {
		b.logger.Error("trace: flush failed", "error", err, "batch_size", len(batch))
		return false, err
	}

	// Remove only the flushed prefix. New events appended while COPY was in
	// progress remain queued for the next flush. Audit entries are similarly
	// trimmed — only the snapshot we flushed is removed.
	b.mu.Lock()
	if len(b.events) >= len(batch) {
		b.events = b.events[len(batch):]
	} else {
		// Defensive guard: should not happen because writers only append.
		b.events = nil
	}
	if len(b.audits) >= len(auditBatch) {
		b.audits = b.audits[len(auditBatch):]
	} else {
		b.audits = nil
	}
	b.mu.Unlock()

	// Advance WAL checkpoint after successful COPY.
	if b.wal != nil && batchWALLSN > 0 {
		if err := b.wal.CheckpointLSN(batchWALLSN); err != nil {
			// Non-fatal: events are in Postgres. The WAL will replay them on
			// next startup, and the idempotent recovery path handles duplicates.
			b.logger.Warn("trace: wal checkpoint failed (events are durable in postgres)", "error", err)
		}
	}

	b.logger.Info("trace: batch flushed",
		"batch_size", count,
		"audit_entries", len(auditBatch),
		"flush_duration_ms", duration.Milliseconds(),
	)
	return true, nil
}

// Drain signals the background flush loop to stop, waits for it to complete
// its final flush, and returns. The ctx parameter controls the maximum time
// to wait for the goroutine to finish and is passed to the final flush so it
// respects the caller's deadline. Drain is idempotent; subsequent calls return
// the same result.
//
// Returns nil if all events were flushed, or an error if the drain context
// expired with events still in the buffer (potential data loss).
func (b *Buffer) Drain(ctx context.Context) error {
	b.drainOnce.Do(func() {
		b.draining.Store(true)
		// Send the drain context to flushLoop via channel (race-free).
		// Must be sent before cancelLoop so flushLoop can receive it on ctx.Done().
		// Use a short timeout to avoid blocking if the channel is unexpectedly full.
		sendCtx, sendCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		select {
		case b.drainCh <- ctx:
		case <-sendCtx.Done():
			b.logger.Warn("trace: drain context channel busy, flush will use fallback timeout")
		}
		sendCancel()
		if b.cancelLoop != nil {
			b.cancelLoop() // Signal flushLoop to exit; it does a final flush before closing b.done.
		}
	})
	select {
	case <-b.done:
	case <-ctx.Done():
		b.logger.Error("trace: drain timed out — unflushed entries will be lost",
			"remaining", b.Len(),
		)
	}

	// Close WAL after drain completes (final flush may have advanced checkpoint).
	if b.wal != nil {
		if err := b.wal.Close(); err != nil {
			b.logger.Warn("trace: wal close failed", "error", err)
		}
	}

	if remaining := b.Len(); remaining > 0 {
		return fmt.Errorf("trace: drain incomplete, %d entries lost", remaining)
	}
	return nil
}

// registerMetrics registers observable OTEL gauges for buffer health monitoring.
// Called from Start() after the global meter provider has been initialized.
func (b *Buffer) registerMetrics() {
	meter := telemetry.Meter("akashi/buffer")

	_, _ = meter.Int64ObservableGauge("akashi.buffer.depth",
		metric.WithDescription("Current number of events and audit entries in the write buffer"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(b.Len()))
			return nil
		}),
	)

	_, _ = meter.Int64ObservableGauge("akashi.buffer.dropped_total",
		metric.WithDescription("Total events rejected at ingress due to capacity or shutdown draining"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(b.DroppedEvents())
			return nil
		}),
	)
}

// Len returns the total number of buffered entries (events + audit).
// Use this for drain/flush-empty checks where both must be flushed.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events) + len(b.audits)
}

// EventLen returns the number of buffered events only, excluding audit
// entries. Use this when comparing against Capacity (which bounds events).
func (b *Buffer) EventLen() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events)
}

// Capacity returns the hard upper limit on buffered events.
func (b *Buffer) Capacity() int {
	return maxBufferCapacity
}

// DroppedEvents returns the total number of events rejected at ingress
// (buffer capacity reached or drain in progress).
func (b *Buffer) DroppedEvents() int64 {
	return b.droppedEvents.Load()
}
