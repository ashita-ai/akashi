package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/ashita-ai/akashi/internal/ratelimit"
)

func TestRateLimitMiddleware(t *testing.T) {
	// MemoryLimiter with rate=1 token/sec and burst=2 allows the first 2 rapid
	// requests (initial burst capacity) then rejects until tokens refill.
	limiter := ratelimit.NewMemoryLimiter(1, 2)
	defer func() { _ = limiter.Close() }()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := rateLimitMiddleware(limiter, logger, false, inner)

	// Simulate 3 rapid requests from the same IP. The rate limiter keys on
	// "ip:<remoteAddr>" for unauthenticated requests. First 2 consume the
	// burst tokens; the third is rejected with 429.
	for i := range 3 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/some-path", nil)
		req.RemoteAddr = "192.168.1.1:12345"
		handler.ServeHTTP(rec, req)

		if i < 2 {
			if rec.Code != http.StatusOK {
				t.Errorf("request %d: got status %d, want %d (within burst)", i+1, rec.Code, http.StatusOK)
			}
		} else {
			if rec.Code != http.StatusTooManyRequests {
				t.Errorf("request %d: got status %d, want %d (burst exhausted)", i+1, rec.Code, http.StatusTooManyRequests)
			}
			if rec.Header().Get("Retry-After") == "" {
				t.Error("rate-limited response should include Retry-After header")
			}
		}
	}
}

func TestRateLimitMiddleware_DifferentIPs(t *testing.T) {
	// Each IP gets its own bucket, so requests from different IPs should
	// not interfere with each other.
	limiter := ratelimit.NewMemoryLimiter(1, 1)
	defer func() { _ = limiter.Close() }()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := rateLimitMiddleware(limiter, logger, false, inner)

	// First request from IP A should succeed.
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest("GET", "/path", nil)
	req1.RemoteAddr = "10.0.0.1:1000"
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Errorf("IP A first request: got %d, want %d", rec1.Code, http.StatusOK)
	}

	// Second request from IP A should be rate-limited (burst=1).
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/path", nil)
	req2.RemoteAddr = "10.0.0.1:1000"
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("IP A second request: got %d, want %d", rec2.Code, http.StatusTooManyRequests)
	}

	// First request from IP B should still succeed (separate bucket).
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/path", nil)
	req3.RemoteAddr = "10.0.0.2:1000"
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Errorf("IP B first request: got %d, want %d", rec3.Code, http.StatusOK)
	}
}
