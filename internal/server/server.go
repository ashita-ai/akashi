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

// ServerConfig holds all dependencies and configuration for creating a Server.
// Optional fields (nil-safe): Broker, Searcher, MCPServer, UIFS, OpenAPISpec.
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
	Port                     int
	ReadTimeout              time.Duration
	WriteTimeout             time.Duration
	Version                  string
	MaxRequestBodyBytes      int64
	CORSAllowedOrigins       []string // Allowed origins for CORS; ["*"] permits all.
	TrustProxy               bool     // When true, use X-Forwarded-For for rate limit client IP.
	IdempotencyInProgressTTL time.Duration
	EnableDestructiveDelete  bool

	// Optional embedded assets.
	UIFS        fs.FS  // Embedded UI filesystem (SPA).
	OpenAPISpec []byte // Embedded OpenAPI YAML.
}

// New creates a new HTTP server with all routes configured.
func New(cfg ServerConfig) *Server {
	h := NewHandlers(HandlersDeps{
		DB:                       cfg.DB,
		JWTMgr:                   cfg.JWTMgr,
		DecisionSvc:              cfg.DecisionSvc,
		Buffer:                   cfg.Buffer,
		Broker:                   cfg.Broker,
		Searcher:                 cfg.Searcher,
		GrantCache:               cfg.GrantCache,
		Logger:                   cfg.Logger,
		Version:                  cfg.Version,
		MaxRequestBodyBytes:      cfg.MaxRequestBodyBytes,
		OpenAPISpec:              cfg.OpenAPISpec,
		IdempotencyInProgressTTL: cfg.IdempotencyInProgressTTL,
		EnableDestructiveDelete:  cfg.EnableDestructiveDelete,
	})

	mux := http.NewServeMux()

	// Auth endpoints (no auth required).
	mux.Handle("POST /auth/token", http.HandlerFunc(h.HandleAuthToken))
	mux.Handle("POST /auth/refresh", http.HandlerFunc(h.HandleAuthToken))

	// Agent management (admin-only).
	adminOnly := requireRole(model.RoleAdmin)
	mux.Handle("POST /v1/agents", adminOnly(http.HandlerFunc(h.HandleCreateAgent)))
	mux.Handle("GET /v1/agents", adminOnly(http.HandlerFunc(h.HandleListAgents)))
	mux.Handle("GET /v1/agents/{agent_id}", adminOnly(http.HandlerFunc(h.HandleGetAgent)))
	mux.Handle("PATCH /v1/agents/{agent_id}", adminOnly(http.HandlerFunc(h.HandleUpdateAgent)))
	mux.Handle("GET /v1/agents/{agent_id}/stats", adminOnly(http.HandlerFunc(h.HandleAgentStats)))
	mux.Handle("PATCH /v1/agents/{agent_id}/tags", adminOnly(http.HandlerFunc(h.HandleUpdateAgentTags)))
	mux.Handle("DELETE /v1/agents/{agent_id}", adminOnly(http.HandlerFunc(h.HandleDeleteAgent)))
	mux.Handle("GET /v1/export/decisions", adminOnly(http.HandlerFunc(h.HandleExportDecisions)))

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

	// Session view (reader+).
	mux.Handle("GET /v1/sessions/{session_id}", readRole(http.HandlerFunc(h.HandleSessionView)))

	// Trace health (admin-only).
	mux.Handle("GET /v1/trace-health", adminOnly(http.HandlerFunc(h.HandleTraceHealth)))

	// Integrity verification (reader+).
	mux.Handle("GET /v1/verify/{id}", readRole(http.HandlerFunc(h.HandleVerifyDecision)))

	// Subscription endpoint (reader+).
	mux.Handle("GET /v1/subscribe", readRole(http.HandlerFunc(h.HandleSubscribe)))

	// Access control (agent+ can grant access to own traces).
	mux.Handle("POST /v1/grants", writeRole(http.HandlerFunc(h.HandleCreateGrant)))
	mux.Handle("DELETE /v1/grants/{grant_id}", writeRole(http.HandlerFunc(h.HandleDeleteGrant)))

	// Conflicts (reader+ for list, agent+ for resolve/patch).
	mux.Handle("GET /v1/conflicts", readRole(http.HandlerFunc(h.HandleListConflicts)))
	mux.Handle("POST /v1/conflicts/{id}/resolve", writeRole(http.HandlerFunc(h.HandleResolveConflict)))
	mux.Handle("PATCH /v1/conflicts/{id}", writeRole(http.HandlerFunc(h.HandlePatchConflict)))

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

	// SPA: serve the embedded UI at the root path.
	// Registered last so all API routes take priority via the mux's longest-match rule.
	if cfg.UIFS != nil {
		mux.Handle("/", newSPAHandler(cfg.UIFS))
		cfg.Logger.Info("ui enabled, serving SPA at /")
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
