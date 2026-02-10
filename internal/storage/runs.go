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
	run := model.AgentRun{
		ID:          uuid.New(),
		AgentID:     req.AgentID,
		OrgID:       req.OrgID,
		TraceID:     req.TraceID,
		ParentRunID: req.ParentRunID,
		Status:      model.RunStatusRunning,
		StartedAt:   time.Now().UTC(),
		Metadata:    req.Metadata,
		CreatedAt:   time.Now().UTC(),
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
			return model.AgentRun{}, fmt.Errorf("storage: run not found: %s", id)
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
		return fmt.Errorf("storage: run not found or already completed: %s", id)
	}
	return nil
}

// ListRunsByAgent returns runs for a given agent_id within an org, ordered by started_at DESC.
func (db *DB) ListRunsByAgent(ctx context.Context, orgID uuid.UUID, agentID string, limit, offset int) ([]model.AgentRun, int, error) {
	if limit <= 0 {
		limit = 50
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
