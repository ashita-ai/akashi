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

WHEN TO USE: Before architecture, design, trade-off, or security decisions —
choices where contradicting a prior decision would cause real damage.

DO NOT CALL for mechanical changes (formatting, typo fixes, renaming,
test updates), pure implementation of an already-decided approach, or
reading/exploring code. The IDE hook gate handles edit permissions
automatically — you do not need to call this tool just to unlock edits.

Pass a natural language query describing what you're about to decide,
and optionally narrow by decision_type. If the audit trail shows
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
           {"source_type":"document","content":"ADR-007 requires event sourcing","source_uri":"adrs/007.md"},
           {"source_type":"metrics","content":"NER benchmark","metrics":{"accuracy":0.93,"f1":0.87}}]
  source_type values: document, api_response, agent_output, user_input, search_result,
                      tool_output, memory, database_query, metrics
  For source_type "metrics", include a "metrics" object with numeric key-value pairs.
  The "content" field is optional for metrics (used as a human-readable summary).
  Include at least 2 pieces of evidence to maximize completeness.
- project: The project or app this belongs to (e.g. "akashi", "my-langchain-app").
  Enables project-scoped queries. Auto-detected from working directory if omitted.
- precedent_ref: Copy the value of precedent_ref_hint from akashi_check's response.
  Wires the attribution graph so the audit trail shows how decisions evolved.
- precedent_reason: When setting precedent_ref, briefly explain WHY the prior decision
  applies. This makes the attribution chain self-documenting for future agents.

