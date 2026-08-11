//go:build !lite

package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SuppressedPairSample is one sampled record of a pair a structural rule
// dropped before LLM validation. See migration 111 for why these live in a
// dedicated table rather than as sampled scored_conflicts rows.
type SuppressedPairSample struct {
	DecisionAID uuid.UUID
	DecisionBID uuid.UUID
	Rule        string
}

// InsertSuppressedPairSamples records sampled suppressions for one org.
// Pair IDs must already be in canonical order (A < B); the table enforces it.
// Re-sampling the same pair on a later rescore is expected and is a no-op.
func (db *DB) InsertSuppressedPairSamples(ctx context.Context, orgID uuid.UUID, samples []SuppressedPairSample) error {
	if len(samples) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, s := range samples {
		batch.Queue(
			`INSERT INTO suppressed_pair_samples (org_id, decision_a_id, decision_b_id, rule)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (decision_a_id, decision_b_id, rule) DO NOTHING`,
			orgID, s.DecisionAID, s.DecisionBID, s.Rule)
	}
	res := db.pool.SendBatch(ctx, batch)
	for range samples {
		if _, err := res.Exec(); err != nil {
			// Close still has to run to release the connection, but its error
			// is the same one we are already returning.
			_ = res.Close()
			return fmt.Errorf("storage: insert suppressed pair samples: %w", err)
		}
	}
	// Close reports errors that no individual Exec surfaced. Returning it keeps
	// a partially-applied batch loud rather than silently short.
	if err := res.Close(); err != nil {
		return fmt.Errorf("storage: close suppressed pair sample batch: %w", err)
	}
	return nil
}

// ListSuppressedPairSamples returns sampled suppressions for one org, newest
// first, optionally restricted to a single rule (empty string = all rules).
func (db *DB) ListSuppressedPairSamples(ctx context.Context, orgID uuid.UUID, rule string, limit int) ([]SuppressedPairSample, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.pool.Query(ctx,
		`SELECT decision_a_id, decision_b_id, rule
		   FROM suppressed_pair_samples
		  WHERE org_id = $1 AND ($2 = '' OR rule = $2)
		  ORDER BY sampled_at DESC
		  LIMIT $3`, orgID, rule, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: list suppressed pair samples: %w", err)
	}
	defer rows.Close()

	var out []SuppressedPairSample
	for rows.Next() {
		var s SuppressedPairSample
		if err := rows.Scan(&s.DecisionAID, &s.DecisionBID, &s.Rule); err != nil {
			return nil, fmt.Errorf("storage: scan suppressed pair sample: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate suppressed pair samples: %w", err)
	}
	return out, nil
}
