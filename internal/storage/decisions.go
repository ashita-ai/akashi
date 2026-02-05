package storage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/ashita-ai/akashi/internal/model"
)

// CreateDecision inserts a decision and returns it.
func (db *DB) CreateDecision(ctx context.Context, d model.Decision) (model.Decision, error) {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	now := time.Now().UTC()
	if d.ValidFrom.IsZero() {
		d.ValidFrom = now
	}
	if d.TransactionTime.IsZero() {
		d.TransactionTime = now
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	if d.Metadata == nil {
		d.Metadata = map[string]any{}
	}

	_, err := db.pool.Exec(ctx,
		`INSERT INTO decisions (id, run_id, agent_id, decision_type, outcome, confidence,
		 reasoning, embedding, metadata, quality_score, precedent_ref, valid_from, valid_to, transaction_time, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		d.ID, d.RunID, d.AgentID, d.DecisionType, d.Outcome, d.Confidence,
		d.Reasoning, d.Embedding, d.Metadata, d.QualityScore, d.PrecedentRef,
		d.ValidFrom, d.ValidTo, d.TransactionTime, d.CreatedAt,
	)
	if err != nil {
		return model.Decision{}, fmt.Errorf("storage: create decision: %w", err)
	}
	return d, nil
}

// GetDecision retrieves a decision by ID, optionally with alternatives and evidence.
func (db *DB) GetDecision(ctx context.Context, id uuid.UUID, includeAlts, includeEvidence bool) (model.Decision, error) {
	var d model.Decision
	err := db.pool.QueryRow(ctx,
		`SELECT id, run_id, agent_id, decision_type, outcome, confidence, reasoning,
		 metadata, quality_score, precedent_ref, valid_from, valid_to, transaction_time, created_at
		 FROM decisions WHERE id = $1`, id,
	).Scan(
		&d.ID, &d.RunID, &d.AgentID, &d.DecisionType, &d.Outcome, &d.Confidence,
		&d.Reasoning, &d.Metadata, &d.QualityScore, &d.PrecedentRef,
		&d.ValidFrom, &d.ValidTo, &d.TransactionTime, &d.CreatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.Decision{}, fmt.Errorf("storage: decision not found: %s", id)
		}
		return model.Decision{}, fmt.Errorf("storage: get decision: %w", err)
	}

	if includeAlts {
		alts, err := db.GetAlternativesByDecision(ctx, id)
		if err != nil {
			return model.Decision{}, err
		}
		d.Alternatives = alts
	}

	if includeEvidence {
		ev, err := db.GetEvidenceByDecision(ctx, id)
		if err != nil {
			return model.Decision{}, err
		}
		d.Evidence = ev
	}

	return d, nil
}

// ReviseDecision invalidates an existing decision by setting valid_to
// and creates a new decision with the revised data.
func (db *DB) ReviseDecision(ctx context.Context, originalID uuid.UUID, revised model.Decision) (model.Decision, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return model.Decision{}, fmt.Errorf("storage: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := time.Now().UTC()

	// Invalidate original decision.
	tag, err := tx.Exec(ctx,
		`UPDATE decisions SET valid_to = $1 WHERE id = $2 AND valid_to IS NULL`,
		now, originalID,
	)
	if err != nil {
		return model.Decision{}, fmt.Errorf("storage: invalidate decision: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.Decision{}, fmt.Errorf("storage: original decision not found or already revised: %s", originalID)
	}

	// Insert revised decision.
	revised.ID = uuid.New()
	revised.ValidFrom = now
	revised.TransactionTime = now
	revised.CreatedAt = now
	if revised.Metadata == nil {
		revised.Metadata = map[string]any{}
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO decisions (id, run_id, agent_id, decision_type, outcome, confidence,
		 reasoning, embedding, metadata, quality_score, precedent_ref, valid_from, valid_to, transaction_time, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		revised.ID, revised.RunID, revised.AgentID, revised.DecisionType, revised.Outcome,
		revised.Confidence, revised.Reasoning, revised.Embedding, revised.Metadata,
		revised.QualityScore, revised.PrecedentRef,
		revised.ValidFrom, revised.ValidTo, revised.TransactionTime, revised.CreatedAt,
	)
	if err != nil {
		return model.Decision{}, fmt.Errorf("storage: insert revised decision: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.Decision{}, fmt.Errorf("storage: commit revision: %w", err)
	}
	return revised, nil
}

// QueryDecisions executes a structured query with filters, ordering, and pagination.
func (db *DB) QueryDecisions(ctx context.Context, req model.QueryRequest) ([]model.Decision, int, error) {
	where, args := buildDecisionWhereClause(req.Filters, 1)

	// Count total matching decisions.
	countQuery := "SELECT COUNT(*) FROM decisions" + where
	var total int
	if err := db.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("storage: count decisions: %w", err)
	}

	// Build order clause.
	orderBy := "valid_from"
	if req.OrderBy != "" {
		switch req.OrderBy {
		case "confidence", "valid_from", "decision_type", "outcome", "quality_score":
			orderBy = req.OrderBy
		}
	}
	orderDir := "DESC"
	if strings.EqualFold(req.OrderDir, "asc") {
		orderDir = "ASC"
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	offset := req.Offset
	if offset < 0 {
		offset = 0
	}

	selectQuery := fmt.Sprintf(
		`SELECT id, run_id, agent_id, decision_type, outcome, confidence, reasoning,
		 metadata, quality_score, precedent_ref, valid_from, valid_to, transaction_time, created_at
		 FROM decisions%s ORDER BY %s %s LIMIT %d OFFSET %d`,
		where, orderBy, orderDir, limit, offset,
	)

	rows, err := db.pool.Query(ctx, selectQuery, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("storage: query decisions: %w", err)
	}
	defer rows.Close()

	decisions, err := scanDecisions(rows)
	if err != nil {
		return nil, 0, err
	}

	// Optionally load related data in batch (avoids N+1 queries).
	includeAlts := containsStr(req.Include, "alternatives")
	includeEvidence := containsStr(req.Include, "evidence")
	if (includeAlts || includeEvidence) && len(decisions) > 0 {
		ids := make([]uuid.UUID, len(decisions))
		for i := range decisions {
			ids[i] = decisions[i].ID
		}

		if includeAlts {
			altsMap, err := db.GetAlternativesByDecisions(ctx, ids)
			if err != nil {
				return nil, 0, err
			}
			for i := range decisions {
				decisions[i].Alternatives = altsMap[decisions[i].ID]
			}
		}
		if includeEvidence {
			evsMap, err := db.GetEvidenceByDecisions(ctx, ids)
			if err != nil {
				return nil, 0, err
			}
			for i := range decisions {
				decisions[i].Evidence = evsMap[decisions[i].ID]
			}
		}
	}

	return decisions, total, nil
}

// QueryDecisionsTemporal executes a bi-temporal point-in-time query.
func (db *DB) QueryDecisionsTemporal(ctx context.Context, req model.TemporalQueryRequest) ([]model.Decision, error) {
	where, args := buildDecisionWhereClause(req.Filters, 1)

	// Add temporal conditions.
	argIdx := len(args) + 1
	where += fmt.Sprintf(
		" AND transaction_time <= $%d AND (valid_to IS NULL OR valid_to > $%d)",
		argIdx, argIdx+1,
	)
	args = append(args, req.AsOf, req.AsOf)

	query := `SELECT id, run_id, agent_id, decision_type, outcome, confidence, reasoning,
		 metadata, quality_score, precedent_ref, valid_from, valid_to, transaction_time, created_at
		 FROM decisions` + where + ` ORDER BY valid_from DESC`

	rows, err := db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: temporal query: %w", err)
	}
	defer rows.Close()

	return scanDecisions(rows)
}

// SearchDecisionsByEmbedding performs semantic similarity search over decisions using pgvector.
func (db *DB) SearchDecisionsByEmbedding(ctx context.Context, embedding pgvector.Vector, filters model.QueryFilters, limit int) ([]model.SearchResult, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 1000 {
		limit = 1000
	}

	where, args := buildDecisionWhereClause(filters, 2)
	if where == "" {
		where = " WHERE embedding IS NOT NULL"
	} else {
		where += " AND embedding IS NOT NULL"
	}

	// Quality-weighted relevance with temporal decay:
	// - Semantic similarity: 60% base weight
	// - Quality score: up to 30% bonus
	// - Recency: decays to 50% at 90 days
	query := fmt.Sprintf(
		`SELECT id, run_id, agent_id, decision_type, outcome, confidence, reasoning,
		 metadata, quality_score, precedent_ref, valid_from, valid_to, transaction_time, created_at,
		 (1 - (embedding <=> $1))
		   * (0.6 + 0.3 * COALESCE(quality_score, 0))
		   * (1.0 / (1.0 + EXTRACT(EPOCH FROM (NOW() - valid_from)) / 86400.0 / 90.0))
		   AS relevance
		 FROM decisions%s
		 ORDER BY relevance DESC
		 LIMIT %d`, where, limit,
	)

	allArgs := append([]any{embedding}, args...)
	rows, err := db.pool.Query(ctx, query, allArgs...)
	if err != nil {
		return nil, fmt.Errorf("storage: search decisions: %w", err)
	}
	defer rows.Close()

	var results []model.SearchResult
	for rows.Next() {
		var d model.Decision
		var relevance float32
		if err := rows.Scan(
			&d.ID, &d.RunID, &d.AgentID, &d.DecisionType, &d.Outcome, &d.Confidence,
			&d.Reasoning, &d.Metadata, &d.QualityScore, &d.PrecedentRef,
			&d.ValidFrom, &d.ValidTo, &d.TransactionTime, &d.CreatedAt,
			&relevance,
		); err != nil {
			return nil, fmt.Errorf("storage: scan search result: %w", err)
		}
		results = append(results, model.SearchResult{Decision: d, SimilarityScore: relevance})
	}
	return results, rows.Err()
}

// GetDecisionsByAgent returns decisions for a given agent with pagination.
func (db *DB) GetDecisionsByAgent(ctx context.Context, agentID string, limit, offset int, from, to *time.Time) ([]model.Decision, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}

	filters := model.QueryFilters{
		AgentIDs: []string{agentID},
	}
	if from != nil || to != nil {
		filters.TimeRange = &model.TimeRange{From: from, To: to}
	}

	where, args := buildDecisionWhereClause(filters, 1)

	var total int
	if err := db.pool.QueryRow(ctx, "SELECT COUNT(*) FROM decisions"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("storage: count agent decisions: %w", err)
	}

	query := fmt.Sprintf(
		`SELECT id, run_id, agent_id, decision_type, outcome, confidence, reasoning,
		 metadata, quality_score, precedent_ref, valid_from, valid_to, transaction_time, created_at
		 FROM decisions%s ORDER BY valid_from DESC LIMIT %d OFFSET %d`,
		where, limit, offset,
	)

	rows, err := db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("storage: get agent decisions: %w", err)
	}
	defer rows.Close()

	decisions, err := scanDecisions(rows)
	return decisions, total, err
}

func buildDecisionWhereClause(f model.QueryFilters, startArgIdx int) (string, []any) {
	var conditions []string
	var args []any
	idx := startArgIdx

	if len(f.AgentIDs) > 0 {
		conditions = append(conditions, fmt.Sprintf("agent_id = ANY($%d)", idx))
		args = append(args, f.AgentIDs)
		idx++
	}
	if f.RunID != nil {
		conditions = append(conditions, fmt.Sprintf("run_id = $%d", idx))
		args = append(args, *f.RunID)
		idx++
	}
	if f.DecisionType != nil {
		conditions = append(conditions, fmt.Sprintf("decision_type = $%d", idx))
		args = append(args, *f.DecisionType)
		idx++
	}
	if f.ConfidenceMin != nil {
		conditions = append(conditions, fmt.Sprintf("confidence >= $%d", idx))
		args = append(args, *f.ConfidenceMin)
		idx++
	}
	if f.Outcome != nil {
		conditions = append(conditions, fmt.Sprintf("outcome = $%d", idx))
		args = append(args, *f.Outcome)
		idx++
	}
	if f.TimeRange != nil {
		if f.TimeRange.From != nil {
			conditions = append(conditions, fmt.Sprintf("valid_from >= $%d", idx))
			args = append(args, *f.TimeRange.From)
			idx++
		}
		if f.TimeRange.To != nil {
			conditions = append(conditions, fmt.Sprintf("valid_from <= $%d", idx))
			args = append(args, *f.TimeRange.To)
		}
	}

	if len(conditions) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

func scanDecisions(rows pgx.Rows) ([]model.Decision, error) {
	var decisions []model.Decision
	for rows.Next() {
		var d model.Decision
		if err := rows.Scan(
			&d.ID, &d.RunID, &d.AgentID, &d.DecisionType, &d.Outcome, &d.Confidence,
			&d.Reasoning, &d.Metadata, &d.QualityScore, &d.PrecedentRef,
			&d.ValidFrom, &d.ValidTo, &d.TransactionTime, &d.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan decision: %w", err)
		}
		decisions = append(decisions, d)
	}
	return decisions, rows.Err()
}

func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
