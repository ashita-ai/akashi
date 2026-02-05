package server

import (
	"context"
	"log/slog"
	"sync"

	"github.com/ashita-ai/akashi/internal/storage"
)

// Broker fans out Postgres LISTEN/NOTIFY messages to SSE subscribers.
// It runs a background goroutine that calls db.WaitForNotification in a loop
// and sends each payload to all active subscriber channels.
type Broker struct {
	db     *storage.DB
	logger *slog.Logger

	mu          sync.RWMutex
	subscribers map[chan []byte]struct{}
}

// NewBroker creates a new SSE broker. Call Start to begin listening.
func NewBroker(db *storage.DB, logger *slog.Logger) *Broker {
	return &Broker{
		db:          db,
		logger:      logger,
		subscribers: make(map[chan []byte]struct{}),
	}
}

// Start begins listening on the decisions and conflicts channels.
// It blocks, so call it in a goroutine. Returns when ctx is cancelled.
func (b *Broker) Start(ctx context.Context) {
	// Subscribe to the notification channels.
	if err := b.db.Listen(ctx, storage.ChannelDecisions); err != nil {
		b.logger.Error("broker: listen decisions", "error", err)
		return
	}
	if err := b.db.Listen(ctx, storage.ChannelConflicts); err != nil {
		b.logger.Error("broker: listen conflicts", "error", err)
		return
	}

	b.logger.Info("broker: listening for notifications",
		"channels", []string{storage.ChannelDecisions, storage.ChannelConflicts})

	for {
		channel, payload, err := b.db.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return // Shutting down.
			}
			b.logger.Warn("broker: notification error, retrying", "error", err)
			continue
		}

		// Format as SSE event.
		event := formatSSE(channel, payload)
		b.broadcast(event)
	}
}

// Subscribe returns a channel that receives SSE-formatted events.
// The caller must call Unsubscribe when done.
func (b *Broker) Subscribe() chan []byte {
	ch := make(chan []byte, 64) // Buffer to avoid blocking the broadcast loop.
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel and closes it.
func (b *Broker) Unsubscribe(ch chan []byte) {
	b.mu.Lock()
	delete(b.subscribers, ch)
	b.mu.Unlock()
	close(ch)
}

// broadcast sends an event to all subscribers. Slow subscribers that have
// a full buffer are skipped (their event is dropped) to prevent one slow
// client from blocking all others.
func (b *Broker) broadcast(event []byte) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for ch := range b.subscribers {
		select {
		case ch <- event:
		default:
			// Subscriber buffer full — drop this event for them.
		}
	}
}

// formatSSE formats a notification as a Server-Sent Events message.
func formatSSE(eventType, data string) []byte {
	// SSE format: "event: <type>\ndata: <payload>\n\n"
	return []byte("event: " + eventType + "\ndata: " + data + "\n\n")
}
