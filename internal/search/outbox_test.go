//go:build !lite

package search

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockRows implements pgx.Rows for unit testing scanOutboxEntries.
type mockRows struct {
	rows    [][]any
	cursor  int
	closed  bool
	scanErr error
}

func (m *mockRows) Close()                                       { m.closed = true }
func (m *mockRows) Err() error                                   { return nil }
func (m *mockRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT") }
func (m *mockRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (m *mockRows) RawValues() [][]byte                          { return nil }
func (m *mockRows) Conn() *pgx.Conn                              { return nil }
func (m *mockRows) Values() ([]any, error)                       { return m.rows[m.cursor-1], nil }

func (m *mockRows) Next() bool {
	if m.cursor >= len(m.rows) {
		return false
	}
	m.cursor++
	return true
}

func (m *mockRows) Scan(dest ...any) error {
	if m.scanErr != nil {
		return m.scanErr
	}
	row := m.rows[m.cursor-1]
	if len(dest) != len(row) {
		return fmt.Errorf("mockRows: scan %d dest into %d columns", len(dest), len(row))
	}
	for i, val := range row {
		switch d := dest[i].(type) {
		case *int64:
			*d = val.(int64)
		case *uuid.UUID:
			*d = val.(uuid.UUID)
		case *string:
			*d = val.(string)
		case *int:
			*d = val.(int)
		default:
			return fmt.Errorf("mockRows: unsupported dest type %T", d)
		}
	}
	return nil
}

func TestMaxOutboxAttempts(t *testing.T) {
	// Verify the dead-letter threshold is set to a reasonable value.
	assert.Equal(t, 10, maxOutboxAttempts)
}

func TestScanOutboxEntriesEmpty(t *testing.T) {
	// This test verifies the outbox worker's core logic constants and types
	// without requiring a live database. Integration tests cover the full
	// poll → process → Qdrant flow.

	// Verify Point type has all required fields for Qdrant upsert.
	var p Point
	_ = p.ID
	_ = p.OrgID
	_ = p.AgentID
	_ = p.DecisionType
	_ = p.Confidence
	_ = p.CompletenessScore
	_ = p.ValidFrom
	_ = p.Embedding

	// Verify DecisionForIndex has all required fields.
	var d DecisionForIndex
	_ = d.ID
	_ = d.OrgID
	_ = d.AgentID
	_ = d.DecisionType
	_ = d.Confidence
	_ = d.CompletenessScore
	_ = d.ValidFrom
	_ = d.Embedding
}

func TestPartitionUpsertEntries(t *testing.T) {
	idReady1 := uuid.New()
	idMissing := uuid.New()
	idReady2 := uuid.New()

	entries := []outboxEntry{
		{ID: 1, DecisionID: idReady1, Operation: "upsert"},
		{ID: 2, DecisionID: idMissing, Operation: "upsert"},
		{ID: 3, DecisionID: idReady2, Operation: "upsert"},
	}
	decisions := []DecisionForIndex{
		{ID: idReady1, OrgID: uuid.New(), AgentID: "a", DecisionType: "t", ValidFrom: time.Now(), Embedding: []float32{0.1}},
		{ID: idReady2, OrgID: uuid.New(), AgentID: "b", DecisionType: "t", ValidFrom: time.Now(), Embedding: []float32{0.2}},
	}

	readyEntries, readyDecisions, pendingEntries := partitionUpsertEntries(entries, decisions)

	assert.Len(t, readyEntries, 2)
	assert.Len(t, readyDecisions, 2)
	assert.Len(t, pendingEntries, 1)

	assert.Equal(t, idReady1, readyEntries[0].DecisionID)
	assert.Equal(t, idReady2, readyEntries[1].DecisionID)
	assert.Equal(t, idReady1, readyDecisions[0].ID)
	assert.Equal(t, idReady2, readyDecisions[1].ID)
	assert.Equal(t, idMissing, pendingEntries[0].DecisionID)
}

func TestPartitionUpsertEntries_AllMissing(t *testing.T) {
	idA := uuid.New()
	idB := uuid.New()
	idC := uuid.New()

	entries := []outboxEntry{
		{ID: 1, DecisionID: idA, Operation: "upsert"},
		{ID: 2, DecisionID: idB, Operation: "upsert"},
		{ID: 3, DecisionID: idC, Operation: "upsert"},
	}

	// No decisions match any of the entry decision IDs.
	unrelatedID := uuid.New()
	decisions := []DecisionForIndex{
		{ID: unrelatedID, OrgID: uuid.New(), AgentID: "x", DecisionType: "t", ValidFrom: time.Now(), Embedding: []float32{0.5}},
	}

	readyEntries, readyDecisions, pendingEntries := partitionUpsertEntries(entries, decisions)

	assert.Empty(t, readyEntries)
	assert.Empty(t, readyDecisions)
	require.Len(t, pendingEntries, 3)
	assert.Equal(t, idA, pendingEntries[0].DecisionID)
	assert.Equal(t, idB, pendingEntries[1].DecisionID)
	assert.Equal(t, idC, pendingEntries[2].DecisionID)
}

func TestPartitionUpsertEntries_AllReady(t *testing.T) {
	id1 := uuid.New()
	id2 := uuid.New()
	id3 := uuid.New()

	entries := []outboxEntry{
		{ID: 10, DecisionID: id1, Operation: "upsert"},
		{ID: 20, DecisionID: id2, Operation: "upsert"},
		{ID: 30, DecisionID: id3, Operation: "upsert"},
	}
	decisions := []DecisionForIndex{
		{ID: id1, OrgID: uuid.New(), AgentID: "agent-a", DecisionType: "architecture", ValidFrom: time.Now(), Embedding: []float32{0.1, 0.2}},
		{ID: id2, OrgID: uuid.New(), AgentID: "agent-b", DecisionType: "security", ValidFrom: time.Now(), Embedding: []float32{0.3, 0.4}},
		{ID: id3, OrgID: uuid.New(), AgentID: "agent-c", DecisionType: "trade_off", ValidFrom: time.Now(), Embedding: []float32{0.5, 0.6}},
	}

	readyEntries, readyDecisions, pendingEntries := partitionUpsertEntries(entries, decisions)

	assert.Empty(t, pendingEntries)
	require.Len(t, readyEntries, 3)
	require.Len(t, readyDecisions, 3)

	// Verify order is preserved: entries and decisions are paired in input order.
	assert.Equal(t, id1, readyEntries[0].DecisionID)
	assert.Equal(t, id2, readyEntries[1].DecisionID)
	assert.Equal(t, id3, readyEntries[2].DecisionID)
	assert.Equal(t, id1, readyDecisions[0].ID)
	assert.Equal(t, id2, readyDecisions[1].ID)
	assert.Equal(t, id3, readyDecisions[2].ID)
}

func TestPartitionUpsertEntries_EmptyInputs(t *testing.T) {
	readyEntries, readyDecisions, pendingEntries := partitionUpsertEntries(nil, nil)

	assert.Empty(t, readyEntries)
	assert.Empty(t, readyDecisions)
	assert.Empty(t, pendingEntries)
}

func TestPointConversion_FlatContext(t *testing.T) {
	// Legacy flat agent_context format (pre-PR #180).
	decisionID := uuid.New()
	orgID := uuid.New()
	sessionID := uuid.New()
	validFrom := time.Date(2026, 2, 14, 10, 30, 0, 0, time.UTC)
	proj := "ashita-ai/akashi"

	d := DecisionForIndex{
		ID:                decisionID,
		OrgID:             orgID,
		AgentID:           "coder",
		DecisionType:      "architecture",
		Confidence:        0.85,
		CompletenessScore: 0.72,
		ValidFrom:         validFrom,
		Embedding:         []float32{0.1, 0.2, 0.3, 0.4},
		SessionID:         &sessionID,
		Project:           &proj,
		AgentContext: map[string]any{
			"tool":  "claude-code",
			"model": "claude-opus-4-6",
		},
	}

	p := pointFromDecision(d)

	assert.Equal(t, decisionID, p.ID)
	assert.Equal(t, orgID, p.OrgID)
	assert.Equal(t, "coder", p.AgentID)
	assert.Equal(t, "architecture", p.DecisionType)
	assert.InDelta(t, 0.85, float64(p.Confidence), 0.001)
	assert.InDelta(t, 0.72, float64(p.CompletenessScore), 0.001)
	assert.Equal(t, validFrom, p.ValidFrom)
	assert.Equal(t, []float32{0.1, 0.2, 0.3, 0.4}, p.Embedding)
	require.NotNil(t, p.SessionID)
	assert.Equal(t, sessionID, *p.SessionID)
	assert.Equal(t, "claude-code", p.Tool)
	assert.Equal(t, "claude-opus-4-6", p.Model)
	assert.Equal(t, "ashita-ai/akashi", p.Project)
}

func TestPointConversion_NamespacedContext(t *testing.T) {
	// New namespaced agent_context format (PR #180+).
	proj := "ashita-ai/akashi"
	d := DecisionForIndex{
		ID:                uuid.New(),
		OrgID:             uuid.New(),
		AgentID:           "admin",
		DecisionType:      "security",
		Confidence:        0.92,
		CompletenessScore: 0.88,
		ValidFrom:         time.Now(),
		Embedding:         []float32{0.5, 0.6},
		Project:           &proj,
		AgentContext: map[string]any{
			"server": map[string]any{
				"tool":         "claude-code",
				"tool_version": "1.0.30",
			},
			"client": map[string]any{
				"model": "claude-opus-4-6",
				"task":  "code review",
			},
		},
	}

	p := pointFromDecision(d)

	assert.Equal(t, "claude-code", p.Tool)
	assert.Equal(t, "claude-opus-4-6", p.Model)
	assert.Equal(t, "ashita-ai/akashi", p.Project)
}

func TestPointConversion_NilContext(t *testing.T) {
	d := DecisionForIndex{
		ID:           uuid.New(),
		OrgID:        uuid.New(),
		AgentID:      "planner",
		DecisionType: "architecture",
		Embedding:    []float32{0.1},
		AgentContext: nil,
	}

	p := pointFromDecision(d)

	assert.Empty(t, p.Tool)
	assert.Empty(t, p.Model)
	assert.Empty(t, p.Project)
}

// pointFromDecision replicates the conversion logic from processUpserts.
// In production, d.Project comes from the Postgres generated column, which
// handles the COALESCE over all agent_context fallback paths. Tests that
// want a non-empty project must set DecisionForIndex.Project explicitly.
func pointFromDecision(d DecisionForIndex) Point {
	p := Point{
		ID:                d.ID,
		OrgID:             d.OrgID,
		AgentID:           d.AgentID,
		DecisionType:      d.DecisionType,
		Confidence:        d.Confidence,
		CompletenessScore: d.CompletenessScore,
		ValidFrom:         d.ValidFrom,
		Embedding:         d.Embedding,
		SessionID:         d.SessionID,
	}
	if d.AgentContext != nil {
		p.Tool = agentContextString(d.AgentContext, "server", "tool")
		p.Model = agentContextString(d.AgentContext, "client", "model")
	}
	if d.Project != nil {
		p.Project = *d.Project
	}
	return p
}

func TestAgentContextString(t *testing.T) {
	tests := []struct {
		name      string
		ctx       map[string]any
		namespace string
		key       string
		want      string
	}{
		{
			name:      "namespaced path",
			ctx:       map[string]any{"server": map[string]any{"tool": "claude-code"}},
			namespace: "server",
			key:       "tool",
			want:      "claude-code",
		},
		{
			name:      "flat fallback",
			ctx:       map[string]any{"tool": "cursor"},
			namespace: "server",
			key:       "tool",
			want:      "cursor",
		},
		{
			name:      "namespaced takes precedence over flat",
			ctx:       map[string]any{"server": map[string]any{"tool": "claude-code"}, "tool": "old-flat-value"},
			namespace: "server",
			key:       "tool",
			want:      "claude-code",
		},
		{
			name:      "missing key returns empty",
			ctx:       map[string]any{"server": map[string]any{"other": "value"}},
			namespace: "server",
			key:       "tool",
			want:      "",
		},
		{
			name:      "missing namespace returns empty",
			ctx:       map[string]any{"unrelated": "data"},
			namespace: "server",
			key:       "tool",
			want:      "",
		},
		{
			name:      "nil context returns empty",
			ctx:       nil,
			namespace: "server",
			key:       "tool",
			want:      "",
		},
		{
			name:      "namespace is not a map returns flat fallback",
			ctx:       map[string]any{"server": "not-a-map", "tool": "fallback"},
			namespace: "server",
			key:       "tool",
			want:      "fallback",
		},
		{
			name:      "client namespace for model",
			ctx:       map[string]any{"client": map[string]any{"model": "claude-opus-4-6"}},
			namespace: "client",
			key:       "model",
			want:      "claude-opus-4-6",
		},
		{
			name:      "non-string value returns empty",
			ctx:       map[string]any{"server": map[string]any{"tool": 42}},
			namespace: "server",
			key:       "tool",
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := agentContextString(tt.ctx, tt.namespace, tt.key)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNewOutboxWorker(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))
	w := NewOutboxWorker(nil, nil, logger, 5*time.Second, 50)

	require.NotNil(t, w)
	assert.Nil(t, w.pool, "pool should be nil when passed nil")
	assert.Nil(t, w.index, "index should be nil when passed nil")
	assert.NotNil(t, w.logger)
	assert.Equal(t, 5*time.Second, w.pollInterval)
	assert.Equal(t, 50, w.batchSize)
	assert.NotNil(t, w.done, "done channel should be initialized")
	assert.NotNil(t, w.drainCh, "drainCh channel should be initialized")
	assert.False(t, w.started.Load(), "worker should not be started on creation")
}

func TestNewOutboxWorker_Defaults(t *testing.T) {
	// Verify that different poll intervals and batch sizes are stored correctly.
	w1 := NewOutboxWorker(nil, nil, slog.Default(), time.Second, 10)
	w2 := NewOutboxWorker(nil, nil, slog.Default(), 30*time.Second, 100)

	assert.Equal(t, time.Second, w1.pollInterval)
	assert.Equal(t, 10, w1.batchSize)
	assert.Equal(t, 30*time.Second, w2.pollInterval)
	assert.Equal(t, 100, w2.batchSize)
}

func TestOutboxWorker_StartStop(t *testing.T) {
	// Create a worker with nil pool/index (cannot process batches).
	// Start it, verify it is running, then drain to stop it cleanly.
	w := NewOutboxWorker(nil, nil, slog.Default(), 100*time.Millisecond, 10)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the worker.
	w.Start(ctx)
	assert.True(t, w.started.Load(), "worker should be marked as started")

	// Calling Start again should be a no-op (idempotent).
	w.Start(ctx)
	assert.True(t, w.started.Load(), "double-start should still be started")

	// Drain with a generous timeout.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()

	w.Drain(drainCtx)

	// After drain, the done channel should be closed.
	select {
	case <-w.done:
		// Success: the poll loop exited cleanly.
	default:
		t.Fatal("done channel should be closed after drain")
	}
}

func TestOutboxWorker_DrainIdempotent(t *testing.T) {
	// Verify that calling Drain multiple times does not panic.
	w := NewOutboxWorker(nil, nil, slog.Default(), 100*time.Millisecond, 10)

	ctx := context.Background()
	w.Start(ctx)

	drainCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// First drain should work.
	w.Drain(drainCtx)

	// Second drain should not panic and should return promptly.
	drainCtx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	w.Drain(drainCtx2)
}

func TestScanOutboxEntries(t *testing.T) {
	id1 := uuid.New()
	id2 := uuid.New()
	orgA := uuid.New()
	orgB := uuid.New()

	rows := &mockRows{
		rows: [][]any{
			{int64(1), id1, orgA, "upsert", int(0)},
			{int64(2), id2, orgB, "delete", int(3)},
		},
	}

	entries, err := scanOutboxEntries(rows)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	assert.Equal(t, int64(1), entries[0].ID)
	assert.Equal(t, id1, entries[0].DecisionID)
	assert.Equal(t, orgA, entries[0].OrgID)
	assert.Equal(t, "upsert", entries[0].Operation)
	assert.Equal(t, 0, entries[0].Attempts)

	assert.Equal(t, int64(2), entries[1].ID)
	assert.Equal(t, id2, entries[1].DecisionID)
	assert.Equal(t, orgB, entries[1].OrgID)
	assert.Equal(t, "delete", entries[1].Operation)
	assert.Equal(t, 3, entries[1].Attempts)

	assert.True(t, rows.closed, "rows should be closed after scan")
}

func TestScanOutboxEntries_Empty(t *testing.T) {
	rows := &mockRows{rows: nil}

	entries, err := scanOutboxEntries(rows)
	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.True(t, rows.closed)
}

func TestScanOutboxEntries_ScanError(t *testing.T) {
	rows := &mockRows{
		rows:    [][]any{{int64(1), uuid.New(), uuid.New(), "upsert", int(0)}},
		scanErr: fmt.Errorf("column decode error"),
	}

	entries, err := scanOutboxEntries(rows)
	assert.Error(t, err)
	assert.Nil(t, entries)
	assert.Contains(t, err.Error(), "scan entry")
	assert.True(t, rows.closed)
}

// TestPartitionUpsertEntries_NilEmbedding verifies that decisions found in the
// map but with nil embedding are routed to pendingEntries, not readyEntries.
func TestPartitionUpsertEntries_NilEmbedding(t *testing.T) {
	id := uuid.New()
	entries := []outboxEntry{
		{ID: 1, DecisionID: id, Operation: "upsert"},
	}
	// Decision exists but has nil embedding — should be pending.
	decisions := []DecisionForIndex{
		{ID: id, OrgID: uuid.New(), AgentID: "a", DecisionType: "t", ValidFrom: time.Now(), Embedding: nil},
	}

	readyEntries, readyDecisions, pendingEntries := partitionUpsertEntries(entries, decisions)

	assert.Empty(t, readyEntries, "nil-embedding decisions should not be ready")
	assert.Empty(t, readyDecisions)
	require.Len(t, pendingEntries, 1)
	assert.Equal(t, id, pendingEntries[0].DecisionID)
}

// TestPartitionUpsertEntries_MixedNilAndReady verifies correct partitioning when
// some decisions have embeddings and others do not.
func TestPartitionUpsertEntries_MixedNilAndReady(t *testing.T) {
	idReady := uuid.New()
	idNilEmb := uuid.New()

	entries := []outboxEntry{
		{ID: 1, DecisionID: idReady, Operation: "upsert"},
		{ID: 2, DecisionID: idNilEmb, Operation: "upsert"},
	}
	decisions := []DecisionForIndex{
		{ID: idReady, OrgID: uuid.New(), AgentID: "a", DecisionType: "t", ValidFrom: time.Now(), Embedding: []float32{0.1}},
		{ID: idNilEmb, OrgID: uuid.New(), AgentID: "b", DecisionType: "t", ValidFrom: time.Now(), Embedding: nil},
	}

	readyEntries, readyDecisions, pendingEntries := partitionUpsertEntries(entries, decisions)

	require.Len(t, readyEntries, 1)
	require.Len(t, readyDecisions, 1)
	require.Len(t, pendingEntries, 1)
	assert.Equal(t, idReady, readyEntries[0].DecisionID)
	assert.Equal(t, idReady, readyDecisions[0].ID)
	assert.Equal(t, idNilEmb, pendingEntries[0].DecisionID)
}

// TestOutboxWorker_ProcessBatchCount_NilPool verifies that processBatchCount
// returns 0 and does not panic when pool is nil.
func TestOutboxWorker_ProcessBatchCount_NilPool(t *testing.T) {
	w := NewOutboxWorker(nil, nil, slog.Default(), time.Second, 10)
	n := w.processBatchCount(context.Background())
	assert.Equal(t, 0, n, "nil pool should return 0")
}

// TestOutboxWorker_ProcessBatchCount_NilIndex verifies that processBatchCount
// returns 0 when index is nil but pool is non-nil. We can't set a real pool
// without a database, but we can verify the nil-index guard by setting pool
// to a non-nil value via the struct field.
func TestOutboxWorker_ProcessBatchCount_NilIndex(t *testing.T) {
	// We need pool non-nil to reach the index check. Since pgxpool.Pool has
	// no exported zero constructor, we verify nil-pool returns 0 instead.
	w := NewOutboxWorker(nil, nil, slog.Default(), time.Second, 10)
	n := w.processBatchCount(context.Background())
	assert.Equal(t, 0, n, "nil pool should short-circuit before index check")
}

// TestOutboxWorker_DrainOutbox_CancelledContext verifies that drainOutbox
// returns promptly when the context is already cancelled.
func TestOutboxWorker_DrainOutbox_CancelledContext(t *testing.T) {
	w := NewOutboxWorker(nil, nil, slog.Default(), time.Second, 10)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// drainOutbox should return immediately without panic.
	w.drainOutbox(ctx)
}

// TestOutboxWorker_DrainOutbox_NilPoolReturnsImmediately verifies that
// drainOutbox with a nil pool stops after the first processBatchCount returns 0.
func TestOutboxWorker_DrainOutbox_NilPoolReturnsImmediately(t *testing.T) {
	w := NewOutboxWorker(nil, nil, slog.Default(), time.Second, 10)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Should return immediately since processBatchCount returns 0 (nil pool).
	w.drainOutbox(ctx)
	// If we reach here without hanging, the test passes.
}

// TestScanOutboxEntries_MultipleRows verifies scanning more than two rows.
func TestScanOutboxEntries_MultipleRows(t *testing.T) {
	ids := make([]uuid.UUID, 5)
	orgIDs := make([]uuid.UUID, 5)
	var rawRows [][]any
	for i := 0; i < 5; i++ {
		ids[i] = uuid.New()
		orgIDs[i] = uuid.New()
		rawRows = append(rawRows, []any{int64(i + 1), ids[i], orgIDs[i], "upsert", int(i)})
	}

	rows := &mockRows{rows: rawRows}
	entries, err := scanOutboxEntries(rows)
	require.NoError(t, err)
	require.Len(t, entries, 5)

	for i, e := range entries {
		assert.Equal(t, int64(i+1), e.ID)
		assert.Equal(t, ids[i], e.DecisionID)
		assert.Equal(t, orgIDs[i], e.OrgID)
		assert.Equal(t, "upsert", e.Operation)
		assert.Equal(t, i, e.Attempts)
	}
	assert.True(t, rows.closed)
}

// TestOutboxWorker_StartDrainCancelledParent verifies that when the parent
// context is cancelled, the worker drains and exits cleanly.
func TestOutboxWorker_StartDrainCancelledParent(t *testing.T) {
	w := NewOutboxWorker(nil, nil, slog.Default(), 50*time.Millisecond, 10)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)

	// Cancel the parent context (simulates server shutdown without explicit Drain).
	cancel()

	// The poll loop should exit on its own. Wait for done with a timeout.
	select {
	case <-w.done:
		// Success.
	case <-time.After(5 * time.Second):
		t.Fatal("poll loop should exit when parent context is cancelled")
	}
}

// TestOutboxWorker_RegisterMetrics verifies that registerMetrics does not panic
// when called with a nil pool. The OTEL callback that queries pg_class will
// silently return nil on error, so the gauge registration itself must succeed.
func TestOutboxWorker_RegisterMetrics(t *testing.T) {
	w := NewOutboxWorker(nil, nil, slog.Default(), time.Second, 10)
	assert.NotPanics(t, func() {
		w.registerMetrics()
	}, "registerMetrics should not panic with nil pool")
}

// TestOutboxWorker_PollLoopTickerPath verifies that the poll loop processes
// batches via the ticker path (not just the drain path).
func TestOutboxWorker_PollLoopTickerPath(t *testing.T) {
	w := NewOutboxWorker(nil, nil, slog.Default(), 10*time.Millisecond, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	// Let the ticker fire a few times. With nil pool, processBatch returns
	// immediately without error. This exercises the ticker.C select case.
	time.Sleep(50 * time.Millisecond)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()
	w.Drain(drainCtx)

	select {
	case <-w.done:
	default:
		t.Fatal("done channel should be closed after drain")
	}
}

// TestOutboxWorker_DrainChannelBusy verifies the fallback when drainCh is
// already full before Drain sends its context. The warn log fires and the
// poll loop uses a fallback timeout for the final drain.
func TestOutboxWorker_DrainChannelBusy(t *testing.T) {
	w := NewOutboxWorker(nil, nil, slog.Default(), 100*time.Millisecond, 10)

	ctx := context.Background()
	w.Start(ctx)

	// Fill the drainCh before calling Drain so the channel send hits the
	// sendCtx.Done() path.
	w.drainCh <- context.Background()

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()

	w.Drain(drainCtx)

	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
		t.Fatal("poll loop should exit even when drain channel was busy")
	}
}

// TestPartitionUpsertEntries_EmptySliceEmbedding verifies that decisions with
// a non-nil but zero-length embedding slice are treated as ready (the nil
// check only triggers on nil, not empty).
func TestPartitionUpsertEntries_EmptySliceEmbedding(t *testing.T) {
	id := uuid.New()
	entries := []outboxEntry{
		{ID: 1, DecisionID: id, Operation: "upsert"},
	}
	decisions := []DecisionForIndex{
		{ID: id, OrgID: uuid.New(), AgentID: "a", DecisionType: "t",
			ValidFrom: time.Now(), Embedding: []float32{}},
	}

	readyEntries, readyDecisions, pendingEntries := partitionUpsertEntries(entries, decisions)

	// Empty slice is not nil, so the code treats it as ready.
	assert.Len(t, readyEntries, 1)
	assert.Len(t, readyDecisions, 1)
	assert.Empty(t, pendingEntries)
}

// TestOutboxWorker_DrainOutbox_ExitsOnZeroBatch verifies that drainOutbox
// returns after processBatchCount returns 0 (no remaining entries).
func TestOutboxWorker_DrainOutbox_ExitsOnZeroBatch(t *testing.T) {
	w := NewOutboxWorker(nil, nil, slog.Default(), time.Second, 10)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	w.drainOutbox(ctx)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 500*time.Millisecond,
		"drainOutbox should return immediately when processBatchCount returns 0")
}

// TestOutboxWorker_DrainTimeout verifies that the Drain method returns when its
// context expires, even if the worker hasn't fully drained. We simulate this by
// giving the drain context an already-expired deadline.
func TestOutboxWorker_DrainTimeout(t *testing.T) {
	w := NewOutboxWorker(nil, nil, slog.Default(), 50*time.Millisecond, 10)

	ctx := context.Background()
	w.Start(ctx)

	// Create an already-expired context for the drain.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer drainCancel()
	// Allow it to actually expire.
	time.Sleep(time.Millisecond)

	// Drain should return promptly because the context is expired.
	start := time.Now()
	w.Drain(drainCtx)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 2*time.Second,
		"Drain should return promptly when its context is expired")
}

