import { describe, it, expect, vi, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders } from "@/test/test-utils";
import Login from "./Login";

describe("Login", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("renders the sign-in form", () => {
    renderWithProviders(<Login />);

    expect(screen.getByRole("heading", { name: /sign in/i })).toBeInTheDocument();
    expect(screen.getByLabelText("Agent ID")).toBeInTheDocument();
    expect(screen.getByLabelText("API Key")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /sign in/i }),
    ).toBeInTheDocument();
  });

  it("renders the Akashi branding", () => {
    renderWithProviders(<Login />);
    expect(screen.getByText("Akashi")).toBeInTheDocument();
    expect(
      screen.getByText("Decision trace layer for multi-agent AI systems"),
    ).toBeInTheDocument();
  });

  it("shows error on failed login", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 401,
        json: () =>
          Promise.resolve({
            error: { code: "UNAUTHORIZED", message: "Invalid credentials" },
          }),
      }),
    );

    renderWithProviders(<Login />);
    const user = userEvent.setup();

    await user.type(screen.getByLabelText("Agent ID"), "bad-agent");
    await user.type(screen.getByLabelText("API Key"), "bad-key");
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    await waitFor(() => {
      expect(screen.getByRole("alert")).toHaveTextContent(
        "Invalid credentials",
      );
    });
  });

  it("disables button while loading", async () => {
    // Fetch that never resolves to keep loading state.
    vi.stubGlobal(
      "fetch",
      vi.fn().mockReturnValue(new Promise(() => {})),
    );

    renderWithProviders(<Login />);
    const user = userEvent.setup();

    await user.type(screen.getByLabelText("Agent ID"), "agent");
    await user.type(screen.getByLabelText("API Key"), "key");
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /signing in/i }),
      ).toBeDisabled();
    });
  });

  it("has a theme toggle button", () => {
    renderWithProviders(<Login />);
    expect(
      screen.getByRole("button", { name: /switch to dark mode/i }),
    ).toBeInTheDocument();
  });
});
