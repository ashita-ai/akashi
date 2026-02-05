package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/akashi/internal/model"
)

// CreateAlternative inserts a single alternative for a decision.
func (db *DB) CreateAlternative(ctx context.Context, alt model.Alternative) (model.Alternative, error) {
	if alt.ID == uuid.Nil {
		alt.ID = uuid.New()
	}
	if alt.CreatedAt.IsZero() {
		alt.CreatedAt = time.Now().UTC()
	}
	if alt.Metadata == nil {
		alt.Metadata = map[string]any{}
	}

	_, err := db.pool.Exec(ctx,
		`INSERT INTO alternatives (id, decision_id, label, score, selected, rejection_reason, metadata, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		alt.ID, alt.DecisionID, alt.Label, alt.Score, alt.Selected,
		alt.RejectionReason, alt.Metadata, alt.CreatedAt,
	)
	if err != nil {
		return model.Alternative{}, fmt.Errorf("storage: create alternative: %w", err)
	}
	return alt, nil
}

// CreateAlternativesBatch inserts multiple alternatives using COPY.
func (db *DB) CreateAlternativesBatch(ctx context.Context, alts []model.Alternative) error {
	if len(alts) == 0 {
		return nil
	}

	columns := []string{"id", "decision_id", "label", "score", "selected", "rejection_reason", "metadata", "created_at"}
	rows := make([][]any, len(alts))
	for i, a := range alts {
		id := a.ID
		if id == uuid.Nil {
			id = uuid.New()
		}
		createdAt := a.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		meta := a.Metadata
		if meta == nil {
			meta = map[string]any{}
		}
		rows[i] = []any{id, a.DecisionID, a.Label, a.Score, a.Selected, a.RejectionReason, meta, createdAt}
	}

	_, err := db.pool.CopyFrom(ctx, pgx.Identifier{"alternatives"}, columns, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("storage: copy alternatives: %w", err)
	}
	return nil
}

// GetAlternativesByDecisions retrieves all alternatives for a set of decision IDs in a single query.
// Results are returned as a map from decision ID to its alternatives.
func (db *DB) GetAlternativesByDecisions(ctx context.Context, decisionIDs []uuid.UUID) (map[uuid.UUID][]model.Alternative, error) {
	if len(decisionIDs) == 0 {
		return nil, nil
	}

	rows, err := db.pool.Query(ctx,
		`SELECT id, decision_id, label, score, selected, rejection_reason, metadata, created_at
		 FROM alternatives WHERE decision_id = ANY($1)
		 ORDER BY score DESC NULLS LAST`, decisionIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get alternatives batch: %w", err)
	}
	defer rows.Close()

	result := make(map[uuid.UUID][]model.Alternative)
	for rows.Next() {
		var a model.Alternative
		if err := rows.Scan(
			&a.ID, &a.DecisionID, &a.Label, &a.Score, &a.Selected,
			&a.RejectionReason, &a.Metadata, &a.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan alternative: %w", err)
		}
		result[a.DecisionID] = append(result[a.DecisionID], a)
	}
	return result, rows.Err()
}

// GetAlternativesByDecision retrieves all alternatives for a decision.
func (db *DB) GetAlternativesByDecision(ctx context.Context, decisionID uuid.UUID) ([]model.Alternative, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, decision_id, label, score, selected, rejection_reason, metadata, created_at
		 FROM alternatives WHERE decision_id = $1
		 ORDER BY score DESC NULLS LAST`, decisionID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get alternatives: %w", err)
	}
	defer rows.Close()

	var alts []model.Alternative
	for rows.Next() {
		var a model.Alternative
		if err := rows.Scan(
			&a.ID, &a.DecisionID, &a.Label, &a.Score, &a.Selected,
			&a.RejectionReason, &a.Metadata, &a.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan alternative: %w", err)
		}
		alts = append(alts, a)
	}
	return alts, rows.Err()
}
