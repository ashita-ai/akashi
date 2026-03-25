//go:build !lite

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

// GetRecentIntegrityProofs returns the N most recent integrity proofs for an org,
// ordered newest-first. Used by the background integrity audit to verify chain linkage.
func (db *DB) GetRecentIntegrityProofs(ctx context.Context, orgID uuid.UUID, limit int) ([]IntegrityProof, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, batch_start, batch_end, decision_count, root_hash, previous_root, created_at
		 FROM integrity_proofs
		 WHERE org_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2`, orgID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get recent integrity proofs: %w", err)
	}
	defer rows.Close()

	var proofs []IntegrityProof
	for rows.Next() {
		var p IntegrityProof
		if err := rows.Scan(&p.ID, &p.OrgID, &p.BatchStart, &p.BatchEnd, &p.DecisionCount, &p.RootHash, &p.PreviousRoot, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("storage: scan integrity proof: %w", err)
		}
		proofs = append(proofs, p)
	}
	return proofs, rows.Err()
}

// IntegrityViolation records a detected integrity proof failure.
// Written by the background audit loop and persisted durably so violations
// survive log rotation and are queryable for incident response.
type IntegrityViolation struct {
	ID            uuid.UUID      `json:"id"`
	OrgID         uuid.UUID      `json:"org_id"`
	ProofID       uuid.UUID      `json:"proof_id"`
	ViolationType string         `json:"violation_type"` // merkle_root_mismatch | chain_linkage_broken | chain_linkage_nil_previous
	Details       map[string]any `json:"details"`
	CreatedAt     time.Time      `json:"created_at"`
}

// CreateIntegrityViolation inserts a detected integrity violation into the
// append-only violations table. This is the durable counterpart to the log
// messages in auditIntegrityProofs — the log is for operators, this is for
// the audit trail.
func (db *DB) CreateIntegrityViolation(ctx context.Context, v IntegrityViolation) error {
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	_, err := db.pool.Exec(ctx,
		`INSERT INTO integrity_violations (id, org_id, proof_id, violation_type, details, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		v.ID, v.OrgID, v.ProofID, v.ViolationType, v.Details, v.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("storage: create integrity violation: %w", err)
	}
	return nil
}

// GetIntegrityViolations returns recent integrity violations for an org,
// ordered newest-first. Used by API endpoints and incident response.
func (db *DB) GetIntegrityViolations(ctx context.Context, orgID uuid.UUID, limit int) ([]IntegrityViolation, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, proof_id, violation_type, details, created_at
		 FROM integrity_violations
		 WHERE org_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2`, orgID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get integrity violations: %w", err)
	}
	defer rows.Close()

	var violations []IntegrityViolation
	for rows.Next() {
		var v IntegrityViolation
		if err := rows.Scan(&v.ID, &v.OrgID, &v.ProofID, &v.ViolationType, &v.Details, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("storage: scan integrity violation: %w", err)
		}
		violations = append(violations, v)
	}
	return violations, rows.Err()
}

// GetDecisionHashesForBatch returns content_hash values for decisions in an org
// created between since (exclusive) and until (inclusive), ordered lexicographically.
//
// Uses created_at (insertion time), not valid_from, so the Merkle chain reflects
// physical write order. This ensures deterministic batching regardless of
// bi-temporal valid_from/valid_to values.
//
// Includes superseded decisions (valid_to IS NOT NULL) intentionally: the integrity
// proof attests to the full write history, not just active rows. Revisions are
// physical writes that should be included in the chain for tamper detection.
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

// IntegrityAuditResult records the outcome of a single integrity check
// (Merkle root verification or chain linkage verification) for a proof.
type IntegrityAuditResult struct {
	ID        uuid.UUID
	OrgID     uuid.UUID
	ProofID   uuid.UUID
	CheckType string // "merkle_root" or "chain_linkage"
	Passed    bool
	SweepType string // "sample" or "full"
	Detail    string // human-readable context on failure
	CheckedAt time.Time
}

// InsertIntegrityAuditResults batch-inserts audit results. Each row records
// whether a specific integrity check passed or failed, providing a durable
// paper trail that survives log rotation.
func (db *DB) InsertIntegrityAuditResults(ctx context.Context, results []IntegrityAuditResult) error {
	if len(results) == 0 {
		return nil
	}
	_, err := db.pool.CopyFrom(ctx,
		pgx.Identifier{"integrity_audit_results"},
		[]string{"id", "org_id", "proof_id", "check_type", "passed", "sweep_type", "detail", "checked_at"},
		pgx.CopyFromSlice(len(results), func(i int) ([]any, error) {
			r := results[i]
			id := r.ID
			if id == uuid.Nil {
				id = uuid.New()
			}
			return []any{id, r.OrgID, r.ProofID, r.CheckType, r.Passed, r.SweepType, r.Detail, r.CheckedAt}, nil
		}),
	)
	if err != nil {
		return fmt.Errorf("storage: insert integrity audit results: %w", err)
	}
	return nil
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

// GetOrgIDByOffset returns the single organization ID at the given offset
// within a deterministic (ORDER BY id) ordering. Returns uuid.Nil and no error
// when the offset exceeds the number of organizations. This replaces loading
// the entire org table for the audit loop's round-robin selection.
func (db *DB) GetOrgIDByOffset(ctx context.Context, offset int) (uuid.UUID, error) {
	var id uuid.UUID
	err := db.pool.QueryRow(ctx,
		`SELECT id FROM organizations ORDER BY id LIMIT 1 OFFSET $1`, offset,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, nil
		}
		return uuid.Nil, fmt.Errorf("storage: get org by offset: %w", err)
	}
	return id, nil
}

// CountOrganizations returns the total number of organizations.
func (db *DB) CountOrganizations(ctx context.Context) (int, error) {
	var count int
	err := db.pool.QueryRow(ctx, `SELECT count(*) FROM organizations`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("storage: count organizations: %w", err)
	}
	return count, nil
}
