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
		AgentContext: map[string]any{
			"tool":  "claude-code",
			"model": "claude-opus-4-6",
			"repo":  "ashita-ai/akashi",
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
	assert.Equal(t, "ashita-ai/akashi", p.Repo)
}

func TestPointConversion_NamespacedContext(t *testing.T) {
	// New namespaced agent_context format (PR #180+).
	d := DecisionForIndex{
		ID:                uuid.New(),
		OrgID:             uuid.New(),
		AgentID:           "admin",
		DecisionType:      "security",
		Confidence:        0.92,
		CompletenessScore: 0.88,
		ValidFrom:         time.Now(),
		Embedding:         []float32{0.5, 0.6},
		AgentContext: map[string]any{
			"server": map[string]any{
				"tool":         "claude-code",
				"tool_version": "1.0.30",
				"repo":         "ashita-ai/akashi",
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
	assert.Equal(t, "ashita-ai/akashi", p.Repo)
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
	assert.Empty(t, p.Repo)
}

// pointFromDecision replicates the conversion logic from processUpserts.
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
		p.Repo = agentContextString(d.AgentContext, "server", "repo")
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
