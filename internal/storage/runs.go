package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/akashi/internal/model"
)

// CreateRun inserts a new agent run and returns it.
func (db *DB) CreateRun(ctx context.Context, req model.CreateRunRequest) (model.AgentRun, error) {
	now := time.Now().UTC()
	run := model.AgentRun{
		ID:          uuid.New(),
		AgentID:     req.AgentID,
		OrgID:       req.OrgID,
		TraceID:     req.TraceID,
		ParentRunID: req.ParentRunID,
		Status:      model.RunStatusRunning,
		StartedAt:   now,
		Metadata:    req.Metadata,
		CreatedAt:   now,
	}
	if run.Metadata == nil {
		run.Metadata = map[string]any{}
	}

	_, err := db.pool.Exec(ctx,
		`INSERT INTO agent_runs (id, agent_id, org_id, trace_id, parent_run_id, status, started_at, metadata, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		run.ID, run.AgentID, run.OrgID, run.TraceID, run.ParentRunID,
		string(run.Status), run.StartedAt, run.Metadata, run.CreatedAt,
	)
	if err != nil {
		return model.AgentRun{}, fmt.Errorf("storage: create run: %w", err)
	}
	return run, nil
}

// CreateRunWithAudit creates a run and inserts a mutation audit entry
// atomically within a single transaction. If either INSERT fails, both
// are rolled back — mutations never persist without their audit record.
func (db *DB) CreateRunWithAudit(ctx context.Context, req model.CreateRunRequest, audit MutationAuditEntry) (model.AgentRun, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return model.AgentRun{}, fmt.Errorf("storage: begin create run tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := time.Now().UTC()
	run := model.AgentRun{
		ID:          uuid.New(),
		AgentID:     req.AgentID,
		OrgID:       req.OrgID,
		TraceID:     req.TraceID,
		ParentRunID: req.ParentRunID,
		Status:      model.RunStatusRunning,
		StartedAt:   now,
		Metadata:    req.Metadata,
		CreatedAt:   now,
	}
	if run.Metadata == nil {
		run.Metadata = map[string]any{}
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_runs (id, agent_id, org_id, trace_id, parent_run_id, status, started_at, metadata, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		run.ID, run.AgentID, run.OrgID, run.TraceID, run.ParentRunID,
		string(run.Status), run.StartedAt, run.Metadata, run.CreatedAt,
	); err != nil {
		return model.AgentRun{}, fmt.Errorf("storage: create run: %w", err)
	}

	audit.ResourceID = run.ID.String()
	audit.AfterData = run
	if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
		return model.AgentRun{}, fmt.Errorf("storage: audit in create run tx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.AgentRun{}, fmt.Errorf("storage: commit create run tx: %w", err)
	}
	return run, nil
}

// GetRun retrieves a run by ID, scoped to the given org.
func (db *DB) GetRun(ctx context.Context, orgID, id uuid.UUID) (model.AgentRun, error) {
	var run model.AgentRun
	err := db.pool.QueryRow(ctx,
		`SELECT id, agent_id, org_id, trace_id, parent_run_id, status, started_at, completed_at, metadata, created_at
		 FROM agent_runs WHERE id = $1 AND org_id = $2`, id, orgID,
	).Scan(
		&run.ID, &run.AgentID, &run.OrgID, &run.TraceID, &run.ParentRunID,
		&run.Status, &run.StartedAt, &run.CompletedAt, &run.Metadata, &run.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.AgentRun{}, fmt.Errorf("storage: run %s: %w", id, ErrNotFound)
		}
		return model.AgentRun{}, fmt.Errorf("storage: get run: %w", err)
	}
	return run, nil
}

// CompleteRun marks a run as completed or failed, scoped to the given org.
func (db *DB) CompleteRun(ctx context.Context, orgID, id uuid.UUID, status model.RunStatus, metadata map[string]any) error {
	now := time.Now().UTC()
	if metadata == nil {
		metadata = map[string]any{}
	}
	tag, err := db.pool.Exec(ctx,
		`UPDATE agent_runs SET status = $1, completed_at = $2, metadata = metadata || $3
		 WHERE id = $4 AND org_id = $5 AND status = 'running'`,
		string(status), now, metadata, id, orgID,
	)
	if err != nil {
		return fmt.Errorf("storage: complete run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var existingStatus string
		err := db.pool.QueryRow(ctx,
			`SELECT status FROM agent_runs WHERE id = $1 AND org_id = $2`,
			id, orgID,
		).Scan(&existingStatus)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("storage: run %s: %w", id, ErrNotFound)
			}
			return fmt.Errorf("storage: complete run status lookup: %w", err)
		}

		// Idempotent success for retries when the run is already finalized.
		if existingStatus == string(model.RunStatusCompleted) || existingStatus == string(model.RunStatusFailed) {
			return nil
		}
		return fmt.Errorf("storage: run %s complete transition rejected from status %q", id, existingStatus)
	}
	return nil
}

// CompleteRunWithAudit marks a run as completed/failed and inserts a mutation
// audit entry atomically within a single transaction.
func (db *DB) CompleteRunWithAudit(ctx context.Context, orgID, id uuid.UUID, status model.RunStatus, metadata map[string]any, audit MutationAuditEntry) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage: begin complete run tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := time.Now().UTC()
	if metadata == nil {
		metadata = map[string]any{}
	}
	tag, err := tx.Exec(ctx,
		`UPDATE agent_runs SET status = $1, completed_at = $2, metadata = metadata || $3
		 WHERE id = $4 AND org_id = $5 AND status = 'running'`,
		string(status), now, metadata, id, orgID,
	)
	if err != nil {
		return fmt.Errorf("storage: complete run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var existingStatus string
		err := tx.QueryRow(ctx,
			`SELECT status FROM agent_runs WHERE id = $1 AND org_id = $2`,
			id, orgID,
		).Scan(&existingStatus)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("storage: run %s: %w", id, ErrNotFound)
			}
			return fmt.Errorf("storage: complete run status lookup: %w", err)
		}
		if existingStatus == string(model.RunStatusCompleted) || existingStatus == string(model.RunStatusFailed) {
			// Idempotent — already finalized. Still commit to release tx.
			return tx.Commit(ctx)
		}
		return fmt.Errorf("storage: run %s complete transition rejected from status %q", id, existingStatus)
	}

	audit.ResourceID = id.String()
	if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
		return fmt.Errorf("storage: audit in complete run tx: %w", err)
	}

	return tx.Commit(ctx)
}

// ListRunsByAgent returns runs for a given agent_id within an org, ordered by started_at DESC.
func (db *DB) ListRunsByAgent(ctx context.Context, orgID uuid.UUID, agentID string, limit, offset int) ([]model.AgentRun, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

	var total int
	err := db.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM agent_runs WHERE org_id = $1 AND agent_id = $2`, orgID, agentID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("storage: count runs: %w", err)
	}

	rows, err := db.pool.Query(ctx,
		`SELECT id, agent_id, org_id, trace_id, parent_run_id, status, started_at, completed_at, metadata, created_at
		 FROM agent_runs WHERE org_id = $1 AND agent_id = $2
		 ORDER BY started_at DESC
		 LIMIT $3 OFFSET $4`,
		orgID, agentID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("storage: list runs: %w", err)
	}
	defer rows.Close()

	var runs []model.AgentRun
	for rows.Next() {
		var r model.AgentRun
		if err := rows.Scan(
			&r.ID, &r.AgentID, &r.OrgID, &r.TraceID, &r.ParentRunID,
			&r.Status, &r.StartedAt, &r.CompletedAt, &r.Metadata, &r.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("storage: scan run: %w", err)
		}
		runs = append(runs, r)
	}
	return runs, total, rows.Err()
}
