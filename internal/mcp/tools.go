package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/ashita-ai/akashi/internal/authz"
	"github.com/ashita-ai/akashi/internal/ctxutil"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/quality"
	"github.com/ashita-ai/akashi/internal/service/tracehealth"
	"github.com/ashita-ai/akashi/internal/storage"
)

func (s *Server) registerTools() {
	// akashi_check — look up precedents and active conflicts before deciding.
	s.mcpServer.AddTool(
		mcplib.NewTool("akashi_check",
			mcplib.WithDescription(`Check the black box for decision precedents before making a new one.

WHEN TO USE: BEFORE making any decision. This is the most important tool —
it prevents contradictions and lets you build on prior work.

Call this FIRST. Pass a natural language query describing what you're about
to decide, and optionally narrow by decision_type. If the audit trail shows
precedents, factor them into your reasoning. If conflicts exist, resolve them.

WHAT YOU GET BACK:
- has_precedent: whether any relevant prior decisions exist
- decisions: the most relevant prior decisions (up to limit)
- conflicts: any active conflicts in this decision area
- prior_resolutions: resolved conflicts for this decision type that had a
  declared winner. Each entry shows winning_outcome (the approach that
  prevailed), winning_agent, losing_outcome (the approach that was rejected),
  and winning_decision_id. Use winning_decision_id as precedent_ref in
  akashi_trace to build explicitly on the validated approach. This is the
  mechanism that prevents agents from resurrecting losing approaches after
  a conflict has been formally resolved.
- precedent_ref_hint: UUID to copy into akashi_trace's precedent_ref field

decision_type is optional. When omitted the search spans all types —
useful when you're not sure how past decisions were categorized.
prior_resolutions are only returned when decision_type is specified.

EXAMPLE: Before choosing a caching strategy, call akashi_check with
query="caching strategy for session data" to find relevant precedents
regardless of whether they were tagged "architecture" or "trade_off".`),
			mcplib.WithReadOnlyHintAnnotation(true),
			mcplib.WithIdempotentHintAnnotation(true),
			mcplib.WithOpenWorldHintAnnotation(false),
			mcplib.WithString("query",
				mcplib.Description("Natural language description of the decision you're about to make. Drives semantic search. If omitted, returns recent decisions filtered by decision_type."),
			),
			mcplib.WithString("decision_type",
				mcplib.Description("Optional: narrow results to a specific category (e.g. architecture, security, trade_off). Case-insensitive. Omit to search across all types."),
			),
			mcplib.WithString("agent_id",
				mcplib.Description("Optional: only check decisions from a specific agent"),
			),
			mcplib.WithString("project",
				mcplib.Description("Optional: filter by project name (e.g. \"akashi\", \"my-langchain-app\"). Auto-detected from the working directory when omitted. Pass \"*\" to disable filtering and see decisions across all projects."),
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

TWO REQUIRED FIELDS — everything else is optional:
- decision_type: A short category (see enum for standard types)
- outcome: What you decided, stated as a fact ("chose gpt-4o for summarization")

OPTIONAL FIELDS (each improves completeness score and future usefulness):
- confidence: How certain you are (0.0-1.0). Use this calibration guide:
    0.3-0.4 = educated guess, limited information, could easily be wrong
    0.5-0.6 = reasonable choice but real uncertainty remains
    0.7      = solid decision with good supporting evidence
    0.8      = strong conviction, have considered alternatives carefully
    0.9+     = near-certain, would be surprised if this is wrong
  Most decisions should land between 0.4 and 0.8. If you find yourself
  always above 0.8, you are probably not being honest about uncertainty.
- reasoning: Your chain of thought. Why this choice over alternatives?
  More detail = higher completeness score. Aim for >100 characters.
- alternatives: JSON array of options you considered and rejected.
  Format: [{"label":"option description","rejection_reason":"why not chosen"}]
  Include 2-3 alternatives with substantive rejection reasons (>20 chars each).
  This is the single biggest driver of completeness after reasoning.
- evidence: JSON array of supporting facts.
  Format: [{"source_type":"tool_output","content":"test suite passed with 0 failures"},
           {"source_type":"document","content":"ADR-007 requires event sourcing","source_uri":"adrs/007.md"}]
  source_type values: document, api_response, agent_output, user_input, search_result,
                      tool_output, memory, database_query
  Include at least 2 pieces of evidence to maximize completeness.
- project: The project or app this belongs to (e.g. "akashi", "my-langchain-app").
  Enables project-scoped queries. Auto-detected from working directory if omitted.
- precedent_ref: Copy the value of precedent_ref_hint from akashi_check's response.
  Wires the attribution graph so the audit trail shows how decisions evolved.

EXAMPLE: After choosing a caching strategy, record decision_type="architecture",
outcome="chose Redis with 5min TTL for session cache", confidence=0.7,
reasoning="Redis handles our expected QPS, TTL prevents stale reads. Memcached lacks native clustering in our stack. In-memory cache won't share across instances.",
project="my-service",
precedent_ref="<paste precedent_ref_hint from akashi_check here, if applicable>",
alternatives='[{"label":"in-memory cache","rejection_reason":"not shared across instances, would require sticky sessions"},{"label":"Memcached","rejection_reason":"no native clustering in our stack, adds operational overhead"}]',
evidence='[{"source_type":"tool_output","content":"load test showed 8k req/s with Redis, 2k with DB"},{"source_type":"document","content":"ADR-003 mandates shared-nothing architecture"}]'

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
				mcplib.Description("Category of decision. Common types: architecture, security, code_review, investigation, planning, assessment, trade_off, feature_scope, deployment, error_handling, model_selection, data_source. Any string is accepted. Stored lowercase."),
				mcplib.Required(),
			),
			mcplib.WithString("outcome",
				mcplib.Description("What you decided, stated as a fact. Be specific: 'chose Redis with 5min TTL' not 'picked a cache'"),
				mcplib.Required(),
			),
			mcplib.WithNumber("confidence",
				mcplib.Description("How certain you are (0.0-1.0). Most decisions should be 0.4-0.8. See calibration guide above. Defaults to 0.5 if omitted."),
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
			mcplib.WithString("project",
				mcplib.Description(`The project, application, or service this decision belongs to (e.g. "akashi", "my-langchain-app", "customer-support-bot"). Include this so the decision appears in project-scoped queries and can be filtered by other agents working on the same project.`),
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

	// akashi_query — structured or semantic query over the decision audit trail.
	s.mcpServer.AddTool(
		mcplib.NewTool("akashi_query",
			mcplib.WithDescription(`Query the decision audit trail with structured filters or free-text search.

WHEN TO USE: When you need to explore or filter past decisions —
either by exact criteria (agent, type, confidence) or by natural language.

TWO MODES:
- With "query": semantic/text search — finds decisions by meaning, not exact match.
  Use this when you don't know exact field values: "caching decisions", "rate limit choices".
- Without "query": structured filter — exact match on the fields you provide.
  Use this when you know exactly what you want: all architecture decisions by agent-7
  with confidence >= 0.8.

Results always sorted by recency. Use limit + offset for pagination.

EXAMPLES:
- Semantic: query="how did we handle rate limiting?"
- Structured: decision_type="architecture", confidence_min=0.8, agent_id="planner"
- Recent activity: no filters, limit=20 (returns newest decisions)`),
			mcplib.WithReadOnlyHintAnnotation(true),
			mcplib.WithIdempotentHintAnnotation(true),
			mcplib.WithOpenWorldHintAnnotation(false),
			mcplib.WithString("query",
				mcplib.Description("Natural language search query. When provided, performs semantic/text search and ignores structured filters except confidence_min and project. When omitted, uses structured filter mode."),
			),
			mcplib.WithString("decision_type",
				mcplib.Description("Filter by decision type (any string, e.g. architecture, security, code_review). Case-insensitive. Ignored when query is provided."),
			),
			mcplib.WithString("agent_id",
				mcplib.Description("Filter by agent ID — whose decisions to look at. Ignored when query is provided."),
			),
			mcplib.WithString("outcome",
				mcplib.Description("Filter by exact outcome text. Ignored when query is provided."),
			),
			mcplib.WithNumber("confidence_min",
				mcplib.Description("Minimum confidence threshold (0.0-1.0). Use 0.7+ for reliable decisions. Applied in both modes."),
				mcplib.Min(0),
				mcplib.Max(1),
			),
			mcplib.WithString("session_id",
				mcplib.Description("Filter by session UUID. Ignored when query is provided."),
			),
			mcplib.WithString("tool",
				mcplib.Description("Filter by tool name (e.g. 'claude-code', 'cursor'). Ignored when query is provided."),
			),
			mcplib.WithString("model",
				mcplib.Description("Filter by model name (e.g. 'claude-opus-4-6'). Ignored when query is provided."),
			),
			mcplib.WithString("project",
				mcplib.Description("Filter by project name (e.g. \"akashi\", \"my-langchain-app\"). Auto-detected from the working directory when omitted. Pass \"*\" to query across all projects. Applied in both modes."),
			),
			mcplib.WithNumber("limit",
				mcplib.Description("Maximum results to return"),
				mcplib.Min(1),
				mcplib.Max(100),
				mcplib.DefaultNumber(10),
			),
			mcplib.WithNumber("offset",
				mcplib.Description("Number of results to skip for pagination. Only applies in structured filter mode."),
				mcplib.Min(0),
				mcplib.DefaultNumber(0),
			),
			mcplib.WithString("format",
				mcplib.Description(`Response format: "concise" (default) returns compact decisions. "full" returns complete decision objects.`),
			),
		),
		s.handleQuery,
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

Use the decision_id from the original akashi_trace response, or from
the id field of a decision returned by akashi_check. You can only assess decisions within
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

// resolveProjectFilter returns the project filter to apply to a read operation.
//
// Priority:
//  1. Explicit "project" param — always wins.
//  2. project == "*" — opt-out wildcard, disables filtering (returns nil).
//  3. No explicit project — auto-detect from MCP roots (git remote > directory name).
//  4. Roots unavailable or detection fails — no filter (nil).
//
// This makes queries naturally project-scoped without requiring agents to know
// the project name. Agents can pass project="*" for intentional cross-project queries.
func (s *Server) resolveProjectFilter(ctx context.Context, request mcplib.CallToolRequest) *string {
	explicit := request.GetString("project", "")
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

	// decision_type is optional — normalize if provided.
	decisionType := strings.ToLower(strings.TrimSpace(request.GetString("decision_type", "")))
	query := request.GetString("query", "")
	agentID := request.GetString("agent_id", "")
	limit := request.GetInt("limit", 5)

	checkInput := decisions.CheckInput{
		DecisionType: decisionType,
		Query:        query,
		AgentID:      agentID,
		Limit:        limit,
	}
	if project := s.resolveProjectFilter(ctx, request); project != nil {
		checkInput.Project = *project
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
		if assessments, aErr := s.db.GetAssessmentSummaryBatch(ctx, orgID, ids); aErr == nil {
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
	compactResolutions := make([]map[string]any, len(resp.PriorResolutions))
	for i, r := range resp.PriorResolutions {
		compactResolutions[i] = compactResolution(r)
	}

	summary := generateCheckSummary(resp.Decisions, resp.Conflicts)
	if len(resp.PriorResolutions) > 0 {
		summary += fmt.Sprintf(" %d prior conflict(s) for this decision type were formally resolved; winning approach(es) listed in prior_resolutions.", len(resp.PriorResolutions))
	}

	result := map[string]any{
		"has_precedent":     resp.HasPrecedent,
		"summary":           summary,
		"action_needed":     actionNeeded(resp.Conflicts),
		"relevant_count":    len(resp.Decisions),
		"decisions":         compactDecs,
		"conflicts":         compactConfs,
		"prior_resolutions": compactResolutions,
	}

	// precedent_ref_hint: the UUID of the best candidate for precedent_ref in the
	// subsequent akashi_trace call. Emitted as a bare UUID so agents can copy it
	// directly without parsing. Only shown when decisions are returned and the caller
	// has write access. We pick the least-cited decision to spread attribution.
	if len(resp.Decisions) > 0 && claims != nil && model.RoleAtLeast(claims.Role, model.RoleAgent) {
		for _, d := range resp.Decisions {
			if d.PrecedentCitationCount < 5 {
				result["precedent_ref_hint"] = d.ID.String()
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
	// Normalize decision_type to lowercase for consistent storage and retrieval.
	decisionType := strings.ToLower(strings.TrimSpace(request.GetString("decision_type", "")))
	outcome := request.GetString("outcome", "")
	confidence := float32(request.GetFloat("confidence", 0.5))
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
	if r := request.GetString("project", ""); r != "" {
		clientCtx["project"] = r
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

	// Compute completeness score and missing-field hints for agent feedback.
	completenessScore := quality.Score(model.TraceDecision{
		DecisionType: decisionType,
		Outcome:      outcome,
		Confidence:   confidence,
		Reasoning:    reasoningPtr,
		Alternatives: alternatives,
		Evidence:     evidence,
	})
	missing := computeMissingFields(decisionType, outcome, confidence, reasoningPtr, alternatives, evidence)

	responseMap := map[string]any{
		"run_id":             result.RunID,
		"decision_id":        result.DecisionID,
		"status":             "recorded",
		"completeness_score": fmt.Sprintf("%.0f%%", completenessScore*100),
	}
	if len(missing) > 0 {
		responseMap["completeness_tips"] = missing
	}

	resultData, _ := json.Marshal(responseMap)

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

	return &mcplib.CallToolResult{
		Content: []mcplib.Content{
			mcplib.TextContent{Type: "text", Text: string(resultData)},
		},
	}, nil
}

// computeMissingFields returns actionable tips for improving trace completeness.
// Each tip tells the agent exactly what to add next time. Tips are ordered by
// completeness score impact (highest first).
func computeMissingFields(decisionType, outcome string, confidence float32, reasoning *string, alternatives []model.TraceAlternative, evidence []model.TraceEvidence) []string {
	var tips []string

	// Reasoning: biggest single factor (up to 0.25).
	if reasoning == nil || len(strings.TrimSpace(*reasoning)) <= 100 {
		if reasoning == nil || len(strings.TrimSpace(*reasoning)) <= 20 {
			tips = append(tips, "Add reasoning (>100 chars) explaining why you chose this over alternatives (+25%)")
		} else {
			tips = append(tips, "Expand reasoning to >100 chars for full credit (+5-15%)")
		}
	}

	// Alternatives with substantive rejection reasons (up to 0.20).
	substantive := 0
	for _, alt := range alternatives {
		if !alt.Selected && alt.RejectionReason != nil && len(strings.TrimSpace(*alt.RejectionReason)) > 20 {
			substantive++
		}
	}
	if substantive < 3 {
		tips = append(tips, fmt.Sprintf("Add %d more rejected alternatives with rejection_reason >20 chars (+%d%%)", 3-substantive, (3-substantive)*5))
	}

	// Evidence (up to 0.15).
	if len(evidence) < 2 {
		if len(evidence) == 0 {
			tips = append(tips, "Add 2+ evidence items (source_type + content) to support your decision (+15%)")
		} else {
			tips = append(tips, "Add 1 more evidence item for full credit (+5%)")
		}
	}

	// Confidence calibration nudge.
	if confidence >= 0.95 || confidence <= 0.05 {
		tips = append(tips, "Confidence is at an extreme — values between 0.4 and 0.8 are more informative")
	}

	// Standard decision type (0.10).
	if !quality.StandardDecisionTypes[decisionType] {
		tips = append(tips, "Use a standard decision_type (architecture, security, trade_off, etc.) for +10%")
	}

	// Substantive outcome (0.05).
	if len(strings.TrimSpace(outcome)) <= 20 {
		tips = append(tips, "Make outcome more specific (>20 chars) for +5%")
	}

	return tips
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

	query := request.GetString("query", "")
	limit := request.GetInt("limit", 10)
	format := request.GetString("format", "concise")

	// Build shared filters (applied to both modes; some are ignored in semantic mode).
	filters := model.QueryFilters{}
	if confMin := float32(request.GetFloat("confidence_min", 0)); confMin > 0 {
		filters.ConfidenceMin = &confMin
	}
	filters.Project = s.resolveProjectFilter(ctx, request)

	if query != "" {
		// Semantic/text search path. Structured filters other than confidence_min
		// and project are intentionally ignored — the query drives discovery.
		results, err := s.decisionSvc.Search(ctx, orgID, query, true, filters, limit)
		if err != nil {
			return errorResult(fmt.Sprintf("search failed: %v", err)), nil
		}
		if claims != nil {
			results, err = authz.FilterSearchResults(ctx, s.db, claims, results, s.grantCache)
			if err != nil {
				return errorResult(fmt.Sprintf("authorization check failed: %v", err)), nil
			}
		}
		var payload any
		if format == "full" {
			payload = map[string]any{"decisions": results, "total": len(results)}
		} else {
			compact := make([]map[string]any, len(results))
			for i, r := range results {
				compact[i] = compactSearchResult(r)
			}
			payload = map[string]any{"decisions": compact, "total": len(results)}
		}
		resultData, _ := json.MarshalIndent(payload, "", "  ")
		return &mcplib.CallToolResult{
			Content: []mcplib.Content{
				mcplib.TextContent{Type: "text", Text: string(resultData)},
			},
		}, nil
	}

	// Structured filter path.
	if agentID := request.GetString("agent_id", ""); agentID != "" {
		filters.AgentIDs = []string{agentID}
	}
	if dt := strings.ToLower(strings.TrimSpace(request.GetString("decision_type", ""))); dt != "" {
		filters.DecisionType = &dt
	}
	if outcome := request.GetString("outcome", ""); outcome != "" {
		filters.Outcome = &outcome
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

	offset := request.GetInt("offset", 0)

	decs, total, err := s.decisionSvc.Query(ctx, orgID, model.QueryRequest{
		Filters:  filters,
		Include:  []string{"alternatives"},
		OrderBy:  "valid_from",
		OrderDir: "desc",
		Limit:    limit,
		Offset:   offset,
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

func (s *Server) handleConflicts(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	orgID := ctxutil.OrgIDFromContext(ctx)
	claims := ctxutil.ClaimsFromContext(ctx)

	if claims == nil {
		return errorResult("authentication required"), nil
	}

	limit := request.GetInt("limit", 10)
	format := request.GetString("format", "concise")

	// Build group filters. The MCP tool defaults to open+acknowledged groups so
	// agents see actionable disagreements, not resolved history.
	statusFilter := request.GetString("status", "")
	groupFilters := storage.ConflictGroupFilters{
		OpenOnly: statusFilter == "" || statusFilter == "open" || statusFilter == "acknowledged",
	}
	if dt := request.GetString("decision_type", ""); dt != "" {
		groupFilters.DecisionType = &dt
	}
	if aid := request.GetString("agent_id", ""); aid != "" {
		groupFilters.AgentID = &aid
	}

	// Use the category/severity filters from the request to post-filter on the
	// representative conflict — they are not group-level columns.
	severityFilter := request.GetString("severity", "")
	categoryFilter := request.GetString("category", "")

	groups, err := s.db.ListConflictGroups(ctx, orgID, groupFilters, limit, 0)
	if err != nil {
		return errorResult(fmt.Sprintf("list conflict groups failed: %v", err)), nil
	}

	// Post-filter by severity/category on the representative conflict.
	if severityFilter != "" || categoryFilter != "" {
		var filtered []model.ConflictGroup
		for _, g := range groups {
			if g.Representative == nil {
				continue
			}
			if severityFilter != "" && (g.Representative.Severity == nil || *g.Representative.Severity != severityFilter) {
				continue
			}
			if categoryFilter != "" && (g.Representative.Category == nil || *g.Representative.Category != categoryFilter) {
				continue
			}
			filtered = append(filtered, g)
		}
		groups = filtered
	}

	// Access filtering: keep groups whose representative conflict passes the authz check.
	// This mirrors the pattern in handleConflicts for individual pairs.
	if claims != nil && len(groups) > 0 {
		// Extract representative conflicts for the authz filter.
		reps := make([]model.DecisionConflict, 0, len(groups))
		for _, g := range groups {
			if g.Representative != nil {
				reps = append(reps, *g.Representative)
			}
		}
		allowed, err := authz.FilterConflicts(ctx, s.db, claims, reps, s.grantCache)
		if err != nil {
			return errorResult(fmt.Sprintf("authorization check failed: %v", err)), nil
		}
		allowedIDs := make(map[string]bool, len(allowed))
		for _, c := range allowed {
			allowedIDs[c.ID.String()] = true
		}
		var accessible []model.ConflictGroup
		for _, g := range groups {
			if g.Representative == nil || allowedIDs[g.Representative.ID.String()] {
				accessible = append(accessible, g)
			}
		}
		groups = accessible
	}

	if groups == nil {
		groups = []model.ConflictGroup{}
	}

	var payload any
	if format == "full" {
		payload = map[string]any{"conflicts": groups, "total": len(groups)}
	} else {
		compact := make([]map[string]any, len(groups))
		for i, g := range groups {
			compact[i] = compactConflictGroup(g)
		}
		payload = map[string]any{"conflicts": compact, "total": len(groups)}
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
