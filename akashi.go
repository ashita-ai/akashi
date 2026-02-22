// Package akashi is the public API for embedding the Akashi decision audit server.
//
// Enterprise and plugin consumers import this package to construct and extend
// the server without forking it:
//
//	app, err := akashi.New(
//	    akashi.WithVersion(version),
//	    akashi.WithLogger(logger),
//	    akashi.WithEventHook(myEnterpriseHook{}),
//	    akashi.WithExtraRoutes(myEnterpriseRoutes),
//	)
//	if err != nil { ... }
//	if err := app.Run(ctx); err != nil { ... }
//
// The import graph enforces a strict no-cycle rule: akashi (root) imports
// internal/*, but internal/* never imports akashi (root).  Public types
// (Decision, Conflict, etc.) are standalone structs with no internal imports;
// conversion helpers (toPublicDecision, toPublicConflict) live here because
// this is the only file that sees both sides of the boundary.
package akashi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"github.com/ashita-ai/akashi/api"
	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/authz"
	"github.com/ashita-ai/akashi/internal/config"
	"github.com/ashita-ai/akashi/internal/conflicts"
	"github.com/ashita-ai/akashi/internal/integrity"
	"github.com/ashita-ai/akashi/internal/mcp"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/ratelimit"
	"github.com/ashita-ai/akashi/internal/search"
	"github.com/ashita-ai/akashi/internal/server"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/service/trace"
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/internal/telemetry"
	"github.com/ashita-ai/akashi/migrations"
	"github.com/ashita-ai/akashi/ui"
)

// App is the Akashi server lifecycle. Construct with New(), run with Run().
// App has no public fields — use New() options to configure it.
type App struct {
	cfg            config.Config
	db             *storage.DB
	srv            *server.Server
	buf            *trace.Buffer
	outbox         *search.OutboxWorker
	qdrantIndex    *search.QdrantIndex // nil when Qdrant is not configured
	grantCache     *authz.GrantCache
	conflictScorer *conflicts.Scorer
	broker         *server.Broker // nil when no notify connection
	otelShutdown   func(context.Context) error
	decisionHooks  []server.DecisionHook
	logger         *slog.Logger
	version        string
}

