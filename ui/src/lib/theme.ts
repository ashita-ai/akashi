import { useEffect, useState } from "react";

type Theme = "light" | "dark";

const STORAGE_KEY = "akashi-theme";

function getSystemTheme(): Theme {
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

function getStoredTheme(): Theme | null {
  const stored = localStorage.getItem(STORAGE_KEY);
  if (stored === "light" || stored === "dark") return stored;
  return null;
}

function applyTheme(theme: Theme) {
  document.documentElement.classList.toggle("dark", theme === "dark");
}

/** Initialize theme on app boot (call once, outside React). */
export function initTheme() {
  const theme = getStoredTheme() ?? getSystemTheme();
  applyTheme(theme);
}

export function useTheme() {
  const [theme, setThemeState] = useState<Theme>(
    () => getStoredTheme() ?? getSystemTheme(),
  );

  useEffect(() => {
    applyTheme(theme);
    localStorage.setItem(STORAGE_KEY, theme);
  }, [theme]);

  function toggle() {
    setThemeState((prev) => (prev === "dark" ? "light" : "dark"));
  }

  return { theme, toggle } as const;
}
