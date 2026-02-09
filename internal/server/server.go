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
	"github.com/ashita-ai/akashi/internal/billing"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/ratelimit"
	"github.com/ashita-ai/akashi/internal/search"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/trace"
	"github.com/ashita-ai/akashi/internal/signup"
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
// Optional fields (nil-safe): BillingSvc, Limiter, Broker, SignupSvc, Searcher,
// MCPServer, UIFS, OpenAPISpec.
type ServerConfig struct {
	// Required dependencies.
	DB          *storage.DB
	JWTMgr      *auth.JWTManager
	DecisionSvc *decisions.Service
	Buffer      *trace.Buffer
	Logger      *slog.Logger

	// Optional dependencies (nil = disabled).
	BillingSvc *billing.Service
	Limiter    *ratelimit.Limiter
	Broker     *Broker
	SignupSvc  *signup.Service
	Searcher   search.Searcher
	MCPServer  *mcpserver.MCPServer

	// HTTP server settings.
	Port                int
	ReadTimeout         time.Duration
	WriteTimeout        time.Duration
	Version             string
	MaxRequestBodyBytes int64

	// Optional embedded assets.
	UIFS        fs.FS  // Embedded UI filesystem (SPA).
	OpenAPISpec []byte // Embedded OpenAPI YAML.
}

