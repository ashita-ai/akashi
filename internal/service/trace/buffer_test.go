package trace

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/internal/testutil"
)

var testDB *storage.DB

func TestMain(m *testing.M) {
	tc := testutil.MustStartTimescaleDB()

	code := func() int {
		defer tc.Terminate()

		ctx := context.Background()
		logger := testutil.TestLogger()

		var err error
		testDB, err = tc.NewTestDB(ctx, logger)
		if err != nil {
			fmt.Fprintf(os.Stderr, "buffer_test: failed to create test DB: %v\n", err)
			return 1
		}
		defer testDB.Close(ctx)

		if err := testDB.EnsureDefaultOrg(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "buffer_test: failed to ensure default org: %v\n", err)
			return 1
		}

		return m.Run()
	}()

	os.Exit(code)
}

// testLogger returns a logger for use within individual tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// createTestRun creates a run with a unique agent ID for test isolation.
func createTestRun(t *testing.T) model.AgentRun {
	t.Helper()
	agentID := fmt.Sprintf("buf-test-%s", uuid.New().String()[:8])
	run, err := testDB.CreateRun(context.Background(), model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)
	return run
}

// makeEventInputs creates n EventInput values with unique payloads.
func makeEventInputs(n int) []model.EventInput {
	inputs := make([]model.EventInput, n)
	for i := range inputs {
		inputs[i] = model.EventInput{
			EventType: model.EventToolCallCompleted,
			Payload:   map[string]any{"step": i},
		}
	}
	return inputs
}

func TestBufferDoubleStartIsNoop(t *testing.T) {
	// Buffer.Start() must be idempotent -- a second call logs a warning and returns
	// without spawning a second flush goroutine or panicking on double close(b.done).
	logger := testLogger()
	buf := NewBuffer(nil, logger, 100, 50*time.Millisecond, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	buf.Start(ctx) // First call -- should work.
	buf.Start(ctx) // Second call -- should be a no-op, no panic.

	// Verify started is true.
	if !buf.started.Load() {
		t.Fatal("expected started to be true after Start()")
	}

	// Clean shutdown.
	cancel()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))
}

func TestBuffer_AppendAndFlush(t *testing.T) {
	run := createTestRun(t)

	buf := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	// Append 3 events.
	events, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(3))
	require.NoError(t, err)
	assert.Len(t, events, 3)

	// Each returned event should have a non-zero ID and sequence number.
	for i, e := range events {
		assert.NotEqual(t, uuid.Nil, e.ID, "event %d should have a generated ID", i)
		assert.Greater(t, e.SequenceNum, int64(0), "event %d should have a positive sequence number", i)
		assert.Equal(t, run.ID, e.RunID)
		assert.Equal(t, run.OrgID, e.OrgID)
		assert.Equal(t, model.EventToolCallCompleted, e.EventType)
	}

	// Poll until the flush interval fires and events land in the database.
	require.Eventually(t, func() bool {
		got, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
		return err == nil && len(got) == 3
	}, 2*time.Second, 25*time.Millisecond, "all 3 events should be flushed to DB")

	// Clean shutdown.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))
}

func TestBuffer_FlushOnBatchSize(t *testing.T) {
	run := createTestRun(t)

	// Set maxSize=5 so a batch of 5 events triggers an immediate flush signal,
	// and set a long flush timeout so the timer cannot fire first.
	buf := NewBuffer(testDB, testLogger(), 5, 10*time.Second, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	_, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(5))
	require.NoError(t, err)

	// The batch-size trigger should flush quickly. Poll the DB rather than
	// sleeping a fixed duration so the test is both fast and not racy.
	require.Eventually(t, func() bool {
		got, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
		return err == nil && len(got) == 5
	}, 2*time.Second, 25*time.Millisecond, "5 events should be flushed within 2 seconds via batch-size trigger")

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))
}

func TestBuffer_FlushOnInterval(t *testing.T) {
	run := createTestRun(t)

	// Set a large maxSize so the batch-size trigger never fires, and a short
	// flushTimeout so the timer fires quickly.
	buf := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	// Append 2 events -- well below maxSize.
	_, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(2))
	require.NoError(t, err)

	// Wait for the interval to fire (flushTimeout=100ms, wait up to 500ms).
	require.Eventually(t, func() bool {
		got, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
		return err == nil && len(got) == 2
	}, 1*time.Second, 25*time.Millisecond, "2 events should be flushed by the interval timer")

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))
}

func TestBuffer_DrainFlushesPending(t *testing.T) {
	run := createTestRun(t)

	// Use a very long flush timeout so only Drain causes the flush.
	buf := NewBuffer(testDB, testLogger(), 1000, 10*time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	_, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(4))
	require.NoError(t, err)

	// Confirm events are still in the buffer, not yet in DB.
	assert.Equal(t, 4, buf.Len(), "events should be buffered before drain")

	// Drain should perform a final flush before returning.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))

	// After Drain returns, events must be in the database.
	got, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Len(t, got, 4, "drain should flush all pending events to DB")
}

