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
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
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
	"github.com/ashita-ai/akashi/internal/service/autoresolve"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/service/quality"
	"github.com/ashita-ai/akashi/internal/service/trace"
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/internal/telemetry"
	"github.com/ashita-ai/akashi/migrations"
	"github.com/ashita-ai/akashi/ui"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// App is the Akashi server lifecycle. Construct with New(), run with Run().
// App has no public fields — use New() options to configure it.
type App struct {
	cfg             config.Config
	db              *storage.DB
	srv             *server.Server
	buf             *trace.Buffer
	outbox          *search.OutboxWorker
	qdrantIndex     *search.QdrantIndex // nil when Qdrant is not configured
	grantCache      *authz.GrantCache
	conflictScorer  *conflicts.Scorer
	decisionSvc     *decisions.Service
	percentileCache *search.PercentileCache
	broker          *server.Broker // nil when no notify connection
	otelShutdown    func(context.Context) error
	limiter         ratelimit.Limiter // rate limiter; closed on shutdown to stop cleanup goroutine
	decisionHooks   []server.DecisionHook
	logger          *slog.Logger
	autoResolver    *autoresolve.Service
	version         string
	auditOrgCounter atomic.Uint64 // round-robin counter for integrity audit org selection
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
	// Log embedding model profile selection.
	_, _, _, knownProfile := config.EmbeddingModelThresholds(cfg.EmbeddingModelProfile)
	if knownProfile {
		logger.Info("conflict scoring: using model profile",
			"model", cfg.EmbeddingModelProfile,
			"profile", cfg.ConflictProfile,
			"claim_topic_sim", cfg.ConflictClaimTopicSimFloor,
			"claim_div", cfg.ConflictClaimDivFloor,
			"decision_topic_sim", cfg.ConflictDecisionTopicSimFloor)
	} else {
		logger.Warn("conflict scoring: unknown embedding model, using mxbai-embed-large defaults",
			"model", cfg.EmbeddingModelProfile,
			"hint", "run 'go run ./cmd/eval-conflicts --mode=benchmark' to calibrate thresholds")
	}

	conflictScorer := conflicts.NewScorer(db, logger, cfg.ConflictSignificanceThreshold, conflictValidator, backfillWorkers, cfg.ConflictDecayLambda).
		WithScoringThresholds(cfg.ConflictClaimTopicSimFloor, cfg.ConflictClaimDivFloor, cfg.ConflictDecisionTopicSimFloor).
		WithCandidateLimit(cfg.ConflictCandidateLimit).
		WithEarlyExitFloor(cfg.ConflictEarlyExitFloor)
	if qdrantIndex != nil {
		conflictScorer = conflictScorer.WithCandidateFinder(qdrantIndex)
	}
	// NLI or cross-encoder reranking (optional, reduces LLM calls).
	// NLI sidecar takes precedence — it uses a purpose-built stance detection
	// model (DeBERTa-v3-base) that outperforms generic cross-encoders on
	// entailment/contradiction classification.
	if cfg.NLIURL != "" {
		nliEncoder := conflicts.NewHTTPCrossEncoder(cfg.NLIURL)
		conflictScorer = conflictScorer.WithCrossEncoder(nliEncoder, cfg.CrossEncoderThreshold)
		logger.Info("conflict pre-filter: NLI sidecar enabled", "url", cfg.NLIURL, "threshold", cfg.CrossEncoderThreshold)
	} else if cfg.CrossEncoderURL != "" {
		crossEnc := conflicts.NewHTTPCrossEncoder(cfg.CrossEncoderURL)
		conflictScorer = conflictScorer.WithCrossEncoder(crossEnc, cfg.CrossEncoderThreshold)
		logger.Info("conflict pre-filter: cross-encoder enabled", "url", cfg.CrossEncoderURL, "threshold", cfg.CrossEncoderThreshold)
	}
	// External pairwise scorer override.
	if o.conflictScorer != nil {
		conflictScorer = conflictScorer.WithPairwiseScorer(&externalScorerAdapter{scorer: o.conflictScorer})
	}

	// Create decision service.
	decisionSvc := decisions.New(db, embedder, searcher, logger, conflictScorer)
	if extractor := newClaimExtractor(cfg, logger); extractor != nil {
		decisionSvc.SetClaimExtractor(extractor)
	}
	rescoreMetrics := search.RegisterReScoreMetrics(logger)
	decisionSvc.SetReScoreMetrics(rescoreMetrics)
	// PercentileCache is wired after App construction in Run() since it needs
	// the cache instance stored on App. Set here so it's available from the first search.
	pctCache := search.NewPercentileCache()
	decisionSvc.SetPercentileCache(pctCache)

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
	mcpSrv := mcp.New(db, decisionSvc, grantCache, logger, version, cfg.HighConfidenceWarnThreshold, quality.BuildStandardTypes(cfg.StandardDecisionTypes))

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
		DB:                          db,
		JWTMgr:                      jwtMgr,
		DecisionSvc:                 decisionSvc,
		Buffer:                      buf,
		Broker:                      broker,
		Searcher:                    searcher,
		GrantCache:                  grantCache,
		Logger:                      logger,
		Port:                        cfg.Port,
		ReadTimeout:                 cfg.ReadTimeout,
		WriteTimeout:                cfg.WriteTimeout,
		MCPServer:                   mcpSrv.MCPServer(),
		Version:                     version,
		MaxRequestBodyBytes:         cfg.MaxRequestBodyBytes,
		RateLimiter:                 limiter,
		TrustProxy:                  cfg.TrustProxy,
		CORSAllowedOrigins:          cfg.CORSAllowedOrigins,
		EnableDestructiveDelete:     cfg.EnableDestructiveDelete,
		RetentionInterval:           cfg.RetentionInterval,
		UIFS:                        uiFS,
		OpenAPISpec:                 api.OpenAPISpec,
		ExtraRoutes:                 extraRoutes,
		Middlewares:                 middlewares,
		DecisionHooks:               decisionHooks,
		HooksEnabled:                cfg.HooksEnabled,
		HooksAPIKey:                 cfg.HooksAPIKey,
		AutoTrace:                   cfg.AutoTrace,
		SignupEnabled:               cfg.SignupEnabled,
		ResolutionRecorder:          conflictScorer,
		ConflictValidator:           conflictValidator,
		HighConfidenceWarnThreshold: cfg.HighConfidenceWarnThreshold,
	})

	// Wire akashi_check → IDE hook gate.
	// The MCP handleCheck calls srv.Handlers().NotifyCheckCalled() so the
	// PreToolUse hook knows a check was performed and allows edits.
	mcpSrv.SetCheckNotify(srv.Handlers().NotifyCheckCalled)

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
		cfg:             cfg,
		db:              db,
		srv:             srv,
		buf:             buf,
		outbox:          outboxWorker,
		qdrantIndex:     qdrantIndex,
		grantCache:      grantCache,
		conflictScorer:  conflictScorer,
		decisionSvc:     decisionSvc,
		percentileCache: pctCache,
		broker:          broker,
		otelShutdown:    otelShutdown,
		limiter:         limiter,
		decisionHooks:   decisionHooks,
		logger:          logger,
		autoResolver:    autoresolve.New(db, logger),
		version:         version,
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
	go a.integrityAuditLoop(ctx)
	go a.idempotencyCleanupLoop(ctx)
	go a.hookCheckCleanupLoop(ctx)
	go a.retentionLoop(ctx)
	go a.claimEmbeddingRetryLoop(ctx)
	go a.percentileRefreshLoop(ctx)
	go a.autoResolveLoop(ctx)

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

	// Phase 1.5: wait for in-flight post-trace async work (claim generation,
	// conflict scoring) so goroutines finish their DB writes before pool close.
	asyncCtx, asyncCancel := contextWithOptionalTimeout(ctx, a.cfg.ShutdownAsyncDrainTimeout)
	if err := a.decisionSvc.DrainAsync(asyncCtx); err != nil {
		a.logger.Warn("async post-trace drain incomplete — some claims or conflict scores may be missing",
			"error", err,
			"configured_timeout", a.cfg.ShutdownAsyncDrainTimeout,
		)
	}
	asyncCancel()

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
		if outboxCtx.Err() != nil {
			a.logger.Error("search outbox drain did not complete within timeout — Qdrant index may be stale",
				"error", outboxCtx.Err(),
				"configured_timeout", a.cfg.ShutdownOutboxDrainTimeout,
			)
		}
		outboxCancel()
	}

	// Cleanup.
	a.grantCache.Close()
	if a.limiter != nil {
		_ = a.limiter.Close()
	}
	a.srv.CloseSignupLimiter()
	if a.qdrantIndex != nil {
		_ = a.qdrantIndex.Close()
	}
	_ = a.otelShutdown(ctx)
	a.db.Close(ctx)

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

