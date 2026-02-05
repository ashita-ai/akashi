// Package model defines the core domain types for Akashi.
//
// All types correspond directly to database tables and event payloads
// defined in SPEC-002. Types use strong typing (UUIDs, time.Time, enums)
// and avoid interface{} wherever possible.
package model

import (
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

// AgentRun is the top-level execution context for an agent.
// Corresponds to an OTEL trace. Immutable once created.
type AgentRun struct {
	ID          uuid.UUID      `json:"id"`
	AgentID     string         `json:"agent_id"`
	TraceID     *string        `json:"trace_id,omitempty"`
	ParentRunID *uuid.UUID     `json:"parent_run_id,omitempty"`
	Status      RunStatus      `json:"status"`
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	Metadata    map[string]any `json:"metadata"`
	CreatedAt   time.Time      `json:"created_at"`
}
