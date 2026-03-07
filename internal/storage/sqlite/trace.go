package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// InsertDecision stores a decision and synchronizes the FTS5 index in a single
// transaction. Called during the trace pipeline. The FTS5 content is built from
// decision_type, outcome, and reasoning concatenated with newline separators.
func (s *Store) InsertDecision(ctx context.Context, d CheckResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin insert tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Rollback after commit is a no-op.

	createdAt := d.CreatedAt.UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO decisions (id, decision_type, outcome, reasoning, confidence, agent_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			decision_type = excluded.decision_type,
			outcome       = excluded.outcome,
			reasoning     = excluded.reasoning,
			confidence    = excluded.confidence,
			agent_id      = excluded.agent_id,
			created_at    = excluded.created_at`,
		d.DecisionID.String(), d.DecisionType, d.Outcome, d.Reasoning,
		d.Confidence, d.AgentID, createdAt,
	)
	if err != nil {
		return fmt.Errorf("sqlite: insert decision: %w", err)
	}

	content := buildFTSContent(d.DecisionType, d.Outcome, d.Reasoning)

	// FTS5 does not support UPDATE; delete then re-insert.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM decisions_fts WHERE decision_id = ?`, d.DecisionID.String(),
	); err != nil {
		return fmt.Errorf("sqlite: delete fts entry: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO decisions_fts (decision_id, content) VALUES (?, ?)`,
		d.DecisionID.String(), content,
	); err != nil {
		return fmt.Errorf("sqlite: insert fts entry: %w", err)
	}

	return tx.Commit()
}

// DeleteDecision removes a decision and its FTS5 entry.
func (s *Store) DeleteDecision(ctx context.Context, id uuid.UUID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin delete tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM decisions_fts WHERE decision_id = ?`, id.String()); err != nil {
		return fmt.Errorf("sqlite: delete fts entry: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM decisions WHERE id = ?`, id.String()); err != nil {
		return fmt.Errorf("sqlite: delete decision: %w", err)
	}

	return tx.Commit()
}

// buildFTSContent concatenates decision fields into a single searchable string.
// Fields are separated by newlines so FTS5 tokenizes them independently.
func buildFTSContent(decisionType, outcome string, reasoning *string) string {
	var b strings.Builder
	b.WriteString(decisionType)
	b.WriteByte('\n')
	b.WriteString(outcome)
	if reasoning != nil && *reasoning != "" {
		b.WriteByte('\n')
		b.WriteString(*reasoning)
	}
	return b.String()
}
