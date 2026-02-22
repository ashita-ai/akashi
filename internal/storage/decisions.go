package storage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/ashita-ai/akashi/internal/integrity"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/search"
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

	if d.AgentContext == nil {
		d.AgentContext = map[string]any{}
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO decisions (id, run_id, agent_id, org_id, decision_type, outcome, confidence,
		 reasoning, embedding, outcome_embedding, metadata, completeness_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)`,
		d.ID, d.RunID, d.AgentID, d.OrgID, d.DecisionType, d.Outcome, d.Confidence,
		d.Reasoning, d.Embedding, d.OutcomeEmbedding, d.Metadata, d.CompletenessScore, d.PrecedentRef,
		d.SupersedesID, d.ContentHash,
		d.ValidFrom, d.ValidTo, d.TransactionTime, d.CreatedAt,
		d.SessionID, d.AgentContext, d.APIKeyID,
	)
	if err != nil {
		return model.Decision{}, fmt.Errorf("storage: create decision: %w", err)
	}

	// Queue search index update inside the same transaction.
	if d.Embedding != nil {
		if _, err := tx.Exec(ctx,
			`INSERT INTO search_outbox (decision_id, org_id, operation)
			 VALUES ($1, $2, 'upsert')
			 ON CONFLICT (decision_id, operation) DO UPDATE SET created_at = now(), attempts = 0, locked_until = NULL`,
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
		 metadata, completeness_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, repo
		 FROM decisions WHERE id = $1 AND org_id = $2`
	if opts.CurrentOnly {
		query += ` AND valid_to IS NULL`
	}

	var d model.Decision
	err := db.pool.QueryRow(ctx, query, id, orgID).Scan(
		&d.ID, &d.RunID, &d.AgentID, &d.OrgID, &d.DecisionType, &d.Outcome, &d.Confidence,
		&d.Reasoning, &d.Metadata, &d.CompletenessScore, &d.PrecedentRef,
		&d.SupersedesID, &d.ContentHash,
		&d.ValidFrom, &d.ValidTo, &d.TransactionTime, &d.CreatedAt,
		&d.SessionID, &d.AgentContext, &d.APIKeyID,
		&d.Tool, &d.Model, &d.Repo,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Decision{}, fmt.Errorf("storage: decision %s: %w", id, ErrNotFound)
		}
		return model.Decision{}, fmt.Errorf("storage: get decision: %w", err)
	}

	if opts.IncludeAlts {
		alts, err := db.GetAlternativesByDecision(ctx, id, orgID)
		if err != nil {
			return model.Decision{}, err
		}
		d.Alternatives = alts
	}

	if opts.IncludeEvidence {
		ev, err := db.GetEvidenceByDecision(ctx, id, orgID)
		if err != nil {
			return model.Decision{}, err
		}
		d.Evidence = ev
	}

	return d, nil
}

// ReviseDecision invalidates an existing decision by setting valid_to
// and creates a new decision with the revised data. When audit is non-nil,
// a mutation audit entry recording the revision is inserted in the same transaction.
func (db *DB) ReviseDecision(ctx context.Context, originalID uuid.UUID, revised model.Decision, audit *MutationAuditEntry) (model.Decision, error) {
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
		return model.Decision{}, fmt.Errorf("storage: original decision %s (or already revised): %w", originalID, ErrNotFound)
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
	if revised.AgentContext == nil {
		revised.AgentContext = map[string]any{}
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO decisions (id, run_id, agent_id, org_id, decision_type, outcome, confidence,
		 reasoning, embedding, outcome_embedding, metadata, completeness_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)`,
		revised.ID, revised.RunID, revised.AgentID, revised.OrgID, revised.DecisionType, revised.Outcome,
		revised.Confidence, revised.Reasoning, revised.Embedding, revised.OutcomeEmbedding, revised.Metadata,
		revised.CompletenessScore, revised.PrecedentRef, revised.SupersedesID, revised.ContentHash,
		revised.ValidFrom, revised.ValidTo, revised.TransactionTime, revised.CreatedAt,
		revised.SessionID, revised.AgentContext, revised.APIKeyID,
	)
	if err != nil {
		return model.Decision{}, fmt.Errorf("storage: insert revised decision: %w", err)
	}

	// Queue search index updates: delete the old decision, upsert the new one.
	if _, err := tx.Exec(ctx,
		`INSERT INTO search_outbox (decision_id, org_id, operation)
		 VALUES ($1, $2, 'delete')
		 ON CONFLICT (decision_id, operation) DO UPDATE SET created_at = now(), attempts = 0, locked_until = NULL`,
		originalID, revised.OrgID); err != nil {
		return model.Decision{}, fmt.Errorf("storage: queue search outbox delete in revision: %w", err)
	}
	if revised.Embedding != nil {
		if _, err := tx.Exec(ctx,
			`INSERT INTO search_outbox (decision_id, org_id, operation)
			 VALUES ($1, $2, 'upsert')
			 ON CONFLICT (decision_id, operation) DO UPDATE SET created_at = now(), attempts = 0, locked_until = NULL`,
			revised.ID, revised.OrgID); err != nil {
			return model.Decision{}, fmt.Errorf("storage: queue search outbox upsert in revision: %w", err)
		}
	}

	// Insert revision audit entry (same tx — atomic with the revision).
	if audit != nil {
		audit.Operation = "decision_revised"
		audit.ResourceType = "decision"
		audit.ResourceID = originalID.String()
		audit.BeforeData = map[string]any{"valid_to": nil}
		audit.AfterData = map[string]any{
			"superseded_by": revised.ID.String(),
			"valid_to":      now,
		}
		if err := InsertMutationAuditTx(ctx, tx, *audit); err != nil {
			return model.Decision{}, fmt.Errorf("storage: audit in revision tx: %w", err)
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
	where, args := buildDecisionWhereClause(orgID, req.Filters, 1, true)

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
		case "confidence", "valid_from", "decision_type", "outcome", "completeness_score", "quality_score":
			// quality_score accepted as deprecated alias; maps to the renamed column.
			if req.OrderBy == "quality_score" {
				orderBy = "completeness_score"
			} else {
				orderBy = req.OrderBy
			}
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
		 metadata, completeness_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, repo
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
			altsMap, err := db.GetAlternativesByDecisions(ctx, ids, orgID)
			if err != nil {
				return nil, 0, err
			}
			for i := range decisions {
				decisions[i].Alternatives = altsMap[decisions[i].ID]
			}
		}
		if includeEvidence {
			evsMap, err := db.GetEvidenceByDecisions(ctx, ids, orgID)
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
	where, args := buildDecisionWhereClause(orgID, req.Filters, 1, false)

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
		 metadata, completeness_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, repo
		 FROM decisions` + where + ` ORDER BY valid_from DESC` + limitClause

	rows, err := db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: temporal query: %w", err)
	}
	defer rows.Close()

	return scanDecisions(rows)
}

