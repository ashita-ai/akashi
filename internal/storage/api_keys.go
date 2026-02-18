package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/akashi/internal/model"
)

// CreateAPIKeyWithAudit inserts a new API key and a mutation audit entry
// atomically within a single transaction.
func (db *DB) CreateAPIKeyWithAudit(ctx context.Context, key model.APIKey, audit MutationAuditEntry) (model.APIKey, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return model.APIKey{}, fmt.Errorf("storage: begin create api key tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if key.ID == uuid.Nil {
		key.ID = uuid.New()
	}
	if key.CreatedAt.IsZero() {
		key.CreatedAt = time.Now().UTC()
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO api_keys (id, prefix, key_hash, agent_id, org_id, label, created_by, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		key.ID, key.Prefix, key.KeyHash, key.AgentID, key.OrgID,
		key.Label, key.CreatedBy, key.CreatedAt, key.ExpiresAt,
	)
	if err != nil {
		return model.APIKey{}, fmt.Errorf("storage: create api key: %w", err)
	}

	audit.ResourceID = key.ID.String()
	audit.AfterData = key
	if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
		return model.APIKey{}, fmt.Errorf("storage: audit in create api key tx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.APIKey{}, fmt.Errorf("storage: commit create api key tx: %w", err)
	}
	return key, nil
}

// GetAPIKeyByPrefixAndAgent looks up a single active API key by (prefix, agent_id).
// Used by verifyAPIKey for O(1) pre-filter before Argon2 verification.
// Returns ErrNotFound if no matching active key exists.
// Global (no org_id) because this is called during auth before org is known.
func (db *DB) GetAPIKeyByPrefixAndAgent(ctx context.Context, agentID, prefix string) (model.APIKey, error) {
	var k model.APIKey
	err := db.pool.QueryRow(ctx,
		`SELECT id, prefix, key_hash, agent_id, org_id, label, created_by, created_at, last_used_at, expires_at, revoked_at
		 FROM api_keys
		 WHERE agent_id = $1
		   AND prefix = $2
		   AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > now())
		 LIMIT 1`,
		agentID, prefix,
	).Scan(
		&k.ID, &k.Prefix, &k.KeyHash, &k.AgentID, &k.OrgID,
		&k.Label, &k.CreatedBy, &k.CreatedAt, &k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.APIKey{}, ErrNotFound
		}
		return model.APIKey{}, fmt.Errorf("storage: get api key by prefix: %w", err)
	}
	return k, nil
}