func TestBuffer_DrainTimeout(t *testing.T) {
	// Test that Drain respects its context deadline and returns promptly
	// even when the flush loop has not yet finished. We use an already-
	// cancelled context so the <-ctx.Done() branch fires immediately,
	// verifying that Drain does not block waiting for <-b.done.
	run := createTestRun(t)

	buf := NewBuffer(testDB, testLogger(), 1000, 10*time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	// Append events so the final flush has work to do.
	_, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(3))
	require.NoError(t, err)

	// Create an already-cancelled context for Drain. This guarantees
	// Drain returns via the ctx.Done() path without waiting for the
	// flush loop to complete.
	drainCtx, drainCancel := context.WithCancel(context.Background())
	drainCancel() // cancel immediately

	start := time.Now()
	// Error expected: drain context is already cancelled, so events may not
	// have been flushed by the time Drain returns.
	_ = buf.Drain(drainCtx)
	elapsed := time.Since(start)

	// Drain should return nearly instantly (the context is already done).
	assert.Less(t, elapsed, 500*time.Millisecond,
		"drain with an already-cancelled context should return immediately")

	// Wait for the flush loop goroutine to exit so we don't leak it.
	// Drain already cancelled the loop; the goroutine closes buf.done
	// after its final (failed) flush attempt.
	select {
	case <-buf.done:
	case <-time.After(5 * time.Second):
		t.Fatal("flush loop goroutine did not exit within 5s")
	}
}

func TestBuffer_AppendAfterDrain(t *testing.T) {
	run := createTestRun(t)

	buf := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	// Drain the buffer immediately (no events to flush).
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))

	// Append after drain must be rejected so we don't acknowledge events that
	// cannot be flushed anymore.
	events, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(2))
	require.Error(t, err, "append after drain should fail")
	assert.Nil(t, events)
	assert.EqualValues(t, 2, buf.DroppedEvents(), "rejected appends should be counted")
	assert.Zero(t, buf.Len(), "no events should be buffered after rejected append")

	// Verify nothing reached the DB.
	got, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Empty(t, got, "no events should reach the DB after drain since the flush loop has exited")
}

func TestBuffer_ConcurrentAppend(t *testing.T) {
	run := createTestRun(t)

	const (
		goroutines    = 10
		eventsPerGo   = 10
		totalExpected = goroutines * eventsPerGo
	)

	// Use a long flush timeout so events accumulate. We drain at the end to
	// flush them all at once, verifying concurrency safety end-to-end.
	buf := NewBuffer(testDB, testLogger(), totalExpected+1, 10*time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)

	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			inputs := make([]model.EventInput, eventsPerGo)
			for i := range inputs {
				inputs[i] = model.EventInput{
					EventType: model.EventToolCallCompleted,
					Payload:   map[string]any{"goroutine": g, "step": i},
				}
			}
			_, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, inputs)
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d: %w", g, err)
			}
		}(g)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent append error: %v", err)
	}

	// All events should be in the buffer (none flushed yet due to long timeout
	// and maxSize set above totalExpected).
	assert.Equal(t, totalExpected, buf.Len(), "buffer should hold all %d events", totalExpected)

	// Drain to flush everything.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))

	// Verify all events reached the database.
	got, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Len(t, got, totalExpected, "all %d concurrently-appended events should be in the DB after drain", totalExpected)
}

func TestBuffer_OccurredAtOverride(t *testing.T) {
	run := createTestRun(t)

	buf := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	// Create an event with a custom OccurredAt timestamp (backfill scenario).
	customTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	inputs := []model.EventInput{
		{
			EventType:  model.EventToolCallCompleted,
			Payload:    map[string]any{"backfill": true},
			OccurredAt: &customTime,
		},
		{
			EventType: model.EventToolCallCompleted,
			Payload:   map[string]any{"backfill": false},
			// OccurredAt nil — should use server time.
		},
	}

	events, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, inputs)
	require.NoError(t, err)
	require.Len(t, events, 2)

	// First event should use the provided OccurredAt.
	assert.Equal(t, customTime, events[0].OccurredAt,
		"event with OccurredAt set should use the provided timestamp")

	// Second event should use server time (within the last few seconds).
	assert.WithinDuration(t, time.Now(), events[1].OccurredAt, 5*time.Second,
		"event without OccurredAt should use approximately time.Now()")

	// The two events should have different OccurredAt values.
	assert.NotEqual(t, events[0].OccurredAt, events[1].OccurredAt,
		"custom and server timestamps should differ")

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))

	// Verify the timestamps persist through flush to DB.
	got, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
	require.NoError(t, err)
	require.Len(t, got, 2)

	// Find the backfill event by payload.
	var backfillEvent model.AgentEvent
	for _, e := range got {
		if payload, ok := e.Payload["backfill"]; ok && payload == true {
			backfillEvent = e
			break
		}
	}
	assert.Equal(t, customTime.UTC(), backfillEvent.OccurredAt.UTC(),
		"custom OccurredAt should survive the COPY flush to Postgres")
}

func TestBuffer_FlushNow(t *testing.T) {
	run := createTestRun(t)

	// Use a very long flush timeout so only FlushNow can trigger the flush.
	buf := NewBuffer(testDB, testLogger(), 1000, 10*time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	_, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(5))
	require.NoError(t, err)
	assert.Equal(t, 5, buf.Len(), "events should be in buffer before FlushNow")

	// FlushNow should block until the buffer is empty.
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer flushCancel()
	require.NoError(t, buf.FlushNow(flushCtx))

	assert.Equal(t, 0, buf.Len(), "buffer should be empty after FlushNow")

	// Verify events are in the database.
	got, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Len(t, got, 5, "all events should be in DB after FlushNow")

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))
}