// New initialises the Akashi server. It connects to the database, runs
// migrations, wires all subsystems, and returns a ready-to-run App.
// It does NOT start any goroutines or accept HTTP connections — call Run().
func New(opts ...Option) (*App, error) {
	// Apply options.
	o := resolvedOptions{}
	for _, fn := range opts {
		fn(&o)
	}

	logger := o.logger
	if logger == nil {
		logger = slog.Default()
	}

	// Load .env file if present (non-fatal; production won't have one).
	_ = godotenv.Load()

	// Load configuration (env vars), then apply option overrides.
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if o.port != 0 {
		cfg.Port = o.port
	}
	if o.databaseURL != "" {
		cfg.DatabaseURL = o.databaseURL
	}
	if o.notifyURL != "" {
		cfg.NotifyURL = o.notifyURL
	}
	version := o.version
	if version == "" {
		version = "dev"
	}

	logger.Info("akashi starting", "version", version, "port", cfg.Port)

	// Initialize OpenTelemetry.
	otelShutdown, err := telemetry.Init(ctx(opts), cfg.OTELEndpoint, cfg.ServiceName, version, cfg.OTELInsecure)
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}

	// Connect to database.
	db, err := storage.New(context.Background(), cfg.DatabaseURL, cfg.NotifyURL, logger)
	if err != nil {
		_ = otelShutdown(context.Background())
		return nil, fmt.Errorf("storage: %w", err)
	}
	db.RegisterPoolMetrics()

	// Run OSS migrations.
	if cfg.SkipEmbeddedMigrations {
		logger.Info("embedded migrations skipped by config")
	} else if err := db.RunMigrations(context.Background(), migrations.FS); err != nil {
		db.Close(context.Background())
		_ = otelShutdown(context.Background())
		return nil, fmt.Errorf("migrations: %w", err)
	}

	// Run extra (enterprise) migrations after OSS migrations.
	for i, extraFS := range o.extraMigrations {
		if err := db.RunMigrations(context.Background(), extraFS); err != nil {
			db.Close(context.Background())
			_ = otelShutdown(context.Background())
			return nil, fmt.Errorf("extra migrations[%d]: %w", i, err)
		}
	}

	// Verify critical tables exist after migration.
	var schemaOK bool
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'decisions')`,
	).Scan(&schemaOK); err != nil {
		db.Close(context.Background())
		_ = otelShutdown(context.Background())
		return nil, fmt.Errorf("schema verification: %w", err)
	}
	if !schemaOK {
		db.Close(context.Background())
		_ = otelShutdown(context.Background())
		return nil, fmt.Errorf("critical table 'decisions' does not exist after migration — check that pgvector and timescaledb extensions are created (see docker/init.sql)")
	}

	// Create JWT manager.
	jwtMgr, err := auth.NewJWTManager(cfg.JWTPrivateKeyPath, cfg.JWTPublicKeyPath, cfg.JWTExpiration)
	if err != nil {
		db.Close(context.Background())
		_ = otelShutdown(context.Background())
		return nil, fmt.Errorf("auth: %w", err)
	}

	// Create embedding provider — external override takes priority over auto-detect.
	var embedder embedding.Provider
	if o.embeddingProvider != nil {
		embedder = embedding.NewPublicProviderAdapter(o.embeddingProvider)
	} else {
		embedder = newEmbeddingProvider(cfg, logger)
	}

	// Initialize Qdrant search index and outbox worker.
	var searcher search.Searcher
	var qdrantIndex *search.QdrantIndex
	var outboxWorker *search.OutboxWorker
	if cfg.QdrantURL != "" {
		var idxErr error
		qdrantIndex, idxErr = search.NewQdrantIndex(search.QdrantConfig{
			URL:        cfg.QdrantURL,
			APIKey:     cfg.QdrantAPIKey,
			Collection: cfg.QdrantCollection,
			Dims:       uint64(cfg.EmbeddingDimensions), //nolint:gosec // validated positive in config.Validate
		}, logger)
		if idxErr != nil {
			db.Close(context.Background())
			_ = otelShutdown(context.Background())
			return nil, fmt.Errorf("qdrant: %w", idxErr)
		}
		if err := qdrantIndex.EnsureCollection(context.Background()); err != nil {
			_ = qdrantIndex.Close()
			db.Close(context.Background())
			_ = otelShutdown(context.Background())
			return nil, fmt.Errorf("qdrant ensure collection: %w", err)
		}
		searcher = qdrantIndex
		outboxWorker = search.NewOutboxWorker(db.Pool(), qdrantIndex, logger, cfg.OutboxPollInterval, cfg.OutboxBatchSize)
		logger.Info("qdrant: enabled", "collection", cfg.QdrantCollection)
	} else {
		logger.Info("qdrant: disabled (no QDRANT_URL)")
	}

	// External Searcher override (replaces Qdrant for user-facing search).
	if o.searcher != nil {
		searcher = &searcherAdapter{s: o.searcher}
	}

	// Create conflict validator.
	conflictValidator := newConflictValidator(cfg, logger)

	// Create conflict scorer.
	backfillWorkers := cfg.ConflictBackfillWorkers
	if _, isOllama := conflictValidator.(*conflicts.OllamaValidator); isOllama && backfillWorkers > 1 {
		backfillWorkers = 1
		logger.Info("conflict backfill: capped workers to 1 (Ollama is serial)")
	}
	conflictScorer := conflicts.NewScorer(db, logger, cfg.ConflictSignificanceThreshold, conflictValidator, backfillWorkers, cfg.ConflictDecayLambda)
	if qdrantIndex != nil {
		conflictScorer = conflictScorer.WithCandidateFinder(qdrantIndex)
	}
	// External pairwise scorer override.
	if o.conflictScorer != nil {
		conflictScorer = conflictScorer.WithPairwiseScorer(&externalScorerAdapter{scorer: o.conflictScorer})
	}

	// Create decision service.
	decisionSvc := decisions.New(db, embedder, searcher, logger, conflictScorer)

	// Embedding backfills (non-fatal).
	if n, err := decisionSvc.BackfillEmbeddings(context.Background(), 500); err != nil {
		logger.Warn("embedding backfill failed", "error", err)
	} else if n > 0 {
		logger.Info("embedding backfill complete", "count", n)
	}
	if n, err := decisionSvc.BackfillOutcomeEmbeddings(context.Background(), 500); err != nil {
		logger.Warn("outcome embedding backfill failed", "error", err)
	} else if n > 0 {
		logger.Info("outcome embedding backfill complete", "count", n)
	}
	if n, err := decisionSvc.BackfillClaims(context.Background(), 500); err != nil {
		logger.Warn("claims backfill failed", "error", err)
	} else if n > 0 {
		logger.Info("claims backfill complete", "count", n)
	}

	// Force conflict rescore if configured.
	if cfg.ForceConflictRescore && conflictScorer.HasLLMValidator() {
		logger.Info("force conflict rescore requested — clearing all conflicts")
		if cleared, err := conflictScorer.ClearAllConflicts(context.Background()); err != nil {
			logger.Warn("failed to clear all conflicts for rescore", "error", err)
		} else {
			logger.Info("cleared all conflicts for rescore", "deleted", cleared)
		}
		if reset, err := db.ResetConflictScoredAt(context.Background()); err != nil {
			logger.Warn("failed to reset conflict scored_at for rescore", "error", err)
		} else if reset > 0 {
			logger.Info("reset conflict scored marks for rescore", "reset", reset)
		}
	} else if cfg.ForceConflictRescore {
		logger.Warn("AKASHI_FORCE_CONFLICT_RESCORE=true but no LLM validator configured — skipping rescore")
	}
	if conflictScorer.HasLLMValidator() && !cfg.ForceConflictRescore {
		if count, err := db.CountUnvalidatedConflicts(context.Background()); err != nil {
			logger.Warn("failed to count unvalidated conflicts", "error", err)
		} else if count > 0 {
			if cleared, err := conflictScorer.ClearUnvalidatedConflicts(context.Background()); err != nil {
				logger.Warn("failed to clear unvalidated conflicts", "error", err)
			} else {
				logger.Info("cleared unvalidated conflicts before LLM backfill", "deleted", cleared)
			}
			if reset, err := db.ResetConflictScoredAt(context.Background()); err != nil {
				logger.Warn("failed to reset conflict scored_at for LLM re-scoring", "error", err)
			} else if reset > 0 {
				logger.Info("reset conflict scored marks for LLM re-scoring", "reset", reset)
			}
		}
	}

	// Event WAL.
	var eventWAL *trace.WAL
	if cfg.WALDir != "" {
		if err := os.MkdirAll(cfg.WALDir, 0o750); err != nil {
			db.Close(context.Background())
			_ = otelShutdown(context.Background())
			return nil, fmt.Errorf("event WAL: create directory %s: %w", cfg.WALDir, err)
		}
		var walErr error
		eventWAL, walErr = trace.NewWAL(logger, trace.WALConfig{
			Dir:            cfg.WALDir,
			SyncMode:       cfg.WALSyncMode,
			SyncInterval:   cfg.WALSyncInterval,
			MaxSegmentSize: int64(cfg.WALSegmentSize),
			MaxSegmentRecs: cfg.WALSegmentRecords,
		})
		if walErr != nil {
			db.Close(context.Background())
			_ = otelShutdown(context.Background())
			return nil, fmt.Errorf("event WAL: %w", walErr)
		}
		logger.Info("write-ahead log", "enabled", true, "dir", cfg.WALDir, "sync_mode", cfg.WALSyncMode)
	} else {
		logger.Warn("write-ahead log", "enabled", false, "reason", "AKASHI_WAL_DISABLE=true",
			"risk", "buffered events will be lost on crash")
	}

	// Event buffer.
	buf := trace.NewBuffer(db, logger, cfg.EventBufferSize, cfg.EventFlushTimeout, eventWAL)

	// Grant cache.
	grantCache := authz.NewGrantCache(30 * time.Second)

	// MCP server.
	mcpSrv := mcp.New(db, decisionSvc, grantCache, logger, version)

	// SSE broker.
	var broker *server.Broker
	if db.HasNotifyConn() {
		broker = server.NewBroker(db, logger)
	} else {
		logger.Info("SSE broker: disabled (no notify connection)")
	}

	// UI filesystem.
	uiFS, err := ui.DistFS()
	if err != nil {
		db.Close(context.Background())
		_ = otelShutdown(context.Background())
		return nil, fmt.Errorf("ui: %w", err)
	}
	if uiFS != nil {
		logger.Info("ui: embedded SPA loaded")
	}

	// Rate limiter.
	var limiter ratelimit.Limiter
	if cfg.RateLimitEnabled {
		limiter = ratelimit.NewMemoryLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)
		logger.Info("rate limiting: memory (in-process token bucket)",
			"rps", cfg.RateLimitRPS, "burst", cfg.RateLimitBurst)
	} else {
		limiter = ratelimit.NoopLimiter{}
		logger.Info("rate limiting: disabled")
	}

	// Adapt event hooks from public akashi.EventHook to internal server.DecisionHook.
	var decisionHooks []server.DecisionHook
	for _, h := range o.eventHooks {
		decisionHooks = append(decisionHooks, &decisionHookAdapter{hook: h})
	}

	// Adapt route registrars from public akashi.RouteRegistrar to internal server format.
	var extraRoutes []func(*http.ServeMux, server.RoleMiddlewareFn)
	for _, fn := range o.routeRegistrars {
		fn := fn // capture
		extraRoutes = append(extraRoutes, func(mux *http.ServeMux, roleFn server.RoleMiddlewareFn) {
			fn(mux, &authHelperImpl{roleFn: roleFn})
		})
	}

	// Adapt middlewares from akashi.Middleware to func(http.Handler) http.Handler.
	var middlewares []func(http.Handler) http.Handler
	for _, mw := range o.middlewares {
		mw := mw // capture
		middlewares = append(middlewares, func(h http.Handler) http.Handler { return mw(h) })
	}

	// Create HTTP server.
	srv := server.New(server.ServerConfig{
		DB:                      db,
		JWTMgr:                  jwtMgr,
		DecisionSvc:             decisionSvc,
		Buffer:                  buf,
		Broker:                  broker,
		Searcher:                searcher,
		GrantCache:              grantCache,
		Logger:                  logger,
		Port:                    cfg.Port,
		ReadTimeout:             cfg.ReadTimeout,
		WriteTimeout:            cfg.WriteTimeout,
		MCPServer:               mcpSrv.MCPServer(),
		Version:                 version,
		MaxRequestBodyBytes:     cfg.MaxRequestBodyBytes,
		RateLimiter:             limiter,
		TrustProxy:              cfg.TrustProxy,
		CORSAllowedOrigins:      cfg.CORSAllowedOrigins,
		EnableDestructiveDelete: cfg.EnableDestructiveDelete,
		UIFS:                    uiFS,
		OpenAPISpec:             api.OpenAPISpec,
		ExtraRoutes:             extraRoutes,
		Middlewares:             middlewares,
		DecisionHooks:           decisionHooks,
	})

	// Seed admin agent.
	if err := srv.Handlers().SeedAdmin(context.Background(), cfg.AdminAPIKey); err != nil {
		db.Close(context.Background())
		_ = otelShutdown(context.Background())
		return nil, fmt.Errorf("admin seed: %w", err)
	}

	// Migrate legacy agent API keys.
	if migrated, err := db.MigrateAgentKeysToAPIKeys(context.Background()); err != nil {
		logger.Warn("api key migration failed (non-fatal, legacy keys still work)", "error", err)
	} else if migrated > 0 {
		logger.Info("migrated legacy agent keys to api_keys table", "count", migrated)
	}

	return &App{
		cfg:            cfg,
		db:             db,
		srv:            srv,
		buf:            buf,
		outbox:         outboxWorker,
		qdrantIndex:    qdrantIndex,
		grantCache:     grantCache,
		conflictScorer: conflictScorer,
		broker:         broker,
		otelShutdown:   otelShutdown,
		decisionHooks:  decisionHooks,
		logger:         logger,
		version:        version,
	}, nil
}

// Run starts all background goroutines and the HTTP server, then blocks until
// ctx is cancelled or a fatal server error occurs. On return, Shutdown is called
// automatically — callers should not call Shutdown separately.
func (a *App) Run(ctx context.Context) error {
	// Start background services.
	a.buf.Start(ctx)
	if a.outbox != nil {
		a.outbox.Start(ctx)
	}
	if a.broker != nil {
		go a.broker.Start(ctx)
	}

	// Background goroutines.
	go a.conflictBackfillLoop(ctx)
	go a.conflictRefreshLoop(ctx)
	go a.integrityProofLoop(ctx)
	go a.idempotencyCleanupLoop(ctx)

	// Start HTTP server.
	errCh := make(chan error, 1)
	go func() {
		if err := a.srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Block until signal or server error.
	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}

	return a.Shutdown(context.Background())
}

// Shutdown performs a three-phase graceful shutdown:
// (1) stop accepting HTTP requests and drain in-flight,
// (2) flush the event buffer to Postgres,
// (3) drain remaining outbox entries to Qdrant.
// It then closes the database pool and OTEL provider.
func (a *App) Shutdown(ctx context.Context) error {
	a.logger.Info("akashi shutting down")

	// Phase 1: HTTP drain.
	httpCtx, httpCancel := contextWithOptionalTimeout(ctx, a.cfg.ShutdownHTTPTimeout)
	if err := a.srv.Shutdown(httpCtx); err != nil {
		a.logger.Error("http shutdown error", "error", err)
	}
	httpCancel()

	// Phase 2: buffer drain.
	bufCtx, bufCancel := contextWithOptionalTimeout(ctx, a.cfg.ShutdownBufferDrainTimeout)
	if err := a.buf.Drain(bufCtx); err != nil {
		a.logger.Error("event buffer drain incomplete — unflushed events will be lost",
			"error", err,
			"remaining_events", a.buf.Len(),
			"configured_timeout", a.cfg.ShutdownBufferDrainTimeout,
		)
		bufCancel()
		return fmt.Errorf("buffer drain failed: %w", err)
	}
	bufCancel()

	// Phase 3: outbox drain.
	if a.outbox != nil {
		outboxCtx, outboxCancel := contextWithOptionalTimeout(ctx, a.cfg.ShutdownOutboxDrainTimeout)
		a.outbox.Drain(outboxCtx)
		outboxCancel()
	}

	// Cleanup.
	a.grantCache.Close()
	if a.qdrantIndex != nil {
		_ = a.qdrantIndex.Close()
	}
	_ = a.otelShutdown(context.Background())
	a.db.Close(context.Background())

	a.logger.Info("akashi stopped")
	return nil
}

// ── Background loops (moved from cmd/akashi/main.go) ──────────────────────────

func (a *App) conflictBackfillLoop(ctx context.Context) {
	// Warm up the Ollama model before the backfill starts. Without this the
	// first backfill request pays the full cold-start cost (model load from
	// disk), which can exceed the per-call timeout on CPU hardware.
	if v, ok := a.conflictScorer.Validator().(*conflicts.OllamaValidator); ok {
		a.logger.Info("conflict backfill: warming up ollama model")
		if err := v.Warmup(ctx); err != nil {
			a.logger.Warn("conflict backfill: ollama warmup failed (will proceed anyway)", "error", err)
		} else {
			a.logger.Info("conflict backfill: ollama model ready")
		}
	}
	if n, err := a.conflictScorer.BackfillScoring(ctx, 500); err != nil {
		a.logger.Warn("conflict scoring backfill failed", "error", err)
	} else if n > 0 {
		a.logger.Info("conflict scoring backfill complete", "decisions_scored", n)
	}
}

func (a *App) conflictRefreshLoop(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.ConflictRefreshInterval)
	defer ticker.Stop()

	lastNotifiedAt := make(map[uuid.UUID]time.Time)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			opCtx, cancel := context.WithTimeout(ctx, conflictRefreshTimeout(a.cfg.ConflictRefreshInterval))
			if err := a.db.RefreshConflicts(opCtx); err != nil {
				cancel()
				a.logger.Warn("conflict refresh failed", "error", err)
				continue
			}
			if err := a.db.RefreshAgentState(opCtx); err != nil {
				a.logger.Warn("agent state refresh failed", "error", err)
			}

			orgIDs, err := a.db.ListOrganizationIDs(opCtx)
			if err != nil {
				cancel()
				a.logger.Warn("conflict org list failed", "error", err)
				continue
			}

			var totalNotified int
			for _, orgID := range orgIDs {
				since, ok := lastNotifiedAt[orgID]
				if !ok {
					since = time.Now().UTC()
					lastNotifiedAt[orgID] = since
				}

				newConflicts, err := a.db.NewConflictsSinceByOrg(opCtx, orgID, since, 1000)
				if err != nil {
					a.logger.Warn("new conflicts query failed", "error", err, "org_id", orgID)
					continue
				}

				for _, c := range newConflicts {
					// Fire OnConflictDetected hooks asynchronously.
					if len(a.decisionHooks) > 0 {
						conflict := c
						hooks := a.decisionHooks
						logger := a.logger
						go func() {
							hookCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							defer cancel()
							for _, h := range hooks {
								if err := h.OnConflictDetected(hookCtx, conflict); err != nil {
									logger.Warn("event hook OnConflictDetected failed", "error", err)
								}
							}
						}()
					}

					payload, err := json.Marshal(map[string]any{
						"org_id":        c.OrgID,
						"conflict_kind": c.ConflictKind,
						"decision_a_id": c.DecisionAID,
						"decision_b_id": c.DecisionBID,
						"agent_a":       c.AgentA,
						"agent_b":       c.AgentB,
						"decision_type": c.DecisionType,
					})
					if err != nil {
						a.logger.Warn("conflict notify marshal failed", "error", err)
						continue
					}
					if err := a.db.Notify(opCtx, storage.ChannelConflicts, string(payload)); err != nil {
						a.logger.Warn("conflict notify failed", "error", err)
					}
					if c.DetectedAt.After(lastNotifiedAt[orgID]) {
						lastNotifiedAt[orgID] = c.DetectedAt
					}
					totalNotified++
				}
			}
			cancel()

			if totalNotified > 0 {
				a.logger.Info("conflict notifications sent", "count", totalNotified)
			}
		}
	}
}

func (a *App) integrityProofLoop(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.IntegrityProofInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			opCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			buildIntegrityProofs(opCtx, a.db, a.logger)
			if hasNull, err := a.db.HasDecisionsWithNullSearchVector(opCtx); err == nil && hasNull {
				a.logger.Warn("decisions with NULL search_vector detected — FTS excludes these rows; check trigger and migration 022 backfill")
			}
			cancel()
		}
	}
}

func (a *App) idempotencyCleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.IdempotencyCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			deleted, err := a.db.CleanupIdempotencyKeys(opCtx, a.cfg.IdempotencyCompletedTTL, a.cfg.IdempotencyAbandonedTTL)
			cancel()
			if err != nil {
				a.logger.Warn("idempotency cleanup failed", "error", err)
				continue
			}
			if deleted > 0 {
				a.logger.Info("idempotency cleanup deleted rows", "deleted", deleted)
			}
		}
	}
}

// ── Adapters (defined here because this file imports both sides) ───────────────

// decisionHookAdapter wraps an akashi.EventHook to satisfy server.DecisionHook.
// It converts internal model types to public akashi types at the boundary.
type decisionHookAdapter struct {
	hook EventHook
}

func (a *decisionHookAdapter) OnDecisionTraced(ctx context.Context, d model.Decision) error {
	return a.hook.OnDecisionTraced(ctx, toPublicDecision(d))
}

func (a *decisionHookAdapter) OnConflictDetected(ctx context.Context, c model.DecisionConflict) error {
	return a.hook.OnConflictDetected(ctx, toPublicConflict(c))
}

// externalScorerAdapter wraps an akashi.ConflictScorer to satisfy conflicts.PairwiseScorer.
type externalScorerAdapter struct {
	scorer ConflictScorer
}

func (a *externalScorerAdapter) ScorePair(ctx context.Context, da, db model.Decision) (float32, string, error) {
	result, err := a.scorer.Score(ctx, toPublicDecision(da), toPublicDecision(db))
	if err != nil {
		return 0, "", err
	}
	return result.Score, result.Explanation, nil
}

// searcherAdapter wraps an akashi.Searcher to satisfy search.Searcher.
// Converts between public SearchFilters/SearchResult and internal model types.
type searcherAdapter struct {
	s Searcher
}

func (a *searcherAdapter) Search(ctx context.Context, orgID uuid.UUID, emb []float32, filters model.QueryFilters, limit int) ([]search.Result, error) {
	pubFilters := SearchFilters{
		AgentIDs:      filters.AgentIDs,
		DecisionType:  filters.DecisionType,
		ConfidenceMin: filters.ConfidenceMin,
		SessionID:     filters.SessionID,
		Tool:          filters.Tool,
		Model:         filters.Model,
		Repo:          filters.Repo,
	}
	results, err := a.s.Search(ctx, orgID, emb, pubFilters, limit)
	if err != nil {
		return nil, err
	}
	out := make([]search.Result, len(results))
	for i, r := range results {
		out[i] = search.Result{DecisionID: r.DecisionID, Score: r.Score}
	}
	return out, nil
}

func (a *searcherAdapter) Healthy(ctx context.Context) error {
	return a.s.Healthy(ctx)
}

// authHelperImpl implements akashi.AuthHelper using an internal server.RoleMiddlewareFn.
// Constructed in the route registrar adapter closure; bridges the public interface
// to the internal RBAC middleware without importing server from enterprise code.
type authHelperImpl struct {
	roleFn server.RoleMiddlewareFn
}

func (a *authHelperImpl) RequireRole(role Role) func(http.Handler) http.Handler {
	return a.roleFn(model.AgentRole(role))
}

// ── Type converters ────────────────────────────────────────────────────────────

// toPublicDecision converts an internal model.Decision to the public akashi.Decision.
// Lives here because this is the only file that imports both sides of the boundary.
func toPublicDecision(d model.Decision) Decision {
	return Decision{
		ID:           d.ID,
		OrgID:        d.OrgID,
		AgentID:      d.AgentID,
		DecisionType: d.DecisionType,
		Outcome:      d.Outcome,
		Reasoning:    d.Reasoning,
		Confidence:   d.Confidence,
		CreatedAt:    d.ValidFrom,
		PrecedentRef: d.PrecedentRef,
		SessionID:    d.SessionID,
		AgentContext: d.AgentContext,
		Metadata:     d.Metadata,
	}
}

// toPublicConflict converts an internal model.DecisionConflict to the public akashi.Conflict.
func toPublicConflict(c model.DecisionConflict) Conflict {
	score := float32(0)
	if c.Significance != nil {
		score = float32(*c.Significance)
	}
	explanation := ""
	if c.Explanation != nil {
		explanation = *c.Explanation
	}
	category := ""
	if c.Category != nil {
		category = *c.Category
	}
	severity := ""
	if c.Severity != nil {
		severity = *c.Severity
	}
	return Conflict{
		ID:           c.ID,
		OrgID:        c.OrgID,
		DecisionAID:  c.DecisionAID,
		DecisionBID:  c.DecisionBID,
		AgentA:       c.AgentA,
		AgentB:       c.AgentB,
		DecisionType: c.DecisionType,
		Score:        score,
		Explanation:  explanation,
		Category:     category,
		Severity:     severity,
		Status:       c.Status,
		DetectedAt:   c.DetectedAt,
	}
}

// ── Helpers (moved from cmd/akashi/main.go) ────────────────────────────────────

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

func newConflictValidator(cfg config.Config, logger *slog.Logger) conflicts.Validator {
	if cfg.ConflictLLMModel != "" {
		logger.Info("conflict validator: ollama", "model", cfg.ConflictLLMModel, "url", cfg.OllamaURL, "num_threads", cfg.ConflictLLMThreads)
		return conflicts.NewOllamaValidator(cfg.OllamaURL, cfg.ConflictLLMModel, cfg.ConflictLLMThreads)
	}
	if cfg.OpenAIAPIKey != "" {
		logger.Info("conflict validator: openai (gpt-4o-mini)")
		return conflicts.NewOpenAIValidator(cfg.OpenAIAPIKey, "gpt-4o-mini")
	}
	logger.Info("conflict validator: noop (no LLM configured, embedding-only conflicts)")
	return conflicts.NoopValidator{}
}

func ollamaReachable(baseURL string) bool {
	c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, baseURL+"/api/tags", nil)
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

func buildIntegrityProofs(ctx context.Context, db *storage.DB, logger *slog.Logger) {
	orgIDs, err := db.ListOrganizationIDs(ctx)
	if err != nil {
		logger.Warn("integrity proof: list orgs failed", "error", err)
		return
	}

	now := time.Now().UTC()

	for _, orgID := range orgIDs {
		latest, err := db.GetLatestIntegrityProof(ctx, orgID)
		if err != nil {
			logger.Warn("integrity proof: get latest failed", "error", err, "org_id", orgID)
			continue
		}

		batchStart := time.Time{}
		var previousRoot *string
		if latest != nil {
			batchStart = latest.BatchEnd
			previousRoot = &latest.RootHash
		}

		hashes, err := db.GetDecisionHashesForBatch(ctx, orgID, batchStart, now)
		if err != nil {
			logger.Warn("integrity proof: get hashes failed", "error", err, "org_id", orgID)
			continue
		}
		if len(hashes) == 0 {
			continue
		}

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

func contextWithOptionalTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

func conflictRefreshTimeout(interval time.Duration) time.Duration {
	const max = 15 * time.Second
	if interval < max {
		return interval
	}
	return max
}

// ctx is a no-op helper so that New(opts ...) can pass a background context to
// telemetry.Init without adding a context parameter to the public API.
// The returned context is never cancelled by this function.
func ctx(_ []Option) context.Context { return context.Background() }
