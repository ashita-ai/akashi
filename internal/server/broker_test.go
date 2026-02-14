package server

import (
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// testLogger returns a logger for tests that discards output.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestBrokerFanOut(t *testing.T) {
	orgID := uuid.New()
	broker := &Broker{
		subscribers: make(map[chan []byte]subscriber),
		logger:      testLogger(),
	}

	// Subscribe two clients in the same org.
	ch1 := broker.Subscribe(orgID)
	ch2 := broker.Subscribe(orgID)

	// Broadcast an event to that org.
	event := formatSSE("akashi_decisions", `{"decision_id":"abc"}`)
	broker.broadcastToOrg(event, orgID)

	// Both should receive it.
	select {
	case got := <-ch1:
		if string(got) != string(event) {
			t.Errorf("ch1: got %q, want %q", got, event)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ch1: timed out waiting for event")
	}

	select {
	case got := <-ch2:
		if string(got) != string(event) {
			t.Errorf("ch2: got %q, want %q", got, event)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ch2: timed out waiting for event")
	}

	// Unsubscribe ch1, broadcast again — only ch2 should receive.
	broker.Unsubscribe(ch1)
	event2 := formatSSE("akashi_decisions", `{"decision_id":"def"}`)
	broker.broadcastToOrg(event2, orgID)

	select {
	case got := <-ch2:
		if string(got) != string(event2) {
			t.Errorf("ch2: got %q, want %q", got, event2)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ch2: timed out waiting for event after ch1 unsubscribed")
	}

	broker.Unsubscribe(ch2)
}

func TestBrokerOrgIsolation(t *testing.T) {
	org1 := uuid.New()
	org2 := uuid.New()
	broker := &Broker{
		subscribers: make(map[chan []byte]subscriber),
		logger:      testLogger(),
	}

	ch1 := broker.Subscribe(org1)
	ch2 := broker.Subscribe(org2)

	// Broadcast to org1 only.
	event := formatSSE("akashi_decisions", `{"decision_id":"abc"}`)
	broker.broadcastToOrg(event, org1)

	// ch1 (org1) should receive it.
	select {
	case got := <-ch1:
		if string(got) != string(event) {
			t.Errorf("ch1: got %q, want %q", got, event)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ch1: timed out waiting for event")
	}

	// ch2 (org2) should NOT receive it.
	select {
	case got := <-ch2:
		t.Fatalf("ch2 (different org) received event it should not have: %q", got)
	case <-time.After(50 * time.Millisecond):
		// Expected: no event for org2.
	}

	broker.Unsubscribe(ch1)
	broker.Unsubscribe(ch2)
}

func TestBrokerDropsNilOrgEvents(t *testing.T) {
	orgID := uuid.New()
	broker := &Broker{
		subscribers: make(map[chan []byte]subscriber),
		logger:      testLogger(),
	}

	ch := broker.Subscribe(orgID)

	// Broadcast with uuid.Nil — should be dropped.
	event := formatSSE("akashi_decisions", `{"decision_id":"abc"}`)
	broker.broadcastToOrg(event, uuid.Nil)

	select {
	case got := <-ch:
		t.Fatalf("subscriber received event that should have been dropped: %q", got)
	case <-time.After(50 * time.Millisecond):
		// Expected: event dropped.
	}

	broker.Unsubscribe(ch)
}

func TestExtractOrgID(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    uuid.UUID
	}{
		{
			name:    "valid org_id",
			payload: `{"org_id":"550e8400-e29b-41d4-a716-446655440000","decision_id":"abc"}`,
			want:    uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
		},
		{
			name:    "missing org_id",
			payload: `{"decision_id":"abc"}`,
			want:    uuid.Nil,
		},
		{
			name:    "invalid JSON",
			payload: `not json`,
			want:    uuid.Nil,
		},
		{
			name:    "empty org_id",
			payload: `{"org_id":"","decision_id":"abc"}`,
			want:    uuid.Nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractOrgID(tt.payload)
			if got != tt.want {
				t.Errorf("extractOrgID(%q) = %v, want %v", tt.payload, got, tt.want)
			}
		})
	}
}

func TestFormatSSE(t *testing.T) {
	got := string(formatSSE("akashi_decisions", `{"id":"123"}`))
	want := "event: akashi_decisions\ndata: {\"id\":\"123\"}\n\n"
	if got != want {
		t.Errorf("formatSSE single-line: got %q, want %q", got, want)
	}

	// Multi-line payloads: each line must be prefixed with "data: " per the SSE spec.
	gotMulti := string(formatSSE("test", "line1\nline2\nline3"))
	wantMulti := "event: test\ndata: line1\ndata: line2\ndata: line3\n\n"
	if gotMulti != wantMulti {
		t.Errorf("formatSSE multi-line: got %q, want %q", gotMulti, wantMulti)
	}
}

func TestBrokerSlowSubscriber(t *testing.T) {
	orgID := uuid.New()
	broker := &Broker{
		subscribers: make(map[chan []byte]subscriber),
		logger:      testLogger(),
	}

	// Create a slow subscriber (small buffer that we won't read from).
	slow := broker.Subscribe(orgID)
	fast := broker.Subscribe(orgID)

	// Fill the slow subscriber's buffer.
	for range 65 {
		broker.broadcastToOrg(formatSSE("test", "fill"), orgID)
	}

	// Fast subscriber should still get events.
	event := formatSSE("test", "after-fill")
	broker.broadcastToOrg(event, orgID)

	select {
	case <-fast:
		// Got a buffered event — fast subscriber is not blocked.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("fast subscriber should receive events even when slow subscriber is blocked")
	}

	broker.Unsubscribe(slow)
	broker.Unsubscribe(fast)
}

func TestBrokerClose(t *testing.T) {
	orgID := uuid.New()
	broker := &Broker{
		subscribers: make(map[chan []byte]subscriber),
		logger:      testLogger(),
	}

	ch := broker.Subscribe(orgID)

	// Verify the channel is open by confirming we can send to it without panic.
	event := formatSSE("test", `{"id":"close-test"}`)
	broker.broadcastToOrg(event, orgID)

	select {
	case got := <-ch:
		if string(got) != string(event) {
			t.Errorf("got %q, want %q", got, event)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for event before close")
	}

	// Unsubscribe closes the channel.
	broker.Unsubscribe(ch)

	// Reading from a closed channel returns the zero value immediately.
	// Verify the channel is closed by attempting a non-blocking receive.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to be closed after Unsubscribe, but received a value")
		}
		// ok == false means the channel is closed. This is the expected path.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel was not closed after Unsubscribe")
	}

	// Verify the subscriber was removed from the map.
	broker.mu.RLock()
	_, exists := broker.subscribers[ch]
	broker.mu.RUnlock()
	if exists {
		t.Fatal("subscriber should be removed from map after Unsubscribe")
	}
}

func TestBrokerConcurrentSubscribe(t *testing.T) {
	orgID := uuid.New()
	broker := &Broker{
		subscribers: make(map[chan []byte]subscriber),
		logger:      testLogger(),
	}

	const numGoroutines = 50
	channels := make([]chan []byte, numGoroutines)

	// Subscribe from multiple goroutines concurrently.
	var wg sync.WaitGroup
	for i := range numGoroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			channels[idx] = broker.Subscribe(orgID)
		}(i)
	}
	wg.Wait()

	// Verify all subscriptions were registered.
	broker.mu.RLock()
	count := len(broker.subscribers)
	broker.mu.RUnlock()
	if count != numGoroutines {
		t.Fatalf("expected %d subscribers, got %d", numGoroutines, count)
	}

	// Broadcast an event and verify all subscribers receive it.
	event := formatSSE("test", `{"concurrent":"true"}`)
	broker.broadcastToOrg(event, orgID)

	for i, ch := range channels {
		select {
		case got := <-ch:
			if string(got) != string(event) {
				t.Errorf("channel %d: got %q, want %q", i, got, event)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("channel %d: timed out waiting for event", i)
		}
	}

	// Unsubscribe all concurrently.
	for i := range numGoroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			broker.Unsubscribe(channels[idx])
		}(i)
	}
	wg.Wait()

	// Verify all subscribers were removed.
	broker.mu.RLock()
	remaining := len(broker.subscribers)
	broker.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("expected 0 subscribers after cleanup, got %d", remaining)
	}
}
