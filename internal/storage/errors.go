package storage

import "errors"

// ErrNotFound is returned when a requested entity does not exist.
var ErrNotFound = errors.New("storage: not found")

// ErrAlreadyErased is returned when attempting to erase an already-erased decision.
var ErrAlreadyErased = errors.New("storage: already erased")

// ErrWinningAgentNotInGroup is returned when the winning agent does not match
// either agent_a or agent_b in the target conflict group.
var ErrWinningAgentNotInGroup = errors.New("storage: winning agent is not a participant in this conflict group")
