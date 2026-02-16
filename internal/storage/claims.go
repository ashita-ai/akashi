package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

// Claim is a sentence-level assertion extracted from a decision outcome,
// stored with its own embedding for fine-grained conflict detection.
type Claim struct {
	ID         uuid.UUID
	DecisionID uuid.UUID
	OrgID      uuid.UUID
	ClaimIdx   int
	ClaimText  string
	Embedding  *pgvector.Vector
}

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
// but no claims yet. Used by the claims backfill.
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
