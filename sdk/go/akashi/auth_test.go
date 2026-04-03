package akashi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	if err != nil {
		t.Fatalf("first getToken: %v", err)
	}
	if tok1 != "tok-valid" {
		t.Fatalf("first token = %q, want %q", tok1, "tok-valid")
	}

	tok2, err := tm.getToken(ctx)
	if err != nil {
		t.Fatalf("second getToken: %v", err)
	}
	if tok2 != "tok-valid" {
		t.Fatalf("second token = %q, want %q", tok2, "tok-valid")
	}

	if n := count.Load(); n != 1 {
		t.Fatalf("server hit %d times, want 1 (token should be cached)", n)
	}
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
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
		if tokens[i] != "tok-valid" {
			t.Errorf("goroutine %d: token = %q, want %q", i, tokens[i], "tok-valid")
		}
	}

	if n := count.Load(); n != 1 {
		t.Fatalf("server hit %d times, want 1 (all concurrent callers should share one refresh)", n)
	}
}

func TestGetToken_CallerContextCancellation(t *testing.T) {
	var count atomic.Int64
	// Very slow server — caller should be able to bail out.
	srv := newTestServer(t, &count, 5*time.Second)
	defer srv.Close()

	tm := newTokenManager(srv.URL, "agent", "key", srv.Client())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := tm.getToken(ctx)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got: %v", err)
	}
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
	if err == nil {
		t.Fatal("expected error on first call, got nil")
	}

	// Second call should retry and succeed — the error must not leave the
	// tokenManager in a permanently broken state.
	tok, err := tm.getToken(ctx)
	if err != nil {
		t.Fatalf("second getToken: %v", err)
	}
	if tok != "tok-retry" {
		t.Fatalf("token = %q, want %q", tok, "tok-retry")
	}
}

func TestGetToken_ExpiredTokenTriggersRefresh(t *testing.T) {
	var count atomic.Int64
	srv := newTestServer(t, &count, 0)
	defer srv.Close()

	tm := newTokenManager(srv.URL, "agent", "key", srv.Client())
	ctx := context.Background()

	// Prime the cache.
	if _, err := tm.getToken(ctx); err != nil {
		t.Fatalf("first getToken: %v", err)
	}
	if n := count.Load(); n != 1 {
		t.Fatalf("server hit %d times after first call, want 1", n)
	}

	// Force expiration by backdating expiresAt.
	tm.mu.Lock()
	tm.expiresAt = time.Now().Add(-1 * time.Minute)
	tm.mu.Unlock()

	tok, err := tm.getToken(ctx)
	if err != nil {
		t.Fatalf("second getToken: %v", err)
	}
	if tok != "tok-valid" {
		t.Fatalf("token = %q, want %q", tok, "tok-valid")
	}
	if n := count.Load(); n != 2 {
		t.Fatalf("server hit %d times, want 2 (expired token should trigger second refresh)", n)
	}
}
