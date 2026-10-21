import { useSyncExternalStore } from "react";
import { createListeners } from "../utils/listeners";

export type ThemeMode = "light" | "dark" | "system";
export type Season = "spring" | "summer" | "autumn" | "winter";
export type SeasonChoice = Season | "auto";
export type Appearance = { theme: ThemeMode; season: SeasonChoice };

const STORAGE_KEY = "msime-admin-theme";
const themes: readonly ThemeMode[] = ["light", "dark", "system"];
const seasons: readonly SeasonChoice[] = ["auto", "spring", "summer", "autumn", "winter"];

export const seasonNames: Record<Season, string> = { spring: "春", summer: "夏", autumn: "秋", winter: "冬" };

// Auto season follows the month: 3–5 spring, 6–8 summer, 9–11 autumn, 12–2 winter.
export function seasonForDate(date: Date): Season {
  const month = date.getMonth() + 1;
  return month >= 3 && month <= 5 ? "spring" : month >= 6 && month <= 8 ? "summer" : month >= 9 && month <= 11 ? "autumn" : "winter";
}

export function resolveSeason(choice: SeasonChoice): Season {
  return choice === "auto" ? seasonForDate(new Date()) : choice;
}

function readStored(): Appearance {
  try {
    const raw: unknown = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? "null");
    if (raw && typeof raw === "object") {
      const value = raw as Record<string, unknown>;
      return {
        theme: themes.includes(value.theme as ThemeMode) ? value.theme as ThemeMode : "light",
        season: seasons.includes(value.season as SeasonChoice) ? value.season as SeasonChoice : "auto",
      };
    }
  } catch {
    // Unreadable storage (private mode, corrupt JSON) falls back to the defaults below.
  }
  return { theme: "light", season: "auto" };
}

let current: Appearance = { theme: "light", season: "auto" };
const listeners = createListeners();
let media: MediaQueryList | null = null;

function apply() {
  const dark = current.theme === "dark" || (current.theme === "system" && Boolean(media?.matches));
  const root = document.documentElement;
  root.dataset.theme = dark ? "dark" : "light";
  root.dataset.season = resolveSeason(current.season);
}

// refreshSeason re-applies the auto season when the month has moved it on; the snapshot gets a new identity so the appearance popover re-renders its label.
function refreshSeason() {
  if (current.season !== "auto" || document.documentElement.dataset.season === resolveSeason("auto")) return;
  current = { ...current };
  apply();
  listeners.emit();
}

const DAY_MS = 24 * 60 * 60 * 1000;

// scheduleSeasonCheck wakes up just after the next local month starts, or within a day because setTimeout cannot wait a whole month (its delay overflows past about 24.8 days).
function scheduleSeasonCheck() {
  const now = new Date();
  const nextMonth = new Date(now.getFullYear(), now.getMonth() + 1, 1);
  window.setTimeout(() => { refreshSeason(); scheduleSeasonCheck(); }, Math.min(nextMonth.getTime() - now.getTime() + 1000, DAY_MS));
}

// initAppearance runs before the first render so the stored theme is applied without a flash; system mode follows prefers-color-scheme changes live, and the auto season follows the month in a tab that stays open across a month boundary (also checked when a sleeping tab becomes visible again, since timers are paused then).
export function initAppearance() {
  current = readStored();
  media = window.matchMedia("(prefers-color-scheme: dark)");
  media.addEventListener("change", () => { apply(); listeners.emit(); });
  document.addEventListener("visibilitychange", () => { if (document.visibilityState === "visible") refreshSeason(); });
  scheduleSeasonCheck();
  apply();
}

export function setAppearance(patch: Partial<Appearance>) {
  current = { ...current, ...patch };
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(current));
  } catch {
    // Storage may be unavailable; the choice still applies for this page view.
  }
  apply();
  listeners.emit();
}

export function useAppearance(): Appearance {
  return useSyncExternalStore(listeners.subscribe, () => current);
}
