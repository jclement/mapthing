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
  /** Details some refusals carry (an invalid generation lists what is wrong with it). */
  problems?: string[];
  constructor(status: number, code: string, message: string, problems?: string[]) {
    super(message);
    this.status = status;
    this.code = code;
    this.problems = problems;
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
    const err = (
      data as
        | { error?: { code?: string; message?: string; problems?: string[] } | string }
        | undefined
    )?.error;
    const code = typeof err === "object" ? (err.code ?? "error") : "error";
    const msg =
      typeof err === "string" ? err : (err?.message ?? (text || `${res.status} ${res.statusText}`));
    throw new ApiError(res.status, code, msg, typeof err === "object" ? err.problems : undefined);
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

/** One Geofabrik extract a custom region is clipped from. */
export interface CatalogParent {
  id: string;
  name: string;
  bytes: number;
  estimated?: boolean; // size guessed from area
}

/** POST /admin/api/catalog/estimate: the cheapest set of extracts covering a custom area. */
export interface CustomEstimate {
  parents: CatalogParent[];
  total_bytes: number; // download: every parent extract
  estimated?: boolean;
  single_best?: CatalogParent; // cheapest single extract covering it alone
  elevation_sources: string[];
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
  parents?: CatalogParent[]; // custom regions: extracts clipped from
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
  regions: { id: string; name: string; data_date: string | null; bbox?: BBox }[];
  sizes: { basemap: number; terrain: number; contours?: number; routing: number; total: number };
  timings: Partial<Record<StageName, number>>; // seconds
  checks: { name: string; ok: boolean; detail?: string }[];
}

/**
 * live: being served. previous: kept for rollback. kept: another complete one. building: a
 * build is writing it. incoming: copied in from another machine, never activated here.
 * invalid: incomplete or damaged (see problems).
 */
export type GenerationStatus = "live" | "previous" | "kept" | "building" | "incoming" | "invalid";

export interface Generation {
  id: string;
  created_at: string;
  status: GenerationStatus;
  owned?: boolean; // built or activated here (retention only prunes these)
  problems?: string[];
  bytes?: number; // size on disk
  manifest: Manifest;
  can_activate?: boolean;
  can_delete?: boolean;
}

export interface GenerationList {
  generations: Generation[];
  disk?: {
    bytes: number; // what they occupy: files shared by hard link counted once
    sum_bytes: number; // their sizes added up
    free_bytes: number;
    total_bytes: number;
  };
}

/** POST /admin/api/generations/{id}/activate */
export interface Activation {
  id: string;
  status: "live";
  already_live: boolean;
  previous: string | null;
  removed: string[];
}

export type LogLevel = "debug" | "info" | "warn" | "error";

/** One line of the server's log (GET /admin/api/logs and its stream). */
export interface LogEntry {
  id: string; // orders entries; a nanosecond timestamp as a decimal string
  t: string;
  source: string; // "mapthing", "build", or a child process ("renderer", "valhalla@8002", "xvfb")
  level: LogLevel;
  job?: string; // build job id, for source "build"
  msg: string;
}

export interface LogPage {
  entries: LogEntry[]; // oldest first
  more: boolean; // older matching entries exist
  sources: string[];
  store: { bytes: number; max_bytes: number; oldest?: string; file_error?: string };
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

/** One process on the overview: this server, a supervised child, or a build's tools. */
export interface ProcessInfo {
  name: string;
  state: "running" | "starting" | "restarting" | "stopped";
  detail?: string;
  pid: number | null;
  uptime_s: number;
  restarts: number;
  rss_bytes: number; // the process and everything it started
  healthy: boolean;
  cpu_percent: number | null; // of one core; null until there are two readings
  standby?: boolean; // a replacement warming up during a generation swap
  role?: string;
}

export type TrafficKindName = "tiles" | "raster" | "route" | "elevation" | "style" | "pack";

export interface TrafficWindow {
  requests: number;
  errors: number; // answered 5xx
  rejected: number; // answered 401, 403 or 429
  per_min: number;
  p50_ms: number;
  p95_ms: number;
}

export interface KindTraffic {
  kind: TrafficKindName | string;
  hour: TrafficWindow;
  day: TrafficWindow;
  series: number[]; // requests per minute, the last 60 minutes, oldest first
}

/** GET /admin/api/overview: the admin home page in one call. */
export interface Overview {
  version: string;
  commit: string;
  started_at: string;
  ready: boolean;
  readiness: { name: string; ok: boolean; detail?: string }[];
  generation: {
    id: string;
    created_at?: string;
    bytes: number;
    regions: { id: string; name: string; data_date: string | null; bbox?: BBox }[];
  } | null;
  regions_installed: number;
  processes: ProcessInfo[];
  system: {
    cpus: number;
    load1: number; // -1 when unknown
    memory_used_bytes: number;
    memory_total_bytes: number;
    platform: string;
  };
  traffic: { since: string; kinds: KindTraffic[] };
  disk: {
    total_bytes: number;
    free_bytes: number;
    data_bytes: number; // what mapthing keeps on the volume
    parts: { name: string; bytes: number }[];
    /** What the next build needs free (its disk guard). */
    guard: { known: boolean; need_bytes?: number; ok?: boolean };
  };
  builds: {
    schedule_enabled: boolean;
    cron: string;
    next_run: string | null;
    active: Job | null;
    last: {
      id: string;
      status: JobStatus;
      trigger: string;
      started_at?: string;
      finished_at?: string;
      duration_s?: number;
      error?: string;
      generation_id?: string;
    } | null;
  };
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
  /** "bridge" | "tunnel" when a deck-mode profile interpolated this sample. */
  structure?: string;
}

/** How a profile treats bridges and tunnels: terrain under them, or the road level across. */
export type BridgeMode = "ground" | "deck";

export interface StructureSpan {
  start_m: number;
  end_m: number;
  kind: "bridge" | "tunnel";
}

export interface ElevationProfile {
  samples: ElevationSample[];
  length_m: number;
  gain_m: number;
  loss_m: number;
  min_m: number;
  max_m: number;
  sources: string[];
  bridges: BridgeMode;
  structure_spans?: StructureSpan[];
  /** Set when deck mode was asked for but the profile fell back to ground. */
  warning?: string;
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
  estimateCustom: (a: { bbox?: BBox; polygon?: LonLat[] }) =>
    request<CustomEstimate>("POST", "/admin/api/catalog/estimate", a),
  removeRegion: (id: string) =>
    request<void>("DELETE", `/admin/api/regions/${encodeURIComponent(id)}`),

  build: (refresh_downloads: boolean, rebuild_terrain = false) =>
    request<Job>("POST", "/admin/api/build", { refresh_downloads, rebuild_terrain }),
  jobs: () => request<{ jobs: Job[] }>("GET", "/admin/api/jobs"),
  job: (id: string) => request<JobDetail>("GET", `/admin/api/jobs/${encodeURIComponent(id)}`),
  generations: () => request<GenerationList>("GET", "/admin/api/generations"),
  activateGeneration: (id: string) =>
    request<Activation>("POST", `/admin/api/generations/${encodeURIComponent(id)}/activate`),
  deleteGeneration: (id: string) =>
    request<void>("DELETE", `/admin/api/generations/${encodeURIComponent(id)}`),

  schedule: () => request<Schedule>("GET", "/admin/api/schedule"),
  saveSchedule: (s: Schedule) => request<Schedule>("PUT", "/admin/api/schedule", s),

  keys: () => request<{ keys: ApiKey[] }>("GET", "/admin/api/keys"),
  createKey: (k: NewKey) => request<CreatedKey>("POST", "/admin/api/keys", k),
  revokeKey: (id: string) => request<void>("DELETE", `/admin/api/keys/${encodeURIComponent(id)}`),

  /** `query` is the string lib/logs.ts apiQuery builds ("" or "?…"). */
  logs: (query: string) => request<LogPage>("GET", `/admin/api/logs${query}`),

  overview: () => request<Overview>("GET", "/admin/api/overview"),
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
  profile: (path: LonLat[], interval_m?: number, bridges?: BridgeMode) =>
    request<ElevationProfile>("POST", "/api/v1/elevation/profile", { path, interval_m, bridges }),
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

/** Live log lines from /admin/api/logs/stream (same query as api.logs, plus after=<id>). */
export function subscribeLogs(
  query: string,
  on: {
    entry: (e: LogEntry) => void;
    dropped?: (count: number) => void;
    state?: (connected: boolean) => void;
  },
): () => void {
  const es = new EventSource(`/admin/api/logs/stream${query}`, { withCredentials: true });
  const entry = (ev: MessageEvent) => {
    try {
      on.entry(JSON.parse(ev.data) as LogEntry);
    } catch {
      /* ignore a malformed event */
    }
  };
  const dropped = (ev: MessageEvent) => {
    try {
      on.dropped?.((JSON.parse(ev.data) as { count: number }).count);
    } catch {
      /* ignore */
    }
  };
  es.addEventListener("entry", entry as EventListener);
  es.addEventListener("dropped", dropped as EventListener);
  es.onopen = () => on.state?.(true);
  es.onerror = () => on.state?.(false);
  return () => {
    es.removeEventListener("entry", entry as EventListener);
    es.removeEventListener("dropped", dropped as EventListener);
    es.close();
  };
}

/** Download link for the filtered log (`query` as for api.logs). */
export function logsDownloadUrl(query: string, format: "text" | "jsonl" = "text"): string {
  const sep = query ? "&" : "?";
  return `/admin/api/logs/download${query}${format === "jsonl" ? `${sep}format=jsonl` : ""}`;
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

/** The style a fresh map shows, whatever the UI theme. */
export const DEFAULT_STYLE: StyleName = "overlay-dark";

/** Pick the style to show by default: overlay-dark, else one matching the UI theme, else the first. */
export function defaultStyle(styles: StyleInfo[] | undefined, dark: boolean): StyleName {
  const want = DEFAULT_STYLE;
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
export function keySnippets(origin: string, secret: string, style: StyleName = DEFAULT_STYLE) {
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
