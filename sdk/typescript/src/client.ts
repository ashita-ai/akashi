import { TokenManager } from "./auth.js";
import {
  AuthenticationError,
  AuthorizationError,
  ConflictError,
  AkashiError,
  NotFoundError,
  RateLimitError,
  ServerError,
  ValidationError,
} from "./errors.js";
import type {
  AdjudicateConflictRequest,
  Agent,
  AgentRun,
  AgentStatsResponse,
  APIKeyInfo,
  APIKeyWithRawKey,
  AssessRequest,
  AssessResponse,
  CheckResponse,
  CompleteRunRequest,
  ConfigResponse,
  ConflictAnalyticsResponse,
  ConflictDetail,
  ConflictGroup,
  ConflictStatusUpdate,
  CreateAgentRequest,
  CreateGrantRequest,
  CreateHoldRequest,
  CreateKeyRequest,
  CreateProjectLinkRequest,
  CreateRunRequest,
  Decision,
  DecisionConflict,
  EraseDecisionResponse,
  EventInput,
  FacetsResponse,
  GetRunResponse,
  Grant,
  HealthResponse,
  IntegrityViolationsResponse,
  AkashiConfig,
  LineageResponse,
  OrgSettings,
  ProjectLink,
  PurgeRequest,
  PurgeResponse,
  QueryFilters,
  QueryResponse,
  ResolveConflictGroupRequest,
  ResolveConflictGroupResponse,
  RetentionHold,
  RetentionPolicy,
  RevisionsResponse,
  RotateKeyResponse,
  ScopedTokenRequest,
  ScopedTokenResponse,
  SearchResponse,
  SearchResult,
  SessionViewResponse,
  SetOrgSettingsRequest,
  SetRetentionRequest,
  SignupRequest,
  SignupResponse,
  TimelineResponse,
  TraceHealthResponse,
  TraceRequest,
  TraceResponse,
  UpdateAgentRequest,
  UsageResponse,
  VerifyResponse,
} from "./types.js";
import {
  DEFAULT_MAX_RETRIES,
  DEFAULT_RETRY_BASE_DELAY_MS,
  isRetryableStatus,
  retryDelayMs,
  parseRetryAfter,
  checkResponseSize,
  sleep,
} from "./retry.js";

const USER_AGENT = "akashi-typescript/0.2.0";

// ---------------------------------------------------------------------------
// Shared body builders — single source of truth for request shapes.
// ---------------------------------------------------------------------------

function buildCheckBody(
  decisionType: string,
  query: string | undefined,
  agentId: string | undefined,
  limit: number,
): Record<string, unknown> {
  const body: Record<string, unknown> = {
    decision_type: decisionType,
    limit,
  };
  if (query !== undefined) body.query = query;
  if (agentId !== undefined) body.agent_id = agentId;
  return body;
}

/** Cache for the git-inferred project name (undefined = not yet computed). */
let inferredProjectCache: string | undefined;

/**
 * @internal Reset the cached project name. Exported for testing only —
 * setting the cache to "" prevents inferProjectFromGit from running git.
 */
export function _resetProjectCache(value: string = ""): void {
  inferredProjectCache = value;
}

/**
 * Resolve the canonical project name from git remote origin.
 * Runs `git remote get-url origin`, strips `.git`, and returns the basename.
 * Cached for the process lifetime. Returns "" on any failure.
 */
function inferProjectFromGit(): string {
  if (inferredProjectCache !== undefined) return inferredProjectCache;
  try {
    // eslint-disable-next-line @typescript-eslint/no-require-imports
    const { execSync } = require("node:child_process");
    const remote = (
      execSync("git remote get-url origin", {
        timeout: 2000,
        encoding: "utf-8",
        stdio: ["ignore", "pipe", "ignore"],
      }) as string
    ).trim();
    if (!remote) {
      inferredProjectCache = "";
      return "";
    }
    const name = remote.replace(/\.git$/, "").split("/").pop() ?? "";
    inferredProjectCache = name;
    return name;
  } catch {
    inferredProjectCache = "";
    return "";
  }
}

function buildTraceBody(
  agentId: string,
  request: TraceRequest,
): Record<string, unknown> {
  const decision: Record<string, unknown> = {
    decision_type: request.decisionType.trim().toLowerCase(),
    outcome: request.outcome,
    confidence: request.confidence,
  };
  if (request.reasoning !== undefined) decision.reasoning = request.reasoning;
  if (request.alternatives !== undefined)
    decision.alternatives = request.alternatives;
  if (request.evidence !== undefined) decision.evidence = request.evidence;

  // Auto-detect project from git remote when not explicitly set.
  // This prevents workspace/directory names from leaking as project names.
  const ctx: Record<string, unknown> = { ...(request.context ?? {}) };
  if (!ctx.project) {
    const detected = inferProjectFromGit();
    if (detected) ctx.project = detected;
  }

  const body: Record<string, unknown> = { agent_id: agentId, decision };
  if (request.precedentRef !== undefined)
    body.precedent_ref = request.precedentRef;
  if (request.precedentReason !== undefined)
    body.precedent_reason = request.precedentReason;
  if (request.metadata !== undefined) body.metadata = request.metadata;
  if (Object.keys(ctx).length > 0) body.context = ctx;
  return body;
}

function buildQueryBody(
  filters: QueryFilters | undefined,
  limit: number,
  offset: number,
  orderBy: string,
  orderDir: string,
): Record<string, unknown> {
  return {
    filters: filters ?? {},
    limit,
    offset,
    order_by: orderBy,
    order_dir: orderDir,
  };
}

function buildSearchBody(
  query: string,
  limit: number,
  semantic: boolean,
): Record<string, unknown> {
  return { query, limit, semantic };
}

function buildRecentParams(
  limit: number,
  agentId: string | undefined,
  decisionType: string | undefined,
): URLSearchParams {
  const params = new URLSearchParams();
  params.set("limit", String(limit));
  if (agentId) params.set("agent_id", agentId);
  if (decisionType) params.set("decision_type", decisionType);
  return params;
}

