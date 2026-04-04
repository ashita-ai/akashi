//go:build !lite

package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
)

// CreateAssessment appends an outcome assessment for a decision.
// Append-only: each call creates a new row regardless of prior assessments
// from the same assessor. An assessor changing their verdict is itself an
// auditable event — we never overwrite prior rows.
// Returns ErrNotFound if decision_id does not exist in the org.
func (db *DB) CreateAssessment(ctx context.Context, orgID uuid.UUID, a model.DecisionAssessment) (model.DecisionAssessment, error) {
	// Verify the decision belongs to the org before inserting.
	var exists bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM decisions WHERE id = $1 AND org_id = $2 AND valid_to IS NULL)`,
		a.DecisionID, orgID,
	).Scan(&exists)
	if err != nil {
		return model.DecisionAssessment{}, fmt.Errorf("storage: assess: verify decision: %w", err)
	}
	if !exists {
		return model.DecisionAssessment{}, ErrNotFound
	}

	source := a.Source
	if source == "" {
		source = model.AssessmentSourceManual
	}
	var out model.DecisionAssessment
	err = db.pool.QueryRow(ctx, `
		INSERT INTO decision_assessments (decision_id, org_id, assessor_agent_id, outcome, notes, source)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, decision_id, org_id, assessor_agent_id, outcome, notes, source, created_at`,
		a.DecisionID, orgID, a.AssessorAgentID, string(a.Outcome), a.Notes, source,
	).Scan(
		&out.ID, &out.DecisionID, &out.OrgID, &out.AssessorAgentID,
		&out.Outcome, &out.Notes, &out.Source, &out.CreatedAt,
	)
	if err != nil {
		return model.DecisionAssessment{}, fmt.Errorf("storage: assess: insert: %w", err)
	}
	return out, nil
}

// UpdateOutcomeScore sets the outcome_score on a decision row.
// Called after recording an assessment to reflect the latest ground-truth feedback.
func (db *DB) UpdateOutcomeScore(ctx context.Context, orgID, decisionID uuid.UUID, score *float32) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE decisions SET outcome_score = $1 WHERE id = $2 AND org_id = $3 AND valid_to IS NULL`,
		score, decisionID, orgID,
	)
	if err != nil {
		return fmt.Errorf("storage: update outcome score: %w", err)
	}
	return nil
}

// ListAssessments returns the full assessment history for a decision, newest first.
// Multiple rows from the same assessor reflect verdict changes over time.
// Returns ErrNotFound if the decision does not exist in the org.
func (db *DB) ListAssessments(ctx context.Context, orgID, decisionID uuid.UUID) ([]model.DecisionAssessment, error) {
	// Verify org ownership first.
	var exists bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM decisions WHERE id = $1 AND org_id = $2 AND valid_to IS NULL)`,
		decisionID, orgID,
	).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("storage: list assessments: verify decision: %w", err)
	}
	if !exists {
		return nil, ErrNotFound
	}

	rows, err := db.pool.Query(ctx, `
		SELECT id, decision_id, org_id, assessor_agent_id, outcome, notes, source, created_at
		FROM decision_assessments
		WHERE decision_id = $1 AND org_id = $2
		ORDER BY created_at DESC`,
		decisionID, orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: list assessments: query: %w", err)
	}
	defer rows.Close()

	out := make([]model.DecisionAssessment, 0)
	for rows.Next() {
		var a model.DecisionAssessment
		if err := rows.Scan(
			&a.ID, &a.DecisionID, &a.OrgID, &a.AssessorAgentID,
			&a.Outcome, &a.Notes, &a.Source, &a.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: list assessments: scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list assessments: rows: %w", err)
	}
	return out, nil
}

// GetAssessmentSummary returns aggregated outcome counts for a decision,
// counting only the latest assessment from each assessor (DISTINCT ON).
// An assessor who changed their verdict from "correct" to "incorrect"
// counts as one "incorrect" in the summary; the history is preserved in
// ListAssessments but does not skew the current-state count.
// Returns a zero-value summary (all counts 0) if no assessments exist.
func (db *DB) GetAssessmentSummary(ctx context.Context, orgID, decisionID uuid.UUID) (model.AssessmentSummary, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT outcome, COUNT(*)
		FROM (
			SELECT DISTINCT ON (assessor_agent_id)
				outcome
			FROM decision_assessments
			WHERE decision_id = $1 AND org_id = $2
			ORDER BY assessor_agent_id, created_at DESC
		) latest
		GROUP BY outcome`,
		decisionID, orgID,
	)
	if err != nil {
		return model.AssessmentSummary{}, fmt.Errorf("storage: assessment summary: %w", err)
	}
	defer rows.Close()

	var s model.AssessmentSummary
	for rows.Next() {
		var outcome string
		var count int
		if err := rows.Scan(&outcome, &count); err != nil {
			return model.AssessmentSummary{}, fmt.Errorf("storage: assessment summary: scan: %w", err)
		}
		s.Total += count
		switch model.AssessmentOutcome(outcome) {
		case model.AssessmentCorrect:
			s.Correct = count
		case model.AssessmentIncorrect:
			s.Incorrect = count
		case model.AssessmentPartiallyCorrect:
			s.PartiallyCorrect = count
		}
	}
	return s, rows.Err()
}

