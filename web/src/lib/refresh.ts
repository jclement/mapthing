// Per-region refresh: what a setting means, in words, and (for the mock backend) how a
// region's own setting resolves against the default schedule. The server does the resolving
// for real (internal/scheduler); this mirrors it.
import { CronExpressionParser } from "cron-parser";
import cronstrue from "cronstrue";
import type { RefreshMode, RefreshPolicy, Schedule } from "../api";
import * as fmt from "./format";

export const MODE_CRON: Partial<Record<RefreshMode, string>> = {
  daily: "0 3 * * *",
  weekly: "0 3 * * 0",
  monthly: "0 3 1 * *",
};

export const MODE_LABEL: Record<RefreshMode, string> = {
  default: "Default schedule",
  never: "Never",
  daily: "Daily",
  weekly: "Weekly",
  monthly: "Monthly",
  cron: "Custom",
};

export const MODE_HELP: Record<RefreshMode, string> = {
  default: "Follow the default schedule above.",
  daily: "Every day at 03:00 UTC.",
  weekly: "Every Sunday at 03:00 UTC.",
  monthly: "On the 1st of each month at 03:00 UTC.",
  cron: "A cron expression of its own.",
  never: "Keep the data it has. It is still built into every generation.",
};

/** A cron expression in words, or the expression itself if it cannot be described. */
export function cronWords(expr: string): string {
  try {
    return cronstrue.toString(expr, { use24HourTimeFormat: true, verbose: false });
  } catch {
    return expr;
  }
}

/** "22:00-06:00" → "22:00–06:00 UTC"; "" → "". */
export function windowWords(w: string | undefined): string {
  return w ? `${w.replace("-", "–")} UTC` : "";
}

export function validWindow(w: string): boolean {
  if (w === "") return true;
  const m = /^(\d{1,2}):(\d{2})-(\d{1,2}):(\d{2})$/.exec(w.trim());
  if (!m) return false;
  const [a, b, c, d] = m.slice(1).map(Number);
  return a < 24 && c < 24 && b < 60 && d < 60 && (a !== c || b !== d);
}

/** One line for a region's setting: "Weekly (default schedule)", "Daily, 22:00–06:00 UTC". */
export function policySummary(p: RefreshPolicy | undefined): string {
  if (!p) return "—";
  if (p.mode === "never") return "Never";
  if (!p.effective.cron) return "Not scheduled (the default schedule is off)";
  const when =
    p.mode === "default" || p.mode === "cron" ? cronWords(p.effective.cron) : MODE_LABEL[p.mode];
  const parts = [p.mode === "default" ? `${when} (default schedule)` : when];
  if (p.effective.window) parts.push(`builds ${windowWords(p.effective.window)}`);
  return parts.join(", ");
}

/** When the region is next refreshed, in words. */
export function nextRefresh(p: RefreshPolicy | undefined, now = Date.now()): string {
  if (!p?.effective.cron || !p.next_due) return "never";
  if (p.waiting && p.next_build)
    return `due, waiting for its build window (${fmt.relTime(p.next_build, now)})`;
  if (p.due) return "due now";
  return fmt.relTime(p.next_build ?? p.next_due, now);
}

/** What the last look at the source found, in words. */
export function lastCheck(p: RefreshPolicy | undefined, now = Date.now()): string {
  if (!p?.last_checked_at) return "not checked yet";
  const when = fmt.relTime(p.last_checked_at, now);
  switch (p.last_check) {
    case "changed":
      return `${when}: newer data available`;
    case "unchanged":
      return `${when}: nothing new`;
    case "error":
      return `${when}: could not check`;
    default:
      return when;
  }
}

/** What a refresh of this region costs. A build is always over all regions together. */
export function refreshCost(
  cost: { download_bytes: number; build_seconds: number | null } | undefined,
) {
  if (!cost) return "—";
  const dl = cost.download_bytes
    ? `${fmt.bytes(cost.download_bytes)} download`
    : "download size unknown";
  return cost.build_seconds
    ? `${dl} + ~${fmt.duration(cost.build_seconds)} build`
    : `${dl} + a build`;
}

/** Whether `at` (UTC) is inside "HH:MM-HH:MM"; an empty window is always open. */
export function inWindow(w: string, at: Date): boolean {
  if (!w) return true;
  const [a, b] = w.split("-").map((x) => {
    const [h, m] = x.split(":").map(Number);
    return h * 60 + m;
  });
  const t = at.getUTCHours() * 60 + at.getUTCMinutes();
  return a < b ? t >= a && t < b : t >= a || t < b;
}

function nextOpen(w: string, at: Date): Date {
  if (inWindow(w, at)) return at;
  const [h, m] = w.split("-")[0].split(":").map(Number);
  const open = new Date(Date.UTC(at.getUTCFullYear(), at.getUTCMonth(), at.getUTCDate(), h, m));
  if (open.getTime() <= at.getTime()) open.setUTCDate(open.getUTCDate() + 1);
  return open;
}

/** A region's own setting, as stored. */
export interface OwnPolicy {
  mode: RefreshMode;
  cron?: string;
  window?: string;
  only_changed: boolean;
}

/**
 * Resolve a region's own setting against the default schedule, as the server does:
 * `handledAt` is when the region's refresh was last decided; its next refresh is the first
 * schedule time after that.
 */
export function resolvePolicy(
  own: OwnPolicy,
  def: Pick<Schedule, "cron" | "enabled" | "window" | "only_changed">,
  handledAt: Date,
  now: Date,
  check?: Pick<RefreshPolicy, "last_checked_at" | "last_check" | "last_check_detail">,
): RefreshPolicy {
  const inherited = own.mode === "default";
  let cron: string | null = null;
  if (own.mode === "default") cron = def.enabled ? def.cron : null;
  else if (own.mode === "cron") cron = own.cron ?? null;
  else if (own.mode !== "never") cron = MODE_CRON[own.mode] ?? null;
  const window =
    own.mode === "never" ? "" : inherited ? (def.window ?? "") : own.window || (def.window ?? "");
  const p: RefreshPolicy = {
    mode: own.mode,
    cron: own.cron,
    window: own.window,
    only_changed: own.only_changed,
    effective: {
      cron,
      window,
      only_changed: inherited ? (def.only_changed ?? true) : own.only_changed,
      inherited,
    },
    next_due: null,
    next_build: null,
    due: false,
    waiting: false,
    last_checked_at: check?.last_checked_at ?? null,
    last_check: check?.last_check,
    last_check_detail: check?.last_check_detail,
  };
  if (!cron) return p;
  try {
    const next = CronExpressionParser.parse(cron, { tz: "UTC", currentDate: handledAt })
      .next()
      .toDate();
    p.next_due = next.toISOString();
    p.due = next.getTime() <= now.getTime();
    p.next_build = nextOpen(window, p.due ? now : next).toISOString();
    p.waiting = p.due && !inWindow(window, now);
  } catch {
    /* an invalid expression: no next refresh */
  }
  return p;
}
