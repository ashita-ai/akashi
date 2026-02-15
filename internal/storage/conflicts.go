package storage

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ashita-ai/akashi/internal/model"
)

// RefreshConflicts is a no-op. Semantic conflicts are populated event-driven
// by the conflict scorer when new decisions are traced. Kept for interface compatibility.
func (db *DB) RefreshConflicts(ctx context.Context) error {
	return nil
}

// RefreshAgentState refreshes the agent_current_state materialized view.
// Uses CONCURRENTLY to avoid blocking reads during refresh (requires the
// unique index idx_agent_current_state_agent_org from 001_initial.sql).
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
	ConflictKind *string // "cross_agent" or "self_contradiction"
	Status       *string // "open", "acknowledged", "resolved", "wont_fix"
	Severity     *string // "critical", "high", "medium", "low"
	Category     *string // "factual", "assessment", "strategic", "temporal"
}

// conflictWhere appends WHERE conditions for the common filter set.
// It returns the query suffix and the args slice (starting from argOffset).
// decision_type uses case-insensitive match to align with view normalization.
func conflictWhere(filters ConflictFilters, argOffset int) (string, []any) {
	var clause string
	var args []any
	if filters.DecisionType != nil {
		clause += fmt.Sprintf(" AND (LOWER(TRIM(sc.decision_type_a)) = LOWER(TRIM($%d)) OR LOWER(TRIM(sc.decision_type_b)) = LOWER(TRIM($%d)))", argOffset, argOffset)
		args = append(args, *filters.DecisionType)
		argOffset++
	}
	if filters.AgentID != nil {
		clause += fmt.Sprintf(" AND (sc.agent_a = $%d OR sc.agent_b = $%d)", argOffset, argOffset)
		args = append(args, *filters.AgentID)
		argOffset++
	}
	if filters.ConflictKind != nil {
		clause += fmt.Sprintf(" AND sc.conflict_kind = $%d", argOffset)
		args = append(args, *filters.ConflictKind)
		argOffset++
	}
	if filters.Status != nil {
		clause += fmt.Sprintf(" AND sc.status = $%d", argOffset)
		args = append(args, *filters.Status)
		argOffset++
	}
	if filters.Severity != nil {
		clause += fmt.Sprintf(" AND sc.severity = $%d", argOffset)
		args = append(args, *filters.Severity)
		argOffset++
	}
	if filters.Category != nil {
		clause += fmt.Sprintf(" AND sc.category = $%d", argOffset)
		args = append(args, *filters.Category)
	}
	return clause, args
}

// CountConflicts returns the total number of conflicts for an org.
func (db *DB) CountConflicts(ctx context.Context, orgID uuid.UUID, filters ConflictFilters) (int, error) {
	query := `SELECT COUNT(*) FROM scored_conflicts sc WHERE sc.org_id = $1`
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

// ListConflicts retrieves detected conflicts within an org from scored_conflicts.
// Joins decisions for reasoning, confidence, run_id, and valid_from.
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

	query := conflictSelectBase + ` WHERE sc.org_id = $1`

	args := []any{orgID}

	suffix, extra := conflictWhere(filters, 2)
	query += suffix
	args = append(args, extra...)

	query += fmt.Sprintf(" ORDER BY sc.detected_at DESC LIMIT %d OFFSET %d", limit, offset)

	rows, err := db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: list conflicts: %w", err)
	}
	defer rows.Close()

	return scanConflictRows(rows)
}

// conflictSelectBase is the common SELECT+JOIN clause for all conflict queries.
const conflictSelectBase = `SELECT sc.id, sc.conflict_kind, sc.decision_a_id, sc.decision_b_id, sc.org_id,
		 sc.agent_a, sc.agent_b,
		 sc.decision_type_a, sc.decision_type_b, sc.outcome_a, sc.outcome_b,
		 sc.topic_similarity, sc.outcome_divergence, sc.significance, sc.scoring_method,
		 sc.explanation, sc.detected_at,
		 sc.category, sc.severity, sc.status,
		 sc.resolved_by, sc.resolved_at, sc.resolution_note,
		 sc.relationship, sc.confidence_weight, sc.temporal_decay, sc.resolution_decision_id,
		 da.run_id, db.run_id, da.confidence, db.confidence, da.reasoning, db.reasoning, da.valid_from, db.valid_from
		 FROM scored_conflicts sc
		 LEFT JOIN decisions da ON da.id = sc.decision_a_id
		 LEFT JOIN decisions db ON db.id = sc.decision_b_id`

