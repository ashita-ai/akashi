package storage

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// isRetriable returns true for Postgres error codes that indicate a transient conflict.
func isRetriable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "40001": // serialization_failure
		return true
	case "40P01": // deadlock_detected
		return true
	default:
		return false
	}
}

// WithRetry executes fn, retrying up to maxRetries times on serialization or deadlock errors.
// Retries use jittered exponential backoff starting at baseDelay.
func WithRetry(ctx context.Context, maxRetries int, baseDelay time.Duration, fn func() error) error {
	var err error
	for attempt := range maxRetries + 1 {
		err = fn()
		if err == nil || !isRetriable(err) {
			return err
		}
		if attempt == maxRetries {
			break
		}
		jitter := time.Duration(rand.Int64N(int64(baseDelay))) //nolint:gosec // jitter doesn't need crypto-strength randomness
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(baseDelay + jitter):
		}
		baseDelay *= 2
	}
	return err
}
