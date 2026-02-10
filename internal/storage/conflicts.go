package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
)

// RefreshConflicts refreshes the decision_conflicts materialized view.
func (db *DB) RefreshConflicts(ctx context.Context) error {
	_, err := db.pool.Exec(ctx, `REFRESH MATERIALIZED VIEW CONCURRENTLY decision_conflicts`)
	if err != nil {
		return fmt.Errorf("storage: refresh conflicts: %w", err)
	}
	return nil
}

// RefreshAgentState refreshes the agent_current_state materialized view.
// Uses CONCURRENTLY to avoid blocking reads during refresh (requires the
// unique index idx_agent_current_state_agent_org from migration 016).
func (db *DB) RefreshAgentState(ctx context.Context) error {
	_, err := db.pool.Exec(ctx, `REFRESH MATERIALIZED VIEW CONCURRENTLY agent_current_state`)
	if err != nil {
		return fmt.Errorf("storage: refresh agent state: %w", err)
	}
	return nil
}

// ConflictFilters holds optional filters for conflict queries.
type ConflictFilters struct {
	DecisionType *string
	AgentID      *string
}

// conflictWhere appends WHERE conditions for the common filter set.
// It returns the query suffix and the args slice (starting from argOffset).
func conflictWhere(filters ConflictFilters, argOffset int) (string, []any) {
	var clause string
	var args []any
	if filters.DecisionType != nil {
		clause += fmt.Sprintf(" AND dc.decision_type = $%d", argOffset)
		args = append(args, *filters.DecisionType)
		argOffset++
	}
	if filters.AgentID != nil {
		clause += fmt.Sprintf(" AND (dc.agent_a = $%d OR dc.agent_b = $%d)", argOffset, argOffset)
		args = append(args, *filters.AgentID)
	}
	return clause, args
}

// CountConflicts returns the total number of conflicts for an org.
func (db *DB) CountConflicts(ctx context.Context, orgID uuid.UUID, filters ConflictFilters) (int, error) {
	query := `SELECT COUNT(*) FROM decision_conflicts dc WHERE dc.org_id = $1`
	args := []any{orgID}

	suffix, extra := conflictWhere(filters, 2)
	query += suffix
	args = append(args, extra...)

	var count int
	if err := db.pool.QueryRow(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("storage: count conflicts: %w", err)
	}
	return count, nil
}

// ListConflicts retrieves detected conflicts within an org.
// Joins the decisions table to include reasoning for both sides.
func (db *DB) ListConflicts(ctx context.Context, orgID uuid.UUID, filters ConflictFilters, limit, offset int) ([]model.DecisionConflict, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

	query := `SELECT dc.decision_a_id, dc.decision_b_id, dc.org_id,
		 dc.agent_a, dc.agent_b, dc.run_a, dc.run_b,
		 dc.decision_type, dc.outcome_a, dc.outcome_b,
		 dc.confidence_a, dc.confidence_b,
		 da.reasoning, db.reasoning,
		 dc.decided_at_a, dc.decided_at_b, dc.detected_at
		 FROM decision_conflicts dc
		 LEFT JOIN decisions da ON da.id = dc.decision_a_id
		 LEFT JOIN decisions db ON db.id = dc.decision_b_id
		 WHERE dc.org_id = $1`

	args := []any{orgID}

	suffix, extra := conflictWhere(filters, 2)
	query += suffix
	args = append(args, extra...)

	query += fmt.Sprintf(" ORDER BY dc.detected_at DESC LIMIT %d OFFSET %d", limit, offset)

	rows, err := db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: list conflicts: %w", err)
	}
	defer rows.Close()

	var conflicts []model.DecisionConflict
	for rows.Next() {
		var c model.DecisionConflict
		if err := rows.Scan(
			&c.DecisionAID, &c.DecisionBID, &c.OrgID, &c.AgentA, &c.AgentB, &c.RunA, &c.RunB,
			&c.DecisionType, &c.OutcomeA, &c.OutcomeB, &c.ConfidenceA, &c.ConfidenceB,
			&c.ReasoningA, &c.ReasoningB,
			&c.DecidedAtA, &c.DecidedAtB, &c.DetectedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan conflict: %w", err)
		}
		conflicts = append(conflicts, c)
	}
	return conflicts, rows.Err()
}

// NewConflictsSince returns conflicts detected after the given time, ordered by
// detected_at ascending. Used by the conflict refresh loop to detect new conflicts
// and send pg_notify events.
func (db *DB) NewConflictsSince(ctx context.Context, since time.Time) ([]model.DecisionConflict, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT dc.decision_a_id, dc.decision_b_id, dc.org_id,
		 dc.agent_a, dc.agent_b, dc.run_a, dc.run_b,
		 dc.decision_type, dc.outcome_a, dc.outcome_b,
		 dc.confidence_a, dc.confidence_b,
		 da.reasoning, db.reasoning,
		 dc.decided_at_a, dc.decided_at_b, dc.detected_at
		 FROM decision_conflicts dc
		 LEFT JOIN decisions da ON da.id = dc.decision_a_id
		 LEFT JOIN decisions db ON db.id = dc.decision_b_id
		 WHERE dc.detected_at > $1
		 ORDER BY dc.detected_at ASC`, since,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: new conflicts since: %w", err)
	}
	defer rows.Close()

	var conflicts []model.DecisionConflict
	for rows.Next() {
		var c model.DecisionConflict
		if err := rows.Scan(
			&c.DecisionAID, &c.DecisionBID, &c.OrgID, &c.AgentA, &c.AgentB, &c.RunA, &c.RunB,
			&c.DecisionType, &c.OutcomeA, &c.OutcomeB, &c.ConfidenceA, &c.ConfidenceB,
			&c.ReasoningA, &c.ReasoningB,
			&c.DecidedAtA, &c.DecidedAtB, &c.DetectedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan new conflict: %w", err)
		}
		conflicts = append(conflicts, c)
	}
	return conflicts, rows.Err()
}
