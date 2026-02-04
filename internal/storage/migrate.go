package storage

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// RunMigrations executes all SQL migration files from the provided filesystem in order.
// This is a simple forward-only migration runner for development and testing.
// Production should use Atlas for proper migration management.
func (db *DB) RunMigrations(ctx context.Context, migrationsFS fs.FS) error {
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

		content, err := fs.ReadFile(migrationsFS, entry.Name())
		if err != nil {
			return fmt.Errorf("storage: read migration %s: %w", entry.Name(), err)
		}

		db.logger.Info("running migration", "file", entry.Name())
		_, err = db.pool.Exec(ctx, string(content))
		if err != nil {
			return fmt.Errorf("storage: execute migration %s: %w", entry.Name(), err)
		}
	}

	return nil
}
