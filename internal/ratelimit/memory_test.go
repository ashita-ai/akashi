package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
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
		ok, err := m.Allow(ctx, "k1")
		if err != nil {
			t.Fatalf("Allow returned error on request %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("expected Allow to return true for request %d (within burst)", i)
		}
	}
}

func TestMemoryLimiterDenyAfterBurst(t *testing.T) {
	m := NewMemoryLimiter(10, 3) // 10 rps, burst 3
	defer closeLimiter(t, m)

	ctx := context.Background()
	// Exhaust the burst.
	for i := 0; i < 3; i++ {
		ok, err := m.Allow(ctx, "k1")
		if err != nil {
			t.Fatalf("Allow error: %v", err)
		}
		if !ok {
			t.Fatalf("expected Allow=true for request %d", i)
		}
	}

	// Next request should be denied.
	ok, err := m.Allow(ctx, "k1")
	if err != nil {
		t.Fatalf("Allow error: %v", err)
	}
	if ok {
		t.Fatal("expected Allow=false after burst exhausted")
	}
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
	ok, _ := m.Allow(ctx, "k1")
	if ok {
		t.Fatal("should be denied immediately after exhausting burst")
	}

	// Wait for refill.
	time.Sleep(5 * time.Millisecond)

	ok, err := m.Allow(ctx, "k1")
	if err != nil {
		t.Fatalf("Allow error: %v", err)
	}
	if !ok {
		t.Fatal("expected Allow=true after refill period")
	}
}

func TestMemoryLimiterIndependentKeys(t *testing.T) {
	m := NewMemoryLimiter(10, 1) // burst 1
	defer closeLimiter(t, m)

	ctx := context.Background()
	// Exhaust key "a".
	ok, _ := m.Allow(ctx, "a")
	if !ok {
		t.Fatal("first request for 'a' should succeed")
	}
	ok, _ = m.Allow(ctx, "a")
	if ok {
		t.Fatal("second request for 'a' should be denied")
	}

	// Key "b" should be unaffected.
	ok, _ = m.Allow(ctx, "b")
	if !ok {
		t.Fatal("first request for 'b' should succeed")
	}
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
				ok, err := m.Allow(ctx, "shared")
				if err != nil {
					t.Errorf("goroutine %d: Allow error: %v", idx, err)
					return
				}
				if ok {
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

	if exists {
		t.Fatal("expected stale bucket to be evicted")
	}
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

	if !exists {
		t.Fatal("expected recent bucket to survive eviction")
	}
}

func TestMemoryLimiterCloseIdempotent(t *testing.T) {
	m := NewMemoryLimiter(10, 5)
	// Double close should not panic.
	if err := m.Close(); err != nil {
		t.Fatalf("first Close error: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close error: %v", err)
	}
}

func TestNoopLimiterAlwaysAllows(t *testing.T) {
	var l NoopLimiter
	ctx := context.Background()
	for i := 0; i < 1000; i++ {
		ok, err := l.Allow(ctx, "anything")
		if err != nil {
			t.Fatalf("NoopLimiter.Allow error: %v", err)
		}
		if !ok {
			t.Fatal("NoopLimiter should always return true")
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("NoopLimiter.Close error: %v", err)
	}
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
		ok, _ := m.Allow(ctx, "k1")
		if !ok {
			t.Fatalf("expected Allow=true for request %d after long idle", i)
		}
	}
	ok, _ := m.Allow(ctx, "k1")
	if ok {
		t.Fatal("expected Allow=false after burst exhausted, even after long idle")
	}
}