// GetAssessmentSummaryBatch returns latest-per-assessor outcome counts for
// multiple decisions. Decisions with no assessments are omitted from the map.
func (db *DB) GetAssessmentSummaryBatch(ctx context.Context, orgID uuid.UUID, decisionIDs []uuid.UUID) (map[uuid.UUID]model.AssessmentSummary, error) {
	if len(decisionIDs) == 0 {
		return nil, nil
	}
	rows, err := db.pool.Query(ctx, `
		SELECT decision_id, outcome, COUNT(*)
		FROM (
			SELECT DISTINCT ON (decision_id, assessor_agent_id)
				decision_id, outcome
			FROM decision_assessments
			WHERE decision_id = ANY($1) AND org_id = $2
			ORDER BY decision_id, assessor_agent_id, created_at DESC
		) latest
		GROUP BY decision_id, outcome`,
		decisionIDs, orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: assessment summary batch: %w", err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID]model.AssessmentSummary)
	for rows.Next() {
		var id uuid.UUID
		var outcome string
		var count int
		if err := rows.Scan(&id, &outcome, &count); err != nil {
			return nil, fmt.Errorf("storage: assessment summary batch: scan: %w", err)
		}
		s := out[id]
		s.Total += count
		switch model.AssessmentOutcome(outcome) {
		case model.AssessmentCorrect:
			s.Correct = count
		case model.AssessmentIncorrect:
			s.Incorrect = count
		case model.AssessmentPartiallyCorrect:
			s.PartiallyCorrect = count
		}
		out[id] = s
	}
	return out, rows.Err()
}

// GetPrecedentCitationCount returns the number of active decisions that cite
// the given decision as a precedent (precedent_ref = decisionID).
func (db *DB) GetPrecedentCitationCount(ctx context.Context, orgID uuid.UUID, decisionID uuid.UUID) (int, error) {
	var count int
	err := db.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM decisions
		 WHERE precedent_ref = $1 AND org_id = $2 AND valid_to IS NULL`,
		decisionID, orgID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("storage: precedent citation count: %w", err)
	}
	return count, nil
}

// HasAssessmentFromSource returns true if an assessment from the given source
// already exists for this decision. Used for idempotency in auto-assessment.
func (db *DB) HasAssessmentFromSource(ctx context.Context, orgID, decisionID uuid.UUID, source string) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM decision_assessments
			WHERE decision_id = $1 AND org_id = $2 AND source = $3
		)`,
		decisionID, orgID, source,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("storage: has assessment from source: %w", err)
	}
	return exists, nil
}
