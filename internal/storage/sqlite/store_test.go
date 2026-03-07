package sqlite

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestNew_CreatesSchema(t *testing.T) {
	s, err := New(":memory:", testLogger())
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	// Verify decisions table exists.
	var count int
	err = s.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='decisions'`,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// Verify FTS5 table exists.
	err = s.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='decisions_fts'`,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// Verify indexes exist.
	var idxCount int
	err = s.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name LIKE 'idx_decisions_%'`,
	).Scan(&idxCount)
	require.NoError(t, err)
	assert.Equal(t, 2, idxCount, "expected idx_decisions_type and idx_decisions_created")
}

func TestNew_IdempotentSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Open and close twice — schema creation must be idempotent.
	s1, err := New(dbPath, testLogger())
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	s2, err := New(dbPath, testLogger())
	require.NoError(t, err)
	require.NoError(t, s2.Close())
}

func TestNew_WALMode(t *testing.T) {
	s, err := New(":memory:", testLogger())
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	var mode string
	err = s.db.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode)
	require.NoError(t, err)
	// In-memory databases may report "memory" instead of "wal".
	// File-based databases will report "wal".
	assert.Contains(t, []string{"wal", "memory"}, mode)
}

func TestNew_WALMode_FileBacked(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "wal_test.db")

	s, err := New(dbPath, testLogger())
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	var mode string
	err = s.db.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode)
	require.NoError(t, err)
	assert.Equal(t, "wal", mode)
}

func TestStore_Close(t *testing.T) {
	s, err := New(":memory:", testLogger())
	require.NoError(t, err)
	require.NoError(t, s.Close())

	// Subsequent operations should fail.
	err = s.db.PingContext(context.Background())
	assert.Error(t, err)
}

func TestStore_DB(t *testing.T) {
	s, err := New(":memory:", testLogger())
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	assert.NotNil(t, s.DB())
	assert.Equal(t, s.db, s.DB())
}
