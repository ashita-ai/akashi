//go:build !lite

package storage

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// WithTx executes fn within a database transaction. It begins a transaction,
// calls fn, and commits on success. If fn returns an error or panics, the
// transaction is rolled back.
func (db *DB) WithTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
