package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	akashi "github.com/ashita-ai/akashi"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	os.Exit(run0())
}

func run0() int {
	level := parseLogLevel(os.Getenv("AKASHI_LOG_LEVEL"))
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	app, err := akashi.New(
		akashi.WithVersion(version),
		akashi.WithLogger(logger),
	)
	if err != nil {
		slog.Error("startup failed", "error", err)
		return 1
	}
	if err := app.Run(ctx); err != nil {
		slog.Error("fatal error", "error", err)
		return 1
	}
	return 0
}

func parseLogLevel(raw string) slog.Level {
	switch raw {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
