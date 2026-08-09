import { useCallback, useEffect, useState } from "react";

/**
 * Theme is OS-driven by default (`prefers-color-scheme`, handled entirely in
 * CSS) with an optional explicit override the sidebar toggle sets. The
 * override persists across visits; the absence of one — most operators,
 * most of the time — leaves the OS preference in charge.
 */
const STORAGE_KEY = "atlas.theme";

type Theme = "dark" | "light";

function systemPrefersDark(): boolean {
  return window.matchMedia("(prefers-color-scheme: dark)").matches;
}

function currentTheme(): Theme {
  const stored = localStorage.getItem(STORAGE_KEY);
  if (stored === "dark" || stored === "light") return stored;
  return systemPrefersDark() ? "dark" : "light";
}

function applyTheme(theme: Theme | null) {
  if (theme) {
    document.documentElement.setAttribute("data-theme", theme);
  } else {
    document.documentElement.removeAttribute("data-theme");
  }
}

export function useTheme(): { theme: Theme; toggle: () => void } {
  const [theme, setTheme] = useState<Theme>(() => currentTheme());

  useEffect(() => {
    const stored = localStorage.getItem(STORAGE_KEY);
    applyTheme(stored === "dark" || stored === "light" ? stored : null);
  }, []);

  const toggle = useCallback(() => {
    setTheme((prev) => {
      const next: Theme = prev === "dark" ? "light" : "dark";
      localStorage.setItem(STORAGE_KEY, next);
      applyTheme(next);
      return next;
    });
  }, []);

  return { theme, toggle };
}
