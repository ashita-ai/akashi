package mcp

import (
	"sync"
	"time"
)

// checkCacheTTL is the maximum age of a cached check result before it is
// discarded. If an agent takes longer than this between akashi_check and
// akashi_trace, the check result is stale and won't be consulted.
const checkCacheTTL = 5 * time.Minute

// checkCacheEntry records whether a check returned results for a session.
type checkCacheEntry struct {
	hadResults bool // true when the check returned at least one decision
	capturedAt time.Time
}

// checkCache tracks whether the most recent akashi_check per MCP session
// returned results, so handleTrace can nudge the agent to cite precedents.
// Keyed by MCP session ID.
type checkCache struct {
	mu    sync.Mutex
	cache map[string]checkCacheEntry
}

func newCheckCache() *checkCache {
	return &checkCache{
		cache: make(map[string]checkCacheEntry),
	}
}

// Store saves whether a check returned results for the given session,
// replacing any previous entry.
func (cc *checkCache) Store(sessionID string, hadResults bool) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.cache[sessionID] = checkCacheEntry{
		hadResults: hadResults,
		capturedAt: time.Now(),
	}
}

// HadResults returns whether the cached check for this session returned
// decisions and removes the entry. Consume-on-read ensures the precedent
// penalty in handleTrace fires at most once per check-then-trace cycle.
func (cc *checkCache) HadResults(sessionID string) bool {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	entry, ok := cc.cache[sessionID]
	if !ok {
		return false
	}
	delete(cc.cache, sessionID)
	if time.Since(entry.capturedAt) > checkCacheTTL {
		return false
	}
	return entry.hadResults
}
