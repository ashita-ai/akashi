//go:build !lite

package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// InsertClaims bulk-inserts claims for a decision. Uses COPY for efficiency.
func (db *DB) InsertClaims(ctx context.Context, claims []Claim) error {
	if len(claims) == 0 {
		return nil
	}

	rows := make([][]any, len(claims))
	for i, c := range claims {
		rows[i] = []any{c.DecisionID, c.OrgID, c.ClaimIdx, c.ClaimText, c.Embedding}
	}

	_, err := db.pool.CopyFrom(ctx,
		pgx.Identifier{"decision_claims"},
		[]string{"decision_id", "org_id", "claim_idx", "claim_text", "embedding"},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return fmt.Errorf("storage: insert claims: %w", err)
	}
	return nil
}

// FindClaimsByDecision returns all claims for a decision, ordered by claim_idx.
func (db *DB) FindClaimsByDecision(ctx context.Context, decisionID, orgID uuid.UUID) ([]Claim, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, decision_id, org_id, claim_idx, claim_text, embedding
		 FROM decision_claims
		 WHERE decision_id = $1 AND org_id = $2
		 ORDER BY claim_idx`, decisionID, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: find claims: %w", err)
	}
	defer rows.Close()

	var claims []Claim
	for rows.Next() {
		var c Claim
		if err := rows.Scan(&c.ID, &c.DecisionID, &c.OrgID, &c.ClaimIdx, &c.ClaimText, &c.Embedding); err != nil {
			return nil, fmt.Errorf("storage: scan claim: %w", err)
		}
		claims = append(claims, c)
	}
	return claims, rows.Err()
}

// FindDecisionIDsMissingClaims returns IDs of decisions that have embeddings
// but no claims yet AND have not been marked as failed (those are handled by
// the retry loop). Used by the claims backfill.
// SECURITY: Intentionally global — background backfill across all orgs. Each
// returned row includes OrgID for downstream scoping (generateClaims).
func (db *DB) FindDecisionIDsMissingClaims(ctx context.Context, limit int) ([]DecisionRef, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := db.pool.Query(ctx,
		`SELECT d.id, d.org_id
		 FROM decisions d
		 LEFT JOIN decision_claims c ON c.decision_id = d.id
		 WHERE d.valid_to IS NULL
		   AND d.embedding IS NOT NULL
		   AND c.id IS NULL
		   AND d.claim_embeddings_failed_at IS NULL
		 ORDER BY d.valid_from ASC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: find decisions missing claims: %w", err)
	}
	defer rows.Close()

	var refs []DecisionRef
	for rows.Next() {
		var r DecisionRef
		if err := rows.Scan(&r.ID, &r.OrgID); err != nil {
			return nil, fmt.Errorf("storage: scan decision ref: %w", err)
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

// MarkClaimEmbeddingFailed records that claim embedding generation failed for a
// decision. Sets claim_embeddings_failed_at to NOW() and increments the attempt
// counter. Safe to call multiple times (each call updates the timestamp and
// increments attempts).
func (db *DB) MarkClaimEmbeddingFailed(ctx context.Context, decisionID, orgID uuid.UUID) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE decisions
		 SET claim_embeddings_failed_at = NOW(),
		     claim_embedding_attempts = claim_embedding_attempts + 1
		 WHERE id = $1 AND org_id = $2`,
		decisionID, orgID)
	if err != nil {
		return fmt.Errorf("storage: mark claim embedding failed: %w", err)
	}
	return nil
}

// ClearClaimEmbeddingFailure clears the failure state after a successful retry.
// Resets both the timestamp and the attempt counter.
func (db *DB) ClearClaimEmbeddingFailure(ctx context.Context, decisionID, orgID uuid.UUID) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE decisions
		 SET claim_embeddings_failed_at = NULL,
		     claim_embedding_attempts = 0
		 WHERE id = $1 AND org_id = $2`,
		decisionID, orgID)
	if err != nil {
		return fmt.Errorf("storage: clear claim embedding failure: %w", err)
	}
	return nil
}

// FindRetriableClaimFailures returns decisions that have failed claim embedding
// generation and are eligible for retry based on exponential backoff.
//
// Backoff schedule: 5min * 4^(attempts-1) after each failure.
//   - After attempt 1: wait 5 minutes
//   - After attempt 2: wait 20 minutes
//   - After attempt 3+: no retry (capped by maxAttempts)
//
// SECURITY: Intentionally global — background retry across all orgs. Each
// returned row includes OrgID for downstream scoping.
func (db *DB) FindRetriableClaimFailures(ctx context.Context, maxAttempts, limit int) ([]ClaimRetryRef, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, claim_embedding_attempts
		 FROM decisions
		 WHERE claim_embeddings_failed_at IS NOT NULL
		   AND claim_embedding_attempts < $1
		   AND valid_to IS NULL
		   AND embedding IS NOT NULL
		   AND claim_embeddings_failed_at + make_interval(secs => 300 * POWER(4, claim_embedding_attempts - 1)) <= NOW()
		 ORDER BY claim_embeddings_failed_at ASC
		 LIMIT $2`, maxAttempts, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: find retriable claim failures: %w", err)
	}
	defer rows.Close()

	var refs []ClaimRetryRef
	for rows.Next() {
		var r ClaimRetryRef
		if err := rows.Scan(&r.ID, &r.OrgID, &r.Attempts); err != nil {
			return nil, fmt.Errorf("storage: scan retriable claim ref: %w", err)
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

// HasClaimsForDecision checks whether a decision already has claims stored.
func (db *DB) HasClaimsForDecision(ctx context.Context, decisionID, orgID uuid.UUID) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM decision_claims WHERE decision_id = $1 AND org_id = $2)`,
		decisionID, orgID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("storage: check claims exist: %w", err)
	}
	return exists, nil
}