// SearchDecisionsByText performs full-text search over decision outcome, reasoning,
// and decision_type using PostgreSQL tsvector/tsquery (migration 022).
// Used as fallback when semantic search is disabled or the embedding provider is noop.
// Only returns active decisions (valid_to IS NULL) for consistency with the Qdrant search path.
//
// Search strategy:
//  1. websearch_to_tsquery: handles stemming, stop word removal, and supports
//     "quoted phrases", OR, and -exclusion. Most queries resolve here.
//  2. ILIKE fallback (OR-any-term): if FTS returns nothing (e.g. all stop words,
//     partial words, or terms absent from the English dictionary), try lenient
//     substring matching where any single term hitting any field is a match.
func (db *DB) SearchDecisionsByText(ctx context.Context, orgID uuid.UUID, query string, filters model.QueryFilters, limit int) ([]model.SearchResult, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 1000 {
		limit = 1000
	}

	// Primary: PostgreSQL full-text search with ts_rank.
	// On FTS failure (e.g. websearch_to_tsquery parse error from malformed query),
	// fall back to ILIKE instead of returning 500.
	results, err := db.searchByFTS(ctx, orgID, query, filters, limit)
	if err != nil {
		return db.searchByILIKE(ctx, orgID, query, filters, limit)
	}
	if len(results) > 0 {
		return results, nil
	}

	// Fallback: OR-based ILIKE for cases FTS misses (typos, partial words,
	// all stop words, non-English terms).
	return db.searchByILIKE(ctx, orgID, query, filters, limit)
}

// HasDecisionsWithNullSearchVector returns true if any active decision has
// search_vector IS NULL (e.g. from a dropped trigger or incomplete backfill).
// Used for monitoring; FTS excludes such rows from results.
// SECURITY: Intentionally global — system health monitoring check, not
// user-facing. Result is a boolean logged by the maintenance goroutine.
func (db *DB) HasDecisionsWithNullSearchVector(ctx context.Context) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM decisions
			WHERE search_vector IS NULL AND valid_to IS NULL
		)`,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("storage: check null search_vector: %w", err)
	}
	return exists, nil
}

// searchByFTS uses PostgreSQL websearch_to_tsquery for full-text search with
// stemming, stop word removal, and weighted ranking (outcome > type > reasoning).
func (db *DB) searchByFTS(ctx context.Context, orgID uuid.UUID, query string, filters model.QueryFilters, limit int) ([]model.SearchResult, error) {
	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	args = append(args, query)
	qp := len(args)
	where += fmt.Sprintf(` AND search_vector @@ websearch_to_tsquery('english', $%d)`, qp)

	sql := fmt.Sprintf(
		`SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		 metadata, completeness_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at, session_id, agent_context,
		 ts_rank(search_vector, websearch_to_tsquery('english', $%d))
		   * (0.6 + 0.3 * COALESCE(completeness_score, 0))
		   * (1.0 / (1.0 + EXTRACT(EPOCH FROM (NOW() - valid_from)) / 86400.0 / 90.0))
		   AS relevance
		 FROM decisions%s
		 ORDER BY relevance DESC
		 LIMIT %d`, qp, where, limit,
	)

	return db.execSearchQuery(ctx, sql, args)
}

// searchByILIKE uses OR-any-term ILIKE matching as a fallback when FTS returns nothing.
// A result matches if any single query term appears in any searchable field.
func (db *DB) searchByILIKE(ctx context.Context, orgID uuid.UUID, query string, filters model.QueryFilters, limit int) ([]model.SearchResult, error) {
	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	words := strings.Fields(query)
	if len(words) > 20 {
		words = words[:20]
	}
	if len(words) == 0 {
		return nil, nil
	}

	// OR across all terms: any word matching any field qualifies the row.
	// Uses ILIKE instead of LOWER()+LIKE so PostgreSQL can use pg_trgm GIN indexes.
	var termClauses []string
	for _, word := range words {
		escaped := strings.NewReplacer("%", `\%`, "_", `\_`).Replace(word)
		args = append(args, "%"+escaped+"%")
		p := len(args)
		termClauses = append(termClauses, fmt.Sprintf(
			`(outcome ILIKE $%d OR COALESCE(reasoning, '') ILIKE $%d OR decision_type ILIKE $%d)`,
			p, p, p))
	}
	where += " AND (" + strings.Join(termClauses, " OR ") + ")"

	sql := fmt.Sprintf(
		`SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		 metadata, completeness_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at, session_id, agent_context,
		 (0.6 + 0.3 * COALESCE(completeness_score, 0))
		   * (1.0 / (1.0 + EXTRACT(EPOCH FROM (NOW() - valid_from)) / 86400.0 / 90.0))
		   AS relevance
		 FROM decisions%s
		 ORDER BY relevance DESC
		 LIMIT %d`, where, limit,
	)

	return db.execSearchQuery(ctx, sql, args)
}

// execSearchQuery runs a search SQL and scans results into SearchResult structs.
func (db *DB) execSearchQuery(ctx context.Context, sql string, args []any) ([]model.SearchResult, error) {
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
			&d.Reasoning, &d.Metadata, &d.CompletenessScore, &d.PrecedentRef,
			&d.SupersedesID, &d.ContentHash,
			&d.ValidFrom, &d.ValidTo, &d.TransactionTime, &d.CreatedAt,
			&d.SessionID, &d.AgentContext,
			&relevance,
		); err != nil {
			return nil, fmt.Errorf("storage: scan text search result: %w", err)
		}
		d.QualityScore = d.CompletenessScore //nolint:staticcheck // deprecated alias emitted for one release cycle
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
	if offset < 0 {
		offset = 0
	}

	filters := model.QueryFilters{
		AgentIDs: []string{agentID},
	}
	if from != nil || to != nil {
		filters.TimeRange = &model.TimeRange{From: from, To: to}
	}

	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	var total int
	if err := db.pool.QueryRow(ctx, "SELECT COUNT(*) FROM decisions"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("storage: count agent decisions: %w", err)
	}

	query := fmt.Sprintf(
		`SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		 metadata, completeness_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, repo
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

