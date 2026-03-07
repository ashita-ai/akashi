import { test, expect } from "@playwright/test";

const EXPIRES_AT = new Date(Date.now() + 24 * 60 * 60 * 1000).toISOString();

function setupAuth(page: import("@playwright/test").Page) {
  return page.evaluate((expiresAt: string) => {
    localStorage.setItem("akashi_token", "tok_test_123");
    localStorage.setItem("akashi_agent_id", "admin");
    localStorage.setItem("akashi_expires_at", expiresAt);
  }, EXPIRES_AT);
}

const sampleDecision = {
  id: "d-001",
  run_id: "r-001",
  agent_id: "coder",
  org_id: "org-1",
  decision_type: "architecture",
  outcome: "Use PostgreSQL for persistence layer",
  confidence: 0.92,
  reasoning: "Best fit for relational + vector queries",
  completeness_score: 0.85,
  outcome_score: null,
  precedent_ref: null,
  valid_from: "2025-06-01T00:00:00Z",
  valid_to: null,
  transaction_time: "2025-06-01T00:00:00Z",
  created_at: "2025-06-01T00:00:00Z",
  metadata: null,
};

test.describe("Decisions list", () => {
  test("displays decisions in a table with pagination", async ({ page }) => {
    // Seed localStorage auth before navigating.
    await page.goto("/login");
    await setupAuth(page);

    // Mock API routes.
    await page.route("**/v1/query", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          data: [sampleDecision],
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
          data: { agents: [{ agent_id: "coder", name: "Coder" }] },
          meta: { request_id: "r1", timestamp: new Date().toISOString() },
        }),
      }),
    );
    await page.route("**/v1/subscribe*", (route) => route.abort());

    await page.goto("/decisions");

    // Table should render with the decision.
    await expect(page.getByText("Use PostgreSQL for persistence")).toBeVisible();
    await expect(page.getByText("coder")).toBeVisible();
    await expect(page.getByText("architecture")).toBeVisible();

    // Pagination info.
    await expect(page.getByText(/Showing 1/)).toBeVisible();
  });

  test("shows empty state when no decisions exist", async ({ page }) => {
    await page.goto("/login");
    await setupAuth(page);

    await page.route("**/v1/query", (route) =>
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

    await page.goto("/decisions");

    await expect(page.getByText("No decisions found.")).toBeVisible();
  });
});
