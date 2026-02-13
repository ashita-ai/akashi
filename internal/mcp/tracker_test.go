package mcp

import (
	"testing"
	"time"
)

func TestCheckTracker_RecordAndCheck(t *testing.T) {
	tracker := newCheckTracker(time.Hour)

	// Not checked yet.
	if tracker.WasChecked("agent-1", "architecture") {
		t.Fatal("expected WasChecked to return false before any Record")
	}

	// Record a check.
	tracker.Record("agent-1", "architecture")

	// Now it should return true.
	if !tracker.WasChecked("agent-1", "architecture") {
		t.Fatal("expected WasChecked to return true after Record")
	}
}

func TestCheckTracker_DifferentTypes(t *testing.T) {
	tracker := newCheckTracker(time.Hour)

	tracker.Record("agent-1", "architecture")

	// Same agent, different type — should not count.
	if tracker.WasChecked("agent-1", "security") {
		t.Fatal("expected WasChecked to return false for unchecked decision type")
	}
}

func TestCheckTracker_DifferentAgents(t *testing.T) {
	tracker := newCheckTracker(time.Hour)

	tracker.Record("agent-1", "architecture")

	// Different agent, same type — should not count.
	if tracker.WasChecked("agent-2", "architecture") {
		t.Fatal("expected WasChecked to return false for different agent")
	}
}

func TestCheckTracker_Expiry(t *testing.T) {
	// Use a very short window so entries expire immediately.
	tracker := newCheckTracker(time.Millisecond)

	tracker.Record("agent-1", "architecture")
	time.Sleep(5 * time.Millisecond)

	if tracker.WasChecked("agent-1", "architecture") {
		t.Fatal("expected WasChecked to return false after window expired")
	}
}

func TestCheckTracker_UpdateTimestamp(t *testing.T) {
	tracker := newCheckTracker(50 * time.Millisecond)

	tracker.Record("agent-1", "architecture")
	time.Sleep(30 * time.Millisecond)

	// Re-record to refresh the timestamp.
	tracker.Record("agent-1", "architecture")
	time.Sleep(30 * time.Millisecond)

	// Should still be valid because we refreshed.
	if !tracker.WasChecked("agent-1", "architecture") {
		t.Fatal("expected WasChecked to return true after timestamp refresh")
	}
}

func TestCheckTracker_PurgeStale(t *testing.T) {
	// Insert 1100 entries with timestamps we control, then verify purgeStale
	// removes the stale ones. We use a short window and a generous sleep to
	// avoid flaky failures on slow CI runners with -race overhead.
	tracker := newCheckTracker(50 * time.Millisecond)

	// Fill with >1000 entries.
	for i := range 1100 {
		tracker.Record("agent-1", string(rune('A'+i%26))+string(rune('0'+i/26)))
	}

	// Wait well beyond the window for all entries to become stale.
	// The generous margin (10x the window) absorbs GC pauses, scheduler
	// jitter, and -race instrumentation overhead on slow CI machines.
	time.Sleep(500 * time.Millisecond)

	// Record two fresh entries. The first one exceeds the 1000-entry threshold
	// and triggers purgeStale, which should remove all 1100 stale entries.
	tracker.Record("agent-fresh", "architecture")
	tracker.Record("agent-trigger", "trigger")

	if !tracker.WasChecked("agent-fresh", "architecture") {
		t.Fatal("expected fresh entry to survive purge")
	}

	tracker.mu.Lock()
	count := len(tracker.checks)
	tracker.mu.Unlock()
	if count > 10 {
		t.Fatalf("expected stale entries to be purged, got %d entries", count)
	}
}
