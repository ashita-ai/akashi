package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// CreateAssessment records an outcome assessment for a decision.
func (l *LiteDB) CreateAssessment(ctx context.Context, orgID uuid.UUID, a model.DecisionAssessment) (model.DecisionAssessment, error) {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	source := a.Source
	if source == "" {
		source = model.AssessmentSourceManual
	}
	row := l.db.QueryRowContext(ctx,
		`INSERT INTO decision_assessments (id, decision_id, org_id, assessor_agent_id, outcome, notes, source)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 RETURNING id, decision_id, org_id, assessor_agent_id, outcome, notes, source, created_at`,
		uuidStr(a.ID),
		uuidStr(a.DecisionID),
		uuidStr(orgID),
		a.AssessorAgentID,
		string(a.Outcome),
		a.Notes,
		source,
	)

	var (
		idStr        string
		decIDStr     string
		orgIDStr     string
		outcome      string
		notes        sql.NullString
		sourceStr    string
		createdAtStr string
	)
	err := row.Scan(&idStr, &decIDStr, &orgIDStr, &a.AssessorAgentID, &outcome, &notes, &sourceStr, &createdAtStr)
	if err != nil {
		return model.DecisionAssessment{}, fmt.Errorf("sqlite: create assessment: %w", err)
	}

	a.ID = parseUUID(idStr)
	a.DecisionID = parseUUID(decIDStr)
	a.OrgID = parseUUID(orgIDStr)
	a.Outcome = model.AssessmentOutcome(outcome)
	if notes.Valid {
		a.Notes = &notes.String
	}
	a.Source = sourceStr
	a.CreatedAt = parseTime(createdAtStr)
	return a, nil
}

// UpdateOutcomeScore sets the outcome_score on a decision row.
func (l *LiteDB) UpdateOutcomeScore(ctx context.Context, orgID, decisionID uuid.UUID, score *float32) error {
	_, err := l.db.ExecContext(ctx,
		`UPDATE decisions SET outcome_score = ? WHERE id = ? AND org_id = ? AND valid_to IS NULL`,
		score, uuidStr(decisionID), uuidStr(orgID),
	)
	if err != nil {
		return fmt.Errorf("sqlite: update outcome score: %w", err)
	}
	return nil
}

