package storage

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// RunMigrations executes unapplied SQL migration files from the provided filesystem in order.
// It tracks applied migrations in a schema_migrations table to ensure each file runs at most once.
// This is a simple forward-only migration runner for development and testing.
// Production should use Atlas for proper migration management.
func (db *DB) RunMigrations(ctx context.Context, migrationsFS fs.FS) error {
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
		if _, err := db.pool.Exec(ctx, string(content)); err != nil {
			return fmt.Errorf("storage: execute migration %s: %w", name, err)
		}

		// Record the migration as applied.
		if _, err := db.pool.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`, name,
		); err != nil {
			return fmt.Errorf("storage: record migration %s: %w", name, err)
		}
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
