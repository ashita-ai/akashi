package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/akashi/internal/integrity"
	"github.com/ashita-ai/akashi/internal/model"
)

// CreateDecision inserts a decision and queues a search outbox entry if the
// decision has an embedding. Both writes happen atomically in a single transaction.
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

	d.ContentHash = integrity.ComputeContentHash(d.ID, d.DecisionType, d.Outcome, d.Confidence, d.Reasoning, d.ValidFrom)

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return model.Decision{}, fmt.Errorf("storage: begin create decision tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx,
		`INSERT INTO decisions (id, run_id, agent_id, org_id, decision_type, outcome, confidence,
		 reasoning, embedding, metadata, quality_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		d.ID, d.RunID, d.AgentID, d.OrgID, d.DecisionType, d.Outcome, d.Confidence,
		d.Reasoning, d.Embedding, d.Metadata, d.QualityScore, d.PrecedentRef,
		d.SupersedesID, d.ContentHash,
		d.ValidFrom, d.ValidTo, d.TransactionTime, d.CreatedAt,
	)
	if err != nil {
		return model.Decision{}, fmt.Errorf("storage: create decision: %w", err)
	}

	// Queue search index update inside the same transaction.
	if d.Embedding != nil {
		if _, err := tx.Exec(ctx,
			`INSERT INTO search_outbox (decision_id, org_id, operation)
			 VALUES ($1, $2, 'upsert') ON CONFLICT (decision_id, operation) DO NOTHING`,
			d.ID, d.OrgID); err != nil {
			return model.Decision{}, fmt.Errorf("storage: queue search outbox in create decision: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return model.Decision{}, fmt.Errorf("storage: commit create decision: %w", err)
	}
	return d, nil
}

// GetDecisionOpts controls GetDecision behavior.
type GetDecisionOpts struct {
	IncludeAlts     bool // Load alternatives.
	IncludeEvidence bool // Load evidence.
	CurrentOnly     bool // If true, return only if the decision has not been superseded (valid_to IS NULL).
}

// GetDecision retrieves a decision by ID with configurable includes and filtering.
func (db *DB) GetDecision(ctx context.Context, orgID, id uuid.UUID, opts GetDecisionOpts) (model.Decision, error) {
	query := `SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		 metadata, quality_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at
		 FROM decisions WHERE id = $1 AND org_id = $2`
	if opts.CurrentOnly {
		query += ` AND valid_to IS NULL`
	}

	var d model.Decision
	err := db.pool.QueryRow(ctx, query, id, orgID).Scan(
		&d.ID, &d.RunID, &d.AgentID, &d.OrgID, &d.DecisionType, &d.Outcome, &d.Confidence,
		&d.Reasoning, &d.Metadata, &d.QualityScore, &d.PrecedentRef,
		&d.SupersedesID, &d.ContentHash,
		&d.ValidFrom, &d.ValidTo, &d.TransactionTime, &d.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Decision{}, fmt.Errorf("storage: decision not found: %s", id)
		}
		return model.Decision{}, fmt.Errorf("storage: get decision: %w", err)
	}

	if opts.IncludeAlts {
		alts, err := db.GetAlternativesByDecision(ctx, id)
		if err != nil {
			return model.Decision{}, err
		}
		d.Alternatives = alts
	}

	if opts.IncludeEvidence {
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

	// Invalidate original decision, scoped by org_id for tenant isolation.
	tag, err := tx.Exec(ctx,
		`UPDATE decisions SET valid_to = $1 WHERE id = $2 AND org_id = $3 AND valid_to IS NULL`,
		now, originalID, revised.OrgID,
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
	revised.SupersedesID = &originalID
	if revised.Metadata == nil {
		revised.Metadata = map[string]any{}
	}
	revised.ContentHash = integrity.ComputeContentHash(revised.ID, revised.DecisionType, revised.Outcome, revised.Confidence, revised.Reasoning, revised.ValidFrom)

	_, err = tx.Exec(ctx,
		`INSERT INTO decisions (id, run_id, agent_id, org_id, decision_type, outcome, confidence,
		 reasoning, embedding, metadata, quality_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		revised.ID, revised.RunID, revised.AgentID, revised.OrgID, revised.DecisionType, revised.Outcome,
		revised.Confidence, revised.Reasoning, revised.Embedding, revised.Metadata,
		revised.QualityScore, revised.PrecedentRef, revised.SupersedesID, revised.ContentHash,
		revised.ValidFrom, revised.ValidTo, revised.TransactionTime, revised.CreatedAt,
	)
	if err != nil {
		return model.Decision{}, fmt.Errorf("storage: insert revised decision: %w", err)
	}

	// Queue search index updates: delete the old decision, upsert the new one.
	if _, err := tx.Exec(ctx,
		`INSERT INTO search_outbox (decision_id, org_id, operation)
		 VALUES ($1, $2, 'delete') ON CONFLICT (decision_id, operation) DO NOTHING`,
		originalID, revised.OrgID); err != nil {
		return model.Decision{}, fmt.Errorf("storage: queue search outbox delete in revision: %w", err)
	}
	if revised.Embedding != nil {
		if _, err := tx.Exec(ctx,
			`INSERT INTO search_outbox (decision_id, org_id, operation)
			 VALUES ($1, $2, 'upsert') ON CONFLICT (decision_id, operation) DO NOTHING`,
			revised.ID, revised.OrgID); err != nil {
			return model.Decision{}, fmt.Errorf("storage: queue search outbox upsert in revision: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return model.Decision{}, fmt.Errorf("storage: commit revision: %w", err)
	}
	return revised, nil
}

// QueryDecisions executes a structured query with filters, ordering, and pagination.
// Only returns active decisions (valid_to IS NULL). Use QueryDecisionsTemporal for
// point-in-time queries that include superseded decisions.
func (db *DB) QueryDecisions(ctx context.Context, orgID uuid.UUID, req model.QueryRequest) ([]model.Decision, int, error) {
	where, args := buildDecisionWhereClause(orgID, req.Filters, 1)
	where += " AND valid_to IS NULL"

	// Filter by OTEL trace_id via agent_runs join.
	if req.TraceID != nil {
		args = append(args, *req.TraceID)
		where += fmt.Sprintf(" AND run_id IN (SELECT id FROM agent_runs WHERE trace_id = $%d AND org_id = $1)", len(args))
	}

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
		`SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		 metadata, quality_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at
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
func (db *DB) QueryDecisionsTemporal(ctx context.Context, orgID uuid.UUID, req model.TemporalQueryRequest) ([]model.Decision, error) {
	where, args := buildDecisionWhereClause(orgID, req.Filters, 1)

	// Add temporal conditions.
	argIdx := len(args) + 1
	where += fmt.Sprintf(
		" AND transaction_time <= $%d AND (valid_to IS NULL OR valid_to > $%d)",
		argIdx, argIdx+1,
	)
	args = append(args, req.AsOf, req.AsOf)

	// Enforce a result cap to prevent unbounded memory allocation.
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	argIdx = len(args) + 1
	limitClause := fmt.Sprintf(" LIMIT $%d", argIdx)
	args = append(args, limit)

	query := `SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		 metadata, quality_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at
		 FROM decisions` + where + ` ORDER BY valid_from DESC` + limitClause

	rows, err := db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: temporal query: %w", err)
	}
	defer rows.Close()

	return scanDecisions(rows)
}

// SearchDecisionsByText performs keyword search over decision outcome and reasoning fields.
// Used as fallback when semantic search is disabled or the embedding provider is noop.
// Only returns active decisions (valid_to IS NULL) for consistency with the Qdrant search path.
func (db *DB) SearchDecisionsByText(ctx context.Context, orgID uuid.UUID, query string, filters model.QueryFilters, limit int) ([]model.SearchResult, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 1000 {
		limit = 1000
	}

	where, args := buildDecisionWhereClause(orgID, filters, 1)
	where += " AND valid_to IS NULL"

	// Use ILIKE for simple keyword matching across outcome, reasoning, and decision_type.
	// The query is split into words (capped at 20 to prevent query explosion),
	// and all must match at least one field. LIKE metacharacters are escaped.
	words := strings.Fields(query)
	if len(words) > 20 {
		words = words[:20]
	}
	for _, word := range words {
		escaped := strings.NewReplacer("%", `\%`, "_", `\_`).Replace(strings.ToLower(word))
		args = append(args, "%"+escaped+"%")
		paramIdx := len(args)
		where += fmt.Sprintf(` AND (LOWER(outcome) LIKE $%d OR LOWER(COALESCE(reasoning, '')) LIKE $%d OR LOWER(decision_type) LIKE $%d)`,
			paramIdx, paramIdx, paramIdx)
	}

	sql := fmt.Sprintf(
		`SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		 metadata, quality_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at,
		 (0.6 + 0.3 * COALESCE(quality_score, 0))
		   * (1.0 / (1.0 + EXTRACT(EPOCH FROM (NOW() - valid_from)) / 86400.0 / 90.0))
		   AS relevance
		 FROM decisions%s
		 ORDER BY relevance DESC
		 LIMIT %d`, where, limit,
	)

	rows, err := db.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: text search decisions: %w", err)
	}
	defer rows.Close()

	var results []model.SearchResult
	for rows.Next() {
		var d model.Decision
		var relevance float32
		if err := rows.Scan(
			&d.ID, &d.RunID, &d.AgentID, &d.OrgID, &d.DecisionType, &d.Outcome, &d.Confidence,
			&d.Reasoning, &d.Metadata, &d.QualityScore, &d.PrecedentRef,
			&d.SupersedesID, &d.ContentHash,
			&d.ValidFrom, &d.ValidTo, &d.TransactionTime, &d.CreatedAt,
			&relevance,
		); err != nil {
			return nil, fmt.Errorf("storage: scan text search result: %w", err)
		}
		results = append(results, model.SearchResult{Decision: d, SimilarityScore: relevance})
	}
	return results, rows.Err()
}

// GetDecisionsByAgent returns active decisions for a given agent within an org with pagination.
// Only returns decisions with valid_to IS NULL (not revised/invalidated).
func (db *DB) GetDecisionsByAgent(ctx context.Context, orgID uuid.UUID, agentID string, limit, offset int, from, to *time.Time) ([]model.Decision, int, error) {
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

	where, args := buildDecisionWhereClause(orgID, filters, 1)
	where += " AND valid_to IS NULL"

	var total int
	if err := db.pool.QueryRow(ctx, "SELECT COUNT(*) FROM decisions"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("storage: count agent decisions: %w", err)
	}

	query := fmt.Sprintf(
		`SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		 metadata, quality_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at
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

func buildDecisionWhereClause(orgID uuid.UUID, f model.QueryFilters, startArgIdx int) (string, []any) {
	var conditions []string
	var args []any
	idx := startArgIdx

	// Org isolation is always the first condition.
	conditions = append(conditions, fmt.Sprintf("org_id = $%d", idx))
	args = append(args, orgID)
	idx++

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
			idx++ //nolint:ineffassign // keep idx consistent so future additions don't miscount
		}
	}

	return " WHERE " + strings.Join(conditions, " AND "), args
}

func scanDecisions(rows pgx.Rows) ([]model.Decision, error) {
	var decisions []model.Decision
	for rows.Next() {
		var d model.Decision
		if err := rows.Scan(
			&d.ID, &d.RunID, &d.AgentID, &d.OrgID, &d.DecisionType, &d.Outcome, &d.Confidence,
			&d.Reasoning, &d.Metadata, &d.QualityScore, &d.PrecedentRef,
			&d.SupersedesID, &d.ContentHash,
			&d.ValidFrom, &d.ValidTo, &d.TransactionTime, &d.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan decision: %w", err)
		}
		decisions = append(decisions, d)
	}
	return decisions, rows.Err()
}

// GetDecisionsByIDs returns active decisions for the given IDs within an org.
// Only returns decisions with valid_to IS NULL (not revised/invalidated).
// Used to hydrate search results from Qdrant back to full Decision objects.
func (db *DB) GetDecisionsByIDs(ctx context.Context, orgID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]model.Decision, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	rows, err := db.pool.Query(ctx,
		`SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		 metadata, quality_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at
		 FROM decisions
		 WHERE org_id = $1 AND id = ANY($2) AND valid_to IS NULL`,
		orgID, ids,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get decisions by IDs: %w", err)
	}
	defer rows.Close()

	decisions, err := scanDecisions(rows)
	if err != nil {
		return nil, err
	}

	result := make(map[uuid.UUID]model.Decision, len(decisions))
	for _, d := range decisions {
		result[d.ID] = d
	}
	return result, nil
}