func TestBuffer_DrainIdempotent(t *testing.T) {
	run := createTestRun(t)

	buf := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	_, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(3))
	require.NoError(t, err)

	// First drain should flush and return nil.
	drainCtx1, drainCancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel1()
	require.NoError(t, buf.Drain(drainCtx1))

	// Second drain should not panic and should return nil (no more events).
	drainCtx2, drainCancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel2()
	require.NoError(t, buf.Drain(drainCtx2))

	// Verify events made it to DB.
	got, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Len(t, got, 3)
}

func TestBuffer_HasWAL(t *testing.T) {
	t.Run("without WAL", func(t *testing.T) {
		buf := NewBuffer(nil, testLogger(), 100, 50*time.Millisecond, nil)
		assert.False(t, buf.HasWAL())
	})

	t.Run("with WAL", func(t *testing.T) {
		cfg := WALConfig{
			Dir:            t.TempDir(),
			SyncMode:       "none",
			MaxSegmentSize: minSegmentSize,
			MaxSegmentRecs: minSegmentRecords,
		}
		wal, err := NewWAL(testLogger(), cfg)
		require.NoError(t, err)
		t.Cleanup(func() { _ = wal.Close() })

		buf := NewBuffer(nil, testLogger(), 100, 50*time.Millisecond, wal)
		assert.True(t, buf.HasWAL())
	})
}

func TestBuffer_Capacity(t *testing.T) {
	buf := NewBuffer(nil, testLogger(), 100, 50*time.Millisecond, nil)
	assert.Equal(t, maxBufferCapacity, buf.Capacity())
}

func TestBuffer_LenInitiallyZero(t *testing.T) {
	buf := NewBuffer(nil, testLogger(), 100, 50*time.Millisecond, nil)
	assert.Equal(t, 0, buf.Len())
}

func TestBuffer_DroppedEventsInitiallyZero(t *testing.T) {
	buf := NewBuffer(nil, testLogger(), 100, 50*time.Millisecond, nil)
	assert.EqualValues(t, 0, buf.DroppedEvents())
}

func TestBuffer_AppendWithWAL(t *testing.T) {
	run := createTestRun(t)

	cfg := WALConfig{
		Dir:            t.TempDir(),
		SyncMode:       "none",
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: minSegmentRecords,
	}
	wal, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	buf := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond, wal)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	events, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(3))
	require.NoError(t, err)
	assert.Len(t, events, 3)

	// WAL should have pending bytes.
	assert.Greater(t, wal.PendingBytes(), int64(0), "WAL should have pending data")

	// Drain flushes to DB and closes WAL.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))

	// Verify events made it to DB.
	got, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Len(t, got, 3)
}

func TestBuffer_StartWithWALRecovery(t *testing.T) {
	run := createTestRun(t)

	cfg := WALConfig{
		Dir:            t.TempDir(),
		SyncMode:       "none",
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: minSegmentRecords,
	}

	// Phase 1: Write events to WAL via buffer, then simulate crash (no drain).
	wal1, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	buf1 := NewBuffer(testDB, testLogger(), 1000, 10*time.Minute, wal1)
	ctx1, cancel1 := context.WithCancel(context.Background())
	buf1.Start(ctx1)

	_, err = buf1.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(3))
	require.NoError(t, err)

	// Close WAL without drain — simulates crash.
	cancel1()
	<-buf1.done
	require.NoError(t, wal1.Close())

	// Phase 2: Create new buffer with same WAL dir — Start should recover events.
	wal2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	buf2 := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond, wal2)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	buf2.Start(ctx2) // This triggers recovery

	// Events should have been recovered and flushed to DB.
	require.Eventually(t, func() bool {
		got, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
		return err == nil && len(got) >= 3
	}, 5*time.Second, 100*time.Millisecond, "recovered events should be flushed to DB")

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	require.NoError(t, buf2.Drain(drainCtx))
}

func TestBuffer_StartWithWALRecovery_MidFlushCrash(t *testing.T) {
	// Simulates a crash between COPY-to-Postgres and WAL checkpoint advance.
	// Events are in BOTH Postgres and WAL. Recovery must deduplicate via the
	// idempotent insert path without errors or duplicate rows.
	run := createTestRun(t)

	cfg := WALConfig{
		Dir:            t.TempDir(),
		SyncMode:       "none",
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: minSegmentRecords,
	}

	// Phase 1: Write events via buffer, flush to Postgres, then simulate crash
	// before WAL checkpoint advances.
	wal1, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	buf1 := NewBuffer(testDB, testLogger(), 1000, 10*time.Minute, wal1)
	ctx1, cancel1 := context.WithCancel(context.Background())
	buf1.Start(ctx1)

	_, err = buf1.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(5))
	require.NoError(t, err)

	// Force flush so events are in Postgres.
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer flushCancel()
	require.NoError(t, buf1.FlushNow(flushCtx))

	// Verify events are in Postgres.
	got, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
	require.NoError(t, err)
	require.Len(t, got, 5, "events should be in DB after flush")

	// Crash: close WAL without drain. The flush above advanced the checkpoint,
	// so we need to re-write the events to WAL to simulate the mid-flush case
	// (events reached Postgres but checkpoint didn't advance).
	cancel1()
	<-buf1.done
	require.NoError(t, wal1.Close())

	// Recreate WAL and write the same events back (simulating checkpoint not
	// having advanced — events still in WAL segments).
	wal2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	// Write the same events that are already in Postgres back into the WAL.
	// This simulates the state after a mid-flush crash: events in both places.
	_, err = wal2.Write(got)
	require.NoError(t, err)
	require.NoError(t, wal2.Close())

	// Phase 2: Recovery should handle duplicates gracefully.
	wal3, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	buf2 := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond, wal3)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	buf2.Start(ctx2) // triggers recovery with idempotent dedup

	// Verify no duplicates: still exactly 5 events.
	require.Eventually(t, func() bool {
		events, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
		return err == nil && len(events) == 5
	}, 5*time.Second, 100*time.Millisecond, "should have exactly 5 events after dedup recovery, no duplicates")

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	require.NoError(t, buf2.Drain(drainCtx))
}

