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
// Source of truth. Never mutated or deleted.
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

// AgentRunStartedPayload is the payload for AgentRunStarted events.
type AgentRunStartedPayload struct {
	AgentID      string         `json:"agent_id"`
	AgentVersion string         `json:"agent_version,omitempty"`
	Trigger      string         `json:"trigger,omitempty"`
	InputSummary string         `json:"input_summary,omitempty"`
	Config       map[string]any `json:"config,omitempty"`
}

// AgentRunCompletedPayload is the payload for AgentRunCompleted events.
type AgentRunCompletedPayload struct {
	OutputSummary string         `json:"output_summary,omitempty"`
	DurationMs    int64          `json:"duration_ms,omitempty"`
	TokenUsage    map[string]int `json:"token_usage,omitempty"`
}

// AgentRunFailedPayload is the payload for AgentRunFailed events.
type AgentRunFailedPayload struct {
	ErrorType     string `json:"error_type"`
	ErrorMessage  string `json:"error_message"`
	PartialOutput string `json:"partial_output,omitempty"`
	Retryable     bool   `json:"retryable"`
}

// DecisionStartedPayload is the payload for DecisionStarted events.
type DecisionStartedPayload struct {
	DecisionType   string `json:"decision_type"`
	ContextSummary string `json:"context_summary,omitempty"`
}

// AlternativeConsideredPayload is the payload for AlternativeConsidered events.
type AlternativeConsideredPayload struct {
	DecisionID         uuid.UUID      `json:"decision_id"`
	Label              string         `json:"label"`
	Score              float32        `json:"score"`
	EvaluationCriteria map[string]any `json:"evaluation_criteria,omitempty"`
}

// EvidenceGatheredPayload is the payload for EvidenceGathered events.
type EvidenceGatheredPayload struct {
	DecisionID     uuid.UUID `json:"decision_id"`
	SourceType     string    `json:"source_type"`
	SourceURI      string    `json:"source_uri,omitempty"`
	ContentSummary string    `json:"content_summary"`
	RelevanceScore float32   `json:"relevance_score,omitempty"`
}

// ReasoningStepCompletedPayload is the payload for ReasoningStepCompleted events.
type ReasoningStepCompletedPayload struct {
	DecisionID  uuid.UUID `json:"decision_id"`
	StepNumber  int       `json:"step_number"`
	Description string    `json:"description"`
	Conclusion  string    `json:"conclusion"`
}

// DecisionMadePayload is the payload for DecisionMade events.
type DecisionMadePayload struct {
	DecisionID       uuid.UUID `json:"decision_id"`
	Outcome          string    `json:"outcome"`
	Confidence       float32   `json:"confidence"`
	ReasoningSummary string    `json:"reasoning_summary,omitempty"`
}

// DecisionRevisedPayload is the payload for DecisionRevised events.
type DecisionRevisedPayload struct {
	OriginalDecisionID uuid.UUID `json:"original_decision_id"`
	RevisedDecisionID  uuid.UUID `json:"revised_decision_id"`
	RevisionReason     string    `json:"revision_reason"`
	PreviousOutcome    string    `json:"previous_outcome"`
	NewOutcome         string    `json:"new_outcome"`
	NewConfidence      float32   `json:"new_confidence"`
}

// ToolCallStartedPayload is the payload for ToolCallStarted events.
type ToolCallStartedPayload struct {
	ToolName string         `json:"tool_name"`
	Input    map[string]any `json:"input,omitempty"`
}

// ToolCallCompletedPayload is the payload for ToolCallCompleted events.
type ToolCallCompletedPayload struct {
	ToolName   string         `json:"tool_name"`
	Output     map[string]any `json:"output,omitempty"`
	DurationMs int64          `json:"duration_ms,omitempty"`
	Error      string         `json:"error,omitempty"`
}

// AgentHandoffPayload is the payload for AgentHandoff events.
type AgentHandoffPayload struct {
	FromAgentID     string         `json:"from_agent_id"`
	ToAgentID       string         `json:"to_agent_id"`
	HandoffReason   string         `json:"handoff_reason"`
	ContextSnapshot map[string]any `json:"context_snapshot,omitempty"`
	Priority        string         `json:"priority,omitempty"`
}

// ConsensusRequestedPayload is the payload for ConsensusRequested events.
type ConsensusRequestedPayload struct {
	Topic               string    `json:"topic"`
	RequestingAgentID   string    `json:"requesting_agent_id"`
	ParticipantAgentIDs []string  `json:"participant_agent_ids"`
	Deadline            time.Time `json:"deadline,omitempty"`
}

// ConflictDetectedPayload is the payload for ConflictDetected events.
type ConflictDetectedPayload struct {
	Topic                string            `json:"topic"`
	ConflictingDecisions []ConflictingPair `json:"conflicting_decisions"`
	ConflictType         string            `json:"conflict_type"`
	Severity             string            `json:"severity"`
}

// ConflictingPair represents one side of a decision conflict.
type ConflictingPair struct {
	AgentID    string    `json:"agent_id"`
	DecisionID uuid.UUID `json:"decision_id"`
	Outcome    string    `json:"outcome"`
}
