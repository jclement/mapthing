// Typed client for the mapthing admin API and the keyed endpoints the UI uses.
// Shapes follow docs/architecture.md ("Admin API shapes"). The admin session cookie
// authorises every call, so the UI never needs an API key of its own.

import type { LineString } from "geojson";

export type LonLat = [number, number];
export type BBox = [number, number, number, number]; // west, south, east, north

// ---------- errors ----------

export class ApiError extends Error {
  status: number;
  code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

let onUnauthorized: (() => void) | null = null;
/** Called whenever an admin call answers 401 (session expired or never existed). */
export function setUnauthorizedHandler(fn: (() => void) | null) {
  onUnauthorized = fn;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: "same-origin",
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  let data: unknown;
  try {
    data = text ? JSON.parse(text) : undefined;
  } catch {
    data = undefined;
  }
  if (!res.ok) {
    if (res.status === 401 && path !== "/admin/api/login") onUnauthorized?.();
    const err = (data as { error?: { code?: string; message?: string } | string } | undefined)
      ?.error;
    const code = typeof err === "object" ? (err.code ?? "error") : "error";
    const msg =
      typeof err === "string" ? err : (err?.message ?? (text || `${res.status} ${res.statusText}`));
    throw new ApiError(res.status, code, msg);
  }
  return data as T;
}

// ---------- types: admin ----------

export interface ElevationSource {
  id: string;
  name: string;
  resolution_m: number;
  license: string;
  attribution: string;
}

export interface CatalogEntry {
  id: string; // e.g. "north-america/us/texas"
  name: string;
  parent: string | null;
  bbox: BBox;
  extract_bytes: number; // 0 for grouping nodes, or while the size is not known yet
  downloadable?: boolean; // has its own extract (selectable); absent → extract_bytes > 0
  elevation_sources: string[]; // ElevationSource ids covering this entry, best first
}

export interface Catalog {
  entries: CatalogEntry[];
  elevation_sources: ElevationSource[];
}

export interface Region {
  id: string;
  name: string;
  catalog_id: string | null; // null for a custom bbox/polygon region
  bbox: BBox;
  polygon?: LonLat[];
  status: "pending" | "installed" | "failed";
  extract_bytes: number;
  disk_bytes: number; // this region's share of the live generation + its download
  data_date: string | null; // OSM data timestamp of the downloaded extract
  downloaded_at: string | null;
  elevation_sources: string[];
}

export type NewRegion = { id: string } | { name: string; bbox: BBox; polygon?: LonLat[] };

// Build stages as the pipeline reports them, in order.
export type StageName =
  | "guard"
  | "download"
  | "merge"
  | "basemap"
  | "valhalla"
  | "terrain"
  | "verify"
  | "swap"
  | "retention";
export type StageStatus = "pending" | "running" | "done" | "failed" | "skipped";

export interface Stage {
  name: StageName;
  status: StageStatus;
  progress: number; // 0..1
  detail?: string;
  started_at?: string;
  finished_at?: string;
}

export type JobStatus = "queued" | "running" | "succeeded" | "failed" | "cancelled";

export interface Job {
  id: string;
  status: JobStatus;
  trigger: "manual" | "schedule";
  refresh_downloads: boolean;
  created_at: string;
  started_at?: string;
  finished_at?: string;
  progress: number; // 0..1 overall
  stages: Stage[];
  error?: string;
  generation_id?: string;
}

export interface LogLine {
  job_id: string;
  t: string;
  level: "debug" | "info" | "warn" | "error";
  msg: string;
}

export interface JobDetail extends Job {
  log: LogLine[]; // tail
}

export interface Manifest {
  regions: { id: string; name: string; data_date: string | null }[];
  sizes: { basemap: number; terrain: number; routing: number; total: number };
  timings: Partial<Record<StageName, number>>; // seconds
  checks: { name: string; ok: boolean; detail?: string }[];
}

export interface Generation {
  id: string;
  created_at: string;
  status: "live" | "kept";
  manifest: Manifest;
}

export interface Schedule {
  cron: string; // 5-field, UTC
  enabled: boolean;
  refresh_downloads: boolean;
  regions: { id: string; enabled: boolean }[];
}

export interface KeyLimits {
  rpm: number | null;
  rpd: number | null;
  origins: string[];
}

export interface ApiKey {
  id: string;
  label: string;
  prefix: string; // first characters of the secret, e.g. "mt_4f9a"
  created_at: string;
  last_used_at: string | null;
  limits: KeyLimits;
  usage: { today: number; total: number; daily: number[] }; // daily: last 14 days, oldest first
}

export interface NewKey {
  label: string;
  rpm?: number | null;
  rpd?: number | null;
  origins?: string[];
}

export interface CreatedKey {
  key: ApiKey;
  secret: string; // shown once
}

export type RequestKind = "tiles" | "raster" | "routes" | "elevation" | "packs";

export interface Stats {
  daily: ({ date: string } & Record<RequestKind, number>)[];
  by_key: { key_id: string; label: string; requests: number }[];
  cache: { name: string; hits: number; misses: number; size_bytes: number }[];
  disk: {
    generations: { id: string; bytes: number }[];
    regions: { id: string; name: string; bytes: number }[];
    downloads_bytes: number;
    elevation_cache_bytes: number;
    imagery_cache_bytes: number;
    free_bytes: number;
    total_bytes: number;
  };
  memory: { process_rss_bytes: number; system_used_bytes: number; system_total_bytes: number };
  processes: {
    name: string;
    state: "running" | "starting" | "restarting" | "stopped";
    pid: number | null;
    uptime_s: number;
    restarts: number;
    rss_bytes: number;
  }[];
}

export interface SystemInfo {
  version: string;
  commit: string;
  build_date: string;
  components: { name: string; version: string }[];
  ready: boolean;
  readiness: { name: string; ok: boolean; detail?: string }[];
  disk: { free_bytes: number; total_bytes: number };
  current_generation: string | null;
  attribution: { name: string; license: string; url?: string; note?: string }[];
  imagery_source: { name: string; attribution: string; url?: string } | null;
}

// ---------- types: keyed ----------

export type RouteProfile = "auto" | "truck";

export interface RouteRequest {
  locations: { lat: number; lon: number }[];
  profile: RouteProfile;
  avoid_unpaved?: boolean;
}

export interface RouteManeuver {
  instruction: string;
  distance_m: number;
  duration_s: number;
  street?: string;
}

export interface RouteResult {
  geometry: LineString;
  distance_m: number;
  duration_s: number;
  surface_breakdown_m: Partial<Record<"paved" | "gravel" | "dirt" | "unknown", number>>;
  legs: { distance_m: number; duration_s: number; maneuvers?: RouteManeuver[] }[];
}

/** POST /api/v1/route response: a GeoJSON Feature whose properties are the route summary. */
export interface RouteFeature {
  type: "Feature";
  bbox?: BBox;
  geometry: LineString;
  // The server names it surface_breakdown (metres by surface class).
  properties: Omit<RouteResult, "geometry" | "surface_breakdown_m"> & {
    surface_breakdown?: RouteResult["surface_breakdown_m"];
    surface_breakdown_m?: RouteResult["surface_breakdown_m"];
  };
}

export interface ElevationSample {
  lon: number;
  lat: number;
  elevation_m: number | null;
  source?: string;
  resolution_m?: number;
  distance_m?: number;
}

export interface ElevationProfile {
  samples: ElevationSample[];
  length_m: number;
  gain_m: number;
  loss_m: number;
  min_m: number;
  max_m: number;
  sources: string[];
}

export interface StyleInfo {
  id: StyleName;
  name: string;
  description: string;
  dark: boolean;
}

// ---------- calls ----------

export const api = {
  login: (username: string, password: string) =>
    request<{ user: string }>("POST", "/admin/api/login", { username, password }),
  logout: () => request<void>("POST", "/admin/api/logout"),

  catalog: () => request<Catalog>("GET", "/admin/api/catalog"),
  regions: () => request<{ regions: Region[] }>("GET", "/admin/api/regions"),
  addRegion: (r: NewRegion) => request<Region>("POST", "/admin/api/regions", r),
  removeRegion: (id: string) =>
    request<void>("DELETE", `/admin/api/regions/${encodeURIComponent(id)}`),

  build: (refresh_downloads: boolean) =>
    request<Job>("POST", "/admin/api/build", { refresh_downloads }),
  jobs: () => request<{ jobs: Job[] }>("GET", "/admin/api/jobs"),
  job: (id: string) => request<JobDetail>("GET", `/admin/api/jobs/${encodeURIComponent(id)}`),
  generations: () => request<{ generations: Generation[] }>("GET", "/admin/api/generations"),

  schedule: () => request<Schedule>("GET", "/admin/api/schedule"),
  saveSchedule: (s: Schedule) => request<Schedule>("PUT", "/admin/api/schedule", s),

  keys: () => request<{ keys: ApiKey[] }>("GET", "/admin/api/keys"),
  createKey: (k: NewKey) => request<CreatedKey>("POST", "/admin/api/keys", k),
  revokeKey: (id: string) => request<void>("DELETE", `/admin/api/keys/${encodeURIComponent(id)}`),

  stats: () => request<Stats>("GET", "/admin/api/stats"),
  system: () => request<SystemInfo>("GET", "/admin/api/system"),

  styles: () => request<StyleInfo[]>("GET", "/styles"),

  // The server answers a GeoJSON Feature (geometry + properties, plus bbox and optional
  // alternates); the UI works with the flattened RouteResult.
  route: (r: RouteRequest) =>
    request<RouteFeature>("POST", "/api/v1/route", r).then(
      ({
        properties: { surface_breakdown, surface_breakdown_m, ...rest },
        geometry,
      }): RouteResult => ({
        ...rest,
        surface_breakdown_m: surface_breakdown ?? surface_breakdown_m ?? {},
        geometry,
      }),
    ),
  elevation: (points: LonLat[]) =>
    request<{ results: ElevationSample[] }>("POST", "/api/v1/elevation", { points }),
  profile: (path: LonLat[], interval_m?: number) =>
    request<ElevationProfile>("POST", "/api/v1/elevation/profile", { path, interval_m }),
};

/** Server-sent events from /admin/api/events. */
export type ServerEvent =
  | { type: "job"; data: Job }
  | { type: "log"; data: LogLine }
  | { type: "generation"; data: { id: string } };

export function subscribeEvents(onEvent: (e: ServerEvent) => void): () => void {
  const es = new EventSource("/admin/api/events", { withCredentials: true });
  const handler = (type: ServerEvent["type"]) => (ev: MessageEvent) => {
    try {
      onEvent({ type, data: JSON.parse(ev.data) } as ServerEvent);
    } catch {
      /* ignore malformed event */
    }
  };
  const types: ServerEvent["type"][] = ["job", "log", "generation"];
  const listeners = types.map((t) => [t, handler(t)] as const);
  for (const [t, h] of listeners) es.addEventListener(t, h as EventListener);
  return () => {
    for (const [t, h] of listeners) es.removeEventListener(t, h as EventListener);
    es.close();
  };
}

// ---------- URLs ----------

/** Style id as listed by GET /styles (e.g. "outdoors", "outdoors-dark", "satellite"). */
export type StyleName = string;

/** Style URL for the UI's own map (session cookie authorises it; no key needed). */
export function styleUrl(name: StyleName): string {
  if (import.meta.env.VITE_MOCK) return mockStyleUrl(name);
  return `/styles/${encodeURIComponent(name)}.json`;
}

/** A small raster thumbnail of a style (one 256 px tile from the raster renderer). */
export function styleThumbUrl(name: StyleName, z: number, x: number, y: number): string {
  return `/styles/${encodeURIComponent(name)}/256/${z}/${x}/${y}.png`;
}

/** Pick the style to show by default: outdoors / outdoors-dark per theme, else the first match. */
export function defaultStyle(styles: StyleInfo[] | undefined, dark: boolean): StyleName {
  const want = dark ? "outdoors-dark" : "outdoors";
  if (!styles?.length) return want;
  return (
    styles.find((s) => s.id === want)?.id ?? styles.find((s) => s.dark === dark)?.id ?? styles[0].id
  );
}

// Mock mode only (dev without a backend): public demo styles so the map has something
// to draw. Statically removed from the production bundle.
function mockStyleUrl(name: StyleName): string {
  return name.endsWith("dark")
    ? "https://tiles.openfreemap.org/styles/dark"
    : name === "satellite" || name === "field"
      ? "https://tiles.openfreemap.org/styles/bright"
      : "https://tiles.openfreemap.org/styles/liberty";
}

/** Snippets shown when a key is created. `origin` is the public base URL. */
export function keySnippets(origin: string, secret: string, style: StyleName = "outdoors") {
  return [
    {
      label: "MapLibre / Mapbox GL style URL",
      value: `${origin}/styles/${style}.json?key=${secret}`,
    },
    {
      label: "Leaflet raster tiles (512 px, retina)",
      value: `${origin}/styles/${style}/512/{z}/{x}/{y}@2x.png?key=${secret}`,
    },
    {
      label: "PMTiles archive (HTTP Range)",
      value: `${origin}/tiles/basemap.pmtiles?key=${secret}`,
    },
    {
      label: "Google Elevation API compatible",
      value: `${origin}/maps/api/elevation/json?locations=31.9686,-102.0779&key=${secret}`,
    },
    {
      label: "OpenRouteService base URL",
      value: `${origin}/ors  (header  Authorization: ${secret})`,
    },
    {
      label: "curl",
      value: `curl -H 'Authorization: Bearer ${secret}' ${origin}/tiles/basemap.json`,
    },
  ];
}
