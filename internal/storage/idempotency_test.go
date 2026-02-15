package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/storage"
)

func TestIdempotency_ReplayAndMismatch(t *testing.T) {
	ctx := context.Background()
	orgID := uuid.Nil
	agentID := "idem-agent-" + uuid.NewString()[:8]
	endpoint := "POST:/v1/trace"
	key := "idem-" + uuid.NewString()

	lookup, err := testDB.BeginIdempotency(ctx, orgID, agentID, endpoint, key, "hash-a")
	require.NoError(t, err)
	assert.False(t, lookup.Completed)

	err = testDB.CompleteIdempotency(ctx, orgID, agentID, endpoint, key, 201, map[string]any{"decision_id": "d1"})
	require.NoError(t, err)

	replay, err := testDB.BeginIdempotency(ctx, orgID, agentID, endpoint, key, "hash-a")
	require.NoError(t, err)
	assert.True(t, replay.Completed)
	assert.Equal(t, 201, replay.StatusCode)
	require.NotEmpty(t, replay.ResponseData)

	_, err = testDB.BeginIdempotency(ctx, orgID, agentID, endpoint, key, "hash-b")
	require.ErrorIs(t, err, storage.ErrIdempotencyPayloadMismatch)
}

func TestIdempotency_StaleInProgressBlocksRetry(t *testing.T) {
	ctx := context.Background()
	orgID := uuid.Nil
	agentID := "idem-agent-" + uuid.NewString()[:8]
	endpoint := "POST:/v1/runs/" + uuid.NewString() + "/events"
	key := "idem-" + uuid.NewString()

	_, err := testDB.BeginIdempotency(ctx, orgID, agentID, endpoint, key, "hash-a")
	require.NoError(t, err)

	// In-progress key blocks retry regardless of staleness (no takeover).
	_, err = testDB.BeginIdempotency(ctx, orgID, agentID, endpoint, key, "hash-a")
	require.ErrorIs(t, err, storage.ErrIdempotencyInProgress)

	// Even after the key is artificially aged, it still blocks — the cleanup
	// job must remove it before the retry can proceed.
	_, err = testDB.Pool().Exec(ctx,
		`UPDATE idempotency_keys SET updated_at = now() - interval '20 minutes'
		 WHERE org_id = $1 AND agent_id = $2 AND endpoint = $3 AND idempotency_key = $4`,
		orgID, agentID, endpoint, key,
	)
	require.NoError(t, err)

	_, err = testDB.BeginIdempotency(ctx, orgID, agentID, endpoint, key, "hash-a")
	require.ErrorIs(t, err, storage.ErrIdempotencyInProgress, "stale in-progress keys must not be taken over")
}

func TestIdempotency_Cleanup(t *testing.T) {
	ctx := context.Background()
	orgID := uuid.Nil
	agentID := "idem-agent-" + uuid.NewString()[:8]

	// Seed one old completed key and one old in-progress key.
	_, err := testDB.Pool().Exec(ctx,
		`INSERT INTO idempotency_keys (org_id, agent_id, endpoint, idempotency_key, request_hash, status, status_code, response_data, created_at, updated_at)
		 VALUES
		 ($1, $2, 'POST:/v1/trace', 'old-completed', 'h1', 'completed', 201, '{"ok":true}', now() - interval '10 days', now() - interval '10 days'),
		 ($1, $2, 'POST:/v1/trace', 'old-in-progress', 'h2', 'in_progress', NULL, NULL, now() - interval '3 days', now() - interval '3 days')`,
		orgID, agentID,
	)
	require.NoError(t, err)

	deleted, err := testDB.CleanupIdempotencyKeys(ctx, 7*24*time.Hour, 24*time.Hour)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, deleted, int64(2))

	var remaining int
	err = testDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM idempotency_keys
		 WHERE org_id = $1 AND agent_id = $2 AND idempotency_key IN ('old-completed', 'old-in-progress')`,
		orgID, agentID,
	).Scan(&remaining)
	require.NoError(t, err)
	assert.Equal(t, 0, remaining)
}
