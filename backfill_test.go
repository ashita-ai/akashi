package akashi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBackfillScorer scripts a sequence of BackfillScoring results so the drain
// logic can be exercised without a database or an LLM judge.
type fakeBackfillScorer struct {
	results []backfillResult
	calls   int
	batches []int // batchSize observed on each call
}

type backfillResult struct {
	n   int
	err error
}

func (f *fakeBackfillScorer) BackfillScoring(_ context.Context, batchSize int) (int, error) {
	f.batches = append(f.batches, batchSize)
	if f.calls >= len(f.results) {
		f.calls++
		return 0, nil
	}
	r := f.results[f.calls]
	f.calls++
	return r.n, r.err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A full batch means more may be pending, so the drain keeps going; the first
// short batch ends the sweep. This is the behaviour the old one-shot backfill
// lacked, which stranded any backlog larger than a single batch.
func TestDrainConflictBackfill_DrainsUntilShortBatch(t *testing.T) {
	s := &fakeBackfillScorer{results: []backfillResult{
		{n: 500}, {n: 500}, {n: 120},
	}}

	total := drainConflictBackfill(context.Background(), s, 500, 200, discardLogger())

	assert.Equal(t, 1120, total, "should sum every batch it processed")
	assert.Equal(t, 3, s.calls, "should stop at the first short batch")
	assert.Equal(t, []int{500, 500, 500}, s.batches, "should pass the configured batch size on every call")
}

// An empty queue must cost exactly one query, not a spin.
func TestDrainConflictBackfill_EmptyQueueMakesOneCall(t *testing.T) {
	s := &fakeBackfillScorer{results: []backfillResult{{n: 0}}}

	total := drainConflictBackfill(context.Background(), s, 500, 200, discardLogger())

	assert.Zero(t, total)
	assert.Equal(t, 1, s.calls)
}

// A batch that errors stops the sweep rather than retrying in place, and the
// count achieved before the failure is still returned so a partial sweep is
// never reported as a clean one.
func TestDrainConflictBackfill_StopsOnErrorAndKeepsPartialCount(t *testing.T) {
	s := &fakeBackfillScorer{results: []backfillResult{
		{n: 500},
		{n: 200, err: errors.New("judge unreachable")},
		{n: 500},
	}}

	total := drainConflictBackfill(context.Background(), s, 500, 200, discardLogger())

	assert.Equal(t, 700, total, "partial progress from the failing batch must still be counted")
	assert.Equal(t, 2, s.calls, "must not continue past an error")
}

// Shutdown must not wait on a long drain: the loop checks cancellation before
// every batch, since App.Shutdown blocks on these goroutines before closing the
// database pool.
func TestDrainConflictBackfill_StopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := &fakeBackfillScorer{results: []backfillResult{{n: 500}, {n: 500}}}
	total := drainConflictBackfill(ctx, s, 500, 200, discardLogger())

	assert.Zero(t, total)
	assert.Zero(t, s.calls, "a cancelled context must not issue a batch")
}

// If a decision were never marked scored it would be re-fetched forever. The
// bound converts that from a hot loop against the database and the LLM judge
// into a capped sweep that resumes on the next tick.
func TestDrainConflictBackfill_HonoursMaxBatches(t *testing.T) {
	// Always returns a full batch — the pathological never-terminating case.
	s := &fakeBackfillScorer{}
	for range 10 {
		s.results = append(s.results, backfillResult{n: 50})
	}

	total := drainConflictBackfill(context.Background(), s, 50, 3, discardLogger())

	assert.Equal(t, 150, total)
	assert.Equal(t, 3, s.calls, "must not exceed the per-sweep bound")
}

// The configured batch size must reach the scorer; a regression here would
// silently restore the hardcoded 500.
func TestDrainConflictBackfill_UsesConfiguredBatchSize(t *testing.T) {
	s := &fakeBackfillScorer{results: []backfillResult{{n: 7}}}

	drainConflictBackfill(context.Background(), s, 25, 200, discardLogger())

	require.Len(t, s.batches, 1)
	assert.Equal(t, 25, s.batches[0])
}
