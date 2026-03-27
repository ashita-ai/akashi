package sqlite

import (
	"context"

	"github.com/google/uuid"
)

// ResolveProjectAlias returns "" in lite mode — project_links are not
// supported in the SQLite backend. Alias resolution is a server-mode
// feature backed by the PostgreSQL project_links table.
func (l *LiteDB) ResolveProjectAlias(_ context.Context, _ uuid.UUID, _ string) (string, error) {
	return "", nil
}

// CreateProjectAlias is a no-op in lite mode.
func (l *LiteDB) CreateProjectAlias(_ context.Context, _ uuid.UUID, _, _, _ string) error {
	return nil
}

// IsAliasTarget always returns false in lite mode — aliases are not supported.
func (l *LiteDB) IsAliasTarget(_ context.Context, _ uuid.UUID, _ string) (bool, error) {
	return false, nil
}