func (a *App) integrityAuditLoop(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.IntegrityAuditInterval)
	defer ticker.Stop()

	// Cap each audit at 80% of the tick interval so a slow audit cannot
	// overlap the next tick. Floor at 1 minute to give small intervals
	// enough time to complete at least one org's proofs.
	timeout := a.cfg.IntegrityAuditInterval * 4 / 5
	if timeout < time.Minute {
		timeout = time.Minute
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			opCtx, cancel := context.WithTimeout(ctx, timeout)
			a.auditIntegrityProofs(opCtx)
			cancel()
		}
	}
}

// OTEL meter and counters for the integrity audit loop.
var (
	auditMeter           = otel.GetMeterProvider().Meter("akashi/integrity")
	auditViolationsTotal metric.Int64Counter
	auditProofsChecked   metric.Int64Counter
	auditOrgsChecked     metric.Int64Counter
)

func init() {
	var err error
	auditViolationsTotal, err = auditMeter.Int64Counter("akashi.integrity.violations_total",
		metric.WithDescription("Total integrity violations detected by the background audit loop"))
	if err != nil {
		panic("integrity metrics: " + err.Error())
	}
	auditProofsChecked, err = auditMeter.Int64Counter("akashi.integrity.proofs_checked_total",
		metric.WithDescription("Total Merkle proofs verified by the background audit loop"))
	if err != nil {
		panic("integrity metrics: " + err.Error())
	}
	auditOrgsChecked, err = auditMeter.Int64Counter("akashi.integrity.orgs_audited_total",
		metric.WithDescription("Total org audit cycles completed"))
	if err != nil {
		panic("integrity metrics: " + err.Error())
	}
}

