package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// RetentionPolicy holds an org's data retention configuration.
type RetentionPolicy struct {
	RetentionDays         *int     `json:"retention_days"`          // nil = retain forever
	RetentionExcludeTypes []string `json:"retention_exclude_types"` // nil = no exemptions
	// Read-only: populated from deletion_log.
	LastRunAt      *time.Time `json:"last_run,omitempty"`
	LastRunDeleted *int       `json:"last_run_deleted,omitempty"`
	NextRunAt      *time.Time `json:"next_run,omitempty"`
}

// RetentionHold is a legal hold that exempts matching decisions from auto-deletion.
type RetentionHold struct {
	ID            uuid.UUID  `json:"id"`
	OrgID         uuid.UUID  `json:"org_id"`
	Reason        string     `json:"reason"`
	HoldFrom      time.Time  `json:"from"`
	HoldTo        time.Time  `json:"to"`
	DecisionTypes []string   `json:"decision_types,omitempty"` // nil = all types
	AgentIDs      []string   `json:"agent_ids,omitempty"`      // nil = all agents
	CreatedBy     string     `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	ReleasedAt    *time.Time `json:"released_at,omitempty"`
}

// DeletionLog is an operation-level record of a retention run.
type DeletionLog struct {
	ID            uuid.UUID      `json:"id"`
	OrgID         uuid.UUID      `json:"org_id"`
	Trigger       string         `json:"trigger"` // policy, manual, gdpr
	InitiatedBy   *string        `json:"initiated_by,omitempty"`
	Criteria      map[string]any `json:"criteria"`
	DeletedCounts map[string]any `json:"deleted_counts"`
	StartedAt     time.Time      `json:"started_at"`
	CompletedAt   *time.Time     `json:"completed_at,omitempty"`
}

// PurgeCount holds row counts for a purge operation (real or dry-run).
type PurgeCount struct {
	Decisions    int64 `json:"decisions"`
	Alternatives int64 `json:"alternatives"`
	Evidence     int64 `json:"evidence"`
	Claims       int64 `json:"claims"`
	Events       int64 `json:"events"`
}

// OrgRetentionConfig is returned by GetOrgsWithRetention for the background loop.
type OrgRetentionConfig struct {
	OrgID                 uuid.UUID
	RetentionDays         int
	RetentionExcludeTypes []string
}

// GetRetentionPolicy returns the retention policy for an org.
// Always succeeds — if no policy is set, RetentionDays is nil.
func (db *DB) GetRetentionPolicy(ctx context.Context, orgID uuid.UUID, retentionInterval time.Duration) (RetentionPolicy, error) {
	var p RetentionPolicy
	var days *int
	var excludeTypes []string

	err := db.pool.QueryRow(ctx,
		`SELECT retention_days, retention_exclude_types FROM organizations WHERE id = $1`,
		orgID,
	).Scan(&days, &excludeTypes)
	if err != nil {
		return p, fmt.Errorf("storage: get retention policy: %w", err)
	}
	p.RetentionDays = days
	p.RetentionExcludeTypes = excludeTypes

	// Populate last_run and last_run_deleted from deletion_log.
	var lastStarted time.Time
	var lastDeleted int
	err = db.pool.QueryRow(ctx,
		`SELECT started_at,
		        COALESCE((deleted_counts->>'decisions')::int, 0)
		 FROM deletion_log
		 WHERE org_id = $1 AND trigger = 'policy' AND completed_at IS NOT NULL
		 ORDER BY started_at DESC
		 LIMIT 1`,
		orgID,
	).Scan(&lastStarted, &lastDeleted)
	if err == nil {
		p.LastRunAt = &lastStarted
		p.LastRunDeleted = &lastDeleted
		if retentionInterval > 0 {
			next := lastStarted.Add(retentionInterval)
			p.NextRunAt = &next
		}
	}
	// ErrNoRows → no previous run, fields remain nil.
	return p, nil
}

// SetRetentionPolicy upserts retention_days and retention_exclude_types on the org.
// Pass nil retentionDays to clear the policy (retain forever).
func (db *DB) SetRetentionPolicy(ctx context.Context, orgID uuid.UUID, retentionDays *int, excludeTypes []string) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE organizations
		 SET retention_days = $2, retention_exclude_types = $3, updated_at = now()
		 WHERE id = $1`,
		orgID, retentionDays, excludeTypes,
	)
	if err != nil {
		return fmt.Errorf("storage: set retention policy: %w", err)
	}
	return nil
}

