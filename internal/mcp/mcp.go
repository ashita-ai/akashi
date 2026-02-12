// Package mcp implements the Model Context Protocol server for Akashi.
//
// The MCP server exposes the same capabilities as the HTTP API through
// MCP resources, tools, and prompts, allowing MCP-compatible AI agents
// to interact with Akashi's decision trace infrastructure.
package mcp

import (
	"log/slog"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/storage"
)

// Server wraps the MCP server with Akashi's service layer.
type Server struct {
	mcpServer    *mcpserver.MCPServer
	db           *storage.DB        // for resources (read-only queries)
	decisionSvc  *decisions.Service // for tools (shared business logic)
	logger       *slog.Logger
	checkTracker *checkTracker // tracks check-before-trace workflow compliance
}

// New creates and configures a new MCP server with all resources, tools, and prompts.
func New(db *storage.DB, decisionSvc *decisions.Service, logger *slog.Logger, version string) *Server {
	s := &Server{
		db:           db,
		decisionSvc:  decisionSvc,
		logger:       logger,
		checkTracker: newCheckTracker(time.Hour),
	}

	s.mcpServer = mcpserver.NewMCPServer(
		"akashi",
		version,
		mcpserver.WithResourceCapabilities(true, true),
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithPromptCapabilities(true),
	)

	s.registerResources()
	s.registerTools()
	s.registerPrompts()

	return s
}

// MCPServer returns the underlying mcp-go server for transport setup.
func (s *Server) MCPServer() *mcpserver.MCPServer {
	return s.mcpServer
}

func errorResult(msg string) *mcplib.CallToolResult {
	return &mcplib.CallToolResult{
		Content: []mcplib.Content{
			mcplib.TextContent{Type: "text", Text: msg},
		},
		IsError: true,
	}
}
