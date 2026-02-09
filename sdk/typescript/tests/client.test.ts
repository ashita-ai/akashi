import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { AkashiClient } from "../src/client.js";
import { withAkashi } from "../src/middleware.js";
import {
  AkashiError,
  AuthenticationError,
  AuthorizationError,
  ConflictError,
  NotFoundError,
  RateLimitError,
  ServerError,
  ValidationError,
} from "../src/errors.js";
import type {
  CheckResponse,
  Decision,
  TraceRequest,
  AgentRun,
  Agent,
  Grant,
  DecisionConflict,
  HealthResponse,
  UsageResponse,
} from "../src/types.js";

// Mock fetch globally.
const mockFetch = vi.fn();
vi.stubGlobal("fetch", mockFetch);

// Helper to create a mock Response.
function mockResponse(
  status: number,
  body: unknown,
  ok = status >= 200 && status < 300,
): Response {
  return {
    ok,
    status,
    json: () => Promise.resolve(body),
  } as Response;
}

// Helper for 204 No Content responses (DELETE).
function mockNoContent(): Response {
  return {
    ok: true,
    status: 204,
    json: () => Promise.reject(new Error("no body")),
  } as Response;
}

// Standard token response for auth.
const TOKEN_RESPONSE = {
  data: {
    token: "test-jwt-token",
    expires_at: new Date(Date.now() + 3600_000).toISOString(),
  },
};

