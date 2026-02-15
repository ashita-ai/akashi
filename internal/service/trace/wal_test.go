package trace

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

func testWALConfig(t *testing.T) WALConfig {
	t.Helper()
	return WALConfig{
		Dir:            t.TempDir(),
		SyncMode:       "none", // fast for tests
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: 200,
	}
}

func testEvents(n int) []model.AgentEvent {
	events := make([]model.AgentEvent, n)
	now := time.Now().UTC()
	orgID := uuid.New()
	runID := uuid.New()
	for i := range events {
		events[i] = model.AgentEvent{
			ID:          uuid.New(),
			RunID:       runID,
			OrgID:       orgID,
			EventType:   model.EventToolCallCompleted,
			SequenceNum: int64(i + 1),
			OccurredAt:  now,
			AgentID:     "wal-test-agent",
			Payload:     map[string]any{"step": i},
			CreatedAt:   now,
		}
	}
	return events
}

func closeWAL(t *testing.T, w *WAL) {
	t.Helper()
	if err := w.Close(); err != nil {
		t.Logf("wal close: %v", err)
	}
}

func TestWAL_WriteAndRecover(t *testing.T) {
	cfg := testWALConfig(t)
	w, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	events := testEvents(5)
	require.NoError(t, w.Write(events))
	require.NoError(t, w.Close())

	// Reopen and recover — all 5 should come back.
	w2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)
	defer closeWAL(t, w2)

	recovered, err := w2.Recover()
	require.NoError(t, err)
	assert.Len(t, recovered, 5)
	for i, r := range recovered {
		assert.Equal(t, events[i].ID, r.ID, "event %d ID mismatch", i)
		assert.Equal(t, events[i].AgentID, r.AgentID, "event %d AgentID mismatch", i)
	}
}

func TestWAL_CheckpointAdvancesRecovery(t *testing.T) {
	cfg := testWALConfig(t)
	w, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	events := testEvents(10)
	require.NoError(t, w.Write(events))

	// Checkpoint first 6 events.
	require.NoError(t, w.Checkpoint(events[:6]))
	require.NoError(t, w.Close())

	// Recover — should get only the last 4.
	w2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)
	defer closeWAL(t, w2)

	recovered, err := w2.Recover()
	require.NoError(t, err)
	assert.Len(t, recovered, 4, "should recover only un-checkpointed events")
	for i, r := range recovered {
		assert.Equal(t, events[6+i].ID, r.ID, "recovered event %d ID mismatch", i)
	}
}

func TestWAL_CheckpointAll_EmptyRecovery(t *testing.T) {
	cfg := testWALConfig(t)
	w, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	events := testEvents(3)
	require.NoError(t, w.Write(events))
	require.NoError(t, w.Checkpoint(events))
	require.NoError(t, w.Close())

	w2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)
	defer closeWAL(t, w2)

	recovered, err := w2.Recover()
	require.NoError(t, err)
	assert.Empty(t, recovered, "all events checkpointed, nothing to recover")
}

func TestWAL_EmptyRecovery(t *testing.T) {
	cfg := testWALConfig(t)
	w, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)
	defer closeWAL(t, w)

	recovered, err := w.Recover()
	require.NoError(t, err)
	assert.Empty(t, recovered, "fresh WAL should have nothing to recover")
}

func TestWAL_SegmentRotation(t *testing.T) {
	cfg := testWALConfig(t)
	cfg.MaxSegmentRecs = minSegmentRecords // 100 records per segment

	w, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	// Write 250 events — should span at least 2 segments.
	events := testEvents(250)
	require.NoError(t, w.Write(events))
	require.NoError(t, w.Close())

	segCount := countWALFiles(t, cfg.Dir)
	assert.GreaterOrEqual(t, segCount, 2, "250 events with 100/segment should produce at least 2 segments")

	// Recover all 250.
	w2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)
	defer closeWAL(t, w2)

	recovered, err := w2.Recover()
	require.NoError(t, err)
	assert.Len(t, recovered, 250, "all events should be recoverable across segments")
}

func TestWAL_SegmentCleanup(t *testing.T) {
	cfg := testWALConfig(t)
	cfg.MaxSegmentRecs = minSegmentRecords

	w, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	events := testEvents(250)
	require.NoError(t, w.Write(events))

	beforeCleanup := countWALFiles(t, cfg.Dir)
	require.GreaterOrEqual(t, beforeCleanup, 2)

	// Checkpoint all events — old segments should be cleaned.
	require.NoError(t, w.Checkpoint(events))

	afterCleanup := countWALFiles(t, cfg.Dir)
	assert.Less(t, afterCleanup, beforeCleanup,
		"checkpoint should delete fully-flushed segments (before=%d, after=%d)", beforeCleanup, afterCleanup)

	require.NoError(t, w.Close())
}

func TestWAL_CorruptedRecord(t *testing.T) {
	cfg := testWALConfig(t)
	w, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	events := testEvents(5)
	require.NoError(t, w.Write(events))
	require.NoError(t, w.Close())

	// Corrupt a byte in the last segment file. This should cause
	// the reader to stop at the corrupted record.
	segs := listWALFiles(t, cfg.Dir)
	require.NotEmpty(t, segs)

	lastSeg := segs[len(segs)-1]
	data, err := os.ReadFile(lastSeg) //nolint:gosec // test file path
	require.NoError(t, err)
	require.Greater(t, len(data), walHeaderSize+walRecordHead+10)

	// Flip a byte in the first record's payload area.
	corruptIdx := walHeaderSize + walRecordHead + 5
	data[corruptIdx] ^= 0xFF
	require.NoError(t, os.WriteFile(lastSeg, data, 0o600))

	// Recover — corruption in first record stops reading that segment.
	w2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)
	defer closeWAL(t, w2)

	recovered, err := w2.Recover()
	require.NoError(t, err)
	assert.Less(t, len(recovered), 5, "corrupted record should truncate recovery")
}

