// Package mcp implements the Model Context Protocol server for Kyoyu.
//
// The MCP server exposes the same capabilities as the HTTP API through
// MCP resources and tools, allowing MCP-compatible AI agents to interact
// with Kyoyu's decision trace infrastructure.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/pgvector/pgvector-go"

	"github.com/ashita-ai/kyoyu/internal/model"
	"github.com/ashita-ai/kyoyu/internal/service/embedding"
	"github.com/ashita-ai/kyoyu/internal/storage"
)

// Server wraps the MCP server with Kyoyu's service layer.
type Server struct {
	mcpServer *mcpserver.MCPServer
	db        *storage.DB
	embedder  embedding.Provider
	logger    *slog.Logger
}

// New creates and configures a new MCP server with all resources and tools.
func New(db *storage.DB, embedder embedding.Provider, logger *slog.Logger) *Server {
	s := &Server{
		db:       db,
		embedder: embedder,
		logger:   logger,
	}

	s.mcpServer = mcpserver.NewMCPServer(
		"kyoyu",
		"0.1.0",
		mcpserver.WithResourceCapabilities(true, true),
		mcpserver.WithToolCapabilities(true),
	)

	s.registerResources()
	s.registerTools()

	return s
}

// MCPServer returns the underlying mcp-go server for transport setup.
func (s *Server) MCPServer() *mcpserver.MCPServer {
	return s.mcpServer
}

func (s *Server) registerResources() {
	// kyoyu://session/current — current session context for the requesting agent.
	s.mcpServer.AddResource(
		mcplib.NewResource(
			"kyoyu://session/current",
			"Current Session",
			mcplib.WithResourceDescription("Current session context for the requesting agent"),
			mcplib.WithMIMEType("application/json"),
		),
		s.handleSessionCurrent,
	)

	// kyoyu://decisions/recent — recent decisions across accessible agents.
	s.mcpServer.AddResource(
		mcplib.NewResource(
			"kyoyu://decisions/recent",
			"Recent Decisions",
			mcplib.WithResourceDescription("Recent decisions across all accessible agents"),
			mcplib.WithMIMEType("application/json"),
		),
		s.handleDecisionsRecent,
	)

	// kyoyu://agent/{id}/history — specific agent's decision history.
	s.mcpServer.AddResourceTemplate(
		mcplib.NewResourceTemplate(
			"kyoyu://agent/{id}/history",
			"Agent History",
			mcplib.WithTemplateDescription("Decision history for a specific agent"),
			mcplib.WithTemplateMIMEType("application/json"),
		),
		s.handleAgentHistory,
	)
}

func (s *Server) registerTools() {
	// kyoyu_trace — record a decision trace.
	s.mcpServer.AddTool(
		mcplib.NewTool("kyoyu_trace",
			mcplib.WithDescription("Record a structured decision trace with alternatives, evidence, and confidence"),
			mcplib.WithString("agent_id", mcplib.Description("Agent identifier"), mcplib.Required()),
			mcplib.WithString("decision_type", mcplib.Description("Category of decision"), mcplib.Required()),
			mcplib.WithString("outcome", mcplib.Description("What was decided"), mcplib.Required()),
			mcplib.WithNumber("confidence", mcplib.Description("Confidence score 0.0-1.0"), mcplib.Required()),
			mcplib.WithString("reasoning", mcplib.Description("Step-by-step reasoning chain")),
		),
		s.handleTrace,
	)

	// kyoyu_query — structured query over past decisions.
	s.mcpServer.AddTool(
		mcplib.NewTool("kyoyu_query",
			mcplib.WithDescription("Query past decisions with structured filters, time ranges, and result ordering"),
			mcplib.WithString("decision_type", mcplib.Description("Filter by decision type")),
			mcplib.WithString("agent_id", mcplib.Description("Filter by agent ID")),
			mcplib.WithString("outcome", mcplib.Description("Filter by outcome")),
			mcplib.WithNumber("confidence_min", mcplib.Description("Minimum confidence threshold")),
			mcplib.WithNumber("limit", mcplib.Description("Maximum results to return")),
		),
		s.handleQuery,
	)

	// kyoyu_search — semantic similarity search.
	s.mcpServer.AddTool(
		mcplib.NewTool("kyoyu_search",
			mcplib.WithDescription("Search decision history by semantic similarity. Find precedents and related decisions."),
			mcplib.WithString("query", mcplib.Description("Natural language search query"), mcplib.Required()),
			mcplib.WithNumber("limit", mcplib.Description("Maximum results to return")),
			mcplib.WithNumber("confidence_min", mcplib.Description("Minimum confidence threshold")),
		),
		s.handleSearch,
	)
}