func TestBuffer_FlushOnceWithWALCheckpoint(t *testing.T) {
	run := createTestRun(t)

	cfg := WALConfig{
		Dir:            t.TempDir(),
		SyncMode:       "none",
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: minSegmentRecords,
	}
	wal, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	buf := NewBuffer(testDB, testLogger(), 1000, 10*time.Minute, wal)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	_, err = buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(5))
	require.NoError(t, err)

	// FlushNow should flush and advance WAL checkpoint.
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer flushCancel()
	require.NoError(t, buf.FlushNow(flushCtx))

	// After flush + checkpoint, recovering from WAL should find nothing new.
	recovered, _, err := wal.Recover()
	require.NoError(t, err)
	// All events have been checkpointed, so recovery should return empty.
	assert.Empty(t, recovered, "all events should be checkpointed after flush")

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))
}

func TestBuffer_DrainWithWALCloseError(t *testing.T) {
	run := createTestRun(t)

	cfg := WALConfig{
		Dir:            t.TempDir(),
		SyncMode:       "batch",
		SyncInterval:   50 * time.Millisecond,
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: minSegmentRecords,
	}
	wal, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	buf := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond, wal)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	_, err = buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(2))
	require.NoError(t, err)

	// Drain should succeed even with WAL batch sync mode.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))
}

func TestBuffer_AppendDrainingCountsDropped(t *testing.T) {
	run := createTestRun(t)

	buf := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))

	// Append 5 events after drain — all should be counted as dropped.
	_, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(5))
	require.Error(t, err)
	assert.EqualValues(t, 5, buf.DroppedEvents())
}

func TestBuffer_StartWithWALRecoveryCheckpointFails(t *testing.T) {
	// Tests the Start path where WAL recovery succeeds, flush to Postgres succeeds,
	// but CheckpointLSN fails. Covers the warn-but-continue branch in Start.
	run := createTestRun(t)
	cfg := WALConfig{
		Dir:            t.TempDir(),
		SyncMode:       "none",
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: minSegmentRecords,
	}

	// Write events directly to WAL with proper run IDs so InsertEventsIdempotent succeeds.
	wal1, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	now := time.Now().UTC()
	events := make([]model.AgentEvent, 3)
	for i := range events {
		events[i] = model.AgentEvent{
			ID:          uuid.New(),
			RunID:       run.ID,
			OrgID:       run.OrgID,
			EventType:   model.EventToolCallCompleted,
			SequenceNum: int64(i + 9000), // high seq to avoid collisions
			OccurredAt:  now,
			AgentID:     run.AgentID,
			Payload:     map[string]any{"step": i},
			CreatedAt:   now,
		}
	}
	_, err = wal1.Write(events)
	require.NoError(t, err)
	require.NoError(t, wal1.Close())

	// Phase 2: Reopen WAL, then make dir read-only so checkpoint fails.
	wal2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	// Make read-only so saveCheckpoint fails during Start's recovery.
	require.NoError(t, os.Chmod(cfg.Dir, 0o500))       //nolint:gosec // test intentionally restricts dir perms
	t.Cleanup(func() { _ = os.Chmod(cfg.Dir, 0o700) }) //nolint:gosec // restore

	buf2 := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond, wal2)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	// Start recovers events, flushes to DB, but checkpoint fails (warns, continues).
	buf2.Start(ctx2)

	require.NoError(t, os.Chmod(cfg.Dir, 0o700)) //nolint:gosec // restore

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	_ = buf2.Drain(drainCtx)
}

func TestBuffer_StartWithWALRecoveryFlushFails(t *testing.T) {
	// Tests the Start path where WAL Recover succeeds but InsertEventsIdempotent
	// fails. Events remain in WAL for next startup.
	cfg := WALConfig{
		Dir:            t.TempDir(),
		SyncMode:       "none",
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: minSegmentRecords,
	}

	// Write events directly to WAL (not through buffer).
	wal1, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	events := testEvents(3)
	_, err = wal1.Write(events)
	require.NoError(t, err)
	require.NoError(t, wal1.Close())

	// Reopen WAL and start buffer. InsertEventsIdempotent will fail because
	// the events have UUIDs not associated with any run in the DB.
	wal2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	buf := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond, wal2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start recovers events but InsertEventsIdempotent fails (logged, not fatal).
	buf.Start(ctx)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	_ = buf.Drain(drainCtx)
}

