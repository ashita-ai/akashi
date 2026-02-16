package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/akashi/internal/model"
)

// ReserveSequenceNums atomically allocates count globally unique sequence numbers
// using a Postgres SEQUENCE. Returns a slice of monotonically increasing values.
// Under concurrent access, values are unique but may not be consecutive (gaps are
// harmless — they just mean another caller grabbed intervening numbers).
func (db *DB) ReserveSequenceNums(ctx context.Context, count int) ([]int64, error) {
	if count <= 0 {
		return nil, nil
	}
	rows, err := db.pool.Query(ctx,
		`SELECT nextval('event_sequence_num_seq') FROM generate_series(1, $1)`, count)
	if err != nil {
		return nil, fmt.Errorf("storage: reserve sequence nums: %w", err)
	}
	defer rows.Close()

	nums := make([]int64, 0, count)
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("storage: scan sequence num: %w", err)
		}
		nums = append(nums, v)
	}
	return nums, rows.Err()
}

// InsertEvents inserts events using the COPY protocol for high throughput.
// Events must have SequenceNum already assigned.
func (db *DB) InsertEvents(ctx context.Context, events []model.AgentEvent) (int64, error) {
	if len(events) == 0 {
		return 0, nil
	}

	columns := []string{"id", "run_id", "org_id", "event_type", "sequence_num", "occurred_at", "agent_id", "payload", "created_at"}

	rows := make([][]any, len(events))
	for i, e := range events {
		rows[i] = []any{
			e.ID,
			e.RunID,
			e.OrgID,
			string(e.EventType),
			e.SequenceNum,
			e.OccurredAt,
			e.AgentID,
			e.Payload,
			e.CreatedAt,
		}
	}

	// Dedicated 30s COPY timeout prevents a hung Postgres from blocking the
	// buffer flush indefinitely. Matches the COPY timeout used for alternatives
	// and evidence in CreateTraceTx.
	copyCtx, copyCancel := context.WithTimeout(ctx, 30*time.Second)
	copyCount, err := db.pool.CopyFrom(
		copyCtx,
		pgx.Identifier{"agent_events"},
		columns,
		pgx.CopyFromRows(rows),
	)
	copyCancel()
	if err != nil {
		return 0, fmt.Errorf("storage: copy events: %w", err)
	}
	return copyCount, nil
}

// InsertEvent inserts a single event (for low-volume operations).
func (db *DB) InsertEvent(ctx context.Context, event model.AgentEvent) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO agent_events (id, run_id, org_id, event_type, sequence_num, occurred_at, agent_id, payload, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		event.ID, event.RunID, event.OrgID, string(event.EventType), event.SequenceNum,
		event.OccurredAt, event.AgentID, event.Payload, event.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("storage: insert event: %w", err)
	}
	return nil
}

// GetEventsByRun retrieves events for a run, scoped by org_id for tenant isolation.
// The limit parameter caps the number of rows returned; if limit <= 0, it defaults
// to 10000. Callers should check if the returned slice length equals the limit to
// detect truncation.
func (db *DB) GetEventsByRun(ctx context.Context, orgID, runID uuid.UUID, limit int) ([]model.AgentEvent, error) {
	if limit <= 0 {
		limit = 10000
	}
	rows, err := db.pool.Query(ctx,
		`SELECT id, run_id, org_id, event_type, sequence_num, occurred_at, agent_id, payload, created_at
		 FROM agent_events WHERE run_id = $1 AND org_id = $2
		 ORDER BY sequence_num ASC
		 LIMIT $3`, runID, orgID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get events by run: %w", err)
	}
	defer rows.Close()

	return scanEvents(rows)
}

// InsertEventsIdempotent inserts events with duplicate safety via ON CONFLICT DO NOTHING.
// Used during WAL recovery when events may have been flushed to Postgres before the
// checkpoint was updated. Slower than COPY but runs only once per startup during recovery.
func (db *DB) InsertEventsIdempotent(ctx context.Context, events []model.AgentEvent) (int64, error) {
	if len(events) == 0 {
		return 0, nil
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("storage: begin idempotent insert tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Create unlogged temp table (no WAL overhead for the temp table itself).
	if _, err := tx.Exec(ctx,
		`CREATE TEMP TABLE _recovery_events (LIKE agent_events INCLUDING DEFAULTS) ON COMMIT DROP`,
	); err != nil {
		return 0, fmt.Errorf("storage: create recovery temp table: %w", err)
	}

	columns := []string{"id", "run_id", "org_id", "event_type", "sequence_num", "occurred_at", "agent_id", "payload", "created_at"}
	rows := make([][]any, len(events))
	for i, e := range events {
		rows[i] = []any{
			e.ID,
			e.RunID,
			e.OrgID,
			string(e.EventType),
			e.SequenceNum,
			e.OccurredAt,
			e.AgentID,
			e.Payload,
			e.CreatedAt,
		}
	}

	// COPY into temp table (fast bulk load).
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"_recovery_events"}, columns, pgx.CopyFromRows(rows)); err != nil {
		return 0, fmt.Errorf("storage: copy into recovery temp table: %w", err)
	}

	// Move to real table, skipping duplicates.
	tag, err := tx.Exec(ctx,
		`INSERT INTO agent_events SELECT * FROM _recovery_events ON CONFLICT (id) DO NOTHING`)
	if err != nil {
		return 0, fmt.Errorf("storage: insert from recovery temp table: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("storage: commit idempotent insert: %w", err)
	}
	return tag.RowsAffected(), nil
}

func scanEvents(rows pgx.Rows) ([]model.AgentEvent, error) {
	var events []model.AgentEvent
	for rows.Next() {
		var e model.AgentEvent
		if err := rows.Scan(
			&e.ID, &e.RunID, &e.OrgID, &e.EventType, &e.SequenceNum,
			&e.OccurredAt, &e.AgentID, &e.Payload, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan event: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
