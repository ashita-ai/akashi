package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/ratelimit"
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

// New creates a new HTTP server with all routes configured.
// mcpSrv is optional; if non-nil the MCP StreamableHTTP transport is mounted at /mcp.
// limiter is optional; if nil, rate limiting is disabled (noop).
func New(
	db *storage.DB,
	jwtMgr *auth.JWTManager,
	decisionSvc *decisions.Service,
	buffer *trace.Buffer,
	limiter *ratelimit.Limiter,
	logger *slog.Logger,
	port int,
	readTimeout, writeTimeout time.Duration,
	mcpSrv *mcpserver.MCPServer,
	version string,
	maxRequestBodyBytes int64,
) *Server {
	h := NewHandlers(db, jwtMgr, decisionSvc, buffer, logger, version, maxRequestBodyBytes)

	// Rate limit rules.
	ingestRL := ratelimit.Middleware(limiter, ratelimit.Rule{
		Prefix: "ingest", Limit: 300, Window: time.Minute,
	}, agentKeyFunc)
	queryRL := ratelimit.Middleware(limiter, ratelimit.Rule{
		Prefix: "query", Limit: 300, Window: time.Minute,
	}, agentKeyFunc)
	searchRL := ratelimit.Middleware(limiter, ratelimit.Rule{
		Prefix: "search", Limit: 100, Window: time.Minute,
	}, agentKeyFunc)
	authRL := ratelimit.Middleware(limiter, ratelimit.Rule{
		Prefix: "auth", Limit: 20, Window: time.Minute,
	}, ratelimit.IPKeyFunc)

	mux := http.NewServeMux()

	// Auth endpoints (no auth required, rate limited by IP).
	mux.Handle("POST /auth/token", authRL(http.HandlerFunc(h.HandleAuthToken)))
	mux.Handle("POST /auth/refresh", authRL(http.HandlerFunc(h.HandleAuthToken)))

	// Agent management (admin-only, no rate limit — admin is exempt).
	adminOnly := requireRole(model.RoleAdmin)
	mux.Handle("POST /v1/agents", adminOnly(http.HandlerFunc(h.HandleCreateAgent)))
	mux.Handle("GET /v1/agents", adminOnly(http.HandlerFunc(h.HandleListAgents)))

	// Trace ingestion (admin + agent, rate limited).
	writeRoles := requireRole(model.RoleAdmin, model.RoleAgent)
	mux.Handle("POST /v1/runs", ingestRL(writeRoles(http.HandlerFunc(h.HandleCreateRun))))
	mux.Handle("POST /v1/runs/{run_id}/events", ingestRL(writeRoles(http.HandlerFunc(h.HandleAppendEvents))))
	mux.Handle("POST /v1/runs/{run_id}/complete", ingestRL(writeRoles(http.HandlerFunc(h.HandleCompleteRun))))
	mux.Handle("POST /v1/trace", ingestRL(writeRoles(http.HandlerFunc(h.HandleTrace))))

	// Query endpoints (all authenticated roles, rate limited).
	allRoles := requireRole(model.RoleAdmin, model.RoleAgent, model.RoleReader)
	mux.Handle("POST /v1/query", queryRL(allRoles(http.HandlerFunc(h.HandleQuery))))
	mux.Handle("POST /v1/query/temporal", queryRL(allRoles(http.HandlerFunc(h.HandleTemporalQuery))))
	mux.Handle("GET /v1/runs/{run_id}", queryRL(allRoles(http.HandlerFunc(h.HandleGetRun))))
	mux.Handle("GET /v1/agents/{agent_id}/history", queryRL(allRoles(http.HandlerFunc(h.HandleAgentHistory))))

	// Search endpoint (all authenticated roles, tighter rate limit).
	mux.Handle("POST /v1/search", searchRL(allRoles(http.HandlerFunc(h.HandleSearch))))

	// Check endpoint — lightweight precedent lookup (query rate limit).
	mux.Handle("POST /v1/check", queryRL(allRoles(http.HandlerFunc(h.HandleCheck))))

	// Recent decisions (query rate limit).
	mux.Handle("GET /v1/decisions/recent", queryRL(allRoles(http.HandlerFunc(h.HandleDecisionsRecent))))

	// Subscription endpoint (no rate limit — long-lived connection).
	mux.Handle("GET /v1/subscribe", allRoles(http.HandlerFunc(h.HandleSubscribe)))

	// Access control (admin + agent owners).
	mux.Handle("POST /v1/grants", writeRoles(http.HandlerFunc(h.HandleCreateGrant)))
	mux.Handle("DELETE /v1/grants/{grant_id}", writeRoles(http.HandlerFunc(h.HandleDeleteGrant)))

	// Conflicts (all authenticated roles, query rate limit).
	mux.Handle("GET /v1/conflicts", queryRL(allRoles(http.HandlerFunc(h.HandleListConflicts))))

	// MCP StreamableHTTP transport (auth required, any authenticated role).
	if mcpSrv != nil {
		mcpHTTP := mcpserver.NewStreamableHTTPServer(mcpSrv)
		mux.Handle("/mcp", allRoles(mcpHTTP))
	}

	// Health (no auth, no rate limit).
	mux.HandleFunc("GET /health", h.HandleHealth)

	// Apply middleware chain: request ID → security headers → tracing → logging → auth.
	var handler http.Handler = mux
	handler = authMiddleware(jwtMgr, handler)
	handler = loggingMiddleware(logger, handler)
	handler = tracingMiddleware(handler)
	handler = securityHeadersMiddleware(handler)
	handler = requestIDMiddleware(handler)

	return &Server{
		httpServer: &http.Server{
			Addr:         fmt.Sprintf(":%d", port),
			Handler:      handler,
			ReadTimeout:  readTimeout,
			WriteTimeout: writeTimeout,
		},
		handler:  handler,
		handlers: h,
		logger:   logger,
	}
}

// agentKeyFunc extracts the agent ID from the request context for rate limiting.
// Returns empty string for admin agents (admin is exempt from rate limits).
func agentKeyFunc(r *http.Request) string {
	claims := ClaimsFromContext(r.Context())
	if claims == nil {
		return ""
	}
	if claims.Role == model.RoleAdmin {
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