func scanConflictRows(rows pgx.Rows) ([]model.DecisionConflict, error) {
	var conflicts []model.DecisionConflict
	for rows.Next() {
		var c model.DecisionConflict
		var runA, runB uuid.UUID
		var confA, confB float32
		var reasonA, reasonB *string
		var validA, validB time.Time
		if err := rows.Scan(
			&c.ID, &c.ConflictKind, &c.DecisionAID, &c.DecisionBID, &c.OrgID, &c.AgentA, &c.AgentB,
			&c.DecisionTypeA, &c.DecisionTypeB, &c.OutcomeA, &c.OutcomeB,
			&c.TopicSimilarity, &c.OutcomeDivergence, &c.Significance, &c.ScoringMethod,
			&c.Explanation, &c.DetectedAt,
			&c.Category, &c.Severity, &c.Status,
			&c.ResolvedBy, &c.ResolvedAt, &c.ResolutionNote,
			&c.Relationship, &c.ConfidenceWeight, &c.TemporalDecay, &c.ResolutionDecisionID,
			&runA, &runB, &confA, &confB, &reasonA, &reasonB, &validA, &validB,
		); err != nil {
			return nil, fmt.Errorf("storage: scan conflict: %w", err)
		}
		c.RunA, c.RunB = runA, runB
		c.ConfidenceA, c.ConfidenceB = confA, confB
		c.ReasoningA, c.ReasoningB = reasonA, reasonB
		c.DecidedAtA, c.DecidedAtB = validA, validB
		c.DecisionType = c.DecisionTypeA
		conflicts = append(conflicts, c)
	}
	return conflicts, rows.Err()
}

// NewConflictsSinceByOrg returns conflicts detected after the given time for one
// organization from scored_conflicts.
func (db *DB) NewConflictsSinceByOrg(ctx context.Context, orgID uuid.UUID, since time.Time, limit int) ([]model.DecisionConflict, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := db.pool.Query(ctx,
		conflictSelectBase+` WHERE sc.org_id = $1 AND sc.detected_at > $2
		 ORDER BY sc.detected_at ASC
		 LIMIT $3`, orgID, since, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: new conflicts since by org: %w", err)
	}
	defer rows.Close()

	return scanConflictRows(rows)
}

// GetConflict retrieves a single conflict by its ID within an org.
func (db *DB) GetConflict(ctx context.Context, id, orgID uuid.UUID) (*model.DecisionConflict, error) {
	rows, err := db.pool.Query(ctx,
		conflictSelectBase+` WHERE sc.id = $1 AND sc.org_id = $2`, id, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: get conflict: %w", err)
	}
	defer rows.Close()

	conflicts, err := scanConflictRows(rows)
	if err != nil {
		return nil, err
	}
	if len(conflicts) == 0 {
		return nil, nil
	}
	return &conflicts[0], nil
}

