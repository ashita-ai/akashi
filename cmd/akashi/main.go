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

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/config"
	"github.com/ashita-ai/akashi/internal/mcp"
	"github.com/ashita-ai/akashi/internal/server"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/service/trace"
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/internal/telemetry"
)

func main() {
	level := slog.LevelInfo
	if os.Getenv("AKASHI_LOG_LEVEL") == "debug" {
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

	slog.Info("akashi starting", "version", "0.1.0", "port", cfg.Port)

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
	embedder := newEmbeddingProvider(cfg, logger)

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
	slog.Info("akashi shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	// Drain event buffer.
	buf.Drain(shutdownCtx)

	// Shutdown HTTP server.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("http shutdown error", "error", err)
	}

	slog.Info("akashi stopped")
	return nil
}

// newEmbeddingProvider creates an embedding provider based on configuration.
// Provider selection: "openai", "ollama", "noop", or "auto" (default).
// Auto mode tries OpenAI if key present, then Ollama if reachable, else noop.
func newEmbeddingProvider(cfg config.Config, logger *slog.Logger) embedding.Provider {
	switch cfg.EmbeddingProvider {
	case "openai":
		if cfg.OpenAIAPIKey == "" {
			logger.Error("OPENAI_API_KEY required when AKASHI_EMBEDDING_PROVIDER=openai")
			return embedding.NewNoopProvider(1536)
		}
		logger.Info("embedding provider: openai", "model", cfg.EmbeddingModel)
		return embedding.NewOpenAIProvider(cfg.OpenAIAPIKey, cfg.EmbeddingModel)

	case "ollama":
		logger.Info("embedding provider: ollama", "url", cfg.OllamaURL, "model", cfg.OllamaModel)
		return embedding.NewOllamaProvider(cfg.OllamaURL, cfg.OllamaModel, 1024, 1536)

	case "noop":
		logger.Info("embedding provider: noop (semantic search disabled)")
		return embedding.NewNoopProvider(1536)

	case "auto":
		fallthrough
	default:
		// Auto-detect: prefer OpenAI if key present, then Ollama if reachable, else noop.
		if cfg.OpenAIAPIKey != "" {
			logger.Info("embedding provider: openai (auto-detected)", "model", cfg.EmbeddingModel)
			return embedding.NewOpenAIProvider(cfg.OpenAIAPIKey, cfg.EmbeddingModel)
		}
		if ollamaReachable(cfg.OllamaURL) {
			logger.Info("embedding provider: ollama (auto-detected)", "url", cfg.OllamaURL, "model", cfg.OllamaModel)
			return embedding.NewOllamaProvider(cfg.OllamaURL, cfg.OllamaModel, 1024, 1536)
		}
		logger.Warn("no embedding provider available, using noop (semantic search disabled)")
		return embedding.NewNoopProvider(1536)
	}
}

// ollamaReachable checks if an Ollama server is responding.
func ollamaReachable(baseURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
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
