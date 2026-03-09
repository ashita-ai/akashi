// Package testutil provides shared test infrastructure for integration tests
// that require a TimescaleDB container with pgvector.
//
// Usage in TestMain:
//
//	func TestMain(m *testing.M) {
//	    tc := testutil.MustStartTimescaleDB()
//	    defer tc.Terminate()
//	    testDB, _ = tc.NewTestDB(context.Background(), logger)
//	    os.Exit(m.Run())
//	}
package testutil

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/migrations"
)

// TestContainer wraps a testcontainers container with a DSN for connecting.
type TestContainer struct {
	Container testcontainers.Container
	DSN       string
}

// MustStartTimescaleDB starts a TimescaleDB container with pgvector and timescaledb
// extensions pre-created. Calls os.Exit(1) on failure (suitable for TestMain).
func MustStartTimescaleDB() *TestContainer {
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "timescale/timescaledb:latest-pg18",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "akashi",
			"POSTGRES_PASSWORD": "akashi",
			"POSTGRES_DB":       "akashi",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(60 * time.Second),
	}

	// Multiple test packages run in parallel and race to create the
	// testcontainers reaper ("Ryuk"). The loser gets a Docker "name
	// already in use" error that is transient — the reaper becomes
	// available once the winner finishes creating it. Retry a few
	// times with a short backoff to handle this.
	var container testcontainers.Container
	var err error
	for attempt := range 5 {
		container, err = testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: req,
			Started:          true,
		})
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "is already in use") {
			break
		}
		fmt.Fprintf(os.Stderr, "testutil: reaper conflict (attempt %d/5), retrying...\n", attempt+1)
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "testutil: failed to start container: %v\n", err)
		os.Exit(1)
	}

	host, err := container.Host(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testutil: failed to get container host: %v\n", err)
		os.Exit(1)
	}

	port, err := container.MappedPort(ctx, "5432")
	if err != nil {
		fmt.Fprintf(os.Stderr, "testutil: failed to get container port: %v\n", err)
		os.Exit(1)
	}

	dsn := fmt.Sprintf("postgres://akashi:akashi@%s:%s/akashi?sslmode=disable", host, port.Port())

	// Bootstrap extensions before any pool is created so pgvector types
	// get registered on the pool's AfterConnect hook.
	bootstrapConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testutil: failed to bootstrap connection: %v\n", err)
		os.Exit(1)
	}
	for _, ext := range []string{"vector", "timescaledb"} {
		if _, err := bootstrapConn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+ext); err != nil {
			fmt.Fprintf(os.Stderr, "testutil: failed to create %s extension: %v\n", ext, err)
			os.Exit(1)
		}
	}
	_ = bootstrapConn.Close(ctx)

	return &TestContainer{Container: container, DSN: dsn}
}

// NewTestDB creates a storage.DB connected to this container and runs all migrations.
func (tc *TestContainer) NewTestDB(ctx context.Context, logger *slog.Logger) (*storage.DB, error) {
	db, err := storage.New(ctx, tc.DSN, "", logger)
	if err != nil {
		return nil, fmt.Errorf("testutil: create DB: %w", err)
	}
	if err := db.RunMigrations(ctx, migrations.FS); err != nil {
		return nil, fmt.Errorf("testutil: run migrations: %w", err)
	}
	return db, nil
}

// NewTestDBWithNotify creates a storage.DB with both pool and notify DSN
// pointing to this container and runs all migrations. The dedicated notify
// connection enables testing LISTEN/NOTIFY, WaitForNotification, and reconnect.
func (tc *TestContainer) NewTestDBWithNotify(ctx context.Context, logger *slog.Logger) (*storage.DB, error) {
	db, err := storage.New(ctx, tc.DSN, tc.DSN, logger)
	if err != nil {
		return nil, fmt.Errorf("testutil: create DB with notify: %w", err)
	}
	if err := db.RunMigrations(ctx, migrations.FS); err != nil {
		return nil, fmt.Errorf("testutil: run migrations (notify): %w", err)
	}
	return db, nil
}

// Terminate stops and removes the container.
func (tc *TestContainer) Terminate() {
	_ = tc.Container.Terminate(context.Background())
}

// TestLogger returns a logger configured for test output (warns only).
func TestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}
