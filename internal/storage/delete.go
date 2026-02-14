package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrAgentNotFound is returned when an agent doesn't exist.
// It wraps ErrNotFound so callers can use errors.Is(err, ErrNotFound) generically.
var ErrAgentNotFound = fmt.Errorf("storage: agent: %w", ErrNotFound)

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

// DeleteAgentData removes all data associated with an agent within an org in a single
// transaction. Deletes respect foreign key ordering: evidence and alternatives
// first, then decisions, events, runs, grants, and finally the agent row.
func (db *DB) DeleteAgentData(ctx context.Context, orgID uuid.UUID, agentID string) (DeleteAgentResult, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: begin delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var result DeleteAgentResult

	// Look up the agent's internal UUID for grant deletion.
	var agentUUID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM agents WHERE org_id = $1 AND agent_id = $2`, orgID, agentID).Scan(&agentUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeleteAgentResult{}, fmt.Errorf("%w: %s", ErrAgentNotFound, agentID)
		}
		return DeleteAgentResult{}, fmt.Errorf("storage: lookup agent: %w", err)
	}

	// 1. Delete evidence (via decision_id for this agent's decisions within the org).
	_, err = tx.Exec(ctx,
		`INSERT INTO deletion_audit_log (org_id, agent_id, table_name, record_id, record_data)
		 SELECT $1, $2, 'evidence', e.id::text, to_jsonb(e)
		 FROM evidence e
		 WHERE e.decision_id IN (
		     SELECT id FROM decisions WHERE org_id = $1 AND agent_id = $2
		 )`,
		orgID, agentID,
	)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: archive evidence for delete: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`DELETE FROM evidence WHERE decision_id IN (
			SELECT id FROM decisions WHERE org_id = $1 AND agent_id = $2
		)`, orgID, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: delete evidence: %w", err)
	}
	result.Evidence = tag.RowsAffected()

	// 2. Delete alternatives (via decision_id for this agent's decisions within the org).
	_, err = tx.Exec(ctx,
		`INSERT INTO deletion_audit_log (org_id, agent_id, table_name, record_id, record_data)
		 SELECT $1, $2, 'alternatives', a.id::text, to_jsonb(a)
		 FROM alternatives a
		 WHERE a.decision_id IN (
		     SELECT id FROM decisions WHERE org_id = $1 AND agent_id = $2
		 )`,
		orgID, agentID,
	)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: archive alternatives for delete: %w", err)
	}

	tag, err = tx.Exec(ctx,
		`DELETE FROM alternatives WHERE decision_id IN (
			SELECT id FROM decisions WHERE org_id = $1 AND agent_id = $2
		)`, orgID, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: delete alternatives: %w", err)
	}
	result.Alternatives = tag.RowsAffected()

	// 3. Clear precedent_ref self-references before deleting decisions.
	_, err = tx.Exec(ctx,
		`UPDATE decisions SET precedent_ref = NULL WHERE org_id = $1 AND agent_id = $2 AND precedent_ref IS NOT NULL`,
		orgID, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: clear precedent refs: %w", err)
	}

	// Also clear precedent_ref from OTHER agents that reference this agent's decisions.
	_, err = tx.Exec(ctx,
		`UPDATE decisions SET precedent_ref = NULL
		 WHERE precedent_ref IN (SELECT id FROM decisions WHERE org_id = $1 AND agent_id = $2)`,
		orgID, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: clear external precedent refs: %w", err)
	}

	// Also clear supersedes_id from OTHER agents that reference this agent's decisions.
	_, err = tx.Exec(ctx,
		`UPDATE decisions SET supersedes_id = NULL
		 WHERE supersedes_id IN (SELECT id FROM decisions WHERE org_id = $1 AND agent_id = $2)`,
		orgID, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: clear external supersedes refs: %w", err)
	}

	// 3b. Queue search index deletions for this agent's decisions.
	// Insert outbox entries before deleting the decisions so the worker can remove them from Qdrant.
	_, err = tx.Exec(ctx,
		`INSERT INTO search_outbox (decision_id, org_id, operation)
		 SELECT id, org_id, 'delete' FROM decisions WHERE org_id = $1 AND agent_id = $2
		 ON CONFLICT (decision_id, operation) DO UPDATE SET created_at = now(), attempts = 0, locked_until = NULL`,
		orgID, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: queue search outbox deletes: %w", err)
	}

	// 4. Delete scored conflicts referencing this agent's decisions.
	_, err = tx.Exec(ctx,
		`INSERT INTO deletion_audit_log (org_id, agent_id, table_name, record_id, record_data)
		 SELECT $1, $2, 'scored_conflicts',
		        (sc.decision_a_id::text || '::' || sc.decision_b_id::text),
		        to_jsonb(sc)
		 FROM scored_conflicts sc
		 WHERE sc.decision_a_id IN (SELECT id FROM decisions WHERE org_id = $1 AND agent_id = $2)
		    OR sc.decision_b_id IN (SELECT id FROM decisions WHERE org_id = $1 AND agent_id = $2)`,
		orgID, agentID,
	)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: archive scored conflicts for delete: %w", err)
	}

	_, err = tx.Exec(ctx,
		`DELETE FROM scored_conflicts
		 WHERE decision_a_id IN (SELECT id FROM decisions WHERE org_id = $1 AND agent_id = $2)
		    OR decision_b_id IN (SELECT id FROM decisions WHERE org_id = $1 AND agent_id = $2)`,
		orgID, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: delete scored conflicts: %w", err)
	}

	// 5. Delete decisions.
	_, err = tx.Exec(ctx,
		`INSERT INTO deletion_audit_log (org_id, agent_id, table_name, record_id, record_data)
		 SELECT $1, $2, 'decisions', d.id::text, to_jsonb(d)
		 FROM decisions d
		 WHERE d.org_id = $1 AND d.agent_id = $2`,
		orgID, agentID,
	)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: archive decisions for delete: %w", err)
	}

	tag, err = tx.Exec(ctx, `DELETE FROM decisions WHERE org_id = $1 AND agent_id = $2`, orgID, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: delete decisions: %w", err)
	}
	result.Decisions = tag.RowsAffected()

	// 6. Delete agent_events (hypertable — no FK, uses agent_id text).
	_, err = tx.Exec(ctx,
		`INSERT INTO deletion_audit_log (org_id, agent_id, table_name, record_id, record_data)
		 SELECT $1, $2, 'agent_events', ae.id::text, to_jsonb(ae)
		 FROM agent_events ae
		 WHERE ae.org_id = $1 AND ae.agent_id = $2`,
		orgID, agentID,
	)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: archive events for delete: %w", err)
	}

	tag, err = tx.Exec(ctx, `DELETE FROM agent_events WHERE org_id = $1 AND agent_id = $2`, orgID, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: delete events: %w", err)
	}
	result.Events = tag.RowsAffected()

	// 7. Clear parent_run_id self-references before deleting runs.
	_, err = tx.Exec(ctx,
		`UPDATE agent_runs SET parent_run_id = NULL WHERE org_id = $1 AND agent_id = $2 AND parent_run_id IS NOT NULL`,
		orgID, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: clear parent run refs: %w", err)
	}

	// Also clear parent_run_id from OTHER agents that reference this agent's runs.
	_, err = tx.Exec(ctx,
		`UPDATE agent_runs SET parent_run_id = NULL
		 WHERE parent_run_id IN (SELECT id FROM agent_runs WHERE org_id = $1 AND agent_id = $2)`,
		orgID, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: clear external parent run refs: %w", err)
	}

	// 8. Delete agent_runs.
	_, err = tx.Exec(ctx,
		`INSERT INTO deletion_audit_log (org_id, agent_id, table_name, record_id, record_data)
		 SELECT $1, $2, 'agent_runs', ar.id::text, to_jsonb(ar)
		 FROM agent_runs ar
		 WHERE ar.org_id = $1 AND ar.agent_id = $2`,
		orgID, agentID,
	)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: archive runs for delete: %w", err)
	}

	tag, err = tx.Exec(ctx, `DELETE FROM agent_runs WHERE org_id = $1 AND agent_id = $2`, orgID, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: delete runs: %w", err)
	}
	result.Runs = tag.RowsAffected()

	// 9. Delete access_grants (where agent is grantor or grantee within org).
	_, err = tx.Exec(ctx,
		`INSERT INTO deletion_audit_log (org_id, agent_id, table_name, record_id, record_data)
		 SELECT $1, $3, 'access_grants', g.id::text, to_jsonb(g)
		 FROM access_grants g
		 WHERE g.org_id = $1 AND (g.grantor_id = $2 OR g.grantee_id = $2)`,
		orgID, agentUUID, agentID,
	)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: archive grants for delete: %w", err)
	}

	tag, err = tx.Exec(ctx,
		`DELETE FROM access_grants WHERE org_id = $1 AND (grantor_id = $2 OR grantee_id = $2)`, orgID, agentUUID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: delete grants: %w", err)
	}
	result.Grants = tag.RowsAffected()

	// 10. Delete the agent itself.
	_, err = tx.Exec(ctx,
		`INSERT INTO deletion_audit_log (org_id, agent_id, table_name, record_id, record_data)
		 SELECT $1, $2, 'agents', a.id::text, to_jsonb(a)
		 FROM agents a
		 WHERE a.org_id = $1 AND a.agent_id = $2`,
		orgID, agentID,
	)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: archive agent for delete: %w", err)
	}

	tag, err = tx.Exec(ctx, `DELETE FROM agents WHERE org_id = $1 AND agent_id = $2`, orgID, agentID)
	if err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: delete agent: %w", err)
	}
	result.Agents = tag.RowsAffected()

	if err := tx.Commit(ctx); err != nil {
		return DeleteAgentResult{}, fmt.Errorf("storage: commit delete tx: %w", err)
	}

	return result, nil
}
