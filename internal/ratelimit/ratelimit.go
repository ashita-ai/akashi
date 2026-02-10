// Package ratelimit provides Redis-backed sliding window rate limiting.
//
// Each rate limit uses a Redis sorted set keyed by (prefix, identifier).
// Entries are scored by timestamp. On each Allow call we atomically:
//  1. Remove entries outside the current window
//  2. Count remaining entries
//  3. If under limit, add the new request; otherwise reject
//
// All operations happen in a single Lua script for atomicity.
package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Lua script for atomic sliding window rate limiting.
// KEYS[1] = sorted set key
// ARGV[1] = window start (oldest allowed timestamp, microseconds)
// ARGV[2] = now (microseconds)
// ARGV[3] = limit
// ARGV[4] = unique member ID (now + random suffix to avoid collisions)
// ARGV[5] = TTL in seconds for the key (window size + buffer)
//
// Returns: {allowed (0 or 1), current_count, ttl_until_reset}
var slidingWindowScript = redis.NewScript(`
local key = KEYS[1]
local window_start = tonumber(ARGV[1])
local now = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local member = ARGV[4]
local ttl = tonumber(ARGV[5])

-- Remove entries outside the window.
redis.call('ZREMRANGEBYSCORE', key, '-inf', window_start)

-- Count current entries.
local count = redis.call('ZCARD', key)

if count < limit then
    -- Under limit: add this request.
    redis.call('ZADD', key, now, member)
    redis.call('EXPIRE', key, ttl)
    return {1, count + 1, 0}
else
    -- Over limit: compute time until the oldest entry expires.
    local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
    local reset_after = 0
    if #oldest >= 2 then
        reset_after = tonumber(oldest[2]) - window_start
    end
    redis.call('EXPIRE', key, ttl)
    return {0, count, reset_after}
end
`)

// Limiter provides rate limiting backed by Redis.
type Limiter struct {
	client     *redis.Client
	logger     *slog.Logger
	counter    atomic.Uint64
	failClosed bool // If true, deny requests when Redis errors (fail-closed).
}

// Rule defines a rate limit: how many requests per window.
type Rule struct {
	Prefix string        // Key prefix, e.g. "auth", "query", "trace".
	Limit  int           // Maximum requests per window.
	Window time.Duration // Window size.
}

// Result holds the outcome of a rate limit check.
type Result struct {
	Allowed   bool
	Limit     int
	Remaining int
	ResetAt   time.Time // When the window resets (for Retry-After).
}

// New creates a Limiter. If client is nil, all requests are allowed (noop mode).
// If failClosed is true, Redis errors at runtime deny requests instead of allowing them.
func New(client *redis.Client, logger *slog.Logger, failClosed bool) *Limiter {
	return &Limiter{client: client, logger: logger, failClosed: failClosed}
}

// Allow checks whether the request identified by key is within the rate limit.
// key is typically an agent ID or IP address.
func (l *Limiter) Allow(ctx context.Context, rule Rule, key string) Result {
	if l.client == nil {
		return Result{Allowed: true, Limit: rule.Limit, Remaining: rule.Limit}
	}

	now := time.Now()
	nowMicro := now.UnixMicro()
	windowStart := now.Add(-rule.Window).UnixMicro()
	ttlSeconds := int(rule.Window.Seconds()) + 10 // Key TTL = window + buffer.
	seq := l.counter.Add(1)
	member := fmt.Sprintf("%d:%d", nowMicro, seq) // Unique per request (atomic counter prevents ZADD collisions).

	redisKey := fmt.Sprintf("akashi:rl:%s:%s", rule.Prefix, key)

	res, err := slidingWindowScript.Run(ctx, l.client,
		[]string{redisKey},
		windowStart, nowMicro, rule.Limit, member, ttlSeconds,
	).Int64Slice()

	if err != nil {
		if l.failClosed {
			l.logger.Error("ratelimit: redis error, denying request (fail-closed)", "error", err, "key", redisKey)
			return Result{Allowed: false, Limit: rule.Limit, Remaining: 0, ResetAt: now.Add(rule.Window)}
		}
		l.logger.Warn("ratelimit: redis error, allowing request (fail-open)", "error", err, "key", redisKey)
		return Result{Allowed: true, Limit: rule.Limit, Remaining: rule.Limit}
	}

	allowed := res[0] == 1
	count := int(res[1])
	remaining := rule.Limit - count
	if remaining < 0 {
		remaining = 0
	}

	resetAt := now.Add(rule.Window)
	if !allowed && res[2] > 0 {
		// res[2] is microseconds until the oldest entry in window expires.
		resetAt = now.Add(time.Duration(res[2]) * time.Microsecond)
	}

	return Result{
		Allowed:   allowed,
		Limit:     rule.Limit,
		Remaining: remaining,
		ResetAt:   resetAt,
	}
}

// Close shuts down the Redis client.
func (l *Limiter) Close() error {
	if l.client == nil {
		return nil
	}
	return l.client.Close()
}

// FormatHeaders writes standard rate limit headers to an HTTP-compatible map.
func (r Result) FormatHeaders() map[string]string {
	return map[string]string{
		"X-RateLimit-Limit":     strconv.Itoa(r.Limit),
		"X-RateLimit-Remaining": strconv.Itoa(r.Remaining),
		"X-RateLimit-Reset":     strconv.FormatInt(r.ResetAt.Unix(), 10),
	}
}
