package search

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvector "github.com/pgvector/pgvector-go/pgx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ashita-ai/akashi/migrations"
)

// testPool is the shared connection pool for all integration tests in this file.
var testPool *pgxpool.Pool

// testLogger is the shared logger for tests.
var testLogger *slog.Logger

func TestMain(m *testing.M) {
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "timescale/timescaledb:latest-pg18",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "akashi",
			"POSTGRES_PASSWORD": "akashi",
			"POSTGRES_DB":       "akashi",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(60 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start container: %v\n", err)
		os.Exit(1)
	}

	host, err := container.Host(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get container host: %v\n", err)
		os.Exit(1)
	}

	port, err := container.MappedPort(ctx, "5432")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get container port: %v\n", err)
		os.Exit(1)
	}

	dsn := fmt.Sprintf("postgres://akashi:akashi@%s:%s/akashi?sslmode=disable", host, port.Port())

	// Bootstrap extensions before pool creation so pgvector types register.
	bootstrapConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to bootstrap connection: %v\n", err)
		os.Exit(1)
	}
	for _, ext := range []string{"vector", "timescaledb"} {
		if _, err := bootstrapConn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+ext); err != nil {
			fmt.Fprintf(os.Stderr, "failed to create %s extension: %v\n", ext, err)
			os.Exit(1)
		}
	}
	_ = bootstrapConn.Close(ctx)

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse pool config: %v\n", err)
		os.Exit(1)
	}
	poolCfg.AfterConnect = pgxvector.RegisterTypes

	testPool, err = pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create pool: %v\n", err)
		os.Exit(1)
	}

	// Run migrations using the embedded migration FS.
	if err := runMigrations(ctx, testPool, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "failed to run migrations: %v\n", err)
		os.Exit(1)
	}

	testLogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	code := m.Run()

	testPool.Close()
	_ = container.Terminate(ctx)
	os.Exit(code)
}

