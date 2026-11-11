// What the Overview page concludes from GET /admin/api/overview, apart from the component.
import type { KindTraffic, Overview, ProcessInfo, UpdateStatus } from "../api";
import * as fmt from "./format";

type Tone = "neutral" | "ok" | "warn" | "bad" | "info" | "accent";

export const KIND_LABEL: Record<string, string> = {
  tiles: "Vector tiles",
  raster: "Raster tiles",
  route: "Routing",
  elevation: "Elevation",
  style: "Styles, fonts, sprites",
  pack: "Offline packs",
};

/** Milliseconds as people read them: 0.4 ms, 12 ms, 1.2 s. */
export function latency(ms: number): string {
  if (!ms) return "—";
  if (ms < 1) return `${ms.toFixed(1)} ms`;
  if (ms < 1000) return `${Math.round(ms)} ms`;
  return `${(ms / 1000).toFixed(ms < 10000 ? 1 : 0)} s`;
}

/** A request rate: per minute, or per second once it is busy. */
export function rate(perMin: number): string {
  if (!perMin) return "0";
  if (perMin >= 120) return `${(perMin / 60).toFixed(perMin >= 600 ? 0 : 1)}/s`;
  if (perMin >= 10) return `${Math.round(perMin)}/min`;
  return `${perMin.toFixed(perMin < 1 ? 2 : 1)}/min`;
}

export function processTone(p: ProcessInfo): Tone {
  if (p.state === "running") return p.healthy ? "ok" : "warn";
  if (p.state === "starting") return "info";
  if (p.state === "restarting") return "bad";
  return "neutral";
}

export function processLabel(p: ProcessInfo): string {
  if (p.standby && p.state === "running") return "warming up";
  if (p.state === "running") return p.healthy ? "healthy" : "not answering";
  if (p.state === "restarting") return "crashed, restarting";
  return p.state;
}

/** A child worth a look: it crashed, is restarting, or is up but not answering its probe. */
export function needsAttention(p: ProcessInfo): boolean {
  return p.state === "restarting" || (p.state === "running" && !p.healthy && !p.standby);
}

/** Where a process's log lines are ("" when it has none of its own). */
export function logLink(p: ProcessInfo): string {
  if (p.name === "build tools") return "/logs?source=build";
  const q = new URLSearchParams({ source: p.name });
  if (p.restarts > 0 || needsAttention(p)) q.set("range", "all");
  return `/logs?${q}`;
}

export interface Headline {
  tone: Tone;
  title: string;
  detail: string;
}

export const UPDATE_COMMAND = "docker compose pull && docker compose up -d";

/** The software's update state in a few words, for the page header ("" = say nothing). */
export function updateLine(u: UpdateStatus | null | undefined, now = Date.now()): string {
  if (!u) return "";
  if (!u.enabled) return `update check off: ${u.reason ?? "not configured"}`;
  if (u.update_available) return "a newer image is available";
  if (u.checked_at) return `up to date (checked ${fmt.relTime(u.checked_at, now)})`;
  if (u.error) return "could not check for a newer image";
  return "not checked for a newer image yet";
}

/** The one-line verdict at the top of the page. */
export function headline(o: Overview): Headline {
  if (!o.generation) {
    if (o.builds.active)
      return {
        tone: "info",
        title: "Building the first map",
        detail: "Nothing is served until this build finishes.",
      };
    return o.regions_installed === 0
      ? {
          tone: "info",
          title: "Nothing to serve yet",
          detail: "Pick the regions you need, then build them.",
        }
      : {
          tone: "warn",
          title: "Regions are chosen but not built",
          detail:
            o.builds.last?.status === "failed"
              ? "The last build failed. See what went wrong, then build again."
              : "Start a build to make the map.",
        };
  }
  const bad = o.processes.filter(needsAttention);
  if (bad.length)
    return {
      tone: "bad",
      title: `${bad.map((p) => p.name).join(", ")} ${bad.length === 1 ? "needs" : "need"} attention`,
      detail: "Requests that depend on it fail until it is back.",
    };
  if (!o.ready) {
    const waiting = o.readiness.filter((r) => !r.ok).map((r) => r.name);
    return {
      tone: "warn",
      title: "Starting up",
      detail: waiting.length ? `Waiting for: ${waiting.join(", ")}.` : "Not ready yet.",
    };
  }
  if (o.builds.last?.status === "failed")
    return {
      tone: "warn",
      title: "Serving, but the last build failed",
      detail: "The previous generation is still live. Data is not being refreshed.",
    };
  if (o.disk.guard.known && o.disk.guard.ok === false)
    return {
      tone: "warn",
      title: "Serving, but the next build will not fit",
      detail: "There is less free disk than a build needs. It will refuse to start.",
    };
  return { tone: "ok", title: "Serving", detail: "Everything is up." };
}

export function totals(kinds: KindTraffic[]) {
  const sum = (w: "hour" | "day") => kinds.reduce((a, k) => a + k[w].requests, 0);
  const errors = (w: "hour" | "day") => kinds.reduce((a, k) => a + k[w].errors, 0);
  const series = Array.from({ length: 60 }, (_, i) =>
    kinds.reduce((a, k) => a + (k.series[i] ?? 0), 0),
  );
  return {
    hour: sum("hour"),
    day: sum("day"),
    errorsHour: errors("hour"),
    errorsDay: errors("day"),
    perMin: kinds.reduce((a, k) => a + k.hour.per_min, 0),
    series,
  };
}

/** Disk as three shares of the volume: mapthing's data, everything else, free. */
export function diskShares(d: Overview["disk"]) {
  const total = Math.max(1, d.total_bytes);
  const other = Math.max(0, d.total_bytes - d.free_bytes - d.data_bytes);
  return {
    data: d.data_bytes / total,
    other: other / total,
    free: d.free_bytes / total,
    otherBytes: other,
    /** Where on the bar the build guard sits: free space must reach this far. */
    need: d.guard.known && d.guard.need_bytes ? Math.min(1, d.guard.need_bytes / total) : null,
  };
}

/** The bounding box of every region, for the map. */
export function unionBBox(
  regions: { bbox?: [number, number, number, number] }[],
): [number, number, number, number] | undefined {
  const boxes = regions
    .map((r) => r.bbox)
    .filter((b): b is [number, number, number, number] => !!b && b.length === 4);
  if (!boxes.length) return undefined;
  return [
    Math.min(...boxes.map((b) => b[0])),
    Math.min(...boxes.map((b) => b[1])),
    Math.max(...boxes.map((b) => b[2])),
    Math.max(...boxes.map((b) => b[3])),
  ];
}