func buildDecisionWhereClause(orgID uuid.UUID, f model.QueryFilters, startArgIdx int, currentOnly bool) (string, []any) {
	var conditions []string
	var args []any
	idx := startArgIdx

	// Org isolation is always the first condition.
	conditions = append(conditions, fmt.Sprintf("org_id = $%d", idx))
	args = append(args, orgID)
	idx++

	if currentOnly {
		conditions = append(conditions, "valid_to IS NULL")
	}

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
			idx++
		}
	}
	if f.SessionID != nil {
		conditions = append(conditions, fmt.Sprintf("session_id = $%d", idx))
		args = append(args, *f.SessionID)
		idx++
	}
	if f.Tool != nil {
		conditions = append(conditions, fmt.Sprintf("tool = $%d", idx))
		args = append(args, *f.Tool)
		idx++
	}
	if f.Model != nil {
		conditions = append(conditions, fmt.Sprintf("model = $%d", idx))
		args = append(args, *f.Model)
		idx++
	}
	if f.Repo != nil {
		conditions = append(conditions, fmt.Sprintf("repo = $%d", idx))
		args = append(args, *f.Repo)
		idx++ //nolint:ineffassign // keep idx consistent so future additions don't miscount
	}

	return " WHERE " + strings.Join(conditions, " AND "), args
}

// ExportDecisionsCursor returns a page of decisions using keyset pagination on
// (valid_from, id). This avoids the O(offset) scan cost of OFFSET-based pagination,
// making it suitable for streaming large exports. Pass a nil cursor for the first page.
func (db *DB) ExportDecisionsCursor(ctx context.Context, orgID uuid.UUID, filters model.QueryFilters, cursor *ExportCursor, limit int) ([]model.Decision, error) {
	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	if cursor != nil {
		idx := len(args) + 1
		where += fmt.Sprintf(" AND (valid_from, id) > ($%d, $%d)", idx, idx+1)
		args = append(args, cursor.ValidFrom, cursor.ID)
	}

	query := fmt.Sprintf(
		`SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		 metadata, completeness_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, repo
		 FROM decisions%s ORDER BY valid_from ASC, id ASC LIMIT %d`,
		where, limit,
	)

	rows, err := db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: export decisions cursor: %w", err)
	}
	defer rows.Close()

	decisions, err := scanDecisions(rows)
	if err != nil {
		return nil, err
	}

	// Batch-load alternatives and evidence.
	if len(decisions) > 0 {
		ids := make([]uuid.UUID, len(decisions))
		for i := range decisions {
			ids[i] = decisions[i].ID
		}

		altsMap, err := db.GetAlternativesByDecisions(ctx, ids, orgID)
		if err != nil {
			return nil, err
		}
		evsMap, err := db.GetEvidenceByDecisions(ctx, ids, orgID)
		if err != nil {
			return nil, err
		}
		for i := range decisions {
			decisions[i].Alternatives = altsMap[decisions[i].ID]
			decisions[i].Evidence = evsMap[decisions[i].ID]
		}
	}

	return decisions, nil
}

// ExportCursor holds the keyset cursor position for cursor-based export pagination.
type ExportCursor struct {
	ValidFrom time.Time
	ID        uuid.UUID
}

