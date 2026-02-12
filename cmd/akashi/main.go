package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/ashita-ai/akashi/api"
	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/authz"
	"github.com/ashita-ai/akashi/internal/config"
	"github.com/ashita-ai/akashi/internal/integrity"
	"github.com/ashita-ai/akashi/internal/mcp"
	"github.com/ashita-ai/akashi/internal/ratelimit"
	"github.com/ashita-ai/akashi/internal/search"
	"github.com/ashita-ai/akashi/internal/server"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/service/trace"
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
	otelShutdown, err := telemetry.Init(ctx, cfg.OTELEndpoint, cfg.ServiceName, version, cfg.OTELInsecure)
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

	// Register connection pool OTEL metrics (after telemetry.Init).
	db.RegisterPoolMetrics()

	// Run migrations (dev mode only; production uses Atlas).
	// RunMigrations tracks applied files in schema_migrations and skips duplicates,
	// so errors here indicate real failures (not "already exists").
	if err := db.RunMigrations(ctx, os.DirFS("migrations")); err != nil {
		slog.Warn("migrations failed", "error", err)
	}

	// Verify critical tables exist after migration. If the pgvector or timescaledb
	// extension failed to create (e.g., missing init.sql), migrations 003+ fail
	// silently and the server starts with no tables. Catch this early.
	var schemaOK bool
	if err := db.Pool().QueryRow(ctx,
		`SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'decisions')`,
	).Scan(&schemaOK); err != nil {
		return fmt.Errorf("schema verification: %w", err)
	}
	if !schemaOK {
		return fmt.Errorf("critical table 'decisions' does not exist after migration — check that pgvector and timescaledb extensions are created (see docker/init.sql)")
	}

	// Create JWT manager.
	jwtMgr, err := auth.NewJWTManager(cfg.JWTPrivateKeyPath, cfg.JWTPublicKeyPath, cfg.JWTExpiration)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	// Create embedding provider.
	embedder := newEmbeddingProvider(cfg, logger)

	// Initialize Qdrant search index and outbox worker (optional — disabled if QDRANT_URL is empty).
	var searcher search.Searcher
	var outboxWorker *search.OutboxWorker
	if cfg.QdrantURL != "" {
		qdrantIndex, err := search.NewQdrantIndex(search.QdrantConfig{
			URL:        cfg.QdrantURL,
			APIKey:     cfg.QdrantAPIKey,
			Collection: cfg.QdrantCollection,
			Dims:       uint64(cfg.EmbeddingDimensions), //nolint:gosec // validated positive in config.Validate
		}, logger)
		if err != nil {
			return fmt.Errorf("qdrant: %w", err)
		}
		defer func() { _ = qdrantIndex.Close() }()

		if err := qdrantIndex.EnsureCollection(ctx); err != nil {
			return fmt.Errorf("qdrant ensure collection: %w", err)
		}

		searcher = qdrantIndex
		outboxWorker = search.NewOutboxWorker(db.Pool(), qdrantIndex, logger, cfg.OutboxPollInterval, cfg.OutboxBatchSize)
		outboxWorker.Start(ctx)
		logger.Info("qdrant: enabled", "collection", cfg.QdrantCollection)
	} else {
		logger.Info("qdrant: disabled (no QDRANT_URL)")
	}

	// Create decision service (shared by HTTP and MCP handlers).
	decisionSvc := decisions.New(db, embedder, searcher, logger)

	// Backfill embeddings for decisions stored without one (e.g. when the
	// provider was previously noop). Runs once at startup, non-fatal.
	if n, err := decisionSvc.BackfillEmbeddings(ctx, 500); err != nil {
		logger.Warn("embedding backfill failed", "error", err)
	} else if n > 0 {
		logger.Info("embedding backfill complete", "count", n)
	}

	// Create event buffer.
	buf := trace.NewBuffer(db, logger, cfg.EventBufferSize, cfg.EventFlushTimeout)
	buf.Start(ctx)

	// Create grant cache (30s TTL — short enough to pick up new grants quickly,
	// long enough to eliminate 2-3 DB queries per request for non-admin users).
	grantCache := authz.NewGrantCache(30 * time.Second)
	defer grantCache.Close()

	// Create MCP server.
	mcpSrv := mcp.New(db, decisionSvc, grantCache, logger, version)

	// Create SSE broker (requires LISTEN/NOTIFY connection).
	var broker *server.Broker
	if db.HasNotifyConn() {
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

	// Create rate limiter.
	var limiter ratelimit.Limiter
	if cfg.RateLimitEnabled {
		limiter = ratelimit.NewMemoryLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)
		defer func() { _ = limiter.Close() }()
		logger.Info("rate limiting: memory (in-process token bucket)",
			"rps", cfg.RateLimitRPS, "burst", cfg.RateLimitBurst)
	} else {
		limiter = ratelimit.NoopLimiter{}
		logger.Info("rate limiting: disabled")
	}

	// Create and start HTTP server (MCP mounted at /mcp).
	srv := server.New(server.ServerConfig{
		DB:                  db,
		JWTMgr:              jwtMgr,
		DecisionSvc:         decisionSvc,
		Buffer:              buf,
		Broker:              broker,
		Searcher:            searcher,
		GrantCache:          grantCache,
		Logger:              logger,
		Port:                cfg.Port,
		ReadTimeout:         cfg.ReadTimeout,
		WriteTimeout:        cfg.WriteTimeout,
		MCPServer:           mcpSrv.MCPServer(),
		Version:             version,
		MaxRequestBodyBytes: cfg.MaxRequestBodyBytes,
		RateLimiter:         limiter,
		CORSAllowedOrigins:  cfg.CORSAllowedOrigins,
		UIFS:                uiFS,
		OpenAPISpec:         api.OpenAPISpec,
	})

	// Seed admin agent.
	if err := srv.Handlers().SeedAdmin(ctx, cfg.AdminAPIKey); err != nil {
		slog.Warn("admin seed failed", "error", err)
	}

	// Start conflict refresh loop.
	go conflictRefreshLoop(ctx, db, logger, cfg.ConflictRefreshInterval)

	// Start integrity proof loop (Merkle tree batch proofs).
	go integrityProofLoop(ctx, db, logger, cfg.IntegrityProofInterval)

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

	// Graceful shutdown. Each phase gets its own timeout so early completion
	// doesn't steal budget from later phases. Total worst-case: 30s.
	// Order: (1) stop accepting new HTTP requests and drain in-flight (they may
	// still append to the buffer), (2) flush the event buffer to Postgres,
	// (3) sync remaining outbox entries to Qdrant.
	slog.Info("akashi shutting down")

	httpCtx, httpCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := srv.Shutdown(httpCtx); err != nil {
		slog.Error("http shutdown error", "error", err)
	}
	httpCancel()

	bufCtx, bufCancel := context.WithTimeout(context.Background(), 10*time.Second)
	buf.Drain(bufCtx)
	bufCancel()

	if outboxWorker != nil {
		outboxCtx, outboxCancel := context.WithTimeout(context.Background(), 10*time.Second)
		outboxWorker.Drain(outboxCtx)
		outboxCancel()
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
		p, err := embedding.NewOpenAIProvider(cfg.OpenAIAPIKey, cfg.EmbeddingModel, dims)
		if err != nil {
			logger.Error("openai provider init failed", "error", err)
			return embedding.NewNoopProvider(dims)
		}
		return p

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
			p, err := embedding.NewOpenAIProvider(cfg.OpenAIAPIKey, cfg.EmbeddingModel, dims)
			if err != nil {
				logger.Error("openai provider init failed", "error", err)
				return embedding.NewNoopProvider(dims)
			}
			return p
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

func conflictRefreshLoop(ctx context.Context, db *storage.DB, logger *slog.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	lastNotifiedAt := time.Now().UTC()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := db.RefreshConflicts(ctx); err != nil {
				logger.Warn("conflict refresh failed", "error", err)
				continue
			}

			// Detect new conflicts since last notification and send pg_notify for each.
			// Cap at 1000 to avoid unbounded memory use; the SSE notification path
			// only needs to signal *that* new conflicts exist — clients re-fetch via API.
			newConflicts, err := db.NewConflictsSince(ctx, lastNotifiedAt, 1000)
			if err != nil {
				logger.Warn("new conflicts query failed", "error", err)
				continue
			}

			for _, c := range newConflicts {
				payload, err := json.Marshal(map[string]any{
					"org_id":        c.OrgID,
					"decision_a_id": c.DecisionAID,
					"decision_b_id": c.DecisionBID,
					"agent_a":       c.AgentA,
					"agent_b":       c.AgentB,
					"decision_type": c.DecisionType,
				})
				if err != nil {
					logger.Warn("conflict notify marshal failed", "error", err)
					continue
				}
				if err := db.Notify(ctx, storage.ChannelConflicts, string(payload)); err != nil {
					logger.Warn("conflict notify failed", "error", err)
				}
				if c.DetectedAt.After(lastNotifiedAt) {
					lastNotifiedAt = c.DetectedAt
				}
			}

			if len(newConflicts) > 0 {
				logger.Info("conflict notifications sent", "count", len(newConflicts))
			}
		}
	}
}

