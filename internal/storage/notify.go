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
// Returns an error if no notify connection is configured.
func (db *DB) Listen(ctx context.Context, channel string) error {
	if db.notifyConn == nil {
		return fmt.Errorf("storage: notify connection not configured")
	}
	_, err := db.notifyConn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize())
	if err != nil {
		return fmt.Errorf("storage: listen %s: %w", channel, err)
	}
	return nil
}

// WaitForNotification blocks until a notification arrives on any listened channel.
// Returns the channel name and payload.
func (db *DB) WaitForNotification(ctx context.Context) (channel, payload string, err error) {
	if db.notifyConn == nil {
		return "", "", fmt.Errorf("storage: notify connection not configured")
	}
	notification, err := db.notifyConn.WaitForNotification(ctx)
	if err != nil {
		return "", "", fmt.Errorf("storage: wait for notification: %w", err)
	}
	return notification.Channel, notification.Payload, nil
}

// Notify sends a notification on the specified channel.
func (db *DB) Notify(ctx context.Context, channel, payload string) error {
	_, err := db.pool.Exec(ctx, "SELECT pg_notify($1, $2)", channel, payload)
	if err != nil {
		return fmt.Errorf("storage: notify %s: %w", channel, err)
	}
	return nil
}