// auditIntegrityProofs picks one org per tick (round-robin) and verifies its
// most recent Merkle proofs by:
// (1) recomputing each root from decision content hashes, and
// (2) checking chain linkage via previous_root.
//
// Coverage is deterministic: with N orgs, each org is audited roughly every
// N * IntegrityAuditInterval. Only the 10 newest proofs per org are checked.
//
// Violations are both logged and persisted to integrity_violations — logs are
// for alerting, the table is the durable paper trail.
func (a *App) auditIntegrityProofs(ctx context.Context) {
	// Use offset-based selection instead of loading the full org table.
	// CountOrganizations is a cheap count(*) on the PK index.
	orgCount, err := a.db.CountOrganizations(ctx)
	if err != nil {
		a.logger.Warn("integrity audit: count orgs failed", "error", err)
		return
	}
	if orgCount == 0 {
		return
	}

	// Round-robin through orgs using atomic counter + offset.
	idx := a.auditOrgCounter.Add(1) - 1
	offset := int(idx % uint64(orgCount)) //nolint:gosec // orgCount is validated positive above; modulo result fits int
	orgID, err := a.db.GetOrgIDByOffset(ctx, offset)
	if err != nil {
		a.logger.Warn("integrity audit: get org by offset failed", "offset", offset, "error", err)
		return
	}
	if orgID == uuid.Nil {
		// Org count changed between count and offset query — skip this tick.
		return
	}

	proofs, err := a.db.GetRecentIntegrityProofs(ctx, orgID, 10)
	if err != nil {
		a.logger.Warn("integrity audit: failed to fetch proofs", "org_id", orgID, "error", err)
		return
	}
	if len(proofs) == 0 {
		return
	}

	violations := 0
	for i, p := range proofs {
		// Verify Merkle root.
		hashes, err := a.db.GetDecisionHashesForBatch(ctx, orgID, p.BatchStart, p.BatchEnd)
		if err != nil {
			a.logger.Warn("integrity audit: failed to fetch hashes for batch",
				"org_id", orgID, "proof_id", p.ID, "error", err)
			continue
		}

		recomputedRoot, err := integrity.BuildMerkleRoot(hashes)
		if err != nil {
			a.logger.Error("integrity audit: merkle recomputation error",
				"org_id", orgID, "proof_id", p.ID, "error", err)
			continue
		}
		if recomputedRoot != p.RootHash {
			a.logger.Error("INTEGRITY VIOLATION: Merkle root mismatch — stored root does not match recomputed root",
				"org_id", orgID, "proof_id", p.ID,
				"stored_root", p.RootHash, "recomputed_root", recomputedRoot, "decision_count", p.DecisionCount)
			violations++
			if dbErr := a.db.CreateIntegrityViolation(ctx, storage.IntegrityViolation{
				OrgID:         orgID,
				ProofID:       p.ID,
				ViolationType: "merkle_mismatch",
				Expected:      p.RootHash,
				Actual:        recomputedRoot,
			}); dbErr != nil {
				a.logger.Error("integrity audit: FAILED TO PERSIST VIOLATION — evidence may be lost",
					"org_id", orgID, "proof_id", p.ID, "error", dbErr)
			}
		}

		// Verify chain linkage: this proof's previous_root should match the
		// next-older proof's root_hash (proofs are newest-first).
		if i+1 < len(proofs) {
			older := proofs[i+1]
			if p.PreviousRoot == nil {
				a.logger.Warn("integrity audit: proof has nil previous_root but older proof exists — chain may be broken",
					"org_id", orgID, "proof_id", p.ID, "older_proof_id", older.ID)
				violations++
				if dbErr := a.db.CreateIntegrityViolation(ctx, storage.IntegrityViolation{
					OrgID:         orgID,
					ProofID:       p.ID,
					ViolationType: "nil_previous_root",
					Expected:      older.RootHash,
					Actual:        "<nil>",
				}); dbErr != nil {
					a.logger.Error("integrity audit: FAILED TO PERSIST VIOLATION — evidence may be lost",
						"org_id", orgID, "proof_id", p.ID, "error", dbErr)
				}
			} else if *p.PreviousRoot != older.RootHash {
				a.logger.Error("INTEGRITY VIOLATION: chain linkage broken — previous_root does not match prior proof",
					"org_id", orgID, "proof_id", p.ID,
					"expected_previous", older.RootHash, "actual_previous", *p.PreviousRoot)
				violations++
				if dbErr := a.db.CreateIntegrityViolation(ctx, storage.IntegrityViolation{
					OrgID:         orgID,
					ProofID:       p.ID,
					ViolationType: "chain_break",
					Expected:      older.RootHash,
					Actual:        *p.PreviousRoot,
				}); dbErr != nil {
					a.logger.Error("integrity audit: FAILED TO PERSIST VIOLATION — evidence may be lost",
						"org_id", orgID, "proof_id", p.ID, "error", dbErr)
				}
			}
		}
	}

	auditProofsChecked.Add(ctx, int64(len(proofs)))
	auditOrgsChecked.Add(ctx, 1)
	if violations > 0 {
		auditViolationsTotal.Add(ctx, int64(violations))
	}
	a.logger.Info("integrity audit completed", "org_id", orgID, "proofs_checked", len(proofs), "violations", violations)
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

func (a *App) hookCheckCleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.srv.Handlers().CleanupHookChecks()
		}
	}
}

