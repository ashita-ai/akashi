package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func closeLimiter(t *testing.T, m *MemoryLimiter) {
	t.Helper()
	if err := m.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
}

func TestMemoryLimiterAllowUnderBurst(t *testing.T) {
	m := NewMemoryLimiter(10, 5) // 10 rps, burst 5
	defer closeLimiter(t, m)

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		res, err := m.Allow(ctx, "k1")
		require.NoError(t, err, "request %d", i)
		assert.True(t, res.Allowed, "expected Allowed for request %d (within burst)", i)
		assert.Equal(t, 5, res.Limit, "Limit should equal burst")
		assert.GreaterOrEqual(t, res.Remaining, 0, "Remaining should be non-negative")
	}
}

func TestMemoryLimiterDenyAfterBurst(t *testing.T) {
	m := NewMemoryLimiter(10, 3) // 10 rps, burst 3
	defer closeLimiter(t, m)

	ctx := context.Background()
	// Exhaust the burst.
	for i := 0; i < 3; i++ {
		res, err := m.Allow(ctx, "k1")
		require.NoError(t, err)
		assert.True(t, res.Allowed, "expected Allowed for request %d", i)
	}

	// Next request should be denied.
	res, err := m.Allow(ctx, "k1")
	require.NoError(t, err)
	assert.False(t, res.Allowed, "expected denial after burst exhausted")
	assert.Equal(t, 0, res.Remaining, "Remaining should be 0 when denied")
	assert.False(t, res.ResetAt.IsZero(), "ResetAt should be set when denied")
}

func TestMemoryLimiterTokenRefill(t *testing.T) {
	// Rate of 1000/s means 1 token per millisecond. With burst=2,
	// after exhausting both tokens, waiting ~2ms should refill at least 1.
	m := NewMemoryLimiter(1000, 2)
	defer closeLimiter(t, m)

	ctx := context.Background()
	// Exhaust.
	for i := 0; i < 2; i++ {
		_, _ = m.Allow(ctx, "k1")
	}
	res, _ := m.Allow(ctx, "k1")
	assert.False(t, res.Allowed, "should be denied immediately after exhausting burst")

	// Wait for refill.
	time.Sleep(5 * time.Millisecond)

	res, err := m.Allow(ctx, "k1")
	require.NoError(t, err)
	assert.True(t, res.Allowed, "expected Allowed after refill period")
}

func TestMemoryLimiterIndependentKeys(t *testing.T) {
	m := NewMemoryLimiter(10, 1) // burst 1
	defer closeLimiter(t, m)

	ctx := context.Background()
	// Exhaust key "a".
	res, _ := m.Allow(ctx, "a")
	assert.True(t, res.Allowed, "first request for 'a' should succeed")
	res, _ = m.Allow(ctx, "a")
	assert.False(t, res.Allowed, "second request for 'a' should be denied")

	// Key "b" should be unaffected.
	res, _ = m.Allow(ctx, "b")
	assert.True(t, res.Allowed, "first request for 'b' should succeed")
}

func TestMemoryLimiterConcurrent(t *testing.T) {
	m := NewMemoryLimiter(100, 50)
	defer closeLimiter(t, m)

	ctx := context.Background()
	var wg sync.WaitGroup
	allowed := make([]int, 10)

	// 10 goroutines each send 10 requests for the same key.
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				res, err := m.Allow(ctx, "shared")
				if err != nil {
					t.Errorf("goroutine %d: Allow error: %v", idx, err)
					return
				}
				if res.Allowed {
					allowed[idx]++
				}
			}
		}(g)
	}
	wg.Wait()

	total := 0
	for _, c := range allowed {
		total += c
	}
	// Burst is 50, so all 100 requests within a single burst should
	// allow at most 50 and at least 1.
	if total < 1 || total > 50 {
		t.Fatalf("expected between 1 and 50 allowed requests, got %d", total)
	}
}

func TestMemoryLimiterEvictStale(t *testing.T) {
	m := NewMemoryLimiter(10, 5)
	defer closeLimiter(t, m)

	ctx := context.Background()
	_, _ = m.Allow(ctx, "stale")

	// Manually backdate the bucket.
	m.mu.Lock()
	m.buckets["stale"].lastAccess = time.Now().Add(-15 * time.Minute)
	m.mu.Unlock()

	m.evictStale()

	m.mu.Lock()
	_, exists := m.buckets["stale"]
	m.mu.Unlock()

	assert.False(t, exists, "expected stale bucket to be evicted")
}

func TestMemoryLimiterEvictKeepsRecent(t *testing.T) {
	m := NewMemoryLimiter(10, 5)
	defer closeLimiter(t, m)

	ctx := context.Background()
	_, _ = m.Allow(ctx, "recent")

	m.evictStale()

	m.mu.Lock()
	_, exists := m.buckets["recent"]
	m.mu.Unlock()

	assert.True(t, exists, "expected recent bucket to survive eviction")
}

func TestMemoryLimiterCloseIdempotent(t *testing.T) {
	m := NewMemoryLimiter(10, 5)
	// Double close should not panic.
	require.NoError(t, m.Close(), "first Close")
	require.NoError(t, m.Close(), "second Close")
}

func TestNoopLimiterAlwaysAllows(t *testing.T) {
	var l NoopLimiter
	ctx := context.Background()
	for i := 0; i < 1000; i++ {
		res, err := l.Allow(ctx, "anything")
		require.NoError(t, err)
		assert.True(t, res.Allowed, "NoopLimiter should always return Allowed")
		assert.Equal(t, 0, res.Limit, "NoopLimiter Limit should be 0 (disabled)")
	}
	require.NoError(t, l.Close())
}

func TestMemoryLimiterTokensCapAtBurst(t *testing.T) {
	// Even after a long idle period, tokens should not exceed burst.
	m := NewMemoryLimiter(1000, 3)
	defer closeLimiter(t, m)

	ctx := context.Background()
	_, _ = m.Allow(ctx, "k1")

	// Backdate so a large refill would be computed.
	m.mu.Lock()
	m.buckets["k1"].lastAccess = time.Now().Add(-1 * time.Hour)
	m.mu.Unlock()

	// After refill, should be capped at burst (3). Consume 3 -> ok, 4th -> denied.
	for i := 0; i < 3; i++ {
		res, _ := m.Allow(ctx, "k1")
		assert.True(t, res.Allowed, "expected Allowed for request %d after long idle", i)
	}
	res, _ := m.Allow(ctx, "k1")
	assert.False(t, res.Allowed, "expected denial after burst exhausted, even after long idle")
}

func TestMemoryLimiterResultHeaders(t *testing.T) {
	m := NewMemoryLimiter(10, 3) // 10 rps, burst 3
	defer closeLimiter(t, m)

	ctx := context.Background()

	// First request: 3 burst, 2 remaining after consuming one.
	res, err := m.Allow(ctx, "hdr")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
	assert.Equal(t, 3, res.Limit)
	assert.Equal(t, 2, res.Remaining)

	// Second request: 1 remaining.
	res, err = m.Allow(ctx, "hdr")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
	assert.Equal(t, 3, res.Limit)
	assert.Equal(t, 1, res.Remaining)

	// Third request: 0 remaining.
	res, err = m.Allow(ctx, "hdr")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
	assert.Equal(t, 3, res.Limit)
	assert.Equal(t, 0, res.Remaining)

	// Fourth request: denied, ResetAt in the future.
	res, err = m.Allow(ctx, "hdr")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, 0, res.Remaining)
	assert.True(t, res.ResetAt.After(time.Now()), "ResetAt should be in the future")
}
