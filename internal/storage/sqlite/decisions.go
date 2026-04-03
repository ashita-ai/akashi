package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// ---- Trace (write path) ----

// CreateTraceTx creates a run, decision, alternatives, evidence, and audit in a single transaction.
func (l *LiteDB) CreateTraceTx(ctx context.Context, params storage.CreateTraceParams) (model.AgentRun, model.Decision, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("sqlite: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	run, dec, err := createTraceInTx(ctx, tx, params)
	if err != nil {
		return model.AgentRun{}, model.Decision{}, err
	}

	if err := tx.Commit(); err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("sqlite: commit trace: %w", err)
	}
	return run, dec, nil
}

// CreateTraceAndAdjudicateConflictTx creates a trace and resolves a conflict atomically.
func (l *LiteDB) CreateTraceAndAdjudicateConflictTx(ctx context.Context, traceParams storage.CreateTraceParams, conflictParams storage.AdjudicateConflictInTraceParams) (model.AgentRun, model.Decision, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("sqlite: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	run, dec, err := createTraceInTx(ctx, tx, traceParams)
	if err != nil {
		return model.AgentRun{}, model.Decision{}, err
	}

	// Adjudicate conflict.
	res, err := tx.ExecContext(ctx,
		`UPDATE scored_conflicts
		 SET status = 'resolved', resolved_by = ?, resolved_at = datetime('now'),
		     resolution_note = ?, resolution_decision_id = ?, winning_decision_id = ?
		 WHERE id = ? AND org_id = ?`,
		conflictParams.ResolvedBy,
		conflictParams.ResNote,
		uuidStr(dec.ID),
		nullUUIDStr(conflictParams.WinningDecisionID),
		uuidStr(conflictParams.ConflictID),
		uuidStr(traceParams.OrgID),
	)
	if err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("sqlite: adjudicate conflict: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("sqlite: conflict: %w", storage.ErrNotFound)
	}

	if err := insertAuditTx(ctx, tx, conflictParams.Audit); err != nil {
		return model.AgentRun{}, model.Decision{}, err
	}

	if err := tx.Commit(); err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("sqlite: commit trace+adjudicate: %w", err)
	}
	return run, dec, nil
}

