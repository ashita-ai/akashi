package storage

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// DeleteAgentResult contains the count of rows deleted per table.
type DeleteAgentResult struct {
	Evidence     int64 `json:"evidence"`
	Alternatives int64 `json:"alternatives"`
	Decisions    int64 `json:"decisions"`
	Events       int64 `json:"events"`
	Runs         int64 `json:"runs"`
	Grants       int64 `json:"grants"`
	Agents       int64 `json:"agents"`
}

// DeleteAgentData removes all data associated with an agent in a single
// transaction. Deletes respect foreign key ordering: evidence and alternatives
// first, then decisions, events, runs, grants, and finally the agent row.
func (db *DB) DeleteAgentData(ctx context.Context, agentID string) (DeleteAgentResult, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: begin delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var result DeleteAgentResult

	// Look up the agent's internal UUID for grant deletion.
	var agentUUID string
	err = tx.QueryRow(ctx, `SELECT id FROM agents WHERE agent_id = $1`, agentID).Scan(&agentUUID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: agent not found: %s", agentID)
	}

	// 1. Delete evidence (via decision_id for this agent's decisions).
	tag, err := tx.Exec(ctx,
		`DELETE FROM evidence WHERE decision_id IN (
			SELECT id FROM decisions WHERE agent_id = $1
		)`, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: delete evidence: %w", err)
	}
	result.Evidence = tag.RowsAffected()

	// 2. Delete alternatives (via decision_id for this agent's decisions).
	tag, err = tx.Exec(ctx,
		`DELETE FROM alternatives WHERE decision_id IN (
			SELECT id FROM decisions WHERE agent_id = $1
		)`, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: delete alternatives: %w", err)
	}
	result.Alternatives = tag.RowsAffected()

	// 3. Clear precedent_ref self-references before deleting decisions.
	_, err = tx.Exec(ctx,
		`UPDATE decisions SET precedent_ref = NULL WHERE agent_id = $1 AND precedent_ref IS NOT NULL`,
		agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: clear precedent refs: %w", err)
	}

	// Also clear precedent_ref from OTHER agents that reference this agent's decisions.
	_, err = tx.Exec(ctx,
		`UPDATE decisions SET precedent_ref = NULL
		 WHERE precedent_ref IN (SELECT id FROM decisions WHERE agent_id = $1)`,
		agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: clear external precedent refs: %w", err)
	}

	// 4. Delete decisions.
	tag, err = tx.Exec(ctx, `DELETE FROM decisions WHERE agent_id = $1`, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: delete decisions: %w", err)
	}
	result.Decisions = tag.RowsAffected()

	// 5. Delete agent_events (hypertable — no FK, uses agent_id text).
	tag, err = tx.Exec(ctx, `DELETE FROM agent_events WHERE agent_id = $1`, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: delete events: %w", err)
	}
	result.Events = tag.RowsAffected()

	// 6. Clear parent_run_id self-references before deleting runs.
	_, err = tx.Exec(ctx,
		`UPDATE agent_runs SET parent_run_id = NULL WHERE agent_id = $1 AND parent_run_id IS NOT NULL`,
		agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: clear parent run refs: %w", err)
	}

	// Also clear parent_run_id from OTHER agents that reference this agent's runs.
	_, err = tx.Exec(ctx,
		`UPDATE agent_runs SET parent_run_id = NULL
		 WHERE parent_run_id IN (SELECT id FROM agent_runs WHERE agent_id = $1)`,
		agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: clear external parent run refs: %w", err)
	}

	// 7. Delete agent_runs.
	tag, err = tx.Exec(ctx, `DELETE FROM agent_runs WHERE agent_id = $1`, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: delete runs: %w", err)
	}
	result.Runs = tag.RowsAffected()

	// 8. Delete access_grants (where agent is grantor or grantee).
	tag, err = tx.Exec(ctx,
		`DELETE FROM access_grants WHERE grantor_id = $1 OR grantee_id = $1`, agentUUID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: delete grants: %w", err)
	}
	result.Grants = tag.RowsAffected()

	// 9. Delete the agent itself.
	tag, err = tx.Exec(ctx, `DELETE FROM agents WHERE agent_id = $1`, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: delete agent: %w", err)
	}
	result.Agents = tag.RowsAffected()

	if err := tx.Commit(ctx); err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: commit delete tx: %w", err)
	}

	return result, nil
}
