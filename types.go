package akashi

import (
	"time"

	"github.com/google/uuid"
)

// Role is an agent's RBAC role.
type Role string

const (
	RolePlatformAdmin Role = "platform_admin"
	RoleOrgOwner      Role = "org_owner"
	RoleAdmin         Role = "admin"
	RoleAgent         Role = "agent"
	RoleReader        Role = "reader"
)

// Decision is the public representation of a traced decision.
// It is a curated view of internal/model.Decision for use in extension interfaces.
// No internal package imports — safe to use from outside the module.
type Decision struct {
	ID           uuid.UUID
	OrgID        uuid.UUID
	AgentID      string
	DecisionType string
	Outcome      string
	Reasoning    *string
	Confidence   float32
	CreatedAt    time.Time
	PrecedentRef *uuid.UUID
	SessionID    *uuid.UUID
	// AgentContext holds composite agent identity fields (model, tool, repo, branch).
	AgentContext map[string]any
	Metadata     map[string]any
}

// Alternative is a rejected option recorded alongside a decision.
type Alternative struct {
	Label           string
	Score           *float32
	Selected        bool
	RejectionReason *string
}

// Evidence is a supporting fact for a decision.
type Evidence struct {
	SourceType     string
	SourceURI      *string
	Content        string
	RelevanceScore *float32
}

// Conflict represents a detected disagreement between two decisions.
type Conflict struct {
	ID           uuid.UUID
	OrgID        uuid.UUID
	DecisionAID  uuid.UUID
	DecisionBID  uuid.UUID
	AgentA       string
	AgentB       string
	DecisionType string
	// Score is the significance of the conflict [0.0, 1.0]. Higher means stronger conflict.
	Score       float32
	Explanation string
	Category    string // factual | assessment | strategic | temporal
	Severity    string // critical | high | medium | low
	Status      string // open | acknowledged | resolved | wont_fix
	DetectedAt  time.Time
}

// ConflictScore is the result of a pairwise conflict scoring call.
type ConflictScore struct {
	// Score is the conflict intensity [0.0 = no conflict, 1.0 = direct contradiction].
	Score       float32
	Explanation string
}

// Violation is a policy rule violation returned by PolicyEvaluator.
// Defined here for interface completeness; the policy engine is a future enterprise feature.
type Violation struct {
	Rule     string
	Severity string
	Message  string
}

// SearchFilters mirrors model.QueryFilters for use in the public Searcher interface.
// All fields are primitive types or stdlib types — no internal package imports.
type SearchFilters struct {
	AgentIDs      []string
	DecisionType  *string
	ConfidenceMin *float32
	SessionID     *uuid.UUID
	Tool          *string
	Model         *string
	Project       *string
}

// SearchResult holds a decision ID and similarity score from a Searcher.
type SearchResult struct {
	DecisionID uuid.UUID
	Score      float32
}
