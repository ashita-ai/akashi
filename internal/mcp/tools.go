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
			mcplib.WithString("repo",
				mcplib.Description("Optional: filter by repository or project name (e.g. \"akashi\", \"my-service\"). Auto-detected from the working directory when omitted. Pass \"*\" to disable filtering and see decisions across all projects."),
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
- precedent_ref: UUID of the prior decision this one builds on. Copy directly from
  akashi_check's precedent_ref_hint field. Wires the attribution graph so the audit
  trail shows how decisions evolved. Omit if there is no clear antecedent.
- alternatives: JSON array of options you considered and rejected (optional but improves quality score).
  Format: [{"label":"option description","rejection_reason":"why not chosen"}]
- evidence: JSON array of supporting facts (optional but improves quality score).
  Format: [{"source_type":"tool_output","content":"test suite passed with 0 failures"},
           {"source_type":"document","content":"ADR-007 requires event sourcing","source_uri":"adrs/007.md"}]
  source_type values: document, api_response, agent_output, user_input, search_result,
                      tool_output, memory, database_query

EXAMPLE: After choosing a caching strategy, record decision_type="architecture",
outcome="chose Redis with 5min TTL for session cache", confidence=0.85,
reasoning="Redis handles our expected QPS, TTL prevents stale reads",
precedent_ref="<uuid from akashi_check's precedent_ref_hint if applicable>",
alternatives='[{"label":"in-memory cache","rejection_reason":"not shared across instances"},{"label":"Memcached","rejection_reason":"no native clustering in our stack"}]',
evidence='[{"source_type":"tool_output","content":"load test showed 8k req/s with Redis, 2k with DB"}]'

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
				mcplib.Description("How certain you are about this decision (0.0 = guessing, 1.0 = certain). Defaults to 0.7 if omitted."),
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
			mcplib.WithString("evidence",
				mcplib.Description(`JSON array of supporting facts. Each item: {"source_type":"<type>","content":"<text>","source_uri":"<optional>","relevance_score":<0-1 optional>}. source_type values: document, api_response, agent_output, user_input, search_result, tool_output, memory, database_query.`),
			),
			mcplib.WithString("alternatives",
				mcplib.Description(`JSON array of options you considered and rejected. Each item: {"label":"<description of option>","rejection_reason":"<why you didn't choose it>"}. Providing alternatives improves completeness scoring and helps future agents understand your reasoning. Example: [{"label":"Use Redis for caching","rejection_reason":"adds operational overhead for our traffic levels"},{"label":"In-memory cache","rejection_reason":"not shared across instances"}]`),
			),
			mcplib.WithString("precedent_ref",
				mcplib.Description("UUID of the prior decision this one directly builds on. Copy the value from akashi_check's precedent_ref_hint field. Wires the attribution graph so the audit trail shows how decisions evolved over time. Omit if there is no clear antecedent."),
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
				mcplib.Description("Filter by repository or project name. Auto-detected from the working directory when omitted. Pass \"*\" to query across all projects."),
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
			mcplib.WithString("repo",
				mcplib.Description("Optional: filter by repository or project name. Auto-detected from the working directory when omitted. Pass \"*\" to search across all projects."),
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
			mcplib.WithString("repo",
				mcplib.Description("Optional: filter by repository or project name. Auto-detected from the working directory when omitted. Pass \"*\" to see recent decisions across all projects."),
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

	// akashi_assess — record explicit outcome feedback for a prior decision.
	s.mcpServer.AddTool(
		mcplib.NewTool("akashi_assess",
			mcplib.WithDescription(`Record explicit outcome feedback for a prior decision.

WHEN TO USE: After you observe whether a prior decision turned out to be
correct — e.g., the build passed, the approach worked, the prediction was
right. Call this to close the learning loop.

Use the decision_id from the original akashi_trace response or from
akashi_check's precedent_ref_hint. You can only assess decisions within
your org. Each call appends a new row — re-assessing creates a revision
record rather than overwriting, preserving the full assessment history.

EXAMPLE: A coder agent implemented a planner's architecture decision.
After testing, the coder calls akashi_assess to mark it correct:
  decision_id="<uuid>", outcome="correct", notes="All tests pass, no regressions"`),
			mcplib.WithDestructiveHintAnnotation(false),
			mcplib.WithIdempotentHintAnnotation(false),
			mcplib.WithOpenWorldHintAnnotation(true),
			mcplib.WithString("decision_id",
				mcplib.Description("UUID of the decision being assessed"),
				mcplib.Required(),
			),
			mcplib.WithString("outcome",
				mcplib.Description(`Assessment verdict: "correct", "incorrect", or "partially_correct"`),
				mcplib.Required(),
			),
			mcplib.WithString("notes",
				mcplib.Description("Optional free-text explanation of the assessment outcome"),
			),
		),
		s.handleAssess,
	)
}

// resolveRepoFilter returns the repo filter to apply to a read operation.
//
// Priority:
//  1. Explicit "repo" param — always wins.
//  2. repo == "*" — opt-out wildcard, disables filtering (returns nil).
//  3. No explicit repo — auto-detect from MCP roots (git remote > directory name).
//  4. Roots unavailable or detection fails — no filter (nil).
//
// This makes queries naturally project-scoped without requiring agents to know
// the project name. Agents can pass repo="*" for intentional cross-project queries.
func (s *Server) resolveRepoFilter(ctx context.Context, request mcplib.CallToolRequest) *string {
	explicit := request.GetString("repo", "")
	if explicit == "*" {
		return nil // cross-project opt-out
	}
	if explicit != "" {
		return &explicit
	}
	// Auto-detect from MCP roots.
	if roots := s.requestRoots(ctx); len(roots) > 0 {
		if project := inferProjectFromRootsWithGit(roots); project != "" {
			return &project
		}
	}
	return nil
}

func (s *Server) handleCheck(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	orgID := ctxutil.OrgIDFromContext(ctx)
	claims := ctxutil.ClaimsFromContext(ctx)

	if claims == nil {
		return errorResult("authentication required"), nil
	}

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

	checkInput := decisions.CheckInput{
		DecisionType: decisionType,
		Query:        query,
		AgentID:      agentID,
		Limit:        limit,
	}
	if repo := s.resolveRepoFilter(ctx, request); repo != nil {
		checkInput.Repo = *repo
	}

	resp, err := s.decisionSvc.Check(ctx, orgID, checkInput)
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

	// Populate consensus scores, outcome signals, and assessment summaries for decisions.
	if len(resp.Decisions) > 0 {
		ids := make([]uuid.UUID, len(resp.Decisions))
		for i := range resp.Decisions {
			ids[i] = resp.Decisions[i].ID
		}
		if consensusMap, cErr := s.decisionSvc.ConsensusScoresBatch(ctx, ids, orgID); cErr == nil {
			for i := range resp.Decisions {
				if scores, ok := consensusMap[resp.Decisions[i].ID]; ok {
					resp.Decisions[i].AgreementCount = scores[0]
					resp.Decisions[i].ConflictCount = scores[1]
				}
			}
		}
		if signalsMap, sErr := s.db.GetDecisionOutcomeSignalsBatch(ctx, ids, orgID); sErr == nil {
			for i := range resp.Decisions {
				if sig, ok := signalsMap[resp.Decisions[i].ID]; ok {
					resp.Decisions[i].SupersessionVelocityHours = sig.SupersessionVelocityHours
					resp.Decisions[i].PrecedentCitationCount = sig.PrecedentCitationCount
					resp.Decisions[i].ConflictFate = sig.ConflictFate
				}
			}
		}
		// Assessment summaries: explicit correctness feedback. Non-fatal on error.
		if assessments, aErr := s.db.GetAssessmentSummaryBatch(ctx, ids); aErr == nil {
			for i := range resp.Decisions {
				if sum, ok := assessments[resp.Decisions[i].ID]; ok {
					cp := sum
					resp.Decisions[i].AssessmentSummary = &cp
				}
			}
		}
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
	// Build agreement count lookup for consensus note generation.
	agreementCounts := make(map[[16]byte]int, len(resp.Decisions))
	for _, d := range resp.Decisions {
		agreementCounts[[16]byte(d.ID)] = d.AgreementCount
	}

	compactDecs := make([]map[string]any, len(resp.Decisions))
	for i, d := range resp.Decisions {
		compactDecs[i] = compactDecision(d)
	}
	compactConfs := make([]map[string]any, len(resp.Conflicts))
	for i, c := range resp.Conflicts {
		note := buildConsensusNote(c, agreementCounts)
		compactConfs[i] = compactConflict(c, note)
	}

	result := map[string]any{
		"has_precedent":  resp.HasPrecedent,
		"summary":        generateCheckSummary(resp.Decisions, resp.Conflicts),
		"action_needed":  actionNeeded(resp.Conflicts),
		"relevant_count": len(resp.Decisions),
		"decisions":      compactDecs,
		"conflicts":      compactConfs,
	}

	// precedent_ref_hint: shown to write-role callers when matching decisions are returned,
	// nudging them to set precedent_ref when tracing to build the attribution graph.
	if len(resp.Decisions) > 0 && claims != nil && model.RoleAtLeast(claims.Role, model.RoleAgent) {
		// Find a decision with low citation count to use as the example.
		for _, d := range resp.Decisions {
			if d.PrecedentCitationCount < 5 {
				result["precedent_ref_hint"] = fmt.Sprintf(
					"If your current decision builds on any of the above, set precedent_ref: %s in akashi_trace. This builds the attribution graph used by outcome signals.",
					d.ID)
				break
			}
		}
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

	if claims == nil {
		return errorResult("authentication required"), nil
	}

	agentID := request.GetString("agent_id", "")
	decisionType := request.GetString("decision_type", "")
	outcome := request.GetString("outcome", "")
	confidence := float32(request.GetFloat("confidence", 0.7))
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

	// Per-field length limits and source_uri scheme validation.
	// Validated here (before evidence/alternatives parsing) so that the raw
	// string values can be checked before JSON unmarshalling allocates slices.
	if len(decisionType) > model.MaxDecisionTypeLen {
		return errorResult(fmt.Sprintf("decision_type exceeds maximum length of %d characters", model.MaxDecisionTypeLen)), nil
	}
	if len(outcome) > model.MaxOutcomeLen {
		return errorResult(fmt.Sprintf("outcome exceeds maximum length of %d bytes", model.MaxOutcomeLen)), nil
	}
	if len(reasoning) > model.MaxReasoningLen {
		return errorResult(fmt.Sprintf("reasoning exceeds maximum length of %d bytes", model.MaxReasoningLen)), nil
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

	// Parse evidence JSON if provided. Invalid JSON is logged and ignored rather
	// than failing the trace — a trace without evidence is better than no trace.
	var evidence []model.TraceEvidence
	if ev := request.GetString("evidence", ""); ev != "" {
		if parseErr := json.Unmarshal([]byte(ev), &evidence); parseErr != nil {
			s.logger.Warn("akashi_trace: ignoring unparseable evidence JSON",
				"error", parseErr, "agent_id", agentID)
			evidence = nil
		}
	}

	// Validate source_uri on each evidence item. Invalid URIs are rejected
	// rather than silently dropped — callers should know their URIs are unsafe.
	for i, ev := range evidence {
		if ev.SourceURI != nil {
			if err := model.ValidateSourceURI(*ev.SourceURI); err != nil {
				return errorResult(fmt.Sprintf("evidence[%d].source_uri: %v", i, err)), nil
			}
		}
	}

	// Parse alternatives JSON if provided. Same lenient approach as evidence:
	// log and continue rather than rejecting the whole trace.
	var alternatives []model.TraceAlternative
	if alt := request.GetString("alternatives", ""); alt != "" {
		if parseErr := json.Unmarshal([]byte(alt), &alternatives); parseErr != nil {
			s.logger.Warn("akashi_trace: ignoring unparseable alternatives JSON",
				"error", parseErr, "agent_id", agentID)
			alternatives = nil
		}
	}

	// Parse precedent_ref UUID if provided. Invalid format is logged and ignored —
	// a trace without a precedent link is better than a failed trace.
	var precedentRef *uuid.UUID
	if pr := request.GetString("precedent_ref", ""); pr != "" {
		if id, parseErr := uuid.Parse(pr); parseErr == nil {
			precedentRef = &id
		} else {
			s.logger.Warn("akashi_trace: ignoring invalid precedent_ref UUID",
				"value", pr, "error", parseErr, "agent_id", agentID)
		}
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
			serverCtx["repo"] = project
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
		payloadHash, hashErr := mcpTraceHash(agentID, decisionType, outcome, confidence, reasoning, evidence, alternatives, precedentRef)
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
		PrecedentRef: precedentRef,
		Decision: model.TraceDecision{
			DecisionType: decisionType,
			Outcome:      outcome,
			Confidence:   confidence,
			Reasoning:    reasoningPtr,
			Alternatives: alternatives,
			Evidence:     evidence,
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
// used for idempotency payload comparison. precedentRef is included so that
// the same outcome recorded with a different attribution link is treated as
// a distinct payload (rather than a replay of the original).
func mcpTraceHash(agentID, decisionType, outcome string, confidence float32, reasoning string, evidence []model.TraceEvidence, alternatives []model.TraceAlternative, precedentRef *uuid.UUID) (string, error) {
	var prStr *string
	if precedentRef != nil {
		s := precedentRef.String()
		prStr = &s
	}
	b, err := json.Marshal(map[string]any{
		"agent_id":      agentID,
		"decision_type": decisionType,
		"outcome":       outcome,
		"confidence":    confidence,
		"reasoning":     reasoning,
		"evidence":      evidence,
		"alternatives":  alternatives,
		"precedent_ref": prStr,
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

	if claims == nil {
		return errorResult("authentication required"), nil
	}

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
	filters.Repo = s.resolveRepoFilter(ctx, request)

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

	if claims == nil {
		return errorResult("authentication required"), nil
	}

	query := request.GetString("query", "")
	if query == "" {
		return errorResult("query is required"), nil
	}

	limit := request.GetInt("limit", 5)
	filters := model.QueryFilters{}
	if confMin := float32(request.GetFloat("confidence_min", 0)); confMin > 0 {
		filters.ConfidenceMin = &confMin
	}
	filters.Repo = s.resolveRepoFilter(ctx, request)

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

	if claims == nil {
		return errorResult("authentication required"), nil
	}

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
	filters.Repo = s.resolveRepoFilter(ctx, request)

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

	if claims == nil {
		return errorResult("authentication required"), nil
	}

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
			compact[i] = compactConflict(c, "")
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

func (s *Server) handleAssess(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	orgID := ctxutil.OrgIDFromContext(ctx)
	claims := ctxutil.ClaimsFromContext(ctx)

	if claims == nil {
		return errorResult("authentication required"), nil
	}

	decisionIDStr := request.GetString("decision_id", "")
	if decisionIDStr == "" {
		return errorResult("decision_id is required"), nil
	}
	decisionID, err := uuid.Parse(decisionIDStr)
	if err != nil {
		return errorResult("decision_id must be a valid UUID"), nil
	}

	outcomeStr := request.GetString("outcome", "")
	outcome := model.AssessmentOutcome(outcomeStr)
	switch outcome {
	case model.AssessmentCorrect, model.AssessmentIncorrect, model.AssessmentPartiallyCorrect:
		// valid
	default:
		return errorResult(`outcome must be one of: "correct", "incorrect", "partially_correct"`), nil
	}

	var notes *string
	if n := request.GetString("notes", ""); n != "" {
		notes = &n
	}

	a := model.DecisionAssessment{
		DecisionID:      decisionID,
		OrgID:           orgID,
		AssessorAgentID: claims.AgentID,
		Outcome:         outcome,
		Notes:           notes,
	}

	result, err := s.db.CreateAssessment(ctx, orgID, a)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return errorResult("decision not found"), nil
		}
		return errorResult(fmt.Sprintf("failed to save assessment: %v", err)), nil
	}

	// Return compact confirmation.
	resultData, _ := json.MarshalIndent(map[string]any{
		"assessment_id": result.ID,
		"decision_id":   result.DecisionID,
		"outcome":       result.Outcome,
		"assessor":      result.AssessorAgentID,
		"recorded_at":   result.CreatedAt,
	}, "", "  ")

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
