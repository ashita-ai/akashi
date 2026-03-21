// TypeScript types mirroring Go models.

export interface ResponseMeta {
  request_id: string;
  timestamp: string;
}

export interface APIResponse<T> {
  data: T;
  meta: ResponseMeta;
}

export interface APIError {
  error: {
    code: string;
    message: string;
    details?: unknown;
  };
  meta: ResponseMeta;
}

// Auth
export interface AuthTokenRequest {
  agent_id: string;
  api_key: string;
}

export interface AuthTokenResponse {
  token: string;
  expires_at: string;
}

// Agent
export type AgentRole =
  | "admin"
  | "agent"
  | "reader";

export interface Agent {
  id: string;
  agent_id: string;
  org_id: string;
  name: string;
  role: AgentRole;
  metadata: Record<string, unknown> | null;
  created_at: string;
  updated_at: string;
}

export interface CreateAgentRequest {
  agent_id: string;
  name: string;
  role: AgentRole;
  api_key: string;
  metadata?: Record<string, unknown>;
}

// Decision
export interface Decision {
  id: string;
  run_id: string;
  agent_id: string;
  org_id: string;
  decision_type: string;
  outcome: string;
  confidence: number;
  reasoning: string | null;
  metadata: Record<string, unknown> | null;
  completeness_score: number;
  outcome_score: number | null;
  precedent_ref: string | null;
  session_id?: string | null;
  project?: string | null;
  agent_context?: Record<string, unknown>;
  tool?: string;
  model?: string;
  valid_from: string;
  valid_to: string | null;
  transaction_time: string;
  created_at: string;
  alternatives?: Alternative[];
  evidence?: Evidence[];
}

export interface Alternative {
  id: string;
  decision_id: string;
  label: string;
  score: number | null;
  selected: boolean;
  rejection_reason: string | null;
  metadata: Record<string, unknown> | null;
  created_at: string;
}

export interface Evidence {
  id: string;
  decision_id: string;
  source_type: string;
  source_uri: string | null;
  content: string;
  relevance_score: number | null;
  metadata: Record<string, unknown> | null;
  created_at: string;
}

// Run
export type RunStatus = "running" | "completed" | "failed";

export interface AgentRun {
  id: string;
  agent_id: string;
  org_id: string;
  trace_id: string | null;
  parent_run_id: string | null;
  status: RunStatus;
  started_at: string;
  completed_at: string | null;
  metadata: Record<string, unknown> | null;
  created_at: string;
  events?: AgentEvent[];
  decisions?: Decision[];
}

// Event
export type EventType =
  | "agent_run_started"
  | "agent_run_completed"
  | "agent_run_failed"
  | "decision_started"
  | "alternative_considered"
  | "evidence_gathered"
  | "reasoning_step_completed"
  | "decision_made"
  | "decision_revised"
  | "tool_call_started"
  | "tool_call_completed"
  | "agent_handoff"
  | "consensus_requested"
  | "conflict_detected";

export interface AgentEvent {
  id: string;
  run_id: string;
  org_id: string;
  event_type: EventType;
  sequence_num: number;
  occurred_at: string;
  agent_id: string;
  payload: Record<string, unknown>;
  created_at: string;
}

// Conflict
export type ConflictKind = "cross_agent" | "self_contradiction";

export interface DecisionConflict {
  id: string;
  conflict_kind: ConflictKind;
  decision_a_id: string;
  decision_b_id: string;
  org_id: string;
  agent_a: string;
  agent_b: string;
  run_a: string;
  run_b: string;
  decision_type: string;
  outcome_a: string;
  outcome_b: string;
  confidence_a: number;
  confidence_b: number;
  reasoning_a: string | null;
  reasoning_b: string | null;
  decided_at_a: string;
  decided_at_b: string;
  detected_at: string;
  explanation: string | null;
  category: ConflictCategory | null;
  severity: ConflictSeverity | null;
  status: ConflictStatus;
  resolved_by: string | null;
  resolved_at: string | null;
  resolution_note: string | null;
}

// Recommendation (computed by GET /v1/conflicts/{id})
export interface Recommendation {
  suggested_winner: string;
  reasons: string[];
  confidence: number;
}

// ConflictDetail extends DecisionConflict with lazily-computed fields.
export interface ConflictDetail extends DecisionConflict {
  recommendation?: Recommendation;
}

// Search
export interface SearchResult {
  decision: Decision;
  similarity_score: number;
}

// Query
export interface QueryFilters {
  agent_id?: string[];
  run_id?: string;
  decision_type?: string;
  confidence_min?: number;
  outcome?: string;
  project?: string;
  time_range?: {
    from: string;
    to: string;
  };
}

export interface DecisionFacets {
  types: string[];
  projects: string[];
}

export interface QueryRequest {
  filters: QueryFilters;
  include?: string[];
  order_by?: string;
  order_dir?: string;
  limit: number;
  offset: number;
}

export interface PaginatedDecisions {
  decisions: Decision[];
  total: number;
  count: number;
  limit: number;
  offset: number;
}

