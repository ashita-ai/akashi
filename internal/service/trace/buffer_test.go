package trace

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	buf := NewBuffer(nil, logger, 100, 50*time.Millisecond)

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
	buf.Drain(drainCtx)
}

func TestBuffer_AppendAndFlush(t *testing.T) {
	run := createTestRun(t)

	buf := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond)
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

	// Wait for the flush interval to fire (buffer flushTimeout is 100ms).
	time.Sleep(300 * time.Millisecond)

	// Verify events landed in the database.
	got, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Len(t, got, 3, "all 3 events should be flushed to DB")

	// Clean shutdown.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()
	buf.Drain(drainCtx)
}

func TestBuffer_FlushOnBatchSize(t *testing.T) {
	run := createTestRun(t)

	// Set maxSize=5 so a batch of 5 events triggers an immediate flush signal,
	// and set a long flush timeout so the timer cannot fire first.
	buf := NewBuffer(testDB, testLogger(), 5, 10*time.Second)
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
	buf.Drain(drainCtx)
}

func TestBuffer_FlushOnInterval(t *testing.T) {
	run := createTestRun(t)

	// Set a large maxSize so the batch-size trigger never fires, and a short
	// flushTimeout so the timer fires quickly.
	buf := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond)
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
	buf.Drain(drainCtx)
}

func TestBuffer_DrainFlushesPending(t *testing.T) {
	run := createTestRun(t)

	// Use a very long flush timeout so only Drain causes the flush.
	buf := NewBuffer(testDB, testLogger(), 1000, 10*time.Minute)
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
	buf.Drain(drainCtx)

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

	buf := NewBuffer(testDB, testLogger(), 1000, 10*time.Minute)
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
	buf.Drain(drainCtx)
	elapsed := time.Since(start)

	// Drain should return nearly instantly (the context is already done).
	assert.Less(t, elapsed, 500*time.Millisecond,
		"drain with an already-cancelled context should return immediately")

	// Give the background flush loop time to finish its final flush
	// so we don't leak the goroutine (the loop has a 10s fallback timeout
	// if the drain context is cancelled, but in practice the flush is fast).
	time.Sleep(200 * time.Millisecond)
}

func TestBuffer_AppendAfterDrain(t *testing.T) {
	run := createTestRun(t)

	buf := NewBuffer(testDB, testLogger(), 1000, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf.Start(ctx)

	// Drain the buffer immediately (no events to flush).
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()
	buf.Drain(drainCtx)

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
	buf := NewBuffer(testDB, testLogger(), totalExpected+1, 10*time.Minute)
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
	buf.Drain(drainCtx)

	// Verify all events reached the database.
	got, err := testDB.GetEventsByRun(context.Background(), run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Len(t, got, totalExpected, "all %d concurrently-appended events should be in the DB after drain", totalExpected)
}
