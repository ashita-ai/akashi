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
  Agent,
  AgentRun,
  CheckResponse,
  CompleteRunRequest,
  CreateAgentRequest,
  CreateGrantRequest,
  CreateRunRequest,
  Decision,
  DecisionConflict,
  EventInput,
  Grant,
  HealthResponse,
  AkashiConfig,
  QueryFilters,
  QueryResponse,
  SearchResponse,
  TraceRequest,
  TraceResponse,
} from "./types.js";

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

function buildTraceBody(
  agentId: string,
  request: TraceRequest,
): Record<string, unknown> {
  const decision: Record<string, unknown> = {
    decision_type: request.decisionType,
    outcome: request.outcome,
    confidence: request.confidence,
  };
  if (request.reasoning !== undefined) decision.reasoning = request.reasoning;
  if (request.alternatives !== undefined)
    decision.alternatives = request.alternatives;
  if (request.evidence !== undefined) decision.evidence = request.evidence;

  const body: Record<string, unknown> = { agent_id: agentId, decision };
  if (request.metadata !== undefined) body.metadata = request.metadata;
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
  asOf: string,
  filters?: QueryFilters,
): Record<string, unknown> {
  const body: Record<string, unknown> = { as_of: asOf };
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
  private readonly timeoutMs: number;
  private readonly tokenManager: TokenManager;

  constructor(config: AkashiConfig) {
    this.baseUrl = config.baseUrl.replace(/\/+$/, "");
    this.agentId = config.agentId;
    this.timeoutMs = config.timeoutMs ?? 30_000;
    this.tokenManager = new TokenManager(
      this.baseUrl,
      config.agentId,
      config.apiKey,
      this.timeoutMs,
    );
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
    return this.post<TraceResponse>(
      "/v1/trace",
      buildTraceBody(this.agentId, request),
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
    return this.post<QueryResponse>(
      "/v1/query",
      buildQueryBody(
        filters,
        options?.limit ?? 50,
        options?.offset ?? 0,
        options?.orderBy ?? "valid_from",
        options?.orderDir ?? "desc",
      ),
    );
  }

  /** Search decision history by semantic similarity. */
  async search(
    query: string,
    limit?: number,
    semantic = false,
  ): Promise<SearchResponse> {
    return this.post<SearchResponse>(
      "/v1/search",
      buildSearchBody(query, limit ?? 5, semantic),
    );
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
    const data = await this.get<{ decisions: Decision[] }>(
      `/v1/decisions/recent?${params.toString()}`,
    );
    return data.decisions ?? [];
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
  async appendEvents(runId: string, events: EventInput[]): Promise<void> {
    await this.post<unknown>(
      `/v1/runs/${encodeURIComponent(runId)}/events`,
      buildAppendEventsBody(events),
    );
  }

  /** Mark a run as complete. */
  async completeRun(
    runId: string,
    req: CompleteRunRequest,
  ): Promise<AgentRun> {
    return this.post<AgentRun>(
      `/v1/runs/${encodeURIComponent(runId)}/complete`,
      buildCompleteRunBody(req),
    );
  }

  /** Get a run by ID. */
  async getRun(runId: string): Promise<AgentRun> {
    return this.get<AgentRun>(`/v1/runs/${encodeURIComponent(runId)}`);
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

  // --- Temporal query ---

  /** Query decisions as of a specific point in time. */
  async temporalQuery(
    asOf: string,
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

  // --- Health (no auth) ---

  /** Check server health. Does not require authentication. */
  async health(): Promise<HealthResponse> {
    return this.getNoAuth<HealthResponse>("/health");
  }

  // --- HTTP transport ---

  private async post<T>(path: string, body: unknown): Promise<T> {
    const token = await this.tokenManager.getToken();
    const resp = await fetch(`${this.baseUrl}${path}`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${token}`,
      },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(this.timeoutMs),
    });
    return handleResponse<T>(resp);
  }

  private async get<T>(path: string): Promise<T> {
    const token = await this.tokenManager.getToken();
    const resp = await fetch(`${this.baseUrl}${path}`, {
      method: "GET",
      headers: { Authorization: `Bearer ${token}` },
      signal: AbortSignal.timeout(this.timeoutMs),
    });
    return handleResponse<T>(resp);
  }

  private async del(path: string): Promise<void> {
    const token = await this.tokenManager.getToken();
    const resp = await fetch(`${this.baseUrl}${path}`, {
      method: "DELETE",
      headers: { Authorization: `Bearer ${token}` },
      signal: AbortSignal.timeout(this.timeoutMs),
    });
    if (resp.status === 204) return;
    await handleResponse<unknown>(resp);
  }

  private async getNoAuth<T>(path: string): Promise<T> {
    const resp = await fetch(`${this.baseUrl}${path}`, {
      method: "GET",
      signal: AbortSignal.timeout(this.timeoutMs),
    });
    return handleResponse<T>(resp);
  }
}