func (a *App) retentionLoop(ctx context.Context) {
	if a.cfg.RetentionInterval <= 0 {
		return
	}
	ticker := time.NewTicker(a.cfg.RetentionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.runRetention(ctx)
		}
	}
}

func (a *App) claimEmbeddingRetryLoop(ctx context.Context) {
	if a.cfg.ClaimRetryInterval <= 0 {
		return
	}
	ticker := time.NewTicker(a.cfg.ClaimRetryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			opCtx, cancel := context.WithTimeout(ctx, a.cfg.ClaimRetryInterval)
			n, err := a.decisionSvc.RetryFailedClaimEmbeddings(opCtx, 50, 3)
			cancel()
			if err != nil {
				a.logger.Warn("claim embedding retry failed", "error", err)
			} else if n > 0 {
				a.logger.Info("claim embedding retry complete", "retried", n)
			}
		}
	}
}

// percentileRefreshLoop periodically recomputes signal percentile breakpoints for all orgs
// and stores them in the in-memory cache. ReScore uses these to normalize citation counts
// into distribution-aware [0,1] scores instead of using the hardcoded log saturation formula.
func (a *App) percentileRefreshLoop(ctx context.Context) {
	if a.cfg.PercentileRefreshInterval <= 0 {
		return
	}

	// Refresh immediately on startup to populate the cache before the first search.
	a.refreshPercentiles(ctx)

	ticker := time.NewTicker(a.cfg.PercentileRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.refreshPercentiles(ctx)
		}
	}
}

