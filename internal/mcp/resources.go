package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/ashita-ai/kyoyu/internal/model"
)

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