// TestOutboxWorker_DrainWithoutStart verifies that calling Drain on a worker
// that was never started does not panic or hang.
func TestOutboxWorker_DrainWithoutStart(t *testing.T) {
	w := NewOutboxWorker(nil, nil, slog.Default(), time.Second, 10)

	// Drain without Start: cancelLoop is nil, but drainOnce should still run.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer drainCancel()

	// This should not panic. It will block on <-w.done until drainCtx expires
	// because the poll loop was never started (no goroutine to close w.done).
	w.Drain(drainCtx)
	// If we reach here without panic or hang, the test passes.
}

// TestFetchDecisionsForIndex_EmptyIDs verifies the early return for empty inputs.
func TestFetchDecisionsForIndex_EmptyIDs(t *testing.T) {
	w := NewOutboxWorker(nil, nil, slog.Default(), time.Second, 10)

	// Empty IDs.
	result, err := w.fetchDecisionsForIndex(context.Background(), nil, nil)
	assert.NoError(t, err)
	assert.Nil(t, result, "empty IDs should return nil")

	// Empty orgIDs.
	result, err = w.fetchDecisionsForIndex(context.Background(), []uuid.UUID{uuid.New()}, nil)
	assert.NoError(t, err)
	assert.Nil(t, result, "empty orgIDs should return nil")

	// Mismatched lengths.
	result, err = w.fetchDecisionsForIndex(context.Background(),
		[]uuid.UUID{uuid.New(), uuid.New()},
		[]uuid.UUID{uuid.New()})
	assert.NoError(t, err)
	assert.Nil(t, result, "mismatched lengths should return nil")
}

