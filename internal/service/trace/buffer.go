// Package trace provides the event ingestion pipeline with buffered COPY-based writes.
package trace

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// maxBufferCapacity is the hard upper limit on buffered events to prevent OOM.
// When this limit is reached, Append applies backpressure by returning an error.
const maxBufferCapacity = 100_000

// Buffer accumulates events in memory and flushes to the database
// using COPY when either the buffer size or flush timeout is reached.
type Buffer struct {
	db           *storage.DB
	logger       *slog.Logger
	maxSize      int
	flushTimeout time.Duration

	mu     sync.Mutex
	events []model.AgentEvent

	flushCh    chan struct{}
	done       chan struct{}
	cancelLoop context.CancelFunc // cancels the flushLoop goroutine
}

// NewBuffer creates a new event buffer.
func NewBuffer(db *storage.DB, logger *slog.Logger, maxSize int, flushTimeout time.Duration) *Buffer {
	return &Buffer{
		db:           db,
		logger:       logger,
		maxSize:      maxSize,
		flushTimeout: flushTimeout,
		flushCh:      make(chan struct{}, 1),
		done:         make(chan struct{}),
	}
}

// Start begins the background flush loop. Call Drain to stop.
func (b *Buffer) Start(ctx context.Context) {
	loopCtx, cancel := context.WithCancel(ctx)
	b.cancelLoop = cancel
	go b.flushLoop(loopCtx)
}

// Append adds events to the buffer, assigning server-side sequence numbers.
// Returns the assigned events with populated IDs and sequence numbers.
// Returns an error if the buffer is at capacity (backpressure).
func (b *Buffer) Append(ctx context.Context, runID uuid.UUID, agentID string, orgID uuid.UUID, inputs []model.EventInput) ([]model.AgentEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Backpressure: reject writes when the buffer is full.
	if len(b.events)+len(inputs) > maxBufferCapacity {
		return nil, fmt.Errorf("trace: buffer at capacity (%d events), try again later", len(b.events))
	}

	// Allocate globally unique sequence numbers from the Postgres SEQUENCE.
	// This is done under the mutex to preserve ordering within a run, but
	// the DB call itself is fast (single round-trip for the whole batch).
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

	b.events = append(b.events, events...)

	if len(b.events) >= b.maxSize {
		select {
		case b.flushCh <- struct{}{}:
		default:
		}
	}

	return events, nil
}

func (b *Buffer) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(b.flushTimeout)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			b.flush(context.Background()) // Final flush on shutdown.
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
	b.mu.Lock()
	if len(b.events) == 0 {
		b.mu.Unlock()
		return
	}
	batch := b.events
	b.events = nil
	b.mu.Unlock()

	start := time.Now()
	count, err := b.db.InsertEvents(ctx, batch)
	duration := time.Since(start)

	if err != nil {
		b.logger.Error("trace: flush failed", "error", err, "batch_size", len(batch))
		// Put events back for retry, but respect the capacity limit.
		b.mu.Lock()
		if len(b.events)+len(batch) <= maxBufferCapacity {
			b.events = append(batch, b.events...)
		} else {
			b.logger.Error("trace: dropping events, buffer at capacity after flush failure", "dropped", len(batch))
		}
		b.mu.Unlock()
		return
	}

	b.logger.Info("trace: batch flushed",
		"batch_size", count,
		"flush_duration_ms", duration.Milliseconds(),
	)
}

// Drain signals the background flush loop to stop, waits for it to complete
// its final flush, and returns. The ctx parameter controls the maximum time
// to wait for the goroutine to finish.
func (b *Buffer) Drain(ctx context.Context) {
	if b.cancelLoop != nil {
		b.cancelLoop() // Signal flushLoop to exit; it does a final flush before closing b.done.
	}
	select {
	case <-b.done:
	case <-ctx.Done():
		b.logger.Warn("trace: drain timed out waiting for flush loop")
	}
}

// Len returns the current number of buffered events.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events)
}
