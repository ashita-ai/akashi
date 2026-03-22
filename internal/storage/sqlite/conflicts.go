package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// ListConflicts returns filtered conflicts.
func (l *LiteDB) ListConflicts(ctx context.Context, orgID uuid.UUID, filters storage.ConflictFilters, limit, offset int) ([]model.DecisionConflict, error) {
	if limit <= 0 {
		limit = 20
	}

	where, args := conflictWhere(orgID, filters)

	q := fmt.Sprintf( //nolint:gosec // G201
		`SELECT sc.id, sc.conflict_kind, sc.decision_a_id, sc.decision_b_id, sc.org_id,
		        sc.agent_a, sc.agent_b, sc.decision_type_a, sc.decision_type_b,
		        sc.outcome_a, sc.outcome_b,
		        sc.topic_similarity, sc.outcome_divergence, sc.significance, sc.scoring_method,
		        sc.explanation, sc.detected_at,
		        sc.category, sc.severity, sc.status,
		        sc.resolved_by, sc.resolved_at, sc.resolution_note,
		        sc.relationship, sc.confidence_weight, sc.temporal_decay,
		        sc.resolution_decision_id, sc.winning_decision_id, sc.group_id,
		        da.run_id, db.run_id, da.confidence, db.confidence,
		        da.reasoning, db.reasoning, da.valid_from, db.valid_from
		 FROM scored_conflicts sc
		 LEFT JOIN decisions da ON da.id = sc.decision_a_id
		 LEFT JOIN decisions db ON db.id = sc.decision_b_id
		 %s
		 ORDER BY sc.detected_at DESC
		 LIMIT ? OFFSET ?`,
		where,
	)
	args = append(args, limit, offset)

	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list conflicts: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	return scanConflictRows(rows)
}

// ListConflictGroups returns grouped conflicts.
func (l *LiteDB) ListConflictGroups(ctx context.Context, orgID uuid.UUID, filters storage.ConflictGroupFilters, limit, offset int) ([]model.ConflictGroup, error) {
	if limit <= 0 {
		limit = 20
	}

	var conds []string
	var args []any
	conds = append(conds, "cg.org_id = ?")
	args = append(args, uuidStr(orgID))

	if filters.DecisionType != nil {
		conds = append(conds, "cg.decision_type = ?")
		args = append(args, *filters.DecisionType)
	}
	if filters.AgentID != nil {
		conds = append(conds, "(cg.agent_a = ? OR cg.agent_b = ?)")
		args = append(args, *filters.AgentID, *filters.AgentID)
	}
	if filters.ConflictKind != nil {
		conds = append(conds, "cg.conflict_kind = ?")
		args = append(args, *filters.ConflictKind)
	}

	where := "WHERE " + strings.Join(conds, " AND ")

	// For open-only, add a HAVING filter after aggregation.
	having := ""
	if filters.OpenOnly {
		having = "HAVING SUM(CASE WHEN sc.status = 'open' THEN 1 ELSE 0 END) > 0"
	}

	q := fmt.Sprintf( //nolint:gosec // G201
		`SELECT cg.id, cg.org_id, cg.agent_a, cg.agent_b, cg.conflict_kind, cg.decision_type,
		        cg.first_detected_at, cg.last_detected_at,
		        COUNT(DISTINCT sc.id) AS conflict_count,
		        SUM(CASE WHEN sc.status = 'open' THEN 1 ELSE 0 END) AS open_count
		 FROM conflict_groups cg
		 LEFT JOIN scored_conflicts sc ON sc.group_id = cg.id
		 %s
		 GROUP BY cg.id, cg.org_id, cg.agent_a, cg.agent_b, cg.conflict_kind, cg.decision_type,
		          cg.first_detected_at, cg.last_detected_at
		 %s
		 ORDER BY cg.last_detected_at DESC
		 LIMIT ? OFFSET ?`,
		where, having,
	)
	args = append(args, limit, offset)

	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list conflict groups: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var groups []model.ConflictGroup
	for rows.Next() {
		var (
			g        model.ConflictGroup
			idStr    string
			orgIDStr string
			kind     string
			firstStr string
			lastStr  string
		)
		err := rows.Scan(&idStr, &orgIDStr, &g.AgentA, &g.AgentB, &kind, &g.DecisionType,
			&firstStr, &lastStr, &g.ConflictCount, &g.OpenCount)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan conflict group: %w", err)
		}
		g.ID = parseUUID(idStr)
		g.OrgID = parseUUID(orgIDStr)
		g.ConflictKind = model.ConflictKind(kind)
		g.FirstDetectedAt = parseTime(firstStr)
		g.LastDetectedAt = parseTime(lastStr)
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: conflict group rows: %w", err)
	}

	// Load representative and open conflicts for each group.
	for i := range groups {
		rep, err := l.loadRepresentativeConflict(ctx, groups[i].ID)
		if err != nil {
			return nil, err
		}
		groups[i].Representative = rep
	}

	if len(groups) == 0 {
		return []model.ConflictGroup{}, nil
	}
	return groups, nil
}

