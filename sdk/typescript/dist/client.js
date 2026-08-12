import { TokenManager } from "./auth.js";
import { AuthenticationError, AuthorizationError, ConflictError, AkashiError, NotFoundError, RateLimitError, ServerError, ValidationError, } from "./errors.js";
const USER_AGENT = "akashi-typescript/0.2.0";
// ---------------------------------------------------------------------------
// Shared body builders — single source of truth for request shapes.
// ---------------------------------------------------------------------------
function buildCheckBody(decisionType, query, agentId, limit) {
    const body = {
        decision_type: decisionType,
        limit,
    };
    if (query !== undefined)
        body.query = query;
    if (agentId !== undefined)
        body.agent_id = agentId;
    return body;
}
function buildTraceBody(agentId, request) {
    const decision = {
        decision_type: request.decisionType,
        outcome: request.outcome,
        confidence: request.confidence,
    };
    if (request.reasoning !== undefined)
        decision.reasoning = request.reasoning;
    if (request.alternatives !== undefined)
        decision.alternatives = request.alternatives;
    if (request.evidence !== undefined)
        decision.evidence = request.evidence;
    const body = { agent_id: agentId, decision };
    if (request.metadata !== undefined)
        body.metadata = request.metadata;
    if (request.context !== undefined)
        body.context = request.context;
    return body;
}
function buildQueryBody(filters, limit, offset, orderBy, orderDir) {
    return {
        filters: filters ?? {},
        limit,
        offset,
        order_by: orderBy,
        order_dir: orderDir,
    };
}
function buildSearchBody(query, limit, semantic) {
    return { query, limit, semantic };
}
function buildRecentParams(limit, agentId, decisionType) {
    const params = new URLSearchParams();
    params.set("limit", String(limit));
    if (agentId)
        params.set("agent_id", agentId);
    if (decisionType)
        params.set("decision_type", decisionType);
    return params;
}
function buildCreateRunBody(agentId, req) {
    const body = { agent_id: agentId };
    if (req?.traceId !== undefined)
        body.trace_id = req.traceId;
    if (req?.parentRunId !== undefined)
        body.parent_run_id = req.parentRunId;
    if (req?.metadata !== undefined)
        body.metadata = req.metadata;
    return body;
}
function buildAppendEventsBody(events) {
    return {
        events: events.map((e) => {
            const ev = { event_type: e.eventType };
            if (e.occurredAt !== undefined)
                ev.occurred_at = e.occurredAt;
            if (e.payload !== undefined)
                ev.payload = e.payload;
            return ev;
        }),
    };
}
function buildCompleteRunBody(req) {
    const body = { status: req.status };
    if (req.metadata !== undefined)
        body.metadata = req.metadata;
    return body;
}
function buildTemporalQueryBody(asOf, filters) {
    const body = { as_of: asOf };
    if (filters !== undefined)
        body.filters = filters;
    return body;
}
function buildCreateAgentBody(req) {
    const body = {
        agent_id: req.agentId,
        name: req.name,
        role: req.role,
        api_key: req.apiKey,
    };
    if (req.metadata !== undefined)
        body.metadata = req.metadata;
    return body;
}
function buildCreateGrantBody(req) {
    const body = {
        grantee_agent_id: req.granteeAgentId,
        resource_type: req.resourceType,
        permission: req.permission,
    };
    if (req.resourceId !== undefined)
        body.resource_id = req.resourceId;
    if (req.expiresAt !== undefined)
        body.expires_at = req.expiresAt;
    return body;
}
async function extractErrorMessage(resp, fallback) {
    try {
        const body = (await resp.json());
        return body.error?.message ?? fallback;
    }
    catch {
        return fallback;
    }
}
async function handleResponse(resp) {
    if (resp.status === 400) {
        throw new ValidationError(await extractErrorMessage(resp, "Bad request"));
    }
    if (resp.status === 401) {
        throw new AuthenticationError(await extractErrorMessage(resp, "Authentication failed"));
    }
    if (resp.status === 403) {
        throw new AuthorizationError(await extractErrorMessage(resp, "Insufficient permissions"));
    }
    if (resp.status === 404) {
        throw new NotFoundError(await extractErrorMessage(resp, "Resource not found"));
    }
    if (resp.status === 409) {
        throw new ConflictError(await extractErrorMessage(resp, "Conflict"));
    }
    if (resp.status === 429) {
        throw new RateLimitError(await extractErrorMessage(resp, "Rate limit exceeded"));
    }
    if (resp.status >= 500) {
        throw new ServerError(resp.status, await extractErrorMessage(resp, "Server error"));
    }
    if (resp.status >= 400) {
        throw new AkashiError(await extractErrorMessage(resp, `Unexpected: ${resp.status}`), resp.status);
    }
    const body = (await resp.json());
    // The server wraps all responses in {data: ...}. If the envelope is
    // present, unwrap it; otherwise return the body as-is. The cast is
    // unavoidable at the boundary — callers get the type they asked for,
    // and Pydantic-style runtime validation isn't idiomatic in TypeScript.
    if (body.data !== undefined) {
        return body.data;
    }
    return body;
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
    baseUrl;
    agentId;
    sessionId;
    timeoutMs;
    tokenManager;
    constructor(config) {
        this.baseUrl = config.baseUrl.replace(/\/+$/, "");
        this.agentId = config.agentId;
        this.sessionId = config.sessionId ?? crypto.randomUUID();
        this.timeoutMs = config.timeoutMs ?? 30_000;
        this.tokenManager = new TokenManager(this.baseUrl, config.agentId, config.apiKey, this.timeoutMs);
    }
    /** Check for existing decisions before making a new one. */
    async check(decisionType, query, options) {
        return this.post("/v1/check", buildCheckBody(decisionType, query, options?.agentId, options?.limit ?? 5));
    }
    /** Record a decision trace. */
    async trace(request) {
        const token = await this.tokenManager.getToken();
        const resp = await fetch(`${this.baseUrl}/v1/trace`, {
            method: "POST",
            headers: {
                "Content-Type": "application/json",
                Authorization: `Bearer ${token}`,
                "User-Agent": USER_AGENT,
                "X-Akashi-Session": this.sessionId,
            },
            body: JSON.stringify(buildTraceBody(this.agentId, request)),
            signal: AbortSignal.timeout(this.timeoutMs),
        });
        return handleResponse(resp);
    }
    /** Query past decisions with structured filters. */
    async query(filters, options) {
        return this.post("/v1/query", buildQueryBody(filters, options?.limit ?? 50, options?.offset ?? 0, options?.orderBy ?? "valid_from", options?.orderDir ?? "desc"));
    }
    /** Search decision history by semantic similarity. */
    async search(query, limit, semantic = false) {
        return this.post("/v1/search", buildSearchBody(query, limit ?? 5, semantic));
    }
    /** Get the most recent decisions. */
    async recent(options) {
        const params = buildRecentParams(options?.limit ?? 10, options?.agentId, options?.decisionType);
        const data = await this.get(`/v1/decisions/recent?${params.toString()}`);
        return data.decisions ?? [];
    }
    // --- Run lifecycle ---
    /** Create a new agent run. */
    async createRun(req) {
        return this.post("/v1/runs", buildCreateRunBody(this.agentId, req));
    }
    /** Append events to an existing run. */
    async appendEvents(runId, events) {
        await this.post(`/v1/runs/${encodeURIComponent(runId)}/events`, buildAppendEventsBody(events));
    }
    /** Mark a run as complete. */
    async completeRun(runId, req) {
        return this.post(`/v1/runs/${encodeURIComponent(runId)}/complete`, buildCompleteRunBody(req));
    }
    /** Get a run by ID. */
    async getRun(runId) {
        return this.get(`/v1/runs/${encodeURIComponent(runId)}`);
    }
    // --- Agent management (admin-only) ---
    /** Create a new agent. Requires admin or higher role. */
    async createAgent(req) {
        return this.post("/v1/agents", buildCreateAgentBody(req));
    }
    /** List all agents in the org. Requires admin or higher role. */
    async listAgents() {
        return this.get("/v1/agents");
    }
    /** Delete an agent by agent_id. Requires admin or higher role. */
    async deleteAgent(agentId) {
        await this.del(`/v1/agents/${encodeURIComponent(agentId)}`);
    }
    // --- Temporal query ---
    /** Query decisions as of a specific point in time. */
    async temporalQuery(asOf, filters) {
        return this.post("/v1/query/temporal", buildTemporalQueryBody(asOf, filters));
    }
    // --- Agent history ---
    /** Get decision history for a specific agent. */
    async agentHistory(agentId, limit) {
        const params = new URLSearchParams();
        if (limit !== undefined)
            params.set("limit", String(limit));
        const qs = params.toString();
        const path = `/v1/agents/${encodeURIComponent(agentId)}/history${qs ? `?${qs}` : ""}`;
        return this.get(path);
    }
    // --- Grants ---
    /** Create an access grant. */
    async createGrant(req) {
        return this.post("/v1/grants", buildCreateGrantBody(req));
    }
    /** Delete an access grant by ID. */
    async deleteGrant(grantId) {
        await this.del(`/v1/grants/${encodeURIComponent(grantId)}`);
    }
    // --- Conflicts ---
    /** List detected decision conflicts. */
    async listConflicts(options) {
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
        return this.get(`/v1/conflicts${qs ? `?${qs}` : ""}`);
    }
    // --- Health (no auth) ---
    /** Check server health. Does not require authentication. */
    async health() {
        return this.getNoAuth("/health");
    }
    // --- HTTP transport ---
    async post(path, body) {
        const token = await this.tokenManager.getToken();
        const resp = await fetch(`${this.baseUrl}${path}`, {
            method: "POST",
            headers: {
                "Content-Type": "application/json",
                Authorization: `Bearer ${token}`,
                "User-Agent": USER_AGENT,
            },
            body: JSON.stringify(body),
            signal: AbortSignal.timeout(this.timeoutMs),
        });
        return handleResponse(resp);
    }
    async get(path) {
        const token = await this.tokenManager.getToken();
        const resp = await fetch(`${this.baseUrl}${path}`, {
            method: "GET",
            headers: {
                Authorization: `Bearer ${token}`,
                "User-Agent": USER_AGENT,
            },
            signal: AbortSignal.timeout(this.timeoutMs),
        });
        return handleResponse(resp);
    }
    async del(path) {
        const token = await this.tokenManager.getToken();
        const resp = await fetch(`${this.baseUrl}${path}`, {
            method: "DELETE",
            headers: {
                Authorization: `Bearer ${token}`,
                "User-Agent": USER_AGENT,
            },
            signal: AbortSignal.timeout(this.timeoutMs),
        });
        if (resp.status === 204)
            return;
        await handleResponse(resp);
    }
    async getNoAuth(path) {
        const resp = await fetch(`${this.baseUrl}${path}`, {
            method: "GET",
            headers: { "User-Agent": USER_AGENT },
            signal: AbortSignal.timeout(this.timeoutMs),
        });
        return handleResponse(resp);
    }
}
//# sourceMappingURL=client.js.map