import { TokenManager } from "./auth.js";
import {
  AuthenticationError,
  AuthorizationError,
  ConflictError,
  AkashiError,
  NotFoundError,
  ServerError,
  ValidationError,
} from "./errors.js";
import type {
  CheckResponse,
  Decision,
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
): Record<string, unknown> {
  return { query, limit };
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
  async search(query: string, limit?: number): Promise<SearchResponse> {
    return this.post<SearchResponse>(
      "/v1/search",
      buildSearchBody(query, limit ?? 5),
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
}
