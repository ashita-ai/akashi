package mcp

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckCache_StoreAndDrain(t *testing.T) {
	cc := newCheckCache()

	cc.Store("session-1", `{"has_precedent": true, "summary": "test"}`)

	result := cc.Drain("session-1")
	require.NotEmpty(t, result)
	assert.Contains(t, result, "has_precedent")
}

func TestCheckCache_DrainClearsEntry(t *testing.T) {
	cc := newCheckCache()

	cc.Store("session-1", "check result")

	first := cc.Drain("session-1")
	require.Equal(t, "check result", first)

	second := cc.Drain("session-1")
	assert.Empty(t, second, "second drain should return empty after entry was consumed")
}

func TestCheckCache_DrainMissing(t *testing.T) {
	cc := newCheckCache()
	assert.Empty(t, cc.Drain("nonexistent"))
}

func TestCheckCache_SessionIsolation(t *testing.T) {
	cc := newCheckCache()

	cc.Store("session-1", "result-1")
	cc.Store("session-2", "result-2")

	assert.Equal(t, "result-1", cc.Drain("session-1"))
	assert.Equal(t, "result-2", cc.Drain("session-2"))
}

func TestCheckCache_StoreOverwritesPrevious(t *testing.T) {
	cc := newCheckCache()

	cc.Store("session-1", "old check")
	cc.Store("session-1", "new check")

	result := cc.Drain("session-1")
	assert.Equal(t, "new check", result, "latest store should overwrite previous")
}

func TestCheckCache_TTLExpiry(t *testing.T) {
	cc := newCheckCache()

	// Inject an expired entry directly.
	cc.mu.Lock()
	cc.cache["session-1"] = checkCacheEntry{
		content:    "stale check",
		capturedAt: time.Now().Add(-checkCacheTTL - time.Second),
	}
	cc.mu.Unlock()

	result := cc.Drain("session-1")
	assert.Empty(t, result, "expired entry should be discarded")

	// Verify the entry was removed from the cache.
	cc.mu.Lock()
	_, exists := cc.cache["session-1"]
	cc.mu.Unlock()
	assert.False(t, exists, "expired entry should be deleted from cache")
}

func TestCheckCache_ConcurrentAccess(t *testing.T) {
	cc := newCheckCache()
	var wg sync.WaitGroup

	// Concurrent stores.
	for i := range 20 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			cc.Store("session-1", "result")
		}(i)
	}

	// Concurrent drains.
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = cc.Drain("session-1")
		}()
	}

	wg.Wait()
	// No race detector failures = pass.
}
