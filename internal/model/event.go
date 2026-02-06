package model

import (
	"time"

	"github.com/google/uuid"
)

// EventType represents the category of an agent event.
type EventType string

const (
	// Run lifecycle events.
	EventAgentRunStarted   EventType = "AgentRunStarted"
	EventAgentRunCompleted EventType = "AgentRunCompleted"
	EventAgentRunFailed    EventType = "AgentRunFailed"

	// Decision events.
	EventDecisionStarted        EventType = "DecisionStarted"
	EventAlternativeConsidered  EventType = "AlternativeConsidered"
	EventEvidenceGathered       EventType = "EvidenceGathered"
	EventReasoningStepCompleted EventType = "ReasoningStepCompleted"
	EventDecisionMade           EventType = "DecisionMade"
	EventDecisionRevised        EventType = "DecisionRevised"

	// Tool events.
	EventToolCallStarted   EventType = "ToolCallStarted"
	EventToolCallCompleted EventType = "ToolCallCompleted"

	// Coordination events.
	EventAgentHandoff       EventType = "AgentHandoff"
	EventConsensusRequested EventType = "ConsensusRequested"
	EventConflictDetected   EventType = "ConflictDetected"
)

// AgentEvent is an append-only event in the event log.
// Source of truth for the black box recorder. Never mutated or deleted.
type AgentEvent struct {
	ID          uuid.UUID      `json:"id"`
	RunID       uuid.UUID      `json:"run_id"`
	EventType   EventType      `json:"event_type"`
	SequenceNum int64          `json:"sequence_num"`
	OccurredAt  time.Time      `json:"occurred_at"`
	AgentID     string         `json:"agent_id"`
	Payload     map[string]any `json:"payload"`
	CreatedAt   time.Time      `json:"created_at"`
}