func scanDecisions(rows pgx.Rows) ([]model.Decision, error) {
	var decisions []model.Decision
	for rows.Next() {
		var d model.Decision
		if err := rows.Scan(
			&d.ID, &d.RunID, &d.AgentID, &d.OrgID, &d.DecisionType, &d.Outcome, &d.Confidence,
			&d.Reasoning, &d.Metadata, &d.CompletenessScore, &d.PrecedentRef,
			&d.SupersedesID, &d.ContentHash,
			&d.ValidFrom, &d.ValidTo, &d.TransactionTime, &d.CreatedAt,
			&d.SessionID, &d.AgentContext, &d.APIKeyID,
			&d.Tool, &d.Model, &d.Repo,
		); err != nil {
			return nil, fmt.Errorf("storage: scan decision: %w", err)
		}
		d.QualityScore = d.CompletenessScore //nolint:staticcheck // deprecated alias emitted for one release cycle
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
		 metadata, completeness_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, repo
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
//
// The CTE is split into two separate recursive queries (forward_chain and
// backward_chain) then UNIONed, because PostgreSQL only treats the last
// branch of a UNION as the recursive term. A single CTE with three branches
// would only recurse on the last (forward) branch, truncating deep backward chains.
// Each recursive branch has a LIMIT 100 safety cap to prevent infinite loops from
// circular supersedes_id references.
func (db *DB) GetDecisionRevisions(ctx context.Context, orgID, id uuid.UUID) ([]model.Decision, error) {
	query := `
	WITH RECURSIVE
	forward_chain AS (
		-- Anchor: the target decision.
		SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		       metadata, completeness_score, precedent_ref, supersedes_id, content_hash,
		       valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, repo, 0 AS depth
		FROM decisions
		WHERE id = $1 AND org_id = $2

		UNION ALL

		-- Walk forward: find decisions that supersede the current one.
		SELECT d.id, d.run_id, d.agent_id, d.org_id, d.decision_type, d.outcome, d.confidence, d.reasoning,
		       d.metadata, d.completeness_score, d.precedent_ref, d.supersedes_id, d.content_hash,
		       d.valid_from, d.valid_to, d.transaction_time, d.created_at, d.session_id, d.agent_context, d.api_key_id, d.tool, d.model, d.repo, fc.depth + 1
		FROM decisions d
		INNER JOIN forward_chain fc ON d.supersedes_id = fc.id
		WHERE d.org_id = $2 AND fc.depth < 100
	),
	backward_chain AS (
		-- Anchor: the target decision.
		SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		       metadata, completeness_score, precedent_ref, supersedes_id, content_hash,
		       valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, repo, 0 AS depth
		FROM decisions
		WHERE id = $1 AND org_id = $2

		UNION ALL

		-- Walk backward: follow supersedes_id links.
		SELECT d.id, d.run_id, d.agent_id, d.org_id, d.decision_type, d.outcome, d.confidence, d.reasoning,
		       d.metadata, d.completeness_score, d.precedent_ref, d.supersedes_id, d.content_hash,
		       d.valid_from, d.valid_to, d.transaction_time, d.created_at, d.session_id, d.agent_context, d.api_key_id, d.tool, d.model, d.repo, bc.depth + 1
		FROM decisions d
		INNER JOIN backward_chain bc ON bc.supersedes_id = d.id
		WHERE d.org_id = $2 AND bc.depth < 100
	),
	all_revisions AS (
		SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		       metadata, completeness_score, precedent_ref, supersedes_id, content_hash,
		       valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, repo
		FROM forward_chain
		UNION
		SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		       metadata, completeness_score, precedent_ref, supersedes_id, content_hash,
		       valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, repo
		FROM backward_chain
	)
	SELECT DISTINCT ON (id) id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
	       metadata, completeness_score, precedent_ref, supersedes_id, content_hash,
	       valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, repo
	FROM all_revisions
	ORDER BY id, valid_from ASC`

	rows, err := db.pool.Query(ctx, query, id, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: get decision revisions: %w", err)
	}
	defer rows.Close()

	decisions, err := scanDecisions(rows)
	if err != nil {
		return nil, err
	}

	// Re-sort by valid_from ASC for chronological ordering.
	// DISTINCT ON (id) sorts by (id, valid_from) which doesn't give us valid_from ASC across IDs.
	sortDecisionsByValidFrom(decisions)
	return decisions, nil
}

// GetRevisionChainIDs returns the IDs of all decisions in the same revision
// chain as the given decision. Walks both forward (decisions that supersede
// this one) and backward (decisions this one supersedes), capped at 100 hops
// in each direction. The result excludes the input ID itself.
//
// This is a lightweight alternative to GetDecisionRevisions for cases where
// only membership testing is needed (e.g., conflict scorer exclusion).
func (db *DB) GetRevisionChainIDs(ctx context.Context, id, orgID uuid.UUID) ([]uuid.UUID, error) {
	query := `
	WITH RECURSIVE
	forward_chain AS (
		SELECT id, supersedes_id, 0 AS depth FROM decisions WHERE id = $1 AND org_id = $2
		UNION ALL
		SELECT d.id, d.supersedes_id, fc.depth + 1
		FROM decisions d
		INNER JOIN forward_chain fc ON d.supersedes_id = fc.id
		WHERE d.org_id = $2 AND fc.depth < 100
	),
	backward_chain AS (
		SELECT id, supersedes_id, 0 AS depth FROM decisions WHERE id = $1 AND org_id = $2
		UNION ALL
		SELECT d.id, d.supersedes_id, bc.depth + 1
		FROM decisions d
		INNER JOIN backward_chain bc ON bc.supersedes_id = d.id
		WHERE d.org_id = $2 AND bc.depth < 100
	)
	SELECT DISTINCT id FROM (
		SELECT id FROM forward_chain WHERE id != $1
		UNION
		SELECT id FROM backward_chain WHERE id != $1
	) chain_ids`

	rows, err := db.pool.Query(ctx, query, id, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: get revision chain IDs: %w", err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var chainID uuid.UUID
		if err := rows.Scan(&chainID); err != nil {
			return nil, fmt.Errorf("storage: scan revision chain ID: %w", err)
		}
		ids = append(ids, chainID)
	}
	return ids, rows.Err()
}

// sortDecisionsByValidFrom sorts a slice of decisions by valid_from ascending.
func sortDecisionsByValidFrom(decisions []model.Decision) {
	sort.Slice(decisions, func(i, j int) bool {
		return decisions[i].ValidFrom.Before(decisions[j].ValidFrom)
	})
}

func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// UnembeddedDecision holds the minimal fields needed to backfill an embedding.
type UnembeddedDecision struct {
	ID           uuid.UUID
	OrgID        uuid.UUID
	DecisionType string
	Outcome      string
	Reasoning    *string
}

// FindUnembeddedDecisions returns active decisions that have no embedding vector,
// ordered oldest-first so the backfill processes them chronologically.
// SECURITY: Intentionally global — background backfill across all orgs. Each
// returned row includes OrgID for downstream scoping (BackfillEmbedding).
func (db *DB) FindUnembeddedDecisions(ctx context.Context, limit int) ([]UnembeddedDecision, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, decision_type, outcome, reasoning
		 FROM decisions
		 WHERE embedding IS NULL AND valid_to IS NULL
		 ORDER BY valid_from ASC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: find unembedded decisions: %w", err)
	}
	defer rows.Close()

	var results []UnembeddedDecision
	for rows.Next() {
		var d UnembeddedDecision
		if err := rows.Scan(&d.ID, &d.OrgID, &d.DecisionType, &d.Outcome, &d.Reasoning); err != nil {
			return nil, fmt.Errorf("storage: scan unembedded decision: %w", err)
		}
		results = append(results, d)
	}
	return results, rows.Err()
}

// BackfillEmbedding updates a decision's embedding and queues a search outbox
// entry so the outbox worker syncs it to Qdrant. Both writes are atomic.
func (db *DB) BackfillEmbedding(ctx context.Context, id, orgID uuid.UUID, emb pgvector.Vector) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage: begin backfill tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE decisions SET embedding = $1 WHERE id = $2 AND org_id = $3 AND valid_to IS NULL`,
		emb, id, orgID)
	if err != nil {
		return fmt.Errorf("storage: update embedding: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // Decision was revised or deleted — skip silently.
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO search_outbox (decision_id, org_id, operation)
		 VALUES ($1, $2, 'upsert')
		 ON CONFLICT (decision_id, operation) DO UPDATE SET created_at = now(), attempts = 0, locked_until = NULL`,
		id, orgID); err != nil {
		return fmt.Errorf("storage: queue backfill outbox: %w", err)
	}

	return tx.Commit(ctx)
}

