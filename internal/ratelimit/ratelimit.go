// Package ratelimit provides a pluggable rate limiting interface.
//
// The OSS distribution ships an in-memory token bucket (MemoryLimiter).
// Enterprise deployments can substitute a Redis-backed implementation
// for cross-instance coordination — the Limiter interface is the contract.
package ratelimit

import (
	"context"
	"time"
)

// Result carries the outcome of a rate limit check along with the bucket
// state needed to populate standard X-RateLimit-* response headers.
type Result struct {
	// Allowed is true when the request should proceed.
	Allowed bool

	// Limit is the bucket capacity (burst). Zero means "no limit" (NoopLimiter).
	Limit int

	// Remaining tokens after this request (floored to int, never negative).
	Remaining int

	// ResetAt is the time when the bucket will next be full.
	// Zero value means the bucket is already full or limiting is disabled.
	// This is always "time until full bucket" regardless of whether the
	// request was allowed or denied — use RetryAfter for 429 Retry-After.
	ResetAt time.Time

	// RetryAfter is the duration a denied client should wait before retrying.
	// Only set when Allowed is false. Zero means not applicable (request allowed).
	RetryAfter time.Duration
}

// Limiter decides whether a request identified by key should be allowed.
// Implementations must be safe for concurrent use.
type Limiter interface {
	// Allow checks whether the request identified by key should proceed.
	// The key is opaque — callers construct it (e.g. "org:<uuid>:agent:<id>").
	// Returning an error signals a limiter malfunction; callers should
	// treat errors as fail-open (permit the request) rather than blocking traffic.
	Allow(ctx context.Context, key string) (Result, error)

	// Close releases resources (cleanup goroutines, connections).
	Close() error
}

// NoopLimiter permits every request. Used when rate limiting is disabled.
type NoopLimiter struct{}

// Allow always permits the request and returns a zero-Limit result,
// signalling to callers that rate limiting is disabled.
func (NoopLimiter) Allow(context.Context, string) (Result, error) {
	return Result{Allowed: true}, nil
}

// Close is a no-op.
func (NoopLimiter) Close() error { return nil }
