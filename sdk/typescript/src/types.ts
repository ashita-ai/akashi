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
  outcome_score?: number | null;
  precedent_ref?: string;
  precedent_reason?: string;
  supersedes_id?: string;
  content_hash?: string;
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
  metrics?: Record<string, number>;
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

/** A single event from the SSE subscribe stream. */
export interface SubscriptionEvent {
  /** SSE event name (e.g. "akashi_decisions" or "akashi_conflicts"). */
  eventType: string;
  /** Parsed JSON payload. */
  data: Record<string, unknown>;
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
  precedentRef?: string;
  precedentReason?: string;
  metadata?: Record<string, unknown>;
  context?: Record<string, unknown>;
  /** Optional idempotency key for safe retries. Auto-generated if omitted. */
  idempotencyKey?: string;
}

export interface TraceAlternative {
  label: string;
  rejection_reason?: string;
}

export interface TraceEvidence {
  source_type: string;
  source_uri?: string;
  content: string;
  relevance_score?: number;
  metrics?: Record<string, number>;
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

/** Response from GET /v1/runs/{run_id} — includes run, events, and decisions. */
export interface GetRunResponse {
  run: AgentRun;
  events: AgentEvent[];
  decisions: Decision[];
}

/** Response from GET /v1/verify/{decisionId} — integrity verification. */
export interface VerifyResponse {
  decision_id: string;
  valid: boolean;
  stored_hash: string;
  computed_hash: string;
}

/** Response from GET /v1/decisions/{decisionId}/revisions — revision chain. */
export interface RevisionsResponse {
  decision_id: string;
  revisions: Decision[];
  count: number;
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
  conflicts_unavailable?: boolean;
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
  /** Maximum retries on transient errors (429, 5xx, network). Default: 3. */
  maxRetries?: number;
  /** Base delay for exponential backoff in ms. Default: 500. */
  retryBaseDelayMs?: number;
}

// --- Assessment types (spec 29) ---

export type AssessOutcome = "correct" | "incorrect" | "partially_correct";

/** Request body for recording an outcome assessment. */
export interface AssessRequest {
  /** Must be "correct", "incorrect", or "partially_correct". */
  outcome: AssessOutcome;
  /** Optional free-text explanation. */
  notes?: string;
}

/** Response from POST /v1/decisions/{id}/assess and element of GET /v1/decisions/{id}/assessments. */
export interface AssessResponse {
  id: string;
  decision_id: string;
  org_id: string;
  assessor_agent_id: string;
  outcome: AssessOutcome;
  notes?: string;
  created_at: string;
}

// --- Phase 2: Decision & conflict management types ---

export interface ConflictRecommendation {
  suggested_winner: string;
  reasons: string[];
  confidence: number;
}

export interface ConflictDetail {
  decision_conflict: DecisionConflict;
  recommendation?: ConflictRecommendation;
}

export interface LineageEntry {
  id: string;
  decision_type: string;
  outcome: string;
  confidence: number;
  agent_id: string;
  valid_from: string;
  precedent_reason?: string;
}

export interface LineageResponse {
  decision_id: string;
  precedent_ref?: string;
  precedent?: LineageEntry;
  cites: LineageEntry[];
}

export interface TimelineDecisionSummary {
  id: string;
  agent_id: string;
  decision_type: string;
  outcome: string;
  confidence: number;
  project?: string;
  created_at: string;
}

export interface TimelineBucket {
  bucket: string;
  decision_count: number;
  avg_confidence: number;
  decision_types: Record<string, number>;
  agents: Record<string, number>;
  conflict_count: number;
  top_decisions?: TimelineDecisionSummary[];
}

export interface TimelineResponse {
  granularity: string;
  buckets: TimelineBucket[];
  projects: string[];
}

export interface FacetsResponse {
  types: string[];
  projects: string[];
}

export interface EraseDecisionResponse {
  decision_id: string;
  erased_at: string;
  original_hash: string;
  erased_hash: string;
  alternatives_erased: number;
  evidence_erased: number;
  claims_erased: number;
}

export interface AdjudicateConflictRequest {
  outcome: string;
  reasoning?: string;
  decision_type?: string;
  winning_decision_id?: string;
}

export interface ConflictStatusUpdate {
  status: string;
  resolution_note?: string;
  winning_decision_id?: string;
  false_positive_label?: string;
}

export interface ResolveConflictGroupRequest {
  status: string;
  resolution_note?: string;
  winning_agent?: string;
  false_positive_label?: string;
}

export interface ResolveConflictGroupResponse {
  group_id: string;
  status: string;
  resolved: number;
}

export interface ConflictGroup {
  id: string;
  org_id: string;
  agent_a: string;
  agent_b: string;
  conflict_kind: ConflictKind;
  decision_type: string;
  group_topic?: string;
  first_detected_at: string;
  last_detected_at: string;
  conflict_count: number;
  open_count: number;
  times_reopened: number;
  representative?: DecisionConflict;
  open_conflicts?: DecisionConflict[];
}

export interface ConflictAnalyticsSummary {
  total_conflicts: number;
  open: number;
  resolved: number;
  false_positives: number;
  avg_days_to_resolution: number;
}

export interface ConflictAgentPairStats {
  agent_a: string;
  agent_b: string;
  count: number;
  open: number;
  resolved: number;
  false_positives: number;
}

export interface ConflictTypeStats {
  decision_type: string;
  count: number;
  open: number;
}

export interface ConflictSeverityStats {
  severity: string;
  count: number;
  open: number;
}

export interface ConflictDailyTrend {
  date: string;
  detected: number;
  resolved: number;
}

export interface ConflictAnalyticsResponse {
  period: string;
  from: string;
  to: string;
  summary: ConflictAnalyticsSummary;
  by_agent_pair: ConflictAgentPairStats[];
  by_decision_type: ConflictTypeStats[];
  by_severity: ConflictSeverityStats[];
  daily_trend: ConflictDailyTrend[];
}

// --- Phase 3: Admin & configuration types ---

export interface APIKeyInfo {
  id: string;
  prefix: string;
  agent_id: string;
  org_id?: string;
  label: string;
  created_by?: string;
  created_at: string;
  expires_at?: string;
  revoked_at?: string;
}

export interface APIKeyWithRawKey {
  api_key: APIKeyInfo;
  raw_key: string;
}

export interface CreateKeyRequest {
  agent_id: string;
  label?: string;
  expires_at?: string;
}

export interface RotateKeyResponse {
  new_key: APIKeyWithRawKey;
  revoked_key_id: string;
}

export interface ConflictResolutionSettings {
  auto_resolve_threshold: number;
  enable_cascade_resolution: boolean;
  cascade_similarity_threshold: number;
}

export interface OrgSettings {
  conflict_resolution: ConflictResolutionSettings;
  updated_at: string;
}

export interface SetOrgSettingsRequest {
  conflict_resolution: ConflictResolutionSettings;
}

export interface RetentionHold {
  id: string;
  org_id: string;
  reason: string;
  hold_from: string;
  hold_to: string;
  decision_types?: string[];
  agent_ids?: string[];
  created_by: string;
  created_at: string;
  released_at?: string;
}

export interface RetentionPolicy {
  retention_days: number;
  retention_exclude_types: string[];
  last_run?: string;
  last_run_deleted: number;
  next_run?: string;
  holds: RetentionHold[];
}

export interface SetRetentionRequest {
  retention_days: number;
  retention_exclude_types?: string[];
}

export interface PurgeCounts {
  decisions: number;
  alternatives: number;
  evidence: number;
  claims: number;
  events: number;
}

export interface PurgeRequest {
  before: string;
  decision_type?: string;
  agent_id?: string;
  dry_run: boolean;
}

export interface PurgeResponse {
  dry_run: boolean;
  would_delete: PurgeCounts;
  deleted: PurgeCounts;
}

export interface CreateHoldRequest {
  reason: string;
  from: string;
  to: string;
  decision_types?: string[];
  agent_ids?: string[];
}

export interface ProjectLink {
  id: string;
  org_id: string;
  project_a: string;
  project_b: string;
  link_type: string;
  created_by: string;
  created_at: string;
}

export interface CreateProjectLinkRequest {
  project_a: string;
  project_b: string;
  link_type?: string;
}

export interface IntegrityViolation {
  id: string;
  decision_id: string;
  org_id: string;
  expected_hash: string;
  actual_hash: string;
  detected_at: string;
}

export interface IntegrityViolationsResponse {
  violations: IntegrityViolation[];
  count: number;
}

export interface TraceHealthResponse {
  total_decisions: number;
  total_assessments: number;
  total_conflicts: number;
  avg_completeness: number;
  avg_confidence: number;
  assessment_rate: number;
  conflict_rate: number;
  compliance_score: number;
}

export interface UsageByKey {
  key_id: string;
  prefix: string;
  label: string;
  agent_id: string;
  decisions: number;
}

export interface UsageByAgent {
  agent_id: string;
  decisions: number;
}

export interface UsageResponse {
  org_id: string;
  period: string;
  total_decisions: number;
  by_key: UsageByKey[];
  by_agent: UsageByAgent[];
}

export interface ScopedTokenRequest {
  as_agent_id: string;
  expires_in?: number;
}

export interface ScopedTokenResponse {
  token: string;
  expires_at: string;
  as_agent_id: string;
  scoped_by: string;
}

export interface SignupRequest {
  org_name: string;
  agent_id: string;
  email: string;
}

export interface MCPConfigInfo {
  url: string;
  header: string;
}

export interface SignupResponse {
  org_id: string;
  org_slug: string;
  agent_id: string;
  api_key: string;
  mcp_config?: MCPConfigInfo;
}

export interface ConfigResponse {
  search_enabled: boolean;
}

// --- Phase 4 types ---

export interface UpdateAgentRequest {
  name?: string;
  metadata?: Record<string, unknown>;
}

export interface AgentStatValues {
  decision_count: number;
  last_decision_at?: string;
  avg_confidence: number;
  conflict_rate: number;
}

export interface AgentStatsResponse {
  agent_id: string;
  stats: AgentStatValues;
}

export interface SessionSummary {
  started_at?: string;
  ended_at?: string;
  duration_secs: number;
  decision_types: Record<string, number>;
  avg_confidence: number;
}

export interface SessionViewResponse {
  session_id: string;
  decisions: Decision[];
  decision_count: number;
  summary: SessionSummary;
}

// --- Admin: conflict validation, evaluation, and labels ---

export interface ValidatePairRequest {
  outcome_a: string;
  outcome_b: string;
  type_a?: string;
  type_b?: string;
  agent_a?: string;
  agent_b?: string;
  reasoning_a?: string;
  reasoning_b?: string;
  project_a?: string;
  project_b?: string;
  topic_similarity?: number;
}

export interface ValidatePairResponse {
  relationship: string;
  category: string;
  severity: string;
  explanation: string;
}

export interface ConflictEvalMetrics {
  total_pairs: number;
  errors: number;
  relationship_accuracy: number;
  conflict_precision: number;
  conflict_recall: number;
  conflict_f1: number;
  true_positives: number;
  false_positives: number;
  true_negatives: number;
  false_negatives: number;
  relationship_hits: number;
}

export interface ConflictEvalResult {
  label: string;
  expected_relationship: string;
  actual_relationship: string;
  correct: boolean;
  conflict_expected: boolean;
  conflict_actual: boolean;
  explanation: string;
  error?: string;
}

export interface ConflictEvalResponse {
  metrics: ConflictEvalMetrics;
  results: ConflictEvalResult[];
}

export interface UpsertConflictLabelRequest {
  label: string;
  notes?: string;
}

export interface ConflictLabelRecord {
  scored_conflict_id: string;
  org_id: string;
  label: string;
  labeled_by: string;
  labeled_at: string;
  notes?: string;
}

export interface ConflictLabelCounts {
  genuine: number;
  related_not_contradicting: number;
  unrelated_false_positive: number;
  total: number;
}

export interface ListConflictLabelsResponse {
  labels: ConflictLabelRecord[];
  counts: ConflictLabelCounts;
}

export interface ScorerEvalResponse {
  precision: number;
  true_positives: number;
  false_positives: number;
  total_labeled: number;
  message?: string;
}