// FindDecisionsMissingOutcomeEmbedding returns active decisions that have
// embedding but no outcome_embedding (for backfilling Option B).
// SECURITY: Intentionally global — background backfill across all orgs. Each
// returned row includes OrgID for downstream scoping (BackfillOutcomeEmbedding).
func (db *DB) FindDecisionsMissingOutcomeEmbedding(ctx context.Context, limit int) ([]UnembeddedDecision, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, decision_type, outcome, reasoning
		 FROM decisions
		 WHERE embedding IS NOT NULL AND outcome_embedding IS NULL AND valid_to IS NULL
		 ORDER BY valid_from ASC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: find decisions missing outcome embedding: %w", err)
	}
	defer rows.Close()

	var results []UnembeddedDecision
	for rows.Next() {
		var d UnembeddedDecision
		if err := rows.Scan(&d.ID, &d.OrgID, &d.DecisionType, &d.Outcome, &d.Reasoning); err != nil {
			return nil, fmt.Errorf("storage: scan decision: %w", err)
		}
		results = append(results, d)
	}
	return results, rows.Err()
}

// BackfillOutcomeEmbedding updates a decision's outcome_embedding. Unlike
// BackfillEmbedding, this does not queue an outbox entry — outcome_embedding
// is used only for semantic conflict detection, not for Qdrant vector search.
func (db *DB) BackfillOutcomeEmbedding(ctx context.Context, id, orgID uuid.UUID, emb pgvector.Vector) error {
	tag, err := db.pool.Exec(ctx,
		`UPDATE decisions SET outcome_embedding = $1 WHERE id = $2 AND org_id = $3 AND valid_to IS NULL`,
		emb, id, orgID)
	if err != nil {
		return fmt.Errorf("storage: update outcome embedding: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // Decision was revised or deleted — skip silently.
	}
	return nil
}

// PgCandidateFinder implements search.CandidateFinder using a Postgres sequential scan.
// This is the "no-Qdrant" fallback: acceptable for small deployments (<100k decisions)
// where latency from a sequential scan is tolerable. At scale, use QdrantIndex instead.
//
// Once Postgres HNSW indexes are dropped (migration 049), this performs a full sequential
// scan for each ANN call — suitable only when Qdrant is unavailable and the dataset is small.
type PgCandidateFinder struct {
	db *DB
}

// NewPgCandidateFinder returns a CandidateFinder backed by Postgres sequential scan.
func NewPgCandidateFinder(db *DB) *PgCandidateFinder {
	return &PgCandidateFinder{db: db}
}

// FindSimilar returns the top-limit decisions ordered by cosine distance to embedding,
// scoped to the org. excludeID is filtered in SQL. repo, when non-nil, restricts to
// decisions with the same repo value or no repo.
func (f *PgCandidateFinder) FindSimilar(ctx context.Context, orgID uuid.UUID, embedding []float32, excludeID uuid.UUID, repo *string, limit int) ([]search.Result, error) {
	if limit <= 0 {
		limit = 50
	}
	emb := pgvector.NewVector(embedding)
	rows, err := f.db.pool.Query(ctx,
		`SELECT id, 1 - (embedding <=> $3) AS score
		 FROM decisions
		 WHERE org_id = $1 AND id != $2 AND embedding IS NOT NULL AND outcome_embedding IS NOT NULL AND valid_to IS NULL
		   AND ($5::text IS NULL OR repo IS NULL OR repo = $5)
		 ORDER BY embedding <=> $3
		 LIMIT $4`, orgID, excludeID, emb, limit, repo)
	if err != nil {
		return nil, fmt.Errorf("storage: pg candidate finder: %w", err)
	}
	defer rows.Close()

	var results []search.Result
	for rows.Next() {
		var id uuid.UUID
		var score float32
		if err := rows.Scan(&id, &score); err != nil {
			return nil, fmt.Errorf("storage: scan candidate: %w", err)
		}
		results = append(results, search.Result{DecisionID: id, Score: score})
	}
	return results, rows.Err()
}

// GetDecisionEmbeddings returns (embedding, outcome_embedding) pairs for a batch of decisions.
// Used by consensus scoring to prepare Qdrant queries and pairwise cosine comparisons.
// Only decisions with both embeddings populated are included in the result.
func (db *DB) GetDecisionEmbeddings(ctx context.Context, ids []uuid.UUID, orgID uuid.UUID) (map[uuid.UUID][2]pgvector.Vector, error) {
	if len(ids) == 0 {
		return map[uuid.UUID][2]pgvector.Vector{}, nil
	}

	rows, err := db.pool.Query(ctx,
		`SELECT id, embedding, outcome_embedding
		 FROM decisions
		 WHERE id = ANY($1) AND org_id = $2 AND valid_to IS NULL
		   AND embedding IS NOT NULL AND outcome_embedding IS NOT NULL`,
		ids, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: get decision embeddings: %w", err)
	}
	defer rows.Close()

	result := make(map[uuid.UUID][2]pgvector.Vector, len(ids))
	for rows.Next() {
		var id uuid.UUID
		var emb, outcomeEmb pgvector.Vector
		if err := rows.Scan(&id, &emb, &outcomeEmb); err != nil {
			return nil, fmt.Errorf("storage: scan decision embeddings: %w", err)
		}
		result[id] = [2]pgvector.Vector{emb, outcomeEmb}
	}
	return result, rows.Err()
}

// GetConflictCount returns the number of open or acknowledged conflicts involving a decision.
func (db *DB) GetConflictCount(ctx context.Context, decisionID, orgID uuid.UUID) (int, error) {
	var count int
	err := db.pool.QueryRow(ctx,
		`SELECT COUNT(*)
		 FROM scored_conflicts
		 WHERE org_id = $2
		   AND status IN ('open', 'acknowledged')
		   AND (decision_a_id = $1 OR decision_b_id = $1)`,
		decisionID, orgID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("storage: conflict count: %w", err)
	}
	return count, nil
}

// GetConflictCountsBatch returns conflict counts for a batch of decisions.
// A conflict is counted if there is an open or acknowledged entry in scored_conflicts.
// Decisions with no conflicts are absent from the returned map.
func (db *DB) GetConflictCountsBatch(ctx context.Context, ids []uuid.UUID, orgID uuid.UUID) (map[uuid.UUID]int, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]int{}, nil
	}

	rows, err := db.pool.Query(ctx, `
		WITH batch AS (SELECT unnest($1::uuid[]) AS id)
		SELECT b.id, COUNT(*) AS conflict_count
		FROM batch b
		JOIN scored_conflicts sc
		     ON (sc.decision_a_id = b.id OR sc.decision_b_id = b.id)
		     AND sc.org_id = $2
		     AND sc.status IN ('open', 'acknowledged')
		GROUP BY b.id`, ids, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: conflict counts batch: %w", err)
	}
	defer rows.Close()

	result := make(map[uuid.UUID]int, len(ids))
	for rows.Next() {
		var id uuid.UUID
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, fmt.Errorf("storage: scan conflict count: %w", err)
		}
		result[id] = count
	}
	return result, rows.Err()
}

