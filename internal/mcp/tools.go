package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/ashita-ai/akashi/internal/authz"
	"github.com/ashita-ai/akashi/internal/ctxutil"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/decisions"
)

func (s *Server) registerTools() {
	// akashi_check — check the black box for decision precedents.
	s.mcpServer.AddTool(
		mcplib.NewTool("akashi_check",
			mcplib.WithDescription(`Check the black box for decision precedents before making a new one.

WHEN TO USE: BEFORE making any decision. This is the most important tool —
it prevents contradictions and lets you build on prior work.

Call this FIRST with the type of decision you're about to make. If the audit
trail shows precedents, factor them into your reasoning. If conflicts exist,
resolve them explicitly.

WHAT YOU GET BACK:
- has_precedent: whether any prior decisions exist for this type
- decisions: the most relevant prior decisions (up to limit)
- conflicts: any active conflicts on this decision type

EXAMPLE: Before choosing a caching strategy, call akashi_check with
decision_type="architecture" to see if anyone already decided on caching.`),
			mcplib.WithReadOnlyHintAnnotation(true),
			mcplib.WithIdempotentHintAnnotation(true),
			mcplib.WithOpenWorldHintAnnotation(false),
			mcplib.WithString("decision_type",
				mcplib.Description("The type of decision you're about to make. Common types: architecture, security, code_review, investigation, planning, assessment, trade_off, feature_scope, deployment, error_handling, model_selection, data_source. Any string is accepted."),
				mcplib.Required(),
			),
			mcplib.WithString("query",
				mcplib.Description("Optional natural language query for semantic search over past decisions. If omitted, returns recent decisions of this type."),
			),
			mcplib.WithString("agent_id",
				mcplib.Description("Optional: only check decisions from a specific agent"),
			),
			mcplib.WithNumber("limit",
				mcplib.Description("Maximum number of precedents to return"),
				mcplib.Min(1),
				mcplib.Max(100),
				mcplib.DefaultNumber(5),
			),
		),
		s.handleCheck,
	)

	// akashi_trace — record a decision to the black box.
	s.mcpServer.AddTool(
		mcplib.NewTool("akashi_trace",
			mcplib.WithDescription(`Record a decision to the black box so there is proof of why it was made.

IMPORTANT: Call akashi_check FIRST to look for existing precedents before
recording. Tracing without checking risks contradicting prior decisions
and duplicating work that was already done.

WHEN TO USE: After you make any non-trivial decision — choosing a model,
selecting an approach, picking a data source, resolving an ambiguity,
or committing to a course of action.

WHAT TO INCLUDE:
- decision_type: A short category (see enum for standard types)
- outcome: What you decided, stated as a fact ("chose gpt-4o for summarization")
- confidence: How certain you are (0.0-1.0). Be honest — 0.6 is fine.
- reasoning: Your chain of thought. Why this choice over alternatives?

EXAMPLE: After choosing a caching strategy, record decision_type="architecture",
outcome="chose Redis with 5min TTL for session cache", confidence=0.85,
reasoning="Redis handles our expected QPS, TTL prevents stale reads"`),
			mcplib.WithDestructiveHintAnnotation(false),
			mcplib.WithIdempotentHintAnnotation(false),
			mcplib.WithOpenWorldHintAnnotation(true),
			mcplib.WithString("agent_id",
				mcplib.Description("Your agent identifier — who is making this decision. Defaults to your authenticated identity if omitted."),
			),
			mcplib.WithString("decision_type",
				mcplib.Description("Category of decision. Common types: architecture, security, code_review, investigation, planning, assessment, trade_off, feature_scope, deployment, error_handling, model_selection, data_source. Any string is accepted."),
				mcplib.Required(),
			),
			mcplib.WithString("outcome",
				mcplib.Description("What you decided, stated as a fact. Be specific: 'chose Redis with 5min TTL' not 'picked a cache'"),
				mcplib.Required(),
			),
			mcplib.WithNumber("confidence",
				mcplib.Description("How certain you are about this decision (0.0 = guessing, 1.0 = certain)"),
				mcplib.Required(),
				mcplib.Min(0),
				mcplib.Max(1),
			),
			mcplib.WithString("reasoning",
				mcplib.Description("Your chain of thought. Why this choice? What trade-offs did you consider?"),
			),
		),
		s.handleTrace,
	)

	// akashi_query — structured query over the decision audit trail.
	s.mcpServer.AddTool(
		mcplib.NewTool("akashi_query",
			mcplib.WithDescription(`Query the decision audit trail with structured filters.

WHEN TO USE: When you need to find specific decisions by exact criteria —
a particular agent, decision type, confidence threshold, or outcome.
For fuzzy/semantic searches, use akashi_search instead.

FILTER EXAMPLES:
- All architecture decisions: decision_type="architecture"
- High-confidence decisions by agent-7: agent_id="agent-7", confidence_min=0.8
- Specific outcome: outcome="chose PostgreSQL"
- Combined: decision_type="model_selection", confidence_min=0.7, limit=20`),
			mcplib.WithReadOnlyHintAnnotation(true),
			mcplib.WithIdempotentHintAnnotation(true),
			mcplib.WithOpenWorldHintAnnotation(false),
			mcplib.WithString("decision_type",
				mcplib.Description("Filter by decision type (any string, e.g. architecture, security, code_review)"),
			),
			mcplib.WithString("agent_id",
				mcplib.Description("Filter by agent ID — whose decisions to look at"),
			),
			mcplib.WithString("outcome",
				mcplib.Description("Filter by exact outcome text"),
			),
			mcplib.WithNumber("confidence_min",
				mcplib.Description("Minimum confidence threshold (0.0-1.0). Use 0.7+ for reliable decisions."),
				mcplib.Min(0),
				mcplib.Max(1),
			),
			mcplib.WithNumber("limit",
				mcplib.Description("Maximum results to return"),
				mcplib.Min(1),
				mcplib.Max(100),
				mcplib.DefaultNumber(10),
			),
		),
		s.handleQuery,
	)

	// akashi_search — search the black box for similar past decisions.
	s.mcpServer.AddTool(
		mcplib.NewTool("akashi_search",
			mcplib.WithDescription(`Search the black box for similar past decisions by semantic similarity.

WHEN TO USE: When you have a natural language question about past decisions
and want to find the most relevant matches regardless of exact wording.
For exact-match filtering, use akashi_query instead.

EXAMPLE QUERIES:
- "How did we handle rate limiting?"
- "What caching decisions were made?"
- "Previous choices about error handling in the payment flow"
- "Model selection for text summarization tasks"`),
			mcplib.WithReadOnlyHintAnnotation(true),
			mcplib.WithIdempotentHintAnnotation(true),
			mcplib.WithOpenWorldHintAnnotation(false),
			mcplib.WithString("query",
				mcplib.Description("Natural language search query — describe what you're looking for"),
				mcplib.Required(),
			),
			mcplib.WithNumber("limit",
				mcplib.Description("Maximum results to return"),
				mcplib.Min(1),
				mcplib.Max(100),
				mcplib.DefaultNumber(5),
			),
			mcplib.WithNumber("confidence_min",
				mcplib.Description("Minimum confidence threshold for results (0.0-1.0)"),
				mcplib.Min(0),
				mcplib.Max(1),
			),
		),
		s.handleSearch,
	)

	// akashi_recent — see what the black box recorded recently.
	s.mcpServer.AddTool(
		mcplib.NewTool("akashi_recent",
			mcplib.WithDescription(`See what the black box recorded recently across all agents.

WHEN TO USE: To get a quick overview of what's been decided recently.
Useful at the start of a session to understand current context,
or to review what other agents have been doing.

Returns decisions ordered by time (newest first) with optional
filters for agent_id and decision_type.`),
			mcplib.WithReadOnlyHintAnnotation(true),
			mcplib.WithIdempotentHintAnnotation(true),
			mcplib.WithOpenWorldHintAnnotation(false),
			mcplib.WithString("agent_id",
				mcplib.Description("Optional: only show decisions from a specific agent"),
			),
			mcplib.WithString("decision_type",
				mcplib.Description("Optional: only show decisions of a specific type (any string, e.g. architecture, security, code_review)"),
			),
			mcplib.WithNumber("limit",
				mcplib.Description("Maximum results to return"),
				mcplib.Min(1),
				mcplib.Max(100),
				mcplib.DefaultNumber(10),
			),
		),
		s.handleRecent,
	)
}

