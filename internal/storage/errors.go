package storage

import (
	"errors"
	"fmt"
)

// ErrNotFound is returned when a requested entity does not exist.
var ErrNotFound = errors.New("storage: not found")

// ErrAgentNotFound is returned when an agent doesn't exist. It wraps ErrNotFound
// so callers can use errors.Is(err, ErrNotFound) generically.
//
// Defined here (a shared, untagged file) rather than in the !lite delete.go
// because the shared decisions service (internal/service/decisions, compiled
// into the lite binary) references it; a definition in a !lite file would leave
// the lite build undefined.
var ErrAgentNotFound = fmt.Errorf("storage: agent: %w", ErrNotFound)

// ErrAlreadyErased is returned when attempting to erase an already-erased decision.
var ErrAlreadyErased = errors.New("storage: already erased")

// ErrWinningAgentNotInGroup is returned when the winning agent does not match
// either agent_a or agent_b in the target conflict group.
var ErrWinningAgentNotInGroup = errors.New("storage: winning agent is not a participant in this conflict group")

// ErrWinningDecisionNotInConflict is returned when the winning_decision_id
// does not match either decision_a_id or decision_b_id of the conflict.
var ErrWinningDecisionNotInConflict = errors.New("storage: winning decision is not a participant in this conflict")

// ErrConflictOpen is returned when an operation requires a terminal conflict
// status but the target conflict is still open.
var ErrConflictOpen = errors.New("storage: conflict is still open")

// ErrSupersededDecisionNotInConflict is returned when adjudication tries to
// supersede a decision that is not one of the target conflict's two sides.
var ErrSupersededDecisionNotInConflict = errors.New("storage: superseded decision is not a participant in this conflict")

// ErrRevisedDecisions is returned when a resolution requiring a decisions JOIN
// finds no current (valid_to IS NULL) decisions, typically because the referenced
// decisions have been superseded by revisions.
var ErrRevisedDecisions = errors.New("storage: referenced decisions have been revised")