// DecisionRef is a lightweight reference to a decision for batch operations.
type DecisionRef struct {
	ID    uuid.UUID
	OrgID uuid.UUID
}

// FindEmbeddedDecisionIDs returns IDs of current decisions that have both
// embedding and outcome_embedding populated but have NOT yet been scored for
// conflicts (conflict_scored_at IS NULL). Used by conflict scoring backfill
// so that server restarts only score new decisions, not the entire corpus.
// SECURITY: Intentionally global — background backfill across all orgs. Each
// returned row includes OrgID for downstream scoping (ScoreForDecision).
func (db *DB) FindEmbeddedDecisionIDs(ctx context.Context, limit int) ([]DecisionRef, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id FROM decisions
		 WHERE valid_to IS NULL
		   AND embedding IS NOT NULL
		   AND outcome_embedding IS NOT NULL
		   AND conflict_scored_at IS NULL
		 ORDER BY valid_from ASC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: find embedded decision IDs: %w", err)
	}
	defer rows.Close()

	var refs []DecisionRef
	for rows.Next() {
		var r DecisionRef
		if err := rows.Scan(&r.ID, &r.OrgID); err != nil {
			return nil, fmt.Errorf("storage: scan embedded decision ref: %w", err)
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

// MarkDecisionConflictScored sets conflict_scored_at to now() for a decision.
// Called after ScoreForDecision completes so the decision won't be re-processed
// on subsequent backfill runs.
func (db *DB) MarkDecisionConflictScored(ctx context.Context, id, orgID uuid.UUID) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE decisions SET conflict_scored_at = now() WHERE id = $1 AND org_id = $2`, id, orgID)
	if err != nil {
		return fmt.Errorf("storage: mark decision conflict scored: %w", err)
	}
	return nil
}

// ResetConflictScoredAt clears conflict_scored_at for all decisions, forcing
// the next backfill to re-score everything. Used when transitioning from
// embedding-only to LLM-validated scoring so all pairs get re-evaluated.
// SECURITY: Intentionally global — this is a one-time migration operation on
// startup that transitions ALL orgs from embedding-only to LLM-validated
// scoring. Scoping per-org would require iterating ListOrganizationIDs and
// yield the same result since the condition is server-wide (HasLLMValidator).
func (db *DB) ResetConflictScoredAt(ctx context.Context) (int64, error) {
	tag, err := db.pool.Exec(ctx,
		`UPDATE decisions SET conflict_scored_at = NULL WHERE conflict_scored_at IS NOT NULL`)
	if err != nil {
		return 0, fmt.Errorf("storage: reset conflict scored_at: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CountUnvalidatedConflicts returns the number of scored_conflicts that were
// not scored by the current LLM classifier (llm_v2). This includes embedding-only
// conflicts and old binary-verdict 'llm' conflicts, both of which are stale.
// Used to decide whether to clear old conflicts at startup when transitioning
// to LLM validation. Returns 0 once all conflicts are llm_v2, so subsequent
// restarts skip the migration entirely.
// SECURITY: Intentionally global — startup check that determines whether the
// LLM migration path should fire. A non-zero count anywhere triggers the
// migration for all orgs.
func (db *DB) CountUnvalidatedConflicts(ctx context.Context) (int, error) {
	var count int
	err := db.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM scored_conflicts WHERE scoring_method NOT IN ('llm_v2')`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("storage: count unvalidated conflicts: %w", err)
	}
	return count, nil
}