function buildCreateRunBody(
  agentId: string,
  req?: CreateRunRequest,
): Record<string, unknown> {
  const body: Record<string, unknown> = { agent_id: agentId };
  if (req?.traceId !== undefined) body.trace_id = req.traceId;
  if (req?.parentRunId !== undefined) body.parent_run_id = req.parentRunId;
  if (req?.metadata !== undefined) body.metadata = req.metadata;
  return body;
}

function buildAppendEventsBody(
  events: EventInput[],
): Record<string, unknown> {
  return {
    events: events.map((e) => {
      const ev: Record<string, unknown> = { event_type: e.eventType };
      if (e.occurredAt !== undefined) ev.occurred_at = e.occurredAt;
      if (e.payload !== undefined) ev.payload = e.payload;
      return ev;
    }),
  };
}

function buildCompleteRunBody(
  req: CompleteRunRequest,
): Record<string, unknown> {
  const body: Record<string, unknown> = { status: req.status };
  if (req.metadata !== undefined) body.metadata = req.metadata;
  return body;
}

function buildTemporalQueryBody(
  asOf: string | Date,
  filters?: QueryFilters,
): Record<string, unknown> {
  const body: Record<string, unknown> = {
    as_of: asOf instanceof Date ? asOf.toISOString() : asOf,
  };
  if (filters !== undefined) body.filters = filters;
  return body;
}

function buildCreateAgentBody(
  req: CreateAgentRequest,
): Record<string, unknown> {
  const body: Record<string, unknown> = {
    agent_id: req.agentId,
    name: req.name,
    role: req.role,
    api_key: req.apiKey,
  };
  if (req.metadata !== undefined) body.metadata = req.metadata;
  return body;
}

function buildCreateGrantBody(
  req: CreateGrantRequest,
): Record<string, unknown> {
  const body: Record<string, unknown> = {
    grantee_agent_id: req.granteeAgentId,
    resource_type: req.resourceType,
    permission: req.permission,
  };
  if (req.resourceId !== undefined) body.resource_id = req.resourceId;
  if (req.expiresAt !== undefined) body.expires_at = req.expiresAt;
  return body;
}

// ---------------------------------------------------------------------------
// Shared response handling
// ---------------------------------------------------------------------------

interface ApiErrorBody {
  error?: { message?: string };
}

interface ApiEnvelope<T> {
  data?: T;
}

interface ListEnvelope<T> {
  items: T[];
  total: number;
  has_more: boolean;
  limit: number;
  offset: number;
}

interface RawListEnvelope {
  data?: unknown;
  total?: number;
  has_more?: boolean;
  limit?: number;
  offset?: number;
}

async function extractErrorMessage(
  resp: Response,
  fallback: string,
): Promise<string> {
  try {
    const body = (await resp.json()) as ApiErrorBody;
    return body.error?.message ?? fallback;
  } catch {
    return fallback;
  }
}

async function handleResponse<T>(resp: Response): Promise<T> {
  if (resp.status === 400) {
    throw new ValidationError(await extractErrorMessage(resp, "Bad request"));
  }
  if (resp.status === 401) {
    throw new AuthenticationError(
      await extractErrorMessage(resp, "Authentication failed"),
    );
  }
  if (resp.status === 403) {
    throw new AuthorizationError(
      await extractErrorMessage(resp, "Insufficient permissions"),
    );
  }
  if (resp.status === 404) {
    throw new NotFoundError(
      await extractErrorMessage(resp, "Resource not found"),
    );
  }
  if (resp.status === 409) {
    throw new ConflictError(await extractErrorMessage(resp, "Conflict"));
  }
  if (resp.status === 429) {
    throw new RateLimitError(
      await extractErrorMessage(resp, "Rate limit exceeded"),
    );
  }
  if (resp.status >= 500) {
    throw new ServerError(
      resp.status,
      await extractErrorMessage(resp, "Server error"),
    );
  }
  if (resp.status >= 400) {
    throw new AkashiError(
      await extractErrorMessage(resp, `Unexpected: ${resp.status}`),
      resp.status,
    );
  }

  const body = (await resp.json()) as ApiEnvelope<T>;
  // The server wraps all responses in {data: ...}. If the envelope is
  // present, unwrap it; otherwise return the body as-is. The cast is
  // unavoidable at the boundary — callers get the type they asked for,
  // and Pydantic-style runtime validation isn't idiomatic in TypeScript.
  if (body.data !== undefined) {
    return body.data;
  }
  return body as unknown as T;
}