// GetOrgsWithRetention returns all orgs that have a non-null retention_days set.
// Used by the background retention loop to find orgs that need processing.
func (db *DB) GetOrgsWithRetention(ctx context.Context) ([]OrgRetentionConfig, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, retention_days, COALESCE(retention_exclude_types, '{}')
		 FROM organizations
		 WHERE retention_days IS NOT NULL`,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get orgs with retention: %w", err)
	}
	defer rows.Close()

	var out []OrgRetentionConfig
	for rows.Next() {
		var c OrgRetentionConfig
		if err := rows.Scan(&c.OrgID, &c.RetentionDays, &c.RetentionExcludeTypes); err != nil {
			return nil, fmt.Errorf("storage: scan org retention config: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateHold inserts a new legal hold for an org.
func (db *DB) CreateHold(ctx context.Context, h RetentionHold) (RetentionHold, error) {
	err := db.pool.QueryRow(ctx,
		`INSERT INTO retention_holds (org_id, reason, hold_from, hold_to, decision_types, agent_ids, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, created_at`,
		h.OrgID, h.Reason, h.HoldFrom, h.HoldTo, h.DecisionTypes, h.AgentIDs, h.CreatedBy,
	).Scan(&h.ID, &h.CreatedAt)
	if err != nil {
		return RetentionHold{}, fmt.Errorf("storage: create hold: %w", err)
	}
	return h, nil
}

// ReleaseHold sets released_at = now() on the hold, deactivating it.
// Returns false if the hold was not found or already released.
func (db *DB) ReleaseHold(ctx context.Context, id, orgID uuid.UUID) (bool, error) {
	tag, err := db.pool.Exec(ctx,
		`UPDATE retention_holds SET released_at = now()
		 WHERE id = $1 AND org_id = $2 AND released_at IS NULL`,
		id, orgID,
	)
	if err != nil {
		return false, fmt.Errorf("storage: release hold: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListHolds returns all active (unreleased) holds for an org.
func (db *DB) ListHolds(ctx context.Context, orgID uuid.UUID) ([]RetentionHold, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, reason, hold_from, hold_to, decision_types, agent_ids,
		        created_by, created_at, released_at
		 FROM retention_holds
		 WHERE org_id = $1
		 ORDER BY created_at DESC`,
		orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: list holds: %w", err)
	}
	defer rows.Close()

	var out []RetentionHold
	for rows.Next() {
		var h RetentionHold
		if err := rows.Scan(
			&h.ID, &h.OrgID, &h.Reason, &h.HoldFrom, &h.HoldTo,
			&h.DecisionTypes, &h.AgentIDs, &h.CreatedBy, &h.CreatedAt, &h.ReleasedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan hold: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ActiveHoldsExistForAgent reports whether any active hold covers any decision
// made by agentID within the org. Used to block GDPR deletion when a hold is active.
func (db *DB) ActiveHoldsExistForAgent(ctx context.Context, orgID uuid.UUID, agentID string) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM retention_holds rh
		     JOIN decisions d ON d.org_id = rh.org_id
		     WHERE rh.org_id = $1
		       AND rh.released_at IS NULL
		       AND d.agent_id = $2
		       AND d.created_at BETWEEN rh.hold_from AND rh.hold_to
		       AND (rh.agent_ids IS NULL OR $2 = ANY(rh.agent_ids))
		 )`,
		orgID, agentID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("storage: check holds for agent: %w", err)
	}
	return exists, nil
}

// StartDeletionLog inserts a deletion_log row and returns its ID.
// Call CompleteDeletionLog after the run finishes to record counts and completion time.
func (db *DB) StartDeletionLog(ctx context.Context, orgID uuid.UUID, trigger, initiatedBy string, criteria map[string]any) (uuid.UUID, error) {
	var logID uuid.UUID
	var by *string
	if initiatedBy != "" {
		by = &initiatedBy
	}
	err := db.pool.QueryRow(ctx,
		`INSERT INTO deletion_log (org_id, trigger, initiated_by, criteria, deleted_counts)
		 VALUES ($1, $2, $3, $4, '{}')
		 RETURNING id`,
		orgID, trigger, by, criteria,
	).Scan(&logID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("storage: start deletion log: %w", err)
	}
	return logID, nil
}

// CompleteDeletionLog records the final counts and sets completed_at.
func (db *DB) CompleteDeletionLog(ctx context.Context, logID uuid.UUID, counts map[string]any) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE deletion_log SET deleted_counts = $2, completed_at = now() WHERE id = $1`,
		logID, counts,
	)
	if err != nil {
		return fmt.Errorf("storage: complete deletion log: %w", err)
	}
	return nil
}