// loadRepresentativeConflict loads the most significant open conflict in a group.
func (l *LiteDB) loadRepresentativeConflict(ctx context.Context, groupID uuid.UUID) (*model.DecisionConflict, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT sc.id, sc.conflict_kind, sc.decision_a_id, sc.decision_b_id, sc.org_id,
		        sc.agent_a, sc.agent_b, sc.decision_type_a, sc.decision_type_b,
		        sc.outcome_a, sc.outcome_b,
		        sc.topic_similarity, sc.outcome_divergence, sc.significance, sc.scoring_method,
		        sc.explanation, sc.detected_at,
		        sc.category, sc.severity, sc.status,
		        sc.resolved_by, sc.resolved_at, sc.resolution_note,
		        sc.relationship, sc.confidence_weight, sc.temporal_decay,
		        sc.resolution_decision_id, sc.winning_decision_id, sc.group_id,
		        da.run_id, db.run_id, da.confidence, db.confidence,
		        da.reasoning, db.reasoning, da.valid_from, db.valid_from
		 FROM scored_conflicts sc
		 LEFT JOIN decisions da ON da.id = sc.decision_a_id
		 LEFT JOIN decisions db ON db.id = sc.decision_b_id
		 WHERE sc.group_id = ?
		 ORDER BY
		     CASE WHEN sc.status = 'open' THEN 0 ELSE 1 END ASC,
		     sc.significance DESC,
		     sc.detected_at DESC
		 LIMIT 1`,
		uuidStr(groupID),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: load representative conflict: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	conflicts, err := scanConflictRows(rows)
	if err != nil {
		return nil, err
	}
	if len(conflicts) == 0 {
		return nil, nil
	}
	return &conflicts[0], nil
}

// GetConflictCount returns the number of open conflicts for a decision.
func (l *LiteDB) GetConflictCount(ctx context.Context, decisionID, orgID uuid.UUID) (int, error) {
	var count int
	err := l.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM scored_conflicts
		 WHERE org_id = ? AND status = 'open'
		   AND (decision_a_id = ? OR decision_b_id = ?)`,
		uuidStr(orgID), uuidStr(decisionID), uuidStr(decisionID),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("sqlite: get conflict count: %w", err)
	}
	return count, nil
}

// GetConflictCountsBatch returns conflict counts for multiple decisions.
func (l *LiteDB) GetConflictCountsBatch(ctx context.Context, ids []uuid.UUID, orgID uuid.UUID) (map[uuid.UUID]int, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]int{}, nil
	}
	idsJSON := uuidSliceToJSON(ids)
	rows, err := l.db.QueryContext(ctx,
		`WITH batch AS (SELECT value AS id FROM json_each(?))
		 SELECT b.id, COUNT(*) AS conflict_count
		 FROM batch b
		 JOIN scored_conflicts sc
		      ON (sc.decision_a_id = b.id OR sc.decision_b_id = b.id)
		      AND sc.org_id = ?
		      AND sc.status = 'open'
		 GROUP BY b.id`,
		idsJSON, uuidStr(orgID),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: get conflict counts batch: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	result := make(map[uuid.UUID]int, len(ids))
	for rows.Next() {
		var idStr string
		var count int
		if err := rows.Scan(&idStr, &count); err != nil {
			return nil, fmt.Errorf("sqlite: scan conflict count: %w", err)
		}
		result[parseUUID(idStr)] = count
	}
	return result, rows.Err()
}