async function handleListResponse<T>(resp: Response): Promise<ListEnvelope<T>> {
  // Error handling is identical to handleResponse.
  if (resp.status === 400) {
    throw new ValidationError(await extractErrorMessage(resp, "Bad request"));
  }
  if (resp.status === 401) {
    throw new AuthenticationError(
      await extractErrorMessage(resp, "Authentication failed"),
    );
  }
  if (resp.status === 403) {
    throw new AuthorizationError(
      await extractErrorMessage(resp, "Insufficient permissions"),
    );
  }
  if (resp.status === 404) {
    throw new NotFoundError(
      await extractErrorMessage(resp, "Resource not found"),
    );
  }
  if (resp.status === 409) {
    throw new ConflictError(await extractErrorMessage(resp, "Conflict"));
  }
  if (resp.status === 429) {
    throw new RateLimitError(
      await extractErrorMessage(resp, "Rate limit exceeded"),
    );
  }
  if (resp.status >= 500) {
    throw new ServerError(
      resp.status,
      await extractErrorMessage(resp, "Server error"),
    );
  }
  if (resp.status >= 400) {
    throw new AkashiError(
      await extractErrorMessage(resp, `Unexpected: ${resp.status}`),
      resp.status,
    );
  }

  const body = (await resp.json()) as RawListEnvelope;
  const items = Array.isArray(body.data) ? (body.data as T[]) : [];
  return {
    items,
    total: body.total ?? items.length,
    has_more: body.has_more ?? false,
    limit: body.limit ?? items.length,
    offset: body.offset ?? 0,
  };
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

/**
 * HTTP client for the Akashi decision-tracing API.
 *
 * Uses native `fetch` with zero runtime dependencies.
 *
 * @example
 * ```ts
 * const client = new AkashiClient({
 *   baseUrl: "http://localhost:8080",
 *   agentId: "my-agent",
 *   apiKey: "secret",
 * });
 *
 * const precedents = await client.check("architecture");
 * if (!precedents.has_precedent) {
 *   await client.trace({
 *     decisionType: "architecture",
 *     outcome: "chose event sourcing",
 *     confidence: 0.8,
 *     reasoning: "Auditability requirement",
 *   });
 * }
 * ```
 */
export class AkashiClient {
  private readonly baseUrl: string;
  private readonly agentId: string;
  private readonly sessionId: string;
  private readonly timeoutMs: number;
  private readonly tokenManager: TokenManager;
  private readonly maxRetries: number;
  private readonly retryBaseDelayMs: number;

  constructor(config: AkashiConfig) {
    this.baseUrl = config.baseUrl.replace(/\/+$/, "");
    this.agentId = config.agentId;
    this.sessionId = config.sessionId ?? crypto.randomUUID();
    this.timeoutMs = config.timeoutMs ?? 30_000;
    this.tokenManager = new TokenManager(
      this.baseUrl,
      config.agentId,
      config.apiKey,
      this.timeoutMs,
    );
    this.maxRetries = config.maxRetries ?? DEFAULT_MAX_RETRIES;
    this.retryBaseDelayMs = config.retryBaseDelayMs ?? DEFAULT_RETRY_BASE_DELAY_MS;
  }

  /** Check for existing decisions before making a new one. */
  async check(
    decisionType: string,
    query?: string,
    options?: { agentId?: string; limit?: number },
  ): Promise<CheckResponse> {
    return this.post<CheckResponse>(
      "/v1/check",
      buildCheckBody(decisionType, query, options?.agentId, options?.limit ?? 5),
    );
  }

  /** Record a decision trace. */
  async trace(request: TraceRequest): Promise<TraceResponse> {
    const idempotencyKey = request.idempotencyKey ?? crypto.randomUUID();
    return this.post<TraceResponse>(
      "/v1/trace",
      buildTraceBody(this.agentId, request),
      { "X-Idempotency-Key": idempotencyKey },
    );
  }

  /** Query past decisions with structured filters. */
  async query(
    filters?: QueryFilters,
    options?: {
      limit?: number;
      offset?: number;
      orderBy?: string;
      orderDir?: string;
    },
  ): Promise<QueryResponse> {
    const limit = options?.limit ?? 50;
    const offset = options?.offset ?? 0;
    const envelope = await this.postList<Decision>(
      "/v1/query",
      buildQueryBody(
        filters,
        limit,
        offset,
        options?.orderBy ?? "valid_from",
        options?.orderDir ?? "desc",
      ),
    );
    return {
      decisions: envelope.items,
      total: envelope.total,
      has_more: envelope.has_more,
      limit: envelope.limit,
      offset: envelope.offset,
    };
  }

  /** Search decision history by semantic similarity. */
  async search(
    query: string,
    limit?: number,
    semantic = false,
  ): Promise<SearchResponse> {
    const envelope = await this.postList<SearchResult>(
      "/v1/search",
      buildSearchBody(query, limit ?? 5, semantic),
    );
    return {
      results: envelope.items,
      total: envelope.total,
      has_more: envelope.has_more,
      limit: envelope.limit,
      offset: envelope.offset,
    };
  }

  /** Get the most recent decisions. */
  async recent(options?: {
    limit?: number;
    agentId?: string;
    decisionType?: string;
  }): Promise<Decision[]> {
    const params = buildRecentParams(
      options?.limit ?? 10,
      options?.agentId,
      options?.decisionType,
    );
    const envelope = await this.getList<Decision>(
      `/v1/decisions/recent?${params.toString()}`,
    );
    return envelope.items;
  }

  // --- Run lifecycle ---

  /** Create a new agent run. */
  async createRun(req?: CreateRunRequest): Promise<AgentRun> {
    return this.post<AgentRun>(
      "/v1/runs",
      buildCreateRunBody(this.agentId, req),
    );
  }

  /** Append events to an existing run. */
  async appendEvents(
    runId: string,
    events: EventInput[],
    idempotencyKey?: string,
  ): Promise<void> {
    const key = idempotencyKey ?? crypto.randomUUID();
    await this.post<unknown>(
      `/v1/runs/${encodeURIComponent(runId)}/events`,
      buildAppendEventsBody(events),
      { "X-Idempotency-Key": key },
    );
  }

  /** Mark a run as complete. */
  async completeRun(
    runId: string,
    req: CompleteRunRequest,
    idempotencyKey?: string,
  ): Promise<AgentRun> {
    const key = idempotencyKey ?? crypto.randomUUID();
    return this.post<AgentRun>(
      `/v1/runs/${encodeURIComponent(runId)}/complete`,
      buildCompleteRunBody(req),
      { "X-Idempotency-Key": key },
    );
  }

  /** Get a run by ID, including its events and decisions. */
  async getRun(runId: string): Promise<GetRunResponse> {
    return this.get<GetRunResponse>(`/v1/runs/${encodeURIComponent(runId)}`);
  }

  // --- Agent management (admin-only) ---

  /** Create a new agent. Requires admin or higher role. */
  async createAgent(req: CreateAgentRequest): Promise<Agent> {
    return this.post<Agent>("/v1/agents", buildCreateAgentBody(req));
  }

  /** List all agents in the org. Requires admin or higher role. */
  async listAgents(): Promise<Agent[]> {
    return this.get<Agent[]>("/v1/agents");
  }

  /** Delete an agent by agent_id. Requires admin or higher role. */
  async deleteAgent(agentId: string): Promise<void> {
    await this.del(`/v1/agents/${encodeURIComponent(agentId)}`);
  }

  /** Replace an agent's tags. Requires admin or higher role. */
  async updateAgentTags(agentId: string, tags: string[]): Promise<Agent> {
    return this.patch<Agent>(
      `/v1/agents/${encodeURIComponent(agentId)}/tags`,
      { tags },
    );
  }

  // --- Integrity ---

  /** Verify the integrity of a decision by recomputing its content hash. */
  async verifyDecision(decisionId: string): Promise<VerifyResponse> {
    return this.get<VerifyResponse>(
      `/v1/verify/${encodeURIComponent(decisionId)}`,
    );
  }

  /** Get the full revision chain for a decision. */
  async getDecisionRevisions(
    decisionId: string,
  ): Promise<RevisionsResponse> {
    return this.get<RevisionsResponse>(
      `/v1/decisions/${encodeURIComponent(decisionId)}/revisions`,
    );
  }

  // --- Temporal query ---

  /** Query decisions as of a specific point in time. */
  async temporalQuery(
    asOf: string | Date,
    filters?: QueryFilters,
  ): Promise<Decision[]> {
    return this.post<Decision[]>(
      "/v1/query/temporal",
      buildTemporalQueryBody(asOf, filters),
    );
  }

  // --- Agent history ---

  /** Get decision history for a specific agent. */
  async agentHistory(agentId: string, limit?: number): Promise<Decision[]> {
    const params = new URLSearchParams();
    if (limit !== undefined) params.set("limit", String(limit));
    const qs = params.toString();
    const path = `/v1/agents/${encodeURIComponent(agentId)}/history${qs ? `?${qs}` : ""}`;
    return this.get<Decision[]>(path);
  }

  // --- Grants ---

  /** Create an access grant. */
  async createGrant(req: CreateGrantRequest): Promise<Grant> {
    return this.post<Grant>("/v1/grants", buildCreateGrantBody(req));
  }

  /** Delete an access grant by ID. */
  async deleteGrant(grantId: string): Promise<void> {
    await this.del(`/v1/grants/${encodeURIComponent(grantId)}`);
  }

  // --- Conflicts ---

  /** List detected decision conflicts. */
  async listConflicts(options?: {
    decisionType?: string;
    agentId?: string;
    conflictKind?: "cross_agent" | "self_contradiction";
    limit?: number;
    offset?: number;
  }): Promise<DecisionConflict[]> {
    const params = new URLSearchParams();
    if (options?.decisionType)
      params.set("decision_type", options.decisionType);
    if (options?.agentId)
      params.set("agent_id", options.agentId);
    if (options?.conflictKind)
      params.set("conflict_kind", options.conflictKind);
    if (options?.limit !== undefined)
      params.set("limit", String(options.limit));
    if (options?.offset !== undefined)
      params.set("offset", String(options.offset));
    const qs = params.toString();
    return this.get<DecisionConflict[]>(`/v1/conflicts${qs ? `?${qs}` : ""}`);
  }

  // --- Assessments (spec 29) ---

  /**
   * Record an outcome assessment for a prior decision.
   *
   * Assessments are append-only — each call creates a new row. An assessor
   * changing their verdict over time is itself an auditable event; prior
   * assessments are never overwritten.
   */
  async assess(
    decisionId: string,
    req: AssessRequest,
  ): Promise<AssessResponse> {
    const body: Record<string, unknown> = { outcome: req.outcome };
    if (req.notes !== undefined) body.notes = req.notes;
    return this.post<AssessResponse>(
      `/v1/decisions/${encodeURIComponent(decisionId)}/assess`,
      body,
    );
  }

  /** Return the full assessment history for a decision, newest first. */
  async listAssessments(decisionId: string): Promise<AssessResponse[]> {
    const envelope = await this.getList<AssessResponse>(
      `/v1/decisions/${encodeURIComponent(decisionId)}/assessments`,
    );
    return envelope.items;
  }

  // --- Phase 2: Decision details ---

  /** Get a single decision by ID. */
  async getDecision(decisionId: string): Promise<Decision> {
    return this.get<Decision>(`/v1/decisions/${encodeURIComponent(decisionId)}`);
  }

  /** List conflicts for a specific decision. */
  async getDecisionConflicts(
    decisionId: string,
    options?: { status?: string; limit?: number; offset?: number },
  ): Promise<DecisionConflict[]> {
    const params = new URLSearchParams();
    if (options?.status) params.set("status", options.status);
    if (options?.limit !== undefined) params.set("limit", String(options.limit));
    if (options?.offset !== undefined) params.set("offset", String(options.offset));
    const qs = params.toString();
    const envelope = await this.getList<DecisionConflict>(
      `/v1/decisions/${encodeURIComponent(decisionId)}/conflicts${qs ? `?${qs}` : ""}`,
    );
    return envelope.items;
  }

  /** Get the precedent lineage for a decision. */
  async getDecisionLineage(decisionId: string): Promise<LineageResponse> {
    return this.get<LineageResponse>(`/v1/decisions/${encodeURIComponent(decisionId)}/lineage`);
  }

  /** Get an aggregated timeline of decisions. */
  async getDecisionTimeline(options?: {
    granularity?: string;
    from?: string;
    to?: string;
    agentId?: string;
    project?: string;
  }): Promise<TimelineResponse> {
    const params = new URLSearchParams();
    if (options?.granularity) params.set("granularity", options.granularity);
    if (options?.from) params.set("from", options.from);
    if (options?.to) params.set("to", options.to);
    if (options?.agentId) params.set("agent_id", options.agentId);
    if (options?.project) params.set("project", options.project);
    const qs = params.toString();
    return this.get<TimelineResponse>(`/v1/decisions/timeline${qs ? `?${qs}` : ""}`);
  }

  /** Get available decision types and projects for filtering. */
  async getDecisionFacets(): Promise<FacetsResponse> {
    return this.get<FacetsResponse>("/v1/decisions/facets");
  }

  /** Retract (soft-delete) a decision. */
  async retractDecision(decisionId: string, reason?: string): Promise<Decision> {
    return this.del_with_body<Decision>(
      `/v1/decisions/${encodeURIComponent(decisionId)}`,
      reason ? { reason } : {},
    );
  }

  /** GDPR-erase a decision, replacing content with hashed placeholders. */
  async eraseDecision(decisionId: string, reason?: string): Promise<EraseDecisionResponse> {
    return this.post<EraseDecisionResponse>(
      `/v1/decisions/${encodeURIComponent(decisionId)}/erase`,
      reason ? { reason } : {},
    );
  }

  // --- Phase 2: Conflict management ---

  /** Get a single conflict with optional recommendation. */
  async getConflict(conflictId: string): Promise<ConflictDetail> {
    return this.get<ConflictDetail>(`/v1/conflicts/${encodeURIComponent(conflictId)}`);
  }

  /** Adjudicate a conflict by choosing a winner. */
  async adjudicateConflict(conflictId: string, req: AdjudicateConflictRequest): Promise<ConflictDetail> {
    return this.post<ConflictDetail>(
      `/v1/conflicts/${encodeURIComponent(conflictId)}/adjudicate`,
      req,
    );
  }

  /** Update a conflict's status (resolve, mark false positive, etc.). */
  async patchConflict(conflictId: string, req: ConflictStatusUpdate): Promise<DecisionConflict> {
    return this.patch<DecisionConflict>(
      `/v1/conflicts/${encodeURIComponent(conflictId)}`,
      req,
    );
  }

  /** List conflict groups (clusters of related conflicts). */
  async listConflictGroups(options?: {
    decisionType?: string;
    agentId?: string;
    conflictKind?: string;
    status?: string;
    limit?: number;
    offset?: number;
  }): Promise<ConflictGroup[]> {
    const params = new URLSearchParams();
    if (options?.decisionType) params.set("decision_type", options.decisionType);
    if (options?.agentId) params.set("agent_id", options.agentId);
    if (options?.conflictKind) params.set("conflict_kind", options.conflictKind);
    if (options?.status) params.set("status", options.status);
    if (options?.limit !== undefined) params.set("limit", String(options.limit));
    if (options?.offset !== undefined) params.set("offset", String(options.offset));
    const qs = params.toString();
    const envelope = await this.getList<ConflictGroup>(`/v1/conflict-groups${qs ? `?${qs}` : ""}`);
    return envelope.items;
  }

  /** Resolve all open conflicts in a group. */
  async resolveConflictGroup(groupId: string, req: ResolveConflictGroupRequest): Promise<ResolveConflictGroupResponse> {
    return this.patch<ResolveConflictGroupResponse>(
      `/v1/conflict-groups/${encodeURIComponent(groupId)}/resolve`,
      req,
    );
  }

  /** Get conflict analytics (summary, trends, breakdowns). */
  async getConflictAnalytics(options?: {
    period?: string;
    from?: string;
    to?: string;
    agentId?: string;
    decisionType?: string;
    conflictKind?: string;
  }): Promise<ConflictAnalyticsResponse> {
    const params = new URLSearchParams();
    if (options?.period) params.set("period", options.period);
    if (options?.from) params.set("from", options.from);
    if (options?.to) params.set("to", options.to);
    if (options?.agentId) params.set("agent_id", options.agentId);
    if (options?.decisionType) params.set("decision_type", options.decisionType);
    if (options?.conflictKind) params.set("conflict_kind", options.conflictKind);
    const qs = params.toString();
    return this.get<ConflictAnalyticsResponse>(`/v1/conflicts/analytics${qs ? `?${qs}` : ""}`);
  }

  // --- Phase 3: API keys ---

  /** Create a new API key. */
  async createKey(req: CreateKeyRequest): Promise<APIKeyWithRawKey> {
    return this.post<APIKeyWithRawKey>("/v1/keys", req);
  }

  /** List API keys for the org. */
  async listKeys(options?: { limit?: number; offset?: number }): Promise<APIKeyInfo[]> {
    const params = new URLSearchParams();
    if (options?.limit !== undefined) params.set("limit", String(options.limit));
    if (options?.offset !== undefined) params.set("offset", String(options.offset));
    const qs = params.toString();
    const envelope = await this.getList<APIKeyInfo>(`/v1/keys${qs ? `?${qs}` : ""}`);
    return envelope.items;
  }

  /** Revoke an API key. */
  async revokeKey(keyId: string): Promise<void> {
    await this.del(`/v1/keys/${encodeURIComponent(keyId)}`);
  }

  /** Rotate an API key (revoke old, create new). */
  async rotateKey(keyId: string): Promise<RotateKeyResponse> {
    return this.post<RotateKeyResponse>(`/v1/keys/${encodeURIComponent(keyId)}/rotate`, {});
  }

  // --- Phase 3: Org settings ---

  /** Get org-level settings. */
  async getOrgSettings(): Promise<OrgSettings> {
    return this.get<OrgSettings>("/v1/org/settings");
  }

  /** Update org-level settings. */
  async setOrgSettings(req: SetOrgSettingsRequest): Promise<OrgSettings> {
    return this.put<OrgSettings>("/v1/org/settings", req);
  }

  // --- Phase 3: Retention ---

  /** Get the retention policy for the org. */
  async getRetention(): Promise<RetentionPolicy> {
    return this.get<RetentionPolicy>("/v1/retention");
  }

  /** Set the retention policy for the org. */
  async setRetention(req: SetRetentionRequest): Promise<RetentionPolicy> {
    return this.put<RetentionPolicy>("/v1/retention", req);
  }

  /** Purge old decisions (optionally dry-run). */
  async purgeDecisions(req: PurgeRequest): Promise<PurgeResponse> {
    return this.post<PurgeResponse>("/v1/retention/purge", req);
  }

  /** Create a retention hold to prevent purging. */
  async createHold(req: CreateHoldRequest): Promise<RetentionHold> {
    return this.post<RetentionHold>("/v1/retention/hold", req);
  }

  /** Release a retention hold. */
  async releaseHold(holdId: string): Promise<void> {
    await this.del(`/v1/retention/hold/${encodeURIComponent(holdId)}`);
  }

  // --- Phase 3: Project links ---

  /** Create a link between two projects. */
  async createProjectLink(req: CreateProjectLinkRequest): Promise<ProjectLink> {
    return this.post<ProjectLink>("/v1/project-links", req);
  }

  /** List project links. */
  async listProjectLinks(options?: { limit?: number; offset?: number }): Promise<ProjectLink[]> {
    const params = new URLSearchParams();
    if (options?.limit !== undefined) params.set("limit", String(options.limit));
    if (options?.offset !== undefined) params.set("offset", String(options.offset));
    const qs = params.toString();
    const envelope = await this.getList<ProjectLink>(`/v1/project-links${qs ? `?${qs}` : ""}`);
    return envelope.items;
  }

  /** Delete a project link. */
  async deleteProjectLink(linkId: string): Promise<void> {
    await this.del(`/v1/project-links/${encodeURIComponent(linkId)}`);
  }

  /** Auto-create links between all known projects. */
  async grantAllProjectLinks(linkType?: string): Promise<{ links_created: number }> {
    return this.post<{ links_created: number }>(
      "/v1/project-links/grant-all",
      linkType ? { link_type: linkType } : {},
    );
  }

  // --- Phase 3: Integrity, trace health, usage ---

  /** List integrity violations (tampered decision hashes). */
  async listIntegrityViolations(limit?: number): Promise<IntegrityViolationsResponse> {
    const params = new URLSearchParams();
    if (limit !== undefined) params.set("limit", String(limit));
    const qs = params.toString();
    return this.get<IntegrityViolationsResponse>(`/v1/integrity/violations${qs ? `?${qs}` : ""}`);
  }

  /** Get trace health metrics (completeness, compliance). */
  async getTraceHealth(options?: { from?: string; to?: string }): Promise<TraceHealthResponse> {
    const params = new URLSearchParams();
    if (options?.from) params.set("from", options.from);
    if (options?.to) params.set("to", options.to);
    const qs = params.toString();
    return this.get<TraceHealthResponse>(`/v1/trace-health${qs ? `?${qs}` : ""}`);
  }

  /** Get API usage statistics. */
  async getUsage(period?: string): Promise<UsageResponse> {
    const params = new URLSearchParams();
    if (period) params.set("period", period);
    const qs = params.toString();
    return this.get<UsageResponse>(`/v1/usage${qs ? `?${qs}` : ""}`);
  }

  // --- Phase 3: Auth ---

  /** Create a scoped token to act as another agent. */
  async scopedToken(req: ScopedTokenRequest): Promise<ScopedTokenResponse> {
    return this.post<ScopedTokenResponse>("/v1/auth/scoped-token", req);
  }

  /** Sign up a new org (no auth required). */
  async signup(req: SignupRequest): Promise<SignupResponse> {
    return this.postNoAuth<SignupResponse>("/auth/signup", req);
  }

  /** Get public server config (no auth required). */
  async getConfig(): Promise<ConfigResponse> {
    return this.getNoAuth<ConfigResponse>("/config");
  }

  // --- Phase 4: Agent management ---

  /** Get a single agent by agent_id. */
  async getAgent(agentId: string): Promise<Agent> {
    return this.get<Agent>(`/v1/agents/${encodeURIComponent(agentId)}`);
  }

  /** Update an agent's name or metadata. */
  async updateAgent(agentId: string, req: UpdateAgentRequest): Promise<Agent> {
    return this.patch<Agent>(`/v1/agents/${encodeURIComponent(agentId)}`, req);
  }

  /** Get aggregate statistics for an agent. */
  async getAgentStats(agentId: string): Promise<AgentStatsResponse> {
    return this.get<AgentStatsResponse>(`/v1/agents/${encodeURIComponent(agentId)}/stats`);
  }

  // --- Phase 4: Grants ---

  /** List access grants with pagination. */
  async listGrants(options?: { limit?: number; offset?: number }): Promise<Grant[]> {
    const params = new URLSearchParams();
    if (options?.limit !== undefined) params.set("limit", String(options.limit));
    if (options?.offset !== undefined) params.set("offset", String(options.offset));
    const qs = params.toString();
    const envelope = await this.getList<Grant>(`/v1/grants${qs ? `?${qs}` : ""}`);
    return envelope.items;
  }

  // --- Phase 4: Sessions ---

  /** Get a session view with decisions and summary. */
  async getSessionView(sessionId: string): Promise<SessionViewResponse> {
    return this.get<SessionViewResponse>(`/v1/sessions/${encodeURIComponent(sessionId)}`);
  }

  // --- Health (no auth) ---

  /** Check server health. Does not require authentication. */
  async health(): Promise<HealthResponse> {
    return this.getNoAuth<HealthResponse>("/health");
  }

  // --- HTTP transport ---

  private async post<T>(
    path: string,
    body: unknown,
    extraHeaders?: Record<string, string>,
  ): Promise<T> {
    let lastError: Error | undefined;
    for (let attempt = 0; attempt <= this.maxRetries; attempt++) {
      const token = await this.tokenManager.getToken();
      const headers: Record<string, string> = {
        "Content-Type": "application/json",
        Authorization: `Bearer ${token}`,
        "User-Agent": USER_AGENT,
        "X-Akashi-Session": this.sessionId,
        ...extraHeaders,
      };
      let resp: Response;
      try {
        resp = await fetch(`${this.baseUrl}${path}`, {
          method: "POST",
          headers,
          body: JSON.stringify(body),
          signal: AbortSignal.timeout(this.timeoutMs),
        });
      } catch (err) {
        lastError = err instanceof Error ? err : new Error(String(err));
        if (attempt < this.maxRetries) {
          await sleep(retryDelayMs(attempt, this.retryBaseDelayMs));
          continue;
        }
        throw lastError;
      }
      if (isRetryableStatus(resp.status) && attempt < this.maxRetries) {
        const ra = parseRetryAfter(resp.headers.get("retry-after"));
        await resp.body?.cancel();
        await sleep(retryDelayMs(attempt, this.retryBaseDelayMs, ra));
        continue;
      }
      checkResponseSize(resp.headers.get("content-length"));
      return handleResponse<T>(resp);
    }
    throw lastError!;
  }

  private async postList<T>(
    path: string,
    body: unknown,
  ): Promise<ListEnvelope<T>> {
    let lastError: Error | undefined;
    for (let attempt = 0; attempt <= this.maxRetries; attempt++) {
      const token = await this.tokenManager.getToken();
      const headers: Record<string, string> = {
        "Content-Type": "application/json",
        Authorization: `Bearer ${token}`,
        "User-Agent": USER_AGENT,
        "X-Akashi-Session": this.sessionId,
      };
      let resp: Response;
      try {
        resp = await fetch(`${this.baseUrl}${path}`, {
          method: "POST",
          headers,
          body: JSON.stringify(body),
          signal: AbortSignal.timeout(this.timeoutMs),
        });
      } catch (err) {
        lastError = err instanceof Error ? err : new Error(String(err));
        if (attempt < this.maxRetries) {
          await sleep(retryDelayMs(attempt, this.retryBaseDelayMs));
          continue;
        }
        throw lastError;
      }
      if (isRetryableStatus(resp.status) && attempt < this.maxRetries) {
        const ra = parseRetryAfter(resp.headers.get("retry-after"));
        await resp.body?.cancel();
        await sleep(retryDelayMs(attempt, this.retryBaseDelayMs, ra));
        continue;
      }
      checkResponseSize(resp.headers.get("content-length"));
      return handleListResponse<T>(resp);
    }
    throw lastError!;
  }

  private async patch<T>(path: string, body: unknown): Promise<T> {
    let lastError: Error | undefined;
    for (let attempt = 0; attempt <= this.maxRetries; attempt++) {
      const token = await this.tokenManager.getToken();
      const headers: Record<string, string> = {
        "Content-Type": "application/json",
        Authorization: `Bearer ${token}`,
        "User-Agent": USER_AGENT,
        "X-Akashi-Session": this.sessionId,
      };
      let resp: Response;
      try {
        resp = await fetch(`${this.baseUrl}${path}`, {
          method: "PATCH",
          headers,
          body: JSON.stringify(body),
          signal: AbortSignal.timeout(this.timeoutMs),
        });
      } catch (err) {
        lastError = err instanceof Error ? err : new Error(String(err));
        if (attempt < this.maxRetries) {
          await sleep(retryDelayMs(attempt, this.retryBaseDelayMs));
          continue;
        }
        throw lastError;
      }
      if (isRetryableStatus(resp.status) && attempt < this.maxRetries) {
        const ra = parseRetryAfter(resp.headers.get("retry-after"));
        await resp.body?.cancel();
        await sleep(retryDelayMs(attempt, this.retryBaseDelayMs, ra));
        continue;
      }
      checkResponseSize(resp.headers.get("content-length"));
      return handleResponse<T>(resp);
    }
    throw lastError!;
  }

  private async get<T>(path: string): Promise<T> {
    let lastError: Error | undefined;
    for (let attempt = 0; attempt <= this.maxRetries; attempt++) {
      const token = await this.tokenManager.getToken();
      const headers: Record<string, string> = {
        Authorization: `Bearer ${token}`,
        "User-Agent": USER_AGENT,
        "X-Akashi-Session": this.sessionId,
      };
      let resp: Response;
      try {
        resp = await fetch(`${this.baseUrl}${path}`, {
          method: "GET",
          headers,
          signal: AbortSignal.timeout(this.timeoutMs),
        });
      } catch (err) {
        lastError = err instanceof Error ? err : new Error(String(err));
        if (attempt < this.maxRetries) {
          await sleep(retryDelayMs(attempt, this.retryBaseDelayMs));
          continue;
        }
        throw lastError;
      }
      if (isRetryableStatus(resp.status) && attempt < this.maxRetries) {
        const ra = parseRetryAfter(resp.headers.get("retry-after"));
        await resp.body?.cancel();
        await sleep(retryDelayMs(attempt, this.retryBaseDelayMs, ra));
        continue;
      }
      checkResponseSize(resp.headers.get("content-length"));
      return handleResponse<T>(resp);
    }
    throw lastError!;
  }

  private async getList<T>(path: string): Promise<ListEnvelope<T>> {
    let lastError: Error | undefined;
    for (let attempt = 0; attempt <= this.maxRetries; attempt++) {
      const token = await this.tokenManager.getToken();
      const headers: Record<string, string> = {
        Authorization: `Bearer ${token}`,
        "User-Agent": USER_AGENT,
        "X-Akashi-Session": this.sessionId,
      };
      let resp: Response;
      try {
        resp = await fetch(`${this.baseUrl}${path}`, {
          method: "GET",
          headers,
          signal: AbortSignal.timeout(this.timeoutMs),
        });
      } catch (err) {
        lastError = err instanceof Error ? err : new Error(String(err));
        if (attempt < this.maxRetries) {
          await sleep(retryDelayMs(attempt, this.retryBaseDelayMs));
          continue;
        }
        throw lastError;
      }
      if (isRetryableStatus(resp.status) && attempt < this.maxRetries) {
        const ra = parseRetryAfter(resp.headers.get("retry-after"));
        await resp.body?.cancel();
        await sleep(retryDelayMs(attempt, this.retryBaseDelayMs, ra));
        continue;
      }
      checkResponseSize(resp.headers.get("content-length"));
      return handleListResponse<T>(resp);
    }
    throw lastError!;
  }

  private async del(path: string): Promise<void> {
    let lastError: Error | undefined;
    for (let attempt = 0; attempt <= this.maxRetries; attempt++) {
      const token = await this.tokenManager.getToken();
      const headers: Record<string, string> = {
        Authorization: `Bearer ${token}`,
        "User-Agent": USER_AGENT,
        "X-Akashi-Session": this.sessionId,
      };
      let resp: Response;
      try {
        resp = await fetch(`${this.baseUrl}${path}`, {
          method: "DELETE",
          headers,
          signal: AbortSignal.timeout(this.timeoutMs),
        });
      } catch (err) {
        lastError = err instanceof Error ? err : new Error(String(err));
        if (attempt < this.maxRetries) {
          await sleep(retryDelayMs(attempt, this.retryBaseDelayMs));
          continue;
        }
        throw lastError;
      }
      if (isRetryableStatus(resp.status) && attempt < this.maxRetries) {
        const ra = parseRetryAfter(resp.headers.get("retry-after"));
        await resp.body?.cancel();
        await sleep(retryDelayMs(attempt, this.retryBaseDelayMs, ra));
        continue;
      }
      if (resp.status === 204) return;
      checkResponseSize(resp.headers.get("content-length"));
      await handleResponse<unknown>(resp);
      return;
    }
    throw lastError!;
  }

  private async getNoAuth<T>(path: string): Promise<T> {
    let lastError: Error | undefined;
    for (let attempt = 0; attempt <= this.maxRetries; attempt++) {
      let resp: Response;
      try {
        resp = await fetch(`${this.baseUrl}${path}`, {
          method: "GET",
          headers: { "User-Agent": USER_AGENT },
          signal: AbortSignal.timeout(this.timeoutMs),
        });
      } catch (err) {
        lastError = err instanceof Error ? err : new Error(String(err));
        if (attempt < this.maxRetries) {
          await sleep(retryDelayMs(attempt, this.retryBaseDelayMs));
          continue;
        }
        throw lastError;
      }
      if (isRetryableStatus(resp.status) && attempt < this.maxRetries) {
        const ra = parseRetryAfter(resp.headers.get("retry-after"));
        await resp.body?.cancel();
        await sleep(retryDelayMs(attempt, this.retryBaseDelayMs, ra));
        continue;
      }
      checkResponseSize(resp.headers.get("content-length"));
      return handleResponse<T>(resp);
    }
    throw lastError!;
  }

  private async put<T>(path: string, body: unknown): Promise<T> {
    let lastError: Error | undefined;
    for (let attempt = 0; attempt <= this.maxRetries; attempt++) {
      const token = await this.tokenManager.getToken();
      const headers: Record<string, string> = {
        "Content-Type": "application/json",
        Authorization: `Bearer ${token}`,
        "User-Agent": USER_AGENT,
        "X-Akashi-Session": this.sessionId,
      };
      let resp: Response;
      try {
        resp = await fetch(`${this.baseUrl}${path}`, {
          method: "PUT",
          headers,
          body: JSON.stringify(body),
          signal: AbortSignal.timeout(this.timeoutMs),
        });
      } catch (err) {
        lastError = err instanceof Error ? err : new Error(String(err));
        if (attempt < this.maxRetries) {
          await sleep(retryDelayMs(attempt, this.retryBaseDelayMs));
          continue;
        }
        throw lastError;
      }
      if (isRetryableStatus(resp.status) && attempt < this.maxRetries) {
        const ra = parseRetryAfter(resp.headers.get("retry-after"));
        await resp.body?.cancel();
        await sleep(retryDelayMs(attempt, this.retryBaseDelayMs, ra));
        continue;
      }
      checkResponseSize(resp.headers.get("content-length"));
      return handleResponse<T>(resp);
    }
    throw lastError!;
  }

  private async postNoAuth<T>(path: string, body: unknown): Promise<T> {
    let lastError: Error | undefined;
    for (let attempt = 0; attempt <= this.maxRetries; attempt++) {
      let resp: Response;
      try {
        resp = await fetch(`${this.baseUrl}${path}`, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            "User-Agent": USER_AGENT,
          },
          body: JSON.stringify(body),
          signal: AbortSignal.timeout(this.timeoutMs),
        });
      } catch (err) {
        lastError = err instanceof Error ? err : new Error(String(err));
        if (attempt < this.maxRetries) {
          await sleep(retryDelayMs(attempt, this.retryBaseDelayMs));
          continue;
        }
        throw lastError;
      }
      if (isRetryableStatus(resp.status) && attempt < this.maxRetries) {
        const ra = parseRetryAfter(resp.headers.get("retry-after"));
        await resp.body?.cancel();
        await sleep(retryDelayMs(attempt, this.retryBaseDelayMs, ra));
        continue;
      }
      checkResponseSize(resp.headers.get("content-length"));
      return handleResponse<T>(resp);
    }
    throw lastError!;
  }

  private async del_with_body<T>(path: string, body: unknown): Promise<T> {
    let lastError: Error | undefined;
    for (let attempt = 0; attempt <= this.maxRetries; attempt++) {
      const token = await this.tokenManager.getToken();
      const headers: Record<string, string> = {
        "Content-Type": "application/json",
        Authorization: `Bearer ${token}`,
        "User-Agent": USER_AGENT,
        "X-Akashi-Session": this.sessionId,
      };
      let resp: Response;
      try {
        resp = await fetch(`${this.baseUrl}${path}`, {
          method: "DELETE",
          headers,
          body: JSON.stringify(body),
          signal: AbortSignal.timeout(this.timeoutMs),
        });
      } catch (err) {
        lastError = err instanceof Error ? err : new Error(String(err));
        if (attempt < this.maxRetries) {
          await sleep(retryDelayMs(attempt, this.retryBaseDelayMs));
          continue;
        }
        throw lastError;
      }
      if (isRetryableStatus(resp.status) && attempt < this.maxRetries) {
        const ra = parseRetryAfter(resp.headers.get("retry-after"));
        await resp.body?.cancel();
        await sleep(retryDelayMs(attempt, this.retryBaseDelayMs, ra));
        continue;
      }
      checkResponseSize(resp.headers.get("content-length"));
      return handleResponse<T>(resp);
    }
    throw lastError!;
  }
}
