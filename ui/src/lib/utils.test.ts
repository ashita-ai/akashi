import { describe, it, expect, vi, afterEach } from "vitest";
import { cn, formatDate, formatRelativeTime, truncate } from "./utils";

describe("cn", () => {
  it("merges class names", () => {
    expect(cn("px-2", "py-1")).toBe("px-2 py-1");
  });

  it("resolves tailwind conflicts (last wins)", () => {
    expect(cn("px-2", "px-4")).toBe("px-4");
  });

  it("handles conditional classes", () => {
    expect(cn("base", false && "hidden", "extra")).toBe("base extra");
  });

  it("returns empty string for no inputs", () => {
    expect(cn()).toBe("");
  });
});

describe("formatDate", () => {
  it("formats a Date object", () => {
    const d = new Date("2025-06-15T14:30:00Z");
    const result = formatDate(d);
    // The exact format depends on locale, but it should contain the year and month.
    expect(result).toContain("2025");
    expect(result).toContain("Jun");
  });

  it("formats an ISO string", () => {
    // Use a mid-month date to avoid timezone-dependent date boundary shifts.
    const result = formatDate("2025-06-15T12:00:00Z");
    expect(result).toContain("2025");
    expect(result).toContain("Jun");
  });
});

describe("formatRelativeTime", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('returns "just now" for times less than 60s ago', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2025-06-15T12:00:30Z"));
    expect(formatRelativeTime("2025-06-15T12:00:00Z")).toBe("just now");
  });

  it("returns minutes ago", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2025-06-15T12:05:00Z"));
    expect(formatRelativeTime("2025-06-15T12:00:00Z")).toBe("5m ago");
  });

  it("returns hours ago", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2025-06-15T15:00:00Z"));
    expect(formatRelativeTime("2025-06-15T12:00:00Z")).toBe("3h ago");
  });

  it("returns days ago", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2025-06-17T12:00:00Z"));
    expect(formatRelativeTime("2025-06-15T12:00:00Z")).toBe("2d ago");
  });

  it("falls back to formatDate for times older than 7 days", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2025-06-30T12:00:00Z"));
    const result = formatRelativeTime("2025-06-15T12:00:00Z");
    expect(result).toContain("Jun");
    expect(result).toContain("2025");
  });
});

describe("truncate", () => {
  it("returns the string unchanged if within limit", () => {
    expect(truncate("hello", 10)).toBe("hello");
  });

  it("returns the string unchanged if exactly at limit", () => {
    expect(truncate("hello", 5)).toBe("hello");
  });

  it("truncates and appends ellipsis", () => {
    const result = truncate("hello world", 6);
    expect(result).toBe("hello\u2026");
    expect(result.length).toBe(6);
  });

  it("handles empty string", () => {
    expect(truncate("", 5)).toBe("");
  });
});