describe("AkashiClient", () => {
  let client: AkashiClient;

  beforeEach(() => {
    mockFetch.mockReset();
    client = new AkashiClient({
      baseUrl: "http://localhost:8080",
      agentId: "test-agent",
      apiKey: "test-key",
    });
  });

  afterEach(() => {
    vi.clearAllMocks();
  });

  describe("authentication", () => {
    it("fetches token before first request", async () => {
      mockFetch
        .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
        .mockResolvedValueOnce(
          mockResponse(200, {
            data: { has_precedent: false, precedents: [] },
          }),
        );

      await client.check("test-type");

      expect(mockFetch).toHaveBeenCalledTimes(2);
      expect(mockFetch).toHaveBeenNthCalledWith(
        1,
        "http://localhost:8080/auth/token",
        expect.objectContaining({
          method: "POST",
          body: JSON.stringify({ agent_id: "test-agent", api_key: "test-key" }),
        }),
      );
    });

    it("reuses cached token for subsequent requests", async () => {
      mockFetch
        .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
        .mockResolvedValueOnce(
          mockResponse(200, { data: { has_precedent: false, precedents: [] } }),
        )
        .mockResolvedValueOnce(
          mockResponse(200, { data: { has_precedent: true, precedents: [] } }),
        );

      await client.check("first");
      await client.check("second");

      // Only one token request, two API requests.
      expect(mockFetch).toHaveBeenCalledTimes(3);
      const calls = mockFetch.mock.calls.map((c) => c[0]);
      expect(calls.filter((u) => u.includes("/auth/token"))).toHaveLength(1);
    });
  });

  describe("check", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("returns precedent check result", async () => {
      const expected: CheckResponse = {
        has_precedent: true,
        precedents: [
          {
            decision_id: "dec-123",
            decision_type: "model_selection",
            outcome: "gpt-4o",
            confidence: 0.9,
            similarity: 0.85,
          },
        ],
      };
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: expected }));

      const result = await client.check("model_selection", "summarization");

      expect(result).toEqual(expected);
      expect(mockFetch).toHaveBeenLastCalledWith(
        "http://localhost:8080/v1/check",
        expect.objectContaining({
          method: "POST",
          headers: expect.objectContaining({
            Authorization: "Bearer test-jwt-token",
            "Content-Type": "application/json",
          }),
        }),
      );
    });

    it("includes optional parameters in request body", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: { has_precedent: false, precedents: [] } }),
      );

      await client.check("arch", "storage", { agentId: "other", limit: 10 });

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body).toEqual({
        decision_type: "arch",
        query: "storage",
        agent_id: "other",
        limit: 10,
      });
    });
  });

  describe("trace", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("records a decision trace", async () => {
      const traceResponse = { run_id: "run-456", decision_id: "dec-789" };
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: traceResponse }),
      );

      const request: TraceRequest = {
        decisionType: "model_selection",
        outcome: "chose gpt-4o",
        confidence: 0.92,
        reasoning: "best for summarization",
        alternatives: [
          { label: "gpt-4o", selected: true, score: 0.92 },
          { label: "claude-3-haiku", selected: false, score: 0.78 },
        ],
        evidence: [
          { source_type: "benchmark", content: "94% accuracy on eval set" },
        ],
      };

      const result = await client.trace(request);

      expect(result).toEqual(traceResponse);

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body.agent_id).toBe("test-agent");
      expect(body.decision.decision_type).toBe("model_selection");
      expect(body.decision.alternatives).toHaveLength(2);
      expect(body.decision.evidence).toHaveLength(1);
    });

    it("handles minimal trace request", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: { run_id: "r", decision_id: "d" } }),
      );

      await client.trace({
        decisionType: "simple",
        outcome: "done",
        confidence: 1.0,
      });

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body.decision.reasoning).toBeUndefined();
      expect(body.decision.alternatives).toBeUndefined();
      expect(body.decision.evidence).toBeUndefined();
    });

    it("sends correct wire format for trace body", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, {
          data: { run_id: "r", decision_id: "d", event_count: 3 },
        }),
      );

      await client.trace({
        decisionType: "model_selection",
        outcome: "gpt-4o",
        confidence: 0.9,
        reasoning: "best option",
        metadata: { env: "prod" },
      });

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body).toEqual({
        agent_id: "test-agent",
        decision: {
          decision_type: "model_selection",
          outcome: "gpt-4o",
          confidence: 0.9,
          reasoning: "best option",
        },
        metadata: { env: "prod" },
      });
    });
  });

  describe("query", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("queries with filters", async () => {
      const decisions: Decision[] = [
        {
          id: "dec-1",
          agent_id: "test",
          run_id: "run-1",
          decision_type: "model",
          outcome: "gpt-4o",
          confidence: 0.9,
          valid_from: "2024-01-01T00:00:00Z",
        },
      ];
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: { decisions, total: 1 } }),
      );

      const result = await client.query(
        { agentId: "test", decisionType: "model", minConfidence: 0.8 },
        { limit: 20, offset: 10 },
      );

      expect(result.decisions).toEqual(decisions);
      expect(result.total).toBe(1);

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body.filters.agentId).toBe("test");
      expect(body.limit).toBe(20);
      expect(body.offset).toBe(10);
    });

    it("uses defaults for optional parameters", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: { decisions: [], total: 0 } }),
      );

      await client.query();

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body.limit).toBe(50);
      expect(body.offset).toBe(0);
      expect(body.order_by).toBe("valid_from");
      expect(body.order_dir).toBe("desc");
    });

    it("sends correct wire format for query body", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, {
          data: { decisions: [], total: 0, limit: 50, offset: 0 },
        }),
      );

      await client.query(
        { decision_type: "arch", confidence_min: 0.5 },
        { orderBy: "created_at", orderDir: "asc" },
      );

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe("http://localhost:8080/v1/query");
      expect(lastCall[1].method).toBe("POST");
      const body = JSON.parse(lastCall[1].body);
      expect(body).toEqual({
        filters: { decision_type: "arch", confidence_min: 0.5 },
        limit: 50,
        offset: 0,
        order_by: "created_at",
        order_dir: "asc",
      });
    });
  });

  describe("search", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("performs semantic search", async () => {
      const results = [
        {
          decision_id: "dec-1",
          decision_type: "model",
          outcome: "gpt-4o",
          confidence: 0.9,
          similarity: 0.95,
        },
      ];
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: { results } }),
      );

      const response = await client.search("which model for text tasks", 10);

      expect(response.results).toEqual(results);

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body.query).toBe("which model for text tasks");
      expect(body.limit).toBe(10);
    });

    it("sends correct wire format for search body", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: { results: [], total: 0 } }),
      );

      await client.search("find decisions about storage");

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe("http://localhost:8080/v1/search");
      expect(lastCall[1].method).toBe("POST");
      const body = JSON.parse(lastCall[1].body);
      expect(body).toEqual({
        query: "find decisions about storage",
        limit: 5,
      });
    });
  });

  describe("recent", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("fetches recent decisions with query params", async () => {
      const decisions: Decision[] = [
        {
          id: "dec-1",
          agent_id: "test",
          run_id: "run-1",
          decision_type: "model",
          outcome: "gpt-4o",
          confidence: 0.9,
          valid_from: "2024-01-01T00:00:00Z",
        },
      ];
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: { decisions } }),
      );

      const result = await client.recent({
        limit: 5,
        agentId: "test",
        decisionType: "model",
      });

      expect(result).toEqual(decisions);
      expect(mockFetch).toHaveBeenLastCalledWith(
        expect.stringContaining("/v1/decisions/recent?"),
        expect.objectContaining({ method: "GET" }),
      );
      const url = mockFetch.mock.calls[mockFetch.mock.calls.length - 1][0];
      expect(url).toContain("limit=5");
      expect(url).toContain("agent_id=test");
      expect(url).toContain("decision_type=model");
    });

    it("uses default limit when no options provided", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: { decisions: [] } }),
      );

      await client.recent();

      const url = mockFetch.mock.calls[mockFetch.mock.calls.length - 1][0];
      expect(url).toContain("limit=10");
    });
  });

  // -----------------------------------------------------------------------
  // Run lifecycle
  // -----------------------------------------------------------------------

  describe("createRun", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("creates a run with defaults", async () => {
      const run: AgentRun = {
        id: "run-1",
        agent_id: "test-agent",
        org_id: "org-1",
        status: "running",
        metadata: {},
        started_at: "2024-01-01T00:00:00Z",
        created_at: "2024-01-01T00:00:00Z",
      };
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: run }));

      const result = await client.createRun();

      expect(result).toEqual(run);
      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe("http://localhost:8080/v1/runs");
      expect(lastCall[1].method).toBe("POST");
      const body = JSON.parse(lastCall[1].body);
      expect(body).toEqual({ agent_id: "test-agent" });
    });

    it("creates a run with all options", async () => {
      const run: AgentRun = {
        id: "run-2",
        agent_id: "test-agent",
        org_id: "org-1",
        trace_id: "trace-1",
        parent_run_id: "run-parent",
        status: "running",
        metadata: { env: "staging" },
        started_at: "2024-01-01T00:00:00Z",
        created_at: "2024-01-01T00:00:00Z",
      };
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: run }));

      const result = await client.createRun({
        traceId: "trace-1",
        parentRunId: "run-parent",
        metadata: { env: "staging" },
      });

      expect(result).toEqual(run);
      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body).toEqual({
        agent_id: "test-agent",
        trace_id: "trace-1",
        parent_run_id: "run-parent",
        metadata: { env: "staging" },
      });
    });
  });

  describe("appendEvents", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("appends events to a run", async () => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: {} }));

      await client.appendEvents("run-1", [
        { eventType: "tool_call", payload: { tool: "grep" } },
        {
          eventType: "observation",
          occurredAt: "2024-01-01T00:00:00Z",
          payload: { result: "found" },
        },
      ]);

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe(
        "http://localhost:8080/v1/runs/run-1/events",
      );
      expect(lastCall[1].method).toBe("POST");
      const body = JSON.parse(lastCall[1].body);
      expect(body).toEqual({
        events: [
          { event_type: "tool_call", payload: { tool: "grep" } },
          {
            event_type: "observation",
            occurred_at: "2024-01-01T00:00:00Z",
            payload: { result: "found" },
          },
        ],
      });
    });

    it("sends minimal event without optional fields", async () => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: {} }));

      await client.appendEvents("run-1", [{ eventType: "start" }]);

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body.events[0]).toEqual({ event_type: "start" });
      expect(body.events[0].occurred_at).toBeUndefined();
      expect(body.events[0].payload).toBeUndefined();
    });
  });

  describe("completeRun", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("completes a run", async () => {
      const run: AgentRun = {
        id: "run-1",
        agent_id: "test-agent",
        org_id: "org-1",
        status: "completed",
        metadata: {},
        started_at: "2024-01-01T00:00:00Z",
        completed_at: "2024-01-01T01:00:00Z",
        created_at: "2024-01-01T00:00:00Z",
      };
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: run }));

      const result = await client.completeRun("run-1", {
        status: "completed",
        metadata: { duration_ms: 3600 },
      });

      expect(result).toEqual(run);
      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe(
        "http://localhost:8080/v1/runs/run-1/complete",
      );
      const body = JSON.parse(lastCall[1].body);
      expect(body).toEqual({
        status: "completed",
        metadata: { duration_ms: 3600 },
      });
    });

    it("completes a run without metadata", async () => {
      const run: AgentRun = {
        id: "run-1",
        agent_id: "test-agent",
        org_id: "org-1",
        status: "failed",
        metadata: {},
        started_at: "2024-01-01T00:00:00Z",
        completed_at: "2024-01-01T00:30:00Z",
        created_at: "2024-01-01T00:00:00Z",
      };
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: run }));

      await client.completeRun("run-1", { status: "failed" });

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body).toEqual({ status: "failed" });
      expect(body.metadata).toBeUndefined();
    });
  });

  describe("getRun", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("gets a run by ID", async () => {
      const run: AgentRun = {
        id: "run-1",
        agent_id: "test-agent",
        org_id: "org-1",
        status: "running",
        metadata: {},
        started_at: "2024-01-01T00:00:00Z",
        created_at: "2024-01-01T00:00:00Z",
      };
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: run }));

      const result = await client.getRun("run-1");

      expect(result).toEqual(run);
      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe("http://localhost:8080/v1/runs/run-1");
      expect(lastCall[1].method).toBe("GET");
    });
  });

  // -----------------------------------------------------------------------
  // Agent management
  // -----------------------------------------------------------------------

  describe("createAgent", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("creates an agent with all fields", async () => {
      const agent: Agent = {
        id: "uuid-1",
        agent_id: "new-agent",
        org_id: "org-1",
        name: "New Agent",
        role: "agent",
        metadata: { team: "backend" },
        created_at: "2024-01-01T00:00:00Z",
      };
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: agent }));

      const result = await client.createAgent({
        agentId: "new-agent",
        name: "New Agent",
        role: "agent",
        apiKey: "secret-key-123",
        metadata: { team: "backend" },
      });

      expect(result).toEqual(agent);
      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe("http://localhost:8080/v1/agents");
      expect(lastCall[1].method).toBe("POST");
      const body = JSON.parse(lastCall[1].body);
      expect(body).toEqual({
        agent_id: "new-agent",
        name: "New Agent",
        role: "agent",
        api_key: "secret-key-123",
        metadata: { team: "backend" },
      });
    });

    it("creates an agent without optional metadata", async () => {
      const agent: Agent = {
        id: "uuid-2",
        agent_id: "minimal-agent",
        org_id: "org-1",
        name: "Minimal",
        role: "reader",
        metadata: {},
        created_at: "2024-01-01T00:00:00Z",
      };
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: agent }));

      await client.createAgent({
        agentId: "minimal-agent",
        name: "Minimal",
        role: "reader",
        apiKey: "key-456",
      });

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body.metadata).toBeUndefined();
    });
  });

  describe("listAgents", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("lists all agents", async () => {
      const agents: Agent[] = [
        {
          id: "uuid-1",
          agent_id: "agent-a",
          org_id: "org-1",
          name: "Agent A",
          role: "admin",
          metadata: {},
          created_at: "2024-01-01T00:00:00Z",
        },
        {
          id: "uuid-2",
          agent_id: "agent-b",
          org_id: "org-1",
          name: "Agent B",
          role: "reader",
          metadata: {},
          created_at: "2024-01-02T00:00:00Z",
        },
      ];
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: agents }));

      const result = await client.listAgents();

      expect(result).toEqual(agents);
      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe("http://localhost:8080/v1/agents");
      expect(lastCall[1].method).toBe("GET");
    });
  });

  describe("deleteAgent", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("deletes an agent by ID", async () => {
      mockFetch.mockResolvedValueOnce(mockNoContent());

      await client.deleteAgent("agent-to-delete");

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe(
        "http://localhost:8080/v1/agents/agent-to-delete",
      );
      expect(lastCall[1].method).toBe("DELETE");
    });

    it("URL-encodes the agent ID", async () => {
      mockFetch.mockResolvedValueOnce(mockNoContent());

      await client.deleteAgent("agent/with spaces");

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe(
        "http://localhost:8080/v1/agents/agent%2Fwith%20spaces",
      );
    });
  });

  // -----------------------------------------------------------------------
  // Temporal query
  // -----------------------------------------------------------------------

  describe("temporalQuery", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("queries decisions at a point in time", async () => {
      const decisions: Decision[] = [
        {
          id: "dec-1",
          agent_id: "test",
          run_id: "run-1",
          decision_type: "model",
          outcome: "gpt-4o",
          confidence: 0.9,
          valid_from: "2024-01-01T00:00:00Z",
          metadata: {},
          transaction_time: "2024-01-01T00:00:00Z",
          created_at: "2024-01-01T00:00:00Z",
        },
      ];
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: decisions }),
      );

      const result = await client.temporalQuery("2024-06-01T00:00:00Z", {
        decision_type: "model",
      });

      expect(result).toEqual(decisions);
      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe(
        "http://localhost:8080/v1/query/temporal",
      );
      expect(lastCall[1].method).toBe("POST");
      const body = JSON.parse(lastCall[1].body);
      expect(body).toEqual({
        as_of: "2024-06-01T00:00:00Z",
        filters: { decision_type: "model" },
      });
    });

    it("queries without filters", async () => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: [] }));

      await client.temporalQuery("2024-06-01T00:00:00Z");

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body).toEqual({ as_of: "2024-06-01T00:00:00Z" });
      expect(body.filters).toBeUndefined();
    });
  });

  // -----------------------------------------------------------------------
  // Agent history
  // -----------------------------------------------------------------------

  describe("agentHistory", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("fetches agent history with limit", async () => {
      const decisions: Decision[] = [
        {
          id: "dec-1",
          agent_id: "coder",
          run_id: "run-1",
          decision_type: "model",
          outcome: "gpt-4o",
          confidence: 0.9,
          valid_from: "2024-01-01T00:00:00Z",
          metadata: {},
          transaction_time: "2024-01-01T00:00:00Z",
          created_at: "2024-01-01T00:00:00Z",
        },
      ];
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: decisions }),
      );

      const result = await client.agentHistory("coder", 25);

      expect(result).toEqual(decisions);
      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe(
        "http://localhost:8080/v1/agents/coder/history?limit=25",
      );
      expect(lastCall[1].method).toBe("GET");
    });

    it("fetches agent history without limit", async () => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: [] }));

      await client.agentHistory("planner");

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe(
        "http://localhost:8080/v1/agents/planner/history",
      );
    });
  });

  // -----------------------------------------------------------------------
  // Grants
  // -----------------------------------------------------------------------

  describe("createGrant", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("creates a grant with all fields", async () => {
      const grant: Grant = {
        id: "grant-1",
        grantor_agent_id: "test-agent",
        grantee_agent_id: "reader-agent",
        resource_type: "decision",
        resource_id: "dec-1",
        permission: "read",
        expires_at: "2025-01-01T00:00:00Z",
        created_at: "2024-01-01T00:00:00Z",
      };
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: grant }));

      const result = await client.createGrant({
        granteeAgentId: "reader-agent",
        resourceType: "decision",
        resourceId: "dec-1",
        permission: "read",
        expiresAt: "2025-01-01T00:00:00Z",
      });

      expect(result).toEqual(grant);
      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe("http://localhost:8080/v1/grants");
      expect(lastCall[1].method).toBe("POST");
      const body = JSON.parse(lastCall[1].body);
      expect(body).toEqual({
        grantee_agent_id: "reader-agent",
        resource_type: "decision",
        resource_id: "dec-1",
        permission: "read",
        expires_at: "2025-01-01T00:00:00Z",
      });
    });

    it("creates a grant without optional fields", async () => {
      const grant: Grant = {
        id: "grant-2",
        grantor_agent_id: "test-agent",
        grantee_agent_id: "other",
        resource_type: "run",
        permission: "write",
        created_at: "2024-01-01T00:00:00Z",
      };
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: grant }));

      await client.createGrant({
        granteeAgentId: "other",
        resourceType: "run",
        permission: "write",
      });

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body.resource_id).toBeUndefined();
      expect(body.expires_at).toBeUndefined();
    });
  });

  describe("deleteGrant", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("deletes a grant by ID", async () => {
      mockFetch.mockResolvedValueOnce(mockNoContent());

      await client.deleteGrant("grant-123");

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe(
        "http://localhost:8080/v1/grants/grant-123",
      );
      expect(lastCall[1].method).toBe("DELETE");
    });
  });

  // -----------------------------------------------------------------------
  // Conflicts
  // -----------------------------------------------------------------------

  describe("listConflicts", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("lists conflicts without options", async () => {
      const conflicts: DecisionConflict[] = [
        {
          decision_a_id: "dec-1",
          decision_b_id: "dec-2",
          agent_a: "planner",
          agent_b: "coder",
          run_a: "run-1",
          run_b: "run-2",
          decision_type: "architecture",
          outcome_a: "microservices",
          outcome_b: "monolith",
          confidence_a: 0.9,
          confidence_b: 0.85,
          decided_at_a: "2024-01-01T00:00:00Z",
          decided_at_b: "2024-01-02T00:00:00Z",
          detected_at: "2024-01-02T00:01:00Z",
        },
      ];
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: conflicts }),
      );

      const result = await client.listConflicts();

      expect(result).toEqual(conflicts);
      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe("http://localhost:8080/v1/conflicts");
      expect(lastCall[1].method).toBe("GET");
    });

    it("lists conflicts with all options", async () => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: [] }));

      await client.listConflicts({
        decisionType: "model",
        limit: 10,
        offset: 5,
      });

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const url = lastCall[0] as string;
      expect(url).toContain("decision_type=model");
      expect(url).toContain("limit=10");
      expect(url).toContain("offset=5");
    });
  });

  // -----------------------------------------------------------------------
  // Usage
  // -----------------------------------------------------------------------

  describe("getUsage", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("returns usage statistics", async () => {
      const usage: UsageResponse = {
        org_id: "org-1",
        period: "2024-01",
        decision_count: 150,
        decision_limit: 10000,
        agent_count: 5,
        agent_limit: 20,
      };
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: usage }));

      const result = await client.getUsage();

      expect(result).toEqual(usage);
      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      expect(lastCall[0]).toBe("http://localhost:8080/v1/usage");
      expect(lastCall[1].method).toBe("GET");
    });
  });

  // -----------------------------------------------------------------------
  // Health (no auth)
  // -----------------------------------------------------------------------

  describe("health", () => {
    it("returns health without authentication", async () => {
      const healthResp: HealthResponse = {
        status: "healthy",
        version: "0.1.0",
        postgres: "connected",
        qdrant: "connected",
        uptime_seconds: 3600,
      };
      // No token mock — health does not require auth.
      mockFetch.mockResolvedValueOnce(mockResponse(200, healthResp));

      const result = await client.health();

      expect(result).toEqual(healthResp);
      // Only one fetch call (no auth token request).
      expect(mockFetch).toHaveBeenCalledTimes(1);
      const lastCall = mockFetch.mock.calls[0];
      expect(lastCall[0]).toBe("http://localhost:8080/health");
      expect(lastCall[1].method).toBe("GET");
      // No Authorization header.
      expect(lastCall[1].headers).toBeUndefined();
    });

    it("returns health without qdrant field", async () => {
      const healthResp: HealthResponse = {
        status: "healthy",
        version: "0.1.0",
        postgres: "connected",
        uptime_seconds: 120,
      };
      mockFetch.mockResolvedValueOnce(mockResponse(200, healthResp));

      const result = await client.health();

      expect(result.qdrant).toBeUndefined();
      expect(result.status).toBe("healthy");
    });

    it("throws on server error from health endpoint", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(503, { error: { message: "DB down" } }, false),
      );

      await expect(client.health()).rejects.toThrow(ServerError);
    });
  });

  // -----------------------------------------------------------------------
  // Error handling
  // -----------------------------------------------------------------------

  describe("error handling", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("maps 400 to ValidationError", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(400, { error: { message: "Invalid field" } }, false),
      );

      await expect(client.check("x")).rejects.toSatisfy((err: unknown) => {
        return (
          err instanceof ValidationError && err.message === "Invalid field"
        );
      });
    });

    it("maps 401 to AuthenticationError", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(401, { error: { message: "Bad token" } }, false),
      );

      await expect(client.check("x")).rejects.toThrow(AuthenticationError);
    });

    it("maps 403 to AuthorizationError", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(403, { error: { message: "No access" } }, false),
      );

      await expect(client.check("x")).rejects.toThrow(AuthorizationError);
    });

    it("maps 404 to NotFoundError", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(404, { error: { message: "Not found" } }, false),
      );

      await expect(client.check("x")).rejects.toThrow(NotFoundError);
    });

    it("maps 409 to ConflictError", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(409, { error: { message: "Duplicate" } }, false),
      );

      await expect(client.check("x")).rejects.toThrow(ConflictError);
    });

    it("maps 429 to RateLimitError", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(429, { error: { message: "Too many requests" } }, false),
      );

      await expect(client.check("x")).rejects.toSatisfy((err: unknown) => {
        return (
          err instanceof RateLimitError &&
          err.message === "Too many requests" &&
          err.statusCode === 429
        );
      });
    });

    it("maps 429 with default message", async () => {
      mockFetch.mockResolvedValueOnce(mockResponse(429, {}, false));

      await expect(client.check("x")).rejects.toSatisfy((err: unknown) => {
        return (
          err instanceof RateLimitError &&
          err.message === "Rate limit exceeded"
        );
      });
    });

    it("maps 5xx to ServerError", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(503, { error: { message: "Overloaded" } }, false),
      );

      await expect(client.check("x")).rejects.toThrow(ServerError);
    });

    it("maps unknown 4xx to AkashiError", async () => {
      mockFetch.mockResolvedValueOnce(mockResponse(418, {}, false));

      await expect(client.check("x")).rejects.toThrow(AkashiError);
    });

    it("uses fallback message when body has no error", async () => {
      mockFetch.mockResolvedValueOnce(mockResponse(401, {}, false));

      try {
        await client.check("x");
      } catch (e) {
        expect(e).toBeInstanceOf(AuthenticationError);
        expect((e as Error).message).toBe("Authentication failed");
      }
    });
  });
});

