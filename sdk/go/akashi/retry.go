package akashi

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"strconv"
	"time"
)

const (
	defaultMaxRetries     = 3
	defaultRetryBaseDelay = 500 * time.Millisecond
	maxRetryDelay         = 30 * time.Second
)

// isRetryableStatus returns true for HTTP status codes that should trigger a retry.
// 429 (rate limited) and 5xx (server errors) are retryable; client errors (4xx) are not.
func isRetryableStatus(code int) bool {
	return code == 429 || code >= 500
}

// retrySleep blocks until the computed backoff delay elapses or the context
// is cancelled. The delay uses exponential backoff with ±25% jitter, capped
// at maxRetryDelay. If retryAfter is positive (from a Retry-After header),
// it takes precedence when it exceeds the calculated delay.
func retrySleep(ctx context.Context, attempt int, baseDelay time.Duration, retryAfter time.Duration) error {
	// Cap shift to avoid overflow; anything beyond 62 would exceed maxRetryDelay anyway.
	shift := attempt
	if shift < 0 {
		shift = 0
	}
	if shift > 62 {
		shift = 62
	}
	delay := baseDelay << uint(shift) //nolint:gosec // shift is bounds-checked above
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}

	// ±25% jitter to prevent thundering herd.
	quarter := int64(delay) / 4
	if quarter > 0 {
		jitter := cryptoInt63n(2*quarter) - quarter
		delay += time.Duration(jitter)
	}

	// Honour Retry-After when it's longer than our calculated delay.
	if retryAfter > 0 && retryAfter > delay {
		delay = retryAfter
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

// parseRetryAfter extracts a duration from a Retry-After header value.
// Supports integer seconds only (RFC 7231 §7.1.3). Returns 0 on failure.
func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	secs, err := strconv.Atoi(header)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// cryptoInt63n returns a cryptographically random int64 in [0, n).
func cryptoInt63n(n int64) int64 {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	// Mask the high bit to guarantee a non-negative int64, avoiding
	// a uint64→int64 conversion that gosec flags as potential overflow.
	raw := binary.LittleEndian.Uint64(buf[:])
	v := int64(raw & 0x7FFFFFFFFFFFFFFF) //nolint:gosec // high bit cleared, value fits int64
	return v % n
}