// GetAssessmentSummaryBatch returns assessment counts per decision.
func (l *LiteDB) GetAssessmentSummaryBatch(ctx context.Context, orgID uuid.UUID, decisionIDs []uuid.UUID) (map[uuid.UUID]model.AssessmentSummary, error) {
	if len(decisionIDs) == 0 {
		return map[uuid.UUID]model.AssessmentSummary{}, nil
	}
	idsJSON := uuidSliceToJSON(decisionIDs)

	// Use ROW_NUMBER to emulate DISTINCT ON (latest assessment per decision+assessor).
	rows, err := l.db.QueryContext(ctx,
		`SELECT decision_id, outcome, COUNT(*) FROM (
		     SELECT decision_id, outcome,
		            ROW_NUMBER() OVER (PARTITION BY decision_id, assessor_agent_id ORDER BY created_at DESC) AS rn
		     FROM decision_assessments
		     WHERE decision_id IN (SELECT value FROM json_each(?)) AND org_id = ?
		 ) WHERE rn = 1
		 GROUP BY decision_id, outcome`,
		idsJSON, uuidStr(orgID),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: get assessment summary batch: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	result := make(map[uuid.UUID]model.AssessmentSummary, len(decisionIDs))
	for rows.Next() {
		var (
			decIDStr string
			outcome  string
			count    int
		)
		if err := rows.Scan(&decIDStr, &outcome, &count); err != nil {
			return nil, fmt.Errorf("sqlite: scan assessment summary: %w", err)
		}
		decID := parseUUID(decIDStr)
		s := result[decID]
		s.Total += count
		switch model.AssessmentOutcome(outcome) {
		case model.AssessmentCorrect:
			s.Correct += count
		case model.AssessmentIncorrect:
			s.Incorrect += count
		case model.AssessmentPartiallyCorrect:
			s.PartiallyCorrect += count
		}
		result[decID] = s
	}
	return result, rows.Err()
}

// GetDecisionOutcomeSignalsBatch returns outcome signals for multiple decisions.
func (l *LiteDB) GetDecisionOutcomeSignalsBatch(ctx context.Context, ids []uuid.UUID, orgID uuid.UUID) (map[uuid.UUID]model.OutcomeSignals, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]model.OutcomeSignals{}, nil
	}
	idsJSON := uuidSliceToJSON(ids)
	result := make(map[uuid.UUID]model.OutcomeSignals, len(ids))

	// Initialize all requested IDs with zero signals.
	for _, id := range ids {
		result[id] = model.OutcomeSignals{}
	}

	// 1. Supersession velocity: hours between decision and its supersession.
	sRows, err := l.db.QueryContext(ctx,
		`SELECT d.id,
		        (julianday(s.valid_from) - julianday(d.valid_from)) * 24.0 AS hours
		 FROM decisions d
		 JOIN decisions s ON s.supersedes_id = d.id AND s.org_id = d.org_id
		 WHERE d.id IN (SELECT value FROM json_each(?)) AND d.org_id = ?`,
		idsJSON, uuidStr(orgID),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: supersession velocity: %w", err)
	}
	defer sRows.Close() //nolint:errcheck
	for sRows.Next() {
		var idStr string
		var hours float64
		if err := sRows.Scan(&idStr, &hours); err != nil {
			return nil, fmt.Errorf("sqlite: scan supersession: %w", err)
		}
		id := parseUUID(idStr)
		sig := result[id]
		sig.SupersessionVelocityHours = &hours
		result[id] = sig
	}
	if err := sRows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: supersession rows: %w", err)
	}

	// 2. Precedent citations.
	pRows, err := l.db.QueryContext(ctx,
		`SELECT precedent_ref, COUNT(*) FROM decisions
		 WHERE precedent_ref IN (SELECT value FROM json_each(?))
		   AND org_id = ? AND valid_to IS NULL
		 GROUP BY precedent_ref`,
		idsJSON, uuidStr(orgID),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: precedent citations: %w", err)
	}
	defer pRows.Close() //nolint:errcheck
	for pRows.Next() {
		var refStr string
		var count int
		if err := pRows.Scan(&refStr, &count); err != nil {
			return nil, fmt.Errorf("sqlite: scan citation: %w", err)
		}
		id := parseUUID(refStr)
		sig := result[id]
		sig.PrecedentCitationCount = count
		result[id] = sig
	}
	if err := pRows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: citation rows: %w", err)
	}

	// 3. Conflict fate.
	cRows, err := l.db.QueryContext(ctx,
		`WITH sides AS (
		     SELECT decision_a_id AS target_id, winning_decision_id, status
		     FROM scored_conflicts
		     WHERE org_id = ? AND status IN ('resolved', 'false_positive')
		       AND decision_a_id IN (SELECT value FROM json_each(?))
		     UNION ALL
		     SELECT decision_b_id AS target_id, winning_decision_id, status
		     FROM scored_conflicts
		     WHERE org_id = ? AND status IN ('resolved', 'false_positive')
		       AND decision_b_id IN (SELECT value FROM json_each(?))
		 )
		 SELECT target_id,
		     SUM(CASE WHEN winning_decision_id = target_id THEN 1 ELSE 0 END),
		     SUM(CASE WHEN winning_decision_id IS NOT NULL AND winning_decision_id != target_id THEN 1 ELSE 0 END),
		     SUM(CASE WHEN winning_decision_id IS NULL AND status = 'resolved' THEN 1 ELSE 0 END)
		 FROM sides
		 GROUP BY target_id`,
		uuidStr(orgID), idsJSON, uuidStr(orgID), idsJSON,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: conflict fate: %w", err)
	}
	defer cRows.Close() //nolint:errcheck
	for cRows.Next() {
		var idStr string
		var won, lost, noWinner int
		if err := cRows.Scan(&idStr, &won, &lost, &noWinner); err != nil {
			return nil, fmt.Errorf("sqlite: scan conflict fate: %w", err)
		}
		id := parseUUID(idStr)
		sig := result[id]
		sig.ConflictFate = model.ConflictFate{Won: won, Lost: lost, ResolvedNoWinner: noWinner}
		result[id] = sig
	}
	if err := cRows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: conflict fate rows: %w", err)
	}

	// 4. Agreement and conflict counts.
	acRows, err := l.db.QueryContext(ctx,
		`WITH sides AS (
		     SELECT decision_a_id AS target_id, relationship, status FROM scored_conflicts
		     WHERE org_id = ? AND decision_a_id IN (SELECT value FROM json_each(?))
		     UNION ALL
		     SELECT decision_b_id AS target_id, relationship, status FROM scored_conflicts
		     WHERE org_id = ? AND decision_b_id IN (SELECT value FROM json_each(?))
		 )
		 SELECT target_id,
		     SUM(CASE WHEN status = 'open' THEN 1 ELSE 0 END),
		     SUM(CASE WHEN relationship = 'complementary' THEN 1 ELSE 0 END)
		 FROM sides GROUP BY target_id`,
		uuidStr(orgID), idsJSON, uuidStr(orgID), idsJSON,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: agreement/conflict counts: %w", err)
	}
	defer acRows.Close() //nolint:errcheck
	for acRows.Next() {
		var idStr string
		var conflictCount, agreementCount int
		if err := acRows.Scan(&idStr, &conflictCount, &agreementCount); err != nil {
			return nil, fmt.Errorf("sqlite: scan agreement/conflict: %w", err)
		}
		id := parseUUID(idStr)
		sig := result[id]
		sig.ConflictCount = conflictCount
		sig.AgreementCount = agreementCount
		result[id] = sig
	}
	return result, acRows.Err()
}

