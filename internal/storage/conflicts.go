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
// Joins the decisions table to include reasoning for both sides.
func (db *DB) ListConflicts(ctx context.Context, orgID uuid.UUID, decisionType *string, limit int) ([]model.DecisionConflict, error) {
	if limit <= 0 {
		limit = 50
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
	if decisionType != nil {
		query += " AND dc.decision_type = $2"
		args = append(args, *decisionType)
	}

	query += " ORDER BY dc.detected_at DESC"
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
			&c.ReasoningA, &c.ReasoningB,
			&c.DecidedAtA, &c.DecidedAtB, &c.DetectedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan conflict: %w", err)
		}
		conflicts = append(conflicts, c)
	}
	return conflicts, rows.Err()
}