// New creates a new HTTP server with all routes configured.
func New(cfg ServerConfig) *Server {
	h := NewHandlers(HandlersDeps{
		DB:                  cfg.DB,
		JWTMgr:              cfg.JWTMgr,
		DecisionSvc:         cfg.DecisionSvc,
		BillingSvc:          cfg.BillingSvc,
		Buffer:              cfg.Buffer,
		Broker:              cfg.Broker,
		SignupSvc:           cfg.SignupSvc,
		Searcher:            cfg.Searcher,
		Logger:              cfg.Logger,
		Version:             cfg.Version,
		MaxRequestBodyBytes: cfg.MaxRequestBodyBytes,
		OpenAPISpec:         cfg.OpenAPISpec,
	})

	// Request ID extractor for rate limit error responses.
	reqIDFunc := func(r *http.Request) string {
		return RequestIDFromContext(r.Context())
	}

	// Rate limit rules.
	ingestRL := ratelimit.MiddlewareWithRequestID(cfg.Limiter, ratelimit.Rule{
		Prefix: "ingest", Limit: 300, Window: time.Minute,
	}, agentKeyFunc, reqIDFunc)
	queryRL := ratelimit.MiddlewareWithRequestID(cfg.Limiter, ratelimit.Rule{
		Prefix: "query", Limit: 300, Window: time.Minute,
	}, agentKeyFunc, reqIDFunc)
	searchRL := ratelimit.MiddlewareWithRequestID(cfg.Limiter, ratelimit.Rule{
		Prefix: "search", Limit: 100, Window: time.Minute,
	}, agentKeyFunc, reqIDFunc)
	authRL := ratelimit.MiddlewareWithRequestID(cfg.Limiter, ratelimit.Rule{
		Prefix: "auth", Limit: 20, Window: time.Minute,
	}, ratelimit.IPKeyFunc, reqIDFunc)

	mux := http.NewServeMux()

	// Auth endpoints (no auth required, rate limited by IP).
	mux.Handle("POST /auth/token", authRL(http.HandlerFunc(h.HandleAuthToken)))
	mux.Handle("POST /auth/refresh", authRL(http.HandlerFunc(h.HandleAuthToken)))

	// Signup endpoints (no auth required, rate limited by IP).
	mux.Handle("POST /auth/signup", authRL(http.HandlerFunc(h.HandleSignup)))
	mux.Handle("GET /auth/verify", authRL(http.HandlerFunc(h.HandleVerifyEmail)))

	// Agent management (admin-only, no rate limit — admin is exempt).
	adminOnly := requireRole(model.RoleAdmin)
	mux.Handle("POST /v1/agents", adminOnly(http.HandlerFunc(h.HandleCreateAgent)))
	mux.Handle("GET /v1/agents", adminOnly(http.HandlerFunc(h.HandleListAgents)))
	mux.Handle("DELETE /v1/agents/{agent_id}", adminOnly(http.HandlerFunc(h.HandleDeleteAgent)))
	mux.Handle("GET /v1/export/decisions", adminOnly(http.HandlerFunc(h.HandleExportDecisions)))

	// Trace ingestion (agent+, rate limited).
	writeRole := requireRole(model.RoleAgent)
	mux.Handle("POST /v1/runs", ingestRL(writeRole(http.HandlerFunc(h.HandleCreateRun))))
	mux.Handle("POST /v1/runs/{run_id}/events", ingestRL(writeRole(http.HandlerFunc(h.HandleAppendEvents))))
	mux.Handle("POST /v1/runs/{run_id}/complete", ingestRL(writeRole(http.HandlerFunc(h.HandleCompleteRun))))
	mux.Handle("POST /v1/trace", ingestRL(writeRole(http.HandlerFunc(h.HandleTrace))))

	// Query endpoints (reader+, rate limited).
	readRole := requireRole(model.RoleReader)
	mux.Handle("POST /v1/query", queryRL(readRole(http.HandlerFunc(h.HandleQuery))))
	mux.Handle("POST /v1/query/temporal", queryRL(readRole(http.HandlerFunc(h.HandleTemporalQuery))))
	mux.Handle("GET /v1/runs/{run_id}", queryRL(readRole(http.HandlerFunc(h.HandleGetRun))))
	mux.Handle("GET /v1/agents/{agent_id}/history", queryRL(readRole(http.HandlerFunc(h.HandleAgentHistory))))

	// Search endpoint (reader+, tighter rate limit).
	mux.Handle("POST /v1/search", searchRL(readRole(http.HandlerFunc(h.HandleSearch))))

	// Check endpoint — lightweight precedent lookup (reader+).
	mux.Handle("POST /v1/check", queryRL(readRole(http.HandlerFunc(h.HandleCheck))))

	// Recent decisions (reader+).
	mux.Handle("GET /v1/decisions/recent", queryRL(readRole(http.HandlerFunc(h.HandleDecisionsRecent))))

	// Subscription endpoint (reader+, no rate limit — long-lived connection).
	mux.Handle("GET /v1/subscribe", readRole(http.HandlerFunc(h.HandleSubscribe)))

	// Access control (agent+ can grant access to own traces).
	mux.Handle("POST /v1/grants", writeRole(http.HandlerFunc(h.HandleCreateGrant)))
	mux.Handle("DELETE /v1/grants/{grant_id}", writeRole(http.HandlerFunc(h.HandleDeleteGrant)))

	// Billing endpoints (org_owner+ for checkout/portal, no auth for webhooks).
	ownerOnly := requireRole(model.RoleOrgOwner)
	mux.Handle("POST /billing/checkout", ownerOnly(http.HandlerFunc(h.HandleBillingCheckout)))
	mux.Handle("POST /billing/portal", ownerOnly(http.HandlerFunc(h.HandleBillingPortal)))
	mux.Handle("POST /billing/webhooks", http.HandlerFunc(h.HandleBillingWebhook))

	// Usage endpoint (reader+, any authenticated user can see their org's usage).
	mux.Handle("GET /v1/usage", queryRL(readRole(http.HandlerFunc(h.HandleUsage))))

	// Conflicts (reader+, query rate limit).
	mux.Handle("GET /v1/conflicts", queryRL(readRole(http.HandlerFunc(h.HandleListConflicts))))

	// MCP StreamableHTTP transport (auth required, reader+).
	if cfg.MCPServer != nil {
		mcpHTTP := mcpserver.NewStreamableHTTPServer(cfg.MCPServer)
		mux.Handle("/mcp", readRole(mcpHTTP))
	}

	// OpenAPI spec (no auth, no rate limit).
	mux.HandleFunc("GET /openapi.yaml", h.HandleOpenAPISpec)

	// Health (no auth, no rate limit).
	mux.HandleFunc("GET /health", h.HandleHealth)

	// SPA: serve the embedded UI at the root path.
	// Registered last so all API routes take priority via the mux's longest-match rule.
	if cfg.UIFS != nil {
		mux.Handle("/", newSPAHandler(cfg.UIFS))
		cfg.Logger.Info("ui enabled, serving SPA at /")
	}

	// Middleware chain (outermost executes first):
	// request ID → security headers → tracing → logging → auth → recovery → handler.
	var handler http.Handler = mux
	handler = recoveryMiddleware(cfg.Logger, handler)
	handler = authMiddleware(cfg.JWTMgr, handler)
	handler = loggingMiddleware(cfg.Logger, handler)
	handler = tracingMiddleware(handler)
	handler = securityHeadersMiddleware(handler)
	handler = requestIDMiddleware(handler)

	return &Server{
		httpServer: &http.Server{
			Addr:         fmt.Sprintf(":%d", cfg.Port),
			Handler:      handler,
			ReadTimeout:  cfg.ReadTimeout,
			WriteTimeout: cfg.WriteTimeout,
		},
		handler:  handler,
		handlers: h,
		logger:   cfg.Logger,
	}
}

// agentKeyFunc extracts the agent ID from the request context for rate limiting.
// Returns empty string for admin+ roles (exempt from rate limits).
func agentKeyFunc(r *http.Request) string {
	claims := ClaimsFromContext(r.Context())
	if claims == nil {
		return ""
	}
	if model.RoleAtLeast(claims.Role, model.RoleAdmin) {
		return ""
	}
	return claims.AgentID
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
