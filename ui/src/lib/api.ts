import type {
  APIResponse,
  APIError,
  AuthTokenRequest,
  AuthTokenResponse,
  Agent,
  AgentsList,
  CreateAgentRequest,
  Decision,
  PaginatedDecisions,
  QueryRequest,
  ConflictsList,
  SearchResponse,
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

let getToken: (() => string | null) | null = null;

export function setTokenProvider(provider: () => string | null) {
  getToken = provider;
}

async function request<T>(
  path: string,
  options: RequestInit = {},
): Promise<T> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    ...(options.headers as Record<string, string>),
  };

  const token = getToken?.();
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
  return request<AgentRun>(`/v1/runs/${runId}`);
}

// Agents
export async function listAgents(): Promise<Agent[]> {
  const result = await request<AgentsList>("/v1/agents");
  // The API may return the agents array directly or wrapped
  return Array.isArray(result) ? result : (result.agents ?? []);
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
  limit?: number;
  offset?: number;
}): Promise<ConflictsList> {
  const searchParams = new URLSearchParams();
  if (params?.limit) searchParams.set("limit", String(params.limit));
  if (params?.offset) searchParams.set("offset", String(params.offset));
  if (params?.decision_type)
    searchParams.set("decision_type", params.decision_type);
  if (params?.agent_id) searchParams.set("agent_id", params.agent_id);
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

export { ApiError };
