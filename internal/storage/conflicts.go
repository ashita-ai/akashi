//go:build !lite

package storage

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pgvector/pgvector-go"

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
		argOffset++
	}
	if filters.DecisionID != nil {
		clause += fmt.Sprintf(" AND (sc.decision_a_id = $%d OR sc.decision_b_id = $%d)", argOffset, argOffset)
		args = append(args, *filters.DecisionID)
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

// GetConflictStatusCounts returns the number of conflicts per resolution status for an org.
func (db *DB) GetConflictStatusCounts(ctx context.Context, orgID uuid.UUID) (ConflictStatusCounts, error) {
	var c ConflictStatusCounts
	err := db.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE status = 'open'),
		       count(*) FILTER (WHERE status = 'acknowledged'),
		       count(*) FILTER (WHERE status = 'resolved'),
		       count(*) FILTER (WHERE status = 'wont_fix')
		FROM scored_conflicts
		WHERE org_id = $1`, orgID).Scan(
		&c.Total, &c.Open, &c.Acknowledged, &c.Resolved, &c.WontFix)
	if err != nil {
		return c, fmt.Errorf("storage: conflict status counts: %w", err)
	}
	return c, nil
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
		 sc.winning_decision_id, sc.group_id,
		 sc.claim_text_a, sc.claim_text_b,
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
			&c.WinningDecisionID, &c.GroupID,
			&c.ClaimTextA, &c.ClaimTextB,
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

// GetResolvedConflictsByType returns recently resolved conflicts with a declared
// winner for the given decision type. Used by akashi_check to surface prior
// resolutions so agents avoid resurrecting the losing approach.
func (db *DB) GetResolvedConflictsByType(ctx context.Context, orgID uuid.UUID, decisionType string, limit int) ([]model.ConflictResolution, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := db.pool.Query(ctx, `
		SELECT
			sc.id,
			sc.decision_type_a,
			sc.winning_decision_id,
			CASE WHEN sc.winning_decision_id = sc.decision_a_id THEN sc.outcome_a ELSE sc.outcome_b END AS winning_outcome,
			CASE WHEN sc.winning_decision_id = sc.decision_a_id THEN sc.outcome_b ELSE sc.outcome_a END AS losing_outcome,
			CASE WHEN sc.winning_decision_id = sc.decision_a_id THEN sc.agent_a  ELSE sc.agent_b  END AS winning_agent,
			CASE WHEN sc.winning_decision_id = sc.decision_a_id THEN sc.agent_b  ELSE sc.agent_a  END AS losing_agent,
			sc.explanation,
			sc.resolution_note,
			sc.resolved_at
		FROM scored_conflicts sc
		WHERE sc.org_id = $1
		  AND sc.status = 'resolved'
		  AND sc.winning_decision_id IS NOT NULL
		  AND sc.decision_type_a = $2
		ORDER BY sc.resolved_at DESC NULLS LAST
		LIMIT $3`,
		orgID, decisionType, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get resolved conflicts by type: %w", err)
	}
	defer rows.Close()

	var results []model.ConflictResolution
	for rows.Next() {
		var r model.ConflictResolution
		if err := rows.Scan(
			&r.ID, &r.DecisionType, &r.WinningDecisionID,
			&r.WinningOutcome, &r.LosingOutcome,
			&r.WinningAgent, &r.LosingAgent,
			&r.Explanation, &r.ResolutionNote,
			&r.ResolvedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan resolved conflict: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
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

// UpdateConflictStatusWithAudit transitions a conflict to a new lifecycle
// state and inserts a mutation audit entry, atomically in a single transaction.
// winningDecisionID is optional; when provided it is written only for "resolved" transitions.
func (db *DB) UpdateConflictStatusWithAudit(ctx context.Context, id, orgID uuid.UUID, status, resolvedBy string, resolutionNote *string, winningDecisionID *uuid.UUID, audit MutationAuditEntry) (oldStatus string, err error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("storage: begin conflict status tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Read old status for audit before_data.
	if scanErr := tx.QueryRow(ctx,
		`SELECT status FROM scored_conflicts WHERE id = $1 AND org_id = $2 FOR UPDATE`,
		id, orgID).Scan(&oldStatus); scanErr != nil {
		return "", fmt.Errorf("storage: conflict not found")
	}

	var tag pgconn.CommandTag
	switch status {
	case "resolved", "wont_fix":
		// winning_decision_id is only meaningful for "resolved"; for wont_fix it
		// is intentionally left NULL (no winner declared on a "we don't care" close).
		var winner *uuid.UUID
		if status == "resolved" {
			winner = winningDecisionID
		}
		tag, err = tx.Exec(ctx,
			`UPDATE scored_conflicts
			 SET status = $1, resolved_by = $2, resolved_at = now(),
			     resolution_note = $3, winning_decision_id = $4
			 WHERE id = $5 AND org_id = $6`,
			status, resolvedBy, resolutionNote, winner, id, orgID)
	default:
		tag, err = tx.Exec(ctx,
			`UPDATE scored_conflicts SET status = $1 WHERE id = $2 AND org_id = $3`,
			status, id, orgID)
	}
	if err != nil {
		return "", fmt.Errorf("storage: update conflict status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", fmt.Errorf("storage: conflict not found")
	}

	audit.BeforeData = map[string]any{"status": oldStatus}
	afterData := map[string]any{"status": status, "resolved_by": resolvedBy}
	if winningDecisionID != nil && status == "resolved" {
		afterData["winning_decision_id"] = winningDecisionID.String()
	}
	audit.AfterData = afterData
	if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
		return "", fmt.Errorf("storage: audit in conflict status tx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("storage: commit conflict status tx: %w", err)
	}
	return oldStatus, nil
}

// InsertScoredConflict inserts a semantic conflict into scored_conflicts.
// When c.GroupID is set (pre-computed by FindOrCreateTopicGroup), the conflict
// is inserted with that group_id directly. When c.GroupID is nil, a fallback
// creates a new group atomically via CTE (used by callers without embeddings).
//
// Canonical pair ordering (decision_a_id < decision_b_id by bytes) is applied
// before the insert; the UNIQUE constraint on (decision_a_id, decision_b_id)
// prevents true duplicate rows.
//
// Returns the scored_conflicts row UUID (the ID field on c is ignored).
func (db *DB) InsertScoredConflict(ctx context.Context, c model.DecisionConflict) (uuid.UUID, error) {
	da, dbID := c.DecisionAID, c.DecisionBID
	agentA, agentB := c.AgentA, c.AgentB
	typeA, typeB := c.DecisionTypeA, c.DecisionTypeB
	outcomeA, outcomeB := c.OutcomeA, c.OutcomeB
	claimTextA, claimTextB := c.ClaimTextA, c.ClaimTextB
	if bytes.Compare(da[:], dbID[:]) > 0 {
		da, dbID = dbID, da
		agentA, agentB = agentB, agentA
		typeA, typeB = typeB, typeA
		outcomeA, outcomeB = outcomeB, outcomeA
		claimTextA, claimTextB = claimTextB, claimTextA
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

	// When the caller pre-computed a group_id (topic-aware path), insert
	// the conflict directly with that group and update the group timestamp.
	if c.GroupID != nil {
		return db.insertScoredConflictWithGroup(ctx,
			da, dbID, c.OrgID, c.ConflictKind,
			agentA, agentB, typeA, typeB, outcomeA, outcomeB,
			topicSim, outcomeDiv, sig, method, c.Explanation,
			c.Category, c.Severity, c.Relationship, c.ConfidenceWeight, c.TemporalDecay,
			claimTextA, claimTextB, *c.GroupID,
		)
	}

	// Fallback: find or create a group without embedding similarity.
	// Reuses the first existing group for the same agent-pair + decision-type
	// (preserving the old one-group-per-pair behavior when embeddings are unavailable).
	grpAgentA, grpAgentB := agentA, agentB
	if grpAgentA > grpAgentB {
		grpAgentA, grpAgentB = grpAgentB, grpAgentA
	}
	topicLabel := TruncateOutcome(outcomeA, 120)

	var id uuid.UUID
	err := db.pool.QueryRow(ctx,
		// CTE step 1: try to find an existing group for this agent pair + type.
		// CTE step 2: update last_detected_at when reusing an existing group.
		// CTE step 3: if none exists, create a new one.
		// CTE step 4: pick whichever returned a row (existing preferred).
		`WITH existing AS (
		     SELECT id FROM conflict_groups
		     WHERE org_id = $3 AND agent_a = $5 AND agent_b = $6
		       AND conflict_kind = $4 AND decision_type = $8
		     ORDER BY last_detected_at DESC
		     LIMIT 1
		 ), upd AS (
		     UPDATE conflict_groups SET last_detected_at = now()
		     WHERE id = (SELECT id FROM existing) AND org_id = $3
		 ), new_grp AS (
		     INSERT INTO conflict_groups
		         (org_id, agent_a, agent_b, conflict_kind, decision_type, group_topic)
		     SELECT $3, $5, $6, $4, $8, $23
		     WHERE NOT EXISTS (SELECT 1 FROM existing)
		     RETURNING id
		 ), grp AS (
		     SELECT id FROM existing
		     UNION ALL
		     SELECT id FROM new_grp
		     LIMIT 1
		 )
		 INSERT INTO scored_conflicts
		     (decision_a_id, decision_b_id, org_id, conflict_kind,
		      agent_a, agent_b, decision_type_a, decision_type_b, outcome_a, outcome_b,
		      topic_similarity, outcome_divergence, significance, scoring_method, explanation,
		      category, severity, relationship, confidence_weight, temporal_decay,
		      claim_text_a, claim_text_b, group_id)
		 SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
		        $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
		        $21, $22, grp.id
		 FROM grp
		 ON CONFLICT (decision_a_id, decision_b_id) DO UPDATE SET
		     topic_similarity    = EXCLUDED.topic_similarity,
		     outcome_divergence  = EXCLUDED.outcome_divergence,
		     significance        = EXCLUDED.significance,
		     scoring_method      = EXCLUDED.scoring_method,
		     explanation         = EXCLUDED.explanation,
		     category            = EXCLUDED.category,
		     severity            = EXCLUDED.severity,
		     relationship        = EXCLUDED.relationship,
		     confidence_weight   = EXCLUDED.confidence_weight,
		     temporal_decay      = EXCLUDED.temporal_decay,
		     claim_text_a        = EXCLUDED.claim_text_a,
		     claim_text_b        = EXCLUDED.claim_text_b,
		     group_id            = EXCLUDED.group_id,
		     detected_at         = now(),
		     status              = CASE WHEN scored_conflicts.status = 'resolved' THEN 'open'
		                                ELSE scored_conflicts.status END,
		     resolved_by         = CASE WHEN scored_conflicts.status = 'resolved' THEN NULL
		                                ELSE scored_conflicts.resolved_by END,
		     resolved_at         = CASE WHEN scored_conflicts.status = 'resolved' THEN NULL
		                                ELSE scored_conflicts.resolved_at END,
		     resolution_note     = CASE WHEN scored_conflicts.status = 'resolved' THEN NULL
		                                ELSE scored_conflicts.resolution_note END
		 RETURNING id`,
		da, dbID, c.OrgID, string(c.ConflictKind),
		grpAgentA, grpAgentB, typeA, typeB, outcomeA, outcomeB,
		topicSim, outcomeDiv, sig, method, c.Explanation,
		c.Category, c.Severity, c.Relationship, c.ConfidenceWeight, c.TemporalDecay,
		claimTextA, claimTextB, topicLabel,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// insertScoredConflictWithGroup inserts a conflict row linked to a pre-determined
// group_id and updates the group's last_detected_at timestamp.
func (db *DB) insertScoredConflictWithGroup(
	ctx context.Context,
	da, dbID, orgID uuid.UUID,
	conflictKind model.ConflictKind,
	agentA, agentB, typeA, typeB, outcomeA, outcomeB string,
	topicSim, outcomeDiv, sig float64,
	method string, explanation, category, severity, relationship *string,
	confWeight, tempDecay *float64,
	claimTextA, claimTextB *string,
	groupID uuid.UUID,
) (uuid.UUID, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("storage: begin insert conflict tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Update the group's last_detected_at.
	if _, err := tx.Exec(ctx,
		`UPDATE conflict_groups SET last_detected_at = now() WHERE id = $1 AND org_id = $2`,
		groupID, orgID,
	); err != nil {
		return uuid.Nil, fmt.Errorf("storage: update group timestamp: %w", err)
	}

	var id uuid.UUID
	err = tx.QueryRow(ctx,
		`INSERT INTO scored_conflicts
		     (decision_a_id, decision_b_id, org_id, conflict_kind,
		      agent_a, agent_b, decision_type_a, decision_type_b, outcome_a, outcome_b,
		      topic_similarity, outcome_divergence, significance, scoring_method, explanation,
		      category, severity, relationship, confidence_weight, temporal_decay,
		      claim_text_a, claim_text_b, group_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
		         $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
		         $21, $22, $23)
		 ON CONFLICT (decision_a_id, decision_b_id) DO UPDATE SET
		     topic_similarity    = EXCLUDED.topic_similarity,
		     outcome_divergence  = EXCLUDED.outcome_divergence,
		     significance        = EXCLUDED.significance,
		     scoring_method      = EXCLUDED.scoring_method,
		     explanation         = EXCLUDED.explanation,
		     category            = EXCLUDED.category,
		     severity            = EXCLUDED.severity,
		     relationship        = EXCLUDED.relationship,
		     confidence_weight   = EXCLUDED.confidence_weight,
		     temporal_decay      = EXCLUDED.temporal_decay,
		     claim_text_a        = EXCLUDED.claim_text_a,
		     claim_text_b        = EXCLUDED.claim_text_b,
		     group_id            = EXCLUDED.group_id,
		     detected_at         = now(),
		     status              = CASE WHEN scored_conflicts.status = 'resolved' THEN 'open'
		                                ELSE scored_conflicts.status END,
		     resolved_by         = CASE WHEN scored_conflicts.status = 'resolved' THEN NULL
		                                ELSE scored_conflicts.resolved_by END,
		     resolved_at         = CASE WHEN scored_conflicts.status = 'resolved' THEN NULL
		                                ELSE scored_conflicts.resolved_at END,
		     resolution_note     = CASE WHEN scored_conflicts.status = 'resolved' THEN NULL
		                                ELSE scored_conflicts.resolution_note END
		 RETURNING id`,
		da, dbID, orgID, string(conflictKind),
		agentA, agentB, typeA, typeB, outcomeA, outcomeB,
		topicSim, outcomeDiv, sig, method, explanation,
		category, severity, relationship, confWeight, tempDecay,
		claimTextA, claimTextB, groupID,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("storage: insert scored conflict: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("storage: commit insert conflict tx: %w", err)
	}
	return id, nil
}

// TopicGroupSimilarityThreshold is the minimum cosine similarity between a new
// conflict's outcome embedding and an existing group's representative outcome
// embedding for the conflict to join that group.
const TopicGroupSimilarityThreshold = 0.85

// FindOrCreateTopicGroup finds an existing conflict group whose representative
// conflict's outcome embedding has cosine similarity > 0.85 to the given
// embedding, or creates a new group. Uses pgvector's cosine distance operator
// to perform the comparison in SQL. The entire lookup-or-create is wrapped in
// a serializable transaction to prevent duplicate groups under concurrency.
// outcomeEmbedding is the outcome embedding of one of the decisions in the
// new conflict (typically the decision being scored). topicLabel is a
// human-readable label for newly created groups (truncated outcome text).
func (db *DB) FindOrCreateTopicGroup(
	ctx context.Context,
	orgID uuid.UUID,
	agentA, agentB string,
	conflictKind model.ConflictKind,
	decisionType string,
	outcomeEmbedding pgvector.Vector,
	topicLabel string,
) (uuid.UUID, error) {
	// Normalize agent pair ordering.
	if agentA > agentB {
		agentA, agentB = agentB, agentA
	}

	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return uuid.Nil, fmt.Errorf("storage: begin topic group tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Look for an existing group whose representative conflict's decision
	// has an outcome embedding similar enough to the new conflict. The
	// serializable transaction ensures no concurrent caller can insert a
	// duplicate between our SELECT and INSERT.
	var groupID uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT cg.id
		 FROM conflict_groups cg
		 JOIN LATERAL (
		     SELECT sc.decision_a_id
		     FROM scored_conflicts sc
		     WHERE sc.group_id = cg.id
		     ORDER BY
		         CASE WHEN sc.status IN ('open', 'acknowledged') THEN 0 ELSE 1 END,
		         sc.significance DESC NULLS LAST,
		         sc.detected_at DESC
		     LIMIT 1
		 ) rep ON true
		 JOIN decisions d ON d.id = rep.decision_a_id
		 WHERE cg.org_id = $1
		   AND cg.agent_a = $2
		   AND cg.agent_b = $3
		   AND cg.conflict_kind = $4
		   AND cg.decision_type = $5
		   AND d.outcome_embedding IS NOT NULL
		   AND 1 - (d.outcome_embedding <=> $6::vector) > $7
		 ORDER BY 1 - (d.outcome_embedding <=> $6::vector) DESC
		 LIMIT 1`,
		orgID, agentA, agentB, string(conflictKind), decisionType,
		outcomeEmbedding, TopicGroupSimilarityThreshold,
	).Scan(&groupID)

	if err == nil {
		// Found a matching group — update its timestamp.
		if _, err := tx.Exec(ctx,
			`UPDATE conflict_groups SET last_detected_at = now() WHERE id = $1 AND org_id = $2`,
			groupID, orgID,
		); err != nil {
			return uuid.Nil, fmt.Errorf("storage: update topic group timestamp: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return uuid.Nil, fmt.Errorf("storage: commit topic group tx: %w", err)
		}
		return groupID, nil
	}
	if err != pgx.ErrNoRows {
		return uuid.Nil, fmt.Errorf("storage: find topic group: %w", err)
	}

	// No matching group — create a new one.
	err = tx.QueryRow(ctx,
		`INSERT INTO conflict_groups
		     (org_id, agent_a, agent_b, conflict_kind, decision_type, group_topic)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id`,
		orgID, agentA, agentB, string(conflictKind), decisionType, topicLabel,
	).Scan(&groupID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("storage: create topic group: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("storage: commit topic group tx: %w", err)
	}
	return groupID, nil
}

// truncateOutcome returns the first maxLen characters of s, or s if shorter.
// Used to derive human-readable group_topic labels from outcome text.
func TruncateOutcome(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}

// listOpenConflictsByGroupIDs batch-fetches all open or acknowledged conflicts
// for the given set of conflict_group IDs within an org. Returns conflicts ordered
// by group_id then significance DESC so callers can attach them in display order
// without additional sorting.
func (db *DB) listOpenConflictsByGroupIDs(ctx context.Context, orgID uuid.UUID, groupIDs []uuid.UUID) ([]model.DecisionConflict, error) {
	if len(groupIDs) == 0 {
		return nil, nil
	}
	rows, err := db.pool.Query(ctx,
		conflictSelectBase+`
		 WHERE sc.group_id = ANY($1) AND sc.org_id = $2
		   AND sc.status IN ('open', 'acknowledged')
		 ORDER BY sc.group_id, sc.significance DESC NULLS LAST, sc.detected_at DESC`,
		groupIDs, orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: list open conflicts by group: %w", err)
	}
	defer rows.Close()
	return scanConflictRows(rows)
}

// conflictGroupWhere builds the WHERE clause suffix and args for ConflictGroupFilters.
// argOffset is the next positional parameter index.
func conflictGroupWhere(f ConflictGroupFilters, argOffset int) (string, []any) {
	var clause string
	var args []any
	if f.DecisionType != nil {
		clause += fmt.Sprintf(" AND cg.decision_type = $%d", argOffset)
		args = append(args, *f.DecisionType)
		argOffset++
	}
	if f.AgentID != nil {
		clause += fmt.Sprintf(" AND (cg.agent_a = $%d OR cg.agent_b = $%d)", argOffset, argOffset)
		args = append(args, *f.AgentID)
		argOffset++
	}
	if f.ConflictKind != nil {
		clause += fmt.Sprintf(" AND cg.conflict_kind = $%d", argOffset)
		args = append(args, *f.ConflictKind)
	}
	return clause, args
}

// CountConflictGroups returns the total number of conflict groups matching the filters.
func (db *DB) CountConflictGroups(ctx context.Context, orgID uuid.UUID, f ConflictGroupFilters) (int, error) {
	query := `SELECT COUNT(*) FROM conflict_groups cg WHERE cg.org_id = $1`
	args := []any{orgID}
	if f.OpenOnly {
		query = `
			SELECT COUNT(DISTINCT cg.id)
			FROM conflict_groups cg
			JOIN scored_conflicts sc ON sc.group_id = cg.id
			WHERE cg.org_id = $1
			  AND sc.status IN ('open', 'acknowledged')`
	}
	suffix, extra := conflictGroupWhere(f, 2)
	query += suffix
	args = append(args, extra...)

	var count int
	if err := db.pool.QueryRow(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("storage: count conflict groups: %w", err)
	}
	return count, nil
}

// ListConflictGroups returns conflict groups with counts and a representative
// conflict per group. Uses a LATERAL JOIN to pick the representative in a single
// query — no N+1.
//
// The representative is the highest-significance open/acknowledged conflict in the
// group, falling back to the highest-significance conflict overall when all are
// closed. This ensures expanding a group shows the conflict that needs attention.
func (db *DB) ListConflictGroups(ctx context.Context, orgID uuid.UUID, f ConflictGroupFilters, limit, offset int) ([]model.ConflictGroup, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

	// Build the optional open-only join/having.
	openJoin := ""
	openHaving := ""
	if f.OpenOnly {
		openJoin = `
			JOIN scored_conflicts sc_open ON sc_open.group_id = cg.id
			    AND sc_open.status IN ('open', 'acknowledged')`
		openHaving = " HAVING COUNT(DISTINCT sc_open.id) > 0"
	}

	query := fmt.Sprintf(`
		SELECT
		    cg.id, cg.org_id, cg.agent_a, cg.agent_b, cg.conflict_kind, cg.decision_type,
		    cg.group_topic,
		    cg.first_detected_at, cg.last_detected_at,
		    COUNT(DISTINCT sc_all.id)::int                                                AS conflict_count,
		    COUNT(DISTINCT sc_all.id) FILTER (
		        WHERE sc_all.status IN ('open', 'acknowledged'))::int                     AS open_count,
		    -- Representative: the highest-significance open/acknowledged conflict, or highest-significance overall.
		    rep.id, rep.conflict_kind, rep.decision_a_id, rep.decision_b_id,
		    rep.agent_a, rep.agent_b,
		    rep.decision_type_a, rep.decision_type_b, rep.outcome_a, rep.outcome_b,
		    rep.topic_similarity, rep.outcome_divergence, rep.significance, rep.scoring_method,
		    rep.explanation, rep.detected_at,
		    rep.category, rep.severity, rep.status,
		    rep.resolved_by, rep.resolved_at, rep.resolution_note,
		    rep.relationship, rep.confidence_weight, rep.temporal_decay,
		    rep.resolution_decision_id, rep.winning_decision_id, rep.group_id,
		    rep.claim_text_a, rep.claim_text_b,
		    rep_da.run_id, rep_db.run_id,
		    rep_da.confidence, rep_db.confidence,
		    rep_da.reasoning, rep_db.reasoning,
		    rep_da.valid_from, rep_db.valid_from
		FROM conflict_groups cg
		%s
		LEFT JOIN scored_conflicts sc_all ON sc_all.group_id = cg.id
		LEFT JOIN LATERAL (
		    SELECT sc2.*
		    FROM scored_conflicts sc2
		    WHERE sc2.group_id = cg.id
		    ORDER BY
		        -- Prefer actionable conflicts so expanding a group shows what needs attention.
		        CASE WHEN sc2.status IN ('open', 'acknowledged') THEN 0 ELSE 1 END ASC,
		        sc2.significance DESC NULLS LAST,
		        sc2.detected_at DESC
		    LIMIT 1
		) rep ON true
		LEFT JOIN decisions rep_da ON rep_da.id = rep.decision_a_id
		LEFT JOIN decisions rep_db ON rep_db.id = rep.decision_b_id
		WHERE cg.org_id = $1`, openJoin)

	args := []any{orgID}
	suffix, extra := conflictGroupWhere(f, 2)
	query += suffix
	args = append(args, extra...)

	query += `
		GROUP BY
		    cg.id, cg.org_id, cg.agent_a, cg.agent_b, cg.conflict_kind, cg.decision_type,
		    cg.group_topic,
		    cg.first_detected_at, cg.last_detected_at,
		    rep.id, rep.conflict_kind, rep.decision_a_id, rep.decision_b_id,
		    rep.agent_a, rep.agent_b,
		    rep.decision_type_a, rep.decision_type_b, rep.outcome_a, rep.outcome_b,
		    rep.topic_similarity, rep.outcome_divergence, rep.significance, rep.scoring_method,
		    rep.explanation, rep.detected_at,
		    rep.category, rep.severity, rep.status,
		    rep.resolved_by, rep.resolved_at, rep.resolution_note,
		    rep.relationship, rep.confidence_weight, rep.temporal_decay,
		    rep.resolution_decision_id, rep.winning_decision_id, rep.group_id,
		    rep.claim_text_a, rep.claim_text_b,
		    rep_da.run_id, rep_db.run_id,
		    rep_da.confidence, rep_db.confidence,
		    rep_da.reasoning, rep_db.reasoning,
		    rep_da.valid_from, rep_db.valid_from`
	query += openHaving
	query += fmt.Sprintf(" ORDER BY cg.last_detected_at DESC LIMIT %d OFFSET %d", limit, offset)

	rows, err := db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: list conflict groups: %w", err)
	}
	defer rows.Close()

	var groups []model.ConflictGroup
	for rows.Next() {
		var g model.ConflictGroup
		var rep model.DecisionConflict
		var repID *uuid.UUID
		var repRunA, repRunB *uuid.UUID
		var repConfA, repConfB *float32
		var repReasonA, repReasonB *string
		var repValidA, repValidB *time.Time
		var repConflictKind *string
		var repDecAID, repDecBID *uuid.UUID
		var repAgentA, repAgentB *string
		var repTypeA, repTypeB *string
		var repOutcomeA, repOutcomeB *string
		var repTopicSim, repOutcomeDiv, repSig *float64
		var repMethod *string
		var repExplanation *string
		var repDetectedAt *time.Time
		var repCategory, repSeverity, repStatus *string
		var repResolvedBy *string
		var repResolvedAt *time.Time
		var repResolutionNote *string
		var repRelationship *string
		var repConfWeight, repTempDecay *float64
		var repResDecID, repWinDecID, repGroupID *uuid.UUID
		var repClaimTextA, repClaimTextB *string

		if err := rows.Scan(
			&g.ID, &g.OrgID, &g.AgentA, &g.AgentB, &g.ConflictKind, &g.DecisionType,
			&g.GroupTopic,
			&g.FirstDetectedAt, &g.LastDetectedAt,
			&g.ConflictCount, &g.OpenCount,
			// representative columns
			&repID, &repConflictKind, &repDecAID, &repDecBID,
			&repAgentA, &repAgentB,
			&repTypeA, &repTypeB, &repOutcomeA, &repOutcomeB,
			&repTopicSim, &repOutcomeDiv, &repSig, &repMethod,
			&repExplanation, &repDetectedAt,
			&repCategory, &repSeverity, &repStatus,
			&repResolvedBy, &repResolvedAt, &repResolutionNote,
			&repRelationship, &repConfWeight, &repTempDecay,
			&repResDecID, &repWinDecID, &repGroupID,
			&repClaimTextA, &repClaimTextB,
			&repRunA, &repRunB,
			&repConfA, &repConfB,
			&repReasonA, &repReasonB,
			&repValidA, &repValidB,
		); err != nil {
			return nil, fmt.Errorf("storage: scan conflict group: %w", err)
		}

		// Only attach a representative if the LATERAL returned a row (group has members).
		if repID != nil {
			rep.ID = *repID
			if repConflictKind != nil {
				rep.ConflictKind = model.ConflictKind(*repConflictKind)
			}
			if repDecAID != nil {
				rep.DecisionAID = *repDecAID
			}
			if repDecBID != nil {
				rep.DecisionBID = *repDecBID
			}
			if repAgentA != nil {
				rep.AgentA = *repAgentA
			}
			if repAgentB != nil {
				rep.AgentB = *repAgentB
			}
			if repTypeA != nil {
				rep.DecisionTypeA = *repTypeA
				rep.DecisionType = *repTypeA
			}
			if repTypeB != nil {
				rep.DecisionTypeB = *repTypeB
			}
			if repOutcomeA != nil {
				rep.OutcomeA = *repOutcomeA
			}
			if repOutcomeB != nil {
				rep.OutcomeB = *repOutcomeB
			}
			rep.TopicSimilarity = repTopicSim
			rep.OutcomeDivergence = repOutcomeDiv
			rep.Significance = repSig
			if repMethod != nil {
				rep.ScoringMethod = *repMethod
			}
			rep.Explanation = repExplanation
			if repDetectedAt != nil {
				rep.DetectedAt = *repDetectedAt
			}
			rep.Category = repCategory
			rep.Severity = repSeverity
			if repStatus != nil {
				rep.Status = *repStatus
			}
			rep.ResolvedBy = repResolvedBy
			rep.ResolvedAt = repResolvedAt
			rep.ResolutionNote = repResolutionNote
			rep.Relationship = repRelationship
			rep.ConfidenceWeight = repConfWeight
			rep.TemporalDecay = repTempDecay
			rep.ResolutionDecisionID = repResDecID
			rep.WinningDecisionID = repWinDecID
			rep.GroupID = repGroupID
			rep.ClaimTextA = repClaimTextA
			rep.ClaimTextB = repClaimTextB
			if repRunA != nil {
				rep.RunA = *repRunA
			}
			if repRunB != nil {
				rep.RunB = *repRunB
			}
			if repConfA != nil {
				rep.ConfidenceA = *repConfA
			}
			if repConfB != nil {
				rep.ConfidenceB = *repConfB
			}
			rep.ReasoningA = repReasonA
			rep.ReasoningB = repReasonB
			if repValidA != nil {
				rep.DecidedAtA = *repValidA
			}
			if repValidB != nil {
				rep.DecidedAtB = *repValidB
			}
			g.Representative = &rep
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: conflict groups rows: %w", err)
	}

	// Batch-fetch all open/acknowledged conflicts for the returned groups so
	// the UI can show every open conflict, not just the single representative.
	if len(groups) > 0 {
		groupIDs := make([]uuid.UUID, len(groups))
		for i, g := range groups {
			groupIDs[i] = g.ID
		}
		openConflicts, err := db.listOpenConflictsByGroupIDs(ctx, orgID, groupIDs)
		if err != nil {
			return nil, fmt.Errorf("storage: open conflicts for groups: %w", err)
		}
		// Index by group_id for O(1) attachment.
		openByGroup := make(map[uuid.UUID][]model.DecisionConflict, len(groups))
		for _, c := range openConflicts {
			if c.GroupID != nil {
				openByGroup[*c.GroupID] = append(openByGroup[*c.GroupID], c)
			}
		}
		for i, g := range groups {
			if cs, ok := openByGroup[g.ID]; ok {
				groups[i].OpenConflicts = cs
			}
		}
	}

	return groups, nil
}

// ResolveConflictGroup batch-resolves all open or acknowledged conflicts in a
// conflict group. When winningAgent is non-nil, each conflict's winning_decision_id
// is set to the decision from that agent (decision_a_id when agent_a matches,
// decision_b_id when agent_b matches). Returns the number of conflicts updated.
func (db *DB) ResolveConflictGroup(
	ctx context.Context,
	groupID, orgID uuid.UUID,
	status, resolvedBy string,
	resolutionNote *string,
	winningAgent *string,
	audit MutationAuditEntry,
) (int, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("storage: begin resolve group tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Verify the group exists and belongs to this org.
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM conflict_groups WHERE id = $1 AND org_id = $2)`,
		groupID, orgID,
	).Scan(&exists); err != nil {
		return 0, fmt.Errorf("storage: check conflict group: %w", err)
	}
	if !exists {
		return 0, fmt.Errorf("storage: conflict group not found")
	}

	// Build the UPDATE. When winning_agent is set, derive winning_decision_id
	// per-row from the agent columns. Only "resolved" sets a winner;
	// "wont_fix" intentionally leaves winning_decision_id NULL.
	var tag pgconn.CommandTag
	if winningAgent != nil && status == "resolved" {
		tag, err = tx.Exec(ctx,
			`UPDATE scored_conflicts
			 SET status = $1,
			     resolved_by = $2,
			     resolved_at = now(),
			     resolution_note = $3,
			     winning_decision_id = CASE
			         WHEN agent_a = $4 THEN decision_a_id
			         WHEN agent_b = $4 THEN decision_b_id
			         ELSE NULL
			     END
			 WHERE group_id = $5 AND org_id = $6
			   AND status IN ('open', 'acknowledged')`,
			status, resolvedBy, resolutionNote, *winningAgent, groupID, orgID)
	} else {
		tag, err = tx.Exec(ctx,
			`UPDATE scored_conflicts
			 SET status = $1,
			     resolved_by = $2,
			     resolved_at = now(),
			     resolution_note = $3
			 WHERE group_id = $4 AND org_id = $5
			   AND status IN ('open', 'acknowledged')`,
			status, resolvedBy, resolutionNote, groupID, orgID)
	}
	if err != nil {
		return 0, fmt.Errorf("storage: resolve conflict group: %w", err)
	}

	affected := int(tag.RowsAffected())

	audit.BeforeData = map[string]any{"group_id": groupID.String(), "open_count": affected}
	afterData := map[string]any{"status": status, "resolved_by": resolvedBy, "resolved_count": affected}
	if winningAgent != nil {
		afterData["winning_agent"] = *winningAgent
	}
	audit.AfterData = afterData
	if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
		return 0, fmt.Errorf("storage: audit in resolve group tx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("storage: commit resolve group tx: %w", err)
	}
	return affected, nil
}

// AutoResolveSupersededConflictsTx resolves all open/acknowledged conflicts
// involving the superseded decision, within the caller's transaction. This is
// called from ReviseDecision so the auto-resolution is atomic with the
// revision itself.
//
// Each resolved conflict gets:
//   - status = "resolved"
//   - resolved_by = "system:revision"
//   - resolution_decision_id = revisedID (the new decision)
//   - resolution_note explaining the supersession
//
// Returns the number of conflicts auto-resolved.
func AutoResolveSupersededConflictsTx(ctx context.Context, tx pgx.Tx, orgID, supersededID, revisedID uuid.UUID) (int, error) {
	note := fmt.Sprintf("Auto-resolved: decision %s superseded by revision %s", supersededID, revisedID)
	tag, err := tx.Exec(ctx,
		`UPDATE scored_conflicts
		 SET status = 'resolved',
		     resolved_by = 'system:revision',
		     resolved_at = now(),
		     resolution_note = $1,
		     resolution_decision_id = $2
		 WHERE org_id = $3
		   AND (decision_a_id = $4 OR decision_b_id = $4)
		   AND status IN ('open', 'acknowledged')`,
		note, revisedID, orgID, supersededID,
	)
	if err != nil {
		return 0, fmt.Errorf("storage: auto-resolve superseded conflicts: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// CascadeResolveByOutcome auto-resolves open conflicts in the same conflict
// group when their outcome embeddings align with the winning decision's outcome.
// For each candidate conflict, cosine similarity is computed between the
// winning decision's outcome_embedding and each side's outcome_embedding.
// If one side exceeds the threshold (typically 0.80), that conflict is resolved
// with the aligned side as winner.
//
// The triggering conflict (triggerID) is excluded from the cascade.
// Returns the number of conflicts auto-resolved.
func (db *DB) CascadeResolveByOutcome(
	ctx context.Context,
	orgID, groupID, winningDecisionID, triggerID uuid.UUID,
	threshold float64,
	audit MutationAuditEntry,
) (int, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("storage: begin cascade resolve tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	note := fmt.Sprintf(
		"Auto-resolved via cascade from conflict %s. Aligned with winning decision %s (threshold %.2f).",
		triggerID, winningDecisionID, threshold,
	)

	// Use pgvector's cosine distance operator (<=>). Cosine similarity = 1 - distance.
	// For each open conflict in the group, compare both sides' outcome_embeddings
	// against the winner. If exactly one side exceeds the threshold, resolve with
	// that side as winner. If both exceed, pick the more aligned side.
	tag, err := tx.Exec(ctx, `
		WITH winning AS (
			SELECT outcome_embedding
			FROM decisions
			WHERE id = $1 AND org_id = $2
		),
		candidates AS (
			SELECT
				sc.id,
				sc.decision_a_id,
				sc.decision_b_id,
				CASE
					WHEN da.outcome_embedding IS NOT NULL AND w.outcome_embedding IS NOT NULL
					THEN 1.0 - (da.outcome_embedding <=> w.outcome_embedding)
					ELSE 0.0
				END AS sim_a,
				CASE
					WHEN db.outcome_embedding IS NOT NULL AND w.outcome_embedding IS NOT NULL
					THEN 1.0 - (db.outcome_embedding <=> w.outcome_embedding)
					ELSE 0.0
				END AS sim_b
			FROM scored_conflicts sc
			JOIN decisions da ON da.id = sc.decision_a_id AND da.org_id = $2
			JOIN decisions db ON db.id = sc.decision_b_id AND db.org_id = $2
			CROSS JOIN winning w
			WHERE sc.group_id = $3
			  AND sc.org_id = $2
			  AND sc.id != $4
			  AND sc.status IN ('open', 'acknowledged')
		)
		UPDATE scored_conflicts sc
		SET status = 'resolved',
		    resolved_by = 'cascade',
		    resolved_at = now(),
		    resolution_note = $5,
		    winning_decision_id = CASE
		        WHEN c.sim_a >= c.sim_b THEN c.decision_a_id
		        ELSE c.decision_b_id
		    END
		FROM candidates c
		WHERE sc.id = c.id
		  AND (c.sim_a >= $6 OR c.sim_b >= $6)`,
		winningDecisionID, orgID, groupID, triggerID, note, threshold,
	)
	if err != nil {
		return 0, fmt.Errorf("storage: cascade resolve by outcome: %w", err)
	}

	affected := int(tag.RowsAffected())
	if affected == 0 {
		// Nothing cascaded — skip audit entry and commit.
		if err := tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("storage: commit cascade resolve tx: %w", err)
		}
		return 0, nil
	}

	audit.BeforeData = map[string]any{
		"trigger_conflict_id":  triggerID.String(),
		"winning_decision_id":  winningDecisionID.String(),
		"group_id":             groupID.String(),
		"similarity_threshold": threshold,
	}
	audit.AfterData = map[string]any{
		"cascade_resolved": affected,
		"resolved_by":      "cascade",
	}
	if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
		return 0, fmt.Errorf("storage: audit in cascade resolve tx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("storage: commit cascade resolve tx: %w", err)
	}
	return affected, nil
}

// AgentWinRate holds an agent's historical resolution win rate for a decision type.
type AgentWinRate struct {
	AgentID  string
	WinRate  float64
	WinCount int
	Total    int
}

// GetAgentWinRates returns resolution win rates for the given agents within a
// decision type. Only counts resolved conflicts with a declared winner.
// Results are keyed by agent ID; agents with no history are absent from the map.
func (db *DB) GetAgentWinRates(ctx context.Context, orgID uuid.UUID, agentIDs []string, decisionType string) (map[string]AgentWinRate, error) {
	if len(agentIDs) == 0 {
		return map[string]AgentWinRate{}, nil
	}

	rows, err := db.pool.Query(ctx, `
		WITH agent_conflicts AS (
			SELECT
				unnest(ARRAY[
					CASE WHEN agent_a = ANY($2) THEN agent_a END,
					CASE WHEN agent_b = ANY($2) THEN agent_b END
				]) AS agent_id,
				winning_decision_id,
				decision_a_id, decision_b_id,
				agent_a, agent_b
			FROM scored_conflicts
			WHERE org_id = $1
			  AND status = 'resolved'
			  AND winning_decision_id IS NOT NULL
			  AND (decision_type_a = $3 OR decision_type_b = $3)
			  AND (agent_a = ANY($2) OR agent_b = ANY($2))
		)
		SELECT
			agent_id,
			COUNT(*)::int AS total,
			COUNT(*) FILTER (WHERE
				(agent_id = agent_a AND winning_decision_id = decision_a_id) OR
				(agent_id = agent_b AND winning_decision_id = decision_b_id)
			)::int AS wins
		FROM agent_conflicts
		WHERE agent_id IS NOT NULL
		GROUP BY agent_id`,
		orgID, agentIDs, decisionType,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get agent win rates: %w", err)
	}
	defer rows.Close()

	result := make(map[string]AgentWinRate)
	for rows.Next() {
		var r AgentWinRate
		if err := rows.Scan(&r.AgentID, &r.Total, &r.WinCount); err != nil {
			return nil, fmt.Errorf("storage: scan agent win rate: %w", err)
		}
		if r.Total > 0 {
			r.WinRate = float64(r.WinCount) / float64(r.Total)
		}
		result[r.AgentID] = r
	}
	return result, rows.Err()
}

// ConflictAnalyticsFilters holds the time range and optional filters
// for the conflict analytics aggregation queries.
type ConflictAnalyticsFilters struct {
	From         time.Time
	To           time.Time
	AgentID      *string
	DecisionType *string
	ConflictKind *string
}

// analyticsWhere appends WHERE conditions for analytics filters.
// argOffset is the next positional parameter ($N) to use.
func analyticsWhere(f ConflictAnalyticsFilters, argOffset int) (string, []any) {
	var clause string
	var args []any
	if f.AgentID != nil {
		clause += fmt.Sprintf(" AND (sc.agent_a = $%d OR sc.agent_b = $%d)", argOffset, argOffset)
		args = append(args, *f.AgentID)
		argOffset++
	}
	if f.DecisionType != nil {
		clause += fmt.Sprintf(" AND (LOWER(TRIM(sc.decision_type_a)) = LOWER(TRIM($%d)) OR LOWER(TRIM(sc.decision_type_b)) = LOWER(TRIM($%d)))", argOffset, argOffset)
		args = append(args, *f.DecisionType)
		argOffset++
	}
	if f.ConflictKind != nil {
		clause += fmt.Sprintf(" AND sc.conflict_kind = $%d", argOffset)
		args = append(args, *f.ConflictKind)
	}
	return clause, args
}

// GetConflictAnalytics computes aggregate conflict metrics for the given
// org and time window. It returns summary stats, breakdowns by agent pair /
// decision type / severity, and a daily detected-vs-resolved trend.
func (db *DB) GetConflictAnalytics(ctx context.Context, orgID uuid.UUID, filters ConflictAnalyticsFilters) (model.ConflictAnalytics, error) {
	var result model.ConflictAnalytics
	result.Period = model.TimePeriod{Start: filters.From, End: filters.To}

	baseWhere := "sc.org_id = $1 AND sc.detected_at >= $2 AND sc.detected_at < $3"
	extraClause, extraArgs := analyticsWhere(filters, 4)
	where := baseWhere + extraClause

	// Build args slice without mutating a shared base (gocritic: appendAssign).
	args := make([]any, 0, 3+len(extraArgs))
	args = append(args, orgID, filters.From, filters.To)
	args = append(args, extraArgs...)

	// 1. Summary: total detected, resolved, MTTR, false-positive rate.
	summaryQuery := fmt.Sprintf(`
		SELECT
			count(*),
			count(*) FILTER (WHERE sc.status IN ('resolved', 'wont_fix')),
			avg(EXTRACT(EPOCH FROM (sc.resolved_at - sc.detected_at)) / 3600)
				FILTER (WHERE sc.resolved_at IS NOT NULL),
			COALESCE(
				count(*) FILTER (WHERE sc.status = 'wont_fix')::double precision
				/ NULLIF(count(*) FILTER (WHERE sc.status IN ('resolved', 'wont_fix')), 0),
				0
			)
		FROM scored_conflicts sc
		WHERE %s`, where)

	if err := db.pool.QueryRow(ctx, summaryQuery, args...).Scan(
		&result.Summary.TotalDetected,
		&result.Summary.TotalResolved,
		&result.Summary.MeanTimeToResolutionHours,
		&result.Summary.FalsePositiveRate,
	); err != nil {
		return result, fmt.Errorf("storage: conflict analytics summary: %w", err)
	}

	// 2. By agent pair (top 50).
	pairQuery := fmt.Sprintf(`
		SELECT sc.agent_a, sc.agent_b,
			count(*),
			count(*) FILTER (WHERE sc.status IN ('open', 'acknowledged')),
			count(*) FILTER (WHERE sc.status IN ('resolved', 'wont_fix'))
		FROM scored_conflicts sc
		WHERE %s
		GROUP BY sc.agent_a, sc.agent_b
		ORDER BY count(*) DESC
		LIMIT 50`, where)

	pairRows, err := db.pool.Query(ctx, pairQuery, args...)
	if err != nil {
		return result, fmt.Errorf("storage: conflict analytics by agent pair: %w", err)
	}
	defer pairRows.Close()

	result.ByAgentPair = []model.AgentPairConflictStats{}
	for pairRows.Next() {
		var s model.AgentPairConflictStats
		if err := pairRows.Scan(&s.AgentA, &s.AgentB, &s.Count, &s.Open, &s.Resolved); err != nil {
			return result, fmt.Errorf("storage: scan conflict analytics agent pair: %w", err)
		}
		result.ByAgentPair = append(result.ByAgentPair, s)
	}
	if err := pairRows.Err(); err != nil {
		return result, fmt.Errorf("storage: conflict analytics agent pair rows: %w", err)
	}

	// 3. By decision type (top 50, using decision_type_a as canonical).
	typeQuery := fmt.Sprintf(`
		SELECT sc.decision_type_a,
			count(*),
			COALESCE(avg(sc.significance), 0)
		FROM scored_conflicts sc
		WHERE %s
		GROUP BY sc.decision_type_a
		ORDER BY count(*) DESC
		LIMIT 50`, where)

	typeRows, err := db.pool.Query(ctx, typeQuery, args...)
	if err != nil {
		return result, fmt.Errorf("storage: conflict analytics by decision type: %w", err)
	}
	defer typeRows.Close()

	result.ByDecisionType = []model.DecisionTypeConflictStats{}
	for typeRows.Next() {
		var s model.DecisionTypeConflictStats
		if err := typeRows.Scan(&s.DecisionType, &s.Count, &s.AvgSignificance); err != nil {
			return result, fmt.Errorf("storage: scan conflict analytics decision type: %w", err)
		}
		result.ByDecisionType = append(result.ByDecisionType, s)
	}
	if err := typeRows.Err(); err != nil {
		return result, fmt.Errorf("storage: conflict analytics decision type rows: %w", err)
	}

	// 4. By severity (ordered by rank).
	sevQuery := fmt.Sprintf(`
		SELECT sc.severity, count(*)
		FROM scored_conflicts sc
		WHERE %s AND sc.severity IS NOT NULL
		GROUP BY sc.severity
		ORDER BY CASE sc.severity
			WHEN 'critical' THEN 1
			WHEN 'high'     THEN 2
			WHEN 'medium'   THEN 3
			WHEN 'low'      THEN 4
		END`, where)

	sevRows, err := db.pool.Query(ctx, sevQuery, args...)
	if err != nil {
		return result, fmt.Errorf("storage: conflict analytics by severity: %w", err)
	}
	defer sevRows.Close()

	result.BySeverity = []model.SeverityConflictStats{}
	for sevRows.Next() {
		var s model.SeverityConflictStats
		if err := sevRows.Scan(&s.Severity, &s.Count); err != nil {
			return result, fmt.Errorf("storage: scan conflict analytics severity: %w", err)
		}
		result.BySeverity = append(result.BySeverity, s)
	}
	if err := sevRows.Err(); err != nil {
		return result, fmt.Errorf("storage: conflict analytics severity rows: %w", err)
	}

	// 5. Daily trend: detected and resolved counts per day.
	// Uses generate_series to produce a row for every day in the range,
	// then left-joins detected (by detected_at) and resolved (by resolved_at).
	// The resolved subquery matches on resolved_at within the period,
	// regardless of when the conflict was detected.
	nextArg := 4 + len(extraArgs)
	resolvedWhere := "sc.org_id = $1 AND sc.resolved_at >= $2 AND sc.resolved_at < $3"
	resolvedWhere += extraClause // same agent/type/kind filters apply
	// We need a second copy of from/to for the resolved subquery's generate_series args.
	trendQuery := fmt.Sprintf(`
		WITH days AS (
			SELECT d::date AS date
			FROM generate_series($%d::date, ($%d::date - interval '1 day')::date, '1 day') d
		),
		detected AS (
			SELECT date_trunc('day', sc.detected_at)::date AS date, count(*) AS cnt
			FROM scored_conflicts sc
			WHERE %s
			GROUP BY 1
		),
		resolved AS (
			SELECT date_trunc('day', sc.resolved_at)::date AS date, count(*) AS cnt
			FROM scored_conflicts sc
			WHERE %s
			GROUP BY 1
		)
		SELECT days.date, COALESCE(det.cnt, 0), COALESCE(res.cnt, 0)
		FROM days
		LEFT JOIN detected det ON days.date = det.date
		LEFT JOIN resolved res ON days.date = res.date
		ORDER BY days.date`,
		nextArg, nextArg+1, where, resolvedWhere)

	// Args: base args (orgID, from, to, extraArgs...) + from, to for generate_series.
	trendArgs := append(args, filters.From, filters.To) //nolint:gocritic // intentional append to new slice

	trendRows, err := db.pool.Query(ctx, trendQuery, trendArgs...)
	if err != nil {
		return result, fmt.Errorf("storage: conflict analytics trend: %w", err)
	}
	defer trendRows.Close()

	result.Trend = []model.ConflictTrendPoint{}
	for trendRows.Next() {
		var s model.ConflictTrendPoint
		var d time.Time
		if err := trendRows.Scan(&d, &s.Detected, &s.Resolved); err != nil {
			return result, fmt.Errorf("storage: scan conflict analytics trend: %w", err)
		}
		s.Date = d.Format("2006-01-02")
		result.Trend = append(result.Trend, s)
	}
	if err := trendRows.Err(); err != nil {
		return result, fmt.Errorf("storage: conflict analytics trend rows: %w", err)
	}

	return result, nil
}

// GetGlobalOpenConflictCount returns the total number of open and acknowledged
// conflicts across all organizations. Used by the OpenTelemetry observable
// gauge callback (runs every ~15s).
// SECURITY: Intentionally global — aggregate metric with no tenant data exposed.
func (db *DB) GetGlobalOpenConflictCount(ctx context.Context) (int64, error) {
	var count int64
	err := db.pool.QueryRow(ctx,
		`SELECT count(*) FROM scored_conflicts WHERE status IN ('open', 'acknowledged')`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("storage: global open conflict count: %w", err)
	}
	return count, nil
}

// GetWontFixRate computes the wont_fix false-positive rate for an org over the
// last 30 days: wont_fix / (resolved + wont_fix). Only conflicts with a
// resolved_at timestamp in the window are counted; open/acknowledged conflicts
// are excluded because they haven't been triaged yet.
func (db *DB) GetWontFixRate(ctx context.Context, orgID uuid.UUID) (WontFixRate, error) {
	var r WontFixRate
	err := db.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'resolved'),
			count(*) FILTER (WHERE status = 'wont_fix'),
			COALESCE(
				count(*) FILTER (WHERE status = 'wont_fix')::double precision
				/ NULLIF(count(*) FILTER (WHERE status IN ('resolved', 'wont_fix')), 0),
				0
			)
		FROM scored_conflicts
		WHERE org_id = $1
		  AND status IN ('resolved', 'wont_fix')
		  AND resolved_at >= now() - interval '30 days'`,
		orgID).Scan(&r.Resolved, &r.WontFix, &r.Rate)
	if err != nil {
		return r, fmt.Errorf("storage: wont_fix rate: %w", err)
	}
	return r, nil
}

