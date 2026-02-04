package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/ashita-ai/kyoyu/internal/auth"
	"github.com/ashita-ai/kyoyu/internal/model"
	"github.com/ashita-ai/kyoyu/internal/service/embedding"
	"github.com/ashita-ai/kyoyu/internal/service/trace"
	"github.com/ashita-ai/kyoyu/internal/storage"
)

// Server is the Kyoyu HTTP server.
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
func New(
	db *storage.DB,
	jwtMgr *auth.JWTManager,
	embedder embedding.Provider,
	buffer *trace.Buffer,
	logger *slog.Logger,
	port int,
	readTimeout, writeTimeout time.Duration,
	mcpSrv *mcpserver.MCPServer,
) *Server {
	h := NewHandlers(db, jwtMgr, embedder, buffer, logger)

	mux := http.NewServeMux()

	// Auth endpoints (no auth required for token issuance).
	mux.HandleFunc("POST /auth/token", h.HandleAuthToken)
	mux.HandleFunc("POST /auth/refresh", h.HandleAuthToken) // Same handler, re-issues token.

	// Agent management (admin-only).
	adminOnly := requireRole(model.RoleAdmin)
	mux.Handle("POST /v1/agents", adminOnly(http.HandlerFunc(h.HandleCreateAgent)))
	mux.Handle("GET /v1/agents", adminOnly(http.HandlerFunc(h.HandleListAgents)))

	// Trace ingestion (admin + agent).
	writeRoles := requireRole(model.RoleAdmin, model.RoleAgent)
	mux.Handle("POST /v1/runs", writeRoles(http.HandlerFunc(h.HandleCreateRun)))
	mux.Handle("POST /v1/runs/{run_id}/events", writeRoles(http.HandlerFunc(h.HandleAppendEvents)))
	mux.Handle("POST /v1/runs/{run_id}/complete", writeRoles(http.HandlerFunc(h.HandleCompleteRun)))
	mux.Handle("POST /v1/trace", writeRoles(http.HandlerFunc(h.HandleTrace)))

	// Query endpoints (all authenticated roles).
	allRoles := requireRole(model.RoleAdmin, model.RoleAgent, model.RoleReader)
	mux.Handle("POST /v1/query", allRoles(http.HandlerFunc(h.HandleQuery)))
	mux.Handle("POST /v1/query/temporal", allRoles(http.HandlerFunc(h.HandleTemporalQuery)))
	mux.Handle("GET /v1/runs/{run_id}", allRoles(http.HandlerFunc(h.HandleGetRun)))
	mux.Handle("GET /v1/agents/{agent_id}/history", allRoles(http.HandlerFunc(h.HandleAgentHistory)))

	// Search endpoint (all authenticated roles).
	mux.Handle("POST /v1/search", allRoles(http.HandlerFunc(h.HandleSearch)))

	// Subscription endpoint (all authenticated roles).
	mux.Handle("GET /v1/subscribe", allRoles(http.HandlerFunc(h.HandleSubscribe)))

	// Access control (admin + agent owners).
	mux.Handle("POST /v1/grants", writeRoles(http.HandlerFunc(h.HandleCreateGrant)))
	mux.Handle("DELETE /v1/grants/{grant_id}", writeRoles(http.HandlerFunc(h.HandleDeleteGrant)))

	// Conflicts (all authenticated roles).
	mux.Handle("GET /v1/conflicts", allRoles(http.HandlerFunc(h.HandleListConflicts)))

	// MCP StreamableHTTP transport (auth required, any authenticated role).
	if mcpSrv != nil {
		mcpHTTP := mcpserver.NewStreamableHTTPServer(mcpSrv)
		mux.Handle("/mcp", allRoles(mcpHTTP))
	}

	// Health (no auth).
	mux.HandleFunc("GET /health", h.HandleHealth)

	// Apply middleware chain: request ID → tracing → logging → auth.
	var handler http.Handler = mux
	handler = authMiddleware(jwtMgr, handler)
	handler = loggingMiddleware(logger, handler)
	handler = tracingMiddleware(handler)
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
