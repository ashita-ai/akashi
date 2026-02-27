import type {
  APIResponse,
  APIError,
  AuthTokenRequest,
  AuthTokenResponse,
  Agent,
  AgentEvent,
  AgentsList,
  AgentStats,
  CreateAgentRequest,
  CreateGrantRequest,
  Decision,
  DecisionConflict,
  Grant,
  GrantsList,
  PaginatedDecisions,
  QueryRequest,
  ConflictsList,
  SearchResponse,
  SessionView,
  TraceHealth,
  AgentRun,
} from "@/types/api";

class ApiError extends Error {
  code: string;
  status: number;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = "ApiError";
    this.code = code;
    this.status = status;
  }
}

// Read the token directly from localStorage on every request. This avoids
// the React effect timing race where a module-level callback would be null
// during the first render cycle after login or page load.
function getStoredToken(): string | null {
  try {
    return localStorage.getItem("akashi_token");
  } catch {
    return null;
  }
}

async function request<T>(
  path: string,
  options: RequestInit = {},
): Promise<T> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    ...(options.headers as Record<string, string>),
  };

  const token = getStoredToken();
  if (token) {
    headers["Authorization"] = `Bearer ${token}`;
  }

  const res = await fetch(path, {
    ...options,
    headers,
  });

  if (!res.ok) {
    let apiError: APIError | null = null;
    try {
      apiError = (await res.json()) as APIError;
    } catch {
      // Response wasn't JSON
    }
    throw new ApiError(
      res.status,
      apiError?.error.code ?? "UNKNOWN",
      apiError?.error.message ?? `Request failed with status ${res.status}`,
    );
  }

  const json = (await res.json()) as APIResponse<T>;
  return json.data;
}

// Auth
export async function login(
  agentId: string,
  apiKey: string,
): Promise<AuthTokenResponse> {
  const body: AuthTokenRequest = { agent_id: agentId, api_key: apiKey };
  return request<AuthTokenResponse>("/auth/token", {
    method: "POST",
    body: JSON.stringify(body),
  });
}

// Decisions
export async function queryDecisions(
  req: QueryRequest,
): Promise<PaginatedDecisions> {
  return request<PaginatedDecisions>("/v1/query", {
    method: "POST",
    body: JSON.stringify(req),
  });
}

export async function getRecentDecisions(params?: {
  limit?: number;
  agent_id?: string;
  decision_type?: string;
}): Promise<PaginatedDecisions> {
  const searchParams = new URLSearchParams();
  if (params?.limit) searchParams.set("limit", String(params.limit));
  if (params?.agent_id) searchParams.set("agent_id", params.agent_id);
  if (params?.decision_type)
    searchParams.set("decision_type", params.decision_type);
  const qs = searchParams.toString();
  return request<PaginatedDecisions>(
    `/v1/decisions/recent${qs ? `?${qs}` : ""}`,
  );
}

// Runs
export async function getRun(runId: string): Promise<AgentRun> {
  const result = await request<{ run: AgentRun; decisions: Decision[] | null; events: AgentEvent[] | null }>(
    `/v1/runs/${runId}`,
  );
  return {
    ...result.run,
    decisions: result.decisions ?? undefined,
    events: result.events ?? undefined,
  };
}

// Agents
export interface AgentWithStats extends Agent {
  decision_count?: number;
  last_decision_at?: string | null;
}

export async function listAgents(): Promise<Agent[]> {
  const result = await request<AgentsList>("/v1/agents");
  // The API may return the agents array directly or wrapped
  return Array.isArray(result) ? result : (result.agents ?? []);
}

export async function listAgentsWithStats(): Promise<AgentWithStats[]> {
  const result = await request<{ agents: AgentWithStats[] }>(
    "/v1/agents?include=stats",
  );
  return Array.isArray(result) ? result : (result.agents ?? []);
}

export async function getAgent(agentId: string): Promise<Agent> {
  return request<Agent>(`/v1/agents/${agentId}`);
}

export async function updateAgent(
  agentId: string,
  updates: { name?: string; metadata?: Record<string, unknown> },
): Promise<Agent> {
  return request<Agent>(`/v1/agents/${agentId}`, {
    method: "PATCH",
    body: JSON.stringify(updates),
  });
}

export async function getAgentStats(agentId: string): Promise<AgentStats> {
  return request<AgentStats>(`/v1/agents/${agentId}/stats`);
}

export async function createAgent(
  req: CreateAgentRequest,
): Promise<Agent> {
  return request<Agent>("/v1/agents", {
    method: "POST",
    body: JSON.stringify(req),
  });
}

export async function deleteAgent(agentId: string): Promise<void> {
  await request<unknown>(`/v1/agents/${agentId}`, {
    method: "DELETE",
  });
}

