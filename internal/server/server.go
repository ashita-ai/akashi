package server

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/authz"
	"github.com/ashita-ai/akashi/internal/conflicts"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/ratelimit"
	"github.com/ashita-ai/akashi/internal/search"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/trace"
	"github.com/ashita-ai/akashi/internal/storage"
)

// Server is the Akashi HTTP server.
type Server struct {
	httpServer *http.Server
	handler    http.Handler
	handlers   *Handlers
	logger     *slog.Logger
}

// Handler returns the root HTTP handler for use in tests.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// RoleMiddlewareFn is a function that wraps an HTTP handler to enforce a minimum RBAC role.
// Exported for use by RouteRegistrar adapters in the root akashi package so enterprise
// routes can use the same auth chain without importing internal/server directly.
type RoleMiddlewareFn func(minRole model.AgentRole) func(http.Handler) http.Handler

// ServerConfig holds all dependencies and configuration for creating a Server.
// Optional fields (nil-safe): Broker, Searcher, MCPServer, UIFS, OpenAPISpec,
// ExtraRoutes, Middlewares, DecisionHooks.
type ServerConfig struct {
	// Required dependencies.
	DB          *storage.DB
	JWTMgr      *auth.JWTManager
	DecisionSvc *decisions.Service
	Buffer      *trace.Buffer
	Logger      *slog.Logger

	// Optional dependencies (nil = disabled).
	Broker      *Broker
	Searcher    search.Searcher
	GrantCache  *authz.GrantCache
	MCPServer   *mcpserver.MCPServer
	RateLimiter ratelimit.Limiter

	// HTTP server settings.
	Port                    int
	ReadTimeout             time.Duration
	WriteTimeout            time.Duration
	Version                 string
	MaxRequestBodyBytes     int64
	CORSAllowedOrigins      []string // Allowed origins for CORS; ["*"] permits all.
	TrustProxy              bool     // When true, use X-Forwarded-For for rate limit client IP.
	EnableDestructiveDelete bool
	RetentionInterval       time.Duration // How often the background retention worker runs (default 24h).

	// Optional embedded assets.
	UIFS        fs.FS  // Embedded UI filesystem (SPA).
	OpenAPISpec []byte // Embedded OpenAPI YAML.

	// Extension points (enterprise / plugin).
	// ExtraRoutes are called after all OSS routes are registered. Each function
	// receives the mux and a RoleMiddlewareFn that applies RBAC role enforcement.
	ExtraRoutes []func(*http.ServeMux, RoleMiddlewareFn)
	// Middlewares wrap the fully-assembled handler (outermost position).
	// Applied in registration order: index 0 is the outermost middleware.
	Middlewares []func(http.Handler) http.Handler
	// DecisionHooks are fired asynchronously on decision lifecycle events.
	DecisionHooks []DecisionHook

	// Self-serve signup.
	SignupEnabled     bool              // Enable POST /auth/signup for self-serve org creation.
	SignupRateLimiter ratelimit.Limiter // Per-IP signup rate limiter; nil = use default (1 RPS, burst 5).

	// IDE hook integration.
	HooksEnabled bool   // Enable /hooks/* IDE integration endpoints.
	HooksAPIKey  string // Optional API key for non-localhost hook access.
	AutoTrace    bool   // Auto-trace git commits from PostToolUse hooks.

	// Conflict metrics.
	ResolutionRecorder conflicts.ResolutionRecorder

	// Conflict validator for the eval endpoint. Nil = eval returns 501.
	ConflictValidator conflicts.Validator
}

