import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { renderWithProviders } from "@/test/test-utils";
import Dashboard from "./Dashboard";

function mockFetchResponses(responses: Record<string, unknown>) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockImplementation((url: string) => {
      for (const [pattern, data] of Object.entries(responses)) {
        if (url.includes(pattern)) {
          return Promise.resolve({
            ok: true,
            json: () => Promise.resolve(data),
          });
        }
      }
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ data: null }),
      });
    }),
  );
}

describe("Dashboard", () => {
  beforeEach(() => {
    // Auth the user so the API module attaches a token.
    localStorage.setItem("akashi_token", "test-token");
    localStorage.setItem("akashi_agent_id", "admin");
    localStorage.setItem(
      "akashi_expires_at",
      new Date(Date.now() + 3600_000).toISOString(),
    );
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("renders dashboard heading", () => {
    mockFetchResponses({});
    renderWithProviders(<Dashboard />);
    expect(screen.getByText("Dashboard")).toBeInTheDocument();
  });

  it("shows metric cards", () => {
    mockFetchResponses({});
    renderWithProviders(<Dashboard />);
    expect(screen.getByText("Decisions")).toBeInTheDocument();
    expect(screen.getByText("Active Agents")).toBeInTheDocument();
    expect(screen.getByText("Open Conflicts")).toBeInTheDocument();
    expect(screen.getByText("Trace Health")).toBeInTheDocument();
  });

  it("displays decision count from API", async () => {
    mockFetchResponses({
      "/v1/decisions/recent": {
        data: [
          {
            id: "d1",
            run_id: "r1",
            agent_id: "coder",
            org_id: "org1",
            decision_type: "architecture",
            outcome: "Use PostgreSQL",
            confidence: 0.9,
            reasoning: null,
            completeness_score: 0.8,
            outcome_score: null,
            precedent_ref: null,
            valid_from: "2025-06-01T00:00:00Z",
            valid_to: null,
            transaction_time: "2025-06-01T00:00:00Z",
            created_at: "2025-06-01T00:00:00Z",
            metadata: null,
          },
        ],
        total: 42,
        has_more: true,
        limit: 10,
        offset: 0,
      },
      "/v1/agents": {
        data: { agents: [{ agent_id: "coder", name: "Coder" }] },
        meta: { request_id: "r1", timestamp: "now" },
      },
      "/v1/trace-health": {
        data: {
          status: "healthy",
          completeness: {
            total_decisions: 42,
            avg_completeness: 0.85,
            below_half: 2,
            below_third: 0,
            with_reasoning: 40,
            reasoning_pct: 95,
            with_alternatives: 30,
            alternatives_pct: 71,
          },
          evidence: {
            total_decisions: 42,
            total_records: 10,
            avg_per_decision: 0.24,
            with_evidence: 10,
            without_evidence: 32,
            coverage_pct: 24,
          },
          gaps: [],
        },
        meta: { request_id: "r1", timestamp: "now" },
      },
    });

    renderWithProviders(<Dashboard />);

    await waitFor(() => {
      expect(screen.getByText("42")).toBeInTheDocument();
    });
  });

  it("shows empty state when no decisions exist", async () => {
    mockFetchResponses({
      "/v1/decisions/recent": {
        data: [],
        total: 0,
        has_more: false,
        limit: 10,
        offset: 0,
      },
      "/v1/agents": {
        data: { agents: [] },
        meta: { request_id: "r1", timestamp: "now" },
      },
      "/v1/trace-health": {
        data: {
          status: "insufficient_data",
          completeness: {
            total_decisions: 0,
            avg_completeness: 0,
            below_half: 0,
            below_third: 0,
            with_reasoning: 0,
            reasoning_pct: 0,
            with_alternatives: 0,
            alternatives_pct: 0,
          },
          evidence: {
            total_decisions: 0,
            total_records: 0,
            avg_per_decision: 0,
            with_evidence: 0,
            without_evidence: 0,
            coverage_pct: 0,
          },
          gaps: ["Start tracing decisions to see health metrics"],
        },
        meta: { request_id: "r1", timestamp: "now" },
      },
    });

    renderWithProviders(<Dashboard />);

    await waitFor(() => {
      expect(
        screen.getByText("No decisions recorded yet."),
      ).toBeInTheDocument();
    });
  });
});