// DecisionQualityStats holds aggregate completeness metrics for an org's decisions.
type DecisionQualityStats struct {
	Total            int
	AvgCompleteness  float64
	BelowHalf        int // completeness_score < 0.5
	BelowThird       int // completeness_score < 0.33
	WithReasoning    int // reasoning IS NOT NULL AND reasoning != ''
	WithAlternatives int // decisions that have at least one alternative
}

// GetDecisionQualityStats returns aggregate quality metrics for current decisions in an org.
func (db *DB) GetDecisionQualityStats(ctx context.Context, orgID uuid.UUID) (DecisionQualityStats, error) {
	var s DecisionQualityStats
	err := db.pool.QueryRow(ctx, `
		SELECT count(*),
		       COALESCE(avg(completeness_score), 0),
		       count(*) FILTER (WHERE completeness_score < 0.5),
		       count(*) FILTER (WHERE completeness_score < 0.33),
		       count(*) FILTER (WHERE reasoning IS NOT NULL AND reasoning != ''),
		       count(*) FILTER (WHERE EXISTS (
		           SELECT 1 FROM alternatives a WHERE a.decision_id = decisions.id
		       ))
		FROM decisions
		WHERE org_id = $1 AND valid_to IS NULL`, orgID).Scan(
		&s.Total, &s.AvgCompleteness, &s.BelowHalf, &s.BelowThird, &s.WithReasoning, &s.WithAlternatives)
	if err != nil {
		return s, fmt.Errorf("storage: decision quality stats: %w", err)
	}
	return s, nil
}

// GetDecisionForScoring returns a decision with embedding, outcome_embedding,
// session_id, agent_context, and repo for conflict scoring. The additional fields
// provide project, task, and session context to the LLM validator.
func (db *DB) GetDecisionForScoring(ctx context.Context, id, orgID uuid.UUID) (model.Decision, error) {
	var d model.Decision
	err := db.pool.QueryRow(ctx,
		`SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		 valid_from, embedding, outcome_embedding, session_id, agent_context, repo
		 FROM decisions WHERE id = $1 AND org_id = $2 AND valid_to IS NULL`,
		id, orgID,
	).Scan(
		&d.ID, &d.RunID, &d.AgentID, &d.OrgID, &d.DecisionType, &d.Outcome, &d.Confidence, &d.Reasoning,
		&d.ValidFrom, &d.Embedding, &d.OutcomeEmbedding, &d.SessionID, &d.AgentContext, &d.Repo,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Decision{}, fmt.Errorf("storage: decision %s: %w", id, ErrNotFound)
		}
		return model.Decision{}, fmt.Errorf("storage: get decision for scoring: %w", err)
	}
	return d, nil
}

