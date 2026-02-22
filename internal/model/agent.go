package model

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// AgentRole represents the RBAC role assigned to an agent.
type AgentRole string

const (
	RolePlatformAdmin AgentRole = "platform_admin"
	RoleOrgOwner      AgentRole = "org_owner"
	RoleAdmin         AgentRole = "admin"
	RoleAgent         AgentRole = "agent"
	RoleReader        AgentRole = "reader"
)

// Agent represents an agent identity with role assignment.
type Agent struct {
	ID         uuid.UUID      `json:"id"`
	AgentID    string         `json:"agent_id"`
	OrgID      uuid.UUID      `json:"org_id"`
	Name       string         `json:"name"`
	Role       AgentRole      `json:"role"`
	APIKeyHash *string        `json:"-"`
	Tags       []string       `json:"tags"`
	Metadata   map[string]any `json:"metadata"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	LastSeen   *time.Time     `json:"last_seen"`
}

// AccessGrant represents a fine-grained access grant between agents.
type AccessGrant struct {
	ID           uuid.UUID  `json:"id"`
	OrgID        uuid.UUID  `json:"org_id"`
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
	PermissionRead Permission = "read"
)

// ResourceType enumerates valid grant resource types.
type ResourceType string

const (
	ResourceAgentTraces ResourceType = "agent_traces"
)

// RoleRank returns the numeric rank of a role (higher = more privileges).
// Only relative ordering matters — RoleAtLeast uses >= comparison.
func RoleRank(r AgentRole) int {
	switch r {
	case RolePlatformAdmin:
		return 5
	case RoleOrgOwner:
		return 4
	case RoleAdmin:
		return 3
	case RoleAgent:
		return 2
	case RoleReader:
		return 1
	default:
		return 0
	}
}

// RoleAtLeast returns true if role r has at least the privileges of minRole.
func RoleAtLeast(r, minRole AgentRole) bool {
	return RoleRank(r) >= RoleRank(minRole)
}

// ValidateTag checks that a tag conforms to the allowed format.
// Tags must start with a lowercase letter and contain only lowercase
// alphanumeric characters, hyphens, and underscores.
func ValidateTag(tag string) error {
	if len(tag) == 0 {
		return fmt.Errorf("tag must not be empty")
	}
	if len(tag) > 64 {
		return fmt.Errorf("tag must be at most 64 characters")
	}
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		if i == 0 {
			if c < 'a' || c > 'z' {
				return fmt.Errorf("tag must start with a lowercase letter, got %q", c)
			}
			continue
		}
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return fmt.Errorf("tag contains invalid character at position %d: %q", i, c)
		}
	}
	return nil
}

// reservedAgentIDs is the set of agent_id values that cannot be claimed by
// user-created agents. These names imply elevated privilege or system identity
// and would be misleading in the audit trail if assigned to ordinary agents.
//
// Note: the seed admin agent (agent_id="admin") is created internally at
// startup and is intentionally exempt from this check.
var reservedAgentIDs = map[string]struct{}{
	"admin":     {},
	"system":    {},
	"root":      {},
	"platform":  {},
	"superuser": {},
	"service":   {},
	"akashi":    {},
	"internal":  {},
}

// IsReservedAgentID reports whether id is a reserved name that cannot be
// claimed by user-created agents. Call this at agent creation time only —
// not on lookup paths, since the seed "admin" agent already exists.
func IsReservedAgentID(id string) bool {
	_, ok := reservedAgentIDs[id]
	return ok
}

// ValidateAgentID checks that an agent ID conforms to the allowed format.
// Agent IDs must be 1-255 ASCII characters: alphanumeric, dots, hyphens,
// underscores, and @ signs.
func ValidateAgentID(id string) error {
	if len(id) == 0 {
		return fmt.Errorf("agent_id is required")
	}
	if len(id) > 255 {
		return fmt.Errorf("agent_id must be at most 255 characters")
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') &&
			c != '.' && c != '-' && c != '_' && c != '@' {
			return fmt.Errorf("agent_id contains invalid character at position %d: %q", i, c)
		}
	}
	return nil
}
