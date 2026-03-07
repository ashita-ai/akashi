package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
)

// CreateAssessment records an outcome assessment for a decision.
func (l *LiteDB) CreateAssessment(ctx context.Context, orgID uuid.UUID, a model.DecisionAssessment) (model.DecisionAssessment, error) {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	row := l.db.QueryRowContext(ctx,
		`INSERT INTO decision_assessments (id, decision_id, org_id, assessor_agent_id, outcome, notes)
		 VALUES (?, ?, ?, ?, ?, ?)
		 RETURNING id, decision_id, org_id, assessor_agent_id, outcome, notes, created_at`,
		uuidStr(a.ID),
		uuidStr(a.DecisionID),
		uuidStr(orgID),
		a.AssessorAgentID,
		string(a.Outcome),
		a.Notes,
	)

	var (
		idStr        string
		decIDStr     string
		orgIDStr     string
		outcome      string
		notes        sql.NullString
		createdAtStr string
	)
	err := row.Scan(&idStr, &decIDStr, &orgIDStr, &a.AssessorAgentID, &outcome, &notes, &createdAtStr)
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
	a.CreatedAt = parseTime(createdAtStr)
	return a, nil
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
		     WHERE org_id = ? AND status IN ('resolved', 'wont_fix')
		       AND decision_a_id IN (SELECT value FROM json_each(?))
		     UNION ALL
		     SELECT decision_b_id AS target_id, winning_decision_id, status
		     FROM scored_conflicts
		     WHERE org_id = ? AND status IN ('resolved', 'wont_fix')
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
		     SUM(CASE WHEN status IN ('open','acknowledged') THEN 1 ELSE 0 END),
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