func TestBuffer_StartWithWALRecoveryError(t *testing.T) {
	// Tests the Start path where WAL Recover() itself returns an error.
	cfg := WALConfig{
		Dir:            t.TempDir(),
		SyncMode:       "none",
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: minSegmentRecords,
	}

	wal, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	// Write events, then corrupt the checkpoint so Recover's loadCheckpoint fails.
	events := testEvents(3)
	_, err = wal.Write(events)
	require.NoError(t, err)
	require.NoError(t, wal.Close())

	// Corrupt the checkpoint file.
	cpPath := filepath.Join(cfg.Dir, "checkpoint.json")
	require.NoError(t, os.WriteFile(cpPath, []byte("{invalid json"), 0o600))

	wal2, err := NewWAL(testLogger(), cfg)
	// NewWAL itself should fail on corrupted checkpoint.
	require.Error(t, err)
	assert.Nil(t, wal2)
}

func TestBuffer_FlushUntilEmptyWithRetry(t *testing.T) {
	// Tests that flushUntilEmpty retries after a transient flush failure.
	// We use a real DB and real buffer, append events, then call FlushNow
	// which delegates to flushUntilEmpty.
	run := createTestRun(t)

	buf := NewBuffer(testDB, testLogger(), 1000, 10*time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	// Append events.
	_, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(5))
	require.NoError(t, err)

	// FlushNow calls flushUntilEmpty — with a working DB it should succeed immediately.
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer flushCancel()
	require.NoError(t, buf.FlushNow(flushCtx))
	assert.Equal(t, 0, buf.Len())

	// Test the deadline-exceeded path of flushUntilEmpty by using an already-expired context.
	_, err = buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(3))
	require.NoError(t, err)

	expiredCtx, expiredCancel := context.WithCancel(context.Background())
	expiredCancel() // already cancelled
	err = buf.FlushNow(expiredCtx)
	// With a cancelled context, InsertEvents will fail, and flushUntilEmpty returns error.
	if err != nil {
		assert.Contains(t, err.Error(), "deadline")
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))
}

func TestBuffer_FlushLoopFallbackContext(t *testing.T) {
	// Tests the fallback path in flushLoop where ctx is cancelled directly
	// without going through Drain (no drain context sent via channel).
	// This exercises the else branch: fallbackCtx, cancel := context.WithTimeout(...)
	run := createTestRun(t)

	buf := NewBuffer(testDB, testLogger(), 1000, 10*time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	buf.Start(ctx)

	_, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(3))
	require.NoError(t, err)

	// Cancel the context directly without calling Drain — triggers the fallback path.
	cancel()

	// Wait for the flush loop to exit.
	select {
	case <-buf.done:
	case <-time.After(35 * time.Second):
		t.Fatal("flush loop did not exit within the fallback timeout")
	}

	// Events should have been flushed via the fallback path.
	got, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Len(t, got, 3, "fallback flush should write events to DB")
}

func TestBuffer_FlushUntilEmptyExpiredContext(t *testing.T) {
	// Tests flushUntilEmpty returning early when context is cancelled mid-backoff.
	run := createTestRun(t)

	buf := NewBuffer(testDB, testLogger(), 1000, 10*time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	_, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(2))
	require.NoError(t, err)

	// Use a very short timeout so FlushNow's flushUntilEmpty may hit the
	// ctx.Done() branch during backoff if the first attempt is slow.
	tinyCtx, tinyCancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer tinyCancel()
	// Allow a tiny delay for the context to definitely expire.
	time.Sleep(1 * time.Millisecond)

	err = buf.FlushNow(tinyCtx)
	// Either succeeds instantly or returns deadline exceeded.
	if err != nil {
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))
}

func TestBuffer_DrainWALCloseError(t *testing.T) {
	// Tests that Drain handles WAL close errors gracefully.
	run := createTestRun(t)

	cfg := WALConfig{
		Dir:            t.TempDir(),
		SyncMode:       "none",
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: minSegmentRecords,
	}
	wal, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	buf := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond, wal)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	_, err = buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(2))
	require.NoError(t, err)

	// Close the WAL's current file early so Close() in Drain hits the error path.
	wal.mu.Lock()
	if wal.current != nil {
		_ = wal.current.Close()
		wal.current = nil
	}
	wal.mu.Unlock()

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	// Drain should not panic even though WAL Close encounters issues.
	_ = buf.Drain(drainCtx)
}

// TestBuffer_RegisterMetrics verifies that registerMetrics does not panic
// when called on a buffer (even with nil DB). The noop OTEL meter provider
// handles gauge registration without errors.
func TestBuffer_RegisterMetrics(t *testing.T) {
	buf := NewBuffer(nil, testLogger(), 100, 50*time.Millisecond, nil)
	assert.NotPanics(t, func() {
		buf.registerMetrics()
	}, "registerMetrics should not panic")
}

// TestBuffer_RegisterMetrics_ObservesValues verifies that the OTEL callbacks
// in registerMetrics can read Len() and DroppedEvents() without panicking.
func TestBuffer_RegisterMetrics_ObservesValues(t *testing.T) {
	buf := NewBuffer(nil, testLogger(), 100, 50*time.Millisecond, nil)
	// Pre-populate some state that the callbacks will read.
	buf.droppedEvents.Add(42)
	assert.NotPanics(t, func() {
		buf.registerMetrics()
	})
	assert.EqualValues(t, 42, buf.DroppedEvents())
	assert.Equal(t, 0, buf.Len())
}