// GetGlobalWontFixRate computes the global wont_fix false-positive rate across
// all orgs over the last 30 days. Used by the OpenTelemetry observable gauge.
// SECURITY: Intentionally global — aggregate metric with no tenant data exposed.
func (db *DB) GetGlobalWontFixRate(ctx context.Context) (float64, error) {
	var rate float64
	err := db.pool.QueryRow(ctx, `
		SELECT COALESCE(
			count(*) FILTER (WHERE status = 'wont_fix')::double precision
			/ NULLIF(count(*) FILTER (WHERE status IN ('resolved', 'wont_fix')), 0),
			0
		)
		FROM scored_conflicts
		WHERE status IN ('resolved', 'wont_fix')
		  AND resolved_at >= now() - interval '30 days'`).Scan(&rate)
	if err != nil {
		return 0, fmt.Errorf("storage: global wont_fix rate: %w", err)
	}
	return rate, nil
}

// GetConflictGroupKind returns the conflict_kind for a conflict group.
// Used by HTTP handlers that need the kind label for resolution metrics.
func (db *DB) GetConflictGroupKind(ctx context.Context, groupID, orgID uuid.UUID) (string, error) {
	var kind string
	err := db.pool.QueryRow(ctx,
		`SELECT conflict_kind FROM conflict_groups WHERE id = $1 AND org_id = $2`,
		groupID, orgID).Scan(&kind)
	if err != nil {
		return "", fmt.Errorf("storage: get conflict group kind: %w", err)
	}
	return kind, nil
}
