package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ashita-ai/kyoyu/internal/auth"
	"github.com/ashita-ai/kyoyu/internal/config"
	"github.com/ashita-ai/kyoyu/internal/mcp"
	"github.com/ashita-ai/kyoyu/internal/server"
	"github.com/ashita-ai/kyoyu/internal/service/embedding"
	"github.com/ashita-ai/kyoyu/internal/service/trace"
	"github.com/ashita-ai/kyoyu/internal/storage"
	"github.com/ashita-ai/kyoyu/internal/telemetry"
)

func main() {
	level := slog.LevelInfo
	if os.Getenv("KYOYU_LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, logger); err != nil {
		slog.Error("fatal error", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	// Load configuration.
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	slog.Info("kyoyu starting", "version", "0.1.0", "port", cfg.Port)

	// Initialize OpenTelemetry.
	otelShutdown, err := telemetry.Init(ctx, cfg.OTELEndpoint, cfg.ServiceName, "0.1.0")
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	defer otelShutdown(context.Background())

	// Connect to database.
	db, err := storage.New(ctx, cfg.DatabaseURL, cfg.NotifyURL, logger)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer db.Close(ctx)

	// Run migrations (dev mode only; production uses Atlas).
	if err := db.RunMigrations(ctx, os.DirFS("migrations")); err != nil {
		slog.Warn("migrations failed (may already exist)", "error", err)
	}

	// Create JWT manager.
	jwtMgr, err := auth.NewJWTManager(cfg.JWTPrivateKeyPath, cfg.JWTPublicKeyPath, cfg.JWTExpiration)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	// Create embedding provider.
	var embedder embedding.Provider
	if cfg.OpenAIAPIKey != "" {
		embedder = embedding.NewOpenAIProvider(cfg.OpenAIAPIKey, cfg.EmbeddingModel)
		slog.Info("embedding provider: openai", "model", cfg.EmbeddingModel)
	} else {
		embedder = embedding.NewNoopProvider(1536)
		slog.Warn("no OpenAI API key configured, using noop embedding provider")
	}

	// Create event buffer.
	buf := trace.NewBuffer(db, logger, cfg.EventBufferSize, cfg.EventFlushTimeout)
	buf.Start(ctx)

	// Create MCP server.
	mcpSrv := mcp.New(db, embedder, logger)

	// Create and start HTTP server (MCP mounted at /mcp).
	srv := server.New(db, jwtMgr, embedder, buf, logger,
		cfg.Port, cfg.ReadTimeout, cfg.WriteTimeout,
		mcpSrv.MCPServer())

	// Seed admin agent.
	if err := srv.Handlers().SeedAdmin(ctx, cfg.AdminAPIKey); err != nil {
		slog.Warn("admin seed failed", "error", err)
	}

	// Start conflict refresh loop.
	go conflictRefreshLoop(ctx, db, logger, cfg.ConflictRefreshInterval)

	// Start HTTP server in background.
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Wait for shutdown signal or server error.
	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}

	// Graceful shutdown.
	fmt.Println()
	slog.Info("kyoyu shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	// Drain event buffer.
	buf.Drain(shutdownCtx)

	// Shutdown HTTP server.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("http shutdown error", "error", err)
	}

	slog.Info("kyoyu stopped")
	return nil
}

func conflictRefreshLoop(ctx context.Context, db *storage.DB, logger *slog.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := db.RefreshConflicts(ctx); err != nil {
				logger.Warn("conflict refresh failed", "error", err)
			}
		}
	}
}
