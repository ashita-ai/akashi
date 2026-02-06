package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/ashita-ai/akashi/internal/ctxutil"
	"github.com/ashita-ai/akashi/internal/model"
)

func (s *Server) registerResources() {
	// akashi://session/current — current session context from the black box.
	s.mcpServer.AddResource(
		mcplib.NewResource(
			"akashi://session/current",
			"Current Session",
			mcplib.WithResourceDescription("Recent decisions from the black box for the current session"),
			mcplib.WithMIMEType("application/json"),
		),
		s.handleSessionCurrent,
	)

	// akashi://decisions/recent — recent decision records across accessible agents.
	s.mcpServer.AddResource(
		mcplib.NewResource(
			"akashi://decisions/recent",
			"Recent Decisions",
			mcplib.WithResourceDescription("Recent decision records from the black box across all accessible agents"),
			mcplib.WithMIMEType("application/json"),
		),
		s.handleDecisionsRecent,
	)

	// akashi://agent/{id}/history — specific agent's decision audit trail.
	s.mcpServer.AddResourceTemplate(
		mcplib.NewResourceTemplate(
			"akashi://agent/{id}/history",
			"Agent History",
			mcplib.WithTemplateDescription("Decision audit trail for a specific agent"),
			mcplib.WithTemplateMIMEType("application/json"),
		),
		s.handleAgentHistory,
	)
}

func (s *Server) handleSessionCurrent(ctx context.Context, request mcplib.ReadResourceRequest) ([]mcplib.ResourceContents, error) {
	orgID := ctxutil.OrgIDFromContext(ctx)

	// Return recent decisions across all agents (limited).
	decisions, _, err := s.db.QueryDecisions(ctx, orgID, model.QueryRequest{
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
			URI:      "akashi://session/current",
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func (s *Server) handleDecisionsRecent(ctx context.Context, request mcplib.ReadResourceRequest) ([]mcplib.ResourceContents, error) {
	orgID := ctxutil.OrgIDFromContext(ctx)

	decisions, _, err := s.db.QueryDecisions(ctx, orgID, model.QueryRequest{
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
			URI:      "akashi://decisions/recent",
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func (s *Server) handleAgentHistory(ctx context.Context, request mcplib.ReadResourceRequest) ([]mcplib.ResourceContents, error) {
	orgID := ctxutil.OrgIDFromContext(ctx)

	// Extract agent_id from the URI template parameter.
	uri := request.Params.URI
	// Parse agent_id from akashi://agent/{id}/history
	var agentID string
	_, err := fmt.Sscanf(uri, "akashi://agent/%s/history", &agentID)
	if err != nil || agentID == "" {
		return nil, fmt.Errorf("mcp: invalid agent history URI: %s", uri)
	}
	// Remove trailing "/history" if Sscanf grabbed it.
	if len(agentID) > 8 && agentID[len(agentID)-8:] == "/history" {
		agentID = agentID[:len(agentID)-8]
	}

	decisions, _, err := s.db.GetDecisionsByAgent(ctx, orgID, agentID, 20, 0, nil, nil)
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
