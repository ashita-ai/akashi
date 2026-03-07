package sqlite

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/storage"
)

// HasClaimsForDecision returns true if claims exist for the given decision.
func (l *LiteDB) HasClaimsForDecision(ctx context.Context, decisionID, orgID uuid.UUID) (bool, error) {
	var exists bool
	err := l.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM decision_claims WHERE decision_id = ? AND org_id = ?)`,
		uuidStr(decisionID), uuidStr(orgID),
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("sqlite: has claims: %w", err)
	}
	return exists, nil
}

// InsertClaims inserts claims in a single transaction (replaces Postgres COPY).
func (l *LiteDB) InsertClaims(ctx context.Context, claims []storage.Claim) error {
	if len(claims) == 0 {
		return nil
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin claims tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO decision_claims (decision_id, org_id, claim_idx, claim_text, category, embedding)
		 VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("sqlite: prepare claims: %w", err)
	}
	defer stmt.Close() //nolint:errcheck

	for _, c := range claims {
		_, err := stmt.ExecContext(ctx,
			uuidStr(c.DecisionID), uuidStr(c.OrgID), c.ClaimIdx, c.ClaimText, c.Category, vectorToBlob(c.Embedding))
		if err != nil {
			return fmt.Errorf("sqlite: insert claim: %w", err)
		}
	}

	return tx.Commit()
}

// FindDecisionIDsMissingClaims returns decisions that have embeddings but no claims.
func (l *LiteDB) FindDecisionIDsMissingClaims(ctx context.Context, limit int) ([]storage.DecisionRef, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT d.id, d.org_id FROM decisions d
		 LEFT JOIN decision_claims c ON c.decision_id = d.id
		 WHERE d.valid_to IS NULL AND d.embedding IS NOT NULL AND c.id IS NULL
		   AND d.claim_embeddings_failed_at IS NULL
		 ORDER BY d.valid_from ASC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: find decisions missing claims: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var refs []storage.DecisionRef
	for rows.Next() {
		var idStr, orgStr string
		if err := rows.Scan(&idStr, &orgStr); err != nil {
			return nil, fmt.Errorf("sqlite: scan decision ref: %w", err)
		}
		refs = append(refs, storage.DecisionRef{ID: parseUUID(idStr), OrgID: parseUUID(orgStr)})
	}
	return refs, rows.Err()
}

// MarkClaimEmbeddingFailed records that claim embedding generation failed for a
// decision. Sets claim_embeddings_failed_at to now and increments the attempt counter.
func (l *LiteDB) MarkClaimEmbeddingFailed(ctx context.Context, decisionID, orgID uuid.UUID) error {
	_, err := l.db.ExecContext(ctx,
		`UPDATE decisions
		 SET claim_embeddings_failed_at = datetime('now'),
		     claim_embedding_attempts = claim_embedding_attempts + 1
		 WHERE id = ? AND org_id = ?`,
		uuidStr(decisionID), uuidStr(orgID))
	if err != nil {
		return fmt.Errorf("sqlite: mark claim embedding failed: %w", err)
	}
	return nil
}

// ClearClaimEmbeddingFailure clears the failure state after a successful retry.
func (l *LiteDB) ClearClaimEmbeddingFailure(ctx context.Context, decisionID, orgID uuid.UUID) error {
	_, err := l.db.ExecContext(ctx,
		`UPDATE decisions
		 SET claim_embeddings_failed_at = NULL,
		     claim_embedding_attempts = 0
		 WHERE id = ? AND org_id = ?`,
		uuidStr(decisionID), uuidStr(orgID))
	if err != nil {
		return fmt.Errorf("sqlite: clear claim embedding failure: %w", err)
	}
	return nil
}

// FindRetriableClaimFailures returns decisions that have failed claim embedding
// generation and are eligible for retry based on exponential backoff.
//
// Backoff: 5min * 4^(attempts-1) after each failure, capped by maxAttempts.
func (l *LiteDB) FindRetriableClaimFailures(ctx context.Context, maxAttempts, limit int) ([]storage.ClaimRetryRef, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, org_id, claim_embedding_attempts
		 FROM decisions
		 WHERE claim_embeddings_failed_at IS NOT NULL
		   AND claim_embedding_attempts < ?
		   AND valid_to IS NULL
		   AND embedding IS NOT NULL
		   AND datetime(claim_embeddings_failed_at, '+' || CAST(300 * CAST(ROUND(POWER(4, claim_embedding_attempts - 1)) AS INTEGER) AS TEXT) || ' seconds') <= datetime('now')
		 ORDER BY claim_embeddings_failed_at ASC
		 LIMIT ?`, maxAttempts, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: find retriable claim failures: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var refs []storage.ClaimRetryRef
	for rows.Next() {
		var idStr, orgStr string
		var attempts int
		if err := rows.Scan(&idStr, &orgStr, &attempts); err != nil {
			return nil, fmt.Errorf("sqlite: scan retriable claim ref: %w", err)
		}
		refs = append(refs, storage.ClaimRetryRef{
			ID:       parseUUID(idStr),
			OrgID:    parseUUID(orgStr),
			Attempts: attempts,
		})
	}
	return refs, rows.Err()
}
