"use client";

import { Monitor, Moon, Sun } from "lucide-react";
import { flushSync } from "react-dom";

import { Button } from "@/components/ui/button";
import { persistPreference } from "@/lib/preferences/preferences-storage";
import { usePreferencesStore } from "@/stores/preferences/preferences-provider";

const THEME_CYCLE = ["light", "dark", "system"] as const;

export function ThemeSwitcher() {
  const themeMode = usePreferencesStore((s) => s.themeMode);
  const setThemeMode = usePreferencesStore((s) => s.setThemeMode);

  const cycleTheme = () => {
    const currentIndex = THEME_CYCLE.indexOf(themeMode);
    const nextTheme = THEME_CYCLE[(currentIndex + 1) % THEME_CYCLE.length];

    const applyTheme = () => {
      setThemeMode(nextTheme);
      void persistPreference("theme_mode", nextTheme);
    };

    if (
      typeof document.startViewTransition === "function" &&
      !window.matchMedia("(prefers-reduced-motion: reduce)").matches
    ) {
      document.startViewTransition(() => flushSync(applyTheme));
    } else {
      applyTheme();
    }
  };

  return (
    <Button
      size="icon"
      onClick={cycleTheme}
      aria-label={`Current theme: ${themeMode}. Click to cycle themes`}
      className="*:animate-in *:fade-in *:spin-in-90 *:zoom-in-50 *:duration-300 motion-reduce:*:animate-none"
    >
      {/* SYSTEM */}
      <Monitor className="hidden [html[data-theme-mode=system]_&]:block" />

      {/* DARK (resolved) */}
      <Sun className="hidden dark:block [html[data-theme-mode=system]_&]:hidden" />

      {/* LIGHT (resolved) */}
      <Moon className="block dark:hidden [html[data-theme-mode=system]_&]:hidden" />
    </Button>
  );
}
