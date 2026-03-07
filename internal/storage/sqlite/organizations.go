package sqlite

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// EnsureDefaultOrg creates the default organization (uuid.Nil) if it doesn't exist.
func (l *LiteDB) EnsureDefaultOrg(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO organizations (id, name, slug, plan, created_at, updated_at)
		 VALUES (?, 'Default', 'default', 'oss', datetime('now'), datetime('now'))`,
		uuidStr(uuid.Nil),
	)
	if err != nil {
		return fmt.Errorf("sqlite: ensure default org: %w", err)
	}
	return nil
}
