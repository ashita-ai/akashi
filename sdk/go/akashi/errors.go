// Package akashi provides a Go client for the Akashi decision-tracing API.
package akashi

import (
	"errors"
	"fmt"
)

// Error represents an error from the Akashi API with the HTTP status code
// and the server's error message.
type Error struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("akashi: %s (%d): %s", e.Code, e.StatusCode, e.Message)
}

// IsNotFound returns true if the error is a 404.
func IsNotFound(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.StatusCode == 404
	}
	return false
}

// IsUnauthorized returns true if the error is a 401.
func IsUnauthorized(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.StatusCode == 401
	}
	return false
}

// IsForbidden returns true if the error is a 403.
func IsForbidden(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.StatusCode == 403
	}
	return false
}

// IsRateLimited returns true if the error is a 429 (Too Many Requests).
func IsRateLimited(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.StatusCode == 429
	}
	return false
}

// IsConflict returns true if the error is a 409.
func IsConflict(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.StatusCode == 409
	}
	return false
}