func integrityProofLoop(ctx context.Context, db *storage.DB, logger *slog.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			buildIntegrityProofs(ctx, db, logger)
		}
	}
}

func buildIntegrityProofs(ctx context.Context, db *storage.DB, logger *slog.Logger) {
	orgIDs, err := db.ListOrganizationIDs(ctx)
	if err != nil {
		logger.Warn("integrity proof: list orgs failed", "error", err)
		return
	}

	now := time.Now().UTC()

	for _, orgID := range orgIDs {
		// Get the latest proof to determine the batch window start.
		latest, err := db.GetLatestIntegrityProof(ctx, orgID)
		if err != nil {
			logger.Warn("integrity proof: get latest failed", "error", err, "org_id", orgID)
			continue
		}

		batchStart := time.Time{} // Zero time: include all decisions from the beginning.
		var previousRoot *string
		if latest != nil {
			batchStart = latest.BatchEnd
			previousRoot = &latest.RootHash
		}

		// Get content hashes for decisions in this batch window.
		hashes, err := db.GetDecisionHashesForBatch(ctx, orgID, batchStart, now)
		if err != nil {
			logger.Warn("integrity proof: get hashes failed", "error", err, "org_id", orgID)
			continue
		}

		if len(hashes) == 0 {
			continue // No new decisions; skip proof.
		}

		// Hashes come pre-sorted from GetDecisionHashesForBatch (ORDER BY content_hash ASC).
		root := integrity.BuildMerkleRoot(hashes)

		proof := storage.IntegrityProof{
			OrgID:         orgID,
			BatchStart:    batchStart,
			BatchEnd:      now,
			DecisionCount: len(hashes),
			RootHash:      root,
			PreviousRoot:  previousRoot,
			CreatedAt:     now,
		}

		if err := db.CreateIntegrityProof(ctx, proof); err != nil {
			logger.Warn("integrity proof: create failed", "error", err, "org_id", orgID)
			continue
		}

		logger.Info("integrity proof created",
			"org_id", orgID,
			"decisions", len(hashes),
			"root_hash", root[:16]+"...",
		)
	}
}
