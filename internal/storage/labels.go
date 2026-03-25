package storage

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
