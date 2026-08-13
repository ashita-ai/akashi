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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joho/godotenv"
	otelmetric "go.opentelemetry.io/otel/metric"

	"github.com/ashita-ai/akashi/api"
	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/authz"
	"github.com/ashita-ai/akashi/internal/config"
	"github.com/ashita-ai/akashi/internal/conflicts"
	"github.com/ashita-ai/akashi/internal/mcp"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/ratelimit"
	"github.com/ashita-ai/akashi/internal/search"
	"github.com/ashita-ai/akashi/internal/server"
	"github.com/ashita-ai/akashi/internal/service/autoassess"
	"github.com/ashita-ai/akashi/internal/service/autoresolve"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/service/pendingassess"
	"github.com/ashita-ai/akashi/internal/service/quality"
	"github.com/ashita-ai/akashi/internal/service/trace"
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/internal/telemetry"
	"github.com/ashita-ai/akashi/migrations"
	"github.com/ashita-ai/akashi/ui"
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

	bgLoops sync.WaitGroup // tracks background goroutines for graceful shutdown

	auditOrgCounter atomic.Uint64 // round-robin counter for integrity audit org selection

	// OTEL metrics for integrity audit violations. Incremented by
	// verifyProofsForOrg on Merkle root mismatch or chain linkage failure.
	integrityViolations otelmetric.Int64Counter
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
	otelShutdown, err := telemetry.Init(ctx(opts), cfg.OTELEndpoint, cfg.ServiceName, version, cfg.OTELInsecure, cfg.OTELSampleRate)
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}

	// Initialize integrity violation counter.
	meter := telemetry.Meter("akashi/integrity")
	integrityViolations, err := meter.Int64Counter("akashi.integrity.violations",
		otelmetric.WithDescription("Count of integrity audit failures (Merkle root mismatch or chain linkage broken)"),
	)
	if err != nil {
		logger.Warn("failed to create integrity violations counter", "error", err)
		integrityViolations, _ = meter.Int64Counter("akashi.integrity.violations")
	}

	// Connect to database.
	db, err := storage.New(context.Background(), cfg.DatabaseURL, cfg.NotifyURL, logger, storage.PoolOptions{
		MaxConns: cfg.DBMaxConns,
		MinConns: cfg.DBMinConns,
	})
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
			APIKey:     cfg.QdrantAPIKey.Value(),
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
		WithEarlyExitFloor(cfg.ConflictEarlyExitFloor).
		WithOutcomeSimFloor(cfg.ConflictOutcomeSimFloor).
		WithSuppressionSampleRate(cfg.ConflictSuppressionSampleRate).
		WithBackfillWindow(cfg.ConflictBackfillWindow)
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

	// Auto-assessor: generates assessments from observable signals (supersession,
	// conflict resolution, citation threshold). Wired into both the decision
	// service (for trace-time signals) and the MCP server (for resolve-time signals).
	assessor := autoassess.New(db, logger)
	decisionSvc.SetAutoAssessor(assessor)

	// Completeness ingest gate (#715). Disabled by default; operators opt in
	// via AKASHI_MIN_COMPLETENESS_MODE=warn|reject. The mode string was
	// validated at config load, so ParseGateMode cannot return an error here.
	gateMode, _ := quality.ParseGateMode(cfg.MinCompletenessMode)
	decisionSvc.SetCompletenessGate(quality.CompletenessGate{
		Mode:      gateMode,
		Threshold: cfg.MinCompleteness,
		ByType:    cfg.MinCompletenessByType,
	})
	if gateMode != quality.GateModeOff {
		logger.Info("completeness ingest gate enabled",
			"mode", string(gateMode),
			"threshold", cfg.MinCompleteness,
			"by_type_count", len(cfg.MinCompletenessByType),
		)
	}

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

	// Outcome-assessment prompt service (issue #716).
	pendingAssess := pendingassess.New(db, cfg.AssessmentWindows, cfg.AssessmentPromptLimit)

	// MCP server.
	mcpSrv := mcp.New(db, decisionSvc, grantCache, logger, version, cfg.HighConfidenceWarnThreshold, quality.BuildStandardTypes(cfg.StandardDecisionTypes))
	mcpSrv.SetAutoAssessor(assessor)
	mcpSrv.SetPendingAssessor(pendingAssess)

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
		HooksAPIKey:                 cfg.HooksAPIKey.Value(),
		AutoTrace:                   cfg.AutoTrace,
		SignupEnabled:               cfg.SignupEnabled,
		ResolutionRecorder:          conflictScorer,
		ConflictValidator:           conflictValidator,
		HighConfidenceWarnThreshold: cfg.HighConfidenceWarnThreshold,
		ExportPageSize:              cfg.ExportPageSize,
		PendingAssessSvc:            pendingAssess,
	})

	// Wire akashi_check → IDE hook gate.
	// The MCP handleCheck calls srv.Handlers().NotifyCheckCalled() so the
	// PreToolUse hook knows a check was performed and allows edits.
	mcpSrv.SetCheckNotify(srv.Handlers().NotifyCheckCalled)

	// Wire akashi_trace outcomes → IDE hook gate. On rejection, the
	// post-commit hook surfaces a warning so the agent can't silently
	// commit after a failed trace; on success, any prior rejection
	// marker is cleared.
	mcpSrv.SetTraceCompleteNotify(srv.Handlers().NotifyTraceComplete)

	// Seed admin agent.
	if err := srv.Handlers().SeedAdmin(context.Background(), cfg.AdminAPIKey.Value()); err != nil {
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
		cfg:                 cfg,
		db:                  db,
		srv:                 srv,
		buf:                 buf,
		outbox:              outboxWorker,
		qdrantIndex:         qdrantIndex,
		grantCache:          grantCache,
		conflictScorer:      conflictScorer,
		decisionSvc:         decisionSvc,
		percentileCache:     pctCache,
		broker:              broker,
		otelShutdown:        otelShutdown,
		limiter:             limiter,
		decisionHooks:       decisionHooks,
		logger:              logger,
		autoResolver:        autoresolve.New(db, logger),
		version:             version,
		integrityViolations: integrityViolations,
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
		a.bgLoops.Add(1)
		go func() {
			defer a.bgLoops.Done()
			a.broker.Start(ctx)
		}()
	}

	// Background goroutines — all tracked by bgLoops so Shutdown can wait
	// for them to exit before closing the database pool.
	for _, fn := range []func(context.Context){
		a.conflictBackfillLoop,
		a.conflictRefreshLoop,
		a.integrityProofLoop,
		a.integrityAuditLoop,
		a.integrityFullAuditLoop,
		a.idempotencyCleanupLoop,
		a.hookCheckCleanupLoop,
		a.retentionLoop,
		a.claimEmbeddingRetryLoop,
		a.percentileRefreshLoop,
		a.autoResolveLoop,
	} {
		a.bgLoops.Add(1)
		go func() {
			defer a.bgLoops.Done()
			fn(ctx)
		}()
	}

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

	// Wait for background loops to exit. The Run() context was cancelled
	// before Shutdown was called, so loops are draining. We bound the wait
	// to avoid hanging on a stuck goroutine.
	bgDone := make(chan struct{})
	go func() { a.bgLoops.Wait(); close(bgDone) }()
	loopCtx, loopCancel := contextWithOptionalTimeout(ctx, a.cfg.ShutdownLoopDrainTimeout)
	select {
	case <-bgDone:
		a.logger.Info("all background loops exited")
	case <-loopCtx.Done():
		a.logger.Warn("background loops did not exit within timeout, proceeding with shutdown",
			"configured_timeout", a.cfg.ShutdownLoopDrainTimeout,
		)
	}
	loopCancel()

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

