package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
)

// GetSessionDecisions returns all active decisions for a given session within an org,
// ordered chronologically (oldest first) to reconstruct the session timeline.
func (db *DB) GetSessionDecisions(ctx context.Context, orgID, sessionID uuid.UUID) ([]model.Decision, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		 metadata, quality_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at, session_id, agent_context
		 FROM decisions
		 WHERE org_id = $1 AND session_id = $2 AND valid_to IS NULL
		 ORDER BY valid_from ASC`,
		orgID, sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get session decisions: %w", err)
	}
	defer rows.Close()

	return scanDecisions(rows)
}