func (s *Server) handleSessionCurrent(ctx context.Context, request mcplib.ReadResourceRequest) ([]mcplib.ResourceContents, error) {
	// Return recent decisions across all agents (limited).
	decisions, _, err := s.db.QueryDecisions(ctx, model.QueryRequest{
		OrderBy:  "valid_from",
		OrderDir: "desc",
		Limit:    10,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: session current: %w", err)
	}

	data, err := json.MarshalIndent(decisions, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal session: %w", err)
	}

	return []mcplib.ResourceContents{
		mcplib.TextResourceContents{
			URI:      "kyoyu://session/current",
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func (s *Server) handleDecisionsRecent(ctx context.Context, request mcplib.ReadResourceRequest) ([]mcplib.ResourceContents, error) {
	decisions, _, err := s.db.QueryDecisions(ctx, model.QueryRequest{
		OrderBy:  "valid_from",
		OrderDir: "desc",
		Limit:    20,
		Include:  []string{"alternatives"},
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: recent decisions: %w", err)
	}

	data, err := json.MarshalIndent(decisions, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal decisions: %w", err)
	}

	return []mcplib.ResourceContents{
		mcplib.TextResourceContents{
			URI:      "kyoyu://decisions/recent",
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func (s *Server) handleAgentHistory(ctx context.Context, request mcplib.ReadResourceRequest) ([]mcplib.ResourceContents, error) {
	// Extract agent_id from the URI template parameter.
	uri := request.Params.URI
	// Parse agent_id from kyoyu://agent/{id}/history
	var agentID string
	_, err := fmt.Sscanf(uri, "kyoyu://agent/%s/history", &agentID)
	if err != nil || agentID == "" {
		return nil, fmt.Errorf("mcp: invalid agent history URI: %s", uri)
	}
	// Remove trailing "/history" if Sscanf grabbed it.
	if len(agentID) > 8 && agentID[len(agentID)-8:] == "/history" {
		agentID = agentID[:len(agentID)-8]
	}

	decisions, _, err := s.db.GetDecisionsByAgent(ctx, agentID, 20, 0, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: agent history: %w", err)
	}

	data, err := json.MarshalIndent(map[string]any{
		"agent_id":  agentID,
		"decisions": decisions,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal history: %w", err)
	}

	return []mcplib.ResourceContents{
		mcplib.TextResourceContents{
			URI:      uri,
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func (s *Server) handleTrace(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	agentID := request.GetString("agent_id", "")
	decisionType := request.GetString("decision_type", "")
	outcome := request.GetString("outcome", "")
	confidence := float32(request.GetFloat("confidence", 0))
	reasoning := request.GetString("reasoning", "")

	if agentID == "" || decisionType == "" || outcome == "" {
		return errorResult("agent_id, decision_type, and outcome are required"), nil
	}

	// Create run.
	run, err := s.db.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	if err != nil {
		return errorResult(fmt.Sprintf("failed to create run: %v", err)), nil
	}

	// Generate embedding.
	embText := decisionType + ": " + outcome
	if reasoning != "" {
		embText += " " + reasoning
	}
	var decisionEmb *pgvector.Vector
	emb, err := s.embedder.Embed(ctx, embText)
	if err != nil {
		s.logger.Warn("mcp: embedding failed", "error", err)
	} else {
		decisionEmb = &emb
	}

	// Create decision.
	var reasoningPtr *string
	if reasoning != "" {
		reasoningPtr = &reasoning
	}
	decision, err := s.db.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: decisionType,
		Outcome:      outcome,
		Confidence:   confidence,
		Reasoning:    reasoningPtr,
		Embedding:    decisionEmb,
	})
	if err != nil {
		return errorResult(fmt.Sprintf("failed to create decision: %v", err)), nil
	}

	// Complete run.
	_ = s.db.CompleteRun(ctx, run.ID, model.RunStatusCompleted, nil)

	// Notify.
	notifyPayload, _ := json.Marshal(map[string]any{
		"decision_id": decision.ID, "agent_id": agentID, "outcome": outcome,
	})
	_ = s.db.Notify(ctx, storage.ChannelDecisions, string(notifyPayload))

	resultData, _ := json.Marshal(map[string]any{
		"run_id":      run.ID,
		"decision_id": decision.ID,
		"status":      "recorded",
	})

	return &mcplib.CallToolResult{
		Content: []mcplib.Content{
			mcplib.TextContent{Type: "text", Text: string(resultData)},
		},
	}, nil
}

func (s *Server) handleQuery(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	filters := model.QueryFilters{}

	if agentID := request.GetString("agent_id", ""); agentID != "" {
		filters.AgentIDs = []string{agentID}
	}
	if dt := request.GetString("decision_type", ""); dt != "" {
		filters.DecisionType = &dt
	}
	if outcome := request.GetString("outcome", ""); outcome != "" {
		filters.Outcome = &outcome
	}
	if confMin := float32(request.GetFloat("confidence_min", 0)); confMin > 0 {
		filters.ConfidenceMin = &confMin
	}

	limit := request.GetInt("limit", 10)

	decisions, total, err := s.db.QueryDecisions(ctx, model.QueryRequest{
		Filters:  filters,
		Include:  []string{"alternatives"},
		OrderBy:  "valid_from",
		OrderDir: "desc",
		Limit:    limit,
	})
	if err != nil {
		return errorResult(fmt.Sprintf("query failed: %v", err)), nil
	}

	resultData, _ := json.MarshalIndent(map[string]any{
		"decisions": decisions,
		"total":     total,
	}, "", "  ")

	return &mcplib.CallToolResult{
		Content: []mcplib.Content{
			mcplib.TextContent{Type: "text", Text: string(resultData)},
		},
	}, nil
}

func (s *Server) handleSearch(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	query := request.GetString("query", "")
	if query == "" {
		return errorResult("query is required"), nil
	}

	limit := request.GetInt("limit", 5)
	filters := model.QueryFilters{}
	if confMin := float32(request.GetFloat("confidence_min", 0)); confMin > 0 {
		filters.ConfidenceMin = &confMin
	}

	queryEmb, err := s.embedder.Embed(ctx, query)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to generate embedding: %v", err)), nil
	}

	results, err := s.db.SearchDecisionsByEmbedding(ctx, queryEmb, filters, limit)
	if err != nil {
		return errorResult(fmt.Sprintf("search failed: %v", err)), nil
	}

	resultData, _ := json.MarshalIndent(map[string]any{
		"results": results,
		"total":   len(results),
	}, "", "  ")

	return &mcplib.CallToolResult{
		Content: []mcplib.Content{
			mcplib.TextContent{Type: "text", Text: string(resultData)},
		},
	}, nil
}

func errorResult(msg string) *mcplib.CallToolResult {
	return &mcplib.CallToolResult{
		Content: []mcplib.Content{
			mcplib.TextContent{Type: "text", Text: msg},
		},
		IsError: true,
	}
}
