package storage

import "errors"

// ErrNotFound is returned when a requested entity does not exist.
var ErrNotFound = errors.New("storage: not found")

// ErrQuotaExceeded is returned when a transactional quota check determines
// that the org has exceeded its decision limit for the current billing period.
var ErrQuotaExceeded = errors.New("storage: quota exceeded")