func createTraceInTx(ctx context.Context, tx *sql.Tx, p storage.CreateTraceParams) (model.AgentRun, model.Decision, error) {
	now := time.Now().UTC()
	d := p.Decision

	// 1. Create run.
	run := model.AgentRun{
		ID:        uuid.New(),
		AgentID:   p.AgentID,
		OrgID:     p.OrgID,
		TraceID:   p.TraceID,
		Status:    "completed",
		StartedAt: now,
		Metadata:  p.Metadata,
		CreatedAt: now,
	}
	completedAt := now
	run.CompletedAt = &completedAt

	_, err := tx.ExecContext(ctx,
		`INSERT INTO agent_runs (id, agent_id, org_id, trace_id, parent_run_id, status, started_at, completed_at, metadata, created_at)
		 VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, ?)`,
		uuidStr(run.ID),
		run.AgentID,
		uuidStr(run.OrgID),
		p.TraceID,
		run.Status,
		timeStr(run.StartedAt),
		timeStr(completedAt),
		jsonStr(run.Metadata),
		timeStr(run.CreatedAt),
	)
	if err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("sqlite: insert run: %w", err)
	}

	// 2. Create decision.
	d.ID = uuid.New()
	d.RunID = run.ID
	d.AgentID = p.AgentID
	d.OrgID = p.OrgID
	d.SessionID = p.SessionID
	if p.AgentContext != nil {
		d.AgentContext = p.AgentContext
	}
	if d.AgentContext == nil {
		d.AgentContext = map[string]any{}
	}
	if d.Metadata == nil {
		d.Metadata = map[string]any{}
	}
	if d.ValidFrom.IsZero() {
		d.ValidFrom = now
	}
	if d.TransactionTime.IsZero() {
		d.TransactionTime = now
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO decisions
		 (id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		  embedding, outcome_embedding, metadata, completeness_score, outcome_score, precedent_ref,
		  precedent_reason, supersedes_id, content_hash, valid_from, valid_to, transaction_time, created_at,
		  session_id, agent_context, api_key_id, tool, model, project)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		uuidStr(d.ID),
		uuidStr(d.RunID),
		d.AgentID,
		uuidStr(d.OrgID),
		d.DecisionType,
		d.Outcome,
		d.Confidence,
		d.Reasoning,
		vectorToBlob(d.Embedding),
		vectorToBlob(d.OutcomeEmbedding),
		jsonStr(d.Metadata),
		d.CompletenessScore,
		d.OutcomeScore,
		nullUUIDStr(d.PrecedentRef),
		d.PrecedentReason,
		nullUUIDStr(d.SupersedesID),
		d.ContentHash,
		timeStr(d.ValidFrom),
		nullTimeStr(d.ValidTo),
		timeStr(d.TransactionTime),
		timeStr(d.CreatedAt),
		nullUUIDStr(d.SessionID),
		jsonStr(d.AgentContext),
		nullUUIDStr(d.APIKeyID),
		d.Tool,
		d.Model,
		d.Project,
	)
	if err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("sqlite: insert decision: %w", err)
	}

	// 3. Insert alternatives.
	for i := range p.Alternatives {
		alt := &p.Alternatives[i]
		if alt.ID == uuid.Nil {
			alt.ID = uuid.New()
		}
		if alt.CreatedAt.IsZero() {
			alt.CreatedAt = now
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO alternatives (id, decision_id, label, rejection_reason, metadata, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			uuidStr(alt.ID),
			uuidStr(d.ID),
			alt.Label,
			alt.RejectionReason,
			jsonStr(alt.Metadata),
			timeStr(alt.CreatedAt),
		)
		if err != nil {
			return model.AgentRun{}, model.Decision{}, fmt.Errorf("sqlite: insert alternative: %w", err)
		}
	}

	// 4. Insert evidence.
	for i := range p.Evidence {
		ev := &p.Evidence[i]
		if ev.ID == uuid.Nil {
			ev.ID = uuid.New()
		}
		if ev.CreatedAt.IsZero() {
			ev.CreatedAt = now
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO evidence (id, decision_id, org_id, source_type, source_uri, content, relevance_score, embedding, metadata, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			uuidStr(ev.ID),
			uuidStr(d.ID),
			uuidStr(p.OrgID),
			string(ev.SourceType),
			ev.SourceURI,
			ev.Content,
			ev.RelevanceScore,
			vectorToBlob(ev.Embedding),
			jsonStr(ev.Metadata),
			timeStr(ev.CreatedAt),
		)
		if err != nil {
			return model.AgentRun{}, model.Decision{}, fmt.Errorf("sqlite: insert evidence: %w", err)
		}
	}

	// 5. Supersession: if this decision supersedes another, close the old one.
	if d.SupersedesID != nil {
		_, err = tx.ExecContext(ctx,
			`UPDATE decisions SET valid_to = ? WHERE id = ? AND org_id = ? AND valid_to IS NULL`,
			timeStr(d.ValidFrom), uuidStr(*d.SupersedesID), uuidStr(d.OrgID),
		)
		if err != nil {
			return model.AgentRun{}, model.Decision{}, fmt.Errorf("sqlite: close superseded decision: %w", err)
		}
	}

	// 6. Insert audit if provided.
	if p.AuditEntry != nil {
		if err := insertAuditTx(ctx, tx, *p.AuditEntry); err != nil {
			return model.AgentRun{}, model.Decision{}, err
		}
	}

	d.Alternatives = p.Alternatives
	d.Evidence = p.Evidence
	return run, d, nil
}

// ---- Query (read path) ----

// decisionCols is the SELECT column list used by all decision queries.
const decisionCols = `id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
	metadata, completeness_score, outcome_score, precedent_ref, precedent_reason, supersedes_id, content_hash,
	valid_from, valid_to, transaction_time, created_at, session_id, agent_context,
	api_key_id, tool, model, project`

// QueryDecisions returns filtered, paginated decisions.
func (l *LiteDB) QueryDecisions(ctx context.Context, orgID uuid.UUID, req model.QueryRequest) ([]model.Decision, int, error) {
	where, args := buildDecisionWhere(orgID, req.Filters, req.TraceID)

	// Order.
	orderCol := "valid_from"
	if req.OrderBy != "" {
		orderCol = sanitizeOrderCol(req.OrderBy)
	}
	orderDir := "DESC"
	if strings.EqualFold(req.OrderDir, "asc") {
		orderDir = "ASC"
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	offset := req.Offset
	if offset < 0 {
		offset = 0
	}

	// Use COUNT(*) OVER() window function to get the total count alongside data
	// rows in a single query, eliminating a separate COUNT(*) table scan.
	q := fmt.Sprintf("SELECT %s, COUNT(*) OVER() FROM decisions %s ORDER BY %s %s LIMIT ? OFFSET ?", //nolint:gosec // G201: interpolated values are sanitized constants
		decisionCols, where, orderCol, orderDir)
	args = append(args, limit, offset)

	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("sqlite: query decisions: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	decisions, total, err := scanDecisionRowsWithTotal(rows)
	if err != nil {
		return nil, 0, err
	}

	// Load alternatives and evidence if requested.
	if len(decisions) > 0 {
		include := make(map[string]bool, len(req.Include))
		for _, inc := range req.Include {
			include[inc] = true
		}
		if include["alternatives"] || include["all"] {
			if err := l.loadAlternatives(ctx, decisions); err != nil {
				return nil, 0, err
			}
		}
		if include["evidence"] || include["all"] {
			if err := l.loadEvidence(ctx, orgID, decisions); err != nil {
				return nil, 0, err
			}
		}
	}

	return decisions, total, nil
}

// QueryDecisionsTemporal returns decisions as they were at a specific point in time.
func (l *LiteDB) QueryDecisionsTemporal(ctx context.Context, orgID uuid.UUID, req model.TemporalQueryRequest) ([]model.Decision, error) {
	where, args := buildDecisionWhere(orgID, req.Filters, nil)

	// Add temporal conditions: transaction_time <= asOf AND (valid_to IS NULL OR valid_to > asOf).
	asOfStr := timeStr(req.AsOf)
	where += " AND transaction_time <= ? AND (valid_to IS NULL OR valid_to > ?)"
	args = append(args, asOfStr, asOfStr)

	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}

	q := fmt.Sprintf("SELECT %s FROM decisions %s ORDER BY valid_from DESC LIMIT ?", //nolint:gosec // G201
		decisionCols, where)
	args = append(args, limit)

	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: query decisions temporal: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	return scanDecisionRows(rows)
}

// GetDecisionsByIDs returns decisions by their IDs.
func (l *LiteDB) GetDecisionsByIDs(ctx context.Context, orgID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]model.Decision, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]model.Decision{}, nil
	}
	idsJSON := uuidSliceToJSON(ids)
	q := fmt.Sprintf(
		`SELECT %s FROM decisions
		 WHERE org_id = ? AND id IN (SELECT value FROM json_each(?)) AND valid_to IS NULL`,
		decisionCols,
	)
	rows, err := l.db.QueryContext(ctx, q, uuidStr(orgID), idsJSON)
	if err != nil {
		return nil, fmt.Errorf("sqlite: get decisions by ids: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	decisions, err := scanDecisionRows(rows)
	if err != nil {
		return nil, err
	}

	result := make(map[uuid.UUID]model.Decision, len(decisions))
	for _, d := range decisions {
		result[d.ID] = d
	}
	return result, nil
}

// GetDecisionsByAgent returns paginated decisions for an agent.
func (l *LiteDB) GetDecisionsByAgent(ctx context.Context, orgID uuid.UUID, agentID string, limit, offset int, from, to *time.Time) ([]model.Decision, int, error) {
	filters := model.QueryFilters{
		AgentIDs: []string{agentID},
	}
	if from != nil || to != nil {
		filters.TimeRange = &model.TimeRange{From: from, To: to}
	}
	req := model.QueryRequest{
		Filters: filters,
		Limit:   limit,
		Offset:  offset,
	}
	return l.QueryDecisions(ctx, orgID, req)
}

// GetDecisionForScoring returns a single decision with embedding fields for conflict scoring.
func (l *LiteDB) GetDecisionForScoring(ctx context.Context, id, orgID uuid.UUID) (model.Decision, error) {
	row := l.db.QueryRowContext(ctx,
		`SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		        valid_from, embedding, outcome_embedding, session_id, agent_context, project, transaction_time
		 FROM decisions WHERE id = ? AND org_id = ? AND valid_to IS NULL`,
		uuidStr(id), uuidStr(orgID),
	)

	var (
		d            model.Decision
		idStr        string
		runIDStr     string
		orgIDStr     string
		validFromStr string
		embBlob      []byte
		outEmbBlob   []byte
		sessionIDStr sql.NullString
		agentCtxJSON sql.NullString
		project      sql.NullString
		txTimeStr    string
	)
	err := row.Scan(&idStr, &runIDStr, &d.AgentID, &orgIDStr, &d.DecisionType,
		&d.Outcome, &d.Confidence, &d.Reasoning,
		&validFromStr, &embBlob, &outEmbBlob,
		&sessionIDStr, &agentCtxJSON, &project, &txTimeStr)
	if err != nil {
		if err == sql.ErrNoRows {
			return model.Decision{}, storage.ErrNotFound
		}
		return model.Decision{}, fmt.Errorf("sqlite: get decision for scoring: %w", err)
	}

	d.ID = parseUUID(idStr)
	d.RunID = parseUUID(runIDStr)
	d.OrgID = parseUUID(orgIDStr)
	d.ValidFrom = parseTime(validFromStr)
	d.TransactionTime = parseTime(txTimeStr)
	d.Embedding = blobToVector(embBlob)
	d.OutcomeEmbedding = blobToVector(outEmbBlob)
	d.SessionID = parseNullUUID(sessionIDStr)
	if agentCtxJSON.Valid {
		d.AgentContext = map[string]any{}
		_ = scanJSON(agentCtxJSON, &d.AgentContext)
	}
	if project.Valid {
		d.Project = &project.String
	}
	return d, nil
}

// ---- Search ----

// SearchDecisionsByText performs FTS5 full-text search over decisions.
func (l *LiteDB) SearchDecisionsByText(ctx context.Context, orgID uuid.UUID, query string, filters model.QueryFilters, limit int) ([]model.SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}

	// Build filter clause for the decisions table (aliased as d).
	filterWhere, filterArgs := buildDecisionFilterWhere("d", orgID, filters)

	// FTS5 search with BM25 ranking.
	q := fmt.Sprintf( //nolint:gosec // G201
		`SELECT d.id, d.run_id, d.agent_id, d.org_id, d.decision_type, d.outcome,
		        d.confidence, d.reasoning, d.metadata, d.completeness_score,
		        d.outcome_score, d.precedent_ref, d.precedent_reason, d.supersedes_id, d.content_hash,
		        d.valid_from, d.valid_to, d.transaction_time, d.created_at,
		        d.session_id, d.agent_context, d.api_key_id, d.tool, d.model, d.project,
		        -rank AS relevance
		 FROM decisions_fts f
		 JOIN decisions d ON d.rowid = f.rowid
		 WHERE decisions_fts MATCH ?
		   AND d.org_id = ? AND d.valid_to IS NULL
		   %s
		 ORDER BY relevance DESC
		 LIMIT ?`,
		filterWhere,
	)

	args := []any{query, uuidStr(orgID)}
	args = append(args, filterArgs...)
	args = append(args, limit)

	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		// If FTS match fails (e.g. bad syntax), fall back to LIKE.
		return l.searchDecisionsByLike(ctx, orgID, query, filters, limit)
	}
	defer rows.Close() //nolint:errcheck

	results := make([]model.SearchResult, 0)
	for rows.Next() {
		var (
			d            model.Decision
			relevance    float32
			idStr        string
			runIDStr     string
			orgIDStr     string
			metaJSON     sql.NullString
			precedent    sql.NullString
			supersedes   sql.NullString
			validFromStr string
			validToStr   sql.NullString
			txTimeStr    string
			createdStr   string
			sessionStr   sql.NullString
			ctxJSON      sql.NullString
			apiKeyStr    sql.NullString
			tool         sql.NullString
			modelStr     sql.NullString
			project      sql.NullString
		)
		err := rows.Scan(&idStr, &runIDStr, &d.AgentID, &orgIDStr, &d.DecisionType,
			&d.Outcome, &d.Confidence, &d.Reasoning, &metaJSON, &d.CompletenessScore,
			&d.OutcomeScore, &precedent, &d.PrecedentReason, &supersedes, &d.ContentHash,
			&validFromStr, &validToStr, &txTimeStr, &createdStr,
			&sessionStr, &ctxJSON, &apiKeyStr, &tool, &modelStr, &project,
			&relevance)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan search result: %w", err)
		}
		d.ID = parseUUID(idStr)
		d.RunID = parseUUID(runIDStr)
		d.OrgID = parseUUID(orgIDStr)
		d.PrecedentRef = parseNullUUID(precedent)
		d.SupersedesID = parseNullUUID(supersedes)
		d.ValidFrom = parseTime(validFromStr)
		d.ValidTo = parseNullTime(validToStr)
		d.TransactionTime = parseTime(txTimeStr)
		d.CreatedAt = parseTime(createdStr)
		d.SessionID = parseNullUUID(sessionStr)
		d.APIKeyID = parseNullUUID(apiKeyStr)
		d.Metadata = map[string]any{}
		_ = scanJSON(metaJSON, &d.Metadata)
		d.AgentContext = map[string]any{}
		_ = scanJSON(ctxJSON, &d.AgentContext)
		if tool.Valid {
			d.Tool = &tool.String
		}
		if modelStr.Valid {
			d.Model = &modelStr.String
		}
		if project.Valid {
			d.Project = &project.String
		}

		results = append(results, model.SearchResult{
			Decision:        d,
			SimilarityScore: relevance,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: search rows: %w", err)
	}
	if len(results) == 0 {
		return []model.SearchResult{}, nil
	}
	return results, nil
}

