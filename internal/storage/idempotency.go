package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrIdempotencyPayloadMismatch is returned when the same idempotency key is reused
	// with a different request payload hash for the same (org, agent, endpoint).
	ErrIdempotencyPayloadMismatch = errors.New("idempotency key reused with different payload")
	// ErrIdempotencyInProgress indicates a matching idempotency key is currently being processed.
	ErrIdempotencyInProgress = errors.New("idempotency key request already in progress")
)

// IdempotencyLookup describes the current state of an idempotency key lookup.
type IdempotencyLookup struct {
	Completed    bool
	StatusCode   int
	ResponseData json.RawMessage
}

// BeginIdempotency reserves a key for processing.
//
// If this call returns (lookup, nil) with lookup.Completed=true, callers should replay
// the stored response instead of executing the operation again.
// If it returns ErrIdempotencyInProgress, another request is actively processing this key.
//
// Stale in-progress keys are NOT taken over — they block retries until the
// background CleanupIdempotencyKeys job removes them. This prevents duplicate
// mutations when the original request committed its work but crashed before
// calling CompleteIdempotency (see issue #57).
func (db *DB) BeginIdempotency(
	ctx context.Context,
	orgID uuid.UUID,
	agentID, endpoint, key, requestHash string,
) (IdempotencyLookup, error) {
	tag, err := db.pool.Exec(ctx,
		`INSERT INTO idempotency_keys (org_id, agent_id, endpoint, idempotency_key, request_hash, status)
		 VALUES ($1, $2, $3, $4, $5, 'in_progress')
		 ON CONFLICT DO NOTHING`,
		orgID, agentID, endpoint, key, requestHash,
	)
	if err != nil {
		return IdempotencyLookup{}, fmt.Errorf("storage: begin idempotency: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return IdempotencyLookup{}, nil // caller owns processing
	}

	var (
		storedHash   string
		status       string
		statusCode   *int
		responseData []byte
	)
	if err := db.pool.QueryRow(ctx,
		`SELECT request_hash, status, status_code, response_data
		 FROM idempotency_keys
		 WHERE org_id = $1 AND agent_id = $2 AND endpoint = $3 AND idempotency_key = $4`,
		orgID, agentID, endpoint, key,
	).Scan(&storedHash, &status, &statusCode, &responseData); err != nil {
		return IdempotencyLookup{}, fmt.Errorf("storage: lookup idempotency: %w", err)
	}

	if storedHash != requestHash {
		return IdempotencyLookup{}, ErrIdempotencyPayloadMismatch
	}
	if status == "completed" {
		code := 0
		if statusCode != nil {
			code = *statusCode
		}
		return IdempotencyLookup{
			Completed:    true,
			StatusCode:   code,
			ResponseData: responseData,
		}, nil
	}
	return IdempotencyLookup{}, ErrIdempotencyInProgress
}

// CompleteIdempotency stores the final response for a previously reserved key.
func (db *DB) CompleteIdempotency(
	ctx context.Context,
	orgID uuid.UUID,
	agentID, endpoint, key string,
	statusCode int,
	responseData any,
) error {
	payload, err := json.Marshal(responseData)
	if err != nil {
		return fmt.Errorf("storage: marshal idempotency response: %w", err)
	}

	tag, err := db.pool.Exec(ctx,
		`UPDATE idempotency_keys
		 SET status = 'completed',
		     status_code = $5,
		     response_data = $6::jsonb,
		     updated_at = now()
		 WHERE org_id = $1 AND agent_id = $2 AND endpoint = $3 AND idempotency_key = $4
		   AND status = 'in_progress'`,
		orgID, agentID, endpoint, key, statusCode, payload,
	)
	if err != nil {
		return fmt.Errorf("storage: complete idempotency: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("storage: complete idempotency: key not found or not in_progress")
	}
	return nil
}

// ClearInProgressIdempotency removes an in-progress reservation so the client can retry.
func (db *DB) ClearInProgressIdempotency(
	ctx context.Context,
	orgID uuid.UUID,
	agentID, endpoint, key string,
) error {
	_, err := db.pool.Exec(ctx,
		`DELETE FROM idempotency_keys
		 WHERE org_id = $1 AND agent_id = $2 AND endpoint = $3 AND idempotency_key = $4
		   AND status = 'in_progress'`,
		orgID, agentID, endpoint, key,
	)
	if err != nil {
		return fmt.Errorf("storage: clear idempotency: %w", err)
	}
	return nil
}

// CleanupIdempotencyKeys removes old completed records and abandoned in-progress records.
func (db *DB) CleanupIdempotencyKeys(
	ctx context.Context,
	completedTTL, inProgressTTL time.Duration,
) (int64, error) {
	tag, err := db.pool.Exec(ctx,
		`DELETE FROM idempotency_keys
		 WHERE (status = 'completed' AND updated_at < now() - ($1 * interval '1 microsecond'))
		    OR (status = 'in_progress' AND updated_at < now() - ($2 * interval '1 microsecond'))`,
		completedTTL.Microseconds(), inProgressTTL.Microseconds(),
	)
	if err != nil {
		return 0, fmt.Errorf("storage: cleanup idempotency keys: %w", err)
	}
	return tag.RowsAffected(), nil
}
