package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/ashita-ai/akashi/internal/authz"
	"github.com/ashita-ai/akashi/internal/ctxutil"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/tracehealth"
	"github.com/ashita-ai/akashi/internal/storage"
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
			mcplib.WithString("format",
				mcplib.Description(`Response format: "concise" (default) returns summary + action_needed + compact decisions. "full" returns complete decision objects.`),
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
reasoning="Redis handles our expected QPS, TTL prevents stale reads"

TRACE AFTER: completing a review, choosing an approach, creating issues/PRs,
finishing a task with choices, making security or access judgments.
SKIP: formatting, typo fixes, running tests, reading code, asking questions.`),
			mcplib.WithDestructiveHintAnnotation(false),
			mcplib.WithIdempotentHintAnnotation(false),
			mcplib.WithOpenWorldHintAnnotation(true),
			mcplib.WithString("agent_id",
				mcplib.Description(`Your role in this task — "reviewer", "coder", "planner", "security-auditor", or similar. Describes what you're doing, not who you authenticate as. Defaults to your authenticated identity if omitted.`),
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
			mcplib.WithString("model",
				mcplib.Description(`The model powering you (e.g. "claude-opus-4-6", "gpt-4o"). Helps distinguish decisions by capability tier.`),
			),
			mcplib.WithString("task",
				mcplib.Description(`What you're working on (e.g. "codebase review", "implement rate limiting"). Groups related decisions.`),
			),
			mcplib.WithString("idempotency_key",
				mcplib.Description("Optional key for retry safety. Same key + same payload replays the original response. Same key + different payload returns an error. Use a UUID or deterministic identifier per logical operation."),
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
			mcplib.WithString("session_id",
				mcplib.Description("Filter by session UUID"),
			),
			mcplib.WithString("tool",
				mcplib.Description("Filter by tool name (e.g. 'claude-code', 'cursor')"),
			),
			mcplib.WithString("model",
				mcplib.Description("Filter by model name (e.g. 'claude-opus-4-6')"),
			),
			mcplib.WithString("repo",
				mcplib.Description("Filter by repository name"),
			),
			mcplib.WithNumber("limit",
				mcplib.Description("Maximum results to return"),
				mcplib.Min(1),
				mcplib.Max(100),
				mcplib.DefaultNumber(10),
			),
			mcplib.WithString("format",
				mcplib.Description(`Response format: "concise" (default) returns compact decisions. "full" returns complete decision objects.`),
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
			mcplib.WithString("format",
				mcplib.Description(`Response format: "concise" (default) returns compact results. "full" returns complete decision objects.`),
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
			mcplib.WithString("session_id",
				mcplib.Description("Optional: filter by session UUID"),
			),
			mcplib.WithString("tool",
				mcplib.Description("Optional: filter by tool name (e.g. 'claude-code', 'cursor')"),
			),
			mcplib.WithString("model",
				mcplib.Description("Optional: filter by model name (e.g. 'claude-opus-4-6')"),
			),
			mcplib.WithNumber("limit",
				mcplib.Description("Maximum results to return"),
				mcplib.Min(1),
				mcplib.Max(100),
				mcplib.DefaultNumber(10),
			),
			mcplib.WithString("format",
				mcplib.Description(`Response format: "concise" (default) returns compact decisions. "full" returns complete decision objects.`),
			),
		),
		s.handleRecent,
	)

	// akashi_stats — aggregate statistics about the decision trail.
	s.mcpServer.AddTool(
		mcplib.NewTool("akashi_stats",
			mcplib.WithDescription(`Get aggregate statistics about the decision audit trail.

WHEN TO USE: To understand the overall health and usage of the decision
trail at a glance. Returns trace health metrics, agent count, conflict
summary, and decision quality statistics.

Useful at the start of a session for situational awareness, or when
reporting on the state of decision tracking.`),
			mcplib.WithReadOnlyHintAnnotation(true),
			mcplib.WithIdempotentHintAnnotation(true),
			mcplib.WithOpenWorldHintAnnotation(false),
		),
		s.handleStats,
	)

	// akashi_conflicts — list and filter conflicts.
	s.mcpServer.AddTool(
		mcplib.NewTool("akashi_conflicts",
			mcplib.WithDescription(`List detected conflicts between decisions.

WHEN TO USE: When you want to see what contradictions or disagreements
exist in the decision trail. Useful for understanding where agents
disagree and what needs resolution.

Returns conflicts filtered by type, agent, status, severity, or category.
Only open/acknowledged conflicts are shown by default.`),
			mcplib.WithReadOnlyHintAnnotation(true),
			mcplib.WithIdempotentHintAnnotation(true),
			mcplib.WithOpenWorldHintAnnotation(false),
			mcplib.WithString("decision_type",
				mcplib.Description("Filter by decision type"),
			),
			mcplib.WithString("agent_id",
				mcplib.Description("Filter by agent involved in the conflict"),
			),
			mcplib.WithString("status",
				mcplib.Description("Filter by status: open, acknowledged, resolved, wont_fix. Defaults to showing open+acknowledged."),
			),
			mcplib.WithString("severity",
				mcplib.Description("Filter by severity: critical, high, medium, low"),
			),
			mcplib.WithString("category",
				mcplib.Description("Filter by category: factual, assessment, strategic, temporal"),
			),
			mcplib.WithNumber("limit",
				mcplib.Description("Maximum results to return"),
				mcplib.Min(1),
				mcplib.Max(100),
				mcplib.DefaultNumber(10),
			),
			mcplib.WithString("format",
				mcplib.Description(`Response format: "concise" (default) returns compact conflicts. "full" returns complete conflict objects.`),
			),
		),
		s.handleConflicts,
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
		resp.Decisions, err = authz.FilterDecisions(ctx, s.db, claims, resp.Decisions, s.grantCache)
		if err != nil {
			return errorResult(fmt.Sprintf("authorization check failed: %v", err)), nil
		}
		resp.Conflicts, err = authz.FilterConflicts(ctx, s.db, claims, resp.Conflicts, s.grantCache)
		if err != nil {
			return errorResult(fmt.Sprintf("authorization check failed: %v", err)), nil
		}
		resp.HasPrecedent = len(resp.Decisions) > 0
	}

	format := request.GetString("format", "concise")
	if format == "full" {
		resultData, _ := json.MarshalIndent(resp, "", "  ")
		return &mcplib.CallToolResult{
			Content: []mcplib.Content{
				mcplib.TextContent{Type: "text", Text: string(resultData)},
			},
		}, nil
	}

	// Concise format: summary + action_needed + compact representations.
	compactDecs := make([]map[string]any, len(resp.Decisions))
	for i, d := range resp.Decisions {
		compactDecs[i] = compactDecision(d)
	}
	compactConfs := make([]map[string]any, len(resp.Conflicts))
	for i, c := range resp.Conflicts {
		compactConfs[i] = compactConflict(c)
	}

	result := map[string]any{
		"has_precedent":  resp.HasPrecedent,
		"summary":        generateCheckSummary(resp.Decisions, resp.Conflicts),
		"action_needed":  actionNeeded(resp.Conflicts),
		"relevant_count": len(resp.Decisions),
		"decisions":      compactDecs,
		"conflicts":      compactConfs,
	}

	resultData, _ := json.MarshalIndent(result, "", "  ")
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
	actorID := ""
	if claims != nil {
		callerRole = claims.Role
		actorID = claims.AgentID
	}
	autoRegAudit := &storage.MutationAuditEntry{
		OrgID:        orgID,
		ActorAgentID: actorID,
		ActorRole:    string(callerRole),
		Endpoint:     "mcp/akashi_trace",
	}
	if _, err := s.decisionSvc.ResolveOrCreateAgent(ctx, orgID, agentID, callerRole, autoRegAudit); err != nil {
		return errorResult(err.Error()), nil
	}

	var reasoningPtr *string
	if reasoning != "" {
		reasoningPtr = &reasoning
	}

	// Build agent_context with server/client namespace split.
	// "server" contains values the server extracted or verified (MCP session,
	// client info, roots, API key prefix). "client" contains self-reported
	// values from tool parameters (model, task).
	var sessionID *uuid.UUID
	serverCtx := map[string]any{}
	clientCtx := map[string]any{}

	if session := mcpserver.ClientSessionFromContext(ctx); session != nil {
		if sid, parseErr := uuid.Parse(session.SessionID()); parseErr == nil {
			sessionID = &sid
		}
		if clientInfoSession, ok := session.(mcpserver.SessionWithClientInfo); ok {
			info := clientInfoSession.GetClientInfo()
			if info.Name != "" {
				serverCtx["tool"] = info.Name
			}
			if info.Version != "" {
				serverCtx["tool_version"] = info.Version
			}
		}
	}

	// Request MCP roots (cached per session, best-effort).
	if roots := s.requestRoots(ctx); len(roots) > 0 {
		if uris := rootURIs(roots); len(uris) > 0 {
			serverCtx["roots"] = uris
		}
		if project := inferProjectFromRoots(roots); project != "" {
			serverCtx["project"] = project
		}
	}

	// API key prefix for server-verified attribution.
	if claims != nil && claims.APIKeyID != nil {
		key, keyErr := s.db.GetAPIKeyByID(ctx, orgID, *claims.APIKeyID)
		if keyErr == nil && key.Prefix != "" {
			serverCtx["api_key_prefix"] = key.Prefix
		}
	}

	// Self-reported context from tool parameters.
	if m := request.GetString("model", ""); m != "" {
		clientCtx["model"] = m
	}
	if t := request.GetString("task", ""); t != "" {
		clientCtx["task"] = t
	}

	// Operator from JWT claims: use the agent's display name if distinct from agent_id.
	if claims != nil {
		agent, agentErr := s.db.GetAgentByAgentID(ctx, orgID, claims.AgentID)
		if agentErr == nil && agent.Name != "" && agent.Name != agent.AgentID {
			clientCtx["operator"] = agent.Name
		}
	}

	// Assemble namespaced agent_context.
	agentContext := map[string]any{}
	if len(serverCtx) > 0 {
		agentContext["server"] = serverCtx
	}
	if len(clientCtx) > 0 {
		agentContext["client"] = clientCtx
	}

	// Idempotency: if the caller provided an idempotency_key, check/reserve it
	// before executing the trace. Reuses the same storage primitives as HTTP.
	idemKey := request.GetString("idempotency_key", "")
	var idemOwned bool // true when this request owns the in-progress reservation
	if idemKey != "" {
		payloadHash, hashErr := mcpTraceHash(agentID, decisionType, outcome, confidence, reasoning)
		if hashErr != nil {
			return errorResult(fmt.Sprintf("failed to hash trace payload: %v", hashErr)), nil
		}
		lookup, beginErr := s.db.BeginIdempotency(ctx, orgID, agentID, "MCP:akashi_trace", idemKey, payloadHash)
		switch {
		case beginErr == nil && lookup.Completed:
			// Replay the stored response.
			return &mcplib.CallToolResult{
				Content: []mcplib.Content{
					mcplib.TextContent{Type: "text", Text: string(lookup.ResponseData)},
				},
			}, nil
		case beginErr == nil:
			idemOwned = true
		case errors.Is(beginErr, storage.ErrIdempotencyPayloadMismatch):
			return errorResult("idempotency key reused with different payload"), nil
		case errors.Is(beginErr, storage.ErrIdempotencyInProgress):
			return errorResult("request with this idempotency key is already in progress"), nil
		default:
			return errorResult(fmt.Sprintf("idempotency lookup failed: %v", beginErr)), nil
		}
	}

	// Build audit metadata so the trace includes an atomic audit record.
	// This closes issue #63: MCP traces previously had no audit trail.
	callerActorID := agentID
	callerActorRole := "agent"
	if claims != nil {
		callerActorID = claims.AgentID
		callerActorRole = string(claims.Role)
	}
	auditMeta := &ctxutil.AuditMeta{
		RequestID:    uuid.New().String(),
		OrgID:        orgID,
		ActorAgentID: callerActorID,
		ActorRole:    callerActorRole,
		HTTPMethod:   "MCP",
		Endpoint:     "akashi_trace",
	}

	// Extract API key ID from claims for per-key attribution.
	var apiKeyID *uuid.UUID
	if claims != nil {
		apiKeyID = claims.APIKeyID
	}

	result, err := s.decisionSvc.Trace(ctx, orgID, decisions.TraceInput{
		AgentID:      agentID,
		SessionID:    sessionID,
		AgentContext: agentContext,
		APIKeyID:     apiKeyID,
		AuditMeta:    auditMeta,
		Decision: model.TraceDecision{
			DecisionType: decisionType,
			Outcome:      outcome,
			Confidence:   confidence,
			Reasoning:    reasoningPtr,
		},
	})
	if err != nil {
		if idemOwned {
			_ = s.db.ClearInProgressIdempotency(ctx, orgID, agentID, "MCP:akashi_trace", idemKey)
		}
		return errorResult(fmt.Sprintf("failed to record decision: %v", err)), nil
	}

	resultData, _ := json.Marshal(map[string]any{
		"run_id":      result.RunID,
		"decision_id": result.DecisionID,
		"status":      "recorded",
	})

	if idemOwned {
		if compErr := s.db.CompleteIdempotency(ctx, orgID, agentID, "MCP:akashi_trace", idemKey, 200, json.RawMessage(resultData)); compErr != nil {
			s.logger.Error("failed to finalize MCP trace idempotency record — clearing key to unblock retries",
				"error", compErr, "idempotency_key", idemKey, "agent_id", agentID)
			// Clear the stuck key so retries don't get ErrIdempotencyInProgress
			// for the duration of the abandoned TTL (#73).
			clearCtx, clearCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if clearErr := s.db.ClearInProgressIdempotency(clearCtx, orgID, agentID, "MCP:akashi_trace", idemKey); clearErr != nil {
				s.logger.Error("failed to clear stuck MCP idempotency key",
					"error", clearErr, "idempotency_key", idemKey)
			}
			clearCancel()
		}
	}

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

// mcpTraceHash computes a deterministic SHA-256 hash of the trace parameters
// used for idempotency payload comparison.
func mcpTraceHash(agentID, decisionType, outcome string, confidence float32, reasoning string) (string, error) {
	b, err := json.Marshal(map[string]any{
		"agent_id":      agentID,
		"decision_type": decisionType,
		"outcome":       outcome,
		"confidence":    confidence,
		"reasoning":     reasoning,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
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
	if sidStr := request.GetString("session_id", ""); sidStr != "" {
		if sid, parseErr := uuid.Parse(sidStr); parseErr == nil {
			filters.SessionID = &sid
		}
	}
	if tool := request.GetString("tool", ""); tool != "" {
		filters.Tool = &tool
	}
	if m := request.GetString("model", ""); m != "" {
		filters.Model = &m
	}
	if repo := request.GetString("repo", ""); repo != "" {
		filters.Repo = &repo
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
		decs, err = authz.FilterDecisions(ctx, s.db, claims, decs, s.grantCache)
		if err != nil {
			return errorResult(fmt.Sprintf("authorization check failed: %v", err)), nil
		}
		if len(decs) < preFilterCount {
			total = len(decs)
		}
	}

	format := request.GetString("format", "concise")
	var payload any
	if format == "full" {
		payload = map[string]any{"decisions": decs, "total": total}
	} else {
		compact := make([]map[string]any, len(decs))
		for i, d := range decs {
			compact[i] = compactDecision(d)
		}
		payload = map[string]any{"decisions": compact, "total": total}
	}

	resultData, _ := json.MarshalIndent(payload, "", "  ")
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
		results, err = authz.FilterSearchResults(ctx, s.db, claims, results, s.grantCache)
		if err != nil {
			return errorResult(fmt.Sprintf("authorization check failed: %v", err)), nil
		}
	}

	format := request.GetString("format", "concise")
	var payload any
	if format == "full" {
		payload = map[string]any{"results": results, "total": len(results)}
	} else {
		compact := make([]map[string]any, len(results))
		for i, r := range results {
			compact[i] = compactSearchResult(r)
		}
		payload = map[string]any{"results": compact, "total": len(results)}
	}

	resultData, _ := json.MarshalIndent(payload, "", "  ")
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
	if sidStr := request.GetString("session_id", ""); sidStr != "" {
		if sid, parseErr := uuid.Parse(sidStr); parseErr == nil {
			filters.SessionID = &sid
		}
	}
	if tool := request.GetString("tool", ""); tool != "" {
		filters.Tool = &tool
	}
	if m := request.GetString("model", ""); m != "" {
		filters.Model = &m
	}

	decs, _, err := s.decisionSvc.Recent(ctx, orgID, filters, limit, 0)
	if err != nil {
		return errorResult(fmt.Sprintf("query failed: %v", err)), nil
	}

	// Apply access filtering.
	if claims != nil {
		decs, err = authz.FilterDecisions(ctx, s.db, claims, decs, s.grantCache)
		if err != nil {
			return errorResult(fmt.Sprintf("authorization check failed: %v", err)), nil
		}
	}

	format := request.GetString("format", "concise")
	var payload any
	if format == "full" {
		payload = map[string]any{"decisions": decs, "total": len(decs)}
	} else {
		compact := make([]map[string]any, len(decs))
		for i, d := range decs {
			compact[i] = compactDecision(d)
		}
		payload = map[string]any{"decisions": compact, "total": len(decs)}
	}

	resultData, _ := json.MarshalIndent(payload, "", "  ")
	return &mcplib.CallToolResult{
		Content: []mcplib.Content{
			mcplib.TextContent{Type: "text", Text: string(resultData)},
		},
	}, nil
}

func (s *Server) handleConflicts(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	orgID := ctxutil.OrgIDFromContext(ctx)
	claims := ctxutil.ClaimsFromContext(ctx)
	limit := request.GetInt("limit", 10)

	filters := storage.ConflictFilters{}
	if dt := request.GetString("decision_type", ""); dt != "" {
		filters.DecisionType = &dt
	}
	if aid := request.GetString("agent_id", ""); aid != "" {
		filters.AgentID = &aid
	}
	if st := request.GetString("status", ""); st != "" {
		filters.Status = &st
	}
	if sev := request.GetString("severity", ""); sev != "" {
		filters.Severity = &sev
	}
	if cat := request.GetString("category", ""); cat != "" {
		filters.Category = &cat
	}

	conflicts, err := s.db.ListConflicts(ctx, orgID, filters, limit, 0)
	if err != nil {
		return errorResult(fmt.Sprintf("list conflicts failed: %v", err)), nil
	}

	// Apply access filtering.
	if claims != nil {
		conflicts, err = authz.FilterConflicts(ctx, s.db, claims, conflicts, s.grantCache)
		if err != nil {
			return errorResult(fmt.Sprintf("authorization check failed: %v", err)), nil
		}
	}

	// Default to open+acknowledged if no status filter was provided.
	if request.GetString("status", "") == "" {
		var actionable []model.DecisionConflict
		for _, c := range conflicts {
			if c.Status == "open" || c.Status == "acknowledged" {
				actionable = append(actionable, c)
			}
		}
		conflicts = actionable
	}

	if conflicts == nil {
		conflicts = []model.DecisionConflict{}
	}

	format := request.GetString("format", "concise")
	var payload any
	if format == "full" {
		payload = map[string]any{"conflicts": conflicts, "total": len(conflicts)}
	} else {
		compact := make([]map[string]any, len(conflicts))
		for i, c := range conflicts {
			compact[i] = compactConflict(c)
		}
		payload = map[string]any{"conflicts": compact, "total": len(conflicts)}
	}

	resultData, _ := json.MarshalIndent(payload, "", "  ")
	return &mcplib.CallToolResult{
		Content: []mcplib.Content{
			mcplib.TextContent{Type: "text", Text: string(resultData)},
		},
	}, nil
}

func (s *Server) handleStats(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	orgID := ctxutil.OrgIDFromContext(ctx)

	svc := tracehealth.New(s.db)
	metrics, err := svc.Compute(ctx, orgID)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to compute trace health: %v", err)), nil
	}

	agentCount, err := s.db.CountAgents(ctx, orgID)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to count agents: %v", err)), nil
	}

	resultData, _ := json.MarshalIndent(map[string]any{
		"trace_health": metrics,
		"agents":       agentCount,
	}, "", "  ")

	return &mcplib.CallToolResult{
		Content: []mcplib.Content{
			mcplib.TextContent{Type: "text", Text: string(resultData)},
		},
	}, nil
}