// GetAssessmentSummary returns aggregated outcome counts for a decision,
// counting only the latest assessment from each assessor.
func (l *LiteDB) GetAssessmentSummary(ctx context.Context, orgID, decisionID uuid.UUID) (model.AssessmentSummary, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT outcome, COUNT(*) FROM (
		     SELECT outcome,
		            ROW_NUMBER() OVER (PARTITION BY assessor_agent_id ORDER BY created_at DESC) AS rn
		     FROM decision_assessments
		     WHERE decision_id = ? AND org_id = ?
		 ) WHERE rn = 1
		 GROUP BY outcome`,
		uuidStr(decisionID), uuidStr(orgID),
	)
	if err != nil {
		return model.AssessmentSummary{}, fmt.Errorf("sqlite: get assessment summary: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var s model.AssessmentSummary
	for rows.Next() {
		var outcome string
		var count int
		if err := rows.Scan(&outcome, &count); err != nil {
			return model.AssessmentSummary{}, fmt.Errorf("sqlite: scan assessment summary: %w", err)
		}
		s.Total += count
		switch model.AssessmentOutcome(outcome) {
		case model.AssessmentCorrect:
			s.Correct += count
		case model.AssessmentIncorrect:
			s.Incorrect += count
		case model.AssessmentPartiallyCorrect:
			s.PartiallyCorrect += count
		}
	}
	return s, rows.Err()
}

// GetPrecedentCitationCount returns the number of active decisions that cite
// the given decision as a precedent.
func (l *LiteDB) GetPrecedentCitationCount(ctx context.Context, orgID uuid.UUID, decisionID uuid.UUID) (int, error) {
	var count int
	err := l.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM decisions
		 WHERE precedent_ref = ? AND org_id = ? AND valid_to IS NULL`,
		uuidStr(decisionID), uuidStr(orgID),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("sqlite: precedent citation count: %w", err)
	}
	return count, nil
}