// runMigrations applies all SQL migration files from the embedded FS.
func runMigrations(ctx context.Context, pool *pgxpool.Pool, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect for migrations: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("read migration dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if len(name) < 5 || name[len(name)-4:] != ".sql" {
			continue
		}
		data, err := migrations.FS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := conn.Exec(ctx, string(data)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}

// defaultOrgID is the default organization created by the initial migration.
var defaultOrgID = uuid.MustParse("00000000-0000-0000-0000-000000000000")

// createTestRun inserts an agent_run and returns the run ID.
func createTestRun(ctx context.Context, t *testing.T, orgID uuid.UUID, agentID string) uuid.UUID {
	t.Helper()
	var runID uuid.UUID
	err := testPool.QueryRow(ctx,
		`INSERT INTO agent_runs (agent_id, status, org_id) VALUES ($1, 'running', $2) RETURNING id`,
		agentID, orgID,
	).Scan(&runID)
	require.NoError(t, err)
	return runID
}

// createTestDecision inserts a decision with an embedding and returns the decision ID.
func createTestDecision(ctx context.Context, t *testing.T, orgID uuid.UUID, agentID, decisionType string, embedding []float32) uuid.UUID {
	t.Helper()
	runID := createTestRun(ctx, t, orgID, agentID)
	var decID uuid.UUID
	emb := pgvector.NewVector(embedding)
	err := testPool.QueryRow(ctx,
		`INSERT INTO decisions (run_id, agent_id, decision_type, outcome, confidence, embedding, org_id, quality_score)
		 VALUES ($1, $2, $3, 'test outcome', 0.8, $4, $5, 0.5) RETURNING id`,
		runID, agentID, decisionType, emb, orgID,
	).Scan(&decID)
	require.NoError(t, err)
	return decID
}

// createTestDecisionWithSession inserts a decision with session_id and agent_context.
func createTestDecisionWithSession(ctx context.Context, t *testing.T, orgID uuid.UUID, agentID, decisionType string, embedding []float32, sessionID uuid.UUID) uuid.UUID {
	t.Helper()
	runID := createTestRun(ctx, t, orgID, agentID)
	var decID uuid.UUID
	emb := pgvector.NewVector(embedding)
	err := testPool.QueryRow(ctx,
		`INSERT INTO decisions (run_id, agent_id, decision_type, outcome, confidence, embedding, org_id, quality_score, session_id, agent_context)
		 VALUES ($1, $2, $3, 'test outcome', 0.8, $4, $5, 0.5, $6, $7) RETURNING id`,
		runID, agentID, decisionType, emb, orgID, sessionID, `{"tool":"claude-code","model":"opus"}`,
	).Scan(&decID)
	require.NoError(t, err)
	return decID
}

// createTestDecisionNoEmbedding inserts a decision without an embedding.
func createTestDecisionNoEmbedding(ctx context.Context, t *testing.T, orgID uuid.UUID, agentID, decisionType string) uuid.UUID {
	t.Helper()
	runID := createTestRun(ctx, t, orgID, agentID)
	var decID uuid.UUID
	err := testPool.QueryRow(ctx,
		`INSERT INTO decisions (run_id, agent_id, decision_type, outcome, confidence, org_id, quality_score)
		 VALUES ($1, $2, $3, 'test outcome', 0.8, $4, 0.5) RETURNING id`,
		runID, agentID, decisionType, orgID,
	).Scan(&decID)
	require.NoError(t, err)
	return decID
}

// insertOutboxEntry inserts a search_outbox entry and returns its ID.
func insertOutboxEntry(ctx context.Context, t *testing.T, decisionID, orgID uuid.UUID, operation string, attempts int) int64 {
	t.Helper()
	var id int64
	err := testPool.QueryRow(ctx,
		`INSERT INTO search_outbox (decision_id, org_id, operation, attempts)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		decisionID, orgID, operation, attempts,
	).Scan(&id)
	require.NoError(t, err)
	return id
}

// insertOutboxEntryOld inserts a search_outbox entry with an old created_at for cleanup tests.
func insertOutboxEntryOld(ctx context.Context, t *testing.T, decisionID, orgID uuid.UUID, operation string, attempts int, age time.Duration) int64 {
	t.Helper()
	var id int64
	err := testPool.QueryRow(ctx,
		`INSERT INTO search_outbox (decision_id, org_id, operation, attempts, created_at)
		 VALUES ($1, $2, $3, $4, now() - $5::interval) RETURNING id`,
		decisionID, orgID, operation, attempts, fmt.Sprintf("%d seconds", int(age.Seconds())),
	).Scan(&id)
	require.NoError(t, err)
	return id
}

// outboxEntryExists checks if an outbox entry with the given ID exists.
func outboxEntryExists(ctx context.Context, t *testing.T, id int64) bool {
	t.Helper()
	var exists bool
	err := testPool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM search_outbox WHERE id = $1)`, id,
	).Scan(&exists)
	require.NoError(t, err)
	return exists
}

// getOutboxEntry fetches an outbox entry by ID.
func getOutboxEntry(ctx context.Context, t *testing.T, id int64) (attempts int, lastError *string, lockedUntil *time.Time) {
	t.Helper()
	err := testPool.QueryRow(ctx,
		`SELECT attempts, last_error, locked_until FROM search_outbox WHERE id = $1`, id,
	).Scan(&attempts, &lastError, &lockedUntil)
	require.NoError(t, err)
	return
}

// cleanOutbox removes all entries from search_outbox to ensure test isolation.
func cleanOutbox(ctx context.Context, t *testing.T) {
	t.Helper()
	_, err := testPool.Exec(ctx, `DELETE FROM search_outbox`)
	require.NoError(t, err)
}

// newTestWorker creates an OutboxWorker with the test pool and nil index.
// The nil index means processUpserts/processDeletes will skip the Qdrant calls,
// but all DB-only functions can be exercised directly.
func newTestWorker() *OutboxWorker {
	return NewOutboxWorker(testPool, nil, testLogger, 100*time.Millisecond, 50)
}

// newTestWorkerWithIndex creates an OutboxWorker with the test pool and a
// QdrantIndex pointing to a non-existent server. This allows processBatch to
// proceed past the nil-index guard, exercising the full select/lock/process
// pipeline. Qdrant RPCs will fail, exercising the error-handling paths in
// processUpserts and processDeletes.
func newTestWorkerWithIndex(t *testing.T) *OutboxWorker {
	t.Helper()
	idx, err := NewQdrantIndex(QdrantConfig{
		URL:        "http://localhost:16335", // Non-standard port, no server.
		Collection: "test_outbox",
		Dims:       1024,
	}, testLogger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })
	return NewOutboxWorker(testPool, idx, testLogger, 100*time.Millisecond, 50)
}

func TestSucceedEntries(t *testing.T) {
	ctx := context.Background()
	cleanOutbox(ctx, t)

	decID1 := uuid.New()
	decID2 := uuid.New()
	orgID := defaultOrgID

	id1 := insertOutboxEntry(ctx, t, decID1, orgID, "upsert", 0)
	id2 := insertOutboxEntry(ctx, t, decID2, orgID, "delete", 2)

	// Verify both entries exist before succeed.
	require.True(t, outboxEntryExists(ctx, t, id1))
	require.True(t, outboxEntryExists(ctx, t, id2))

	w := newTestWorker()
	entries := []outboxEntry{
		{ID: id1, DecisionID: decID1, OrgID: orgID, Operation: "upsert", Attempts: 0},
		{ID: id2, DecisionID: decID2, OrgID: orgID, Operation: "delete", Attempts: 2},
	}

	w.succeedEntries(ctx, entries)

	// Both entries should be removed after success.
	assert.False(t, outboxEntryExists(ctx, t, id1), "entry 1 should be deleted after succeedEntries")
	assert.False(t, outboxEntryExists(ctx, t, id2), "entry 2 should be deleted after succeedEntries")
}

