package storage

import "errors"

// ErrNotFound is returned when a requested entity does not exist.
var ErrNotFound = errors.New("storage: not found")

// ErrAlreadyErased is returned when attempting to erase an already-erased decision.
var ErrAlreadyErased = errors.New("storage: already erased")

// ErrWinningAgentNotInGroup is returned when the winning agent does not match
// either agent_a or agent_b in the target conflict group.
var ErrWinningAgentNotInGroup = errors.New("storage: winning agent is not a participant in this conflict group")

// ErrWinningDecisionNotInConflict is returned when the winning_decision_id
// does not match either decision_a_id or decision_b_id of the conflict.
var ErrWinningDecisionNotInConflict = errors.New("storage: winning decision is not a participant in this conflict")

// ErrRevisedDecisions is returned when a resolution requiring a decisions JOIN
// finds no current (valid_to IS NULL) decisions, typically because the referenced
// decisions have been superseded by revisions.
var ErrRevisedDecisions = errors.New("storage: referenced decisions have been revised")

// ComputeFPLabel returns the ground truth label for a false_positive resolution.
// Returns nil when status is not "false_positive". When rawLabel is
// "related_not_contradicting" it is used; otherwise defaults to
// "unrelated_false_positive". rawLabel may be nil.
func ComputeFPLabel(status string, rawLabel *string) *string {
	if status != "false_positive" {
		return nil
	}
	label := "unrelated_false_positive"
	if rawLabel != nil && *rawLabel == "related_not_contradicting" {
		label = *rawLabel
	}
	return &label
}