// UpdateConflictStatus transitions a conflict to a new lifecycle state.
func (db *DB) UpdateConflictStatus(ctx context.Context, id, orgID uuid.UUID, status, resolvedBy string, resolutionNote *string) error {
	var tag pgconn.CommandTag
	var err error
	switch status {
	case "resolved", "wont_fix":
		tag, err = db.pool.Exec(ctx,
			`UPDATE scored_conflicts SET status = $1, resolved_by = $2, resolved_at = now(), resolution_note = $3
			 WHERE id = $4 AND org_id = $5`,
			status, resolvedBy, resolutionNote, id, orgID)
	default:
		tag, err = db.pool.Exec(ctx,
			`UPDATE scored_conflicts SET status = $1 WHERE id = $2 AND org_id = $3`,
			status, id, orgID)
	}
	if err != nil {
		return fmt.Errorf("storage: update conflict status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("storage: conflict not found")
	}
	return nil
}

// ResolveConflictWithDecision links a conflict resolution to the decision that resolved it.
func (db *DB) ResolveConflictWithDecision(ctx context.Context, id, orgID, resolutionDecisionID uuid.UUID, resolvedBy string, resolutionNote *string) error {
	tag, err := db.pool.Exec(ctx,
		`UPDATE scored_conflicts SET status = 'resolved', resolved_by = $1, resolved_at = now(),
		 resolution_note = $2, resolution_decision_id = $3
		 WHERE id = $4 AND org_id = $5`,
		resolvedBy, resolutionNote, resolutionDecisionID, id, orgID)
	if err != nil {
		return fmt.Errorf("storage: resolve conflict with decision: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("storage: conflict not found")
	}
	return nil
}

// GetConflictsByDecision returns all conflicts involving a specific decision.
func (db *DB) GetConflictsByDecision(ctx context.Context, orgID, decisionID uuid.UUID) ([]model.DecisionConflict, error) {
	rows, err := db.pool.Query(ctx,
		conflictSelectBase+` WHERE sc.org_id = $1 AND (sc.decision_a_id = $2 OR sc.decision_b_id = $2)
		 ORDER BY sc.detected_at DESC`, orgID, decisionID)
	if err != nil {
		return nil, fmt.Errorf("storage: get conflicts by decision: %w", err)
	}
	defer rows.Close()

	return scanConflictRows(rows)
}

// InsertScoredConflict inserts a semantic conflict into scored_conflicts.
// Ensures decision_a_id < decision_b_id for consistent ordering.
func (db *DB) InsertScoredConflict(ctx context.Context, c model.DecisionConflict) error {
	da, dbID := c.DecisionAID, c.DecisionBID
	agentA, agentB := c.AgentA, c.AgentB
	typeA, typeB := c.DecisionTypeA, c.DecisionTypeB
	outcomeA, outcomeB := c.OutcomeA, c.OutcomeB
	if bytes.Compare(da[:], dbID[:]) > 0 {
		da, dbID = dbID, da
		agentA, agentB = agentB, agentA
		typeA, typeB = typeB, typeA
		outcomeA, outcomeB = outcomeB, outcomeA
	}
	topicSim := 0.0
	if c.TopicSimilarity != nil {
		topicSim = *c.TopicSimilarity
	}
	outcomeDiv := 0.0
	if c.OutcomeDivergence != nil {
		outcomeDiv = *c.OutcomeDivergence
	}
	sig := 0.0
	if c.Significance != nil {
		sig = *c.Significance
	}
	method := c.ScoringMethod
	if method == "" {
		method = "embedding"
	}
	_, err := db.pool.Exec(ctx,
		`INSERT INTO scored_conflicts (decision_a_id, decision_b_id, org_id, conflict_kind,
		 agent_a, agent_b, decision_type_a, decision_type_b, outcome_a, outcome_b,
		 topic_similarity, outcome_divergence, significance, scoring_method, explanation,
		 category, severity, relationship, confidence_weight, temporal_decay)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
		 ON CONFLICT (decision_a_id, decision_b_id) DO UPDATE SET
		 topic_similarity = EXCLUDED.topic_similarity,
		 outcome_divergence = EXCLUDED.outcome_divergence,
		 significance = EXCLUDED.significance,
		 scoring_method = EXCLUDED.scoring_method,
		 explanation = EXCLUDED.explanation,
		 category = EXCLUDED.category,
		 severity = EXCLUDED.severity,
		 relationship = EXCLUDED.relationship,
		 confidence_weight = EXCLUDED.confidence_weight,
		 temporal_decay = EXCLUDED.temporal_decay,
		 detected_at = now()`,
		da, dbID, c.OrgID, string(c.ConflictKind),
		agentA, agentB, typeA, typeB, outcomeA, outcomeB,
		topicSim, outcomeDiv, sig, method, c.Explanation,
		c.Category, c.Severity, c.Relationship, c.ConfidenceWeight, c.TemporalDecay,
	)
	return err
}
