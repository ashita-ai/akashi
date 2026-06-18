package storage_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/storage"
)

func ptr[T any](v T) *T { return &v }

func TestComputeFPLabel(t *testing.T) {
	relatedNotContradicting := "related_not_contradicting"
	garbage := "not_a_real_label"

	cases := []struct {
		name     string
		status   string
		rawLabel *string
		want     *string
	}{
		{"non-fp status yields no label", "resolved", &relatedNotContradicting, nil},
		{"open status yields no label", "open", nil, nil},
		{"fp with nil raw defaults to unrelated", "false_positive", nil, ptr("unrelated_false_positive")},
		{"fp with empty raw defaults to unrelated", "false_positive", ptr(""), ptr("unrelated_false_positive")},
		{"fp honors related_not_contradicting", "false_positive", &relatedNotContradicting, ptr("related_not_contradicting")},
		{"fp ignores unrecognized raw and defaults", "false_positive", &garbage, ptr("unrelated_false_positive")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := storage.ComputeFPLabel(tc.status, tc.rawLabel)
			assertLabelEqual(t, tc.want, got)
		})
	}
}

func TestComputeResolutionLabel(t *testing.T) {
	winner := ptr(uuid.New())
	relatedNotContradicting := "related_not_contradicting"

	cases := []struct {
		name      string
		status    string
		winningID *uuid.UUID
		rawFP     *string
		want      *string
	}{
		{"resolved with winner is genuine", "resolved", winner, nil, ptr("genuine")},
		{"resolved without winner has no label", "resolved", nil, nil, nil},
		{"false_positive falls through to FP label", "false_positive", nil, nil, ptr("unrelated_false_positive")},
		{"false_positive honors subtype even with no winner", "false_positive", nil, &relatedNotContradicting, ptr("related_not_contradicting")},
		{"open has no label", "open", nil, nil, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := storage.ComputeResolutionLabel(tc.status, tc.winningID, tc.rawFP)
			assertLabelEqual(t, tc.want, got)
		})
	}
}

func TestComputeGroupResolutionLabel(t *testing.T) {
	winningAgent := ptr("agent-a")
	relatedNotContradicting := "related_not_contradicting"

	cases := []struct {
		name         string
		status       string
		winningAgent *string
		rawFP        *string
		want         *string
	}{
		{"resolved with winning agent is genuine", "resolved", winningAgent, nil, ptr("genuine")},
		{"resolved without winning agent has no label", "resolved", nil, nil, nil},
		{"false_positive falls through to FP label", "false_positive", nil, nil, ptr("unrelated_false_positive")},
		{"false_positive honors subtype", "false_positive", nil, &relatedNotContradicting, ptr("related_not_contradicting")},
		{"open has no label", "open", nil, nil, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := storage.ComputeGroupResolutionLabel(tc.status, tc.winningAgent, tc.rawFP)
			assertLabelEqual(t, tc.want, got)
		})
	}
}

func assertLabelEqual(t *testing.T, want, got *string) {
	t.Helper()
	if want == nil {
		assert.Nil(t, got)
		return
	}
	if assert.NotNil(t, got) {
		assert.Equal(t, *want, *got)
	}
}
