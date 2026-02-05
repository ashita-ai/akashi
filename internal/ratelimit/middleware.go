package ratelimit

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// KeyFunc extracts the rate limit key from a request.
// Returns empty string to skip rate limiting for this request (e.g., admin).
type KeyFunc func(r *http.Request) string

// Middleware returns HTTP middleware that enforces a rate limit.
// keyFunc determines the identifier to rate limit by.
// If the limiter is in noop mode (nil Redis), all requests pass through.
func Middleware(limiter *Limiter, rule Rule, keyFunc KeyFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := keyFunc(r)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}

			result := limiter.Allow(r.Context(), rule, key)

			// Always set rate limit headers.
			for k, v := range result.FormatHeaders() {
				w.Header().Set(k, v)
			}

			if !result.Allowed {
				retryAfter := time.Until(result.ResetAt).Seconds()
				if retryAfter < 1 {
					retryAfter = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter)))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded","message":"too many requests"}}`))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// IPKeyFunc extracts the client IP from the request for rate limiting.
func IPKeyFunc(r *http.Request) string {
	// Prefer X-Forwarded-For if behind a reverse proxy.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.IndexByte(xff, ','); idx != -1 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	// Fall back to RemoteAddr (strip port).
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		return addr[:idx]
	}
	return addr
}
