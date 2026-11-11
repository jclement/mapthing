// Logic of the Logs page, kept apart from the component so it can be tested: the filter and
// its URL form, the time ranges, and how a page of history and a live stream are merged.
import type { LogEntry, LogLevel } from "../api";

export type RangeKey = "live" | "1h" | "24h" | "all" | "custom";

export interface LogFilter {
  sources: string[]; // empty = every source
  level: LogLevel | ""; // this level and above; "" = everything
  q: string;
  job: string; // build job id; "" = any
  range: RangeKey;
  since: string; // ISO, used when range is "custom"
  until: string; // ISO, used when range is "custom"
}

export const EMPTY_FILTER: LogFilter = {
  sources: [],
  level: "",
  q: "",
  job: "",
  range: "live",
  since: "",
  until: "",
};

const LEVELS: (LogLevel | "")[] = ["", "debug", "info", "warn", "error"];
const RANGES: RangeKey[] = ["live", "1h", "24h", "all", "custom"];

/** The filter a page URL describes, e.g. /logs?job=12&level=warn or ?source=renderer. */
export function parseFilter(p: URLSearchParams): LogFilter {
  const level = p.get("level") ?? "";
  const since = validIso(p.get("since"));
  const until = validIso(p.get("until"));
  let range = (p.get("range") ?? "") as RangeKey;
  if (!RANGES.includes(range)) range = since || until ? "custom" : p.get("job") ? "all" : "live";
  return {
    sources: p
      .getAll("source")
      .flatMap((s) => s.split(","))
      .map((s) => s.trim())
      .filter(Boolean),
    level: LEVELS.includes(level as LogLevel) ? (level as LogLevel) : "",
    q: p.get("q") ?? "",
    job: p.get("job") ?? "",
    range,
    since: range === "custom" ? since : "",
    until: range === "custom" ? until : "",
  };
}

function validIso(v: string | null): string {
  if (!v) return "";
  const t = Date.parse(v);
  return Number.isNaN(t) ? "" : new Date(t).toISOString();
}

/** The page URL's query for a filter (defaults left out, so a plain /logs stays plain). */
export function filterToSearch(f: LogFilter): URLSearchParams {
  const p = new URLSearchParams();
  if (f.sources.length) p.set("source", f.sources.join(","));
  if (f.level) p.set("level", f.level);
  if (f.q) p.set("q", f.q);
  if (f.job) p.set("job", f.job);
  const dflt = f.job ? "all" : "live";
  if (f.range !== dflt) p.set("range", f.range);
  if (f.range === "custom") {
    if (f.since) p.set("since", f.since);
    if (f.until) p.set("until", f.until);
  }
  return p;
}

/** The time window a filter asks the server for. Relative ranges are resolved against now. */
export function timeWindow(f: LogFilter, now = Date.now()): { since?: string; until?: string } {
  switch (f.range) {
    case "1h":
      return { since: new Date(now - 3600_000).toISOString() };
    case "24h":
      return { since: new Date(now - 86400_000).toISOString() };
    case "custom":
      return { since: f.since || undefined, until: f.until || undefined };
    default:
      return {};
  }
}

/** Whether new lines can still arrive in this view (it has no end, or the end is ahead). */
export function follows(f: LogFilter, now = Date.now()): boolean {
  if (f.range !== "custom" || !f.until) return true;
  return Date.parse(f.until) > now;
}

/** Query string for the logs API (list, stream and download share it). */
export function apiQuery(
  f: LogFilter,
  extra: Record<string, string | undefined> = {},
  now = Date.now(),
): string {
  const p = new URLSearchParams();
  if (f.sources.length) p.set("source", f.sources.join(","));
  if (f.level) p.set("level", f.level);
  if (f.q.trim()) p.set("q", f.q.trim());
  if (f.job) p.set("job", f.job);
  const w = timeWindow(f, now);
  if (w.since) p.set("since", w.since);
  if (w.until) p.set("until", w.until);
  for (const [k, v] of Object.entries(extra)) if (v) p.set(k, v);
  const s = p.toString();
  return s ? `?${s}` : "";
}

/** Ids are nanosecond timestamps as decimal strings: compare by length, then text. */
export function compareIds(a: string, b: string): number {
  return a.length !== b.length ? a.length - b.length : a < b ? -1 : a > b ? 1 : 0;
}

/**
 * Add entries to a list kept oldest first, without duplicates (the stream replays the lines
 * between the history request and the connection). When the list outgrows cap, the oldest are
 * dropped (`keep: "new"`, a live tail) or the newest (`keep: "old"`, paging back).
 */
export function mergeEntries(
  have: LogEntry[],
  add: LogEntry[],
  cap: number,
  keep: "new" | "old" = "new",
): LogEntry[] {
  if (add.length === 0) return have;
  const last = have[have.length - 1];
  let out: LogEntry[];
  if (!last || compareIds(add[0].id, last.id) > 0) {
    // The common case: a live batch strictly after what is shown.
    out = have.concat(add);
  } else {
    const seen = new Set(have.map((e) => e.id));
    out = have.concat(add.filter((e) => !seen.has(e.id)));
    out.sort((a, b) => compareIds(a.id, b.id));
  }
  if (out.length > cap) out = keep === "new" ? out.slice(out.length - cap) : out.slice(0, cap);
  return out;
}

/** A stable colour slot (1 to 5) for a source name, so each keeps its colour. */
export function sourceSlot(source: string): number {
  if (source === "mapthing") return 0;
  let h = 0;
  for (let i = 0; i < source.length; i++) h = (h * 31 + source.charCodeAt(i)) >>> 0;
  return (h % 5) + 1;
}

/** Time as shown beside a line: the clock time, with the date when it is not today. */
export function lineTime(iso: string, now = new Date()): string {
  const d = new Date(iso);
  const t = d.toLocaleTimeString(undefined, { hour12: false });
  if (d.toDateString() === now.toDateString()) return t;
  return `${d.toLocaleDateString(undefined, { month: "short", day: "numeric" })} ${t}`;
}

/** datetime-local value (local time, minute precision) for an ISO time, and back. */
export function toLocalInput(iso: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}
export function fromLocalInput(v: string): string {
  if (!v) return "";
  const t = new Date(v).getTime();
  return Number.isNaN(t) ? "" : new Date(t).toISOString();
}
