// Package model defines the core domain types for Kyoyu.
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
	ID           uuid.UUID         `json:"id"`
	AgentID      string            `json:"agent_id"`
	TraceID      *string           `json:"trace_id,omitempty"`
	ParentRunID  *uuid.UUID        `json:"parent_run_id,omitempty"`
	Status       RunStatus         `json:"status"`
	StartedAt    time.Time         `json:"started_at"`
	CompletedAt  *time.Time        `json:"completed_at,omitempty"`
	Metadata     map[string]any    `json:"metadata"`
	CreatedAt    time.Time         `json:"created_at"`
}

// RunParam is an immutable key-value pair set at run start.
type RunParam struct {
	ID    uuid.UUID `json:"id"`
	RunID uuid.UUID `json:"run_id"`
	Key   string    `json:"key"`
	Value string    `json:"value"`
}

// RunMetric is an append-only numeric measurement.
type RunMetric struct {
	ID         uuid.UUID `json:"id"`
	RunID      uuid.UUID `json:"run_id"`
	Key        string    `json:"key"`
	Value      float64   `json:"value"`
	Step       int64     `json:"step"`
	RecordedAt time.Time `json:"recorded_at"`
}

// RunTag is a mutable tag for categorization (upsert semantics).
type RunTag struct {
	ID    uuid.UUID `json:"id"`
	RunID uuid.UUID `json:"run_id"`
	Key   string    `json:"key"`
	Value string    `json:"value"`
}
