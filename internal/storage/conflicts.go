package storage

import (
	"context"
	"fmt"

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
func (db *DB) RefreshAgentState(ctx context.Context) error {
	_, err := db.pool.Exec(ctx, `REFRESH MATERIALIZED VIEW agent_current_state`)
	if err != nil {
		return fmt.Errorf("storage: refresh agent state: %w", err)
	}
	return nil
}

// ListConflicts retrieves detected conflicts within an org, optionally filtered by decision_type.
func (db *DB) ListConflicts(ctx context.Context, orgID uuid.UUID, decisionType *string, limit int) ([]model.DecisionConflict, error) {
	if limit <= 0 {
		limit = 50
	}

	query := `SELECT decision_a_id, decision_b_id, org_id, agent_a, agent_b, run_a, run_b,
		 decision_type, outcome_a, outcome_b, confidence_a, confidence_b,
		 decided_at_a, decided_at_b, detected_at
		 FROM decision_conflicts WHERE org_id = $1`

	args := []any{orgID}
	if decisionType != nil {
		query += " AND decision_type = $2"
		args = append(args, *decisionType)
	}

	query += " ORDER BY detected_at DESC"
	query += fmt.Sprintf(" LIMIT %d", limit)

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
			&c.DecidedAtA, &c.DecidedAtB, &c.DetectedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan conflict: %w", err)
		}
		conflicts = append(conflicts, c)
	}
	return conflicts, rows.Err()
}