func TestDeferPendingEntries(t *testing.T) {
	ctx := context.Background()
	cleanOutbox(ctx, t)

	decID := uuid.New()
	orgID := defaultOrgID

	id := insertOutboxEntry(ctx, t, decID, orgID, "upsert", 3)

	w := newTestWorker()
	entries := []outboxEntry{
		{ID: id, DecisionID: decID, OrgID: orgID, Operation: "upsert", Attempts: 3},
	}

	w.deferPendingEntries(ctx, entries, "decision not ready")

	// Entry should still exist with incremented attempts and a locked_until in the future.
	attempts, lastErr, lockedUntil := getOutboxEntry(ctx, t, id)
	assert.Equal(t, 4, attempts, "attempts should be incremented by 1")
	require.NotNil(t, lastErr)
	assert.Equal(t, "decision not ready", *lastErr)
	require.NotNil(t, lockedUntil)
	assert.True(t, lockedUntil.After(time.Now()), "locked_until should be in the future")
	// The defer uses a 30-minute backoff, so locked_until should be ~30 minutes from now.
	assert.True(t, lockedUntil.After(time.Now().Add(25*time.Minute)),
		"locked_until should be at least 25 minutes from now (30-minute backoff)")
}

func TestFailEntries(t *testing.T) {
	ctx := context.Background()
	cleanOutbox(ctx, t)

	decID1 := uuid.New()
	decID2 := uuid.New()
	orgID := defaultOrgID

	id1 := insertOutboxEntry(ctx, t, decID1, orgID, "upsert", 0)
	id2 := insertOutboxEntry(ctx, t, decID2, orgID, "upsert", 5)

	w := newTestWorker()
	entries := []outboxEntry{
		{ID: id1, DecisionID: decID1, OrgID: orgID, Operation: "upsert", Attempts: 0},
		{ID: id2, DecisionID: decID2, OrgID: orgID, Operation: "upsert", Attempts: 5},
	}

	w.failEntries(ctx, entries, "qdrant unavailable")

	// Both entries should still exist with incremented attempts and error message.
	attempts1, lastErr1, lockedUntil1 := getOutboxEntry(ctx, t, id1)
	assert.Equal(t, 1, attempts1, "attempts should be incremented")
	require.NotNil(t, lastErr1)
	assert.Equal(t, "qdrant unavailable", *lastErr1)
	require.NotNil(t, lockedUntil1)
	assert.True(t, lockedUntil1.After(time.Now()), "locked_until should be in the future")

	attempts2, lastErr2, _ := getOutboxEntry(ctx, t, id2)
	assert.Equal(t, 6, attempts2)
	require.NotNil(t, lastErr2)
	assert.Equal(t, "qdrant unavailable", *lastErr2)
}

func TestFailEntries_ExponentialBackoff(t *testing.T) {
	ctx := context.Background()
	cleanOutbox(ctx, t)

	// Entry with 0 attempts: backoff = 2^(0+1) = 2 seconds
	decID1 := uuid.New()
	id1 := insertOutboxEntry(ctx, t, decID1, defaultOrgID, "upsert", 0)

	// Entry with 4 attempts: backoff = 2^(4+1) = 32 seconds
	decID2 := uuid.New()
	id2 := insertOutboxEntry(ctx, t, decID2, defaultOrgID, "upsert", 4)

	w := newTestWorker()

	w.failEntries(ctx, []outboxEntry{
		{ID: id1, DecisionID: decID1, OrgID: defaultOrgID, Operation: "upsert", Attempts: 0},
	}, "error")
	w.failEntries(ctx, []outboxEntry{
		{ID: id2, DecisionID: decID2, OrgID: defaultOrgID, Operation: "upsert", Attempts: 4},
	}, "error")

	_, _, locked1 := getOutboxEntry(ctx, t, id1)
	_, _, locked2 := getOutboxEntry(ctx, t, id2)

	require.NotNil(t, locked1)
	require.NotNil(t, locked2)

	// locked1 should be ~2 seconds from now; locked2 should be ~32 seconds from now.
	// Use wide bounds since DB clock may differ slightly.
	assert.True(t, locked1.Before(time.Now().Add(10*time.Second)),
		"low-attempt entry should have short backoff")
	assert.True(t, locked2.After(time.Now().Add(20*time.Second)),
		"high-attempt entry should have longer backoff")
}

func TestFetchDecisionsForIndex(t *testing.T) {
	ctx := context.Background()

	orgID := defaultOrgID
	embedding := make([]float32, 1024)
	for i := range embedding {
		embedding[i] = float32(i) * 0.001
	}

	decID := createTestDecision(ctx, t, orgID, "test-agent", "architecture", embedding)

	w := newTestWorker()

	decisions, err := w.fetchDecisionsForIndex(ctx, []uuid.UUID{decID}, []uuid.UUID{orgID})
	require.NoError(t, err)
	require.Len(t, decisions, 1)

	d := decisions[0]
	assert.Equal(t, decID, d.ID)
	assert.Equal(t, orgID, d.OrgID)
	assert.Equal(t, "test-agent", d.AgentID)
	assert.Equal(t, "architecture", d.DecisionType)
	assert.InDelta(t, 0.8, float64(d.Confidence), 0.01)
	assert.InDelta(t, 0.5, float64(d.QualityScore), 0.01)
	assert.False(t, d.ValidFrom.IsZero())
	require.Len(t, d.Embedding, 1024)
	assert.InDelta(t, 0.001, float64(d.Embedding[1]), 0.0001)
}

