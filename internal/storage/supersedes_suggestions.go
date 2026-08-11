//go:build !lite

package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
)

// InsertSupersedesSuggestion writes a detector-inferred supersedes link as a
// 'suggested' row in decision_supersedes. Idempotent: if a row already exists
// for (superseding_id, superseded_id) — whether confirmed or earlier-suggested
// — the insert is a no-op, leaving any confirmed link intact.
//
// It also refuses to write when the INVERSE row exists. The primary key is the
// ordered pair (superseding_id, superseded_id), so (X,Y) and (Y,X) do not
// collide, and direction is no longer a function of stored data: it is whatever
// the judge answered on that pass, sampled at the provider's default
// temperature. The same pair is judged in both orientations routinely — the
// per-run pairCache does not span backfill runs — so an inconsistent answer
// across two passes would otherwise persist two contradictory, agent-facing
// links with no reconciliation path. Reports inverseExists so the caller can
// surface the disagreement rather than silently resolve it.
//
// Residual race: the existence check and the insert share one statement and so
// one snapshot, but under READ COMMITTED a concurrent transaction can still
// commit the inverse between them. Closing that fully needs a unique index on
// the unordered pair (LEAST/GREATEST) for suggested rows; this narrows the
// window to concurrent inserts of the same pair rather than any two runs.
//
// SuggestedBy must be non-empty (enforced by migration 105's CHECK constraint).
// SupersedingID and SupersededID must be distinct (enforced by table CHECK).
func (db *DB) InsertSupersedesSuggestion(ctx context.Context, s SupersedesSuggestionInsert) (inverseExists bool, err error) {
	if s.SuggestedBy == "" {
		return false, errors.New("storage: suggested_by is required for supersedes suggestion")
	}
	if s.SupersedingID == s.SupersededID {
		return false, errors.New("storage: superseding_id and superseded_id must differ")
	}
	err = db.pool.QueryRow(ctx,
		`WITH inverse AS (
		     SELECT 1 FROM decision_supersedes
		      WHERE superseding_id = $2 AND superseded_id = $1 AND org_id = $3
		 ), ins AS (
		     INSERT INTO decision_supersedes
		         (superseding_id, superseded_id, org_id, relationship, is_primary,
		          suggested_by, suggested_confidence, suggested_reason)
		     SELECT $1, $2, $3, 'suggested', FALSE, $4, $5, $6
		      WHERE NOT EXISTS (SELECT 1 FROM inverse)
		     ON CONFLICT (superseding_id, superseded_id) DO NOTHING
		     RETURNING 1
		 )
		 SELECT EXISTS (SELECT 1 FROM inverse)`,
		s.SupersedingID, s.SupersededID, s.OrgID,
		s.SuggestedBy, s.Confidence, nullableString(s.Reason),
	).Scan(&inverseExists)
	if err != nil {
		return false, fmt.Errorf("storage: insert supersedes suggestion: %w", err)
	}
	return inverseExists, nil
}

// ListSupersedesSuggestionsForDecisions returns all detector-suggested
// supersedes links where the superseding decision is in the supplied set.
// Returned in (superseding_id, recorded_at DESC) order so callers can group
// by superseding decision and present newest-first.
//
// Returns an empty slice when supersedingIDs is empty.
func (db *DB) ListSupersedesSuggestionsForDecisions(ctx context.Context, orgID uuid.UUID, supersedingIDs []uuid.UUID) ([]model.SupersedesSuggestion, error) {
	if len(supersedingIDs) == 0 {
		return nil, nil
	}
	rows, err := db.pool.Query(ctx,
		`SELECT superseding_id, superseded_id, suggested_by,
		        suggested_confidence, suggested_reason, recorded_at
		 FROM decision_supersedes
		 WHERE org_id = $1
		   AND relationship = 'suggested'
		   AND superseding_id = ANY($2)
		 ORDER BY superseding_id, recorded_at DESC`,
		orgID, supersedingIDs)
	if err != nil {
		return nil, fmt.Errorf("storage: list supersedes suggestions: %w", err)
	}
	defer rows.Close()

	// No capacity hint: a single superseding decision can have multiple
	// suggested predecessors, so len(supersedingIDs) is a poor lower bound.
	var out []model.SupersedesSuggestion
	for rows.Next() {
		var s model.SupersedesSuggestion
		var reason *string
		if err := rows.Scan(&s.SupersedingID, &s.SupersededID, &s.SuggestedBy,
			&s.Confidence, &reason, &s.RecordedAt); err != nil {
			return nil, fmt.Errorf("storage: scan supersedes suggestion: %w", err)
		}
		if reason != nil {
			s.Reason = *reason
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate supersedes suggestions: %w", err)
	}
	return out, nil
}

// DeleteOldSupersedesSuggestions removes 'suggested' rows older than the
// given cutoff. Confirmed rows (relationship='supersedes' or 'reconciles')
// are never touched. Returns the number of rows deleted.
//
// Used by the retention worker to bound suggestion-table growth — agents
// either confirm a suggestion (which promotes the row via the trigger from
// migration 104) or implicitly dismiss it by letting the cutoff pass.
func (db *DB) DeleteOldSupersedesSuggestions(ctx context.Context, before time.Time) (int64, error) {
	tag, err := db.pool.Exec(ctx,
		`DELETE FROM decision_supersedes
		 WHERE relationship = 'suggested' AND recorded_at < $1`,
		before)
	if err != nil {
		return 0, fmt.Errorf("storage: delete old supersedes suggestions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// nullableString returns nil for empty strings so the database stores NULL
// rather than the empty string. Used for optional VARCHAR columns where the
// distinction matters for CHECK constraints.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
