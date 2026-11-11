// The Settings page's logic: grouping, and what to show for a value.
import type { Setting } from "../api";

export const UNRECOGNISED = "Not recognised";

export interface SettingGroup {
  name: string;
  settings: Setting[];
  changed: number; // how many are set in the environment
}

/** Settings in the server's order, grouped; with `onlySet`, only those set in the environment. */
export function groupSettings(settings: Setting[], onlySet = false): SettingGroup[] {
  const groups: SettingGroup[] = [];
  for (const s of settings) {
    let g = groups.find((x) => x.name === s.group);
    if (!g) {
      g = { name: s.group, settings: [], changed: 0 };
      groups.push(g);
    }
    if (s.source === "env") g.changed++;
    if (!onlySet || s.source === "env") g.settings.push(s);
  }
  // Mistakes first: an unrecognised variable is the one thing here that needs doing.
  groups.sort((a, b) => Number(b.name === UNRECOGNISED) - Number(a.name === UNRECOGNISED));
  return groups.filter((g) => g.settings.length > 0);
}

/** What to print for a setting's value. Secrets are never sent by the server. */
export function shownValue(s: Setting): { text: string; muted: boolean } {
  if (s.secret) {
    if (s.set) return { text: s.value || "••••••••", muted: false };
    return { text: s.default ? `not set (${s.default})` : "not set", muted: true };
  }
  if (s.source === "env") return { text: s.value, muted: false };
  if (s.source === "image") return { text: s.value, muted: true };
  return { text: s.value || s.default || "not set", muted: true };
}

export function countSet(settings: Setting[]): number {
  return settings.filter((s) => s.source === "env").length;
}
