// Package trace provides the event ingestion pipeline with buffered COPY-based writes.
package trace

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/kyoyu/internal/model"
	"github.com/ashita-ai/kyoyu/internal/storage"
)

// Buffer accumulates events in memory and flushes to the database
// using COPY when either the buffer size or flush timeout is reached.
type Buffer struct {
	db           *storage.DB
	logger       *slog.Logger
	maxSize      int
	flushTimeout time.Duration

	mu     sync.Mutex
	events []model.AgentEvent
	seqNums map[uuid.UUID]int64 // run_id -> next sequence number

	flushCh chan struct{}
	done    chan struct{}
}

// NewBuffer creates a new event buffer.
func NewBuffer(db *storage.DB, logger *slog.Logger, maxSize int, flushTimeout time.Duration) *Buffer {
	return &Buffer{
		db:           db,
		logger:       logger,
		maxSize:      maxSize,
		flushTimeout: flushTimeout,
		seqNums:      make(map[uuid.UUID]int64),
		flushCh:      make(chan struct{}, 1),
		done:         make(chan struct{}),
	}
}

// Start begins the background flush loop. Call Drain to stop.
func (b *Buffer) Start(ctx context.Context) {
	go b.flushLoop(ctx)
}

// Append adds events to the buffer, assigning server-side sequence numbers.
// Returns the assigned events with populated IDs and sequence numbers.
func (b *Buffer) Append(ctx context.Context, runID uuid.UUID, agentID string, inputs []model.EventInput) ([]model.AgentEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Get or initialize the sequence counter for this run.
	if _, ok := b.seqNums[runID]; !ok {
		seq, err := b.db.NextSequenceNum(ctx, runID)
		if err != nil {
			return nil, fmt.Errorf("trace: get sequence num: %w", err)
		}
		b.seqNums[runID] = seq
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
			EventType:   input.EventType,
			SequenceNum: b.seqNums[runID],
			OccurredAt:  occurredAt,
			AgentID:     agentID,
			Payload:     input.Payload,
			CreatedAt:   now,
		}
		b.seqNums[runID]++
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
		// Put events back for retry.
		b.mu.Lock()
		b.events = append(batch, b.events...)
		b.mu.Unlock()
		return
	}

	b.logger.Info("trace: batch flushed",
		"batch_size", count,
		"flush_duration_ms", duration.Milliseconds(),
	)
}

// Drain flushes all remaining events and waits for completion.
func (b *Buffer) Drain(ctx context.Context) {
	b.flush(ctx)
}

// Len returns the current number of buffered events.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events)
}
