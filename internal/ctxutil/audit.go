package ctxutil

import "github.com/google/uuid"

// AuditMeta carries the metadata needed to build a MutationAuditEntry.
// It lives in ctxutil so both server and mcp packages can populate it
// without circular imports.
type AuditMeta struct {
	RequestID    string
	OrgID        uuid.UUID
	ActorAgentID string
	ActorRole    string
	HTTPMethod   string
	Endpoint     string
}
