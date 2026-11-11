// On-demand terrain: the logic behind its card on the Overview, its section in Settings and
// the per-region choice. Kept apart from the components so it can be tested.
import type { OnDemandHost, OnDemandStatus, Region, TerrainMode } from "../api";

/** The share of terrain tiles answered from the cache since the server started; null before any. */
export function cacheHitRate(c: OnDemandStatus["counters"]): number | null {
  const total = c.hits + c.built;
  return total > 0 ? c.hits / total : null;
}

/** How full the cache is, 0..1. */
export function cacheFill(s: OnDemandStatus): number {
  return s.cache.max_bytes > 0 ? Math.min(1, s.cache.bytes / s.cache.max_bytes) : 0;
}

export type HostTone = "ok" | "bad" | "neutral";

/** What to say about one DEM host. */
export function hostState(h: OnDemandHost): { tone: HostTone; label: string } {
  if (h.requests === 0) return { tone: "neutral", label: "Not used yet" };
  return h.healthy ? { tone: "ok", label: "Answering" } : { tone: "bad", label: "Not answering" };
}

/** Hosts in a stable order: the failing ones first, then by name. */
export function sortedHosts(hosts: OnDemandHost[]): OnDemandHost[] {
  return [...hosts].sort(
    (a, b) => Number(a.healthy) - Number(b.healthy) || a.host.localeCompare(b.host),
  );
}

/**
 * Where on demand applies, as a sentence fragment: "Texas and Alberta", "everywhere outside
 * built regions", both, or "nowhere". names maps region ids to display names.
 */
export function appliesTo(s: OnDemandStatus, names: Record<string, string> = {}): string {
  const regions = s.regions.map((id) => names[id] ?? id);
  const parts: string[] = [];
  if (regions.length > 0) {
    parts.push(
      regions.length <= 3
        ? list(regions)
        : `${regions.slice(0, 2).join(", ")} and ${regions.length - 2} more regions`,
    );
  }
  if (s.config.outside === "on_demand") parts.push("everywhere outside built regions");
  return parts.length > 0 ? list(parts) : "nowhere";
}

function list(items: string[]): string {
  if (items.length <= 1) return items.join("");
  return `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;
}

/** The two ways a region's terrain can be provided, for the choice in the Regions page. */
export const TERRAIN_CHOICES: { value: TerrainMode; title: string; detail: string }[] = [
  {
    value: "prebuilt",
    title: "Build it",
    detail:
      "Every zoom is made by the build and kept on this server. Always instant, works with no internet. Costs disk and build time.",
  },
  {
    value: "on_demand",
    title: "Read on demand",
    detail:
      "Only the zoomed-out levels are built. Closer terrain and elevations are fetched from the public elevation sources the first time a place is asked for (about a second), then kept in the cache. Needs the internet for places not seen before.",
  },
];

export function terrainMode(r: Pick<Region, "terrain_mode">): TerrainMode {
  return r.terrain_mode ?? "prebuilt";
}

/**
 * The terrain disk a region needs when it is on demand: its low zooms only. A build keeps one
 * tile of the chunk level for every 256 of the deepest, plus the pyramid below it: about 1 %.
 */
export function onDemandTerrainBytes(prebuiltBytes: number): number {
  return Math.round(prebuiltBytes * 0.01);
}

/** Megabytes ↔ the gigabytes the cache size field shows (decimal, as sizes are shown). */
export function mbToGb(mb: number): number {
  return Math.round(mb / 10) / 100;
}

export function gbToMb(gb: number): number {
  return Math.round(gb * 1000);
}

/**
 * Why a cache size (in GB, as typed) cannot be saved; "" when it can. A size larger than the
 * free disk is allowed (it is a budget, not a reservation); the page warns about it.
 */
export function cacheSizeProblem(gb: number): string {
  if (!Number.isFinite(gb)) return "Give a size in gigabytes.";
  if (gbToMb(gb) < 64) return "At least 64 MB (0.064 GB).";
  if (gb > 4000) return "At most 4000 GB.";
  return "";
}