// GetAPIKeysByIDs retrieves multiple API keys by their UUIDs, scoped to an org.
// Used for batch metadata lookup in usage reporting to avoid N+1 queries.
func (db *DB) GetAPIKeysByIDs(ctx context.Context, orgID uuid.UUID, ids []uuid.UUID) ([]model.APIKey, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := db.pool.Query(ctx,
		`SELECT id, prefix, key_hash, agent_id, org_id, label, created_by, created_at, last_used_at, expires_at, revoked_at
		 FROM api_keys WHERE id = ANY($1) AND org_id = $2`,
		ids, orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get api keys by ids: %w", err)
	}
	defer rows.Close()

	var keys []model.APIKey
	for rows.Next() {
		var k model.APIKey
		if err := rows.Scan(
			&k.ID, &k.Prefix, &k.KeyHash, &k.AgentID, &k.OrgID,
			&k.Label, &k.CreatedBy, &k.CreatedAt, &k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan api key: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// GetAPIKeyByID retrieves a single API key by its UUID, scoped to an org.
func (db *DB) GetAPIKeyByID(ctx context.Context, orgID uuid.UUID, keyID uuid.UUID) (model.APIKey, error) {
	var k model.APIKey
	err := db.pool.QueryRow(ctx,
		`SELECT id, prefix, key_hash, agent_id, org_id, label, created_by, created_at, last_used_at, expires_at, revoked_at
		 FROM api_keys WHERE id = $1 AND org_id = $2`,
		keyID, orgID,
	).Scan(
		&k.ID, &k.Prefix, &k.KeyHash, &k.AgentID, &k.OrgID,
		&k.Label, &k.CreatedBy, &k.CreatedAt, &k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.APIKey{}, fmt.Errorf("storage: api key %s: %w", keyID, ErrNotFound)
		}
		return model.APIKey{}, fmt.Errorf("storage: get api key: %w", err)
	}
	return k, nil
}

// GetActiveAPIKeysByAgentIDGlobal returns all active (not revoked, not expired)
// API keys for an agent_id across all orgs. Used for authentication where org
// isn't known yet. Similar to GetAgentsByAgentIDGlobal.
func (db *DB) GetActiveAPIKeysByAgentIDGlobal(ctx context.Context, agentID string) ([]model.APIKey, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, prefix, key_hash, agent_id, org_id, label, created_by, created_at, last_used_at, expires_at, revoked_at
		 FROM api_keys
		 WHERE agent_id = $1
		   AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > now())
		 ORDER BY created_at ASC`,
		agentID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get api keys by agent_id: %w", err)
	}
	defer rows.Close()

	var keys []model.APIKey
	for rows.Next() {
		var k model.APIKey
		if err := rows.Scan(
			&k.ID, &k.Prefix, &k.KeyHash, &k.AgentID, &k.OrgID,
			&k.Label, &k.CreatedBy, &k.CreatedAt, &k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan api key: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// ListAPIKeys returns API keys for an org with pagination.
// Includes revoked/expired keys for admin visibility. Use the revoked_at and
// expires_at fields to filter in the UI if needed.
func (db *DB) ListAPIKeys(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]model.APIKey, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

	var total int
	if err := db.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM api_keys WHERE org_id = $1`, orgID,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("storage: count api keys: %w", err)
	}

	rows, err := db.pool.Query(ctx,
		`SELECT id, prefix, key_hash, agent_id, org_id, label, created_by, created_at, last_used_at, expires_at, revoked_at
		 FROM api_keys WHERE org_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2 OFFSET $3`,
		orgID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("storage: list api keys: %w", err)
	}
	defer rows.Close()

	var keys []model.APIKey
	for rows.Next() {
		var k model.APIKey
		if err := rows.Scan(
			&k.ID, &k.Prefix, &k.KeyHash, &k.AgentID, &k.OrgID,
			&k.Label, &k.CreatedBy, &k.CreatedAt, &k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("storage: scan api key: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("storage: list api keys: %w", err)
	}
	return keys, total, nil
}

// RevokeAPIKeyWithAudit sets revoked_at on an API key and records a mutation
// audit entry atomically.
func (db *DB) RevokeAPIKeyWithAudit(ctx context.Context, orgID uuid.UUID, keyID uuid.UUID, audit MutationAuditEntry) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage: begin revoke api key tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Fetch the key before revoking for audit.
	var before model.APIKey
	err = tx.QueryRow(ctx,
		`SELECT id, prefix, key_hash, agent_id, org_id, label, created_by, created_at, last_used_at, expires_at, revoked_at
		 FROM api_keys WHERE id = $1 AND org_id = $2`,
		keyID, orgID,
	).Scan(
		&before.ID, &before.Prefix, &before.KeyHash, &before.AgentID, &before.OrgID,
		&before.Label, &before.CreatedBy, &before.CreatedAt, &before.LastUsedAt, &before.ExpiresAt, &before.RevokedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("storage: api key %s: %w", keyID, ErrNotFound)
		}
		return fmt.Errorf("storage: get api key for revocation: %w", err)
	}
	if before.RevokedAt != nil {
		return fmt.Errorf("storage: api key %s already revoked", keyID)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND org_id = $2 AND revoked_at IS NULL`,
		keyID, orgID,
	)
	if err != nil {
		return fmt.Errorf("storage: revoke api key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("storage: api key %s: %w", keyID, ErrNotFound)
	}

	audit.ResourceID = keyID.String()
	audit.BeforeData = before
	if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
		return fmt.Errorf("storage: audit in revoke api key tx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("storage: commit revoke api key tx: %w", err)
	}
	return nil
}

// RotateAPIKeyWithAudit revokes the old key and creates a new one atomically.
// Returns the newly created key.
func (db *DB) RotateAPIKeyWithAudit(ctx context.Context, orgID uuid.UUID, oldKeyID uuid.UUID, newKey model.APIKey, audit MutationAuditEntry) (model.APIKey, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return model.APIKey{}, fmt.Errorf("storage: begin rotate api key tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Revoke the old key.
	tag, err := tx.Exec(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND org_id = $2 AND revoked_at IS NULL`,
		oldKeyID, orgID,
	)
	if err != nil {
		return model.APIKey{}, fmt.Errorf("storage: revoke old key during rotation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.APIKey{}, fmt.Errorf("storage: old key %s not found or already revoked: %w", oldKeyID, ErrNotFound)
	}

	// Create the new key.
	if newKey.ID == uuid.Nil {
		newKey.ID = uuid.New()
	}
	if newKey.CreatedAt.IsZero() {
		newKey.CreatedAt = time.Now().UTC()
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO api_keys (id, prefix, key_hash, agent_id, org_id, label, created_by, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		newKey.ID, newKey.Prefix, newKey.KeyHash, newKey.AgentID, newKey.OrgID,
		newKey.Label, newKey.CreatedBy, newKey.CreatedAt, newKey.ExpiresAt,
	)
	if err != nil {
		return model.APIKey{}, fmt.Errorf("storage: create new key during rotation: %w", err)
	}

	audit.ResourceID = newKey.ID.String()
	audit.AfterData = map[string]any{
		"new_key_id":     newKey.ID,
		"revoked_key_id": oldKeyID,
	}
	if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
		return model.APIKey{}, fmt.Errorf("storage: audit in rotate api key tx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.APIKey{}, fmt.Errorf("storage: commit rotate api key tx: %w", err)
	}
	return newKey, nil
}

// TouchAPIKeyLastUsed updates the last_used_at timestamp for an API key.
// Called from the auth middleware on successful authentication via a managed key.
// Uses a fire-and-forget pattern — callers should not block on the result.
func (db *DB) TouchAPIKeyLastUsed(ctx context.Context, keyID uuid.UUID) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE api_keys SET last_used_at = now() WHERE id = $1`,
		keyID,
	)
	if err != nil {
		return fmt.Errorf("storage: touch api key last_used: %w", err)
	}
	return nil
}

// MigrateAgentKeysToAPIKeys copies api_key_hash from agents to api_keys for
// agents that still have a legacy key. Idempotent: skips agents that already
// have at least one entry in api_keys. NULLs out agents.api_key_hash after copy.
// Called once at startup.
func (db *DB) MigrateAgentKeysToAPIKeys(ctx context.Context) (int, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("storage: begin key migration tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Find agents with legacy keys that haven't been migrated yet.
	rows, err := tx.Query(ctx,
		`SELECT a.id, a.agent_id, a.org_id, a.api_key_hash
		 FROM agents a
		 WHERE a.api_key_hash IS NOT NULL
		   AND NOT EXISTS (
		       SELECT 1 FROM api_keys k
		       WHERE k.org_id = a.org_id AND k.agent_id = a.agent_id
		   )`,
	)
	if err != nil {
		return 0, fmt.Errorf("storage: query agents for key migration: %w", err)
	}
	defer rows.Close()

	type legacyKey struct {
		agentUUID uuid.UUID
		agentID   string
		orgID     uuid.UUID
		keyHash   string
	}
	var toMigrate []legacyKey

	for rows.Next() {
		var lk legacyKey
		if err := rows.Scan(&lk.agentUUID, &lk.agentID, &lk.orgID, &lk.keyHash); err != nil {
			return 0, fmt.Errorf("storage: scan legacy key: %w", err)
		}
		toMigrate = append(toMigrate, lk)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("storage: iterate legacy keys: %w", err)
	}

	for _, lk := range toMigrate {
		keyID := uuid.New()
		now := time.Now().UTC()
		_, err := tx.Exec(ctx,
			`INSERT INTO api_keys (id, prefix, key_hash, agent_id, org_id, label, created_by, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			keyID, "legacy__", lk.keyHash, lk.agentID, lk.orgID,
			"Migrated from agent", "system", now,
		)
		if err != nil {
			return 0, fmt.Errorf("storage: insert migrated key for agent %s: %w", lk.agentID, err)
		}

		// Write audit trail for the migrated key so the mutation_audit_log
		// reflects when and why this key entered the api_keys table.
		if err := InsertMutationAuditTx(ctx, tx, MutationAuditEntry{
			RequestID:    "system:startup:key-migration",
			OrgID:        lk.orgID,
			ActorAgentID: "system",
			ActorRole:    "platform_admin",
			Operation:    "migrate_api_key",
			ResourceType: "api_key",
			ResourceID:   keyID.String(),
			AfterData: map[string]any{
				"agent_id": lk.agentID,
				"org_id":   lk.orgID,
				"source":   "legacy_api_key_hash",
			},
		}); err != nil {
			return 0, fmt.Errorf("storage: audit migrate key for agent %s: %w", lk.agentID, err)
		}

		// NULL out the legacy hash.
		_, err = tx.Exec(ctx,
			`UPDATE agents SET api_key_hash = NULL WHERE id = $1`,
			lk.agentUUID,
		)
		if err != nil {
			return 0, fmt.Errorf("storage: null legacy hash for agent %s: %w", lk.agentID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("storage: commit key migration tx: %w", err)
	}
	return len(toMigrate), nil
}

// CountDecisionsByAPIKey returns decision counts grouped by api_key_id for
// usage metering. Keys with NULL api_key_id are grouped under uuid.Nil.
//
// AND valid_to IS NULL counts only currently-active decisions created in the period.
// A decision that is later superseded (valid_to set by a revision) stops counting
// under its original period; its successor counts under the revision period.
// This is intentional: billing tracks active decisions, not raw trace events.
// To count all decisions ever traced in a period regardless of revision status,
// remove the valid_to IS NULL filter.
func (db *DB) CountDecisionsByAPIKey(ctx context.Context, orgID uuid.UUID, from, to time.Time) (map[uuid.UUID]int, int, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT api_key_id, count(*)
		 FROM decisions
		 WHERE org_id = $1 AND created_at >= $2 AND created_at < $3 AND valid_to IS NULL
		 GROUP BY api_key_id`,
		orgID, from, to,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("storage: count decisions by api key: %w", err)
	}
	defer rows.Close()

	result := make(map[uuid.UUID]int)
	total := 0
	for rows.Next() {
		var keyID *uuid.UUID
		var count int
		if err := rows.Scan(&keyID, &count); err != nil {
			return nil, 0, fmt.Errorf("storage: scan decision count by key: %w", err)
		}
		id := uuid.Nil
		if keyID != nil {
			id = *keyID
		}
		result[id] = count
		total += count
	}
	return result, total, rows.Err()
}
