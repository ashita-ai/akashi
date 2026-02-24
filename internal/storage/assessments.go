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

	var out model.DecisionAssessment
	err = db.pool.QueryRow(ctx, `
		INSERT INTO decision_assessments (decision_id, org_id, assessor_agent_id, outcome, notes)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, decision_id, org_id, assessor_agent_id, outcome, notes, created_at`,
		a.DecisionID, orgID, a.AssessorAgentID, string(a.Outcome), a.Notes,
	).Scan(
		&out.ID, &out.DecisionID, &out.OrgID, &out.AssessorAgentID,
		&out.Outcome, &out.Notes, &out.CreatedAt,
	)
	if err != nil {
		return model.DecisionAssessment{}, fmt.Errorf("storage: assess: insert: %w", err)
	}
	return out, nil
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
		SELECT id, decision_id, org_id, assessor_agent_id, outcome, notes, created_at
		FROM decision_assessments
		WHERE decision_id = $1
		ORDER BY created_at DESC`,
		decisionID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: list assessments: query: %w", err)
	}
	defer rows.Close()

	var out []model.DecisionAssessment
	for rows.Next() {
		var a model.DecisionAssessment
		if err := rows.Scan(
			&a.ID, &a.DecisionID, &a.OrgID, &a.AssessorAgentID,
			&a.Outcome, &a.Notes, &a.CreatedAt,
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
func (db *DB) GetAssessmentSummary(ctx context.Context, decisionID uuid.UUID) (model.AssessmentSummary, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT outcome, COUNT(*)
		FROM (
			SELECT DISTINCT ON (assessor_agent_id)
				outcome
			FROM decision_assessments
			WHERE decision_id = $1
			ORDER BY assessor_agent_id, created_at DESC
		) latest
		GROUP BY outcome`,
		decisionID,
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
func (db *DB) GetAssessmentSummaryBatch(ctx context.Context, decisionIDs []uuid.UUID) (map[uuid.UUID]model.AssessmentSummary, error) {
	if len(decisionIDs) == 0 {
		return nil, nil
	}
	rows, err := db.pool.Query(ctx, `
		SELECT decision_id, outcome, COUNT(*)
		FROM (
			SELECT DISTINCT ON (decision_id, assessor_agent_id)
				decision_id, outcome
			FROM decision_assessments
			WHERE decision_id = ANY($1)
			ORDER BY decision_id, assessor_agent_id, created_at DESC
		) latest
		GROUP BY decision_id, outcome`,
		decisionIDs,
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
