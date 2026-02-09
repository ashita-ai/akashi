// Package storage provides the PostgreSQL storage layer for Akashi.
//
// It manages connection pooling (via pgxpool through PgBouncer),
// a dedicated connection for LISTEN/NOTIFY (direct to Postgres),
// COPY-based batch ingestion for events, and query methods for all tables.
package storage

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvector "github.com/pgvector/pgvector-go/pgx"
)

// DB wraps a pgxpool.Pool for normal queries (via PgBouncer)
// and a dedicated pgx.Conn for LISTEN/NOTIFY (direct to Postgres).
type DB struct {
	pool       *pgxpool.Pool
	notifyConn *pgx.Conn
	notifyDSN  string
	notifyMu   sync.Mutex
	// listenChannels tracks subscribed channels so they can be re-established after reconnect.
	listenChannels []string
	logger         *slog.Logger
}

// New creates a new DB with a connection pool.
// poolDSN should point to PgBouncer (or directly to Postgres in dev).
// notifyDSN should point directly to Postgres for LISTEN/NOTIFY support.
func New(ctx context.Context, poolDSN, notifyDSN string, logger *slog.Logger) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(poolDSN)
	if err != nil {
		return nil, fmt.Errorf("storage: parse pool DSN: %w", err)
	}

	// Register pgvector types on each new connection so COPY can encode vectors.
	// The registration is best-effort: if the vector extension hasn't been
	// created yet (e.g. during initial pool startup before migrations),
	// we log a warning and proceed. Subsequent connections will succeed once
	// the extension exists.
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if err := pgxvector.RegisterTypes(ctx, conn); err != nil {
			logger.Debug("storage: pgvector types not registered (extension may not exist yet)", "error", err)
		}
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("storage: create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("storage: ping pool: %w", err)
	}

	var notifyConn *pgx.Conn
	if notifyDSN != "" {
		notifyConn, err = pgx.Connect(ctx, notifyDSN)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("storage: connect notify: %w", err)
		}
	}

	return &DB{
		pool:       pool,
		notifyConn: notifyConn,
		notifyDSN:  notifyDSN,
		logger:     logger,
	}, nil
}

// Pool returns the underlying connection pool for use by other packages.
func (db *DB) Pool() *pgxpool.Pool {
	return db.pool
}

// HasNotifyConn reports whether a dedicated LISTEN/NOTIFY connection is configured.
// Use this instead of accessing the raw connection — the connection is managed
// internally by WaitForNotification and its reconnect logic.
func (db *DB) HasNotifyConn() bool {
	db.notifyMu.Lock()
	defer db.notifyMu.Unlock()
	return db.notifyConn != nil
}

// Ping checks connectivity to the database.
func (db *DB) Ping(ctx context.Context) error {
	return db.pool.Ping(ctx)
}

// Close shuts down the connection pool and notify connection.
func (db *DB) Close(ctx context.Context) {
	db.pool.Close()
	db.notifyMu.Lock()
	defer db.notifyMu.Unlock()
	if db.notifyConn != nil {
		if err := db.notifyConn.Close(ctx); err != nil {
			db.logger.Warn("storage: close notify connection", "error", err)
		}
	}
}

// reconnectNotify attempts to re-establish the dedicated LISTEN/NOTIFY connection
// with exponential backoff and jitter. It re-subscribes to all previously tracked
// channels on success. Must be called with db.notifyMu held.
func (db *DB) reconnectNotify(ctx context.Context) error {
	if db.notifyDSN == "" {
		return fmt.Errorf("storage: no notify DSN configured")
	}

	// Close the old connection if it's still around.
	if db.notifyConn != nil {
		_ = db.notifyConn.Close(ctx)
		db.notifyConn = nil
	}

	const maxRetries = 5
	var lastErr error
	backoff := 500 * time.Millisecond

	for attempt := range maxRetries {
		if attempt > 0 {
			// Jitter: 0 to backoff/2.
			jitter := time.Duration(rand.Int64N(int64(backoff / 2))) //nolint:gosec // jitter doesn't need crypto-strength randomness
			sleep := backoff + jitter

			db.logger.Info("storage: reconnecting notify",
				"attempt", attempt+1,
				"backoff", sleep,
			)

			timer := time.NewTimer(sleep)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			backoff *= 2
		}

		conn, err := pgx.Connect(ctx, db.notifyDSN)
		if err != nil {
			lastErr = err
			db.logger.Warn("storage: notify reconnect attempt failed",
				"attempt", attempt+1,
				"error", err,
			)
			continue
		}

		// Re-subscribe to all tracked channels.
		resubOK := true
		for _, ch := range db.listenChannels {
			if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{ch}.Sanitize()); err != nil {
				_ = conn.Close(ctx)
				lastErr = err
				db.logger.Warn("storage: re-listen failed during reconnect",
					"channel", ch,
					"error", err,
				)
				resubOK = false
				break
			}
		}
		if !resubOK {
			continue
		}

		db.notifyConn = conn
		db.logger.Info("storage: notify connection restored",
			"attempt", attempt+1,
			"channels", db.listenChannels,
		)
		return nil
	}

	return fmt.Errorf("storage: notify reconnect failed after %d attempts: %w", maxRetries, lastErr)
}
