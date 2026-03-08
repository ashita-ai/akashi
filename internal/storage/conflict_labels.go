//go:build !lite

package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ConflictLabel represents a ground truth label for a scored conflict.
type ConflictLabel struct {
	ScoredConflictID uuid.UUID
	OrgID            uuid.UUID
	Label            string // "genuine", "related_not_contradicting", "unrelated_false_positive"
	LabeledBy        string
	LabeledAt        time.Time
	Notes            *string
}

// UpsertConflictLabel inserts or updates a ground truth label for a scored conflict.
// Returns pgx.ErrNoRows if the scored_conflict_id exists but belongs to a different org.
func (db *DB) UpsertConflictLabel(ctx context.Context, cl ConflictLabel) error {
	tag, err := db.pool.Exec(ctx, `
		INSERT INTO conflict_labels (scored_conflict_id, org_id, label, labeled_by, labeled_at, notes)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (scored_conflict_id)
		DO UPDATE SET label = EXCLUDED.label,
		              labeled_by = EXCLUDED.labeled_by,
		              labeled_at = EXCLUDED.labeled_at,
		              notes = EXCLUDED.notes
		WHERE conflict_labels.org_id = EXCLUDED.org_id`,
		cl.ScoredConflictID, cl.OrgID, cl.Label, cl.LabeledBy, cl.LabeledAt, cl.Notes)
	if err != nil {
		return fmt.Errorf("storage: upsert conflict label: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// GetConflictLabel returns the label for a single scored conflict.
func (db *DB) GetConflictLabel(ctx context.Context, conflictID, orgID uuid.UUID) (ConflictLabel, error) {
	var cl ConflictLabel
	err := db.pool.QueryRow(ctx, `
		SELECT scored_conflict_id, org_id, label, labeled_by, labeled_at, notes
		FROM conflict_labels
		WHERE scored_conflict_id = $1 AND org_id = $2`, conflictID, orgID).
		Scan(&cl.ScoredConflictID, &cl.OrgID, &cl.Label, &cl.LabeledBy, &cl.LabeledAt, &cl.Notes)
	if err != nil {
		return cl, fmt.Errorf("storage: get conflict label: %w", err)
	}
	return cl, nil
}

// ListConflictLabels returns all ground truth labels for an org.
func (db *DB) ListConflictLabels(ctx context.Context, orgID uuid.UUID) ([]ConflictLabel, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT scored_conflict_id, org_id, label, labeled_by, labeled_at, notes
		FROM conflict_labels
		WHERE org_id = $1
		ORDER BY labeled_at DESC`, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: list conflict labels: %w", err)
	}
	defer rows.Close()

	var labels []ConflictLabel
	for rows.Next() {
		var cl ConflictLabel
		if err := rows.Scan(&cl.ScoredConflictID, &cl.OrgID, &cl.Label, &cl.LabeledBy, &cl.LabeledAt, &cl.Notes); err != nil {
			return nil, fmt.Errorf("storage: scan conflict label: %w", err)
		}
		labels = append(labels, cl)
	}
	return labels, rows.Err()
}

// ConflictLabelCounts holds label distribution for precision/recall computation.
type ConflictLabelCounts struct {
	Genuine                 int
	RelatedNotContradicting int
	UnrelatedFalsePositive  int
	Total                   int
}

// GetConflictLabelCounts returns the label distribution for an org.
func (db *DB) GetConflictLabelCounts(ctx context.Context, orgID uuid.UUID) (ConflictLabelCounts, error) {
	var c ConflictLabelCounts
	err := db.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE label = 'genuine'),
		       count(*) FILTER (WHERE label = 'related_not_contradicting'),
		       count(*) FILTER (WHERE label = 'unrelated_false_positive')
		FROM conflict_labels
		WHERE org_id = $1`, orgID).Scan(&c.Total, &c.Genuine, &c.RelatedNotContradicting, &c.UnrelatedFalsePositive)
	if err != nil {
		return c, fmt.Errorf("storage: conflict label counts: %w", err)
	}
	return c, nil
}

// DeleteConflictLabel removes a label. Used for re-labeling workflows.
func (db *DB) DeleteConflictLabel(ctx context.Context, conflictID, orgID uuid.UUID) error {
	tag, err := db.pool.Exec(ctx,
		`DELETE FROM conflict_labels WHERE scored_conflict_id = $1 AND org_id = $2`,
		conflictID, orgID)
	if err != nil {
		return fmt.Errorf("storage: delete conflict label: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