func (s *Server) handleCheck(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	orgID := ctxutil.OrgIDFromContext(ctx)
	claims := ctxutil.ClaimsFromContext(ctx)

	decisionType := request.GetString("decision_type", "")
	if decisionType == "" {
		return errorResult("decision_type is required"), nil
	}

	// Record that this caller checked precedents for this decision type.
	// handleTrace uses this to detect the check-before-trace workflow.
	if claims != nil {
		s.checkTracker.Record(claims.AgentID, decisionType)
	}

	query := request.GetString("query", "")
	agentID := request.GetString("agent_id", "")
	limit := request.GetInt("limit", 5)

	resp, err := s.decisionSvc.Check(ctx, orgID, decisionType, query, agentID, limit)
	if err != nil {
		return errorResult(fmt.Sprintf("check failed: %v", err)), nil
	}

	// Apply access filtering (same as HTTP handlers).
	if claims != nil {
		resp.Decisions, err = authz.FilterDecisions(ctx, s.db, claims, resp.Decisions)
		if err != nil {
			return errorResult(fmt.Sprintf("authorization check failed: %v", err)), nil
		}
		resp.Conflicts, err = authz.FilterConflicts(ctx, s.db, claims, resp.Conflicts)
		if err != nil {
			return errorResult(fmt.Sprintf("authorization check failed: %v", err)), nil
		}
		resp.HasPrecedent = len(resp.Decisions) > 0
	}

	resultData, _ := json.MarshalIndent(resp, "", "  ")
	return &mcplib.CallToolResult{
		Content: []mcplib.Content{
			mcplib.TextContent{Type: "text", Text: string(resultData)},
		},
	}, nil
}

