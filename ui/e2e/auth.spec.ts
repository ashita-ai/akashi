import { test, expect } from "@playwright/test";

test.describe("Authentication flow", () => {
  test("redirects unauthenticated users to /login", async ({ page }) => {
    await page.goto("/");
    await expect(page).toHaveURL(/\/login/);
  });

  test("login page renders sign-in form", async ({ page }) => {
    await page.goto("/login");
    await expect(page.getByText("Sign in")).toBeVisible();
    await expect(page.getByLabel("Agent ID")).toBeVisible();
    await expect(page.getByLabel("API Key")).toBeVisible();
    await expect(page.getByRole("button", { name: /sign in/i })).toBeVisible();
  });

  test("shows error on invalid credentials", async ({ page }) => {
    await page.goto("/login");

    // Mock the auth endpoint to return 401.
    await page.route("**/auth/token", (route) =>
      route.fulfill({
        status: 401,
        contentType: "application/json",
        body: JSON.stringify({
          error: { code: "UNAUTHORIZED", message: "Invalid credentials" },
          meta: { request_id: "r1", timestamp: new Date().toISOString() },
        }),
      }),
    );

    await page.getByLabel("Agent ID").fill("bad-agent");
    await page.getByLabel("API Key").fill("bad-key");
    await page.getByRole("button", { name: /sign in/i }).click();

    await expect(page.getByRole("alert")).toContainText("Invalid credentials");
  });

  test("successful login redirects to dashboard", async ({ page }) => {
    await page.goto("/login");

    const expiresAt = new Date(
      Date.now() + 24 * 60 * 60 * 1000,
    ).toISOString();

    // Mock auth endpoint.
    await page.route("**/auth/token", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          data: { token: "tok_test_123", expires_at: expiresAt },
          meta: { request_id: "r1", timestamp: new Date().toISOString() },
        }),
      }),
    );

    // Mock dashboard API calls so the page loads.
    await page.route("**/v1/decisions/recent*", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          data: [],
          total: 0,
          has_more: false,
          limit: 10,
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
    await page.route("**/v1/trace-health*", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
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
            gaps: [],
          },
          meta: { request_id: "r1", timestamp: new Date().toISOString() },
        }),
      }),
    );
    await page.route("**/v1/subscribe*", (route) => route.abort());

    await page.getByLabel("Agent ID").fill("admin");
    await page.getByLabel("API Key").fill("valid-key");
    await page.getByRole("button", { name: /sign in/i }).click();

    // After login, window.location.replace("/") fires, so we end up at "/".
    await page.waitForURL("/");
    await expect(page.getByText("Dashboard")).toBeVisible();
  });
});
