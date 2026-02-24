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

	"github.com/ashita-ai/akashi/internal/authz"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/storage"
)

// serverInstructions is sent to every MCP client during the initialize handshake.
// This ensures every connected agent knows the check-before/record-after workflow
// without requiring per-project configuration (CLAUDE.md, agents.md, etc.).
const serverInstructions = `You have access to Akashi, a decision audit trail for AI agents.

WORKFLOW — follow this for every non-trivial decision:

1. BEFORE deciding: call akashi_check with the decision_type you're about to make.
   This returns prior precedents and conflicts. Use them to make a better decision
   and avoid contradicting prior work.

2. AFTER deciding: call akashi_trace with what you decided (outcome), why (reasoning),
   your confidence (0.0–1.0), and project (the project or app name, e.g. "akashi",
   "my-langchain-app"). This creates a provable record so other agents can learn from it.

TOOLS:
- akashi_check: look up precedents before deciding (always call first)
- akashi_trace: record a decision after making it (always call after)
- akashi_query: filter the audit trail by type, agent, confidence, time range
- akashi_search: semantic similarity search across decisions
- akashi_recent: see recent decisions for context

CHECK BEFORE: choosing architecture/technology, starting a review or audit,
making trade-offs, filing issues/PRs, changing existing behavior.

TRACE AFTER: completing a review, choosing an approach, creating issues/PRs,
finishing a task that involved choices, making security or access judgments.

SKIP: pure execution (formatting, typo fixes), reading/exploring code,
asking the user a question (no decision yet).

Be honest about confidence. Reference precedents when they influence you.`

// Server wraps the MCP server with Akashi's service layer.
type Server struct {
	mcpServer    *mcpserver.MCPServer
	db           *storage.DB        // for resources (read-only queries)
	decisionSvc  *decisions.Service // for tools (shared business logic)
	grantCache   *authz.GrantCache  // optional cache for LoadGrantedSet
	logger       *slog.Logger
	checkTracker *checkTracker // tracks check-before-trace workflow compliance
	rootsCache   *rootsCache   // caches MCP roots per session (one request per session)
}

// New creates and configures a new MCP server with all resources, tools, and prompts.
func New(db *storage.DB, decisionSvc *decisions.Service, grantCache *authz.GrantCache, logger *slog.Logger, version string) *Server {
	s := &Server{
		db:           db,
		decisionSvc:  decisionSvc,
		grantCache:   grantCache,
		logger:       logger,
		checkTracker: newCheckTracker(time.Hour),
		rootsCache:   newRootsCache(),
	}

	s.mcpServer = mcpserver.NewMCPServer(
		"akashi",
		version,
		mcpserver.WithResourceCapabilities(true, true),
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithPromptCapabilities(true),
		mcpserver.WithRoots(),
		mcpserver.WithInstructions(serverInstructions),
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
