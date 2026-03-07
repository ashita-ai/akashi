//go:build !lite

package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// GetOutcomeSignalsSummary returns org-level aggregate outcome signal counts
// for use in GET /v1/trace-health.
func (db *DB) GetOutcomeSignalsSummary(ctx context.Context, orgID uuid.UUID) (OutcomeSignalsSummary, error) {
	var s OutcomeSignalsSummary

	// Decision-level counts: total, never_superseded, revised_within_48h, citation coverage.
	// Uses EXISTS subqueries to avoid joins that would multiply rows.
	err := db.pool.QueryRow(ctx, `
		SELECT
		    COUNT(*)::int,
		    COUNT(*) FILTER (WHERE NOT EXISTS (
		        SELECT 1 FROM decisions sup
		        WHERE sup.supersedes_id = d.id AND sup.org_id = d.org_id
		    ))::int,
		    COUNT(*) FILTER (WHERE EXISTS (
		        SELECT 1 FROM decisions sup
		        WHERE sup.supersedes_id = d.id
		          AND sup.org_id = d.org_id
		          AND EXTRACT(EPOCH FROM (sup.valid_from - d.valid_from)) / 3600 < 48
		    ))::int,
		    COUNT(*) FILTER (WHERE NOT EXISTS (
		        SELECT 1 FROM decisions cit
		        WHERE cit.precedent_ref = d.id AND cit.org_id = d.org_id AND cit.valid_to IS NULL
		    ))::int,
		    COUNT(*) FILTER (WHERE EXISTS (
		        SELECT 1 FROM decisions cit
		        WHERE cit.precedent_ref = d.id AND cit.org_id = d.org_id AND cit.valid_to IS NULL
		    ))::int
		FROM decisions d
		WHERE d.org_id = $1 AND d.valid_to IS NULL`, orgID).Scan(
		&s.DecisionsTotal,
		&s.NeverSuperseded,
		&s.RevisedWithin48h,
		&s.NeverCited,
		&s.CitedAtLeastOnce,
	)
	if err != nil {
		return s, fmt.Errorf("storage: outcome signals summary: %w", err)
	}

	// Conflict fate: org-level counts from resolved/wont_fix conflicts.
	// ConflictsWon = conflicts where a winner was declared (= ConflictsLost, symmetric).
	// ConflictsNoWinner = conflicts resolved without declaring a winner.
	err = db.pool.QueryRow(ctx, `
		SELECT
		    COUNT(*) FILTER (WHERE winning_decision_id IS NOT NULL)::int,
		    COUNT(*) FILTER (WHERE winning_decision_id IS NULL AND status = 'resolved')::int
		FROM scored_conflicts
		WHERE org_id = $1 AND status IN ('resolved', 'wont_fix')`, orgID).Scan(
		&s.ConflictsWon, &s.ConflictsNoWinner,
	)
	if err != nil {
		return s, fmt.Errorf("storage: outcome signals conflict counts: %w", err)
	}
	s.ConflictsLost = s.ConflictsWon // every decided win has a corresponding loss

	return s, nil
}
