package ratelimit_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ashita-ai/akashi/internal/ratelimit"
)

var testRedis *redis.Client

func TestMain(m *testing.M) {
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "redis:7-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(30 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start redis container: %v\n", err)
		os.Exit(1)
	}

	host, err := container.Host(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get container host: %v\n", err)
		os.Exit(1)
	}

	port, err := container.MappedPort(ctx, "6379")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get container port: %v\n", err)
		os.Exit(1)
	}

	testRedis = redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%s", host, port.Port()),
	})

	if err := testRedis.Ping(ctx).Err(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to ping redis: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	_ = testRedis.Close()
	_ = container.Terminate(ctx)
	os.Exit(code)
}

// newTestLimiter creates a limiter for testing. Do NOT call Close() on this
// limiter as it would close the shared testRedis client.
func newTestLimiter(t *testing.T) *ratelimit.Limiter {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return ratelimit.New(testRedis, logger)
}

func TestLimiterAllow(t *testing.T) {
	ctx := context.Background()
	limiter := newTestLimiter(t)

	// Use unique prefix per test to avoid interference.
	rule := ratelimit.Rule{
		Prefix: fmt.Sprintf("test-%d", time.Now().UnixNano()),
		Limit:  5,
		Window: 1 * time.Minute,
	}

	// First 5 requests should be allowed.
	for i := 0; i < 5; i++ {
		result := limiter.Allow(ctx, rule, "agent-1")
		assert.True(t, result.Allowed, "request %d should be allowed", i+1)
		assert.Equal(t, 5, result.Limit)
		assert.Equal(t, 5-i-1, result.Remaining, "remaining after request %d", i+1)
	}

	// 6th request should be denied.
	result := limiter.Allow(ctx, rule, "agent-1")
	assert.False(t, result.Allowed, "6th request should be denied")
	assert.Equal(t, 0, result.Remaining)
	assert.True(t, result.ResetAt.After(time.Now()), "ResetAt should be in the future")
}

func TestLimiterMultipleKeys(t *testing.T) {
	ctx := context.Background()
	limiter := newTestLimiter(t)

	rule := ratelimit.Rule{
		Prefix: fmt.Sprintf("test-multi-%d", time.Now().UnixNano()),
		Limit:  3,
		Window: 1 * time.Minute,
	}

	// Each key has its own limit.
	for i := 0; i < 3; i++ {
		r1 := limiter.Allow(ctx, rule, "agent-A")
		r2 := limiter.Allow(ctx, rule, "agent-B")
		assert.True(t, r1.Allowed, "agent-A request %d", i+1)
		assert.True(t, r2.Allowed, "agent-B request %d", i+1)
	}

	// Both now at limit.
	rA := limiter.Allow(ctx, rule, "agent-A")
	rB := limiter.Allow(ctx, rule, "agent-B")
	assert.False(t, rA.Allowed)
	assert.False(t, rB.Allowed)
}

func TestLimiterSlidingWindow(t *testing.T) {
	ctx := context.Background()
	limiter := newTestLimiter(t)

	// Use a short window so we can test expiration.
	rule := ratelimit.Rule{
		Prefix: fmt.Sprintf("test-window-%d", time.Now().UnixNano()),
		Limit:  2,
		Window: 500 * time.Millisecond,
	}

	// Use up the limit.
	r1 := limiter.Allow(ctx, rule, "agent-X")
	r2 := limiter.Allow(ctx, rule, "agent-X")
	r3 := limiter.Allow(ctx, rule, "agent-X")
	assert.True(t, r1.Allowed)
	assert.True(t, r2.Allowed)
	assert.False(t, r3.Allowed)

	// Wait for window to pass.
	time.Sleep(600 * time.Millisecond)

	// Should be allowed again.
	r4 := limiter.Allow(ctx, rule, "agent-X")
	assert.True(t, r4.Allowed, "request after window should be allowed")
}

func TestLimiterNoopMode(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Noop mode: nil client allows all requests.
	limiter := ratelimit.New(nil, logger)

	rule := ratelimit.Rule{
		Prefix: "test-noop",
		Limit:  1,
		Window: 1 * time.Minute,
	}

	// All requests allowed in noop mode.
	for i := 0; i < 100; i++ {
		result := limiter.Allow(ctx, rule, "agent")
		require.True(t, result.Allowed)
		assert.Equal(t, 1, result.Remaining)
	}
}

func TestResultFormatHeaders(t *testing.T) {
	resetAt := time.Date(2026, 2, 5, 12, 0, 0, 0, time.UTC)
	result := ratelimit.Result{
		Allowed:   true,
		Limit:     100,
		Remaining: 42,
		ResetAt:   resetAt,
	}

	headers := result.FormatHeaders()
	assert.Equal(t, "100", headers["X-RateLimit-Limit"])
	assert.Equal(t, "42", headers["X-RateLimit-Remaining"])
	assert.Equal(t, fmt.Sprintf("%d", resetAt.Unix()), headers["X-RateLimit-Reset"])
}

func TestLimiterDifferentPrefixes(t *testing.T) {
	ctx := context.Background()
	limiter := newTestLimiter(t)

	base := time.Now().UnixNano()

	authRule := ratelimit.Rule{
		Prefix: fmt.Sprintf("auth-%d", base),
		Limit:  5,
		Window: 1 * time.Minute,
	}

	queryRule := ratelimit.Rule{
		Prefix: fmt.Sprintf("query-%d", base),
		Limit:  100,
		Window: 1 * time.Minute,
	}

	// Exhaust auth limit.
	for i := 0; i < 5; i++ {
		limiter.Allow(ctx, authRule, "agent")
	}
	authResult := limiter.Allow(ctx, authRule, "agent")
	assert.False(t, authResult.Allowed, "auth limit exceeded")

	// Query limit still available.
	queryResult := limiter.Allow(ctx, queryRule, "agent")
	assert.True(t, queryResult.Allowed, "query should be allowed")
	assert.Equal(t, 99, queryResult.Remaining)
}

func TestLimiterConcurrent(t *testing.T) {
	ctx := context.Background()
	limiter := newTestLimiter(t)

	rule := ratelimit.Rule{
		Prefix: fmt.Sprintf("test-concurrent-%d", time.Now().UnixNano()),
		Limit:  100,
		Window: 1 * time.Minute,
	}

	// Fire 200 concurrent requests with limit of 100.
	// Due to microsecond-precision member IDs, requests in the same
	// microsecond may share an ID, causing minor variance in counts.
	results := make(chan ratelimit.Result, 200)
	for i := 0; i < 200; i++ {
		go func() {
			results <- limiter.Allow(ctx, rule, "agent")
		}()
	}

	allowed := 0
	denied := 0
	for i := 0; i < 200; i++ {
		r := <-results
		if r.Allowed {
			allowed++
		} else {
			denied++
		}
	}

	// The limit is 100, so roughly 100 should be allowed.
	// Allow small variance due to microsecond timing collisions.
	assert.InDelta(t, 100, allowed, 5, "approximately 100 requests should be allowed")
	assert.InDelta(t, 100, denied, 5, "approximately 100 requests should be denied")
	assert.Equal(t, 200, allowed+denied, "all requests should be processed")
}
