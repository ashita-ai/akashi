/** A recorded decision with bi-temporal modeling. */
export interface Decision {
  id: string;
  run_id: string;
  agent_id: string;
  decision_type: string;
  outcome: string;
  confidence: number;
  reasoning?: string;
  metadata: Record<string, unknown>;
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

/** A detected conflict between two decisions. */
export interface DecisionConflict {
  decision_a_id: string;
  decision_b_id: string;
  agent_a: string;
  agent_b: string;
  run_a: string;
  run_b: string;
  decision_type: string;
  outcome_a: string;
  outcome_b: string;
  confidence_a: number;
  confidence_b: number;
  decided_at_a: string;
  decided_at_b: string;
  detected_at: string;
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
}

/** Result type that can be converted to a trace request. */
export interface Traceable {
  toTrace(): TraceRequest;
}

/** Configuration for the Kyoyu client. */
export interface KyoyuConfig {
  baseUrl: string;
  agentId: string;
  apiKey: string;
  /** Request timeout in milliseconds. Default: 30_000. */
  timeoutMs?: number;
}
