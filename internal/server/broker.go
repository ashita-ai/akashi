package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/storage"
)

// subscriber tracks an SSE subscriber's channel and org scope.
type subscriber struct {
	orgID uuid.UUID
}

// Broker fans out Postgres LISTEN/NOTIFY messages to SSE subscribers.
// It runs a background goroutine that calls db.WaitForNotification in a loop
// and sends each payload only to subscribers in the matching org.
type Broker struct {
	db     *storage.DB
	logger *slog.Logger

	mu          sync.RWMutex
	subscribers map[chan []byte]subscriber
}

// NewBroker creates a new SSE broker. Call Start to begin listening.
func NewBroker(db *storage.DB, logger *slog.Logger) *Broker {
	return &Broker{
		db:          db,
		logger:      logger,
		subscribers: make(map[chan []byte]subscriber),
	}
}

// Start begins listening on the decisions and conflicts channels.
// It blocks, so call it in a goroutine. Returns when ctx is cancelled.
// Each Listen call is retried with exponential backoff (up to 5 attempts)
// to handle transient connection issues during startup.
func (b *Broker) Start(ctx context.Context) {
	// Subscribe to the notification channels with retry.
	for _, ch := range []string{storage.ChannelDecisions, storage.ChannelConflicts} {
		if err := b.listenWithRetry(ctx, ch); err != nil {
			b.logger.Error("broker: failed to listen after retries, giving up",
				"channel", ch, "error", err)
			return
		}
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

		// Extract org_id from the notification payload for tenant isolation.
		orgID := extractOrgID(payload)

		// Format as SSE event.
		event := formatSSE(channel, payload)
		b.broadcastToOrg(event, orgID)
	}
}

// listenWithRetry attempts to subscribe to a Postgres LISTEN channel with
// exponential backoff. Returns nil on success, or the last error after 5 attempts.
func (b *Broker) listenWithRetry(ctx context.Context, ch string) error {
	const maxAttempts = 5
	var err error
	for attempt := range maxAttempts {
		if err = b.db.Listen(ctx, ch); err == nil {
			return nil
		}
		backoff := time.Duration(1<<attempt) * time.Second
		b.logger.Warn("broker: listen failed, retrying",
			"channel", ch, "attempt", attempt+1, "backoff", backoff, "error", err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("broker: listen %s failed after %d attempts: %w", ch, maxAttempts, err)
}

// Subscribe returns a channel that receives SSE-formatted events scoped to
// the given org. Only notifications whose payload contains a matching org_id
// are delivered to this subscriber.
func (b *Broker) Subscribe(orgID uuid.UUID) chan []byte {
	ch := make(chan []byte, 64) // Buffer to avoid blocking the broadcast loop.
	b.mu.Lock()
	b.subscribers[ch] = subscriber{orgID: orgID}
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

// broadcastToOrg sends an event only to subscribers belonging to the given org.
// If orgID is uuid.Nil (e.g. payload couldn't be parsed), the event is dropped
// rather than leaked to all tenants. Slow subscribers that have a full buffer
// are skipped to prevent one slow client from blocking all others.
func (b *Broker) broadcastToOrg(event []byte, orgID uuid.UUID) {
	if orgID == uuid.Nil {
		b.logger.Warn("broker: dropping event with unparseable org_id")
		return
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	for ch, sub := range b.subscribers {
		if sub.orgID != orgID {
			continue
		}
		select {
		case ch <- event:
		default:
			b.logger.Warn("broker: dropped event for slow subscriber",
				"org_id", orgID,
				"buffer_cap", cap(ch),
				"event_size", len(event))
		}
	}
}

// extractOrgID parses the notification payload JSON to extract the org_id field.
// Returns uuid.Nil if the payload is not valid JSON or lacks an org_id.
func extractOrgID(payload string) uuid.UUID {
	var p struct {
		OrgID string `json:"org_id"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return uuid.Nil
	}
	id, err := uuid.Parse(p.OrgID)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// formatSSE formats a notification as a Server-Sent Events message.
// Per the SSE spec, each line in a multi-line data field must be
// prefixed with "data: " to avoid desynchronizing the client parser.
func formatSSE(eventType, data string) []byte {
	var buf bytes.Buffer
	buf.WriteString("event: ")
	buf.WriteString(eventType)
	buf.WriteByte('\n')
	for _, line := range strings.Split(data, "\n") {
		buf.WriteString("data: ")
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}
