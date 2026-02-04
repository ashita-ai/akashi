package model

import (
	"time"

	"github.com/google/uuid"
)

// AgentRole represents the RBAC role assigned to an agent.
type AgentRole string

const (
	RoleAdmin  AgentRole = "admin"
	RoleAgent  AgentRole = "agent"
	RoleReader AgentRole = "reader"
)

// Agent represents an agent identity with role assignment.
type Agent struct {
	ID         uuid.UUID      `json:"id"`
	AgentID    string         `json:"agent_id"`
	Name       string         `json:"name"`
	Role       AgentRole      `json:"role"`
	APIKeyHash *string        `json:"-"`
	Metadata   map[string]any `json:"metadata"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

// AccessGrant represents a fine-grained access grant between agents.
type AccessGrant struct {
	ID           uuid.UUID  `json:"id"`
	GrantorID    uuid.UUID  `json:"grantor_id"`
	GranteeID    uuid.UUID  `json:"grantee_id"`
	ResourceType string     `json:"resource_type"`
	ResourceID   *string    `json:"resource_id,omitempty"`
	Permission   string     `json:"permission"`
	GrantedAt    time.Time  `json:"granted_at"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

// Permission enumerates valid grant permissions.
type Permission string

const (
	PermissionRead  Permission = "read"
	PermissionWrite Permission = "write"
)

// ResourceType enumerates valid grant resource types.
type ResourceType string

const (
	ResourceAgentTraces ResourceType = "agent_traces"
	ResourceDecision    ResourceType = "decision"
	ResourceRun         ResourceType = "run"
)
