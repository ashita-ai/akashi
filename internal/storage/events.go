package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/kyoyu/internal/model"
)

// NextSequenceNum returns the next sequence number for a run.
func (db *DB) NextSequenceNum(ctx context.Context, runID uuid.UUID) (int64, error) {
	var seq int64
	err := db.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(sequence_num), 0) + 1 FROM agent_events WHERE run_id = $1`, runID,
	).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("storage: next sequence num: %w", err)
	}
	return seq, nil
}

// InsertEvents inserts events using the COPY protocol for high throughput.
// Events must have SequenceNum already assigned.
func (db *DB) InsertEvents(ctx context.Context, events []model.AgentEvent) (int64, error) {
	if len(events) == 0 {
		return 0, nil
	}

	columns := []string{"id", "run_id", "event_type", "sequence_num", "occurred_at", "agent_id", "payload", "created_at"}

	rows := make([][]any, len(events))
	for i, e := range events {
		rows[i] = []any{
			e.ID,
			e.RunID,
			string(e.EventType),
			e.SequenceNum,
			e.OccurredAt,
			e.AgentID,
			e.Payload,
			e.CreatedAt,
		}
	}

	copyCount, err := db.pool.CopyFrom(
		ctx,
		pgx.Identifier{"agent_events"},
		columns,
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return 0, fmt.Errorf("storage: copy events: %w", err)
	}
	return copyCount, nil
}

// InsertEvent inserts a single event (for low-volume operations).
func (db *DB) InsertEvent(ctx context.Context, event model.AgentEvent) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO agent_events (id, run_id, event_type, sequence_num, occurred_at, agent_id, payload, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		event.ID, event.RunID, string(event.EventType), event.SequenceNum,
		event.OccurredAt, event.AgentID, event.Payload, event.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("storage: insert event: %w", err)
	}
	return nil
}

// GetEventsByRun retrieves all events for a run, ordered by sequence_num.
func (db *DB) GetEventsByRun(ctx context.Context, runID uuid.UUID) ([]model.AgentEvent, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, run_id, event_type, sequence_num, occurred_at, agent_id, payload, created_at
		 FROM agent_events WHERE run_id = $1
		 ORDER BY sequence_num ASC`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get events by run: %w", err)
	}
	defer rows.Close()

	return scanEvents(rows)
}

// GetEventsByRunBeforeTime retrieves events for a run that occurred before a given time.
// Used for context replay.
func (db *DB) GetEventsByRunBeforeTime(ctx context.Context, runID uuid.UUID, before time.Time) ([]model.AgentEvent, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, run_id, event_type, sequence_num, occurred_at, agent_id, payload, created_at
		 FROM agent_events WHERE run_id = $1 AND occurred_at <= $2
		 ORDER BY sequence_num ASC`, runID, before,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get events before time: %w", err)
	}
	defer rows.Close()

	return scanEvents(rows)
}

func scanEvents(rows pgx.Rows) ([]model.AgentEvent, error) {
	var events []model.AgentEvent
	for rows.Next() {
		var e model.AgentEvent
		if err := rows.Scan(
			&e.ID, &e.RunID, &e.EventType, &e.SequenceNum,
			&e.OccurredAt, &e.AgentID, &e.Payload, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan event: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