func (s *Server) handleTrace(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	orgID := ctxutil.OrgIDFromContext(ctx)
	claims := ctxutil.ClaimsFromContext(ctx)

	agentID := request.GetString("agent_id", "")
	decisionType := request.GetString("decision_type", "")
	outcome := request.GetString("outcome", "")
	confidence := float32(request.GetFloat("confidence", 0))
	reasoning := request.GetString("reasoning", "")

	// Default agent_id to the caller's authenticated identity.
	if agentID == "" {
		if claims != nil {
			agentID = claims.AgentID
		} else {
			return errorResult("agent_id is required"), nil
		}
	}

	if decisionType == "" || outcome == "" {
		return errorResult("decision_type and outcome are required"), nil
	}

	// Validate agent_id format (same as HTTP handler).
	if err := model.ValidateAgentID(agentID); err != nil {
		return errorResult(fmt.Sprintf("invalid agent_id: %v", err)), nil
	}

	// Non-admin callers can only trace for their own agent_id.
	if claims != nil && !model.RoleAtLeast(claims.Role, model.RoleAdmin) && agentID != claims.AgentID {
		return errorResult("agents can only record decisions for their own agent_id"), nil
	}

	// Verify the agent exists within the org, auto-registering if the caller
	// is admin+ and the agent is new (reduces friction for first-time traces).
	callerRole := model.AgentRole("")
	if claims != nil {
		callerRole = claims.Role
	}
	if err := s.decisionSvc.ResolveOrCreateAgent(ctx, orgID, agentID, callerRole); err != nil {
		return errorResult(err.Error()), nil
	}

	var reasoningPtr *string
	if reasoning != "" {
		reasoningPtr = &reasoning
	}

	result, err := s.decisionSvc.Trace(ctx, orgID, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: decisionType,
			Outcome:      outcome,
			Confidence:   confidence,
			Reasoning:    reasoningPtr,
		},
	})
	if err != nil {
		return errorResult(fmt.Sprintf("failed to record decision: %v", err)), nil
	}

	resultData, _ := json.Marshal(map[string]any{
		"run_id":      result.RunID,
		"decision_id": result.DecisionID,
		"status":      "recorded",
	})

	contents := []mcplib.Content{
		mcplib.TextContent{Type: "text", Text: string(resultData)},
	}

	// Nudge: if the caller didn't call akashi_check for this decision_type
	// recently, include a reminder. The trace still succeeds — this is
	// advisory, not a gate.
	callerID := ""
	if claims != nil {
		callerID = claims.AgentID
	}
	if callerID != "" && !s.checkTracker.WasChecked(callerID, decisionType) {
		contents = append(contents, mcplib.TextContent{
			Type: "text",
			Text: "NOTE: No akashi_check was called for decision_type=\"" + decisionType + "\" before this trace. " +
				"Checking for precedents first prevents contradictions and duplicate work. " +
				"Next time, call akashi_check before akashi_trace.",
		})
	}

	return &mcplib.CallToolResult{Content: contents}, nil
}

