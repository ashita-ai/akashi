package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/pgvector/pgvector-go"

	"github.com/ashita-ai/kyoyu/internal/model"
	"github.com/ashita-ai/kyoyu/internal/storage"
)

func (s *Server) registerTools() {
	// kyoyu_check — look before you leap.
	s.mcpServer.AddTool(
		mcplib.NewTool("kyoyu_check",
			mcplib.WithDescription(`Check for existing decisions before making a new one.

WHEN TO USE: BEFORE making any decision. This is the most important tool —
it prevents contradictions and lets you build on prior work.

Call this FIRST with the type of decision you're about to make. If precedents
exist, factor them into your reasoning. If conflicts exist, resolve them
explicitly.

WHAT YOU GET BACK:
- has_precedent: whether any prior decisions exist for this type
- decisions: the most relevant prior decisions (up to limit)
- conflicts: any active conflicts on this decision type

EXAMPLE: Before choosing a caching strategy, call kyoyu_check with
decision_type="architecture" to see if anyone already decided on caching.`),
			mcplib.WithReadOnlyHintAnnotation(true),
			mcplib.WithIdempotentHintAnnotation(true),
			mcplib.WithOpenWorldHintAnnotation(false),
			mcplib.WithString("decision_type",
				mcplib.Description("The type of decision you're about to make"),
				mcplib.Required(),
				mcplib.Enum("model_selection", "architecture", "data_source", "error_handling", "feature_scope", "trade_off", "deployment", "security"),
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

	// kyoyu_trace — record a decision.
	s.mcpServer.AddTool(
		mcplib.NewTool("kyoyu_trace",
			mcplib.WithDescription(`Record a decision you just made so other agents can learn from it.

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
				mcplib.Description("Your agent identifier — who is making this decision"),
				mcplib.Required(),
			),
			mcplib.WithString("decision_type",
				mcplib.Description("Category of decision. Use a standard type when possible."),
				mcplib.Required(),
				mcplib.Enum("model_selection", "architecture", "data_source", "error_handling", "feature_scope", "trade_off", "deployment", "security"),
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

	// kyoyu_query — structured query over past decisions.
	s.mcpServer.AddTool(
		mcplib.NewTool("kyoyu_query",
			mcplib.WithDescription(`Query past decisions with structured filters.

WHEN TO USE: When you need to find specific decisions by exact criteria —
a particular agent, decision type, confidence threshold, or outcome.
For fuzzy/semantic searches, use kyoyu_search instead.

FILTER EXAMPLES:
- All architecture decisions: decision_type="architecture"
- High-confidence decisions by agent-7: agent_id="agent-7", confidence_min=0.8
- Specific outcome: outcome="chose PostgreSQL"
- Combined: decision_type="model_selection", confidence_min=0.7, limit=20`),
			mcplib.WithReadOnlyHintAnnotation(true),
			mcplib.WithIdempotentHintAnnotation(true),
			mcplib.WithOpenWorldHintAnnotation(false),
			mcplib.WithString("decision_type",
				mcplib.Description("Filter by decision type"),
				mcplib.Enum("model_selection", "architecture", "data_source", "error_handling", "feature_scope", "trade_off", "deployment", "security"),
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

	// kyoyu_search — semantic similarity search.
	s.mcpServer.AddTool(
		mcplib.NewTool("kyoyu_search",
			mcplib.WithDescription(`Search decision history by semantic similarity.

WHEN TO USE: When you have a natural language question about past decisions
and want to find the most relevant matches regardless of exact wording.
For exact-match filtering, use kyoyu_query instead.

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

	// kyoyu_recent — what happened recently.
	s.mcpServer.AddTool(
		mcplib.NewTool("kyoyu_recent",
			mcplib.WithDescription(`Get the most recent decisions across all agents.

WHEN TO USE: To get a quick overview of what's been decided recently.
Useful at the start of a session to understand current context,
or to check what other agents have been doing.

Returns decisions ordered by time (newest first) with optional
filters for agent_id and decision_type.`),
			mcplib.WithReadOnlyHintAnnotation(true),
			mcplib.WithIdempotentHintAnnotation(true),
			mcplib.WithOpenWorldHintAnnotation(false),
			mcplib.WithString("agent_id",
				mcplib.Description("Optional: only show decisions from a specific agent"),
			),
			mcplib.WithString("decision_type",
				mcplib.Description("Optional: only show decisions of a specific type"),
				mcplib.Enum("model_selection", "architecture", "data_source", "error_handling", "feature_scope", "trade_off", "deployment", "security"),
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
	decisionType := request.GetString("decision_type", "")
	if decisionType == "" {
		return errorResult("decision_type is required"), nil
	}

	query := request.GetString("query", "")
	agentID := request.GetString("agent_id", "")
	limit := request.GetInt("limit", 5)

	var decisions []model.Decision

	if query != "" {
		// Semantic search path.
		queryEmb, err := s.embedder.Embed(ctx, query)
		if err != nil {
			return errorResult(fmt.Sprintf("failed to generate embedding: %v", err)), nil
		}

		filters := model.QueryFilters{
			DecisionType: &decisionType,
		}
		if agentID != "" {
			filters.AgentIDs = []string{agentID}
		}

		results, err := s.db.SearchDecisionsByEmbedding(ctx, queryEmb, filters, limit)
		if err != nil {
			return errorResult(fmt.Sprintf("search failed: %v", err)), nil
		}
		for _, sr := range results {
			decisions = append(decisions, sr.Decision)
		}
	} else {
		// Structured query path.
		filters := model.QueryFilters{
			DecisionType: &decisionType,
		}
		if agentID != "" {
			filters.AgentIDs = []string{agentID}
		}

		queried, _, err := s.db.QueryDecisions(ctx, model.QueryRequest{
			Filters:  filters,
			Include:  []string{"alternatives"},
			OrderBy:  "valid_from",
			OrderDir: "desc",
			Limit:    limit,
		})
		if err != nil {
			return errorResult(fmt.Sprintf("query failed: %v", err)), nil
		}
		decisions = queried
	}

	// Always check for conflicts.
	conflicts, err := s.db.ListConflicts(ctx, &decisionType, limit)
	if err != nil {
		s.logger.Warn("mcp: check conflicts failed", "error", err)
		conflicts = nil
	}

	resp := model.CheckResponse{
		HasPrecedent: len(decisions) > 0,
		Decisions:    decisions,
		Conflicts:    conflicts,
	}

	resultData, _ := json.MarshalIndent(resp, "", "  ")
	return &mcplib.CallToolResult{
		Content: []mcplib.Content{
			mcplib.TextContent{Type: "text", Text: string(resultData)},
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

func (s *Server) handleRecent(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	limit := request.GetInt("limit", 10)

	filters := model.QueryFilters{}
	if agentID := request.GetString("agent_id", ""); agentID != "" {
		filters.AgentIDs = []string{agentID}
	}
	if dt := request.GetString("decision_type", ""); dt != "" {
		filters.DecisionType = &dt
	}

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
