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
  quality_score: number;
  precedent_ref: string | null;
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
  time_range?: {
    from: string;
    to: string;
  };
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

export interface AgentsList {
  agents: Agent[];
}

export interface SearchResponse {
  results: SearchResult[];
  total: number;
}

// Health
export interface HealthResponse {
  status: string;
  version: string;
  postgres: string;
  uptime_seconds: number;
}
