package sqlite

import "context"

// Notify is a no-op for SQLite (no LISTEN/NOTIFY support).
func (l *LiteDB) Notify(_ context.Context, _, _ string) error {
	return nil
}

// HasNotifyConn always returns false for SQLite.
func (l *LiteDB) HasNotifyConn() bool {
	return false
}