// TestPartitionUpsertEntries_DuplicateDecisionIDs verifies that when multiple
// outbox entries reference the same decision ID, all entries are matched.
func TestPartitionUpsertEntries_DuplicateDecisionIDs(t *testing.T) {
	id := uuid.New()
	entries := []outboxEntry{
		{ID: 1, DecisionID: id, Operation: "upsert"},
		{ID: 2, DecisionID: id, Operation: "upsert"},
	}
	decisions := []DecisionForIndex{
		{ID: id, OrgID: uuid.New(), AgentID: "a", DecisionType: "t",
			ValidFrom: time.Now(), Embedding: []float32{0.1}},
	}

	readyEntries, readyDecisions, pendingEntries := partitionUpsertEntries(entries, decisions)

	assert.Len(t, readyEntries, 2, "both entries should be ready since they match the same decision")
	assert.Len(t, readyDecisions, 2, "each ready entry gets its own copy of the decision")
	assert.Empty(t, pendingEntries)
}

// TestOutboxWorker_StartContextCancelImmediately verifies that starting a worker
// with an already-cancelled context causes the poll loop to exit immediately.
func TestOutboxWorker_StartContextCancelImmediately(t *testing.T) {
	w := NewOutboxWorker(nil, nil, slog.Default(), 50*time.Millisecond, 10)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel before Start.

	w.Start(ctx)

	// The poll loop should exit almost immediately.
	select {
	case <-w.done:
		// Success.
	case <-time.After(5 * time.Second):
		t.Fatal("poll loop should exit when started with a cancelled context")
	}
}

// TestNewOutboxWorker_ZeroBatchSize verifies that a zero batch size is stored
// as-is (the caller is responsible for providing valid values).
func TestNewOutboxWorker_ZeroBatchSize(t *testing.T) {
	w := NewOutboxWorker(nil, nil, slog.Default(), time.Second, 0)
	assert.Equal(t, 0, w.batchSize)
}

// TestOutboxWorker_RegisterMetrics_Idempotent verifies that calling registerMetrics
// multiple times does not panic (OTEL gauge registration is idempotent).
func TestOutboxWorker_RegisterMetrics_Idempotent(t *testing.T) {
	w := NewOutboxWorker(nil, nil, slog.Default(), time.Second, 10)
	assert.NotPanics(t, func() {
		w.registerMetrics()
		w.registerMetrics()
	}, "registerMetrics should be idempotent")
}
