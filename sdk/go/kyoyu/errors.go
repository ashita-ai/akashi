// Package kyoyu provides a Go client for the Kyoyu decision-tracing API.
package kyoyu

import "fmt"

// Error represents an error from the Kyoyu API with the HTTP status code
// and the server's error message.
type Error struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("kyoyu: %s (%d): %s", e.Code, e.StatusCode, e.Message)
}

// IsNotFound returns true if the error is a 404.
func IsNotFound(err error) bool {
	if e, ok := err.(*Error); ok {
		return e.StatusCode == 404
	}
	return false
}

// IsUnauthorized returns true if the error is a 401.
func IsUnauthorized(err error) bool {
	if e, ok := err.(*Error); ok {
		return e.StatusCode == 401
	}
	return false
}

// IsForbidden returns true if the error is a 403.
func IsForbidden(err error) bool {
	if e, ok := err.(*Error); ok {
		return e.StatusCode == 403
	}
	return false
}