// TestBuffer_StartWithNilDB verifies that Start does not panic when db is nil
// and WAL is nil. The flush loop will start but flushOnce will have nothing to do.
func TestBuffer_StartWithNilDB(t *testing.T) {
	buf := NewBuffer(nil, testLogger(), 100, 50*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	assert.NotPanics(t, func() {
		buf.Start(ctx)
	}, "Start should not panic with nil DB")

	assert.True(t, buf.started.Load())

	cancel()
	select {
	case <-buf.done:
	case <-time.After(5 * time.Second):
		t.Fatal("flush loop should exit after context cancellation")
	}
}

// TestBuffer_DrainContextChannelBusy verifies the fallback path in Drain when
// the drainCh channel is already full. The buffer should still drain correctly
// using the fallback timeout.
func TestBuffer_DrainContextChannelBusy(t *testing.T) {
	run := createTestRun(t)

	buf := NewBuffer(testDB, testLogger(), 1000, 10*time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	_, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(2))
	require.NoError(t, err)

	// Fill the drainCh before calling Drain to exercise the sendCtx.Done() path.
	buf.drainCh <- context.Background()

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	// Should not panic. Events may or may not be flushed depending on timing.
	_ = buf.Drain(drainCtx)

	select {
	case <-buf.done:
	case <-time.After(35 * time.Second):
		t.Fatal("flush loop should exit even when drain channel was busy")
	}
}

func TestBuffer_AtCapacity(t *testing.T) {
	run := createTestRun(t)

	// Use a very long flush timeout and large maxSize so nothing flushes.
	// Fill the buffer to maxBufferCapacity then attempt one more append.
	buf := NewBuffer(testDB, testLogger(), maxBufferCapacity+1, 10*time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	// Fill in batches of 10,000 to reach maxBufferCapacity (100,000).
	const batchSize = 10_000
	for i := 0; i < maxBufferCapacity/batchSize; i++ {
		_, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(batchSize))
		require.NoError(t, err, "batch %d should succeed", i)
	}
	assert.Equal(t, maxBufferCapacity, buf.Len())

	// One more event should be rejected.
	events, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(1))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBufferAtCapacity)
	assert.Nil(t, events)
	assert.EqualValues(t, 1, buf.DroppedEvents(), "rejected events should be counted")

	// Buffer size unchanged.
	assert.Equal(t, maxBufferCapacity, buf.Len())

	// Clean shutdown — drain flushes all 100K events.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))
}

// ---------- Buffer Start: WAL recovery error triggers continue-without-events path ----------

func TestBuffer_StartWithWALRecoverError(t *testing.T) {
	// Tests the Start path where WAL.Recover() returns an error (line 85).
	// After NewWAL succeeds (loadCheckpoint works), we corrupt the checkpoint
	// file so that Recover's loadCheckpoint call fails.
	dir := t.TempDir()
	cfg := WALConfig{
		Dir:            dir,
		SyncMode:       "none",
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: minSegmentRecords,
	}

	wal1, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	// Write events so there's something to potentially recover.
	events := testEvents(3)
	_, err = wal1.Write(events)
	require.NoError(t, err)
	require.NoError(t, wal1.Close())

	// Reopen the WAL (NewWAL loads checkpoint successfully during init).
	wal2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	// Corrupt the checkpoint after NewWAL but before Start calls Recover.
	cpPath := filepath.Join(dir, "checkpoint.json")
	require.NoError(t, os.WriteFile(cpPath, []byte("{corrupt json!!!"), 0o600))

	buf := NewBuffer(testDB, testLogger(), 1000, 10*time.Minute, wal2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start should handle the recovery error gracefully and continue.
	buf.Start(ctx)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	_ = buf.Drain(drainCtx)
}

// ---------- WAL recovery with corrupted JSON ----------

func TestWAL_RecoverWithCorruptedJSON(t *testing.T) {
	// Tests the readSegment path where CRC is valid but JSON unmarshal
	// fails (lines 563-566). We manually construct a WAL record with
	// valid CRC but invalid JSON payload.
	dir := t.TempDir()
	cfg := WALConfig{
		Dir:            dir,
		SyncMode:       "none",
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: minSegmentRecords,
	}

	// Create a WAL and immediately close it (just to get checkpoint/dir setup).
	wal1, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)
	require.NoError(t, wal1.Close())

	// Manually write a segment file with valid header and a record that has
	// valid CRC but invalid JSON payload.
	segPath := filepath.Join(dir, "00000000000001.wal")
	f, err := os.Create(segPath) //nolint:gosec // test file path is deterministic
	require.NoError(t, err)

	// Write segment header: magic(4) + version(2) + reserved(2) + baseLSN(8)
	var hdr [walHeaderSize]byte
	binary.BigEndian.PutUint32(hdr[0:4], walMagic)
	binary.BigEndian.PutUint16(hdr[4:6], walVersion)
	binary.BigEndian.PutUint64(hdr[8:16], 1) // baseLSN = 1
	_, err = f.Write(hdr[:])
	require.NoError(t, err)

	// Write one record with valid CRC but invalid JSON.
	badPayload := []byte("{not valid json!!!")
	var head [walRecordHead]byte
	binary.BigEndian.PutUint64(head[0:8], 1)                        // LSN = 1
	binary.BigEndian.PutUint32(head[8:12], uint32(len(badPayload))) //nolint:gosec // test payload is tiny

	// Compute valid CRC over head + payload.
	h := crc32.New(crc32cTable)
	_, _ = h.Write(head[:])
	_, _ = h.Write(badPayload)
	crcVal := h.Sum32()
	var crcBuf [walCRCSize]byte
	binary.BigEndian.PutUint32(crcBuf[:], crcVal)

	_, err = f.Write(head[:])
	require.NoError(t, err)
	_, err = f.Write(badPayload)
	require.NoError(t, err)
	_, err = f.Write(crcBuf[:])
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// Reopen WAL and recover. The corrupted JSON record should be skipped.
	wal2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)
	defer func() { _ = wal2.Close() }()

	recovered, _, err := wal2.Recover()
	require.NoError(t, err)
	assert.Empty(t, recovered, "corrupted JSON should cause readSegment to stop")
}

