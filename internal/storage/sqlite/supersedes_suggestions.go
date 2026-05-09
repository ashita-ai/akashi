package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// InsertSupersedesSuggestion writes a detector-inferred supersedes link as a
// 'suggested' row in decision_supersedes. Mirrors the Postgres behavior:
// idempotent, no-op when a row already exists for (superseding_id,
// superseded_id) — confirmed links are never overwritten by suggestions.
func (l *LiteDB) InsertSupersedesSuggestion(ctx context.Context, s storage.SupersedesSuggestionInsert) error {
	if s.SuggestedBy == "" {
		return errors.New("sqlite: suggested_by is required for supersedes suggestion")
	}
	if s.SupersedingID == s.SupersededID {
		return errors.New("sqlite: superseding_id and superseded_id must differ")
	}
	var conf any
	if s.Confidence != nil {
		conf = *s.Confidence
	}
	var reason any
	if s.Reason != "" {
		reason = s.Reason
	}
	// Explicit recorded_at ensures the stored format (RFC 3339) matches what
	// DeleteOldSupersedesSuggestions writes when comparing — SQLite's default
	// datetime('now') uses a space-separated format that breaks lexicographic
	// comparison against RFC 3339 (' ' < 'T' in ASCII).
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO decision_supersedes
		    (superseding_id, superseded_id, org_id, relationship, is_primary,
		     suggested_by, suggested_confidence, suggested_reason, recorded_at)
		 VALUES (?, ?, ?, 'suggested', 0, ?, ?, ?, ?)
		 ON CONFLICT (superseding_id, superseded_id) DO NOTHING`,
		uuidStr(s.SupersedingID), uuidStr(s.SupersededID), uuidStr(s.OrgID),
		s.SuggestedBy, conf, reason, timeStr(time.Now().UTC()),
	)
	if err != nil {
		return fmt.Errorf("sqlite: insert supersedes suggestion: %w", err)
	}
	return nil
}

// ListSupersedesSuggestionsForDecisions returns suggested supersedes links
// where the superseding decision is in the supplied set, mirroring the
// Postgres ordering (superseding_id, recorded_at DESC).
func (l *LiteDB) ListSupersedesSuggestionsForDecisions(ctx context.Context, orgID uuid.UUID, supersedingIDs []uuid.UUID) ([]model.SupersedesSuggestion, error) {
	if len(supersedingIDs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(supersedingIDs))
	args := make([]any, 0, 1+len(supersedingIDs))
	args = append(args, uuidStr(orgID))
	for i, id := range supersedingIDs {
		placeholders[i] = "?"
		args = append(args, uuidStr(id))
	}

	// Placeholders are a fixed sequence of "?" tokens — no caller input is
	// concatenated into the SQL. The args slice carries the actual values
	// through QueryContext's parameter binding.
	q := `SELECT superseding_id, superseded_id, suggested_by, ` + //nolint:gosec // G202: only "?" placeholders are concatenated
		`             suggested_confidence, suggested_reason, recorded_at
	      FROM decision_supersedes
	      WHERE org_id = ?
	        AND relationship = 'suggested'
	        AND superseding_id IN (` + strings.Join(placeholders, ",") + `)
	      ORDER BY superseding_id, recorded_at DESC`

	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list supersedes suggestions: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	// No capacity hint: a single superseding decision can have multiple
	// suggested predecessors, so len(supersedingIDs) is a poor lower bound.
	var out []model.SupersedesSuggestion
	for rows.Next() {
		var (
			supID, subID, suggestedBy, recordedAt string
			conf                                  sql.NullFloat64
			reason                                sql.NullString
		)
		if err := rows.Scan(&supID, &subID, &suggestedBy, &conf, &reason, &recordedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan supersedes suggestion: %w", err)
		}
		s := model.SupersedesSuggestion{
			SupersedingID: parseUUID(supID),
			SupersededID:  parseUUID(subID),
			SuggestedBy:   suggestedBy,
			RecordedAt:    parseTime(recordedAt),
		}
		if conf.Valid {
			f := float32(conf.Float64)
			s.Confidence = &f
		}
		if reason.Valid {
			s.Reason = reason.String
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate supersedes suggestions: %w", err)
	}
	return out, nil
}

// DeleteOldSupersedesSuggestions removes suggested rows older than cutoff.
// Mirrors the Postgres semantics: confirmed rows are untouched.
func (l *LiteDB) DeleteOldSupersedesSuggestions(ctx context.Context, before time.Time) (int64, error) {
	res, err := l.db.ExecContext(ctx,
		`DELETE FROM decision_supersedes
		 WHERE relationship = 'suggested' AND recorded_at < ?`,
		timeStr(before))
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete old supersedes suggestions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: rows affected: %w", err)
	}
	return n, nil
}
