/** A recorded decision with bi-temporal modeling. */
export interface Decision {
  id: string;
  run_id: string;
  agent_id: string;
  org_id: string;
  decision_type: string;
  outcome: string;
  confidence: number;
  reasoning?: string;
  metadata: Record<string, unknown>;
  completeness_score: number;
  precedent_ref?: string;
  supersedes_id?: string;
  content_hash?: string;
  tags?: string[];
  /** Composite agent identity (spec 31). */
  session_id?: string;
  agent_context?: Record<string, unknown>;
  valid_from: string;
  valid_to?: string;
  transaction_time: string;
  created_at: string;
  alternatives?: Alternative[];
  evidence?: Evidence[];
}

/** An option considered for a decision. */
export interface Alternative {
  id: string;
  decision_id: string;
  label: string;
  score?: number;
  selected: boolean;
  rejection_reason?: string;
  metadata: Record<string, unknown>;
  created_at: string;
}

/** Supporting information for a decision. */
export interface Evidence {
  id: string;
  decision_id: string;
  source_type: string;
  source_uri?: string;
  content: string;
  relevance_score?: number;
  metadata: Record<string, unknown>;
  created_at: string;
}

export type ConflictKind = "cross_agent" | "self_contradiction";

/** A detected conflict between two decisions. */
export interface DecisionConflict {
  conflict_kind: ConflictKind;
  decision_a_id: string;
  decision_b_id: string;
  org_id: string;
  agent_a: string;
  agent_b: string;
  run_a: string;
  run_b: string;
  decision_type: string;
  decision_type_a?: string;
  decision_type_b?: string;
  outcome_a: string;
  outcome_b: string;
  confidence_a: number;
  confidence_b: number;
  reasoning_a?: string;
  reasoning_b?: string;
  decided_at_a: string;
  decided_at_b: string;
  detected_at: string;
  topic_similarity?: number;
  outcome_divergence?: number;
  significance?: number;
  scoring_method?: string;
}

/** An agent run (a unit of work that can contain decisions and events). */
export interface AgentRun {
  id: string;
  agent_id: string;
  org_id: string;
  trace_id?: string;
  parent_run_id?: string;
  status: string;
  metadata: Record<string, unknown>;
  started_at: string;
  completed_at?: string;
  created_at: string;
}

/** An event within an agent run. */
export interface AgentEvent {
  id: string;
  run_id: string;
  org_id: string;
  event_type: string;
  sequence_num: number;
  occurred_at: string;
  agent_id: string;
  trace_id?: string;
  span_id?: string;
  payload: Record<string, unknown>;
  created_at: string;
}

/** A registered agent. */
export interface Agent {
  id: string;
  agent_id: string;
  org_id: string;
  name: string;
  role: string;
  tags: string[];
  metadata: Record<string, unknown>;
  created_at: string;
  updated_at: string;
}

/** An access grant between agents. */
export interface Grant {
  id: string;
  org_id: string;
  grantor_id: string;
  grantee_id: string;
  resource_type: string;
  resource_id?: string;
  permission: string;
  granted_at: string;
  expires_at?: string;
}

/** Health check payload returned in the standard API envelope. */
export interface HealthResponse {
  status: string;
  version: string;
  postgres: string;
  qdrant?: string;
  buffer_depth: number;
  buffer_status: "ok" | "high" | "critical";
  sse_broker?: string;
  uptime_seconds: number;
}

// --- Request types ---

/** Request body for recording a decision. */
export interface TraceRequest {
  decisionType: string;
  outcome: string;
  confidence: number;
  reasoning?: string;
  alternatives?: TraceAlternative[];
  evidence?: TraceEvidence[];
  metadata?: Record<string, unknown>;
  context?: Record<string, unknown>;
}

export interface TraceAlternative {
  label: string;
  score?: number;
  selected?: boolean;
  rejection_reason?: string;
}

export interface TraceEvidence {
  source_type: string;
  source_uri?: string;
  content: string;
  relevance_score?: number;
}

export interface QueryFilters {
  agent_id?: string[];
  decision_type?: string;
  confidence_min?: number;
  outcome?: string;
  /** Composite agent identity filters (spec 31). */
  session_id?: string;
  tool?: string;
  model?: string;
  project?: string;
}

/** Request body for creating a run. */
export interface CreateRunRequest {
  traceId?: string;
  parentRunId?: string;
  metadata?: Record<string, unknown>;
}

/** An event to append to a run. */
export interface EventInput {
  eventType: string;
  occurredAt?: string;
  payload?: Record<string, unknown>;
}

/** Request body for completing a run. */
export interface CompleteRunRequest {
  status: string;
  metadata?: Record<string, unknown>;
}

/** Request body for creating an agent (admin-only). */
export interface CreateAgentRequest {
  agentId: string;
  name: string;
  role: string;
  apiKey: string;
  metadata?: Record<string, unknown>;
}

/** Request body for creating an access grant. */
export interface CreateGrantRequest {
  granteeAgentId: string;
  resourceType: string;
  resourceId?: string;
  permission: string;
  expiresAt?: string;
}

// --- Response types ---

export interface TraceResponse {
  run_id: string;
  decision_id: string;
  event_count: number;
}

export interface CheckResponse {
  has_precedent: boolean;
  decisions: Decision[];
  conflicts?: DecisionConflict[];
}

export interface QueryResponse {
  decisions: Decision[];
  total: number;
  has_more: boolean;
  limit: number;
  offset: number;
}

export interface SearchResult {
  decision: Decision;
  similarity_score: number;
}

export interface SearchResponse {
  results: SearchResult[];
  total: number;
  has_more: boolean;
  limit: number;
  offset: number;
}

/** Result type that can be converted to a trace request. */
export interface Traceable {
  toTrace(): TraceRequest;
}

/** Configuration for the Akashi client. */
export interface AkashiConfig {
  baseUrl: string;
  agentId: string;
  apiKey: string;
  /** Request timeout in milliseconds. Default: 30_000. */
  timeoutMs?: number;
  /** Override the auto-generated session UUID. If omitted, one is generated. */
  sessionId?: string;
}