func (a *App) refreshPercentiles(ctx context.Context) {
	opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	orgIDs, err := a.db.ListOrganizationIDs(opCtx)
	if err != nil {
		a.logger.Warn("percentile refresh: list orgs", "error", err)
		return
	}

	refreshed := 0
	for _, orgID := range orgIDs {
		bp, err := a.db.GetCitationPercentilesForOrg(opCtx, orgID)
		if err != nil {
			a.logger.Warn("percentile refresh: org query failed", "org_id", orgID, "error", err)
			continue
		}
		a.percentileCache.Set(orgID, search.OrgPercentiles{
			CitationBreakpoints: bp,
			RefreshedAt:         time.Now(),
		})
		refreshed++
	}

	if refreshed > 0 {
		a.logger.Debug("percentile refresh complete", "orgs", refreshed)
	}
}

// autoResolveLoop periodically runs auto-resolution of conflicts based on org policies.
func (a *App) autoResolveLoop(ctx context.Context) {
	if a.cfg.AutoResolveInterval <= 0 {
		return
	}
	ticker := time.NewTicker(a.cfg.AutoResolveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.autoResolver.RunOnce(ctx); err != nil {
				a.logger.Warn("auto-resolve loop failed", "error", err)
			}
		}
	}
}

// runRetention processes data retention policies for all orgs that have a
// retention_days set. Each org gets its own deletion_log entry.
func (a *App) runRetention(ctx context.Context) {
	opCtx, cancel := context.WithTimeout(ctx, a.cfg.RetentionInterval/2)
	defer cancel()

	orgs, err := a.db.GetOrgsWithRetention(opCtx)
	if err != nil {
		a.logger.Warn("retention: failed to list orgs with policy", "error", err)
		return
	}
	if len(orgs) == 0 {
		return
	}

	for _, org := range orgs {
		cutoff := time.Now().UTC().AddDate(0, 0, -org.RetentionDays)
		criteria := map[string]any{"before": cutoff, "retention_days": org.RetentionDays}
		if len(org.RetentionExcludeTypes) > 0 {
			criteria["exclude_types"] = org.RetentionExcludeTypes
		}

		logID, err := a.db.StartDeletionLog(opCtx, org.OrgID, "policy", "", criteria)
		if err != nil {
			a.logger.Warn("retention: failed to start deletion log", "org_id", org.OrgID, "error", err)
			continue
		}

		counts, err := a.db.BatchDeleteDecisions(opCtx, org.OrgID, cutoff, nil, nil, org.RetentionExcludeTypes, 1000)
		if err != nil {
			a.logger.Warn("retention: batch delete failed", "org_id", org.OrgID, "error", err)
			// Still complete the log even on partial failure so the run is recorded.
		}

		countMap := map[string]any{
			"decisions":    counts.Decisions,
			"alternatives": counts.Alternatives,
			"evidence":     counts.Evidence,
			"claims":       counts.Claims,
			"events":       counts.Events,
		}
		if cerr := a.db.CompleteDeletionLog(opCtx, logID, countMap); cerr != nil {
			a.logger.Warn("retention: failed to complete deletion log", "org_id", org.OrgID, "error", cerr)
		}

		if counts.Decisions > 0 || counts.Events > 0 {
			a.logger.Info("retention: deleted stale records",
				"org_id", org.OrgID,
				"decisions", counts.Decisions,
				"events", counts.Events,
				"cutoff", cutoff,
			)
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
		Project:       filters.Project,
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

// newClaimExtractor creates an LLM-backed claim extractor when configured.
// Returns nil when LLM claim extraction is disabled or no LLM is available.
func newClaimExtractor(cfg config.Config, logger *slog.Logger) conflicts.ClaimExtractor {
	if !cfg.ClaimExtractionLLM {
		return nil
	}
	if cfg.ConflictLLMModel != "" {
		logger.Info("claim extractor: ollama", "model", cfg.ConflictLLMModel, "url", cfg.OllamaURL)
		return conflicts.NewOllamaExtractor(cfg.OllamaURL, cfg.ConflictLLMModel, cfg.ConflictLLMThreads)
	}
	if cfg.OpenAIAPIKey != "" {
		logger.Info("claim extractor: openai (gpt-4o-mini)")
		return conflicts.NewOpenAIExtractor(cfg.OpenAIAPIKey, "gpt-4o-mini")
	}
	logger.Warn("claim extraction LLM requested but no LLM configured, using regex fallback")
	return nil
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
	_, _ = io.Copy(io.Discard, resp.Body)
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

		root, err := integrity.BuildMerkleRoot(hashes)
		if err != nil {
			logger.Warn("integrity proof: merkle root construction failed", "error", err, "org_id", orgID)
			continue
		}

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
