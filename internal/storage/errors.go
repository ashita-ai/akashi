package storage

import "errors"

// ErrNotFound is returned when a requested entity does not exist.
var ErrNotFound = errors.New("storage: not found")

// ErrAlreadyErased is returned when attempting to erase an already-erased decision.
var ErrAlreadyErased = errors.New("storage: already erased")
