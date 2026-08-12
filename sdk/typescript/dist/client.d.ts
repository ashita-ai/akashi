import type { Agent, AgentRun, CheckResponse, CompleteRunRequest, CreateAgentRequest, CreateGrantRequest, CreateRunRequest, Decision, DecisionConflict, EventInput, Grant, HealthResponse, AkashiConfig, QueryFilters, QueryResponse, SearchResponse, TraceRequest, TraceResponse } from "./types.js";
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
export declare class AkashiClient {
    private readonly baseUrl;
    private readonly agentId;
    private readonly sessionId;
    private readonly timeoutMs;
    private readonly tokenManager;
    constructor(config: AkashiConfig);
    /** Check for existing decisions before making a new one. */
    check(decisionType: string, query?: string, options?: {
        agentId?: string;
        limit?: number;
    }): Promise<CheckResponse>;
    /** Record a decision trace. */
    trace(request: TraceRequest): Promise<TraceResponse>;
    /** Query past decisions with structured filters. */
    query(filters?: QueryFilters, options?: {
        limit?: number;
        offset?: number;
        orderBy?: string;
        orderDir?: string;
    }): Promise<QueryResponse>;
    /** Search decision history by semantic similarity. */
    search(query: string, limit?: number, semantic?: boolean): Promise<SearchResponse>;
    /** Get the most recent decisions. */
    recent(options?: {
        limit?: number;
        agentId?: string;
        decisionType?: string;
    }): Promise<Decision[]>;
    /** Create a new agent run. */
    createRun(req?: CreateRunRequest): Promise<AgentRun>;
    /** Append events to an existing run. */
    appendEvents(runId: string, events: EventInput[]): Promise<void>;
    /** Mark a run as complete. */
    completeRun(runId: string, req: CompleteRunRequest): Promise<AgentRun>;
    /** Get a run by ID. */
    getRun(runId: string): Promise<AgentRun>;
    /** Create a new agent. Requires admin or higher role. */
    createAgent(req: CreateAgentRequest): Promise<Agent>;
    /** List all agents in the org. Requires admin or higher role. */
    listAgents(): Promise<Agent[]>;
    /** Delete an agent by agent_id. Requires admin or higher role. */
    deleteAgent(agentId: string): Promise<void>;
    /** Query decisions as of a specific point in time. */
    temporalQuery(asOf: string, filters?: QueryFilters): Promise<Decision[]>;
    /** Get decision history for a specific agent. */
    agentHistory(agentId: string, limit?: number): Promise<Decision[]>;
    /** Create an access grant. */
    createGrant(req: CreateGrantRequest): Promise<Grant>;
    /** Delete an access grant by ID. */
    deleteGrant(grantId: string): Promise<void>;
    /** List detected decision conflicts. */
    listConflicts(options?: {
        decisionType?: string;
        agentId?: string;
        conflictKind?: "cross_agent" | "self_contradiction";
        limit?: number;
        offset?: number;
    }): Promise<DecisionConflict[]>;
    /** Check server health. Does not require authentication. */
    health(): Promise<HealthResponse>;
    private post;
    private get;
    private del;
    private getNoAuth;
}
//# sourceMappingURL=client.d.ts.map