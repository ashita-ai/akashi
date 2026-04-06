package mcp

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCheckCache_StoreAndHadResults(t *testing.T) {
	cc := newCheckCache()

	cc.Store("session-1", true)
	assert.True(t, cc.HadResults("session-1"))

	cc.Store("session-2", false)
	assert.False(t, cc.HadResults("session-2"))
}

func TestCheckCache_HadResults_ConsumeOnRead(t *testing.T) {
	cc := newCheckCache()

	cc.Store("session-1", true)
	assert.True(t, cc.HadResults("session-1"), "first call should return true")
	assert.False(t, cc.HadResults("session-1"), "second call should return false after consume")
}

func TestCheckCache_HadResults_Missing(t *testing.T) {
	cc := newCheckCache()
	assert.False(t, cc.HadResults("nonexistent"))
}

func TestCheckCache_SessionIsolation(t *testing.T) {
	cc := newCheckCache()

	cc.Store("session-1", true)
	cc.Store("session-2", false)

	assert.True(t, cc.HadResults("session-1"))
	assert.False(t, cc.HadResults("session-2"))
}

func TestCheckCache_StoreOverwritesPrevious(t *testing.T) {
	cc := newCheckCache()

	cc.Store("session-1", false)
	cc.Store("session-1", true)

	assert.True(t, cc.HadResults("session-1"), "latest store should overwrite previous")
}

func TestCheckCache_TTLExpiry(t *testing.T) {
	cc := newCheckCache()

	// Inject an expired entry directly.
	cc.mu.Lock()
	cc.cache["session-1"] = checkCacheEntry{
		hadResults: true,
		capturedAt: time.Now().Add(-checkCacheTTL - time.Second),
	}
	cc.mu.Unlock()

	assert.False(t, cc.HadResults("session-1"), "expired entry should report no results")

	// Verify the entry was cleaned up.
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
			cc.Store("session-1", n%2 == 0)
		}(i)
	}

	// Concurrent reads.
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = cc.HadResults("session-1")
		}()
	}

	wg.Wait()
	// No race detector failures = pass.
}
