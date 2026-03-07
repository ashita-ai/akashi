// Package sqlite provides a SQLite-backed storage layer for akashi local-lite.
//
// This is a single-tenant, zero-infrastructure alternative to the PostgreSQL storage
// used by the full server. Schema is self-creating on first run (no Atlas migrations).
// Concurrent reads during async embedding writes are supported via WAL mode.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	_ "modernc.org/sqlite" // Pure-Go SQLite driver, no CGO.
)

// Store wraps a *sql.DB backed by SQLite for local-lite single-tenant use.
// Safe for concurrent use: WAL mode allows readers during async embedding writes.
type Store struct {
	db     *sql.DB
	logger *slog.Logger
}

// CheckResult holds a decision returned by Check, with its relevance score.
// This is the local-lite analog of model.SearchResult, simplified for
// single-tenant use: no org_id, no QueryFilters, no pgvector dependency.
type CheckResult struct {
	DecisionID   uuid.UUID
	DecisionType string
	Outcome      string
	Reasoning    *string
	Confidence   float32
	AgentID      string
	CreatedAt    time.Time
	Score        float64 // BM25 rank (negated) or cosine similarity.
}

// New opens (or creates) a SQLite database at dbPath and initializes the schema.
// Use ":memory:" for tests. The caller must call Close when done.
func New(dbPath string, logger *slog.Logger) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", dbPath, err)
	}

	// SQLite serializes all writes through a single writer and :memory: databases
	// are per-connection. A single connection avoids the separate-database problem
	// with in-memory databases and matches SQLite's natural concurrency model.
	// The database/sql pool serializes goroutine access to this one connection.
	// With sub-millisecond SQLite operations, this is imperceptible at local-lite scale.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, logger: logger}
	if err := s.setPragmas(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.ensureSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying *sql.DB for use by callers that need direct access
// (e.g., the local-lite trace pipeline). This is intentionally not hidden behind
// an interface: local-lite has a single storage backend with no need for abstraction.
func (s *Store) DB() *sql.DB {
	return s.db
}

// setPragmas configures SQLite for concurrent access and durability.
func (s *Store) setPragmas(ctx context.Context) error {
	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = NORMAL",
	}
	for _, p := range pragmas {
		if _, err := s.db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("sqlite: %s: %w", p, err)
		}
	}
	return nil
}

// ensureSchema creates tables and indexes if they don't exist.
// Idempotent: safe to call on every startup.
func (s *Store) ensureSchema(ctx context.Context) error {
	ddl := `
		CREATE TABLE IF NOT EXISTS decisions (
			id            TEXT PRIMARY KEY,
			decision_type TEXT NOT NULL,
			outcome       TEXT NOT NULL,
			reasoning     TEXT,
			confidence    REAL NOT NULL DEFAULT 0.7,
			agent_id      TEXT NOT NULL DEFAULT '',
			embedding     BLOB,
			created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%%H:%%M:%%fZ', 'now'))
		);

		CREATE INDEX IF NOT EXISTS idx_decisions_type ON decisions(decision_type);
		CREATE INDEX IF NOT EXISTS idx_decisions_created ON decisions(created_at DESC);
	`
	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("sqlite: create decisions table: %w", err)
	}

	// FTS5 virtual tables cannot use IF NOT EXISTS directly in all SQLite versions.
	// Check if the table exists first.
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='decisions_fts'`,
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("sqlite: check fts table: %w", err)
	}
	if count == 0 {
		_, err = s.db.ExecContext(ctx, `
			CREATE VIRTUAL TABLE decisions_fts USING fts5(
				decision_id UNINDEXED,
				content,
				tokenize = 'porter unicode61'
			)
		`)
		if err != nil {
			return fmt.Errorf("sqlite: create fts table: %w", err)
		}
	}

	return nil
}