// New creates a new HTTP server with all routes configured.
func New(cfg ServerConfig) *Server {
	h := NewHandlers(HandlersDeps{
		DB:                      cfg.DB,
		JWTMgr:                  cfg.JWTMgr,
		DecisionSvc:             cfg.DecisionSvc,
		Buffer:                  cfg.Buffer,
		Broker:                  cfg.Broker,
		Searcher:                cfg.Searcher,
		GrantCache:              cfg.GrantCache,
		Logger:                  cfg.Logger,
		Version:                 cfg.Version,
		MaxRequestBodyBytes:     cfg.MaxRequestBodyBytes,
		OpenAPISpec:             cfg.OpenAPISpec,
		EnableDestructiveDelete: cfg.EnableDestructiveDelete,
		RetentionInterval:       cfg.RetentionInterval,
		DecisionHooks:           cfg.DecisionHooks,
		AutoTrace:               cfg.AutoTrace,
		TrustProxy:              cfg.TrustProxy,
		ResolutionRecorder:      cfg.ResolutionRecorder,
		ConflictValidator:       cfg.ConflictValidator,
	})

	mux := http.NewServeMux()

	// Auth endpoints (no auth required).
	mux.Handle("POST /auth/token", http.HandlerFunc(h.HandleAuthToken))
	mux.Handle("POST /auth/refresh", http.HandlerFunc(h.HandleAuthToken))

	// Self-serve signup (no auth required, gated by config flag).
	if cfg.SignupEnabled {
		if cfg.SignupRateLimiter != nil {
			h.signupLimiter = cfg.SignupRateLimiter
		} else {
			h.signupLimiter = ratelimit.NewMemoryLimiter(1.0, 5) // 1 RPS, burst 5 per IP
		}
		mux.Handle("POST /auth/signup", http.HandlerFunc(h.HandleSignup))
	}

	// Agent management (admin-only).
	adminOnly := requireRole(model.RoleAdmin)
	mux.Handle("POST /v1/auth/scoped-token", adminOnly(http.HandlerFunc(h.HandleScopedToken)))
	mux.Handle("POST /v1/agents", adminOnly(http.HandlerFunc(h.HandleCreateAgent)))
	mux.Handle("GET /v1/agents", adminOnly(http.HandlerFunc(h.HandleListAgents)))
	mux.Handle("GET /v1/agents/{agent_id}", adminOnly(http.HandlerFunc(h.HandleGetAgent)))
	mux.Handle("PATCH /v1/agents/{agent_id}", adminOnly(http.HandlerFunc(h.HandleUpdateAgent)))
	mux.Handle("GET /v1/agents/{agent_id}/stats", adminOnly(http.HandlerFunc(h.HandleAgentStats)))
	mux.Handle("PATCH /v1/agents/{agent_id}/tags", adminOnly(http.HandlerFunc(h.HandleUpdateAgentTags)))
	mux.Handle("DELETE /v1/agents/{agent_id}", adminOnly(http.HandlerFunc(h.HandleDeleteAgent)))
	mux.Handle("DELETE /v1/decisions/{id}", adminOnly(http.HandlerFunc(h.HandleRetractDecision)))
	mux.Handle("GET /v1/export/decisions", adminOnly(http.HandlerFunc(h.HandleExportDecisions)))

	// GDPR erasure (org_owner+ — stronger than admin because erasure is irreversible).
	orgOwnerOnly := requireRole(model.RoleOrgOwner)
	mux.Handle("POST /v1/decisions/{id}/erase", orgOwnerOnly(http.HandlerFunc(h.HandleEraseDecision)))

	// API key management (admin-only).
	mux.Handle("POST /v1/keys", adminOnly(http.HandlerFunc(h.HandleCreateKey)))
	mux.Handle("GET /v1/keys", adminOnly(http.HandlerFunc(h.HandleListKeys)))
	mux.Handle("DELETE /v1/keys/{id}", adminOnly(http.HandlerFunc(h.HandleRevokeKey)))
	mux.Handle("POST /v1/keys/{id}/rotate", adminOnly(http.HandlerFunc(h.HandleRotateKey)))

	// Usage metering (admin-only).
	mux.Handle("GET /v1/usage", adminOnly(http.HandlerFunc(h.HandleGetUsage)))

	// Trace ingestion (agent+).
	writeRole := requireRole(model.RoleAgent)
	mux.Handle("POST /v1/runs", writeRole(http.HandlerFunc(h.HandleCreateRun)))
	mux.Handle("POST /v1/runs/{run_id}/events", writeRole(http.HandlerFunc(h.HandleAppendEvents)))
	mux.Handle("POST /v1/runs/{run_id}/complete", writeRole(http.HandlerFunc(h.HandleCompleteRun)))
	mux.Handle("POST /v1/trace", writeRole(http.HandlerFunc(h.HandleTrace)))

	// Query endpoints (reader+).
	readRole := requireRole(model.RoleReader)
	mux.Handle("GET /v1/decisions/{id}", readRole(http.HandlerFunc(h.HandleGetDecision)))
	mux.Handle("POST /v1/query", readRole(http.HandlerFunc(h.HandleQuery)))
	mux.Handle("POST /v1/query/temporal", readRole(http.HandlerFunc(h.HandleTemporalQuery)))
	mux.Handle("GET /v1/runs/{run_id}", readRole(http.HandlerFunc(h.HandleGetRun)))
	mux.Handle("GET /v1/agents/{agent_id}/history", readRole(http.HandlerFunc(h.HandleAgentHistory)))

	// Search endpoint (reader+).
	mux.Handle("POST /v1/search", readRole(http.HandlerFunc(h.HandleSearch)))

	// Check endpoint — lightweight precedent lookup (reader+).
	mux.Handle("POST /v1/check", readRole(http.HandlerFunc(h.HandleCheck)))

	// Recent decisions (reader+).
	mux.Handle("GET /v1/decisions/recent", readRole(http.HandlerFunc(h.HandleDecisionsRecent)))

	// Decision revision history (reader+).
	mux.Handle("GET /v1/decisions/{id}/revisions", readRole(http.HandlerFunc(h.HandleDecisionRevisions)))

	// Decision conflicts (reader+).
	mux.Handle("GET /v1/decisions/{id}/conflicts", readRole(http.HandlerFunc(h.HandleDecisionConflicts)))

	// Decision assessments: explicit outcome feedback (spec 29 / ADR-020 Tier 2).
	mux.Handle("POST /v1/decisions/{id}/assess", writeRole(http.HandlerFunc(h.HandleAssessDecision)))
	mux.Handle("GET /v1/decisions/{id}/assessments", readRole(http.HandlerFunc(h.HandleListAssessments)))

	// Session view (reader+).
	mux.Handle("GET /v1/sessions/{session_id}", readRole(http.HandlerFunc(h.HandleSessionView)))

	// Trace health (admin-only).
	mux.Handle("GET /v1/trace-health", adminOnly(http.HandlerFunc(h.HandleTraceHealth)))

	// Integrity verification (reader+).
	mux.Handle("GET /v1/verify/{id}", readRole(http.HandlerFunc(h.HandleVerifyDecision)))

	// Subscription endpoint (reader+).
	mux.Handle("GET /v1/subscribe", readRole(http.HandlerFunc(h.HandleSubscribe)))

	// Access control (admin for list, agent+ can grant access to own traces).
	mux.Handle("GET /v1/grants", adminOnly(http.HandlerFunc(h.HandleListGrants)))
	mux.Handle("POST /v1/grants", writeRole(http.HandlerFunc(h.HandleCreateGrant)))
	mux.Handle("DELETE /v1/grants/{grant_id}", writeRole(http.HandlerFunc(h.HandleDeleteGrant)))

	// Conflicts (reader+ for list/detail/analytics, agent+ for adjudicate/patch/resolve).
	mux.Handle("GET /v1/conflicts/analytics", readRole(http.HandlerFunc(h.HandleConflictAnalytics)))
	mux.Handle("GET /v1/conflicts", readRole(http.HandlerFunc(h.HandleListConflicts)))
	mux.Handle("GET /v1/conflicts/{id}", readRole(http.HandlerFunc(h.HandleGetConflict)))
	mux.Handle("GET /v1/conflict-groups", readRole(http.HandlerFunc(h.HandleListConflictGroups)))
	mux.Handle("PATCH /v1/conflict-groups/{id}/resolve", writeRole(http.HandlerFunc(h.HandleResolveConflictGroup)))
	mux.Handle("POST /v1/conflicts/{id}/adjudicate", writeRole(http.HandlerFunc(h.HandleAdjudicateConflict)))
	mux.Handle("PATCH /v1/conflicts/{id}", writeRole(http.HandlerFunc(h.HandlePatchConflict)))

	// Conflict eval and labeling (admin-only).
	mux.Handle("POST /v1/admin/conflicts/validate-pair", adminOnly(http.HandlerFunc(h.HandleValidatePair)))
	mux.Handle("POST /v1/admin/conflicts/eval", adminOnly(http.HandlerFunc(h.HandleConflictEval)))
	mux.Handle("PUT /v1/admin/conflicts/{id}/label", adminOnly(http.HandlerFunc(h.HandleUpsertConflictLabel)))
	mux.Handle("GET /v1/admin/conflicts/{id}/label", adminOnly(http.HandlerFunc(h.HandleGetConflictLabel)))
	mux.Handle("DELETE /v1/admin/conflicts/{id}/label", adminOnly(http.HandlerFunc(h.HandleDeleteConflictLabel)))
	mux.Handle("GET /v1/admin/conflict-labels", adminOnly(http.HandlerFunc(h.HandleListConflictLabels)))
	mux.Handle("POST /v1/admin/scorer-eval", adminOnly(http.HandlerFunc(h.HandleScorerEval)))

	// Retention policy and legal holds (admin for writes, reader+ for GET).
	mux.Handle("GET /v1/retention", readRole(http.HandlerFunc(h.HandleGetRetention)))
	mux.Handle("PUT /v1/retention", adminOnly(http.HandlerFunc(h.HandleSetRetention)))
	mux.Handle("POST /v1/retention/purge", adminOnly(http.HandlerFunc(h.HandlePurge)))
	mux.Handle("POST /v1/retention/hold", adminOnly(http.HandlerFunc(h.HandleCreateHold)))
	mux.Handle("DELETE /v1/retention/hold/{id}", adminOnly(http.HandlerFunc(h.HandleReleaseHold)))

	// Org settings (reader+ for GET, admin-only for PUT).
	mux.Handle("GET /v1/org/settings", readRole(http.HandlerFunc(h.HandleGetOrgSettings)))
	mux.Handle("PUT /v1/org/settings", adminOnly(http.HandlerFunc(h.HandleSetOrgSettings)))

	// Project links (admin-only).
	mux.Handle("POST /v1/project-links", adminOnly(http.HandlerFunc(h.HandleCreateProjectLink)))
	mux.Handle("GET /v1/project-links", adminOnly(http.HandlerFunc(h.HandleListProjectLinks)))
	mux.Handle("DELETE /v1/project-links/{id}", adminOnly(http.HandlerFunc(h.HandleDeleteProjectLink)))
	mux.Handle("POST /v1/project-links/grant-all", adminOnly(http.HandlerFunc(h.HandleGrantAllProjectLinks)))

	// MCP StreamableHTTP transport (auth required, reader+).
	if cfg.MCPServer != nil {
		mcpHTTP := mcpserver.NewStreamableHTTPServer(cfg.MCPServer)
		mux.Handle("/mcp", readRole(mcpHTTP))
	}

	// OpenAPI spec (no auth).
	mux.HandleFunc("GET /openapi.yaml", h.HandleOpenAPISpec)

	// Config (no auth — feature flags for UI).
	mux.HandleFunc("GET /config", h.HandleConfig)

	// Health (no auth).
	mux.HandleFunc("GET /health", h.HandleHealth)

	// MCP info (no auth) — lets clients confirm connectivity and discover auth schemes.
	mux.HandleFunc("GET /mcp/info", h.HandleMCPInfo)

	// IDE hook endpoints (no auth, localhost-only).
	if cfg.HooksEnabled {
		hookGuard := func(next http.Handler) http.Handler {
			return localhostOnly(cfg.HooksAPIKey, next)
		}
		mux.Handle("POST /hooks/session-start", hookGuard(http.HandlerFunc(h.HandleHookSessionStart)))
		mux.Handle("POST /hooks/pre-tool-use", hookGuard(http.HandlerFunc(h.HandleHookPreToolUse)))
		mux.Handle("POST /hooks/post-tool-use", hookGuard(http.HandlerFunc(h.HandleHookPostToolUse)))
		cfg.Logger.Info("IDE hook endpoints enabled at /hooks/*")
	}

	// SPA: serve the embedded UI at the root path.
	// Registered last so all API routes take priority via the mux's longest-match rule.
	if cfg.UIFS != nil {
		mux.Handle("/", newSPAHandler(cfg.UIFS))
		cfg.Logger.Info("ui enabled, serving SPA at /")
	}

	// Extra routes from enterprise/plugins — registered after all OSS routes so
	// OSS routes always take precedence when there is a path conflict.
	roleFn := RoleMiddlewareFn(requireRole)
	for _, fn := range cfg.ExtraRoutes {
		fn(mux, roleFn)
	}

	// Middleware chain (outermost executes first):
	// request ID → security headers → CORS → tracing → logging → baggage → auth → recovery → rateLimit → handler.
	var handler http.Handler = mux
	if cfg.RateLimiter != nil {
		handler = rateLimitMiddleware(cfg.RateLimiter, cfg.Logger, cfg.TrustProxy, handler)
	}
	handler = recoveryMiddleware(cfg.Logger, handler)
	handler = authMiddleware(cfg.JWTMgr, cfg.DB, handler)
	handler = baggageMiddleware(handler)
	handler = loggingMiddleware(cfg.Logger, handler)
	handler = tracingMiddleware(handler)
	handler = corsMiddleware(cfg.CORSAllowedOrigins, handler)
	handler = securityHeadersMiddleware(handler)
	handler = requestIDMiddleware(handler)

	// Enterprise/plugin middlewares — applied outermost (index 0 = first-registered = outermost).
	// Iterate in reverse so that Middlewares[0] ends up as the true outermost layer.
	for i := len(cfg.Middlewares) - 1; i >= 0; i-- {
		handler = cfg.Middlewares[i](handler)
	}

	return &Server{
		httpServer: &http.Server{
			Addr:         fmt.Sprintf(":%d", cfg.Port),
			Handler:      handler,
			ReadTimeout:  cfg.ReadTimeout,
			WriteTimeout: cfg.WriteTimeout,
			IdleTimeout:  2 * cfg.ReadTimeout, // Prevent accumulation of idle connections.
		},
		handler:  handler,
		handlers: h,
		logger:   cfg.Logger,
	}
}

// Handlers returns the underlying Handlers for access to SeedAdmin etc.
func (s *Server) Handlers() *Handlers {
	return s.handlers
}

// Start begins serving HTTP requests.
func (s *Server) Start() error {
	s.logger.Info("http server starting", "addr", s.httpServer.Addr)
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("http server shutting down")
	return s.httpServer.Shutdown(ctx)
}