func TestFetchDecisionsForIndex_WithSession(t *testing.T) {
	ctx := context.Background()

	orgID := defaultOrgID
	sessionID := uuid.New()
	embedding := make([]float32, 1024)
	for i := range embedding {
		embedding[i] = 0.5
	}

	decID := createTestDecisionWithSession(ctx, t, orgID, "coder", "trade_off", embedding, sessionID)

	w := newTestWorker()

	decisions, err := w.fetchDecisionsForIndex(ctx, []uuid.UUID{decID}, []uuid.UUID{orgID})
	require.NoError(t, err)
	require.Len(t, decisions, 1)

	d := decisions[0]
	assert.Equal(t, decID, d.ID)
	require.NotNil(t, d.SessionID)
	assert.Equal(t, sessionID, *d.SessionID)
	require.NotNil(t, d.AgentContext)
	assert.Equal(t, "claude-code", d.AgentContext["tool"])
	assert.Equal(t, "opus", d.AgentContext["model"])
}

func TestFetchDecisionsForIndex_NoEmbedding(t *testing.T) {
	ctx := context.Background()

	orgID := defaultOrgID
	decID := createTestDecisionNoEmbedding(ctx, t, orgID, "test-agent", "security")

	w := newTestWorker()

	decisions, err := w.fetchDecisionsForIndex(ctx, []uuid.UUID{decID}, []uuid.UUID{orgID})
	require.NoError(t, err)
	// Decision without embedding IS returned by fetchDecisionsForIndex (#60).
	// partitionUpsertEntries will defer it until a backfill provides an embedding.
	require.Len(t, decisions, 1, "decision without embedding should be fetched")
	assert.Equal(t, decID, decisions[0].ID)
	assert.Nil(t, decisions[0].Embedding, "embedding should be nil")
}

func TestFetchDecisionsForIndex_EmptyInput(t *testing.T) {
	ctx := context.Background()
	w := newTestWorker()

	// Empty slices should return nil without error.
	decisions, err := w.fetchDecisionsForIndex(ctx, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, decisions)

	// Mismatched lengths should return nil.
	decisions, err = w.fetchDecisionsForIndex(ctx, []uuid.UUID{uuid.New()}, nil)
	require.NoError(t, err)
	assert.Nil(t, decisions)
}

func TestFetchDecisionsForIndex_WrongOrg(t *testing.T) {
	ctx := context.Background()

	orgID := defaultOrgID
	embedding := make([]float32, 1024)
	decID := createTestDecision(ctx, t, orgID, "test-agent", "architecture", embedding)

	w := newTestWorker()

	// Query with a different org_id should return nothing.
	otherOrg := uuid.New()
	decisions, err := w.fetchDecisionsForIndex(ctx, []uuid.UUID{decID}, []uuid.UUID{otherOrg})
	require.NoError(t, err)
	assert.Empty(t, decisions, "decision from different org should not be returned")
}

func TestCleanupDeadLetters(t *testing.T) {
	ctx := context.Background()
	cleanOutbox(ctx, t)

	orgID := defaultOrgID
	decID1 := uuid.New()
	decID2 := uuid.New()
	decID3 := uuid.New()

	// Old dead-letter entry: max attempts, created 8 days ago. Should be cleaned.
	id1 := insertOutboxEntryOld(ctx, t, decID1, orgID, "upsert", maxOutboxAttempts, 8*24*time.Hour)

	// Recent dead-letter entry: max attempts, created 1 day ago. Should NOT be cleaned
	// (less than 7 days old).
	id2 := insertOutboxEntryOld(ctx, t, decID2, orgID, "upsert", maxOutboxAttempts, 1*24*time.Hour)

	// Old entry but below max attempts: created 8 days ago, 5 attempts. Should NOT be cleaned.
	id3 := insertOutboxEntryOld(ctx, t, decID3, orgID, "upsert", 5, 8*24*time.Hour)

	w := newTestWorker()
	w.cleanupDeadLetters(ctx)

	assert.False(t, outboxEntryExists(ctx, t, id1),
		"old dead-letter entry (max attempts, >7 days) should be removed")
	assert.True(t, outboxEntryExists(ctx, t, id2),
		"recent dead-letter entry (max attempts, <7 days) should be kept")
	assert.True(t, outboxEntryExists(ctx, t, id3),
		"old entry with low attempts should be kept")
}

func TestCleanupDeadLetters_NoEntries(t *testing.T) {
	ctx := context.Background()
	cleanOutbox(ctx, t)

	// Cleanup with no entries should not error.
	w := newTestWorker()
	w.cleanupDeadLetters(ctx)
	// If we reach here without panic, the test passes.
}

func TestProcessBatch_NilIndex(t *testing.T) {
	ctx := context.Background()
	cleanOutbox(ctx, t)

	// With nil index, processBatch should skip processing and log a warning.
	w := NewOutboxWorker(testPool, nil, testLogger, 100*time.Millisecond, 50)
	w.processBatch(ctx) // Should not panic, just log and return.
}