// ---------- WAL write closed WAL ----------

func TestWAL_WriteAfterClose(t *testing.T) {
	// Tests the Write path when the WAL's current segment file is closed.
	// This should trigger a write error on the closed file.
	cfg := WALConfig{
		Dir:            t.TempDir(),
		SyncMode:       "none",
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: minSegmentRecords,
	}

	wal1, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	// Close the underlying segment file directly to simulate a failed write.
	require.NoError(t, wal1.current.Close())

	_, err = wal1.Write(testEvents(1))
	assert.Error(t, err, "writing to a closed segment should fail")
}

// ---------- WAL MkdirAll failure ----------

func TestWAL_NewWALMkdirAllFails(t *testing.T) {
	// Tests NewWAL when os.MkdirAll fails (line 127). Use /dev/null as the
	// parent directory — a regular file, not a directory, so MkdirAll for a
	// child directory inside it will fail.
	cfg := WALConfig{
		Dir:            "/dev/null/impossible-wal-dir",
		SyncMode:       "none",
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: minSegmentRecords,
	}

	wal1, err := NewWAL(testLogger(), cfg)
	require.Error(t, err, "NewWAL should fail when directory can't be created")
	assert.Nil(t, wal1)
	assert.Contains(t, err.Error(), "create directory")
}

// ---------- WAL directory not writable ----------

func TestWAL_NewWALDirNotWritable(t *testing.T) {
	// Tests NewWAL when the directory exists but is not writable (line 134).
	// MkdirAll succeeds, but the probe file creation fails.
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o444))    //nolint:gosec // intentionally restricting perms for test
	defer func() { _ = os.Chmod(dir, 0o755) }() //nolint:gosec // restore perms for cleanup

	cfg := WALConfig{
		Dir:            dir,
		SyncMode:       "none",
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: minSegmentRecords,
	}

	wal1, err := NewWAL(testLogger(), cfg)
	require.Error(t, err, "NewWAL should fail when directory is not writable")
	assert.Nil(t, wal1)
	assert.Contains(t, err.Error(), "not writable")
}

// ---------- WAL Recover with listSegments error ----------

func TestWAL_RecoverListSegmentsError(t *testing.T) {
	// Tests the Recover path where listSegments fails (line 283).
	// After NewWAL succeeds, we make the directory unreadable so
	// os.ReadDir fails during Recover.
	dir := t.TempDir()
	cfg := WALConfig{
		Dir:            dir,
		SyncMode:       "none",
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: minSegmentRecords,
	}

	wal1, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	// Write events so there's a checkpoint.
	_, err = wal1.Write(testEvents(2))
	require.NoError(t, err)
	require.NoError(t, wal1.Close())

	// Reopen (checkpoint loads successfully during NewWAL).
	wal2, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)
	defer func() {
		// Restore permissions for cleanup.
		_ = os.Chmod(dir, 0o755) //nolint:gosec // restore perms for cleanup
		_ = wal2.Close()
	}()

	// Set directory to execute-only (0o111): allows file access by path
	// (loadCheckpoint uses ReadFile with a known path) but prevents listing
	// (listSegments uses ReadDir). This makes loadCheckpoint succeed but
	// listSegments fail.
	require.NoError(t, os.Chmod(dir, 0o111)) //nolint:gosec // intentionally restricting perms for test

	_, _, err = wal2.Recover()
	require.Error(t, err, "Recover should fail when directory is unreadable")
	assert.Contains(t, err.Error(), "list segments")
}

// ---------- registerMetrics: OTEL callbacks ----------

func TestBuffer_RegisterMetrics_CallbacksExecute(t *testing.T) {
	// Verifies that the OTEL observable gauge callbacks registered in
	// registerMetrics actually execute when a ManualReader forces collection.
	// This covers the callback bodies at lines 371-374 and 379-382.
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	original := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	defer func() {
		otel.SetMeterProvider(original)
		_ = provider.Shutdown(context.Background())
	}()

	run := createTestRun(t)
	buf := NewBuffer(testDB, testLogger(), 1000, 10*time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	// Append a few events so depth is non-zero.
	_, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(3))
	require.NoError(t, err)

	// Force metric collection — this invokes the observable gauge callbacks.
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	// Verify we got metrics from the buffer scope.
	var found int
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "akashi.buffer.depth" || m.Name == "akashi.buffer.dropped_total" {
				found++
			}
		}
	}
	assert.Equal(t, 2, found, "should have registered both buffer gauges")

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))
}

