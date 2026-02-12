package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// IntegrityProof represents a Merkle tree batch proof for an organization.
type IntegrityProof struct {
	ID            uuid.UUID `json:"id"`
	OrgID         uuid.UUID `json:"org_id"`
	BatchStart    time.Time `json:"batch_start"`
	BatchEnd      time.Time `json:"batch_end"`
	DecisionCount int       `json:"decision_count"`
	RootHash      string    `json:"root_hash"`
	PreviousRoot  *string   `json:"previous_root,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// GetLatestIntegrityProof returns the most recent integrity proof for an org.
// Returns nil if no proofs exist.
func (db *DB) GetLatestIntegrityProof(ctx context.Context, orgID uuid.UUID) (*IntegrityProof, error) {
	var p IntegrityProof
	err := db.pool.QueryRow(ctx,
		`SELECT id, org_id, batch_start, batch_end, decision_count, root_hash, previous_root, created_at
		 FROM integrity_proofs
		 WHERE org_id = $1
		 ORDER BY created_at DESC
		 LIMIT 1`, orgID,
	).Scan(&p.ID, &p.OrgID, &p.BatchStart, &p.BatchEnd, &p.DecisionCount, &p.RootHash, &p.PreviousRoot, &p.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("storage: get latest integrity proof: %w", err)
	}
	return &p, nil
}

// CreateIntegrityProof inserts a new integrity proof.
func (db *DB) CreateIntegrityProof(ctx context.Context, p IntegrityProof) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	_, err := db.pool.Exec(ctx,
		`INSERT INTO integrity_proofs (id, org_id, batch_start, batch_end, decision_count, root_hash, previous_root, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		p.ID, p.OrgID, p.BatchStart, p.BatchEnd, p.DecisionCount, p.RootHash, p.PreviousRoot, p.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("storage: create integrity proof: %w", err)
	}
	return nil
}

// GetDecisionHashesForBatch returns content_hash values for decisions in an org
// created between since (exclusive) and until (inclusive), ordered lexicographically.
func (db *DB) GetDecisionHashesForBatch(ctx context.Context, orgID uuid.UUID, since, until time.Time) ([]string, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT content_hash FROM decisions
		 WHERE org_id = $1 AND created_at > $2 AND created_at <= $3
		   AND content_hash IS NOT NULL AND content_hash != ''
		 ORDER BY content_hash ASC`,
		orgID, since, until,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get decision hashes for batch: %w", err)
	}
	defer rows.Close()

	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("storage: scan decision hash: %w", err)
		}
		hashes = append(hashes, h)
	}
	return hashes, rows.Err()
}

// ListOrganizationIDs returns all active organization IDs.
func (db *DB) ListOrganizationIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := db.pool.Query(ctx, `SELECT id FROM organizations ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("storage: list organization IDs: %w", err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("storage: scan organization ID: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