func TestProcessBatch_NilPool(t *testing.T) {
	ctx := context.Background()

	// With nil pool, processBatch should skip processing and log a warning.
	w := NewOutboxWorker(nil, nil, testLogger, 100*time.Millisecond, 50)
	w.processBatch(ctx) // Should not panic, just log and return.
}

func TestProcessBatch_EmptyOutbox(t *testing.T) {
	ctx := context.Background()
	cleanOutbox(ctx, t)

	// processBatch with an empty outbox should return cleanly. Even though
	// index is nil, the empty outbox is detected before reaching the nil check
	// for upserts/deletes (len(entries) == 0 returns early).
	// But we actually need a non-nil index for the nil check to pass...
	// processBatch checks pool then index first. With nil index, it returns early.
	// So this test demonstrates the early return path with empty outbox and nil index.
	w := NewOutboxWorker(testPool, nil, testLogger, 100*time.Millisecond, 50)
	w.processBatch(ctx) // Returns early because index is nil.
}

func TestProcessBatch_SelectsAndLocksEntries(t *testing.T) {
	ctx := context.Background()
	cleanOutbox(ctx, t)

	orgID := defaultOrgID
	embedding := make([]float32, 1024)

	// Create decisions with embeddings so the outbox entries are "real".
	decID1 := createTestDecision(ctx, t, orgID, "agent-a", "architecture", embedding)
	decID2 := createTestDecision(ctx, t, orgID, "agent-b", "security", embedding)

	// Insert outbox entries for these decisions. Use the unique index's ON CONFLICT
	// behavior by using unique (decision_id, operation) pairs.
	id1 := insertOutboxEntry(ctx, t, decID1, orgID, "upsert", 0)
	id2 := insertOutboxEntry(ctx, t, decID2, orgID, "delete", 0)

	// We cannot test the full processBatch with nil index because it returns early.
	// Instead, test that the entries were selected and locked by manually running
	// the SELECT + lock logic from processBatch.
	tx, err := testPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT id, decision_id, org_id, operation, attempts
		 FROM search_outbox
		 WHERE (locked_until IS NULL OR locked_until < now())
		   AND attempts < $1
		 ORDER BY created_at ASC
		 LIMIT $2
		 FOR UPDATE SKIP LOCKED`,
		maxOutboxAttempts, 50,
	)
	require.NoError(t, err)

	entries, err := scanOutboxEntries(rows)
	require.NoError(t, err)
	require.Len(t, entries, 2, "should select both pending entries")

	// Verify the entries match what we inserted.
	entryIDs := map[int64]bool{id1: false, id2: false}
	for _, e := range entries {
		entryIDs[e.ID] = true
	}
	assert.True(t, entryIDs[id1], "entry 1 should be selected")
	assert.True(t, entryIDs[id2], "entry 2 should be selected")

	_ = tx.Rollback(ctx)
}

func TestProcessBatch_SkipsLockedEntries(t *testing.T) {
	ctx := context.Background()
	cleanOutbox(ctx, t)

	orgID := defaultOrgID
	decID := uuid.New()

	// Insert an entry that is locked until far in the future.
	var id int64
	err := testPool.QueryRow(ctx,
		`INSERT INTO search_outbox (decision_id, org_id, operation, attempts, locked_until)
		 VALUES ($1, $2, 'upsert', 0, now() + interval '1 hour') RETURNING id`,
		decID, orgID,
	).Scan(&id)
	require.NoError(t, err)

	// The processBatch SELECT should skip this entry because locked_until > now().
	tx, err := testPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT id, decision_id, org_id, operation, attempts
		 FROM search_outbox
		 WHERE (locked_until IS NULL OR locked_until < now())
		   AND attempts < $1
		 ORDER BY created_at ASC
		 LIMIT $2
		 FOR UPDATE SKIP LOCKED`,
		maxOutboxAttempts, 50,
	)
	require.NoError(t, err)

	entries, err := scanOutboxEntries(rows)
	require.NoError(t, err)
	assert.Empty(t, entries, "locked entry should be skipped")

	_ = tx.Rollback(ctx)
}

