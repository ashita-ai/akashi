// Package storage provides the PostgreSQL storage layer for Kyoyu.
//
// It manages connection pooling (via pgxpool through PgBouncer),
// a dedicated connection for LISTEN/NOTIFY (direct to Postgres),
// COPY-based batch ingestion for events, and query methods for all tables.
package storage

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvector "github.com/pgvector/pgvector-go/pgx"
)

// DB wraps a pgxpool.Pool for normal queries (via PgBouncer)
// and a dedicated pgx.Conn for LISTEN/NOTIFY (direct to Postgres).
type DB struct {
	pool       *pgxpool.Pool
	notifyConn *pgx.Conn
	logger     *slog.Logger
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
		logger:     logger,
	}, nil
}

// Pool returns the underlying connection pool for use by other packages.
func (db *DB) Pool() *pgxpool.Pool {
	return db.pool
}

// NotifyConn returns the dedicated LISTEN/NOTIFY connection, or nil if not configured.
func (db *DB) NotifyConn() *pgx.Conn {
	return db.notifyConn
}

// Ping checks connectivity to the database.
func (db *DB) Ping(ctx context.Context) error {
	return db.pool.Ping(ctx)
}

// Close shuts down the connection pool and notify connection.
func (db *DB) Close(ctx context.Context) {
	db.pool.Close()
	if db.notifyConn != nil {
		if err := db.notifyConn.Close(ctx); err != nil {
			db.logger.Warn("storage: close notify connection", "error", err)
		}
	}
}
