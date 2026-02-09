package storage

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// NotifyChannel is a Postgres LISTEN/NOTIFY channel name.
const (
	ChannelDecisions = "akashi_decisions"
	ChannelConflicts = "akashi_conflicts"
)

// Listen starts listening on the specified channel using the dedicated notify connection.
// The channel is tracked so it can be re-established automatically after a reconnect.
func (db *DB) Listen(ctx context.Context, channel string) error {
	db.notifyMu.Lock()
	defer db.notifyMu.Unlock()

	if db.notifyConn == nil {
		return fmt.Errorf("storage: notify connection not configured")
	}
	_, err := db.notifyConn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize())
	if err != nil {
		return fmt.Errorf("storage: listen %s: %w", channel, err)
	}

	// Track for automatic re-subscription after reconnect.
	for _, ch := range db.listenChannels {
		if ch == channel {
			return nil // Already listening.
		}
	}
	db.listenChannels = append(db.listenChannels, channel)
	return nil
}

// WaitForNotification blocks until a notification arrives on any listened channel.
// If the connection is lost, it attempts to reconnect with exponential backoff.
// Returns the channel name and payload. The caller should retry on error after
// a successful reconnect (indicated by an error wrapping the original failure).
func (db *DB) WaitForNotification(ctx context.Context) (channel, payload string, err error) {
	db.notifyMu.Lock()
	conn := db.notifyConn
	db.notifyMu.Unlock()

	if conn == nil {
		return "", "", fmt.Errorf("storage: notify connection not configured")
	}

	notification, err := conn.WaitForNotification(ctx)
	if err != nil {
		// Connection may have dropped. Attempt reconnect.
		db.notifyMu.Lock()
		reconnectErr := db.reconnectNotify(ctx)
		db.notifyMu.Unlock()

		if reconnectErr != nil {
			return "", "", fmt.Errorf("storage: notification failed and reconnect failed: %w (original: %w)", reconnectErr, err)
		}
		// Reconnect succeeded. Return the original error so the caller can retry.
		return "", "", fmt.Errorf("storage: notification interrupted, connection restored (retry): %w", err)
	}
	return notification.Channel, notification.Payload, nil
}

// Notify sends a notification on the specified channel via the connection pool.
func (db *DB) Notify(ctx context.Context, channel, payload string) error {
	_, err := db.pool.Exec(ctx, "SELECT pg_notify($1, $2)", channel, payload)
	if err != nil {
		return fmt.Errorf("storage: notify %s: %w", channel, err)
	}
	return nil
}
