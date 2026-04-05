package trace

import (
	"log/slog"
	"os"
)

// testLogger returns a warn-level logger for use within individual tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}
