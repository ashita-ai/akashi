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

	mu     sync.Mutex
	events []model.AgentEvent

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
		recovered, err := b.wal.Recover()
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
				if err := b.wal.Checkpoint(recovered); err != nil {
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
		if err := b.wal.Write(events); err != nil {
			return nil, fmt.Errorf("trace: wal write: %w", err)
		}
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
					b.logger.Warn("trace: final drain flush incomplete", "error", err, "remaining_events", b.Len())
				}
			} else {
				// Fallback for direct cancellation without Drain (e.g., tests).
				fallbackCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				if err := b.flushUntilEmpty(fallbackCtx); err != nil {
					b.logger.Warn("trace: fallback final flush incomplete", "error", err, "remaining_events", b.Len())
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
func (b *Buffer) flushOnce(ctx context.Context) (bool, error) {
	b.mu.Lock()
	if len(b.events) == 0 {
		b.mu.Unlock()
		return false, nil
	}
	// Keep events in memory until COPY succeeds.
	// This avoids data loss on transient flush failures.
	batch := make([]model.AgentEvent, len(b.events))
	copy(batch, b.events)
	b.mu.Unlock()

	start := time.Now()
	count, err := b.db.InsertEvents(ctx, batch)
	duration := time.Since(start)

	if err != nil {
		b.logger.Error("trace: flush failed", "error", err, "batch_size", len(batch))
		return false, err
	}

	// Remove only the flushed prefix. New events appended while COPY was in
	// progress remain queued for the next flush.
	b.mu.Lock()
	if len(b.events) >= len(batch) {
		b.events = b.events[len(batch):]
	} else {
		// Defensive guard: should not happen because writers only append.
		b.events = nil
	}
	b.mu.Unlock()

	// Advance WAL checkpoint after successful COPY.
	if b.wal != nil {
		if err := b.wal.Checkpoint(batch); err != nil {
			// Non-fatal: events are in Postgres. The WAL will replay them on
			// next startup, and the idempotent recovery path handles duplicates.
			b.logger.Warn("trace: wal checkpoint failed (events are durable in postgres)", "error", err)
		}
	}

	b.logger.Info("trace: batch flushed",
		"batch_size", count,
		"flush_duration_ms", duration.Milliseconds(),
	)
	return true, nil
}

// Drain signals the background flush loop to stop, waits for it to complete
// its final flush, and returns. The ctx parameter controls the maximum time
// to wait for the goroutine to finish and is passed to the final flush so it
// respects the caller's deadline. Drain is idempotent; subsequent calls are no-ops.
func (b *Buffer) Drain(ctx context.Context) {
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
		b.logger.Warn("trace: drain timed out waiting for flush loop")
	}

	// Close WAL after drain completes (final flush may have advanced checkpoint).
	if b.wal != nil {
		if err := b.wal.Close(); err != nil {
			b.logger.Warn("trace: wal close failed", "error", err)
		}
	}
}

// registerMetrics registers observable OTEL gauges for buffer health monitoring.
// Called from Start() after the global meter provider has been initialized.
func (b *Buffer) registerMetrics() {
	meter := telemetry.Meter("akashi/buffer")

	_, _ = meter.Int64ObservableGauge("akashi.buffer.depth",
		metric.WithDescription("Current number of events in the write buffer"),
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

// Len returns the current number of buffered events.
func (b *Buffer) Len() int {
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
