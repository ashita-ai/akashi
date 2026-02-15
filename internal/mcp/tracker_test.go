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
	// Deterministic test: insert 1100 entries with timestamps in the past,
	// then trigger purgeStale via a fresh Record. No sleep needed.
	tracker := newCheckTracker(time.Hour) // long window — we'll backdate entries

	// Fill with >1000 entries, all backdated to well before the window.
	staleTime := time.Now().Add(-2 * time.Hour)
	tracker.mu.Lock()
	for i := range 1100 {
		key := checkKey{
			agentID:      "agent-1",
			decisionType: string(rune('A'+i%26)) + string(rune('0'+i/26)),
		}
		tracker.checks[key] = staleTime
	}
	tracker.mu.Unlock()

	// Record two fresh entries. The first exceeds the 1000-entry threshold
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
