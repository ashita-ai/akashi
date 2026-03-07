// Package sqlite provides a SQLite-backed implementation of storage.Store
// for Akashi's local-lite mode. It requires no external infrastructure
// (no Docker, no Postgres, no Qdrant, no Ollama) and starts in under 3 seconds.
//
// See ADR-009 for the architectural rationale behind this backend.
package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/ashita-ai/akashi/internal/storage"
)

//go:embed schema.sql
var schemaSQL string

// LiteDB is a SQLite-backed storage.Store implementation.
type LiteDB struct {
	db     *sql.DB
	logger *slog.Logger
}

// New opens (or creates) a SQLite database at path and applies the schema.
// Pass ":memory:" for an in-memory database (useful for tests).
func New(ctx context.Context, path string, logger *slog.Logger) (*LiteDB, error) {
	if path != ":memory:" {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // G301: user-data directory, group-readable is fine
			return nil, fmt.Errorf("sqlite: create directory %s: %w", dir, err)
		}
	}

	// _txlock=immediate ensures BEGIN starts an immediate lock, avoiding
	// SQLITE_BUSY on the first write inside a transaction.
	dsn := path
	if path != ":memory:" {
		dsn = fmt.Sprintf("file:%s?_txlock=immediate&_busy_timeout=5000", path)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}

	// SQLite is single-writer; limit to 1 connection for writes,
	// but allow concurrent readers.
	db.SetMaxOpenConns(1)

	// Enable WAL mode and foreign keys.
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close() //nolint:errcheck,gosec // best-effort cleanup on error path
			return nil, fmt.Errorf("sqlite: %s: %w", pragma, err)
		}
	}

	// Apply schema (all CREATE IF NOT EXISTS, idempotent).
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		db.Close() //nolint:errcheck,gosec // best-effort cleanup on error path
		return nil, fmt.Errorf("sqlite: apply schema: %w", err)
	}

	return &LiteDB{db: db, logger: logger}, nil
}

// Ping checks database connectivity.
func (l *LiteDB) Ping(ctx context.Context) error {
	return l.db.PingContext(ctx)
}

// Close shuts down the database connection.
func (l *LiteDB) Close(_ context.Context) {
	if err := l.db.Close(); err != nil {
		l.logger.Warn("sqlite: close", "error", err)
	}
}

// RawDB returns the underlying *sql.DB for use by cross-cutting concerns
// (search index, conflict scorer) that need direct database access.
func (l *LiteDB) RawDB() *sql.DB {
	return l.db
}

// Compile-time assertion: *LiteDB satisfies storage.Store.
var _ storage.Store = (*LiteDB)(nil)