// GetDecisionOutcomeSignals returns temporal, graph, and fate outcome signals for a single decision.
// All signals are computed at query time from existing schema columns; none are stored.
func (db *DB) GetDecisionOutcomeSignals(ctx context.Context, id, orgID uuid.UUID) (model.OutcomeSignals, error) {
	var signals model.OutcomeSignals

	// Supersession velocity: hours from this decision's valid_from to the superseding decision's valid_from.
	if err := db.pool.QueryRow(ctx, `
		SELECT EXTRACT(EPOCH FROM (s.valid_from - d.valid_from)) / 3600
		FROM decisions d
		JOIN decisions s ON s.supersedes_id = d.id AND s.org_id = d.org_id
		WHERE d.id = $1 AND d.org_id = $2
		LIMIT 1`, id, orgID).Scan(&signals.SupersessionVelocityHours); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return signals, fmt.Errorf("storage: supersession velocity: %w", err)
		}
		// ErrNoRows → never superseded, SupersessionVelocityHours remains nil.
	}

	// Precedent citation count: how many live decisions cite this one as a precedent.
	if err := db.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM decisions
		WHERE precedent_ref = $1 AND org_id = $2 AND valid_to IS NULL`,
		id, orgID).Scan(&signals.PrecedentCitationCount); err != nil {
		return signals, fmt.Errorf("storage: precedent citation count: %w", err)
	}

	// Conflict fate: won/lost/no-winner counts across all resolved conflicts involving this decision.
	if err := db.pool.QueryRow(ctx, `
		SELECT
		    COUNT(*) FILTER (WHERE winning_decision_id = $1)::int,
		    COUNT(*) FILTER (WHERE winning_decision_id IS NOT NULL AND winning_decision_id != $1)::int,
		    COUNT(*) FILTER (WHERE winning_decision_id IS NULL AND status = 'resolved')::int
		FROM scored_conflicts
		WHERE org_id = $2
		  AND status IN ('resolved', 'wont_fix')
		  AND (decision_a_id = $1 OR decision_b_id = $1)`,
		id, orgID).Scan(
		&signals.ConflictFate.Won,
		&signals.ConflictFate.Lost,
		&signals.ConflictFate.ResolvedNoWinner,
	); err != nil {
		return signals, fmt.Errorf("storage: conflict fate: %w", err)
	}

	return signals, nil
}

// GetDecisionOutcomeSignalsBatch returns outcome signals for multiple decisions in three
// batched queries (no N+1). Returns a map of decision ID → OutcomeSignals.
// Decisions not found in the result have zero-value OutcomeSignals.
func (db *DB) GetDecisionOutcomeSignalsBatch(ctx context.Context, ids []uuid.UUID, orgID uuid.UUID) (map[uuid.UUID]model.OutcomeSignals, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]model.OutcomeSignals{}, nil
	}

	result := make(map[uuid.UUID]model.OutcomeSignals, len(ids))
	for _, id := range ids {
		result[id] = model.OutcomeSignals{}
	}

	// Batched supersession velocity.
	velRows, err := db.pool.Query(ctx, `
		SELECT d.id, EXTRACT(EPOCH FROM (s.valid_from - d.valid_from)) / 3600
		FROM decisions d
		JOIN decisions s ON s.supersedes_id = d.id AND s.org_id = d.org_id
		WHERE d.id = ANY($1) AND d.org_id = $2`, ids, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: batch supersession velocity: %w", err)
	}
	defer velRows.Close()
	for velRows.Next() {
		var id uuid.UUID
		var v float64
		if err := velRows.Scan(&id, &v); err != nil {
			return nil, fmt.Errorf("storage: scan batch supersession velocity: %w", err)
		}
		s := result[id]
		s.SupersessionVelocityHours = &v
		result[id] = s
	}
	if err := velRows.Err(); err != nil {
		return nil, fmt.Errorf("storage: batch supersession velocity rows: %w", err)
	}

	// Batched precedent citation counts.
	citeRows, err := db.pool.Query(ctx, `
		SELECT precedent_ref, COUNT(*)::int
		FROM decisions
		WHERE precedent_ref = ANY($1) AND org_id = $2 AND valid_to IS NULL
		GROUP BY precedent_ref`, ids, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: batch precedent citations: %w", err)
	}
	defer citeRows.Close()
	for citeRows.Next() {
		var id uuid.UUID
		var count int
		if err := citeRows.Scan(&id, &count); err != nil {
			return nil, fmt.Errorf("storage: scan batch precedent citations: %w", err)
		}
		s := result[id]
		s.PrecedentCitationCount = count
		result[id] = s
	}
	if err := citeRows.Err(); err != nil {
		return nil, fmt.Errorf("storage: batch precedent citations rows: %w", err)
	}

	// Batched conflict fate. Use UNION ALL so both sides of each conflict are correctly attributed.
	fateRows, err := db.pool.Query(ctx, `
		WITH sides AS (
		    SELECT decision_a_id AS target_id, winning_decision_id, status
		    FROM scored_conflicts
		    WHERE org_id = $2
		      AND status IN ('resolved', 'wont_fix')
		      AND decision_a_id = ANY($1)
		    UNION ALL
		    SELECT decision_b_id AS target_id, winning_decision_id, status
		    FROM scored_conflicts
		    WHERE org_id = $2
		      AND status IN ('resolved', 'wont_fix')
		      AND decision_b_id = ANY($1)
		)
		SELECT
		    target_id,
		    COUNT(*) FILTER (WHERE winning_decision_id = target_id)::int,
		    COUNT(*) FILTER (WHERE winning_decision_id IS NOT NULL AND winning_decision_id != target_id)::int,
		    COUNT(*) FILTER (WHERE winning_decision_id IS NULL AND status = 'resolved')::int
		FROM sides
		GROUP BY target_id`, ids, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: batch conflict fate: %w", err)
	}
	defer fateRows.Close()
	for fateRows.Next() {
		var id uuid.UUID
		var won, lost, noWinner int
		if err := fateRows.Scan(&id, &won, &lost, &noWinner); err != nil {
			return nil, fmt.Errorf("storage: scan batch conflict fate: %w", err)
		}
		s := result[id]
		s.ConflictFate = model.ConflictFate{Won: won, Lost: lost, ResolvedNoWinner: noWinner}
		result[id] = s
	}
	if err := fateRows.Err(); err != nil {
		return nil, fmt.Errorf("storage: batch conflict fate rows: %w", err)
	}

	return result, nil
}
