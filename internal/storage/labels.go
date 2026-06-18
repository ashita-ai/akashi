package storage

import "github.com/google/uuid"

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

// ComputeResolutionLabel returns the ground truth label implied by a deliberate
// conflict adjudication. False positives preserve their explicit FP subtype; a
// resolved conflict only becomes training data when a winner is declared.
//
// Only deliberate adjudications (an actor resolving through the API or MCP) may
// use this. Automated paths — autoresolve (policy/timeout) and cascade
// (embedding similarity) — must pass a nil label, because counting the system's
// own resolutions as ground truth biases the scorer precision metric upward.
func ComputeResolutionLabel(status string, winningDecisionID *uuid.UUID, rawFPLabel *string) *string {
	if status == "resolved" && winningDecisionID != nil {
		label := "genuine"
		return &label
	}
	return ComputeFPLabel(status, rawFPLabel)
}

// ComputeGroupResolutionLabel is the group-resolution equivalent of
// ComputeResolutionLabel. A winning agent means each updated conflict gets a
// concrete winning_decision_id during the storage transaction.
func ComputeGroupResolutionLabel(status string, winningAgent *string, rawFPLabel *string) *string {
	if status == "resolved" && winningAgent != nil {
		label := "genuine"
		return &label
	}
	return ComputeFPLabel(status, rawFPLabel)
}
