package server

import (
	"log/slog"
	"os"
	"testing"
	"time"
)

// testLogger returns a logger for tests that discards output.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestBrokerFanOut(t *testing.T) {
	broker := &Broker{
		subscribers: make(map[chan []byte]struct{}),
		logger:      testLogger(),
	}

	// Subscribe two clients.
	ch1 := broker.Subscribe()
	ch2 := broker.Subscribe()

	// Broadcast an event.
	event := formatSSE("akashi_decisions", `{"decision_id":"abc"}`)
	broker.broadcast(event)

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
	broker.broadcast(event2)

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

func TestFormatSSE(t *testing.T) {
	got := string(formatSSE("akashi_decisions", `{"id":"123"}`))
	want := "event: akashi_decisions\ndata: {\"id\":\"123\"}\n\n"
	if got != want {
		t.Errorf("formatSSE: got %q, want %q", got, want)
	}
}

func TestBrokerSlowSubscriber(t *testing.T) {
	broker := &Broker{
		subscribers: make(map[chan []byte]struct{}),
		logger:      testLogger(),
	}

	// Create a slow subscriber (small buffer that we won't read from).
	slow := broker.Subscribe()
	fast := broker.Subscribe()

	// Fill the slow subscriber's buffer.
	for range 65 {
		broker.broadcast(formatSSE("test", "fill"))
	}

	// Fast subscriber should still get events.
	event := formatSSE("test", "after-fill")
	broker.broadcast(event)

	select {
	case <-fast:
		// Got a buffered event — fast subscriber is not blocked.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("fast subscriber should receive events even when slow subscriber is blocked")
	}

	broker.Unsubscribe(slow)
	broker.Unsubscribe(fast)
}