// GetDecisionRevisions returns the full revision chain for a decision, walking
// both backwards (via supersedes_id) and forwards (via decisions that reference
// this one's id as their supersedes_id). Results are ordered by valid_from ASC.
func (db *DB) GetDecisionRevisions(ctx context.Context, orgID, id uuid.UUID) ([]model.Decision, error) {
	query := `
	WITH RECURSIVE chain AS (
		-- Anchor: the requested decision.
		SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		       metadata, quality_score, precedent_ref, supersedes_id, content_hash,
		       valid_from, valid_to, transaction_time, created_at, 0 AS depth
		FROM decisions
		WHERE id = $1 AND org_id = $2

		UNION

		-- Walk backwards: find what this decision supersedes.
		SELECT d.id, d.run_id, d.agent_id, d.org_id, d.decision_type, d.outcome, d.confidence, d.reasoning,
		       d.metadata, d.quality_score, d.precedent_ref, d.supersedes_id, d.content_hash,
		       d.valid_from, d.valid_to, d.transaction_time, d.created_at, c.depth + 1
		FROM decisions d
		INNER JOIN chain c ON d.id = c.supersedes_id
		WHERE d.org_id = $2 AND c.depth < 100

		UNION

		-- Walk forwards: find decisions that supersede this one.
		SELECT d.id, d.run_id, d.agent_id, d.org_id, d.decision_type, d.outcome, d.confidence, d.reasoning,
		       d.metadata, d.quality_score, d.precedent_ref, d.supersedes_id, d.content_hash,
		       d.valid_from, d.valid_to, d.transaction_time, d.created_at, c.depth + 1
		FROM decisions d
		INNER JOIN chain c ON d.supersedes_id = c.id
		WHERE d.org_id = $2 AND c.depth < 100
	)
	SELECT DISTINCT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
	       metadata, quality_score, precedent_ref, supersedes_id, content_hash,
	       valid_from, valid_to, transaction_time, created_at
	FROM chain
	ORDER BY valid_from ASC`

	rows, err := db.pool.Query(ctx, query, id, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: get decision revisions: %w", err)
	}
	defer rows.Close()

	return scanDecisions(rows)
}

func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