func TestWAL_ConcurrentWrites(t *testing.T) {
	cfg := testWALConfig(t)
	w, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	const goroutines = 10
	const eventsPerGo = 20

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)

	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			events := testEvents(eventsPerGo)
			if err := w.Write(events); err != nil {
				errCh <- err
			}
		}(g)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent write error: %v", err)
	}

	require.NoError(t, w.Close())

	// Recover all written events.
	w2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)
	defer closeWAL(t, w2)

	recovered, err := w2.Recover()
	require.NoError(t, err)
	assert.Equal(t, goroutines*eventsPerGo, len(recovered),
		"all concurrently-written events should be recoverable")
}

func TestWAL_DisabledWhenDirEmpty(t *testing.T) {
	w, err := NewWAL(testLogger(), WALConfig{Dir: ""})
	require.NoError(t, err)
	assert.Nil(t, w, "empty dir should return nil WAL")
}

func TestWAL_InvalidSyncMode(t *testing.T) {
	cfg := testWALConfig(t)
	cfg.SyncMode = "turbo"

	_, err := NewWAL(testLogger(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid sync mode")
}

func TestWAL_SegmentSizeTooSmall(t *testing.T) {
	cfg := testWALConfig(t)
	cfg.MaxSegmentSize = 100 // below minSegmentSize

	_, err := NewWAL(testLogger(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "segment size")
}

func TestWAL_SegmentRecordsTooSmall(t *testing.T) {
	cfg := testWALConfig(t)
	cfg.MaxSegmentRecs = 5 // below minSegmentRecords

	_, err := NewWAL(testLogger(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "segment records")
}

func TestWAL_BatchSyncMode(t *testing.T) {
	cfg := testWALConfig(t)
	cfg.SyncMode = "batch"
	cfg.SyncInterval = 50 * time.Millisecond

	w, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	events := testEvents(3)
	require.NoError(t, w.Write(events))

	// Let the sync goroutine fire at least once.
	time.Sleep(100 * time.Millisecond)

	require.NoError(t, w.Close())

	// Verify events are recoverable.
	w2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)
	defer closeWAL(t, w2)

	recovered, err := w2.Recover()
	require.NoError(t, err)
	assert.Len(t, recovered, 3)
}

func TestWAL_FullSyncMode(t *testing.T) {
	cfg := testWALConfig(t)
	cfg.SyncMode = "full"

	w, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	events := testEvents(3)
	require.NoError(t, w.Write(events))
	require.NoError(t, w.Close())

	w2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)
	defer closeWAL(t, w2)

	recovered, err := w2.Recover()
	require.NoError(t, err)
	assert.Len(t, recovered, 3)
}

func TestWAL_PendingBytesAndSegmentCount(t *testing.T) {
	cfg := testWALConfig(t)
	w, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)
	defer closeWAL(t, w)

	assert.GreaterOrEqual(t, w.SegmentCount(), 1, "should have at least the initial segment")

	events := testEvents(10)
	require.NoError(t, w.Write(events))
	assert.Greater(t, w.PendingBytes(), int64(0), "pending bytes should be > 0 after writes")
}

func TestWAL_CheckpointEmptyIsNoop(t *testing.T) {
	cfg := testWALConfig(t)
	w, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)
	defer closeWAL(t, w)

	// Checkpointing an empty slice should be a no-op, not an error.
	require.NoError(t, w.Checkpoint(nil))
	require.NoError(t, w.Checkpoint([]model.AgentEvent{}))
}

func TestWAL_BadMagicRejected(t *testing.T) {
	cfg := testWALConfig(t)
	w, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	events := testEvents(3)
	require.NoError(t, w.Write(events))
	require.NoError(t, w.Close())

	// Corrupt the magic bytes of the first segment.
	segs := listWALFiles(t, cfg.Dir)
	require.NotEmpty(t, segs)

	data, err := os.ReadFile(segs[0]) //nolint:gosec // test file path
	require.NoError(t, err)
	binary.BigEndian.PutUint32(data[0:4], 0xDEADBEEF)
	require.NoError(t, os.WriteFile(segs[0], data, 0o600))

	w2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)
	defer closeWAL(t, w2)

	recovered, err := w2.Recover()
	require.NoError(t, err)
	// The corrupted segment should be skipped; recovery stops.
	assert.Empty(t, recovered, "bad magic should prevent recovery from that segment")
}

func TestWAL_TruncatedRecord(t *testing.T) {
	cfg := testWALConfig(t)
	w, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	events := testEvents(5)
	require.NoError(t, w.Write(events))
	require.NoError(t, w.Close())

	// Truncate the last segment mid-record.
	segs := listWALFiles(t, cfg.Dir)
	require.NotEmpty(t, segs)

	lastSeg := segs[len(segs)-1]
	info, err := os.Stat(lastSeg)
	require.NoError(t, err)

	// Chop off 20 bytes from the end — should corrupt the last record.
	truncSize := info.Size() - 20
	require.Greater(t, truncSize, int64(walHeaderSize))
	require.NoError(t, os.Truncate(lastSeg, truncSize))

	w2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)
	defer closeWAL(t, w2)

	recovered, err := w2.Recover()
	require.NoError(t, err)
	// Should recover some but not all events.
	assert.Less(t, len(recovered), 5, "truncated segment should lose at least the last record")
	assert.Greater(t, len(recovered), 0, "should still recover records before the truncation point")
}

// --- helpers ---

func countWALFiles(t *testing.T, dir string) int {
	t.Helper()
	return len(listWALFiles(t, dir))
}

func listWALFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var paths []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wal" {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	return paths
}
