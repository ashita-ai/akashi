// Package mcp implements the Model Context Protocol server for Akashi.
//
// The MCP server exposes the same capabilities as the HTTP API through
// MCP resources, tools, and prompts, allowing MCP-compatible AI agents
// to interact with Akashi's decision trace infrastructure.
package mcp

import (
	"log/slog"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/ashita-ai/akashi/internal/authz"
	"github.com/ashita-ai/akashi/internal/service/autoassess"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/quality"
	"github.com/ashita-ai/akashi/internal/storage"
)

// serverInstructions is sent to every MCP client during the initialize handshake.
// This ensures every connected agent knows the check-before/record-after workflow
// without requiring per-project configuration (CLAUDE.md, agents.md, etc.).
const serverInstructions = `You have access to Akashi, a decision audit trail for AI agents.

WORKFLOW — follow this for architecture, design, and trade-off decisions:

1. BEFORE making an architecture, design, trade-off, or security decision: call akashi_check to look for prior decisions and active conflicts.
   Pass a natural language query describing what you're about to decide. Use the results
   to avoid contradicting prior work and to cite relevant precedents.

2. AFTER deciding: call akashi_trace with what you decided (outcome), why (reasoning),
   your confidence (0.0–1.0), and project (the project or app name, e.g. "akashi",
   "my-langchain-app"). This creates a provable record so other agents can learn from it.

TOOLS:
- akashi_check: look up precedents and conflicts before deciding (always call first)
- akashi_trace: record a decision after making it (always call after)
- akashi_query: filter or search the audit trail by type, agent, confidence, or free-text
- akashi_conflicts: list and filter open conflicts between agents
- akashi_resolve: resolve a conflict or mark it as a false positive (set winner or false_positive)
- akashi_assess: record whether a prior decision turned out to be correct
- akashi_stats: aggregate health metrics for the decision trail

CHECK BEFORE: choosing architecture/technology, starting a review or audit,
making trade-offs, filing issues/PRs, changing existing behavior.

TRACE AFTER: completing a review, choosing an approach, creating issues/PRs,
finishing a task that involved choices, making security or access judgments.

SKIP: pure execution (formatting, typo fixes), reading/exploring code,
asking the user a question (no decision yet).

Be honest about confidence — most decisions warrant 0.4-0.8, not 0.9+. Reference precedents when they influence you.`

// Server wraps the MCP server with Akashi's service layer.
type Server struct {
	mcpServer                   *mcpserver.MCPServer
	db                          storage.Store      // for resources (read-only queries)
	decisionSvc                 *decisions.Service // for tools (shared business logic)
	grantCache                  *authz.GrantCache  // optional cache for LoadGrantedSet
	logger                      *slog.Logger
	rootsCache                  *rootsCache          // caches MCP roots per session (one request per session)
	checkCache                  *checkCache          // caches last akashi_check response per session for evidence auto-attach
	onCheck                     func(agentID string) // called when akashi_check is invoked; wires IDE hook gate
	highConfidenceWarnThreshold float32              // confidence above this with no evidence triggers a warning
	standardTypes               map[string]bool      // configurable set of standard decision types for tips
	autoAssessor                *autoassess.Assessor // optional auto-assessor for conflict resolution signals
}

// SetCheckNotify registers a callback that fires whenever akashi_check is called.
// The callback receives the authenticated agent_id so the hook gate can track
// checks per-agent rather than per-machine.
func (s *Server) SetCheckNotify(f func(agentID string)) {
	s.onCheck = f
}

// SetAutoAssessor registers the auto-assessor for conflict resolution signals.
func (s *Server) SetAutoAssessor(a *autoassess.Assessor) {
	s.autoAssessor = a
}

// New creates and configures a new MCP server with all resources, tools, and prompts.
// standardTypes controls which decision types are suggested in completeness tips.
// Pass nil to use quality.DefaultStandardDecisionTypes.
func New(db storage.Store, decisionSvc *decisions.Service, grantCache *authz.GrantCache, logger *slog.Logger, version string, highConfWarnThreshold float32, standardTypes map[string]bool) *Server {
	if standardTypes == nil {
		standardTypes = quality.DefaultStandardDecisionTypes
	}
	s := &Server{
		db:                          db,
		decisionSvc:                 decisionSvc,
		grantCache:                  grantCache,
		logger:                      logger,
		rootsCache:                  newRootsCache(),
		checkCache:                  newCheckCache(),
		highConfidenceWarnThreshold: highConfWarnThreshold,
		standardTypes:               standardTypes,
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