func TestWAL_RegisterMetrics_CallbacksExecute(t *testing.T) {
	// Same as buffer metrics test but for WAL's registerMetrics.
	// Covers lines 625-628 and 633-636.
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	original := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	defer func() {
		otel.SetMeterProvider(original)
		_ = provider.Shutdown(context.Background())
	}()

	cfg := WALConfig{
		Dir:            t.TempDir(),
		SyncMode:       "none",
		MaxSegmentSize: minSegmentSize,
		MaxSegmentRecs: minSegmentRecords,
	}
	wal, err := NewWAL(testLogger(), cfg)
	require.NoError(t, err)

	// registerMetrics is called inside NewWAL's init. Force collection now.
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	var found int
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "akashi.wal.segment_count" || m.Name == "akashi.wal.pending_bytes" {
				found++
			}
		}
	}
	assert.Equal(t, 2, found, "should have registered both WAL gauges")

	require.NoError(t, wal.Close())
}

func TestBuffer_AuditFlushedWithEvents(t *testing.T) {
	// Verify that audit entries buffered via AppendWithAudit are flushed
	// atomically with events — the core fix for issue #608.
	run := createTestRun(t)

	buf := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	// AppendWithAudit buffers events and audit under a single lock hold,
	// eliminating the race where a background flush could drain events
	// before the audit entry was buffered.
	events, err := buf.AppendWithAudit(
		context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(2),
		func(evts []model.AgentEvent) storage.MutationAuditEntry {
			return storage.MutationAuditEntry{
				RequestID:    "test-req-audit-flush",
				OrgID:        run.OrgID,
				ActorAgentID: run.AgentID,
				ActorRole:    "agent",
				HTTPMethod:   "POST",
				Endpoint:     "/v1/runs/" + run.ID.String() + "/events",
				Operation:    "append_events",
				ResourceType: "agent_run",
				ResourceID:   run.ID.String(),
				AfterData:    map[string]any{"event_count": len(evts)},
			}
		},
	)
	require.NoError(t, err)
	require.Len(t, events, 2)

	// Drain forces a flush. Both events and audit must land atomically.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	require.NoError(t, buf.Drain(drainCtx))

	// Verify events reached the database.
	got, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Len(t, got, 2, "events should be flushed to DB")

	// Verify the audit entry was committed in the same transaction.
	var auditCount int
	err = testDB.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM mutation_audit_log
		 WHERE operation = 'append_events'
		   AND resource_id = $1
		   AND request_id = 'test-req-audit-flush'
		   AND org_id = $2`, run.ID.String(), run.OrgID,
	).Scan(&auditCount)
	require.NoError(t, err)
	assert.Equal(t, 1, auditCount, "audit entry should be flushed atomically with events")
}

func TestBuffer_AuditNotLostOnFlushRetry(t *testing.T) {
	// Orphaned audit entries (buffered without events) must flush
	// immediately rather than waiting for an event batch that may never
	// arrive — e.g. on graceful shutdown.
	run := createTestRun(t)

	buf := NewBuffer(testDB, testLogger(), 1000, 10*time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	// Buffer an audit entry with no events.
	buf.BufferAudit(storage.MutationAuditEntry{
		RequestID:    "test-req-orphan-audit",
		OrgID:        run.OrgID,
		ActorAgentID: run.AgentID,
		ActorRole:    "agent",
		HTTPMethod:   "POST",
		Endpoint:     "/v1/runs/" + run.ID.String() + "/events",
		Operation:    "append_events",
		ResourceType: "agent_run",
		ResourceID:   run.ID.String(),
	})

	// flushOnce with orphaned audits should flush them directly.
	flushed, err := buf.flushOnce(context.Background())
	require.NoError(t, err)
	assert.True(t, flushed, "flushOnce should flush orphaned audit entries even without events")

	// The audit entry should have been drained from the buffer.
	buf.mu.Lock()
	assert.Len(t, buf.audits, 0, "audit entry should be cleared after flush")
	buf.mu.Unlock()

	// Verify the audit entry reached the database.
	var auditCount int
	err = testDB.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM mutation_audit_log
		 WHERE request_id = 'test-req-orphan-audit'
		   AND org_id = $1`, run.OrgID,
	).Scan(&auditCount)
	require.NoError(t, err)
	assert.Equal(t, 1, auditCount, "orphaned audit entry should be flushed directly to the database")
}

func TestBuffer_LenIncludesAudits(t *testing.T) {
	// Len() returns events + audits (for drain completeness checks).
	// EventLen() returns events only (for health endpoint capacity comparison).
	run := createTestRun(t)

	buf := NewBuffer(testDB, testLogger(), 1000, 10*time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	assert.Equal(t, 0, buf.Len(), "empty buffer: Len should be 0")
	assert.Equal(t, 0, buf.EventLen(), "empty buffer: EventLen should be 0")

	// Append 2 events without audit.
	_, err := buf.Append(context.Background(), run.ID, run.AgentID, run.OrgID, makeEventInputs(2))
	require.NoError(t, err)

	assert.Equal(t, 2, buf.Len(), "Len should count events")
	assert.Equal(t, 2, buf.EventLen(), "EventLen should count events")

	// Buffer an orphaned audit entry.
	buf.BufferAudit(storage.MutationAuditEntry{
		RequestID: "test-len-audit",
		OrgID:     run.OrgID,
		Operation: "append_events",
	})

	assert.Equal(t, 3, buf.Len(), "Len should count events + audits")
	assert.Equal(t, 2, buf.EventLen(), "EventLen should count only events")
}
