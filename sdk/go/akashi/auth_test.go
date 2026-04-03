package akashi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestServer returns an httptest.Server that responds to /auth/token with a
// valid JWT-like token. refreshCount tracks how many times the endpoint was hit.
func newTestServer(t *testing.T, refreshCount *atomic.Int64, delay time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCount.Add(1)
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		resp := authResponseEnvelope{}
		resp.Data.Token = "tok-valid"
		resp.Data.ExpiresAt = time.Now().Add(10 * time.Minute)
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestGetToken_CachesValidToken(t *testing.T) {
	var count atomic.Int64
	srv := newTestServer(t, &count, 0)
	defer srv.Close()

	tm := newTokenManager(srv.URL, "agent", "key", srv.Client())
	ctx := context.Background()

	tok1, err := tm.getToken(ctx)
	require.NoError(t, err, "first getToken")
	assert.Equal(t, "tok-valid", tok1)

	tok2, err := tm.getToken(ctx)
	require.NoError(t, err, "second getToken")
	assert.Equal(t, "tok-valid", tok2)

	assert.Equal(t, int64(1), count.Load(), "server should be hit exactly once (token should be cached)")
}

func TestGetToken_ConcurrentCallersShareOneRefresh(t *testing.T) {
	var count atomic.Int64
	// Simulate a slow token endpoint so concurrent callers overlap.
	srv := newTestServer(t, &count, 100*time.Millisecond)
	defer srv.Close()

	tm := newTokenManager(srv.URL, "agent", "key", srv.Client())

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make([]error, goroutines)
	tokens := make([]string, goroutines)

	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			tokens[idx], errs[idx] = tm.getToken(context.Background())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		assert.NoError(t, err, "goroutine %d", i)
		assert.Equal(t, "tok-valid", tokens[i], "goroutine %d", i)
	}

	assert.Equal(t, int64(1), count.Load(), "all concurrent callers should share one refresh")
}

func TestGetToken_CallerContextCancellation(t *testing.T) {
	var count atomic.Int64
	// Very slow server -- caller should be able to bail out.
	srv := newTestServer(t, &count, 5*time.Second)
	defer srv.Close()

	tm := newTokenManager(srv.URL, "agent", "key", srv.Client())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := tm.getToken(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestGetToken_RefreshErrorDoesNotPoison(t *testing.T) {
	var callNum atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callNum.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := authResponseEnvelope{}
		resp.Data.Token = "tok-retry"
		resp.Data.ExpiresAt = time.Now().Add(10 * time.Minute)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	tm := newTokenManager(srv.URL, "agent", "key", srv.Client())
	ctx := context.Background()

	// First call fails.
	_, err := tm.getToken(ctx)
	require.Error(t, err)

	// Second call should retry and succeed -- the error must not leave the
	// tokenManager in a permanently broken state.
	tok, err := tm.getToken(ctx)
	require.NoError(t, err, "second getToken")
	assert.Equal(t, "tok-retry", tok)
}

func TestGetToken_ExpiredTokenTriggersRefresh(t *testing.T) {
	var count atomic.Int64
	srv := newTestServer(t, &count, 0)
	defer srv.Close()

	tm := newTokenManager(srv.URL, "agent", "key", srv.Client())
	ctx := context.Background()

	// Prime the cache.
	_, err := tm.getToken(ctx)
	require.NoError(t, err, "first getToken")
	require.Equal(t, int64(1), count.Load(), "server hit count after first call")

	// Force expiration by backdating expiresAt.
	tm.mu.Lock()
	tm.expiresAt = time.Now().Add(-1 * time.Minute)
	tm.mu.Unlock()

	tok, err := tm.getToken(ctx)
	require.NoError(t, err, "second getToken")
	assert.Equal(t, "tok-valid", tok)
	assert.Equal(t, int64(2), count.Load(), "expired token should trigger second refresh")
}
