import { test, expect } from "@playwright/test";

const EXPIRES_AT = new Date(Date.now() + 24 * 60 * 60 * 1000).toISOString();

function setupAuth(page: import("@playwright/test").Page) {
  return page.evaluate((expiresAt: string) => {
    localStorage.setItem("akashi_token", "tok_test_123");
    localStorage.setItem("akashi_agent_id", "admin");
    localStorage.setItem("akashi_expires_at", expiresAt);
  }, EXPIRES_AT);
}

const sampleConflictGroup = {
  id: "cg-001",
  agent_a: "coder",
  agent_b: "reviewer",
  conflict_kind: "cross_agent",
  decision_type: "architecture",
  first_detected_at: "2025-06-01T00:00:00Z",
  last_detected_at: "2025-06-15T00:00:00Z",
  conflict_count: 2,
  open_count: 1,
  representative: {
    id: "c-001",
    conflict_kind: "cross_agent",
    decision_a_id: "d-001",
    decision_b_id: "d-002",
    org_id: "org-1",
    agent_a: "coder",
    agent_b: "reviewer",
    run_a: "r-001",
    run_b: "r-002",
    decision_type: "architecture",
    outcome_a: "Use PostgreSQL",
    outcome_b: "Use MongoDB",
    confidence_a: 0.9,
    confidence_b: 0.7,
    reasoning_a: null,
    reasoning_b: null,
    decided_at_a: "2025-06-01T00:00:00Z",
    decided_at_b: "2025-06-02T00:00:00Z",
    detected_at: "2025-06-02T01:00:00Z",
    explanation: "Agents disagree on the database choice",
    category: "strategic",
    severity: "high",
    status: "open",
    resolved_by: null,
    resolved_at: null,
    resolution_note: null,
  },
  open_conflicts: [
    {
      id: "c-001",
      conflict_kind: "cross_agent",
      decision_a_id: "d-001",
      decision_b_id: "d-002",
      org_id: "org-1",
      agent_a: "coder",
      agent_b: "reviewer",
      run_a: "r-001",
      run_b: "r-002",
      decision_type: "architecture",
      outcome_a: "Use PostgreSQL",
      outcome_b: "Use MongoDB",
      confidence_a: 0.9,
      confidence_b: 0.7,
      reasoning_a: null,
      reasoning_b: null,
      decided_at_a: "2025-06-01T00:00:00Z",
      decided_at_b: "2025-06-02T00:00:00Z",
      detected_at: "2025-06-02T01:00:00Z",
      explanation: "Agents disagree on the database choice",
      category: "strategic",
      severity: "high",
      status: "open",
      resolved_by: null,
      resolved_at: null,
      resolution_note: null,
    },
  ],
};

test.describe("Conflicts page", () => {
  test("displays conflict groups", async ({ page }) => {
    await page.goto("/login");
    await setupAuth(page);

    await page.route("**/v1/conflict-groups*", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          data: [sampleConflictGroup],
          total: 1,
          has_more: false,
          limit: 25,
          offset: 0,
        }),
      }),
    );
    await page.route("**/v1/agents*", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          data: { agents: [] },
          meta: { request_id: "r1", timestamp: new Date().toISOString() },
        }),
      }),
    );
    await page.route("**/v1/subscribe*", (route) => route.abort());

    await page.goto("/conflicts");

    await expect(page.getByText("Conflicts")).toBeVisible();
    await expect(page.getByText("coder")).toBeVisible();
    await expect(page.getByText("reviewer")).toBeVisible();
    await expect(
      page.getByText("Agents disagree on the database choice"),
    ).toBeVisible();
    await expect(page.getByText("High")).toBeVisible();
  });

  test("shows empty state when no conflicts", async ({ page }) => {
    await page.goto("/login");
    await setupAuth(page);

    await page.route("**/v1/conflict-groups*", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          data: [],
          total: 0,
          has_more: false,
          limit: 25,
          offset: 0,
        }),
      }),
    );
    await page.route("**/v1/agents*", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          data: { agents: [] },
          meta: { request_id: "r1", timestamp: new Date().toISOString() },
        }),
      }),
    );
    await page.route("**/v1/subscribe*", (route) => route.abort());

    await page.goto("/conflicts");

    await expect(
      page.getByText("No conflicts detected. Agents are in agreement."),
    ).toBeVisible();
  });

  test("opens adjudication dialog", async ({ page }) => {
    await page.goto("/login");
    await setupAuth(page);

    await page.route("**/v1/conflict-groups*", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          data: [sampleConflictGroup],
          total: 1,
          has_more: false,
          limit: 25,
          offset: 0,
        }),
      }),
    );
    await page.route("**/v1/agents*", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          data: { agents: [] },
          meta: { request_id: "r1", timestamp: new Date().toISOString() },
        }),
      }),
    );
    await page.route("**/v1/subscribe*", (route) => route.abort());

    await page.goto("/conflicts");

    // Click the Adjudicate button on the conflict group card.
    await page.getByRole("button", { name: /adjudicate/i }).first().click();

    // Dialog should open.
    await expect(page.getByText("Adjudicate Conflict")).toBeVisible();
    await expect(page.getByRole("button", { name: /acknowledge/i })).toBeVisible();
    await expect(page.getByRole("button", { name: /resolve/i })).toBeVisible();
  });
});
