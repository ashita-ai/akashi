package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/storage"
)

// BeginIdempotency attempts to claim an idempotency key. If the key already exists,
// returns the cached response (if completed) or signals a concurrent request.
func (l *LiteDB) BeginIdempotency(ctx context.Context, orgID uuid.UUID, agentID, endpoint, key, requestHash string) (storage.IdempotencyLookup, error) {
	res, err := l.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO idempotency_keys (org_id, agent_id, endpoint, idempotency_key, request_hash, status)
		 VALUES (?, ?, ?, ?, ?, 'in_progress')`,
		uuidStr(orgID), agentID, endpoint, key, requestHash,
	)
	if err != nil {
		return storage.IdempotencyLookup{}, fmt.Errorf("sqlite: begin idempotency: %w", err)
	}

	n, _ := res.RowsAffected()
	if n > 0 {
		// Successfully claimed — no prior entry.
		return storage.IdempotencyLookup{}, nil
	}

	// Key already exists — look up the existing entry.
	var (
		existingHash string
		status       string
		statusCode   sql.NullInt64
		respData     sql.NullString
	)
	err = l.db.QueryRowContext(ctx,
		`SELECT request_hash, status, status_code, response_data
		 FROM idempotency_keys
		 WHERE org_id = ? AND agent_id = ? AND endpoint = ? AND idempotency_key = ?`,
		uuidStr(orgID), agentID, endpoint, key,
	).Scan(&existingHash, &status, &statusCode, &respData)
	if err != nil {
		return storage.IdempotencyLookup{}, fmt.Errorf("sqlite: lookup idempotency: %w", err)
	}

	if existingHash != requestHash {
		return storage.IdempotencyLookup{}, storage.ErrIdempotencyPayloadMismatch
	}

	if status == "completed" {
		var data json.RawMessage
		if respData.Valid {
			data = json.RawMessage(respData.String)
		}
		return storage.IdempotencyLookup{
			Completed:    true,
			StatusCode:   int(statusCode.Int64),
			ResponseData: data,
		}, nil
	}

	return storage.IdempotencyLookup{}, storage.ErrIdempotencyInProgress
}

// CompleteIdempotency marks an idempotency key as completed with the response.
func (l *LiteDB) CompleteIdempotency(ctx context.Context, orgID uuid.UUID, agentID, endpoint, key string, statusCode int, responseData any) error {
	respJSON, err := json.Marshal(responseData)
	if err != nil {
		return fmt.Errorf("sqlite: marshal response data: %w", err)
	}
	_, err = l.db.ExecContext(ctx,
		`UPDATE idempotency_keys
		 SET status = 'completed', status_code = ?, response_data = ?, updated_at = datetime('now')
		 WHERE org_id = ? AND agent_id = ? AND endpoint = ? AND idempotency_key = ?
		   AND status = 'in_progress'`,
		statusCode, string(respJSON),
		uuidStr(orgID), agentID, endpoint, key,
	)
	if err != nil {
		return fmt.Errorf("sqlite: complete idempotency: %w", err)
	}
	return nil
}

// ClearInProgressIdempotency removes an in-progress idempotency key (cleanup on error).
func (l *LiteDB) ClearInProgressIdempotency(ctx context.Context, orgID uuid.UUID, agentID, endpoint, key string) error {
	_, err := l.db.ExecContext(ctx,
		`DELETE FROM idempotency_keys
		 WHERE org_id = ? AND agent_id = ? AND endpoint = ? AND idempotency_key = ?
		   AND status = 'in_progress'`,
		uuidStr(orgID), agentID, endpoint, key,
	)
	if err != nil {
		return fmt.Errorf("sqlite: clear idempotency: %w", err)
	}
	return nil
}
