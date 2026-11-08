import { CronExpressionParser } from "cron-parser";
import cronstrue from "cronstrue";

export const CRON_PRESETS = [
  { id: "daily", label: "Daily", cron: "0 3 * * *" },
  { id: "weekly", label: "Weekly", cron: "0 3 * * 0" },
  { id: "monthly", label: "Monthly", cron: "0 3 1 * *" },
] as const;

export type CronPreview =
  | { ok: true; description: string; next: Date[] }
  | { ok: false; error: string };

/** Describe a 5-field cron expression (evaluated in UTC) and list its next runs. */
export function previewCron(expr: string, count = 3, from: Date = new Date()): CronPreview {
  const e = expr.trim().replace(/\s+/g, " ");
  if (e.split(" ").length !== 5) {
    return { ok: false, error: "Use five fields: minute hour day-of-month month day-of-week" };
  }
  try {
    const it = CronExpressionParser.parse(e, { tz: "UTC", currentDate: from });
    const next: Date[] = [];
    for (let i = 0; i < count; i++) next.push(it.next().toDate());
    const description = cronstrue.toString(e, { use24HourTimeFormat: true, verbose: false });
    return { ok: true, description: `${description} (UTC)`, next };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : "Invalid expression" };
  }
}

export function presetFor(expr: string): string {
  const e = expr.trim().replace(/\s+/g, " ");
  return CRON_PRESETS.find((p) => p.cron === e)?.id ?? "custom";
}
