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
