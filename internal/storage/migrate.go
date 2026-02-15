package storage

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

const migrationAdvisoryLockKey int64 = 9021001

// RunMigrations executes unapplied SQL migration files from the provided filesystem in order.
// It tracks applied migrations in a schema_migrations table to ensure each file runs at most once.
// This is a simple forward-only migration runner for development and testing.
// Production should use Atlas for proper migration management.
func (db *DB) RunMigrations(ctx context.Context, migrationsFS fs.FS) error {
	// Ensure only one process runs migrations at a time.
	var gotLock bool
	if err := db.pool.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, migrationAdvisoryLockKey).Scan(&gotLock); err != nil {
		return fmt.Errorf("storage: acquire migration advisory lock: %w", err)
	}
	if !gotLock {
		return fmt.Errorf("storage: migrations already running in another process")
	}
	defer func() {
		if _, err := db.pool.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationAdvisoryLockKey); err != nil {
			db.logger.Warn("migration advisory unlock failed", "error", err)
		}
	}()

	// Ensure the tracking table exists. This is idempotent.
	if _, err := db.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("storage: create schema_migrations: %w", err)
	}

	// Load the set of already-applied migrations.
	applied, err := db.loadAppliedMigrations(ctx)
	if err != nil {
		return fmt.Errorf("storage: load applied migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, ".")
	if err != nil {
		return fmt.Errorf("storage: read migrations dir: %w", err)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		name := entry.Name()
		if applied[name] {
			db.logger.Debug("migration already applied, skipping", "file", name)
			continue
		}

		content, err := fs.ReadFile(migrationsFS, name)
		if err != nil {
			return fmt.Errorf("storage: read migration %s: %w", name, err)
		}

		db.logger.Info("running migration", "file", name)

		sql := string(content)
		if isTxModeNone(sql) {
			// Migrations with "-- atlas:txmode none" contain statements like
			// CREATE INDEX CONCURRENTLY that cannot run inside a transaction.
			// Execute them directly, then record separately.
			if err := db.runMigrationNoTx(ctx, name, sql); err != nil {
				return err
			}
		} else {
			// Execute and record the migration within a single transaction so a
			// failure between execution and recording can't leave an applied-but-
			// unrecorded migration.
			if err := db.runMigrationTx(ctx, name, sql); err != nil {
				return err
			}
		}
	}

	return nil
}

// isTxModeNone detects the Atlas "-- atlas:txmode none" directive in a migration.
// Migrations with this directive contain statements (e.g. CREATE INDEX CONCURRENTLY)
// that PostgreSQL forbids inside a transaction block.
func isTxModeNone(sql string) bool {
	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "-- atlas:txmode none" {
			return true
		}
		// Stop scanning after the comment header ends to avoid false positives
		// in SQL content. A non-comment, non-empty line means we're past the header.
		if trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			break
		}
	}
	return false
}

// runMigrationNoTx executes a migration outside a transaction (for statements
// like CREATE INDEX CONCURRENTLY) and then records it in schema_migrations.
//
// IMPORTANT: txmode-none migrations MUST be fully idempotent (IF NOT EXISTS /
// IF EXISTS) because there is an unavoidable window between execution and
// recording where a crash would leave the migration applied but unrecorded.
// On restart, the migration runner will re-execute it (#62).
//
// The recording step retries up to 3 times to reduce the probability of this
// window, but it cannot be eliminated without a transaction (which is
// incompatible with statements like CREATE INDEX CONCURRENTLY).
func (db *DB) runMigrationNoTx(ctx context.Context, name, sql string) error {
	if _, err := db.pool.Exec(ctx, sql); err != nil {
		return fmt.Errorf("storage: execute migration %s (no-tx): %w", name, err)
	}

	// Retry the recording INSERT to minimize the applied-but-unrecorded window.
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := db.pool.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`, name,
		); err != nil {
			lastErr = err
			db.logger.Warn("migration recording attempt failed",
				"file", name, "attempt", attempt, "error", err)
			continue
		}
		return nil
	}
	// If all retries fail, log at error level so operators notice.
	// The migration SQL already executed — next restart will re-run it,
	// which is safe only if the migration is idempotent.
	return fmt.Errorf("storage: record migration %s (no-tx) after 3 attempts: %w", name, lastErr)
}

// runMigrationTx executes a migration and records it in schema_migrations within
// a single transaction. If either step fails, both are rolled back.
func (db *DB) runMigrationTx(ctx context.Context, name, sql string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage: begin migration tx %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("storage: execute migration %s: %w", name, err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`, name,
	); err != nil {
		return fmt.Errorf("storage: record migration %s: %w", name, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("storage: commit migration %s: %w", name, err)
	}
	return nil
}

// loadAppliedMigrations returns the set of migration filenames already recorded
// in the schema_migrations table.
func (db *DB) loadAppliedMigrations(ctx context.Context) (map[string]bool, error) {
	rows, err := db.pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}
