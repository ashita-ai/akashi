package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// GetAPIKeyByID returns the API key with the given ID in the org.
func (l *LiteDB) GetAPIKeyByID(ctx context.Context, orgID uuid.UUID, keyID uuid.UUID) (model.APIKey, error) {
	var (
		k         model.APIKey
		id        string
		oid       string
		lastUsed  sql.NullString
		expiresAt sql.NullString
		revokedAt sql.NullString
	)
	err := l.db.QueryRowContext(ctx,
		`SELECT id, prefix, key_hash, agent_id, org_id, label, created_by,
		        created_at, last_used_at, expires_at, revoked_at
		 FROM api_keys WHERE id = ? AND org_id = ?`,
		uuidStr(keyID), uuidStr(orgID),
	).Scan(&id, &k.Prefix, &k.KeyHash, &k.AgentID, &oid, &k.Label, &k.CreatedBy,
		&k.CreatedAt, &lastUsed, &expiresAt, &revokedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return model.APIKey{}, storage.ErrNotFound
		}
		return model.APIKey{}, fmt.Errorf("sqlite: get api key: %w", err)
	}

	k.ID = parseUUID(id)
	k.OrgID = parseUUID(oid)
	k.LastUsedAt = parseNullTime(lastUsed)
	k.ExpiresAt = parseNullTime(expiresAt)
	k.RevokedAt = parseNullTime(revokedAt)
	return k, nil
}