describe("withAkashi middleware", () => {
  beforeEach(() => {
    mockFetch.mockReset();
  });

  it("calls check, executes function, and traces result", async () => {
    // Token, check, trace.
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
      .mockResolvedValueOnce(
        mockResponse(200, {
          data: { has_precedent: false, precedents: [] },
        }),
      )
      .mockResolvedValueOnce(
        mockResponse(200, { data: { run_id: "r", decision_id: "d" } }),
      );

    const client = new AkashiClient({
      baseUrl: "http://localhost:8080",
      agentId: "agent",
      apiKey: "key",
    });

    const result = await withAkashi(
      client,
      "model_selection",
      async (precedents: CheckResponse) => {
        expect(precedents.has_precedent).toBe(false);
        return {
          value: "gpt-4o",
          toTrace: (): TraceRequest => ({
            decisionType: "model_selection",
            outcome: "chose gpt-4o",
            confidence: 0.85,
          }),
        };
      },
    );

    expect(result.value).toBe("gpt-4o");
    expect(mockFetch).toHaveBeenCalledTimes(3);

    // Verify trace was called with correct body.
    const traceCall = mockFetch.mock.calls[2];
    expect(traceCall[0]).toBe("http://localhost:8080/v1/trace");
    const body = JSON.parse(traceCall[1].body);
    expect(body.decision.decision_type).toBe("model_selection");
    expect(body.decision.outcome).toBe("chose gpt-4o");
  });

  it("does not trace if function throws", async () => {
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
      .mockResolvedValueOnce(
        mockResponse(200, { data: { has_precedent: false, precedents: [] } }),
      );

    const client = new AkashiClient({
      baseUrl: "http://localhost:8080",
      agentId: "agent",
      apiKey: "key",
    });

    await expect(
      withAkashi(client, "test", async () => {
        throw new Error("boom");
      }),
    ).rejects.toThrow("boom");

    // Only token + check, no trace.
    expect(mockFetch).toHaveBeenCalledTimes(2);
  });
});

describe("error classes", () => {
  it("AkashiError sets name and statusCode", () => {
    const err = new AkashiError("test", 500);
    expect(err.name).toBe("AkashiError");
    expect(err.statusCode).toBe(500);
    expect(err.message).toBe("test");
  });

  it("AuthenticationError defaults message", () => {
    const err = new AuthenticationError();
    expect(err.message).toBe("Authentication failed");
    expect(err.statusCode).toBe(401);
  });

  it("RateLimitError defaults message", () => {
    const err = new RateLimitError();
    expect(err.message).toBe("Rate limit exceeded");
    expect(err.statusCode).toBe(429);
    expect(err.name).toBe("RateLimitError");
  });

  it("RateLimitError accepts custom message", () => {
    const err = new RateLimitError("slow down");
    expect(err.message).toBe("slow down");
    expect(err.statusCode).toBe(429);
  });

  it("ServerError takes status code", () => {
    const err = new ServerError(502, "Bad gateway");
    expect(err.statusCode).toBe(502);
    expect(err.message).toBe("Bad gateway");
  });
});