// GetResolvedConflictsByType returns resolved conflicts with a winning decision for a type.
func (l *LiteDB) GetResolvedConflictsByType(ctx context.Context, orgID uuid.UUID, decisionType string, limit int) ([]model.ConflictResolution, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := l.db.QueryContext(ctx,
		`SELECT sc.id, sc.decision_type_a,
		        sc.winning_decision_id,
		        CASE WHEN sc.winning_decision_id = sc.decision_a_id THEN sc.agent_a ELSE sc.agent_b END,
		        CASE WHEN sc.winning_decision_id = sc.decision_a_id THEN sc.outcome_a ELSE sc.outcome_b END,
		        CASE WHEN sc.winning_decision_id = sc.decision_a_id THEN sc.agent_b ELSE sc.agent_a END,
		        CASE WHEN sc.winning_decision_id = sc.decision_a_id THEN sc.outcome_b ELSE sc.outcome_a END,
		        sc.explanation, sc.resolution_note, sc.resolved_at
		 FROM scored_conflicts sc
		 WHERE sc.org_id = ? AND sc.status = 'resolved'
		   AND sc.winning_decision_id IS NOT NULL
		   AND (sc.decision_type_a = ? OR sc.decision_type_b = ?)
		 ORDER BY sc.resolved_at DESC
		 LIMIT ?`,
		uuidStr(orgID), decisionType, decisionType, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: get resolved conflicts by type: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var results []model.ConflictResolution
	for rows.Next() {
		var (
			r          model.ConflictResolution
			idStr      string
			winIDStr   string
			resolvedAt sql.NullString
		)
		err := rows.Scan(&idStr, &r.DecisionType, &winIDStr,
			&r.WinningAgent, &r.WinningOutcome, &r.LosingAgent, &r.LosingOutcome,
			&r.Explanation, &r.ResolutionNote, &resolvedAt)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan resolved conflict: %w", err)
		}
		r.ID = parseUUID(idStr)
		r.WinningDecisionID = parseUUID(winIDStr)
		if resolvedAt.Valid {
			r.ResolvedAt = parseTime(resolvedAt.String)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: resolved conflicts rows: %w", err)
	}
	if results == nil {
		results = []model.ConflictResolution{}
	}
	return results, nil
}

// GetConflict returns a single conflict by ID.
func (l *LiteDB) GetConflict(ctx context.Context, id, orgID uuid.UUID) (*model.DecisionConflict, error) {
	q := `SELECT sc.id, sc.conflict_kind, sc.decision_a_id, sc.decision_b_id, sc.org_id,
		        sc.agent_a, sc.agent_b, sc.decision_type_a, sc.decision_type_b,
		        sc.outcome_a, sc.outcome_b,
		        sc.topic_similarity, sc.outcome_divergence, sc.significance, sc.scoring_method,
		        sc.explanation, sc.detected_at,
		        sc.category, sc.severity, sc.status,
		        sc.resolved_by, sc.resolved_at, sc.resolution_note,
		        sc.relationship, sc.confidence_weight, sc.temporal_decay,
		        sc.resolution_decision_id, sc.winning_decision_id, sc.group_id,
		        da.run_id, db.run_id, da.confidence, db.confidence,
		        da.reasoning, db.reasoning, da.valid_from, db.valid_from
		 FROM scored_conflicts sc
		 LEFT JOIN decisions da ON da.id = sc.decision_a_id
		 LEFT JOIN decisions db ON db.id = sc.decision_b_id
		 WHERE sc.id = ? AND sc.org_id = ?`
	rows, err := l.db.QueryContext(ctx, q, uuidStr(id), uuidStr(orgID))
	if err != nil {
		return nil, fmt.Errorf("sqlite: get conflict: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	conflicts, err := scanConflictRows(rows)
	if err != nil {
		return nil, err
	}
	if len(conflicts) == 0 {
		return nil, nil
	}
	return &conflicts[0], nil
}

// UpdateConflictStatusWithAudit transitions a conflict to a new lifecycle state.
func (l *LiteDB) UpdateConflictStatusWithAudit(ctx context.Context, id, orgID uuid.UUID, status, resolvedBy string, resolutionNote *string, winningDecisionID *uuid.UUID, _ storage.MutationAuditEntry) (string, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("sqlite: begin conflict status tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var oldStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM scored_conflicts WHERE id = ? AND org_id = ?`,
		uuidStr(id), uuidStr(orgID),
	).Scan(&oldStatus)
	if err != nil {
		return "", fmt.Errorf("sqlite: get old conflict status: %w", err)
	}

	switch status {
	case "resolved", "false_positive":
		var winIDVal any
		if winningDecisionID != nil {
			winIDVal = uuidStr(*winningDecisionID)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE scored_conflicts
			 SET status = ?, resolved_by = ?, resolved_at = datetime('now'),
			     resolution_note = ?, winning_decision_id = ?
			 WHERE id = ? AND org_id = ?`,
			status, resolvedBy, resolutionNote, winIDVal,
			uuidStr(id), uuidStr(orgID),
		)
	default:
		_, err = tx.ExecContext(ctx,
			`UPDATE scored_conflicts SET status = ? WHERE id = ? AND org_id = ?`,
			status, uuidStr(id), uuidStr(orgID),
		)
	}
	if err != nil {
		return "", fmt.Errorf("sqlite: update conflict status: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("sqlite: commit conflict status: %w", err)
	}
	return oldStatus, nil
}

// CascadeResolveByOutcome is a no-op in SQLite mode (no pgvector for embedding similarity).
func (l *LiteDB) CascadeResolveByOutcome(_ context.Context, _, _, _, _ uuid.UUID, _ float64, _ storage.MutationAuditEntry) (int, error) {
	return 0, nil
}

// ---- Internal helpers ----

func conflictWhere(orgID uuid.UUID, f storage.ConflictFilters) (string, []any) {
	var conds []string
	var args []any

	conds = append(conds, "sc.org_id = ?")
	args = append(args, uuidStr(orgID))

	if f.DecisionType != nil {
		conds = append(conds, "(LOWER(TRIM(sc.decision_type_a)) = LOWER(TRIM(?)) OR LOWER(TRIM(sc.decision_type_b)) = LOWER(TRIM(?)))")
		args = append(args, *f.DecisionType, *f.DecisionType)
	}
	if f.AgentID != nil {
		conds = append(conds, "(sc.agent_a = ? OR sc.agent_b = ?)")
		args = append(args, *f.AgentID, *f.AgentID)
	}
	if f.ConflictKind != nil {
		conds = append(conds, "sc.conflict_kind = ?")
		args = append(args, *f.ConflictKind)
	}
	if f.Status != nil {
		conds = append(conds, "sc.status = ?")
		args = append(args, *f.Status)
	}
	if f.Severity != nil {
		conds = append(conds, "sc.severity = ?")
		args = append(args, *f.Severity)
	}
	if f.Category != nil {
		conds = append(conds, "sc.category = ?")
		args = append(args, *f.Category)
	}
	if f.DecisionID != nil {
		conds = append(conds, "(sc.decision_a_id = ? OR sc.decision_b_id = ?)")
		args = append(args, uuidStr(*f.DecisionID), uuidStr(*f.DecisionID))
	}
	if f.GroupID != nil {
		conds = append(conds, "sc.group_id = ?")
		args = append(args, uuidStr(*f.GroupID))
	}

	return "WHERE " + strings.Join(conds, " AND "), args
}

func scanConflictRows(rows *sql.Rows) ([]model.DecisionConflict, error) {
	var conflicts []model.DecisionConflict
	for rows.Next() {
		var (
			c             model.DecisionConflict
			idStr         string
			kind          string
			decAStr       string
			decBStr       string
			orgStr        string
			detectedStr   string
			category      sql.NullString
			severity      sql.NullString
			resolvedBy    sql.NullString
			resolvedAt    sql.NullString
			resNote       sql.NullString
			relationship  sql.NullString
			resDecisionID sql.NullString
			winningDecID  sql.NullString
			groupID       sql.NullString
			runAStr       sql.NullString
			runBStr       sql.NullString
			confA         sql.NullFloat64
			confB         sql.NullFloat64
			reasonA       sql.NullString
			reasonB       sql.NullString
			decidedAtAStr sql.NullString
			decidedAtBStr sql.NullString
		)
		err := rows.Scan(
			&idStr, &kind, &decAStr, &decBStr, &orgStr,
			&c.AgentA, &c.AgentB, &c.DecisionTypeA, &c.DecisionTypeB,
			&c.OutcomeA, &c.OutcomeB,
			&c.TopicSimilarity, &c.OutcomeDivergence, &c.Significance, &c.ScoringMethod,
			&c.Explanation, &detectedStr,
			&category, &severity, &c.Status,
			&resolvedBy, &resolvedAt, &resNote,
			&relationship, &c.ConfidenceWeight, &c.TemporalDecay,
			&resDecisionID, &winningDecID, &groupID,
			&runAStr, &runBStr, &confA, &confB,
			&reasonA, &reasonB, &decidedAtAStr, &decidedAtBStr,
		)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan conflict: %w", err)
		}

		c.ID = parseUUID(idStr)
		c.ConflictKind = model.ConflictKind(kind)
		c.DecisionAID = parseUUID(decAStr)
		c.DecisionBID = parseUUID(decBStr)
		c.OrgID = parseUUID(orgStr)
		c.DetectedAt = parseTime(detectedStr)

		if category.Valid {
			c.Category = &category.String
		}
		if severity.Valid {
			c.Severity = &severity.String
		}
		if resolvedBy.Valid {
			c.ResolvedBy = &resolvedBy.String
		}
		c.ResolvedAt = parseNullTime(resolvedAt)
		if resNote.Valid {
			c.ResolutionNote = &resNote.String
		}
		if relationship.Valid {
			c.Relationship = &relationship.String
		}
		c.ResolutionDecisionID = parseNullUUID(resDecisionID)
		c.WinningDecisionID = parseNullUUID(winningDecID)
		c.GroupID = parseNullUUID(groupID)

		if runAStr.Valid {
			c.RunA = parseUUID(runAStr.String)
		}
		if runBStr.Valid {
			c.RunB = parseUUID(runBStr.String)
		}
		if confA.Valid {
			c.ConfidenceA = float32(confA.Float64)
		}
		if confB.Valid {
			c.ConfidenceB = float32(confB.Float64)
		}
		if reasonA.Valid {
			c.ReasoningA = &reasonA.String
		}
		if reasonB.Valid {
			c.ReasoningB = &reasonB.String
		}
		if decidedAtAStr.Valid {
			t := parseTime(decidedAtAStr.String)
			c.DecidedAtA = t
		}
		if decidedAtBStr.Valid {
			t := parseTime(decidedAtBStr.String)
			c.DecidedAtB = t
		}

		// Set DecisionType from the A-side as the canonical type.
		c.DecisionType = c.DecisionTypeA

		conflicts = append(conflicts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: conflict rows: %w", err)
	}
	if conflicts == nil {
		conflicts = []model.DecisionConflict{}
	}
	return conflicts, nil
}

// UpsertConflictLabel inserts or updates a ground truth label for a scored conflict.
// Mirrors the Postgres version: only inserts if the conflict belongs to the given org,
// and the ON CONFLICT update is guarded by org_id to prevent cross-tenant overwrites.
func (l *LiteDB) UpsertConflictLabel(ctx context.Context, cl storage.ConflictLabel) error {
	res, err := l.db.ExecContext(ctx,
		`INSERT INTO conflict_labels (scored_conflict_id, org_id, label, labeled_by, labeled_at, notes)
		 SELECT ?, ?, ?, ?, ?, ?
		 FROM scored_conflicts
		 WHERE id = ? AND org_id = ?
		 ON CONFLICT (scored_conflict_id) DO UPDATE SET
		   label = excluded.label,
		   labeled_by = excluded.labeled_by,
		   labeled_at = excluded.labeled_at,
		   notes = excluded.notes
		 WHERE conflict_labels.org_id = excluded.org_id`,
		uuidStr(cl.ScoredConflictID), uuidStr(cl.OrgID),
		cl.Label, cl.LabeledBy, cl.LabeledAt, cl.Notes,
		uuidStr(cl.ScoredConflictID), uuidStr(cl.OrgID),
	)
	if err != nil {
		return fmt.Errorf("sqlite: upsert conflict label: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("sqlite: upsert conflict label: conflict not found or org mismatch")
	}
	return nil
}