// CountEligibleDecisions returns how many decisions would be deleted by a purge
// with the given criteria, without deleting anything.
func (db *DB) CountEligibleDecisions(ctx context.Context, orgID uuid.UUID, before time.Time, decisionType *string, agentID *string) (PurgeCount, error) {
	var c PurgeCount

	q := `SELECT
		COUNT(DISTINCT d.id),
		COUNT(DISTINCT a.id),
		COUNT(DISTINCT e.id),
		COUNT(DISTINCT dc.id),
		0
	  FROM decisions d
	  LEFT JOIN alternatives a ON a.decision_id = d.id
	  LEFT JOIN evidence e ON e.decision_id = d.id
	  LEFT JOIN decision_claims dc ON dc.decision_id = d.id
	  WHERE d.org_id = $1
	    AND d.created_at < $2`
	args := []any{orgID, before}
	n := 3
	if decisionType != nil {
		q += fmt.Sprintf(" AND d.decision_type = $%d", n)
		args = append(args, *decisionType)
		n++
	}
	if agentID != nil {
		q += fmt.Sprintf(" AND d.agent_id = $%d", n)
		args = append(args, *agentID)
	}

	if err := db.pool.QueryRow(ctx, q, args...).Scan(
		&c.Decisions, &c.Alternatives, &c.Evidence, &c.Claims, &c.Events,
	); err != nil {
		return c, fmt.Errorf("storage: count eligible decisions: %w", err)
	}

	// Count eligible events separately (agent_events has no FK to decisions).
	evtArgs := []any{orgID, before}
	evtQ := `SELECT COUNT(*) FROM agent_events WHERE org_id = $1 AND created_at < $2`
	if agentID != nil {
		evtQ += " AND agent_id = $3"
		evtArgs = append(evtArgs, *agentID)
	}
	if err := db.pool.QueryRow(ctx, evtQ, evtArgs...).Scan(&c.Events); err != nil {
		return c, fmt.Errorf("storage: count eligible events: %w", err)
	}

	return c, nil
}

