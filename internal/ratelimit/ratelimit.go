// Package ratelimit provides a pluggable rate limiting interface.
//
// The OSS distribution ships an in-memory token bucket (MemoryLimiter).
// Enterprise deployments can substitute a Redis-backed implementation
// for cross-instance coordination — the Limiter interface is the contract.
package ratelimit

import "context"

// Limiter decides whether a request identified by key should be allowed.
// Implementations must be safe for concurrent use.
type Limiter interface {
	// Allow returns true if the request should proceed.
	// The key is opaque — callers construct it (e.g. "org:<uuid>:agent:<id>").
	// Returning an error signals a limiter malfunction; callers should
	// treat errors as fail-open (permit the request) rather than blocking traffic.
	Allow(ctx context.Context, key string) (bool, error)

	// Close releases resources (cleanup goroutines, connections).
	Close() error
}

// NoopLimiter permits every request. Used when rate limiting is disabled.
type NoopLimiter struct{}

// Allow always returns true.
func (NoopLimiter) Allow(context.Context, string) (bool, error) { return true, nil }

// Close is a no-op.
func (NoopLimiter) Close() error { return nil }
