package trace

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"
)

func TestBufferDoubleStartIsNoop(t *testing.T) {
	// Buffer.Start() must be idempotent — a second call logs a warning and returns
	// without spawning a second flush goroutine or panicking on double close(b.done).
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	buf := NewBuffer(nil, logger, 100, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	buf.Start(ctx) // First call — should work.
	buf.Start(ctx) // Second call — should be a no-op, no panic.

	// Verify started is true.
	if !buf.started.Load() {
		t.Fatal("expected started to be true after Start()")
	}

	// Clean shutdown.
	cancel()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()
	buf.Drain(drainCtx)
}