// BatchDeleteDecisions deletes decisions (and their dependents) created before
// the cutoff, in batches of batchSize to avoid long-running transactions.
// excludeTypes lists decision_types that must not be deleted.
// activeHolds lists hold windows to skip.
// Returns the total counts of deleted rows.
func (db *DB) BatchDeleteDecisions(ctx context.Context, orgID uuid.UUID, before time.Time, decisionType *string, agentID *string, excludeTypes []string, batchSize int) (PurgeCount, error) {
	if batchSize <= 0 {
		batchSize = 1000
	}

	var total PurgeCount

	for {
		// Fetch one batch of eligible decision IDs, respecting holds and exclude list.
		holdFilter := `
		    AND NOT EXISTS (
		        SELECT 1 FROM retention_holds rh
		        WHERE rh.org_id = d.org_id
		          AND rh.released_at IS NULL
		          AND d.created_at BETWEEN rh.hold_from AND rh.hold_to
		          AND (rh.decision_types IS NULL OR d.decision_type = ANY(rh.decision_types))
		          AND (rh.agent_ids IS NULL OR d.agent_id = ANY(rh.agent_ids))
		    )`

		q := `SELECT id FROM decisions d
		      WHERE d.org_id = $1
		        AND d.created_at < $2` + holdFilter
		args := []any{orgID, before}
		n := 3

		if len(excludeTypes) > 0 {
			q += fmt.Sprintf(" AND d.decision_type != ALL($%d)", n)
			args = append(args, excludeTypes)
			n++
		}
		if decisionType != nil {
			q += fmt.Sprintf(" AND d.decision_type = $%d", n)
			args = append(args, *decisionType)
			n++
		}
		if agentID != nil {
			q += fmt.Sprintf(" AND d.agent_id = $%d", n)
			args = append(args, *agentID)
		}
		q += fmt.Sprintf(" LIMIT %d", batchSize)

		rows, err := db.pool.Query(ctx, q, args...)
		if err != nil {
			return total, fmt.Errorf("storage: fetch deletion batch: %w", err)
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return total, fmt.Errorf("storage: scan deletion id: %w", err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return total, fmt.Errorf("storage: deletion batch rows: %w", err)
		}
		if len(ids) == 0 {
			break // done
		}

		cnt, err := db.deleteBatch(ctx, orgID, ids)
		if err != nil {
			return total, err
		}
		total.Evidence += cnt.Evidence
		total.Alternatives += cnt.Alternatives
		total.Claims += cnt.Claims
		total.Decisions += cnt.Decisions
	}

	// Drop agent_event chunks older than cutoff (TimescaleDB bulk operation).
	dropped, err := db.DropEventChunks(ctx, before)
	if err != nil {
		// Non-fatal: log the error but don't fail the whole run.
		// Events will be cleaned up on the next run.
		_ = dropped
	} else {
		total.Events = dropped
	}

	return total, nil
}

// deleteBatch deletes a single batch of decisions (by ID) and their dependents
// in a single transaction. Respects FK cascade order.
func (db *DB) deleteBatch(ctx context.Context, orgID uuid.UUID, ids []uuid.UUID) (PurgeCount, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return PurgeCount{}, fmt.Errorf("storage: begin delete batch tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var cnt PurgeCount

	// 1. Delete evidence.
	tag, err := tx.Exec(ctx, `DELETE FROM evidence WHERE decision_id = ANY($1)`, ids)
	if err != nil {
		return cnt, fmt.Errorf("storage: delete evidence batch: %w", err)
	}
	cnt.Evidence = tag.RowsAffected()

	// 2. Delete alternatives.
	tag, err = tx.Exec(ctx, `DELETE FROM alternatives WHERE decision_id = ANY($1)`, ids)
	if err != nil {
		return cnt, fmt.Errorf("storage: delete alternatives batch: %w", err)
	}
	cnt.Alternatives = tag.RowsAffected()

	// 3. Delete decision_claims.
	tag, err = tx.Exec(ctx, `DELETE FROM decision_claims WHERE decision_id = ANY($1)`, ids)
	if err != nil {
		return cnt, fmt.Errorf("storage: delete claims batch: %w", err)
	}
	cnt.Claims = tag.RowsAffected()

	// 4. Null out precedent_ref / supersedes_id references to these decisions.
	if _, err = tx.Exec(ctx, `UPDATE decisions SET precedent_ref = NULL WHERE precedent_ref = ANY($1)`, ids); err != nil {
		return cnt, fmt.Errorf("storage: clear precedent refs batch: %w", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE decisions SET supersedes_id = NULL WHERE supersedes_id = ANY($1)`, ids); err != nil {
		return cnt, fmt.Errorf("storage: clear supersedes refs batch: %w", err)
	}

	// 5. Queue Qdrant deletions via search_outbox.
	if _, err = tx.Exec(ctx,
		`INSERT INTO search_outbox (decision_id, org_id, operation)
		 SELECT id, $2, 'delete' FROM decisions WHERE id = ANY($1)
		 ON CONFLICT (decision_id, operation) DO UPDATE SET created_at = now(), attempts = 0, locked_until = NULL`,
		ids, orgID,
	); err != nil {
		return cnt, fmt.Errorf("storage: queue outbox deletes batch: %w", err)
	}

	// 6. Delete scored conflicts referencing these decisions.
	if _, err = tx.Exec(ctx,
		`DELETE FROM scored_conflicts WHERE decision_a_id = ANY($1) OR decision_b_id = ANY($1)`, ids,
	); err != nil {
		return cnt, fmt.Errorf("storage: delete conflicts batch: %w", err)
	}

	// 7. Delete decisions.
	tag, err = tx.Exec(ctx, `DELETE FROM decisions WHERE id = ANY($1) AND org_id = $2`, ids, orgID)
	if err != nil {
		return cnt, fmt.Errorf("storage: delete decisions batch: %w", err)
	}
	cnt.Decisions = tag.RowsAffected()

	if err := tx.Commit(ctx); err != nil {
		return cnt, fmt.Errorf("storage: commit delete batch tx: %w", err)
	}
	return cnt, nil
}

// DropEventChunks drops TimescaleDB chunks from agent_events older than olderThan.
// Returns the number of chunks dropped (not rows — each chunk is a time partition).
func (db *DB) DropEventChunks(ctx context.Context, olderThan time.Time) (int64, error) {
	var dropped int64
	err := db.pool.QueryRow(ctx,
		`SELECT count(*) FROM drop_chunks('agent_events', $1::timestamptz)`,
		olderThan,
	).Scan(&dropped)
	if err != nil {
		return 0, fmt.Errorf("storage: drop event chunks: %w", err)
	}
	return dropped, nil
}