// searchDecisionsByLike is a fallback when FTS5 MATCH fails (e.g. bad query syntax).
func (l *LiteDB) searchDecisionsByLike(ctx context.Context, orgID uuid.UUID, query string, filters model.QueryFilters, limit int) ([]model.SearchResult, error) {
	pattern := "%" + query + "%"
	filterWhere, filterArgs := buildDecisionFilterWhere("d", orgID, filters)

	q := fmt.Sprintf( //nolint:gosec // G201
		`SELECT d.id, d.run_id, d.agent_id, d.org_id, d.decision_type, d.outcome,
		        d.confidence, d.reasoning, d.metadata, d.completeness_score,
		        d.outcome_score, d.precedent_ref, d.precedent_reason, d.supersedes_id, d.content_hash,
		        d.valid_from, d.valid_to, d.transaction_time, d.created_at,
		        d.session_id, d.agent_context, d.api_key_id, d.tool, d.model, d.project,
		        1.0 AS relevance
		 FROM decisions d
		 WHERE d.org_id = ? AND d.valid_to IS NULL
		   AND (d.outcome LIKE ? OR COALESCE(d.reasoning, '') LIKE ? OR d.decision_type LIKE ?)
		   %s
		 ORDER BY d.valid_from DESC
		 LIMIT ?`,
		filterWhere,
	)

	args := []any{uuidStr(orgID), pattern, pattern, pattern}
	args = append(args, filterArgs...)
	args = append(args, limit)

	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: like search: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	results := make([]model.SearchResult, 0)
	for rows.Next() {
		var (
			d            model.Decision
			relevance    float32
			idStr        string
			runIDStr     string
			orgIDStr     string
			metaJSON     sql.NullString
			precedent    sql.NullString
			supersedes   sql.NullString
			validFromStr string
			validToStr   sql.NullString
			txTimeStr    string
			createdStr   string
			sessionStr   sql.NullString
			ctxJSON      sql.NullString
			apiKeyStr    sql.NullString
			tool         sql.NullString
			modelStr     sql.NullString
			project      sql.NullString
		)
		err := rows.Scan(&idStr, &runIDStr, &d.AgentID, &orgIDStr, &d.DecisionType,
			&d.Outcome, &d.Confidence, &d.Reasoning, &metaJSON, &d.CompletenessScore,
			&d.OutcomeScore, &precedent, &d.PrecedentReason, &supersedes, &d.ContentHash,
			&validFromStr, &validToStr, &txTimeStr, &createdStr,
			&sessionStr, &ctxJSON, &apiKeyStr, &tool, &modelStr, &project,
			&relevance)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan like result: %w", err)
		}
		d.ID = parseUUID(idStr)
		d.RunID = parseUUID(runIDStr)
		d.OrgID = parseUUID(orgIDStr)
		d.PrecedentRef = parseNullUUID(precedent)
		d.SupersedesID = parseNullUUID(supersedes)
		d.ValidFrom = parseTime(validFromStr)
		d.ValidTo = parseNullTime(validToStr)
		d.TransactionTime = parseTime(txTimeStr)
		d.CreatedAt = parseTime(createdStr)
		d.SessionID = parseNullUUID(sessionStr)
		d.APIKeyID = parseNullUUID(apiKeyStr)
		d.Metadata = map[string]any{}
		_ = scanJSON(metaJSON, &d.Metadata)
		d.AgentContext = map[string]any{}
		_ = scanJSON(ctxJSON, &d.AgentContext)
		if tool.Valid {
			d.Tool = &tool.String
		}
		if modelStr.Valid {
			d.Model = &modelStr.String
		}
		if project.Valid {
			d.Project = &project.String
		}
		results = append(results, model.SearchResult{
			Decision:        d,
			SimilarityScore: relevance,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: like rows: %w", err)
	}
	if len(results) == 0 {
		return []model.SearchResult{}, nil
	}
	return results, nil
}

// ---- WHERE clause builder ----

// buildDecisionWhere builds a WHERE clause for the decisions table (no alias).
func buildDecisionWhere(orgID uuid.UUID, f model.QueryFilters, traceID *string) (string, []any) {
	var conds []string
	var args []any

	conds = append(conds, "org_id = ?")
	args = append(args, uuidStr(orgID))

	conds = append(conds, "valid_to IS NULL")

	if len(f.AgentIDs) > 0 {
		conds = append(conds, fmt.Sprintf("agent_id IN (%s)", placeholders(len(f.AgentIDs))))
		for _, id := range f.AgentIDs {
			args = append(args, id)
		}
	}
	if f.RunID != nil {
		conds = append(conds, "run_id = ?")
		args = append(args, uuidStr(*f.RunID))
	}
	if f.DecisionType != nil {
		conds = append(conds, "decision_type = ?")
		args = append(args, *f.DecisionType)
	}
	if f.ConfidenceMin != nil {
		conds = append(conds, "confidence >= ?")
		args = append(args, *f.ConfidenceMin)
	}
	if f.Outcome != nil {
		conds = append(conds, "outcome = ?")
		args = append(args, *f.Outcome)
	}
	if f.SessionID != nil {
		conds = append(conds, "session_id = ?")
		args = append(args, uuidStr(*f.SessionID))
	}
	if f.Tool != nil {
		conds = append(conds, "tool = ?")
		args = append(args, *f.Tool)
	}
	if f.Model != nil {
		conds = append(conds, "model = ?")
		args = append(args, *f.Model)
	}
	if f.Project != nil {
		conds = append(conds, "project = ?")
		args = append(args, *f.Project)
	}
	if f.TimeRange != nil {
		if f.TimeRange.From != nil {
			conds = append(conds, "valid_from >= ?")
			args = append(args, timeStr(*f.TimeRange.From))
		}
		if f.TimeRange.To != nil {
			conds = append(conds, "valid_from <= ?")
			args = append(args, timeStr(*f.TimeRange.To))
		}
	}
	if traceID != nil {
		conds = append(conds, "run_id IN (SELECT id FROM agent_runs WHERE trace_id = ? AND org_id = ?)")
		args = append(args, *traceID, uuidStr(orgID))
	}

	return "WHERE " + strings.Join(conds, " AND "), args
}

// buildDecisionFilterWhere builds additional filter conditions for an aliased decisions table.
// Returns the extra AND clauses (without leading AND) and args.
func buildDecisionFilterWhere(alias string, orgID uuid.UUID, f model.QueryFilters) (string, []any) {
	var conds []string
	var args []any

	if len(f.AgentIDs) > 0 {
		conds = append(conds, fmt.Sprintf("%s.agent_id IN (%s)", alias, placeholders(len(f.AgentIDs))))
		for _, id := range f.AgentIDs {
			args = append(args, id)
		}
	}
	if f.DecisionType != nil {
		conds = append(conds, fmt.Sprintf("%s.decision_type = ?", alias))
		args = append(args, *f.DecisionType)
	}
	if f.ConfidenceMin != nil {
		conds = append(conds, fmt.Sprintf("%s.confidence >= ?", alias))
		args = append(args, *f.ConfidenceMin)
	}
	if f.Project != nil {
		conds = append(conds, fmt.Sprintf("%s.project = ?", alias))
		args = append(args, *f.Project)
	}
	if f.TimeRange != nil {
		if f.TimeRange.From != nil {
			conds = append(conds, fmt.Sprintf("%s.valid_from >= ?", alias))
			args = append(args, timeStr(*f.TimeRange.From))
		}
		if f.TimeRange.To != nil {
			conds = append(conds, fmt.Sprintf("%s.valid_from <= ?", alias))
			args = append(args, timeStr(*f.TimeRange.To))
		}
	}

	if len(conds) == 0 {
		return "", nil
	}
	return "AND " + strings.Join(conds, " AND "), args
}

// sanitizeOrderCol restricts order columns to known-safe values.
func sanitizeOrderCol(col string) string {
	switch strings.ToLower(col) {
	case "valid_from", "created_at", "confidence", "completeness_score", "outcome_score", "decision_type":
		return col
	default:
		return "valid_from"
	}
}

// ---- Row scanning ----

// scanDecisionRows scans multiple decision rows (25 columns matching decisionCols).
func scanDecisionRows(rows *sql.Rows) ([]model.Decision, error) {
	decisions := make([]model.Decision, 0)
	for rows.Next() {
		d, err := scanOneDecision(rows)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: decision rows: %w", err)
	}
	return decisions, nil
}

// scanDecisionRowsWithTotal scans rows that include a trailing COUNT(*) OVER()
// column, returning the decisions and the total count in a single pass.
func scanDecisionRowsWithTotal(rows *sql.Rows) ([]model.Decision, int, error) {
	decisions := make([]model.Decision, 0)
	var total int
	for rows.Next() {
		d, t, err := scanOneDecisionWithTotal(rows)
		if err != nil {
			return nil, 0, err
		}
		total = t
		decisions = append(decisions, d)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("sqlite: decision rows: %w", err)
	}
	return decisions, total, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanOneDecision scans the 25-column decisionCols from a row.
func scanOneDecision(row rowScanner) (model.Decision, error) {
	var (
		d            model.Decision
		idStr        string
		runIDStr     string
		orgIDStr     string
		metaJSON     sql.NullString
		precedent    sql.NullString
		supersedes   sql.NullString
		validFromStr string
		validToStr   sql.NullString
		txTimeStr    string
		createdStr   string
		sessionStr   sql.NullString
		ctxJSON      sql.NullString
		apiKeyStr    sql.NullString
		tool         sql.NullString
		modelStr     sql.NullString
		project      sql.NullString
	)
	err := row.Scan(&idStr, &runIDStr, &d.AgentID, &orgIDStr, &d.DecisionType,
		&d.Outcome, &d.Confidence, &d.Reasoning, &metaJSON, &d.CompletenessScore,
		&d.OutcomeScore, &precedent, &d.PrecedentReason, &supersedes, &d.ContentHash,
		&validFromStr, &validToStr, &txTimeStr, &createdStr,
		&sessionStr, &ctxJSON, &apiKeyStr, &tool, &modelStr, &project)
	if err != nil {
		return model.Decision{}, fmt.Errorf("sqlite: scan decision: %w", err)
	}

	d.ID = parseUUID(idStr)
	d.RunID = parseUUID(runIDStr)
	d.OrgID = parseUUID(orgIDStr)
	d.PrecedentRef = parseNullUUID(precedent)
	d.SupersedesID = parseNullUUID(supersedes)
	d.ValidFrom = parseTime(validFromStr)
	d.ValidTo = parseNullTime(validToStr)
	d.TransactionTime = parseTime(txTimeStr)
	d.CreatedAt = parseTime(createdStr)
	d.SessionID = parseNullUUID(sessionStr)
	d.APIKeyID = parseNullUUID(apiKeyStr)
	d.Metadata = map[string]any{}
	_ = scanJSON(metaJSON, &d.Metadata)
	d.AgentContext = map[string]any{}
	_ = scanJSON(ctxJSON, &d.AgentContext)
	if tool.Valid {
		d.Tool = &tool.String
	}
	if modelStr.Valid {
		d.Model = &modelStr.String
	}
	if project.Valid {
		d.Project = &project.String
	}
	return d, nil
}

// scanOneDecisionWithTotal scans the 25-column decisionCols plus a trailing
// COUNT(*) OVER() total from a row.
func scanOneDecisionWithTotal(row rowScanner) (model.Decision, int, error) {
	var (
		d            model.Decision
		total        int
		idStr        string
		runIDStr     string
		orgIDStr     string
		metaJSON     sql.NullString
		precedent    sql.NullString
		supersedes   sql.NullString
		validFromStr string
		validToStr   sql.NullString
		txTimeStr    string
		createdStr   string
		sessionStr   sql.NullString
		ctxJSON      sql.NullString
		apiKeyStr    sql.NullString
		tool         sql.NullString
		modelStr     sql.NullString
		project      sql.NullString
	)
	err := row.Scan(&idStr, &runIDStr, &d.AgentID, &orgIDStr, &d.DecisionType,
		&d.Outcome, &d.Confidence, &d.Reasoning, &metaJSON, &d.CompletenessScore,
		&d.OutcomeScore, &precedent, &d.PrecedentReason, &supersedes, &d.ContentHash,
		&validFromStr, &validToStr, &txTimeStr, &createdStr,
		&sessionStr, &ctxJSON, &apiKeyStr, &tool, &modelStr, &project,
		&total)
	if err != nil {
		return model.Decision{}, 0, fmt.Errorf("sqlite: scan decision with total: %w", err)
	}

	d.ID = parseUUID(idStr)
	d.RunID = parseUUID(runIDStr)
	d.OrgID = parseUUID(orgIDStr)
	d.PrecedentRef = parseNullUUID(precedent)
	d.SupersedesID = parseNullUUID(supersedes)
	d.ValidFrom = parseTime(validFromStr)
	d.ValidTo = parseNullTime(validToStr)
	d.TransactionTime = parseTime(txTimeStr)
	d.CreatedAt = parseTime(createdStr)
	d.SessionID = parseNullUUID(sessionStr)
	d.APIKeyID = parseNullUUID(apiKeyStr)
	d.Metadata = map[string]any{}
	_ = scanJSON(metaJSON, &d.Metadata)
	d.AgentContext = map[string]any{}
	_ = scanJSON(ctxJSON, &d.AgentContext)
	if tool.Valid {
		d.Tool = &tool.String
	}
	if modelStr.Valid {
		d.Model = &modelStr.String
	}
	if project.Valid {
		d.Project = &project.String
	}
	return d, total, nil
}

// ---- Batch loaders for alternatives and evidence ----

func (l *LiteDB) loadAlternatives(ctx context.Context, decisions []model.Decision) error {
	ids := make([]any, len(decisions))
	for i, d := range decisions {
		ids[i] = uuidStr(d.ID)
	}
	q := fmt.Sprintf( //nolint:gosec // G201
		`SELECT id, decision_id, label, rejection_reason, metadata, created_at
		 FROM alternatives WHERE decision_id IN (%s)`,
		placeholders(len(ids)),
	)
	rows, err := l.db.QueryContext(ctx, q, ids...)
	if err != nil {
		return fmt.Errorf("sqlite: load alternatives: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	byDecision := make(map[uuid.UUID][]model.Alternative)
	for rows.Next() {
		var (
			a          model.Alternative
			idStr      string
			decIDStr   string
			metaJSON   sql.NullString
			createdStr string
		)
		if err := rows.Scan(&idStr, &decIDStr, &a.Label,
			&a.RejectionReason, &metaJSON, &createdStr); err != nil {
			return fmt.Errorf("sqlite: scan alternative: %w", err)
		}
		a.ID = parseUUID(idStr)
		a.DecisionID = parseUUID(decIDStr)
		a.Metadata = map[string]any{}
		_ = scanJSON(metaJSON, &a.Metadata)
		a.CreatedAt = parseTime(createdStr)
		byDecision[a.DecisionID] = append(byDecision[a.DecisionID], a)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: alternatives rows: %w", err)
	}

	for i := range decisions {
		if alts, ok := byDecision[decisions[i].ID]; ok {
			decisions[i].Alternatives = alts
		}
	}
	return nil
}

func (l *LiteDB) loadEvidence(ctx context.Context, orgID uuid.UUID, decisions []model.Decision) error {
	ids := make([]any, len(decisions))
	for i, d := range decisions {
		ids[i] = uuidStr(d.ID)
	}
	q := fmt.Sprintf( //nolint:gosec // G201
		`SELECT id, decision_id, org_id, source_type, source_uri, content,
		        relevance_score, metadata, created_at
		 FROM evidence WHERE decision_id IN (%s) AND org_id = ?`,
		placeholders(len(ids)),
	)
	args := make([]any, len(ids)+1)
	copy(args, ids)
	args[len(ids)] = uuidStr(orgID)
	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("sqlite: load evidence: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	byDecision := make(map[uuid.UUID][]model.Evidence)
	for rows.Next() {
		var (
			e          model.Evidence
			idStr      string
			decIDStr   string
			orgIDStr   string
			sourceType string
			metaJSON   sql.NullString
			createdStr string
		)
		if err := rows.Scan(&idStr, &decIDStr, &orgIDStr, &sourceType,
			&e.SourceURI, &e.Content, &e.RelevanceScore, &metaJSON, &createdStr); err != nil {
			return fmt.Errorf("sqlite: scan evidence: %w", err)
		}
		e.ID = parseUUID(idStr)
		e.DecisionID = parseUUID(decIDStr)
		e.OrgID = parseUUID(orgIDStr)
		e.SourceType = model.SourceType(sourceType)
		e.Metadata = map[string]any{}
		_ = scanJSON(metaJSON, &e.Metadata)
		e.CreatedAt = parseTime(createdStr)
		byDecision[e.DecisionID] = append(byDecision[e.DecisionID], e)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: evidence rows: %w", err)
	}

	for i := range decisions {
		if evs, ok := byDecision[decisions[i].ID]; ok {
			decisions[i].Evidence = evs
		}
	}
	return nil
}
