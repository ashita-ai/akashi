package mcp

import (
	"sync"
	"time"
)

// checkTracker records recent akashi_check calls so handleTrace can detect
// when a caller skips the check-before-trace workflow and nudge them.
//
// The tracker is keyed on (agentID, decisionType) with a configurable time
// window. If a check was recorded within the window, WasChecked returns true.
// This is an in-memory, per-process structure — it does not survive restarts,
// which is acceptable because the nudge is advisory, not a hard gate.
type checkTracker struct {
	mu     sync.Mutex
	checks map[checkKey]time.Time
	window time.Duration // how long a check is considered "recent"
}

type checkKey struct {
	agentID      string
	decisionType string
}

func newCheckTracker(window time.Duration) *checkTracker {
	return &checkTracker{
		checks: make(map[checkKey]time.Time),
		window: window,
	}
}

// Record notes that the given agent checked for precedents of this decision type.
func (t *checkTracker) Record(agentID, decisionType string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.checks[checkKey{agentID, decisionType}] = time.Now()

	// Lazy cleanup: if the map has grown large, purge stale entries to prevent
	// unbounded growth from many distinct (agent, type) pairs over time.
	if len(t.checks) > 1000 {
		t.purgeStale()
	}
}

// WasChecked reports whether the given agent checked this decision type
// within the configured time window.
func (t *checkTracker) WasChecked(agentID, decisionType string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ts, ok := t.checks[checkKey{agentID, decisionType}]
	if !ok {
		return false
	}
	if time.Since(ts) > t.window {
		delete(t.checks, checkKey{agentID, decisionType})
		return false
	}
	return true
}

// purgeStale removes entries older than the window. Must be called with mu held.
func (t *checkTracker) purgeStale() {
	now := time.Now()
	for k, ts := range t.checks {
		if now.Sub(ts) > t.window {
			delete(t.checks, k)
		}
	}
}
