package mcp

import (
	"sync"
	"time"
)

// checkCacheTTL is the maximum age of a cached check result before it is
// discarded. If an agent takes longer than this between akashi_check and
// akashi_trace, the check result is stale and won't be auto-attached.
const checkCacheTTL = 5 * time.Minute

// checkCacheEntry stores a serialized akashi_check response for a session.
type checkCacheEntry struct {
	content    string
	hadResults bool // true when the check returned at least one decision
	capturedAt time.Time
}

// checkCache stores the most recent akashi_check response per MCP session,
// so handleTrace can auto-inject it as evidence. Keyed by MCP session ID —
// both handleCheck and handleTrace run on the same session, so this avoids
// the session ID mismatch problem that plagued the PostToolUse approach
// (Claude Code uses different session IDs for MCP vs built-in tools).
type checkCache struct {
	mu    sync.Mutex
	cache map[string]checkCacheEntry
}

func newCheckCache() *checkCache {
	return &checkCache{
		cache: make(map[string]checkCacheEntry),
	}
}

// Store saves a check result for the given session, replacing any previous entry.
// hadResults indicates whether the check returned at least one decision.
func (cc *checkCache) Store(sessionID, content string, hadResults bool) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.cache[sessionID] = checkCacheEntry{
		content:    content,
		hadResults: hadResults,
		capturedAt: time.Now(),
	}
}

// HadResults returns whether the cached check for this session returned decisions.
// Non-destructive: the entry remains in the cache for Drain to consume.
func (cc *checkCache) HadResults(sessionID string) bool {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	entry, ok := cc.cache[sessionID]
	if !ok {
		return false
	}
	if time.Since(entry.capturedAt) > checkCacheTTL {
		return false
	}
	return entry.hadResults
}

// Drain returns and removes the cached check result for the given session.
// Returns empty string if no entry exists or the entry has expired.
func (cc *checkCache) Drain(sessionID string) string {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	entry, ok := cc.cache[sessionID]
	if !ok {
		return ""
	}
	delete(cc.cache, sessionID)

	if time.Since(entry.capturedAt) > checkCacheTTL {
		return ""
	}
	return entry.content
}
