package ratelimit

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ashita-ai/akashi/internal/model"
)

// KeyFunc extracts the rate limit key from a request.
// Returns empty string to skip rate limiting for this request (e.g., admin).
type KeyFunc func(r *http.Request) string

// RequestIDFunc extracts the request ID from the request context.
// Injected by the caller to avoid a dependency on the server package.
type RequestIDFunc func(r *http.Request) string

// Middleware returns HTTP middleware that enforces a rate limit.
// keyFunc determines the identifier to rate limit by.
// If the limiter is in noop mode (nil Redis), all requests pass through.
func Middleware(limiter *Limiter, rule Rule, keyFunc KeyFunc) func(http.Handler) http.Handler {
	return MiddlewareWithRequestID(limiter, rule, keyFunc, nil)
}

// MiddlewareWithRequestID is like Middleware but includes the request ID in the
// rate-limit error response, matching the standard API error envelope.
func MiddlewareWithRequestID(limiter *Limiter, rule Rule, keyFunc KeyFunc, reqIDFunc RequestIDFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limiter == nil {
				next.ServeHTTP(w, r)
				return
			}

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

				var requestID string
				if reqIDFunc != nil {
					requestID = reqIDFunc(r)
				}
				writeRateLimitError(w, requestID)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// writeRateLimitError writes a rate-limit error using the standard API error envelope.
func writeRateLimitError(w http.ResponseWriter, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(model.APIError{
		Error: model.ErrorDetail{
			Code:    model.ErrCodeRateLimited,
			Message: "too many requests",
		},
		Meta: model.ResponseMeta{
			RequestID: requestID,
			Timestamp: time.Now().UTC(),
		},
	})
}

// IPKeyFunc extracts the client IP from the request for rate limiting.
// Uses RemoteAddr only. X-Forwarded-For is not trusted because the server
// may not be behind a reverse proxy that sanitizes the header, and any
// client can set an arbitrary value to bypass rate limiting.
// If deployed behind a trusted proxy, configure the proxy to set RemoteAddr
// (e.g., nginx realip module, Cloudflare Authenticated Origin Pulls).
func IPKeyFunc(r *http.Request) string {
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		return addr[:idx]
	}
	return addr
}
