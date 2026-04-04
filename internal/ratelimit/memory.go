package ratelimit

import (
	"context"
	"math"
	"sync"
	"time"
)

// bucket is a single token bucket for one rate-limit key.
type bucket struct {
	tokens     float64
	lastAccess time.Time
}

// MemoryLimiter implements Limiter using an in-memory token bucket per key.
//
// Each key gets an independent bucket with a configurable refill rate
// (tokens per second) and burst capacity (maximum tokens). A background
// goroutine evicts stale entries every minute to bound memory.
type MemoryLimiter struct {
	rate  float64 // tokens added per second
	burst float64 // maximum tokens (bucket capacity)

	mu      sync.Mutex
	buckets map[string]*bucket

	stopOnce sync.Once
	done     chan struct{}
}

// NewMemoryLimiter creates a token bucket limiter.
//   - rate: sustained requests per second per key
//   - burst: maximum burst size (token bucket capacity)
//
// A background goroutine evicts keys not accessed in the last 10 minutes.
// Call Close to stop it.
func NewMemoryLimiter(rate float64, burst int) *MemoryLimiter {
	m := &MemoryLimiter{
		rate:    rate,
		burst:   float64(burst),
		buckets: make(map[string]*bucket),
		done:    make(chan struct{}),
	}
	go m.cleanup()
	return m
}

// Allow consumes one token from the bucket for key. The returned Result
// includes the bucket state so callers can populate rate limit headers.
func (m *MemoryLimiter) Allow(_ context.Context, key string) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	burstInt := int(m.burst)

	b, ok := m.buckets[key]
	if !ok {
		// First request for this key: start with a full bucket minus one token.
		remaining := m.burst - 1
		m.buckets[key] = &bucket{
			tokens:     remaining,
			lastAccess: now,
		}
		return Result{
			Allowed:   true,
			Limit:     burstInt,
			Remaining: int(remaining),
			ResetAt:   m.resetAt(now, remaining),
		}, nil
	}

	// Refill tokens based on elapsed time.
	elapsed := now.Sub(b.lastAccess).Seconds()
	b.tokens += elapsed * m.rate
	if b.tokens > m.burst {
		b.tokens = m.burst
	}
	b.lastAccess = now

	if b.tokens < 1 {
		// Denied — compute when the next token arrives for Retry-After,
		// and when the bucket will be full for X-RateLimit-Reset.
		deficit := 1 - b.tokens
		retryAfter := time.Duration(math.Ceil(deficit/m.rate) * float64(time.Second))
		return Result{
			Allowed:    false,
			Limit:      burstInt,
			Remaining:  0,
			ResetAt:    m.resetAt(now, b.tokens),
			RetryAfter: retryAfter,
		}, nil
	}
	b.tokens--
	return Result{
		Allowed:   true,
		Limit:     burstInt,
		Remaining: int(b.tokens),
		ResetAt:   m.resetAt(now, b.tokens),
	}, nil
}

// resetAt computes when the bucket will be full given the current token count.
func (m *MemoryLimiter) resetAt(now time.Time, tokens float64) time.Time {
	if tokens >= m.burst {
		return time.Time{} // already full
	}
	deficit := m.burst - tokens
	seconds := deficit / m.rate
	return now.Add(time.Duration(math.Ceil(seconds) * float64(time.Second)))
}

// Close stops the cleanup goroutine. Safe to call multiple times.
func (m *MemoryLimiter) Close() error {
	m.stopOnce.Do(func() { close(m.done) })
	return nil
}

const staleThreshold = 10 * time.Minute

// cleanup periodically evicts buckets that haven't been accessed recently.
func (m *MemoryLimiter) cleanup() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.evictStale()
		}
	}
}

func (m *MemoryLimiter) evictStale() {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-staleThreshold)
	for key, b := range m.buckets {
		if b.lastAccess.Before(cutoff) {
			delete(m.buckets, key)
		}
	}
}
