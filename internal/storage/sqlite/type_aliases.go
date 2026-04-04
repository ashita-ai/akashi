package sqlite

import (
	"context"

	"github.com/google/uuid"
)

// ResolveDecisionTypeAlias returns "" in lite mode — decision type aliases
// are not supported in the SQLite backend.
func (l *LiteDB) ResolveDecisionTypeAlias(_ context.Context, _ uuid.UUID, _ string) (string, error) {
	return "", nil
}

// CreateDecisionTypeAlias is a no-op in lite mode.
func (l *LiteDB) CreateDecisionTypeAlias(_ context.Context, _ uuid.UUID, _, _, _ string) error {
	return nil
}
