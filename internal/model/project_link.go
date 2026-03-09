package model

import (
	"time"

	"github.com/google/uuid"
)

// ProjectLink connects two projects for cross-project conflict detection.
// Links are bidirectional: a link from A→B means conflicts between decisions
// in project A and project B will be detected. The link_type field allows
// future extensibility beyond conflict_scope.
type ProjectLink struct {
	ID        uuid.UUID `json:"id"`
	OrgID     uuid.UUID `json:"org_id"`
	ProjectA  string    `json:"project_a"`
	ProjectB  string    `json:"project_b"`
	LinkType  string    `json:"link_type"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateProjectLinkRequest is the request body for POST /v1/project-links.
type CreateProjectLinkRequest struct {
	ProjectA string `json:"project_a"`
	ProjectB string `json:"project_b"`
	LinkType string `json:"link_type,omitempty"`
}

// GrantAllProjectLinksRequest is the request body for POST /v1/project-links/grant-all.
type GrantAllProjectLinksRequest struct {
	LinkType string `json:"link_type,omitempty"`
}
