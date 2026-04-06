package model

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunStatus_IsTerminal(t *testing.T) {
	tests := []struct {
		status   RunStatus
		terminal bool
	}{
		{RunStatusRunning, false},
		{RunStatusCompleted, true},
		{RunStatusFailed, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			assert.Equal(t, tt.terminal, tt.status.IsTerminal())
		})
	}
}

func TestRunStatus_ValidateTransition(t *testing.T) {
	t.Run("valid transitions", func(t *testing.T) {
		require.NoError(t, RunStatusRunning.ValidateTransition(RunStatusCompleted),
			"running → completed should be allowed")
		require.NoError(t, RunStatusRunning.ValidateTransition(RunStatusFailed),
			"running → failed should be allowed")
	})

	t.Run("terminal states reject all transitions", func(t *testing.T) {
		terminals := []RunStatus{RunStatusCompleted, RunStatusFailed}
		targets := []RunStatus{RunStatusRunning, RunStatusCompleted, RunStatusFailed}

		for _, from := range terminals {
			for _, to := range targets {
				err := from.ValidateTransition(to)
				assert.Errorf(t, err, "%s → %s should be rejected (terminal state)", from, to)
				assert.Contains(t, err.Error(), "invalid run status transition")
			}
		}
	})

	t.Run("running to running is invalid", func(t *testing.T) {
		err := RunStatusRunning.ValidateTransition(RunStatusRunning)
		assert.Error(t, err, "running → running is a no-op and should be rejected")
	})

	t.Run("unknown status values are rejected", func(t *testing.T) {
		err := RunStatus("unknown").ValidateTransition(RunStatusCompleted)
		assert.Error(t, err, "unknown source status should be rejected")

		err = RunStatusRunning.ValidateTransition(RunStatus("cancelled"))
		assert.Error(t, err, "unknown target status should be rejected")
	})
}

func TestAgentRun_MetadataPreservedAcrossTransitions(t *testing.T) {
	// Verifies that building a run, then simulating a transition,
	// does not lose metadata. This tests the domain model's integrity
	// rather than the storage layer.
	meta := map[string]any{
		"model":       "gpt-4",
		"temperature": 0.7,
		"tags":        []string{"prod", "v2"},
	}

	run := AgentRun{
		ID:        uuid.New(),
		AgentID:   "test-agent",
		OrgID:     uuid.New(),
		Status:    RunStatusRunning,
		StartedAt: time.Now().UTC(),
		Metadata:  meta,
		CreatedAt: time.Now().UTC(),
	}

	// Simulate completing the run (as the storage layer would).
	require.NoError(t, run.Status.ValidateTransition(RunStatusCompleted))
	now := time.Now().UTC()
	run.Status = RunStatusCompleted
	run.CompletedAt = &now

	assert.Equal(t, RunStatusCompleted, run.Status)
	assert.NotNil(t, run.CompletedAt)
	assert.Equal(t, "gpt-4", run.Metadata["model"])
	assert.Equal(t, 0.7, run.Metadata["temperature"])
	assert.Equal(t, []string{"prod", "v2"}, run.Metadata["tags"])
}

func TestAgentRun_OptionalFieldsNilByDefault(t *testing.T) {
	run := AgentRun{
		ID:        uuid.New(),
		AgentID:   "agent-1",
		OrgID:     uuid.New(),
		Status:    RunStatusRunning,
		StartedAt: time.Now().UTC(),
		CreatedAt: time.Now().UTC(),
	}

	assert.Nil(t, run.TraceID, "TraceID should be nil when not set")
	assert.Nil(t, run.ParentRunID, "ParentRunID should be nil when not set")
	assert.Nil(t, run.CompletedAt, "CompletedAt should be nil for a running run")
	assert.Nil(t, run.Metadata, "Metadata should be nil when not set")
}