// ListPendingAssessments returns active decisions whose valid_from is older
// than their per-type assessment window AND have no recorded assessment from
// any source. Mirrors the PostgreSQL implementation; see its doc comment for
// the full design rationale.
func (l *LiteDB) ListPendingAssessments(ctx context.Context, orgID uuid.UUID, opts storage.ListPendingAssessmentsOpts) ([]model.PendingAssessment, error) {
	if len(opts.Windows) == 0 {
		return nil, nil
	}
	if opts.AgentIDs != nil && len(opts.AgentIDs) == 0 {
		return nil, nil
	}
	if opts.Limit <= 0 {
		opts.Limit = 10
	}

	// Build dynamic VALUES (?, ?), (?, ?), ... for the windows CTE.
	values := make([]string, len(opts.Windows))
	args := make([]any, 0, len(opts.Windows)*2+4)
	for i, w := range opts.Windows {
		values[i] = "(?, ?)"
		args = append(args, w.DecisionType, w.Cutoff.UTC().Format(time.RFC3339Nano))
	}
	args = append(args, uuidStr(orgID))

	// The interpolated segment is literal "(?, ?)" placeholders only — never
	// user-controlled text. Every (decision_type, cutoff) value goes through
	// the parameterized args slice. SQLite has no array binding equivalent
	// to Postgres' unnest(), so a dynamic VALUES list is the standard pattern.
	q := `WITH windows(decision_type, cutoff) AS (VALUES ` + //nolint:gosec // G202: only "(?, ?)" placeholders are concatenated; values flow through args
		strings.Join(values, ", ") + `)
		SELECT d.id, d.agent_id, d.decision_type, d.outcome, d.confidence,
		       d.project, d.valid_from
		FROM decisions d
		JOIN windows w ON w.decision_type = d.decision_type
		             AND datetime(d.valid_from) < datetime(w.cutoff)
		LEFT JOIN decision_assessments a
		    ON a.decision_id = d.id AND a.org_id = d.org_id
		WHERE d.org_id = ?
		  AND d.valid_to IS NULL
		  AND a.id IS NULL`

	if opts.Project != nil {
		q += " AND d.project = ?"
		args = append(args, *opts.Project)
	}
	if opts.AgentIDs != nil {
		// json_each is the standard SQLite pattern for IN (?,?,...) with a
		// variable-length set — also used by GetAssessmentSummaryBatch and
		// GetDecisionsByIDs in this package.
		idsJSON, err := json.Marshal(opts.AgentIDs)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list pending assessments: marshal agent_ids: %w", err)
		}
		q += " AND d.agent_id IN (SELECT value FROM json_each(?))"
		args = append(args, string(idsJSON))
	}
	q += " ORDER BY datetime(d.valid_from) ASC LIMIT ?"
	args = append(args, opts.Limit)

	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list pending assessments: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	now := time.Now().UTC()
	out := make([]model.PendingAssessment, 0)
	for rows.Next() {
		var (
			idStr        string
			agentID      string
			decisionType string
			outcome      string
			confidence   float32
			project      sql.NullString
			validFromStr string
		)
		if err := rows.Scan(&idStr, &agentID, &decisionType, &outcome, &confidence, &project, &validFromStr); err != nil {
			return nil, fmt.Errorf("sqlite: list pending assessments: scan: %w", err)
		}
		p := model.PendingAssessment{
			DecisionID:   parseUUID(idStr),
			AgentID:      agentID,
			DecisionType: decisionType,
			Outcome:      outcome,
			Confidence:   confidence,
			ValidFrom:    parseTime(validFromStr),
		}
		if project.Valid {
			s := project.String
			p.Project = &s
		}
		p.AgeHours = now.Sub(p.ValidFrom).Hours()
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list pending assessments: rows: %w", err)
	}
	return out, nil
}

// HasAssessmentFromSource returns true if an assessment from the given source
// already exists for this decision.
func (l *LiteDB) HasAssessmentFromSource(ctx context.Context, orgID, decisionID uuid.UUID, source string) (bool, error) {
	var exists bool
	err := l.db.QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM decision_assessments
			WHERE decision_id = ? AND org_id = ? AND source = ?
		)`,
		uuidStr(decisionID), uuidStr(orgID), source,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("sqlite: has assessment from source: %w", err)
	}
	return exists, nil
}
