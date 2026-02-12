package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/ashita-ai/akashi/internal/authz"
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
	claims := ctxutil.ClaimsFromContext(ctx)

	decisions, _, err := s.db.QueryDecisions(ctx, orgID, model.QueryRequest{
		OrderBy:  "valid_from",
		OrderDir: "desc",
		Limit:    10,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: session current: %w", err)
	}

	// Apply access filtering.
	if claims != nil {
		decisions, err = authz.FilterDecisions(ctx, s.db, claims, decisions)
		if err != nil {
			return nil, fmt.Errorf("mcp: session current: access filter: %w", err)
		}
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
	claims := ctxutil.ClaimsFromContext(ctx)

	decisions, _, err := s.db.QueryDecisions(ctx, orgID, model.QueryRequest{
		OrderBy:  "valid_from",
		OrderDir: "desc",
		Limit:    20,
		Include:  []string{"alternatives"},
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: recent decisions: %w", err)
	}

	// Apply access filtering.
	if claims != nil {
		decisions, err = authz.FilterDecisions(ctx, s.db, claims, decisions)
		if err != nil {
			return nil, fmt.Errorf("mcp: recent decisions: access filter: %w", err)
		}
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
	claims := ctxutil.ClaimsFromContext(ctx)

	// Extract agent_id from URI: akashi://agent/{id}/history
	uri := request.Params.URI
	agentID, err := parseAgentHistoryURI(uri)
	if err != nil {
		return nil, err
	}

	if err := model.ValidateAgentID(agentID); err != nil {
		return nil, fmt.Errorf("mcp: invalid agent_id in URI: %w", err)
	}

	// Check access before querying.
	if claims != nil {
		ok, err := authz.CanAccessAgent(ctx, s.db, claims, agentID)
		if err != nil {
			return nil, fmt.Errorf("mcp: agent history: access check: %w", err)
		}
		if !ok {
			return nil, fmt.Errorf("mcp: no access to agent %q", agentID)
		}
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

// parseAgentHistoryURI extracts the agent_id from "akashi://agent/{id}/history".
// Uses string splitting instead of fmt.Sscanf to correctly handle agent IDs
// that contain characters Sscanf would misparse.
func parseAgentHistoryURI(uri string) (string, error) {
	// Expected: akashi://agent/{id}/history
	const prefix = "akashi://agent/"
	const suffix = "/history"

	if !strings.HasPrefix(uri, prefix) || !strings.HasSuffix(uri, suffix) {
		return "", fmt.Errorf("mcp: invalid agent history URI: %s", uri)
	}

	agentID := uri[len(prefix) : len(uri)-len(suffix)]
	if agentID == "" {
		return "", fmt.Errorf("mcp: empty agent_id in URI: %s", uri)
	}

	return agentID, nil
}