func (s *Server) handleQuery(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	orgID := ctxutil.OrgIDFromContext(ctx)
	claims := ctxutil.ClaimsFromContext(ctx)
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

	decs, total, err := s.decisionSvc.Query(ctx, orgID, model.QueryRequest{
		Filters:  filters,
		Include:  []string{"alternatives"},
		OrderBy:  "valid_from",
		OrderDir: "desc",
		Limit:    limit,
	})
	if err != nil {
		return errorResult(fmt.Sprintf("query failed: %v", err)), nil
	}

	// Apply access filtering and adjust total to match filtered results.
	// Without this adjustment, the unfiltered DB total leaks the count of
	// decisions the caller cannot see (same fix as the HTTP handler).
	if claims != nil {
		preFilterCount := len(decs)
		decs, err = authz.FilterDecisions(ctx, s.db, claims, decs)
		if err != nil {
			return errorResult(fmt.Sprintf("authorization check failed: %v", err)), nil
		}
		if len(decs) < preFilterCount {
			total = len(decs)
		}
	}

	resultData, _ := json.MarshalIndent(map[string]any{
		"decisions": decs,
		"total":     total,
	}, "", "  ")

	return &mcplib.CallToolResult{
		Content: []mcplib.Content{
			mcplib.TextContent{Type: "text", Text: string(resultData)},
		},
	}, nil
}

func (s *Server) handleSearch(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	orgID := ctxutil.OrgIDFromContext(ctx)
	claims := ctxutil.ClaimsFromContext(ctx)

	query := request.GetString("query", "")
	if query == "" {
		return errorResult("query is required"), nil
	}

	limit := request.GetInt("limit", 5)
	filters := model.QueryFilters{}
	if confMin := float32(request.GetFloat("confidence_min", 0)); confMin > 0 {
		filters.ConfidenceMin = &confMin
	}

	results, err := s.decisionSvc.Search(ctx, orgID, query, true, filters, limit)
	if err != nil {
		return errorResult(fmt.Sprintf("search failed: %v", err)), nil
	}

	// Apply access filtering.
	if claims != nil {
		results, err = authz.FilterSearchResults(ctx, s.db, claims, results)
		if err != nil {
			return errorResult(fmt.Sprintf("authorization check failed: %v", err)), nil
		}
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

func (s *Server) handleRecent(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	orgID := ctxutil.OrgIDFromContext(ctx)
	claims := ctxutil.ClaimsFromContext(ctx)
	limit := request.GetInt("limit", 10)

	filters := model.QueryFilters{}
	if agentID := request.GetString("agent_id", ""); agentID != "" {
		filters.AgentIDs = []string{agentID}
	}
	if dt := request.GetString("decision_type", ""); dt != "" {
		filters.DecisionType = &dt
	}

	decs, _, err := s.decisionSvc.Recent(ctx, orgID, filters, limit, 0)
	if err != nil {
		return errorResult(fmt.Sprintf("query failed: %v", err)), nil
	}

	// Apply access filtering.
	if claims != nil {
		decs, err = authz.FilterDecisions(ctx, s.db, claims, decs)
		if err != nil {
			return errorResult(fmt.Sprintf("authorization check failed: %v", err)), nil
		}
	}

	resultData, _ := json.MarshalIndent(map[string]any{
		"decisions": decs,
		"total":     len(decs),
	}, "", "  ")

	return &mcplib.CallToolResult{
		Content: []mcplib.Content{
			mcplib.TextContent{Type: "text", Text: string(resultData)},
		},
	}, nil
}
