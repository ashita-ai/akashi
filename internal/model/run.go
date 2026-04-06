// Package model defines the core domain types for Akashi.
//
// All types correspond directly to database tables and event payloads
// defined in SPEC-002. Types use strong typing (UUIDs, time.Time, enums)
// and avoid interface{} wherever possible.
package model

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// RunStatus represents the lifecycle state of an agent run.
type RunStatus string

const (
	RunStatusRunning   RunStatus = "running"
	RunStatusCompleted RunStatus = "completed"
	RunStatusFailed    RunStatus = "failed"
)

// IsTerminal reports whether the status represents an end state
// from which no further transitions are allowed.
func (s RunStatus) IsTerminal() bool {
	return s == RunStatusCompleted || s == RunStatusFailed
}

// ValidateTransition checks whether moving from the current status to next
// is a legal state transition. The valid state machine is:
//
//	running → completed
//	running → failed
//
// Terminal states (completed, failed) reject all further transitions.
func (s RunStatus) ValidateTransition(next RunStatus) error {
	if s == RunStatusRunning && (next == RunStatusCompleted || next == RunStatusFailed) {
		return nil
	}
	return fmt.Errorf("invalid run status transition: %q → %q", s, next)
}

// AgentRun is the top-level execution context for an agent.
// Corresponds to an OTEL trace. Immutable once created.
type AgentRun struct {
	ID          uuid.UUID      `json:"id"`
	AgentID     string         `json:"agent_id"`
	OrgID       uuid.UUID      `json:"org_id"`
	TraceID     *string        `json:"trace_id,omitempty"`
	ParentRunID *uuid.UUID     `json:"parent_run_id,omitempty"`
	Status      RunStatus      `json:"status"`
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	Metadata    map[string]any `json:"metadata"`
	CreatedAt   time.Time      `json:"created_at"`
}