export interface ConflictsList {
  conflicts: DecisionConflict[];
  total: number;
  limit: number;
  offset: number;
}

export interface ConflictGroup {
  id: string;
  agent_a: string;
  agent_b: string;
  conflict_kind: ConflictKind;
  decision_type: string;
  first_detected_at: string;
  last_detected_at: string;
  conflict_count: number;
  open_count: number;
  representative?: DecisionConflict;
  /** All open/acknowledged conflicts in this group, ordered by significance DESC. */
  open_conflicts?: DecisionConflict[];
}

export interface ConflictGroupsList {
  conflict_groups: ConflictGroup[];
  total: number;
  limit: number;
  offset: number;
}

export interface AgentsList {
  agents: Agent[];
}

export interface SearchResponse {
  results: SearchResult[];
  total: number;
}

// Conflict lifecycle
export type ConflictStatus = "open" | "acknowledged" | "resolved" | "wont_fix";
export type ConflictCategory = "factual" | "assessment" | "strategic" | "temporal";
export type ConflictSeverity = "critical" | "high" | "medium" | "low";

// Agent stats
export interface AgentStats {
  agent_id: string;
  decision_count: number;
  avg_confidence: number;
  first_decision_at: string | null;
  last_decision_at: string | null;
  low_completeness_count: number;
  type_breakdown: Record<string, number>;
}

// Outcome signals — mirrors storage.OutcomeSignalsSummary from Go
export interface OutcomeSignals {
  decisions_total: number;
  never_superseded: number;
  revised_within_48h: number;
  never_cited: number;
  cited_at_least_once: number;
  conflicts_won: number;
  conflicts_lost: number;
  conflicts_no_winner: number;
}

// Decision type distribution — mirrors storage.DecisionTypeCount from Go
export interface DecisionTypeCount {
  decision_type: string;
  count: number;
}

// Trace health — mirrors tracehealth.Metrics from Go
export interface TraceHealth {
  status: string;
  completeness: TraceHealthCompleteness;
  evidence: TraceHealthEvidence;
  conflicts?: TraceHealthConflicts;
  outcome_signals?: OutcomeSignals;
  decision_type_distribution?: DecisionTypeCount[];
  gaps: string[];
}

export interface TraceHealthCompleteness {
  total_decisions: number;
  avg_completeness: number;
  below_half: number;
  below_third: number;
  with_reasoning: number;
  reasoning_pct: number;
  with_alternatives: number;
  alternatives_pct: number;
}

export interface TraceHealthEvidence {
  total_decisions: number;
  total_records: number;
  avg_per_decision: number;
  with_evidence: number;
  without_evidence: number;
  coverage_pct: number;
}

export interface TraceHealthConflicts {
  total: number;
  open: number;
  acknowledged: number;
  resolved: number;
  wont_fix: number;
  resolved_pct: number;
}

// Session view
export interface SessionView {
  session_id: string;
  decisions: Decision[];
  decision_count: number;
  summary: SessionSummary;
}

export interface SessionSummary {
  started_at: string;
  ended_at: string;
  duration_secs: number;
  decision_types: Record<string, number>;
  avg_confidence: number;
}

// Grant
export interface Grant {
  id: string;
  org_id: string;
  grantor_id: string;
  grantee_id: string;
  resource_type: string;
  resource_id: string | null;
  permission: string;
  granted_at: string;
  expires_at: string | null;
}

export interface GrantsList {
  grants: Grant[];
  total: number;
  limit: number;
  offset: number;
  has_more: boolean;
}

export interface CreateGrantRequest {
  grantee_agent_id: string;
  resource_type: string;
  resource_id?: string;
  permission: string;
  expires_at?: string;
}

// Conflict analytics
export interface ConflictAnalytics {
  period: { start: string; end: string };
  summary: {
    total_detected: number;
    total_resolved: number;
    mean_time_to_resolution_hours: number | null;
    false_positive_rate: number;
  };
  by_agent_pair: AgentPairConflictStats[];
  by_decision_type: DecisionTypeConflictStats[];
  by_severity: SeverityConflictStats[];
  trend: ConflictTrendPoint[];
}

export interface AgentPairConflictStats {
  agent_a: string;
  agent_b: string;
  count: number;
  open: number;
  resolved: number;
}

export interface DecisionTypeConflictStats {
  decision_type: string;
  count: number;
  avg_significance: number;
}

export interface SeverityConflictStats {
  severity: string;
  count: number;
}

export interface ConflictTrendPoint {
  date: string;
  detected: number;
  resolved: number;
}

// Lineage
export interface LineageEntry {
  id: string;
  run_id: string;
  agent_id: string;
  decision_type: string;
  outcome: string;
  confidence: number;
  project?: string | null;
  created_at: string;
  valid_from: string;
  valid_to?: string | null;
}

export interface DecisionLineage {
  decision_id: string;
  preceded_by: LineageEntry | null;
  cited_by: LineageEntry[];
  cited_by_has_more: boolean;
}

// Health
export interface HealthResponse {
  status: string;
  version: string;
  postgres: string;
  uptime_seconds: number;
}
