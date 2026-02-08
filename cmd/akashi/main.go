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

	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/billing"
	"github.com/ashita-ai/akashi/internal/config"
	"github.com/ashita-ai/akashi/internal/mcp"
	"github.com/ashita-ai/akashi/internal/ratelimit"
	"github.com/ashita-ai/akashi/internal/server"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/service/trace"
	"github.com/ashita-ai/akashi/internal/signup"
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/internal/telemetry"
	"github.com/ashita-ai/akashi/ui"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	os.Exit(run0())
}

func run0() int {
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
		return 1
	}
	return 0
}

func run(ctx context.Context, logger *slog.Logger) error {
	// Load .env file if present (non-fatal; production won't have one).
	_ = godotenv.Load()

	// Load configuration.
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	slog.Info("akashi starting", "version", version, "port", cfg.Port)

	// Initialize OpenTelemetry.
	otelShutdown, err := telemetry.Init(ctx, cfg.OTELEndpoint, cfg.ServiceName, version)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	defer func() { _ = otelShutdown(context.Background()) }()

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

	// Create billing service (Stripe integration; disabled if STRIPE_SECRET_KEY is empty).
	billingSvc := billing.New(db, billing.Config{
		SecretKey:     cfg.StripeSecretKey,
		WebhookSecret: cfg.StripeWebhookSecret,
		PriceIDPro:    cfg.StripePriceIDPro,
	}, logger)
	if billingSvc.Enabled() {
		logger.Info("billing: enabled (Stripe configured)")
	} else {
		logger.Info("billing: disabled (no STRIPE_SECRET_KEY)")
	}

	// Create decision service (shared by HTTP and MCP handlers).
	decisionSvc := decisions.New(db, embedder, billingSvc, logger)

	// Create event buffer.
	buf := trace.NewBuffer(db, logger, cfg.EventBufferSize, cfg.EventFlushTimeout)
	buf.Start(ctx)

	// Create rate limiter (backed by Redis if configured, noop otherwise).
	limiter := newRateLimiter(cfg, logger)
	if limiter != nil {
		defer func() { _ = limiter.Close() }()
	}

	// Create signup service.
	signupSvc := signup.New(db, signup.Config{
		SMTPHost: cfg.SMTPHost,
		SMTPPort: cfg.SMTPPort,
		SMTPUser: cfg.SMTPUser,
		SMTPPass: cfg.SMTPPassword,
		SMTPFrom: cfg.SMTPFrom,
		BaseURL:  cfg.BaseURL,
	}, logger)

	// Create MCP server.
	mcpSrv := mcp.New(db, decisionSvc, logger, version)

	// Create SSE broker (requires LISTEN/NOTIFY connection).
	var broker *server.Broker
	if db.NotifyConn() != nil {
		broker = server.NewBroker(db, logger)
		go broker.Start(ctx)
	} else {
		logger.Info("SSE broker: disabled (no notify connection)")
	}

	// Load embedded UI filesystem (non-nil only when built with -tags ui).
	uiFS, err := ui.DistFS()
	if err != nil {
		return fmt.Errorf("ui: %w", err)
	}
	if uiFS != nil {
		logger.Info("ui: embedded SPA loaded")
	}

	// Create and start HTTP server (MCP mounted at /mcp).
	srv := server.New(db, jwtMgr, decisionSvc, billingSvc, buf, limiter, broker, signupSvc, logger,
		cfg.Port, cfg.ReadTimeout, cfg.WriteTimeout,
		mcpSrv.MCPServer(), version, cfg.MaxRequestBodyBytes, uiFS)

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
// Provider selection: "ollama", "openai", "noop", or "auto" (default).
// Auto mode tries Ollama if reachable, then OpenAI if key present, else noop.
// Ollama is preferred: embeddings stay on-premises with no external API costs.
func newEmbeddingProvider(cfg config.Config, logger *slog.Logger) embedding.Provider {
	dims := cfg.EmbeddingDimensions

	switch cfg.EmbeddingProvider {
	case "openai":
		if cfg.OpenAIAPIKey == "" {
			logger.Error("OPENAI_API_KEY required when AKASHI_EMBEDDING_PROVIDER=openai")
			return embedding.NewNoopProvider(dims)
		}
		logger.Info("embedding provider: openai", "model", cfg.EmbeddingModel, "dimensions", dims)
		return embedding.NewOpenAIProvider(cfg.OpenAIAPIKey, cfg.EmbeddingModel, dims)

	case "ollama":
		logger.Info("embedding provider: ollama", "url", cfg.OllamaURL, "model", cfg.OllamaModel, "dimensions", dims)
		return embedding.NewOllamaProvider(cfg.OllamaURL, cfg.OllamaModel, dims)

	case "noop":
		logger.Info("embedding provider: noop (semantic search disabled)")
		return embedding.NewNoopProvider(dims)

	case "auto":
		fallthrough
	default:
		// Auto-detect: prefer Ollama (on-premises, no cost), then OpenAI, else noop.
		if ollamaReachable(cfg.OllamaURL) {
			logger.Info("embedding provider: ollama (auto-detected)", "url", cfg.OllamaURL, "model", cfg.OllamaModel, "dimensions", dims)
			return embedding.NewOllamaProvider(cfg.OllamaURL, cfg.OllamaModel, dims)
		}
		if cfg.OpenAIAPIKey != "" {
			logger.Info("embedding provider: openai (auto-detected)", "model", cfg.EmbeddingModel, "dimensions", dims)
			return embedding.NewOpenAIProvider(cfg.OpenAIAPIKey, cfg.EmbeddingModel, dims)
		}
		logger.Warn("no embedding provider available, using noop (semantic search disabled)")
		return embedding.NewNoopProvider(dims)
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
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// newRateLimiter creates a Redis-backed rate limiter. If Redis is not configured
// or not reachable, returns a noop limiter (all requests allowed).
func newRateLimiter(cfg config.Config, logger *slog.Logger) *ratelimit.Limiter {
	if cfg.RedisURL == "" {
		logger.Info("rate limiting: disabled (no REDIS_URL)")
		return ratelimit.New(nil, logger)
	}

	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		logger.Warn("rate limiting: disabled (invalid REDIS_URL)", "error", err)
		return ratelimit.New(nil, logger)
	}

	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		logger.Warn("rate limiting: disabled (Redis unreachable)", "error", err)
		_ = client.Close()
		return ratelimit.New(nil, logger)
	}

	logger.Info("rate limiting: enabled", "redis", cfg.RedisURL)
	return ratelimit.New(client, logger)
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