// Conflicts
export async function listConflicts(params?: {
  decision_type?: string;
  agent_id?: string;
  conflict_kind?: "cross_agent" | "self_contradiction";
  status?: string;
  limit?: number;
  offset?: number;
}): Promise<ConflictsList> {
  const searchParams = new URLSearchParams();
  if (params?.limit) searchParams.set("limit", String(params.limit));
  if (params?.offset) searchParams.set("offset", String(params.offset));
  if (params?.decision_type)
    searchParams.set("decision_type", params.decision_type);
  if (params?.agent_id) searchParams.set("agent_id", params.agent_id);
  if (params?.conflict_kind) searchParams.set("conflict_kind", params.conflict_kind);
  if (params?.status) searchParams.set("status", params.status);
  const qs = searchParams.toString();
  return request<ConflictsList>(`/v1/conflicts${qs ? `?${qs}` : ""}`);
}

// Search
export async function searchDecisions(
  query: string,
  semantic: boolean,
  limit = 20,
): Promise<SearchResponse> {
  return request<SearchResponse>("/v1/search", {
    method: "POST",
    body: JSON.stringify({ query, semantic, limit }),
  });
}

// Agent history
export async function getAgentHistory(
  agentId: string,
  params?: { limit?: number; offset?: number; from?: string; to?: string },
): Promise<{
  agent_id: string;
  decisions: Decision[];
  total: number;
  limit: number;
  offset: number;
}> {
  const searchParams = new URLSearchParams();
  if (params?.limit) searchParams.set("limit", String(params.limit));
  if (params?.offset) searchParams.set("offset", String(params.offset));
  if (params?.from) searchParams.set("from", params.from);
  if (params?.to) searchParams.set("to", params.to);
  const qs = searchParams.toString();
  return request(`/v1/agents/${agentId}/history${qs ? `?${qs}` : ""}`);
}

// Single decision
export async function getDecision(id: string): Promise<Decision> {
  return request<Decision>(`/v1/decisions/${id}`);
}

// Revisions
export async function getDecisionRevisions(
  id: string,
): Promise<{ decision_id: string; revisions: Decision[]; count: number }> {
  return request(`/v1/decisions/${id}/revisions`);
}

// Decision integrity verification
export async function verifyDecisionIntegrity(
  id: string,
): Promise<{ decision_id: string; status: string; valid?: boolean; content_hash?: string; message?: string }> {
  return request(`/v1/verify/${id}`);
}

// Decision conflicts
export async function getDecisionConflicts(
  id: string,
): Promise<{ conflicts: DecisionConflict[]; total: number }> {
  return request(`/v1/decisions/${id}/conflicts`);
}

// Patch conflict status
export async function patchConflict(
  id: string,
  body: { status: string; resolution_note?: string; winning_decision_id?: string },
): Promise<DecisionConflict> {
  return request<DecisionConflict>(`/v1/conflicts/${id}`, {
    method: "PATCH",
    body: JSON.stringify(body),
  });
}

// Adjudicate conflict — creates a decision trace and links it to the conflict
export async function adjudicateConflict(
  id: string,
  body: { outcome: string; reasoning?: string; decision_type?: string; winning_decision_id?: string },
): Promise<DecisionConflict> {
  return request<DecisionConflict>(`/v1/conflicts/${id}/adjudicate`, {
    method: "POST",
    body: JSON.stringify(body),
  });
}

// Trace health
export async function getTraceHealth(): Promise<TraceHealth> {
  return request<TraceHealth>("/v1/trace-health");
}

// Session view
export async function getSession(sessionId: string): Promise<SessionView> {
  return request<SessionView>(`/v1/sessions/${sessionId}`);
}

// Grants
export async function listGrants(params?: {
  limit?: number;
  offset?: number;
}): Promise<GrantsList> {
  const searchParams = new URLSearchParams();
  if (params?.limit) searchParams.set("limit", String(params.limit));
  if (params?.offset) searchParams.set("offset", String(params.offset));
  const qs = searchParams.toString();
  return request<GrantsList>(`/v1/grants${qs ? `?${qs}` : ""}`);
}

export async function createGrant(
  req: CreateGrantRequest,
): Promise<Grant> {
  return request<Grant>("/v1/grants", {
    method: "POST",
    body: JSON.stringify(req),
  });
}

export async function deleteGrant(grantId: string): Promise<void> {
  await request<unknown>(`/v1/grants/${grantId}`, {
    method: "DELETE",
  });
}

export { ApiError };

// setTokenProvider kept for compatibility but is no longer used — token is
// read directly from localStorage in request().
export function setTokenProvider(_provider: () => string | null): void {}
