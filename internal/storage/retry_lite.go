//go:build lite

package storage

import (
	"context"
	"time"
)

// WithRetry executes fn without retrying. SQLite's single-writer model with WAL
// mode does not produce the serialization_failure or deadlock_detected errors
// that the PostgreSQL retry logic handles.
func WithRetry(_ context.Context, _ int, _ time.Duration, fn func() error) error {
	return fn()
}