// ── Background loop helper ─────────────────────────────────────────────────────

// ── Background loops (moved from cmd/akashi/main.go) ──────────────────────────

// ── Adapters (defined here because this file imports both sides) ───────────────

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
	return Conflict{
		ID:           c.ID,
		OrgID:        c.OrgID,
		DecisionAID:  c.DecisionAID,
		DecisionBID:  c.DecisionBID,
		AgentA:       c.AgentA,
		AgentB:       c.AgentB,
		DecisionType: c.DecisionType,
		Score:        float32(derefOr(c.Significance, 0)),
		Explanation:  derefOr(c.Explanation, ""),
		Category:     derefOr(c.Category, ""),
		Severity:     derefOr(c.Severity, ""),
		Status:       c.Status,
		DetectedAt:   c.DetectedAt,
	}
}

// derefOr returns the dereferenced value of ptr, or fallback if ptr is nil.
func derefOr[T any](ptr *T, fallback T) T {
	if ptr != nil {
		return *ptr
	}
	return fallback
}

// ── Helpers (moved from cmd/akashi/main.go) ────────────────────────────────────

// ctx is a no-op helper so that New(opts ...) can pass a background context to
// telemetry.Init without adding a context parameter to the public API.
// The returned context is never cancelled by this function.
func ctx(_ []Option) context.Context { return context.Background() }