func TestProcessBatch_SkipsMaxAttempts(t *testing.T) {
	ctx := context.Background()
	cleanOutbox(ctx, t)

	orgID := defaultOrgID
	decID := uuid.New()

	// Insert an entry that has reached max attempts.
	insertOutboxEntry(ctx, t, decID, orgID, "upsert", maxOutboxAttempts)

	tx, err := testPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT id, decision_id, org_id, operation, attempts
		 FROM search_outbox
		 WHERE (locked_until IS NULL OR locked_until < now())
		   AND attempts < $1
		 ORDER BY created_at ASC
		 LIMIT $2
		 FOR UPDATE SKIP LOCKED`,
		maxOutboxAttempts, 50,
	)
	require.NoError(t, err)

	entries, err := scanOutboxEntries(rows)
	require.NoError(t, err)
	assert.Empty(t, entries, "entry at max attempts should be skipped")

	_ = tx.Rollback(ctx)
}

func TestFetchDecisionsForIndex_MultipleDecisions(t *testing.T) {
	ctx := context.Background()

	orgID := defaultOrgID
	embedding := make([]float32, 1024)

	decID1 := createTestDecision(ctx, t, orgID, "planner", "architecture", embedding)
	decID2 := createTestDecision(ctx, t, orgID, "coder", "trade_off", embedding)
	decID3 := createTestDecision(ctx, t, orgID, "reviewer", "code_review", embedding)

	w := newTestWorker()

	decisions, err := w.fetchDecisionsForIndex(ctx,
		[]uuid.UUID{decID1, decID2, decID3},
		[]uuid.UUID{orgID, orgID, orgID},
	)
	require.NoError(t, err)
	require.Len(t, decisions, 3)

	// Verify all decision IDs are present.
	ids := make(map[uuid.UUID]bool, 3)
	for _, d := range decisions {
		ids[d.ID] = true
	}
	assert.True(t, ids[decID1])
	assert.True(t, ids[decID2])
	assert.True(t, ids[decID3])
}

func TestFetchDecisionsForIndex_MixedEmbeddings(t *testing.T) {
	ctx := context.Background()

	orgID := defaultOrgID
	embedding := make([]float32, 1024)

	// One decision with embedding, one without.
	decWithEmb := createTestDecision(ctx, t, orgID, "agent-a", "architecture", embedding)
	decNoEmb := createTestDecisionNoEmbedding(ctx, t, orgID, "agent-b", "security")

	w := newTestWorker()

	decisions, err := w.fetchDecisionsForIndex(ctx,
		[]uuid.UUID{decWithEmb, decNoEmb},
		[]uuid.UUID{orgID, orgID},
	)
	require.NoError(t, err)
	// Both decisions are returned (#60). partitionUpsertEntries separates
	// ready (has embedding) from deferred (nil embedding).
	require.Len(t, decisions, 2, "both decisions should be fetched")

	// Verify the one with embedding has it, and the one without has nil.
	byID := make(map[uuid.UUID]DecisionForIndex, 2)
	for _, d := range decisions {
		byID[d.ID] = d
	}
	assert.NotNil(t, byID[decWithEmb].Embedding, "decision with embedding should have it")
	assert.Nil(t, byID[decNoEmb].Embedding, "decision without embedding should have nil")
}

func TestOutboxWorker_FullCycle(t *testing.T) {
	// Test the full worker lifecycle: start, insert outbox entries, verify they
	// get picked up by the poll loop, and drain. Since the index is nil, the
	// worker will log warnings but the poll loop will still run and attempt to
	// process batches.
	ctx := context.Background()
	cleanOutbox(ctx, t)

	w := NewOutboxWorker(testPool, nil, testLogger, 50*time.Millisecond, 50)

	bgCtx, bgCancel := context.WithCancel(ctx)
	defer bgCancel()

	w.Start(bgCtx)
	assert.True(t, w.started.Load())

	// Let the worker tick a couple of times.
	time.Sleep(200 * time.Millisecond)

	// Drain should complete cleanly.
	drainCtx, drainCancel := context.WithTimeout(ctx, 3*time.Second)
	defer drainCancel()
	w.Drain(drainCtx)

	// Verify the done channel is closed.
	select {
	case <-w.done:
		// Success.
	default:
		t.Fatal("done channel should be closed after drain")
	}
}

func TestSucceedEntries_SingleEntry(t *testing.T) {
	ctx := context.Background()
	cleanOutbox(ctx, t)

	decID := uuid.New()
	id := insertOutboxEntry(ctx, t, decID, defaultOrgID, "delete", 1)

	w := newTestWorker()
	w.succeedEntries(ctx, []outboxEntry{
		{ID: id, DecisionID: decID, OrgID: defaultOrgID, Operation: "delete", Attempts: 1},
	})

	assert.False(t, outboxEntryExists(ctx, t, id))
}

func TestDeferPendingEntries_MultipleEntries(t *testing.T) {
	ctx := context.Background()
	cleanOutbox(ctx, t)

	decID1 := uuid.New()
	decID2 := uuid.New()

	id1 := insertOutboxEntry(ctx, t, decID1, defaultOrgID, "upsert", 0)
	id2 := insertOutboxEntry(ctx, t, decID2, defaultOrgID, "upsert", 2)

	w := newTestWorker()
	w.deferPendingEntries(ctx, []outboxEntry{
		{ID: id1, DecisionID: decID1, OrgID: defaultOrgID, Operation: "upsert", Attempts: 0},
		{ID: id2, DecisionID: decID2, OrgID: defaultOrgID, Operation: "upsert", Attempts: 2},
	}, "backfill pending")

	attempts1, lastErr1, _ := getOutboxEntry(ctx, t, id1)
	assert.Equal(t, 1, attempts1)
	require.NotNil(t, lastErr1)
	assert.Equal(t, "backfill pending", *lastErr1)

	attempts2, lastErr2, _ := getOutboxEntry(ctx, t, id2)
	assert.Equal(t, 3, attempts2)
	require.NotNil(t, lastErr2)
	assert.Equal(t, "backfill pending", *lastErr2)
}

func TestFailEntries_DeadLetterLogging(t *testing.T) {
	// When an entry's attempts + 1 >= maxOutboxAttempts, it becomes a dead-letter.
	// This test verifies the entry is still updated correctly even at the threshold.
	ctx := context.Background()
	cleanOutbox(ctx, t)

	decID := uuid.New()
	id := insertOutboxEntry(ctx, t, decID, defaultOrgID, "upsert", maxOutboxAttempts-1)

	w := newTestWorker()
	w.failEntries(ctx, []outboxEntry{
		{ID: id, DecisionID: decID, OrgID: defaultOrgID, Operation: "upsert", Attempts: maxOutboxAttempts - 1},
	}, "final failure")

	attempts, lastErr, lockedUntil := getOutboxEntry(ctx, t, id)
	assert.Equal(t, maxOutboxAttempts, attempts, "should reach max attempts")
	require.NotNil(t, lastErr)
	assert.Equal(t, "final failure", *lastErr)
	require.NotNil(t, lockedUntil)
	// At max attempts, backoff = LEAST(2^10, 300) = 300 seconds = 5 minutes.
	assert.True(t, lockedUntil.After(time.Now().Add(4*time.Minute)),
		"dead-letter entry should have max backoff (~5 min)")
}

func TestCleanupDeadLetters_LockedEntryNotCleaned(t *testing.T) {
	ctx := context.Background()
	cleanOutbox(ctx, t)

	decID := uuid.New()

	// Insert an old dead-letter entry that is still locked.
	var id int64
	err := testPool.QueryRow(ctx,
		`INSERT INTO search_outbox (decision_id, org_id, operation, attempts, created_at, locked_until)
		 VALUES ($1, $2, 'upsert', $3, now() - interval '8 days', now() + interval '1 hour') RETURNING id`,
		decID, defaultOrgID, maxOutboxAttempts,
	).Scan(&id)
	require.NoError(t, err)

	w := newTestWorker()
	w.cleanupDeadLetters(ctx)

	// Entry should NOT be cleaned because it's still locked.
	assert.True(t, outboxEntryExists(ctx, t, id),
		"locked dead-letter entry should not be cleaned")
}

func TestProcessBatch_WithIndex_Upserts(t *testing.T) {
	// Tests the full processBatch pipeline with a non-nil QdrantIndex.
	// Entries are selected, locked, fetched, and sent to Qdrant. Since Qdrant
	// is unreachable, the upsert fails and entries are marked as failed via failEntries.
	ctx := context.Background()
	cleanOutbox(ctx, t)

	orgID := defaultOrgID
	embedding := make([]float32, 1024)
	for i := range embedding {
		embedding[i] = float32(i) * 0.001
	}

	decID := createTestDecision(ctx, t, orgID, "agent-x", "architecture", embedding)
	id := insertOutboxEntry(ctx, t, decID, orgID, "upsert", 0)

	w := newTestWorkerWithIndex(t)
	w.lastCleanup = time.Now() // Prevent cleanup from running.

	batchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	w.processBatch(batchCtx)

	// The Qdrant upsert will fail because no server is running.
	// The entry should be marked as failed (attempts incremented, last_error set).
	attempts, lastErr, _ := getOutboxEntry(ctx, t, id)
	assert.Equal(t, 1, attempts, "attempts should be incremented after failed upsert")
	require.NotNil(t, lastErr)
	assert.Contains(t, *lastErr, "qdrant upsert", "error should reference qdrant upsert failure")
}

func TestProcessBatch_WithIndex_Deletes(t *testing.T) {
	// Tests processBatch with delete entries. The Qdrant delete will fail,
	// exercising the processDeletes error path.
	ctx := context.Background()
	cleanOutbox(ctx, t)

	orgID := defaultOrgID
	decID := uuid.New() // No actual decision needed for deletes.
	id := insertOutboxEntry(ctx, t, decID, orgID, "delete", 0)

	w := newTestWorkerWithIndex(t)
	w.lastCleanup = time.Now()

	batchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	w.processBatch(batchCtx)

	// The Qdrant delete will fail because no server is running.
	attempts, lastErr, _ := getOutboxEntry(ctx, t, id)
	assert.Equal(t, 1, attempts, "attempts should be incremented after failed delete")
	require.NotNil(t, lastErr)
	assert.Contains(t, *lastErr, "qdrant delete", "error should reference qdrant delete failure")
}

func TestProcessBatch_WithIndex_MixedOperations(t *testing.T) {
	// Tests processBatch with both upsert and delete entries in the same batch.
	ctx := context.Background()
	cleanOutbox(ctx, t)

	orgID := defaultOrgID
	embedding := make([]float32, 1024)

	decID1 := createTestDecision(ctx, t, orgID, "agent-1", "architecture", embedding)
	decID2 := uuid.New()

	id1 := insertOutboxEntry(ctx, t, decID1, orgID, "upsert", 0)
	id2 := insertOutboxEntry(ctx, t, decID2, orgID, "delete", 0)

	w := newTestWorkerWithIndex(t)
	w.lastCleanup = time.Now()

	batchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	w.processBatch(batchCtx)

	// Both should fail with incremented attempts.
	attempts1, lastErr1, _ := getOutboxEntry(ctx, t, id1)
	assert.Equal(t, 1, attempts1)
	require.NotNil(t, lastErr1)

	attempts2, lastErr2, _ := getOutboxEntry(ctx, t, id2)
	assert.Equal(t, 1, attempts2)
	require.NotNil(t, lastErr2)
}

func TestProcessBatch_WithIndex_PendingEntries(t *testing.T) {
	// Tests the pending entry path: outbox entry references a decision that
	// has no embedding. The entry should be deferred (not failed).
	ctx := context.Background()
	cleanOutbox(ctx, t)

	orgID := defaultOrgID
	decID := createTestDecisionNoEmbedding(ctx, t, orgID, "agent-noEmb", "planning")
	id := insertOutboxEntry(ctx, t, decID, orgID, "upsert", 0)

	w := newTestWorkerWithIndex(t)
	w.lastCleanup = time.Now()

	batchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	w.processBatch(batchCtx)

	// The entry should be deferred (not failed), because the decision exists
	// but has no embedding. Attempts incremented, locked_until set to ~30 min.
	attempts, lastErr, lockedUntil := getOutboxEntry(ctx, t, id)
	assert.Equal(t, 1, attempts, "attempts should be incremented for deferred entry")
	require.NotNil(t, lastErr)
	assert.Contains(t, *lastErr, "not ready")
	require.NotNil(t, lockedUntil)
	assert.True(t, lockedUntil.After(time.Now().Add(25*time.Minute)),
		"deferred entry should have ~30 minute lockout")
}

func TestProcessBatch_WithIndex_PendingMaxAttempts(t *testing.T) {
	// Tests that a pending entry at max-1 attempts gets failed (not deferred).
	ctx := context.Background()
	cleanOutbox(ctx, t)

	orgID := defaultOrgID
	decID := createTestDecisionNoEmbedding(ctx, t, orgID, "agent-stale", "planning")
	id := insertOutboxEntry(ctx, t, decID, orgID, "upsert", maxOutboxAttempts-1)

	w := newTestWorkerWithIndex(t)
	w.lastCleanup = time.Now()

	batchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	w.processBatch(batchCtx)

	// At max-1 attempts, the pending entry should be failed (not deferred).
	attempts, lastErr, _ := getOutboxEntry(ctx, t, id)
	assert.Equal(t, maxOutboxAttempts, attempts)
	require.NotNil(t, lastErr)
	assert.Contains(t, *lastErr, "not ready after max defer cycles")
}

func TestProcessBatch_WithIndex_EmptyOutbox(t *testing.T) {
	// Tests processBatch when the outbox is empty. Should return cleanly
	// without any Qdrant calls.
	ctx := context.Background()
	cleanOutbox(ctx, t)

	w := newTestWorkerWithIndex(t)

	batchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	w.processBatch(batchCtx)
	// If we reach here without panic or hang, the test passes.
}

func TestProcessBatch_TriggersCleanup(t *testing.T) {
	// Tests that processBatch triggers cleanupDeadLetters when lastCleanup is old.
	// Cleanup runs only after processing at least one entry, so we insert both
	// a dead-letter entry (to be cleaned) and a processable entry (to ensure
	// the batch doesn't return early at the len(entries)==0 check).
	ctx := context.Background()
	cleanOutbox(ctx, t)

	orgID := defaultOrgID

	// Insert an old dead-letter entry that should be cleaned.
	deadLetterDecID := uuid.New()
	deadLetterID := insertOutboxEntryOld(ctx, t, deadLetterDecID, orgID, "upsert", maxOutboxAttempts, 8*24*time.Hour)

	// Insert a processable entry so the batch proceeds past len(entries)==0.
	processableDecID := uuid.New()
	insertOutboxEntry(ctx, t, processableDecID, orgID, "delete", 0)

	w := newTestWorkerWithIndex(t)
	// Set lastCleanup to >1 hour ago to trigger cleanup.
	w.lastCleanup = time.Now().Add(-2 * time.Hour)

	batchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	w.processBatch(batchCtx)

	// The dead-letter entry should have been cleaned up.
	assert.False(t, outboxEntryExists(ctx, t, deadLetterID),
		"old dead-letter entry should be cleaned during processBatch")
}

func TestOutboxWorker_FullCycleWithIndex(t *testing.T) {
	// Full lifecycle test with a non-nil QdrantIndex. The worker starts,
	// processes a few ticks (all Qdrant calls fail), and drains cleanly.
	ctx := context.Background()
	cleanOutbox(ctx, t)

	// Insert an outbox entry that will be picked up.
	orgID := defaultOrgID
	decID := uuid.New()
	insertOutboxEntry(ctx, t, decID, orgID, "delete", 0)

	w := newTestWorkerWithIndex(t)

	bgCtx, bgCancel := context.WithCancel(ctx)
	defer bgCancel()

	w.Start(bgCtx)
	assert.True(t, w.started.Load())

	// Let the worker tick a couple of times.
	time.Sleep(300 * time.Millisecond)

	drainCtx, drainCancel := context.WithTimeout(ctx, 5*time.Second)
	defer drainCancel()
	w.Drain(drainCtx)

	select {
	case <-w.done:
		// Success.
	default:
		t.Fatal("done channel should be closed after drain")
	}
}
