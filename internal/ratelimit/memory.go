package ratelimit

import (
	"context"
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

// Allow consumes one token from the bucket for key. Returns true if a token
// was available (request should proceed), false otherwise (rate limited).
func (m *MemoryLimiter) Allow(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	b, ok := m.buckets[key]
	if !ok {
		// First request for this key: start with a full bucket minus one token.
		m.buckets[key] = &bucket{
			tokens:     m.burst - 1,
			lastAccess: now,
		}
		return true, nil
	}

	// Refill tokens based on elapsed time.
	elapsed := now.Sub(b.lastAccess).Seconds()
	b.tokens += elapsed * m.rate
	if b.tokens > m.burst {
		b.tokens = m.burst
	}
	b.lastAccess = now

	if b.tokens < 1 {
		return false, nil
	}
	b.tokens--
	return true, nil
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