EXAMPLE: After choosing a caching strategy, record decision_type="architecture",
outcome="chose Redis with 5min TTL for session cache", confidence=0.7,
reasoning="Redis handles our expected QPS, TTL prevents stale reads. Memcached lacks native clustering in our stack. In-memory cache won't share across instances.",
task="session infrastructure redesign",
project="my-service",
precedent_ref="<paste precedent_ref_hint from akashi_check here, if applicable>",
precedent_reason="extends ADR-003 shared-nothing mandate to session storage layer",
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
				mcplib.Description(`The repository or project name (e.g. "akashi", "my-langchain-app"). Auto-detected from the git remote when omitted — prefer omitting unless you know the exact canonical name. Do NOT use workspace directory names.`),
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
			mcplib.WithString("precedent_reason",
				mcplib.Description("Brief explanation of why the cited precedent_ref applies to this decision. Helps future agents understand the reasoning lineage without re-reading both decisions. Example: \"extends the first_detected_at fix to also handle rescored decisions\". Omit if precedent_ref is not set."),
			),
			mcplib.WithString("supersedes_id",
				mcplib.Description("UUID of a prior decision that this one explicitly replaces. The superseded decision will be invalidated (valid_to set) and its open conflicts auto-resolved. Use this when your decision reverses or replaces a prior one, rather than just building on it. Omit for new decisions or refinements."),
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
The "task" field is returned for each decision when set (e.g. "codebase review",
"CI pipeline fix"). Use semantic search with the task name as query to find
all decisions from a specific work session.

EXAMPLES:
- Semantic: query="how did we handle rate limiting?"
- By task: query="dashboard redesign" (finds decisions grouped under that task)
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
trail at a glance. Returns trace health metrics (completeness, evidence
coverage, conflict summary), agent count, decision quality statistics,
and the rolling 30-day false_positive_rate (false positive rate for conflict
detection).

The false_positive_rate field shows resolved/false_positive counts and the
ratio false_positive/(resolved+false_positive). An elevated rate signals
LLM validator drift.

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
All statuses are shown by default; pass status to narrow results.`),
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
				mcplib.Description("Filter by status: open, resolved, false_positive. Shows all statuses by default."),
			),
			mcplib.WithString("severity",
				mcplib.Description("Filter by severity: critical, high, medium, low"),
			),
			mcplib.WithString("category",
				mcplib.Description("Filter by category: factual, assessment, strategic, temporal"),
			),
			mcplib.WithString("project",
				mcplib.Description("Filter by project name. Auto-detected from the working directory when omitted. Pass \"*\" to disable filtering and see conflicts across all projects."),
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

	// akashi_resolve — resolve or mark a conflict as false positive.
	s.mcpServer.AddTool(
		mcplib.NewTool("akashi_resolve",
			mcplib.WithDescription(`Resolve a conflict or mark it as a false positive.

WHEN TO USE: When you or the user decides the outcome of a conflict detected
by akashi. For example, after reviewing two contradictory decisions, you can
declare one the winner or mark the conflict as a false positive if the
detector was wrong.

Use the conflict id from akashi_check or akashi_conflicts results. The id
can be either a scored_conflict ID or a conflict_group ID — both work.
When a group ID is provided, all open conflicts in the group are resolved
with the same status and resolution note.

You must provide a status: "resolved" (with a winner) or "false_positive"
(the detector was wrong — automatically labels the conflict for ground truth
tracking so the detector can be improved).

When status is "resolved" and winning_decision_id is provided, the system
will also cascade-resolve other open conflicts in the same group whose
outcome embeddings align with the winner (cosine similarity >= 0.80).

EXAMPLE: After a user says "go with approach A", call akashi_resolve with
  conflict_id="<uuid>", status="resolved",
  winning_decision_id="<uuid of decision A>",
  resolution_note="User chose approach A because ..."`),
			mcplib.WithDestructiveHintAnnotation(false),
			mcplib.WithIdempotentHintAnnotation(false),
			mcplib.WithOpenWorldHintAnnotation(true),
			mcplib.WithString("conflict_id",
				mcplib.Description("UUID of the conflict or conflict group to resolve"),
				mcplib.Required(),
			),
			mcplib.WithString("status",
				mcplib.Description(`New status: "resolved" or "false_positive"`),
				mcplib.Required(),
			),
			mcplib.WithString("winning_decision_id",
				mcplib.Description("UUID of the winning decision. Only valid when status is \"resolved\". Must be one of the two decisions in the conflict."),
			),
			mcplib.WithString("false_positive_label",
				mcplib.Description(`Optional label when status is "false_positive": "unrelated_false_positive" (default) or "related_not_contradicting"`),
			),
			mcplib.WithString("resolution_note",
				mcplib.Description("Optional explanation of why the conflict was resolved this way"),
			),
		),
		s.handleResolve,
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
		// Resolve alias: the agent may have passed a workspace name.
		orgID := ctxutil.OrgIDFromContext(ctx)
		if canonical, err := s.db.ResolveProjectAlias(ctx, orgID, explicit); err == nil && canonical != "" {
			return &canonical
		}
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

	// Notify the IDE hook gate that this agent called akashi_check.
	if s.onCheck != nil {
		s.onCheck(claims.AgentID)
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
	if resp.ConflictsUnavailable {
		summary += " ⚠ Conflict data unavailable due to a transient error — treat conflict list as incomplete."
	}
	if len(resp.PriorResolutions) > 0 {
		summary += fmt.Sprintf(" %d prior conflict(s) for this decision type were formally resolved; winning approach(es) listed in prior_resolutions.", len(resp.PriorResolutions))
	}

	result := map[string]any{
		"has_precedent":     resp.HasPrecedent,
		"summary":           summary,
		"action_needed":     actionNeeded(resp.Conflicts) || resp.ConflictsUnavailable,
		"relevant_count":    len(resp.Decisions),
		"decisions":         compactDecs,
		"conflicts":         compactConfs,
		"prior_resolutions": compactResolutions,
	}

	if resp.ConflictsUnavailable {
		result["conflicts_unavailable"] = true
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

	// Cache the compact check response so handleTrace can auto-inject it
	// as evidence. Both handlers share the same MCP session.
	if session := mcpserver.ClientSessionFromContext(ctx); session != nil {
		if sid := session.SessionID(); sid != "" {
			s.checkCache.Store(sid, string(resultData), len(resp.Decisions) > 0)
		}
	}

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
	// Normalize decision_type for validation and hash computation. The service
	// layer performs canonical normalization, but we normalize here too so the
	// idempotency hash matches regardless of casing.
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

	// Enrich generic agent IDs (e.g., "admin" from JWT) with the MCP tool
	// name so decisions are attributable to the actual client, not the JWT
	// default. The original ID is preserved in agent_context for auditability.
	var agentIDEnriched bool
	var originalAgentID string
	if toolName := mcpToolName(ctx); toolName != "" {
		originalAgentID = agentID
		agentID, agentIDEnriched = deriveAgentID(agentID, toolName)
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

	// Parse precedent_reason: only meaningful when precedent_ref is set.
	var precedentReason *string
	if pr := request.GetString("precedent_reason", ""); pr != "" && precedentRef != nil {
		reason := pr
		if len(reason) > model.MaxPrecedentReasonLen {
			reason = reason[:model.MaxPrecedentReasonLen]
		}
		precedentReason = &reason
	}

	// Parse supersedes_id: marks a prior decision as replaced by this one.
	// Unlike precedent_ref (advisory), supersedes_id has destructive side effects
	// (invalidates a decision, auto-resolves conflicts), so we reject on bad input
	// rather than silently dropping it.
	var supersedesID *uuid.UUID
	if sid := request.GetString("supersedes_id", ""); sid != "" {
		id, parseErr := uuid.Parse(sid)
		if parseErr != nil {
			return errorResult(fmt.Sprintf("supersedes_id is not a valid UUID: %s", sid)), nil
		}
		if id == uuid.Nil {
			return errorResult("supersedes_id must be a valid non-nil UUID"), nil
		}
		supersedesID = &id
	}
	if supersedesID != nil && precedentRef != nil && *supersedesID == *precedentRef {
		return errorResult("supersedes_id and precedent_ref cannot reference the same decision"), nil
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
		if project := inferProjectFromRootsWithGit(roots); project != "" {
			serverCtx["project"] = project
		}
	}

	// Record the original agent_id when it was enriched from the tool name,
	// so the audit trail preserves the mapping from JWT default to derived ID.
	if agentIDEnriched {
		serverCtx["original_agent_id"] = originalAgentID
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

	// Server-inferred model from MCP tool name. Only set when the agent
	// hasn't explicitly provided a model, so explicit values always win
	// (generated column extraction: client > server > flat).
	if _, hasClientModel := clientCtx["model"]; !hasClientModel {
		if toolName, _ := serverCtx["tool"].(string); toolName != "" {
			if inferred := inferModelFromToolName(toolName); inferred != "" {
				serverCtx["model"] = inferred
			}
		}
	}

	// Operator from JWT claims: use the agent's display name if distinct from agent_id.
	if claims != nil {
		agent, agentErr := s.db.GetAgentByAgentID(ctx, orgID, claims.AgentID)
		if agentErr == nil && agent.Name != "" && agent.Name != agent.AgentID {
			clientCtx["operator"] = agent.Name
		}
	}

	// Normalize project: prefer server-inferred (from MCP roots + git remote)
	// over client self-report (which may be a workspace name, not the repo).
	serverProject, _ := serverCtx["project"].(string)
	clientProject, _ := clientCtx["project"].(string)
	if serverProject != "" && clientProject != "" && serverProject != clientProject {
		s.logger.Info("project normalized from server inference",
			"original", clientProject,
			"canonical", serverProject,
		)
		clientCtx["project_submitted"] = clientProject
		clientCtx["project"] = serverProject

		// Auto-create alias so future traces with the same workspace name
		// get normalized even when MCP roots are unavailable.
		//
		// Chain guard (both directions):
		//   1. Don't create A→B if B is itself an alias source (B→C exists).
		//      Otherwise B resolves to C but A resolves to B (stale).
		//   2. Don't create A→B if A is already a canonical target (X→A exists).
		//      Otherwise X resolves to A but A now points to B (broken chain).
		canonicalIsAlias := false
		if existing, err := s.db.ResolveProjectAlias(ctx, orgID, serverProject); err == nil && existing != "" {
			canonicalIsAlias = true
		}
		aliasIsTarget := false
		if isTarget, err := s.db.IsAliasTarget(ctx, orgID, clientProject); err == nil && isTarget {
			aliasIsTarget = true
		}
		switch {
		case canonicalIsAlias:
			s.logger.Debug("skipping alias creation: canonical is itself an alias",
				"alias", clientProject, "canonical", serverProject)
		case aliasIsTarget:
			s.logger.Debug("skipping alias creation: alias name is already a canonical target",
				"alias", clientProject, "canonical", serverProject)
		default:
			if err := s.db.CreateProjectAlias(ctx, orgID, clientProject, serverProject, "system:auto-alias"); err != nil {
				s.logger.Warn("failed to auto-create project alias (non-fatal)",
					"alias", clientProject, "canonical", serverProject, "error", err)
			} else {
				s.logger.Info("auto-created project alias",
					"alias", clientProject, "canonical", serverProject)
			}
		}
	} else if clientProject != "" && serverProject == "" {
		// No server inference available — try alias lookup as fallback.
		canonical, aliasErr := s.db.ResolveProjectAlias(ctx, orgID, clientProject)
		if aliasErr == nil && canonical != "" {
			s.logger.Info("project normalized from alias",
				"original", clientProject,
				"canonical", canonical,
			)
			clientCtx["project_submitted"] = clientProject
			clientCtx["project"] = canonical
		} else {
			// No alias found. Validate against known projects to prevent
			// workspace directory names (e.g. "riyadh-v1") from leaking
			// into the project field. If the org already has projects and
			// this name isn't one of them, reject the trace.
			known, existsErr := s.db.ProjectExists(ctx, orgID, clientProject)
			if existsErr != nil {
				// DB error — fail closed rather than silently accepting
				// an unvalidated project name.
				s.logger.Error("project existence check failed",
					"project", clientProject, "error", existsErr)
				return errorResult(fmt.Sprintf(
					"project validation failed: %v", existsErr,
				)), nil
			}
			if !known {
				hasProjects, hpErr := s.db.HasAnyProjects(ctx, orgID)
				if hpErr != nil {
					s.logger.Error("has-any-projects check failed", "error", hpErr)
					return errorResult(fmt.Sprintf(
						"project validation failed: %v", hpErr,
					)), nil
				}
				if hasProjects {
					return errorResult(fmt.Sprintf(
						"unknown project %q: no server-side git verification available and no alias mapping exists. "+
							"Ensure MCP roots are configured so the server can verify the project from git, "+
							"or ask an admin to create a project alias",
						clientProject,
					)), nil
				}
				// First-ever project in this org — accept it to bootstrap.
				s.logger.Info("accepting unverified project for new org",
					"project", clientProject, "org_id", orgID)
			}
		}
	}

	// Reject traces with no project when the org already has projects.
	// Project can come from server inference (MCP roots + git) or client self-report.
	finalProject, _ := clientCtx["project"].(string)
	if finalProject == "" {
		if sp, _ := serverCtx["project"].(string); sp != "" {
			finalProject = sp
		}
	}
	if finalProject == "" {
		hasProjects, hpErr := s.db.HasAnyProjects(ctx, orgID)
		if hpErr != nil {
			s.logger.Error("has-any-projects check failed", "error", hpErr)
			return errorResult(fmt.Sprintf("project validation failed: %v", hpErr)), nil
		}
		if hasProjects {
			return errorResult(
				"project is required: provide project in the trace call, set repo_url in context, " +
					"or configure MCP roots so the server can detect the project from git",
			), nil
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
		payloadHash, hashErr := mcpTraceHash(agentID, decisionType, outcome, confidence, reasoning, evidence, alternatives, precedentRef, supersedesID)
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

	// Auto-attach the preceding akashi_check response as evidence when the
	// agent provided none. This injects the research step into the decision
	// record automatically, rather than suggesting it after the fact.
	if len(evidence) == 0 {
		if session := mcpserver.ClientSessionFromContext(ctx); session != nil {
			if sid := session.SessionID(); sid != "" {
				if checkResult := s.checkCache.Drain(sid); checkResult != "" {
					relevance := float32(0.6)
					sourceURI := "akashi://check"
					evidence = []model.TraceEvidence{{
						SourceType:     "tool_output",
						SourceURI:      &sourceURI,
						Content:        checkResult,
						RelevanceScore: &relevance,
					}}
				}
			}
		}
	}

	result, err := s.decisionSvc.Trace(ctx, orgID, decisions.TraceInput{
		AgentID:         agentID,
		SessionID:       sessionID,
		AgentContext:    agentContext,
		APIKeyID:        apiKeyID,
		AuditMeta:       auditMeta,
		PrecedentRef:    precedentRef,
		PrecedentReason: precedentReason,
		SupersedesID:    supersedesID,
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
	}, precedentRef != nil)

	// Precedent penalty: when the agent called akashi_check, got results, but
	// didn't cite any precedent, apply a -0.05 scoring penalty. This is a
	// server-side incentive to build the attribution graph.
	checkHadResults := false
	if precedentRef == nil {
		if session := mcpserver.ClientSessionFromContext(ctx); session != nil {
			if sid := session.SessionID(); sid != "" {
				checkHadResults = s.checkCache.HadResults(sid)
			}
		}
	}
	if checkHadResults {
		completenessScore -= 0.05
		if completenessScore < 0 {
			completenessScore = 0
		}
	}

	hasModel := clientCtx["model"] != nil || serverCtx["model"] != nil
	missing := computeMissingFields(decisionType, outcome, confidence, reasoningPtr, alternatives, evidence, precedentRef != nil, hasModel, request.GetString("task", "") != "", s.standardTypes)

	// Precedent nudge: when the agent called akashi_check, got results, but
	// didn't cite any precedent, flag the missed opportunity and add a tip.
	if checkHadResults {
		missing = append(missing, "akashi_check returned relevant precedents but you didn't cite any — pass precedent_ref to build the attribution graph")
	}

	responseMap := map[string]any{
		"run_id":             result.RunID,
		"decision_id":        result.DecisionID,
		"status":             "recorded",
		"completeness_score": fmt.Sprintf("%.0f%%", completenessScore*100),
	}
	// Surface decision type normalization when an alias or Levenshtein match
	// changed the stored type from what the agent submitted.
	if result.Decision.DecisionType != decisionType {
		responseMap["decision_type_normalized"] = result.Decision.DecisionType
		responseMap["original_decision_type"] = decisionType
	}
	if len(missing) > 0 {
		responseMap["completeness_tips"] = missing
	}
	if warnings := model.HighConfidenceWarnings(confidence, len(evidence), s.highConfidenceWarnThreshold); len(warnings) > 0 {
		responseMap["warnings"] = warnings
	}
	if checkHadResults {
		responseMap["precedent_ref_missed"] = true
	}

	// Surface confidence adjustment so agents know their value was deflated.
	reasoningLen := len(strings.TrimSpace(reasoning))
	confAdj := quality.AdjustConfidence(confidence, len(evidence), len(alternatives), reasoningLen)
	if confAdj.WasAdjusted {
		responseMap["confidence_adjusted"] = true
		responseMap["original_confidence"] = fmt.Sprintf("%.2f", confAdj.Original)
		responseMap["stored_confidence"] = fmt.Sprintf("%.2f", confAdj.Adjusted)
		responseMap["confidence_reasons"] = confAdj.Reasons
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
// completeness score impact (highest first) and are profile-aware: decision types
// that don't expect alternatives or evidence won't get tips for those factors.
// hasModel is true when the model field is either explicitly provided or
// successfully inferred from session metadata / HTTP headers; tips are only
// surfaced when neither source produced a value.
func computeMissingFields(decisionType, outcome string, confidence float32, reasoning *string, alternatives []model.TraceAlternative, evidence []model.TraceEvidence, hasPrecedentRef, hasModel, hasTask bool, standardTypes map[string]bool) []string {
	profile := quality.ProfileFor(decisionType, nil)
	var tips []string

	// Reasoning: biggest single factor. Weight increases when alternatives
	// or evidence weight is redistributed to reasoning by the profile.
	reasoningPctLabel := "30"
	if !profile.AlternativesExpected || profile.MinEvidence == 0 {
		reasoningPctLabel = "up to 65"
	}
	if reasoning == nil || len(strings.TrimSpace(*reasoning)) <= 100 {
		if reasoning == nil || len(strings.TrimSpace(*reasoning)) <= 20 {
			tips = append(tips, fmt.Sprintf("Add reasoning (>100 chars) explaining why you chose this over alternatives (+%s%%)", reasoningPctLabel))
		} else {
			tips = append(tips, "Expand reasoning to >100 chars for full credit (+5-15%)")
		}
	}

	// Alternatives with substantive rejection reasons (up to 0.20).
	// Only suggest when the profile expects alternatives.
	if profile.AlternativesExpected {
		substantive := 0
		for _, alt := range alternatives {
			if alt.RejectionReason != nil && len(strings.TrimSpace(*alt.RejectionReason)) > 20 {
				substantive++
			}
		}
		if substantive < 3 {
			tips = append(tips, fmt.Sprintf("Add %d more rejected alternatives with rejection_reason >20 chars (+%d%%)", 3-substantive, (3-substantive)*5))
		}
	}

	// Evidence (up to 0.15). Only suggest when the profile expects evidence.
	if profile.MinEvidence > 0 {
		if len(evidence) < 2 {
			if len(evidence) == 0 {
				tips = append(tips, "Add evidence to make this trace verifiable: attach file paths, error messages, test output, benchmark numbers, or the constraint that drove the choice (source_type + content, 2+ items for +15%)")
			} else {
				tips = append(tips, "Add 1 more evidence item for full credit — e.g. a file path, error message, test result, or benchmark number (+5%)")
			}
		}
	}

	// Confidence calibration nudge.
	if confidence >= 0.95 || confidence <= 0.05 {
		tips = append(tips, "Confidence is at an extreme — values between 0.4 and 0.8 are more informative")
	}

	// Profile-specific confidence penalty warning.
	if len(evidence) == 0 && confidence > profile.MaxConfidenceNoEvidence {
		tips = append(tips, fmt.Sprintf("Confidence %.2f exceeds max %.2f for %s without evidence — add evidence or lower confidence to avoid penalty",
			confidence, profile.MaxConfidenceNoEvidence, decisionType))
	}

	// Suggest standard type when the input is close to one (typo, delimiter variant).
	// No scoring penalty for custom types — this is purely informational.
	if !standardTypes[decisionType] {
		if suggestion := quality.SuggestStandardType(decisionType, standardTypes, 3); suggestion != "" {
			tips = append(tips, fmt.Sprintf("Did you mean %q? Your type %q is close to a standard type", suggestion, decisionType))
		}
	}

	// Precedent reference (0.10).
	if !hasPrecedentRef {
		tips = append(tips, "Set precedent_ref to the precedent_ref_hint from akashi_check to build the attribution graph (+10%)")
	}

	// Model attribution (not scored, but critical for analysis).
	if !hasModel {
		tips = append(tips, `Pass "model" (e.g. "claude-opus-4-6") so decisions can be correlated by model capability tier`)
	}

	// Substantive outcome (0.10).
	if len(strings.TrimSpace(outcome)) <= 20 {
		tips = append(tips, "Make outcome more specific (>20 chars) for +10%")
	}

	// Task label: not a scoring factor, but useful for grouping related decisions.
	if !hasTask {
		tips = append(tips, "Add task (e.g. \"codebase review\", \"CI pipeline fix\") to group related decisions from this work session")
	}

	return tips
}

// knownToolModels maps MCP client tool names (lowercase) to model families.
// Only includes tools that exclusively use a single vendor's models, so the
// inference is defensible. Agents should still provide the exact model string
// via the "model" parameter for full precision — this is a best-effort fallback
// that prevents NULL when the agent forgets.
var knownToolModels = map[string]string{
	"claude-code":    "claude",
	"claude-desktop": "claude",
}

// genericAgentIDs are agent IDs that represent the JWT default or system
// accounts rather than a meaningful agent identity. When a trace arrives with
// one of these IDs and the MCP session carries a tool name, we derive a
// richer agent_id so that decisions are attributable to the actual tool.
var genericAgentIDs = map[string]bool{
	"admin":  true,
	"system": true,
}

// deriveAgentID enriches a generic agent_id (e.g., "admin" from JWT defaults)
// with the MCP tool name extracted from the session. Returns the original
// agent_id unchanged when it is not generic or when no tool name is available.
//
// The derived format is "{tool}" (e.g., "claude-code"). The session prefix is
// intentionally omitted to keep agent cardinality manageable — one agent per
// tool, not one per session.
func deriveAgentID(agentID, toolName string) (derived string, wasEnriched bool) {
	if !genericAgentIDs[agentID] {
		return agentID, false
	}
	toolName = strings.ToLower(strings.TrimSpace(toolName))
	if toolName == "" {
		return agentID, false
	}
	// Sanitize tool name to valid agent_id characters (a-z, 0-9, -, _, .).
	var b strings.Builder
	for i := 0; i < len(toolName); i++ {
		c := toolName[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			b.WriteByte(c)
		} else if c == ' ' {
			b.WriteByte('-')
		}
		// Drop other characters silently.
	}
	derived = b.String()
	if derived == "" || model.ValidateAgentID(derived) != nil {
		return agentID, false
	}
	return derived, true
}

// mcpToolName extracts the MCP client tool name from the session context.
// Returns "" when no session or client info is available.
func mcpToolName(ctx context.Context) string {
	session := mcpserver.ClientSessionFromContext(ctx)
	if session == nil {
		return ""
	}
	if cis, ok := session.(mcpserver.SessionWithClientInfo); ok {
		return cis.GetClientInfo().Name
	}
	return ""
}

// inferModelFromToolName returns a model family identifier for well-known MCP
// tools that are tied to a single model vendor. Returns "" when the tool name
// is unknown or ambiguous (e.g., "cursor" can use any model).
func inferModelFromToolName(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	if m, ok := knownToolModels[lower]; ok {
		return m
	}
	return ""
}

// mcpTraceHash computes a deterministic SHA-256 hash of the trace parameters
// used for idempotency payload comparison. precedentRef and supersedesID are
// included so that the same outcome recorded with different linkage is treated
// as a distinct payload (rather than a replay of the original). This is
// especially important for supersedesID, which has write side effects
// (invalidating a decision, auto-resolving conflicts).
func mcpTraceHash(agentID, decisionType, outcome string, confidence float32, reasoning string, evidence []model.TraceEvidence, alternatives []model.TraceAlternative, precedentRef *uuid.UUID, supersedesID *uuid.UUID) (string, error) {
	var prStr *string
	if precedentRef != nil {
		s := precedentRef.String()
		prStr = &s
	}
	var ssStr *string
	if supersedesID != nil {
		s := supersedesID.String()
		ssStr = &s
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
		"supersedes_id": ssStr,
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

	// Build group filters. By default the MCP tool shows all groups so agents
	// see both open and resolved conflicts. Agents can pass status="open",
	// "resolved", etc. to narrow results.
	statusFilter := request.GetString("status", "")
	groupFilters := storage.ConflictGroupFilters{}
	if statusFilter != "" && statusFilter != "all" {
		groupFilters.Status = &statusFilter
	}
	if dt := request.GetString("decision_type", ""); dt != "" {
		groupFilters.DecisionType = &dt
	}
	if aid := request.GetString("agent_id", ""); aid != "" {
		groupFilters.AgentID = &aid
	}

	// Use the category/severity/project filters from the request to post-filter
	// on the representative conflict — they are not group-level columns.
	severityFilter := request.GetString("severity", "")
	categoryFilter := request.GetString("category", "")
	projectFilter := s.resolveProjectFilter(ctx, request)

	groups, err := s.db.ListConflictGroups(ctx, orgID, groupFilters, limit, 0)
	if err != nil {
		return errorResult(fmt.Sprintf("list conflict groups failed: %v", err)), nil
	}

	// Post-filter by severity/category/project on the representative conflict.
	if severityFilter != "" || categoryFilter != "" || projectFilter != nil {
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
			if projectFilter != nil {
				pa := g.Representative.ProjectA
				pb := g.Representative.ProjectB
				if (pa == nil || *pa != *projectFilter) && (pb == nil || *pb != *projectFilter) {
					continue
				}
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

// cascadeSimilarityThreshold is the minimum cosine similarity for cascade resolution.
const cascadeSimilarityThreshold = 0.80

func (s *Server) handleResolve(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	orgID := ctxutil.OrgIDFromContext(ctx)
	claims := ctxutil.ClaimsFromContext(ctx)

	if claims == nil {
		return errorResult("authentication required"), nil
	}

	// Parse and validate conflict_id.
	conflictIDStr := request.GetString("conflict_id", "")
	if conflictIDStr == "" {
		return errorResult("conflict_id is required"), nil
	}
	conflictID, err := uuid.Parse(conflictIDStr)
	if err != nil {
		return errorResult("conflict_id must be a valid UUID"), nil
	}

	// Parse and validate status.
	status := request.GetString("status", "")
	switch status {
	case "resolved", "false_positive":
		// valid
	default:
		return errorResult(`status must be one of: "resolved", "false_positive"`), nil
	}

	// Parse optional winning_decision_id.
	var winningDecisionID *uuid.UUID
	if winStr := request.GetString("winning_decision_id", ""); winStr != "" {
		wid, parseErr := uuid.Parse(winStr)
		if parseErr != nil {
			return errorResult("winning_decision_id must be a valid UUID"), nil
		}
		winningDecisionID = &wid
	}

	// winning_decision_id only makes sense with "resolved".
	if winningDecisionID != nil && status != "resolved" {
		return errorResult("winning_decision_id can only be set when status is 'resolved'"), nil
	}

	// Parse optional resolution note.
	var resolutionNote *string
	if n := request.GetString("resolution_note", ""); n != "" {
		resolutionNote = &n
	}

	resolvedBy := claims.AgentID
	if resolvedBy == "" {
		resolvedBy = claims.Subject
	}

	actorRole := string(claims.Role)

	// Compute false_positive label once — passed to both resolution paths.
	rawFPLabel := request.GetString("false_positive_label", "")
	fpLabel := storage.ComputeFPLabel(status, &rawFPLabel)

	// Attempt single-conflict resolution first. UpdateConflictStatusWithAudit
	// uses SELECT ... FOR UPDATE inside its transaction, so there is no
	// read-then-write race. If the ID doesn't match a scored_conflict, fall
	// back to group resolution.
	singleResult, singleErr := s.resolveSingleConflict(ctx, conflictID, orgID, status, resolvedBy, actorRole, resolutionNote, winningDecisionID, fpLabel)
	if singleErr != nil {
		return nil, singleErr
	}
	if singleResult != nil {
		return singleResult, nil
	}

	// Not a scored_conflict ID — try resolving as a conflict_group ID.
	return s.resolveGroup(ctx, conflictID, orgID, status, resolvedBy, actorRole, resolutionNote, winningDecisionID, fpLabel)
}

// resolveSingleConflict attempts to resolve the given ID as a scored_conflict.
// Returns (nil, nil) if the ID doesn't match any scored_conflict, signaling
// the caller to try group resolution instead. This avoids a read-then-write
// race — UpdateConflictStatusWithAudit uses SELECT ... FOR UPDATE internally.
func (s *Server) resolveSingleConflict(
	ctx context.Context,
	conflictID, orgID uuid.UUID,
	status, resolvedBy, actorRole string,
	resolutionNote *string,
	winningDecisionID *uuid.UUID,
	fpLabel *string,
) (*mcplib.CallToolResult, error) {
	audit := storage.MutationAuditEntry{
		OrgID:        orgID,
		ActorAgentID: resolvedBy,
		ActorRole:    actorRole,
		Endpoint:     "mcp/akashi_resolve",
		Operation:    "conflict_status_changed",
		ResourceType: "conflict",
		ResourceID:   conflictID.String(),
		Metadata:     map[string]any{"new_status": status, "resolved_by": resolvedBy},
	}

	oldStatus, err := s.db.UpdateConflictStatusWithAudit(ctx, conflictID, orgID, status, resolvedBy, resolutionNote, winningDecisionID, fpLabel, audit)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// Not a scored_conflict — return nil to signal fallback to group path.
			return nil, nil
		}
		if errors.Is(err, storage.ErrWinningDecisionNotInConflict) {
			return errorResult("winning_decision_id must be one of the two decisions in this conflict"), nil
		}
		return errorResult(fmt.Sprintf("failed to update conflict: %v", err)), nil
	}

	// Post-resolution: auto-assess conflict outcome and cascade to group.
	var cascaded int
	if status == "resolved" && winningDecisionID != nil {
		conflict, cErr := s.db.GetConflict(ctx, conflictID, orgID)
		if cErr == nil && conflict != nil {
			// Auto-assess: winner is correct, loser is incorrect.
			if s.autoAssessor != nil {
				loserID := conflict.DecisionAID
				if loserID == *winningDecisionID {
					loserID = conflict.DecisionBID
				}
				s.autoAssessor.OnConflictResolved(ctx, orgID, *winningDecisionID, loserID)
			}

			// Cascade resolution to other conflicts in the same group.
			if conflict.GroupID != nil {
				cascadeAudit := storage.MutationAuditEntry{
					OrgID:        orgID,
					ActorAgentID: resolvedBy,
					ActorRole:    actorRole,
					Endpoint:     "mcp/akashi_resolve",
					Operation:    "conflict_cascade_resolved",
					ResourceType: "conflict",
					ResourceID:   conflictID.String(),
					Metadata:     map[string]any{"trigger_conflict_id": conflictID.String(), "winning_decision_id": winningDecisionID.String()},
				}
				var cascadeErr error
				cascaded, cascadeErr = s.db.CascadeResolveByOutcome(ctx, orgID, *conflict.GroupID, *winningDecisionID, conflictID, cascadeSimilarityThreshold, cascadeAudit)
				if cascadeErr != nil {
					s.logger.Warn("mcp: resolution cascade failed",
						"trigger_conflict_id", conflictID,
						"group_id", conflict.GroupID,
						"error", cascadeErr,
					)
				} else if cascaded > 0 {
					s.logger.Info("mcp: resolution cascade resolved conflicts",
						"trigger_conflict_id", conflictID,
						"group_id", conflict.GroupID,
						"cascade_resolved", cascaded,
					)
				}
			}
		}
	}

	resultData, _ := json.MarshalIndent(map[string]any{
		"conflict_id":      conflictID,
		"old_status":       oldStatus,
		"new_status":       status,
		"resolved_by":      resolvedBy,
		"cascade_resolved": cascaded,
	}, "", "  ")

	return &mcplib.CallToolResult{
		Content: []mcplib.Content{
			mcplib.TextContent{Type: "text", Text: string(resultData)},
		},
	}, nil
}

// resolveGroup atomically resolves all open conflicts in a conflict group
// by delegating to the storage layer's ResolveConflictGroup, which performs
// the entire operation — including false_positive labeling — in a single
// transaction with one audit entry.
func (s *Server) resolveGroup(
	ctx context.Context,
	groupID, orgID uuid.UUID,
	status, resolvedBy, actorRole string,
	resolutionNote *string,
	winningDecisionID *uuid.UUID,
	fpLabel *string,
) (*mcplib.CallToolResult, error) {
	// Convert winning_decision_id → winning agent. The storage method resolves
	// per-conflict winners atomically via SQL CASE on agent_a/agent_b, which is
	// correct because a group is defined by a fixed (agent_a, agent_b) pair.
	var winningAgent *string
	if winningDecisionID != nil {
		decs, err := s.db.GetDecisionsByIDs(ctx, orgID, []uuid.UUID{*winningDecisionID})
		if err != nil {
			return errorResult(fmt.Sprintf("failed to look up winning decision: %v", err)), nil
		}
		dec, ok := decs[*winningDecisionID]
		if !ok {
			return errorResult("winning_decision_id not found"), nil
		}
		winningAgent = &dec.AgentID
	}

	audit := storage.MutationAuditEntry{
		OrgID:        orgID,
		ActorAgentID: resolvedBy,
		ActorRole:    actorRole,
		Endpoint:     "mcp/akashi_resolve",
		Operation:    "conflict_group_resolved",
		ResourceType: "conflict_group",
		ResourceID:   groupID.String(),
		Metadata:     map[string]any{"new_status": status, "resolved_by": resolvedBy},
	}

	affected, err := s.db.ResolveConflictGroup(ctx, groupID, orgID, status, resolvedBy, resolutionNote, winningAgent, fpLabel, audit)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return errorResult("conflict not found (no scored_conflict or conflict_group matches this ID)"), nil
		}
		if errors.Is(err, storage.ErrWinningAgentNotInGroup) {
			return errorResult("winning_decision_id belongs to an agent that is not a participant in this conflict group"), nil
		}
		if errors.Is(err, storage.ErrRevisedDecisions) {
			return errorResult(fmt.Sprintf("cannot resolve group with winner: %v", err)), nil
		}
		return errorResult(fmt.Sprintf("failed to resolve conflict group: %v", err)), nil
	}

	if affected > 0 {
		s.logger.Info("mcp: resolved conflict group",
			"group_id", groupID,
			"resolved_count", affected,
		)
	}

	oldStatusStr := "open"
	if affected == 0 {
		oldStatusStr = "already_resolved"
	}

	resultData, _ := json.MarshalIndent(map[string]any{
		"group_id":       groupID,
		"old_status":     oldStatusStr,
		"new_status":     status,
		"resolved_by":    resolvedBy,
		"resolved_count": affected,
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
	metrics, err := svc.Compute(ctx, orgID, nil, nil)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to compute trace health: %v", err)), nil
	}

	agentCount, err := s.db.CountAgents(ctx, orgID)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to count agents: %v", err)), nil
	}

	falsePositiveRate, err := s.db.GetFalsePositiveRate(ctx, orgID)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to get false positive rate: %v", err)), nil
	}

	resultData, _ := json.MarshalIndent(map[string]any{
		"trace_health":        metrics,
		"agents":              agentCount,
		"false_positive_rate": falsePositiveRate,
	}, "", "  ")

	return &mcplib.CallToolResult{
		Content: []mcplib.Content{
			mcplib.TextContent{Type: "text", Text: string(resultData)},
		},
	}, nil
}
