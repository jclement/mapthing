// An in-memory mapthing backend: patches fetch and EventSource so the UI is fully
// clickable without a server. Loaded only in mock mode and in tests; never bundled
// into the production build (see main.tsx).
import type {
  ApiKey,
  CreatedKey,
  ElevationSample,
  Generation,
  Job,
  LogLine,
  LonLat,
  NewKey,
  NewRegion,
  Region,
  RouteRequest,
  Schedule,
  ServerEvent,
  Stage,
  StageName,
  Stats,
} from "../api";
import { bboxOf, haversine } from "../lib/geo";
import * as fx from "./fixtures";

export interface MockOptions {
  /** Start with nothing installed (first-run experience). */
  empty?: boolean;
  /** Start already signed in. */
  authed?: boolean;
  /** Milliseconds per simulated build tick. */
  tickMs?: number;
}

interface State {
  authed: boolean;
  regions: Region[];
  generations: Generation[];
  jobs: Job[];
  logs: Record<string, LogLine[]>;
  keys: ApiKey[];
  schedule: Schedule;
  tickMs: number;
}

let S: State;
const listeners = new Set<(e: ServerEvent) => void>();
let timer: ReturnType<typeof setInterval> | null = null;

export function resetMock(opts: MockOptions = {}) {
  if (timer) clearInterval(timer);
  timer = null;
  const empty = !!opts.empty;
  const regions = empty ? [] : fx.installedRegions();
  const gens = empty ? [] : fx.generations();
  const history: Job[] = empty
    ? []
    : [
        doneJob("j_0103", 3.1, "schedule", gens[0].id, gens[0].manifest.timings),
        failedJob("j_0102", 6.2),
        doneJob("j_0101", 10.1, "schedule", gens[1].id, gens[1].manifest.timings),
      ];
  S = {
    authed: !!opts.authed,
    regions,
    generations: gens,
    jobs: history,
    logs: {},
    keys: empty ? [] : fx.apiKeys(),
    schedule: {
      cron: "0 3 * * 0",
      enabled: true,
      refresh_downloads: true,
      regions: regions.map((r) => ({ id: r.id, enabled: r.id !== "custom/permian-core" })),
    },
    tickMs: opts.tickMs ?? 350,
  };
}

const STAGES: StageName[] = [
  "guard",
  "download",
  "merge",
  "basemap",
  "valhalla",
  "terrain",
  "verify",
  "swap",
  "retention",
];
const STAGE_WEIGHT: Record<StageName, number> = {
  guard: 0.005,
  download: 0.115,
  merge: 0.04,
  basemap: 0.24,
  valhalla: 0.26,
  terrain: 0.28,
  verify: 0.05,
  swap: 0.005,
  retention: 0.005,
};

function ago(days: number) {
  return new Date(Date.now() - days * 86400000).toISOString();
}

function doneJob(
  id: string,
  daysAgo: number,
  trigger: Job["trigger"],
  gen: string,
  timings: Partial<Record<StageName, number>>,
): Job {
  let t = Date.now() - daysAgo * 86400000;
  const stages: Stage[] = STAGES.map((name) => {
    const s = t;
    t += (timings[name] ?? 10) * 1000;
    return {
      name,
      status: "done",
      progress: 1,
      started_at: new Date(s).toISOString(),
      finished_at: new Date(t).toISOString(),
    };
  });
  return {
    id,
    status: "succeeded",
    trigger,
    refresh_downloads: true,
    created_at: ago(daysAgo),
    started_at: ago(daysAgo),
    finished_at: new Date(t).toISOString(),
    progress: 1,
    stages,
    generation_id: gen,
  };
}

function failedJob(id: string, daysAgo: number): Job {
  const j = doneJob(id, daysAgo, "schedule", "", {
    download: 230,
    merge: 50,
    basemap: 1500,
    valhalla: 410,
  });
  j.status = "failed";
  j.generation_id = undefined;
  j.error = "valhalla: valhalla_build_tiles exited with status 137 (out of memory)";
  j.stages = j.stages.map((s) =>
    s.name === "valhalla"
      ? { ...s, status: "failed", progress: 0.38, detail: "exit status 137" }
      : STAGES.indexOf(s.name) > STAGES.indexOf("valhalla")
        ? { name: s.name, status: "pending", progress: 0 }
        : s,
  );
  j.finished_at = j.stages.find((s) => s.name === "valhalla")?.finished_at;
  return j;
}

function emit(e: ServerEvent) {
  for (const l of listeners) l(e);
}

function log(job: Job, msg: string, level: LogLine["level"] = "info") {
  const line: LogLine = { job_id: job.id, t: new Date().toISOString(), level, msg };
  if (!S.logs[job.id]) S.logs[job.id] = [];
  const arr = S.logs[job.id];
  arr.push(line);
  if (arr.length > 500) arr.shift();
  emit({ type: "log", data: line });
}

const STAGE_LOGS: Record<StageName, string[]> = {
  guard: [
    "guard: 31.4 GB free, estimated need 9.8 GB",
    "guard: tools ok (osmium, planetiler, valhalla)",
  ],
  download: [
    "fetching %r.osm.pbf from download.geofabrik.de",
    "%r: 41% (182 MB / 446 MB) 38 MB/s",
    "%r: up to date (sequence 4521), skipping",
  ],
  merge: ["osmium merge: 3 inputs → merged.osm.pbf", "osmium: 52,118,902 nodes, 4,881,330 ways"],
  basemap: [
    "planetiler: profile=protomaps maxzoom=15",
    "planetiler: [osm_pass1] nodes: 38M ways: 3.1M rels: 41k",
    "planetiler: [osm_pass2] 61% features: 18M",
    "planetiler: [mbtiles] tiles: 4.2M z15 bytes: 1.3G",
  ],
  valhalla: [
    "valhalla_build_tiles: parsing ways",
    "valhalla_build_tiles: building graph (level 2, 214 tiles)",
    "valhalla_build_tiles: adding elevation to edges",
    "valhalla_build_tiles: validating and building shortcuts",
  ],
  terrain: [
    "terrain: sources usgs-3dep-10m (cached), nrcan-hrdem (3 new tiles)",
    "terrain: gdalwarp → EPSG:3857, zoom 13",
    "terrain: encoding terrarium tiles 47%",
  ],
  verify: ["verify: tiles 9/9 ok", "verify: routes 3/3 ok", "verify: elevation 3/3 ok"],
  swap: ["swap: current → generations/%g", "supervisor: restarting valhalla_service, renderer"],
  retention: ["retention: keeping 2 generations, removed 1"],
};

function startBuild(refresh: boolean, trigger: Job["trigger"] = "manual"): Job {
  const id = `j_${Date.now().toString(36)}`;
  const job: Job = {
    id,
    status: S.jobs.some((j) => j.status === "running") ? "queued" : "running",
    trigger,
    refresh_downloads: refresh,
    created_at: new Date().toISOString(),
    started_at: new Date().toISOString(),
    progress: 0,
    stages: STAGES.map((name) => ({ name, status: "pending", progress: 0 })),
  };
  S.jobs.unshift(job);
  emit({ type: "job", data: clone(job) });
  log(job, `build started (${trigger}${refresh ? ", refreshing downloads" : ""})`);
  for (const r of S.regions)
    if (r.status === "pending") log(job, `region ${r.name}: new, will download`);
  if (!timer) timer = setInterval(tick, S.tickMs);
  return job;
}

function tick() {
  const job = S.jobs.find((j) => j.status === "running");
  if (!job) {
    if (timer) clearInterval(timer);
    timer = null;
    return;
  }
  const gid = new Date().toISOString().replace(/[-:]/g, "").replace("T", "-").slice(0, 15);
  const idx = job.stages.findIndex((s) => s.status !== "done" && s.status !== "skipped");
  if (idx < 0) return;
  const st = job.stages[idx];
  if (st.status === "pending") {
    st.status = "running";
    st.started_at = new Date().toISOString();
  }
  const steps = st.name === "swap" || st.name === "merge" ? 2 : st.name === "verify" ? 3 : 7;
  st.progress = Math.min(1, st.progress + 1 / steps);
  const lines = STAGE_LOGS[st.name];
  const pick = lines[Math.min(lines.length - 1, Math.floor(st.progress * lines.length - 0.001))];
  const regionName =
    S.regions[Math.floor(Math.random() * Math.max(1, S.regions.length))]?.name ?? "region";
  if (Math.random() < 0.7)
    log(job, pick.replace("%r", regionName.toLowerCase().replace(/ /g, "-")).replace("%g", gid));
  st.detail = `${Math.round(st.progress * 100)}%`;
  if (st.progress >= 1) {
    st.status = "done";
    st.finished_at = new Date().toISOString();
    st.detail = undefined;
  }
  job.progress = job.stages.reduce(
    (s, x) => s + STAGE_WEIGHT[x.name] * (x.status === "done" ? 1 : x.progress),
    0,
  );
  if (job.stages.every((s) => s.status === "done")) {
    job.status = "succeeded";
    job.progress = 1;
    job.finished_at = new Date().toISOString();
    job.generation_id = gid;
    for (const r of S.regions) {
      r.status = "installed";
      r.data_date = r.data_date ?? new Date(Date.now() - 6 * 3600000).toISOString();
      r.downloaded_at = new Date().toISOString();
      r.disk_bytes = r.disk_bytes || Math.max(25e6 * 4, r.extract_bytes * 5.5);
    }
    for (const g of S.generations) g.status = "kept";
    const total = S.regions.reduce((s, r) => s + r.disk_bytes, 0);
    S.generations.unshift({
      id: gid,
      created_at: new Date().toISOString(),
      status: "live",
      manifest: {
        regions: S.regions.map((r) => ({ id: r.id, name: r.name, data_date: r.data_date })),
        sizes: { basemap: total * 0.22, terrain: total * 0.48, routing: total * 0.3, total },
        timings: {
          download: 41,
          merge: 6,
          basemap: 182,
          valhalla: 240,
          terrain: 300,
          verify: 9,
          swap: 1,
        },
        checks: [
          {
            name: "tiles readable at known points",
            ok: true,
            detail: `${S.regions.length * 3} tiles`,
          },
          {
            name: "sample route per region",
            ok: true,
            detail: `${S.regions.length}/${S.regions.length} routes`,
          },
          {
            name: "elevation inside each region",
            ok: true,
            detail: `${S.regions.length}/${S.regions.length} regions`,
          },
        ],
      },
    });
    S.generations = S.generations.slice(0, 2);
    log(job, `generation ${gid} is live`);
    emit({ type: "job", data: clone(job) });
    emit({ type: "generation", data: { id: gid } });
    const next = S.jobs.filter((j) => j.status === "queued").pop();
    if (next) {
      next.status = "running";
      next.started_at = new Date().toISOString();
      emit({ type: "job", data: clone(next) });
    }
    return;
  }
  emit({ type: "job", data: clone(job) });
}

function clone<T>(x: T): T {
  return JSON.parse(JSON.stringify(x));
}

// ---------- synthetic terrain + routing ----------

function elevAt([lon, lat]: LonLat): number {
  // Smooth synthetic terrain: rises westwards (plains → foothills), with ripples.
  const base = 300 + Math.max(0, -lon - 95) * 85 + Math.max(0, lat - 49) * 30;
  return (
    base +
    120 * Math.sin(lon * 3.1) * Math.cos(lat * 2.3) +
    35 * Math.sin(lon * 17 + lat * 11) +
    8 * Math.sin(lon * 90) * Math.cos(lat * 70)
  );
}

function sourceAt([lon, lat]: LonLat) {
  if (lat > 49 && lon < -110 && lon > -120) return { source: "nrcan-hrdem", resolution_m: 2 };
  if (lat > 49) return { source: "nrcan-mrdem", resolution_m: 30 };
  if (lon < -66 && lon > -125 && lat > 24) return { source: "usgs-3dep-10m", resolution_m: 10 };
  return { source: "copernicus-glo30", resolution_m: 30 };
}

function sample(p: LonLat, distance_m?: number): ElevationSample {
  return {
    lon: p[0],
    lat: p[1],
    elevation_m: Math.round(elevAt(p) * 10) / 10,
    ...sourceAt(p),
    distance_m,
  };
}

function routeBetween(a: LonLat, b: LonLat): LonLat[] {
  // A plausible road-ish path: an L-shaped grid route with gentle wiggles.
  const pts: LonLat[] = [a];
  const corner: LonLat = [b[0], a[1]];
  const legs: [LonLat, LonLat][] = [
    [a, corner],
    [corner, b],
  ];
  for (const [p, q] of legs) {
    const n = Math.max(4, Math.round(haversine(p, q) / 800));
    for (let i = 1; i <= n; i++) {
      const f = i / n;
      const w = Math.sin(f * Math.PI * 3) * 0.004;
      pts.push([
        p[0] + (q[0] - p[0]) * f + (p[1] === q[1] ? 0 : w),
        p[1] + (q[1] - p[1]) * f + (p[1] === q[1] ? w : 0),
      ]);
    }
  }
  return pts;
}

function profileOf(path: LonLat[], interval: number) {
  const samples: ElevationSample[] = [];
  let acc = 0;
  samples.push(sample(path[0], 0));
  let next = interval;
  for (let i = 1; i < path.length; i++) {
    const seg = haversine(path[i - 1], path[i]);
    while (acc + seg >= next) {
      const f = (next - acc) / seg;
      samples.push(
        sample(
          [
            path[i - 1][0] + (path[i][0] - path[i - 1][0]) * f,
            path[i - 1][1] + (path[i][1] - path[i - 1][1]) * f,
          ],
          next,
        ),
      );
      next += interval;
    }
    acc += seg;
  }
  samples.push(sample(path[path.length - 1], acc));
  let gain = 0;
  let loss = 0;
  for (let i = 1; i < samples.length; i++) {
    const d = samples[i].elevation_m! - samples[i - 1].elevation_m!;
    if (d > 0) gain += d;
    else loss -= d;
  }
  const el = samples.map((s) => s.elevation_m!);
  return {
    samples,
    length_m: acc,
    gain_m: Math.round(gain),
    loss_m: Math.round(loss),
    min_m: Math.min(...el),
    max_m: Math.max(...el),
    sources: [...new Set(samples.map((s) => s.source!))],
  };
}

function inInstalled(p: LonLat) {
  return S.regions.some(
    (r) =>
      r.status === "installed" &&
      p[0] >= r.bbox[0] &&
      p[0] <= r.bbox[2] &&
      p[1] >= r.bbox[1] &&
      p[1] <= r.bbox[3],
  );
}

// ---------- HTTP ----------

type Handler = (m: RegExpMatchArray, body: unknown, url: URL) => unknown | Response;
const routes: [string, RegExp, Handler][] = [];
const on = (method: string, re: RegExp, h: Handler) => routes.push([method, re, h]);

function json(status: number, data: unknown) {
  return new Response(data === undefined ? null : JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

on("POST", /^\/admin\/api\/login$/, (_m, b) => {
  const { username, password } = b as { username: string; password: string };
  if (username === "admin" && password === "admin") {
    S.authed = true;
    return { user: username };
  }
  return json(401, { error: { code: "bad_credentials", message: "Wrong username or password" } });
});
on("POST", /^\/admin\/api\/logout$/, () => {
  S.authed = false;
  return json(204, undefined);
});
on("GET", /^\/admin\/api\/catalog$/, () => ({
  entries: fx.catalog,
  elevation_sources: fx.elevationSources,
}));
on("GET", /^\/admin\/api\/regions$/, () => ({ regions: S.regions }));
on("POST", /^\/admin\/api\/regions$/, (_m, b) => {
  const nr = b as NewRegion;
  let r: Region;
  if ("id" in nr) {
    const c = fx.catalog.find((e) => e.id === nr.id);
    if (!c) return json(404, { error: { code: "not_found", message: "No such catalog entry" } });
    if (S.regions.some((x) => x.id === c.id))
      return json(409, { error: { code: "exists", message: `${c.name} is already installed` } });
    r = {
      id: c.id,
      name: c.name,
      catalog_id: c.id,
      bbox: c.bbox,
      status: "pending",
      extract_bytes: c.extract_bytes,
      disk_bytes: 0,
      data_date: null,
      downloaded_at: null,
      elevation_sources: c.elevation_sources.slice(0, 2),
    };
  } else {
    const slug =
      nr.name
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, "-")
        .replace(/^-|-$/g, "") || "area";
    const bbox = nr.bbox ?? bboxOf(nr.polygon ?? []);
    r = {
      id: `custom/${slug}`,
      name: nr.name,
      catalog_id: null,
      bbox,
      polygon: nr.polygon,
      status: "pending",
      extract_bytes: 0,
      disk_bytes: 0,
      data_date: null,
      downloaded_at: null,
      elevation_sources: [sourceAt([(bbox[0] + bbox[2]) / 2, (bbox[1] + bbox[3]) / 2]).source],
    };
  }
  S.regions.push(r);
  S.schedule.regions.push({ id: r.id, enabled: true });
  return json(201, r);
});
on("DELETE", /^\/admin\/api\/regions\/(.+)$/, (m) => {
  const id = decodeURIComponent(m[1]);
  S.regions = S.regions.filter((r) => r.id !== id);
  S.schedule.regions = S.schedule.regions.filter((r) => r.id !== id);
  return json(204, undefined);
});
on("POST", /^\/admin\/api\/build$/, (_m, b) =>
  json(202, startBuild(!!(b as { refresh_downloads?: boolean })?.refresh_downloads)),
);
on("GET", /^\/admin\/api\/jobs$/, () => ({ jobs: S.jobs }));
on("GET", /^\/admin\/api\/jobs\/(.+)$/, (m) => {
  const j = S.jobs.find((x) => x.id === m[1]);
  if (!j) return json(404, { error: { code: "not_found", message: "No such job" } });
  return { ...j, log: S.logs[j.id] ?? [] };
});
on("GET", /^\/admin\/api\/generations$/, () => ({ generations: S.generations }));
on("GET", /^\/admin\/api\/schedule$/, () => S.schedule);
on("PUT", /^\/admin\/api\/schedule$/, (_m, b) => {
  S.schedule = b as Schedule;
  return S.schedule;
});
on("GET", /^\/admin\/api\/keys$/, () => ({ keys: S.keys }));
on("POST", /^\/admin\/api\/keys$/, (_m, b) => {
  const nk = b as NewKey;
  if (!nk.label?.trim())
    return json(400, { error: { code: "invalid", message: "A label is required" } });
  const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789";
  let secret = "mt_";
  for (let i = 0; i < 40; i++) secret += alphabet[Math.floor(Math.random() * alphabet.length)];
  const key: ApiKey = {
    id: `k_${Date.now().toString(36)}`,
    label: nk.label.trim(),
    prefix: secret.slice(0, 7),
    created_at: new Date().toISOString(),
    last_used_at: null,
    limits: { rpm: nk.rpm ?? null, rpd: nk.rpd ?? null, origins: nk.origins ?? [] },
    usage: { today: 0, total: 0, daily: new Array(14).fill(0) },
  };
  S.keys.unshift(key);
  return json(201, { key, secret } satisfies CreatedKey);
});
on("DELETE", /^\/admin\/api\/keys\/(.+)$/, (m) => {
  S.keys = S.keys.filter((k) => k.id !== m[1]);
  return json(204, undefined);
});
on("GET", /^\/admin\/api\/stats$/, () => stats());
on("GET", /^\/admin\/api\/system$/, () => {
  const live = S.generations.find((g) => g.status === "live");
  return fx.systemInfo(!!live, live?.id ?? null);
});

on("GET", /^\/styles$/, () => [
  {
    id: "outdoors",
    name: "Outdoors",
    description: "Topographic basemap with hillshade, contours and trails",
    dark: false,
  },
  {
    id: "outdoors-dark",
    name: "Outdoors dark",
    description: "The outdoors map for night use and dark UIs",
    dark: true,
  },
  {
    id: "satellite",
    name: "Satellite",
    description: "Imagery with roads and labels on top",
    dark: false,
  },
  {
    id: "field",
    name: "Field",
    description: "High contrast: lease roads, tracks and surfaces emphasised",
    dark: false,
  },
  {
    id: "overlay",
    name: "Overlay",
    description: "Roads and labels only, transparent background, for layering",
    dark: false,
  },
  {
    id: "overlay-dark",
    name: "Overlay dark",
    description: "Overlay for dark imagery or dark UIs",
    dark: true,
  },
]);
on("POST", /^\/api\/v1\/route$/, (_m, b) => {
  const req = b as RouteRequest;
  const locs = req.locations.map((l) => [l.lon, l.lat] as LonLat);
  const outside = locs.findIndex((p) => !inInstalled(p));
  if (outside >= 0)
    return json(400, {
      error: {
        code: "no_road",
        message: `Location ${outside + 1} is not near any road in the installed regions`,
      },
    });
  let coords: LonLat[] = [locs[0]];
  const legs = [];
  for (let i = 1; i < locs.length; i++) {
    const seg = routeBetween(locs[i - 1], locs[i]);
    coords = coords.concat(seg.slice(1));
    let d = 0;
    for (let k = 1; k < seg.length; k++) d += haversine(seg[k - 1], seg[k]);
    const speed = req.profile === "truck" ? 21 : 25;
    legs.push({
      distance_m: d,
      duration_s: d / speed,
      maneuvers: [
        {
          instruction: "Head east on County Road 1250",
          distance_m: d * 0.18,
          duration_s: (d * 0.18) / 14,
          street: "County Road 1250",
        },
        {
          instruction: "Turn left onto TX-302",
          distance_m: d * 0.55,
          duration_s: (d * 0.55) / 29,
          street: "TX-302",
        },
        {
          instruction: "Turn right onto lease road",
          distance_m: d * 0.27,
          duration_s: (d * 0.27) / 9,
        },
        { instruction: "You have arrived at your destination", distance_m: 0, duration_s: 0 },
      ],
    });
  }
  const dist = legs.reduce((s, l) => s + l.distance_m, 0);
  const unpaved = req.avoid_unpaved ? 0.04 : 0.27;
  return {
    type: "Feature",
    geometry: { type: "LineString", coordinates: coords },
    properties: {
      distance_m: dist,
      duration_s: legs.reduce((s, l) => s + l.duration_s, 0),
      surface_breakdown_m: {
        paved: dist * (1 - unpaved - 0.03),
        gravel: dist * unpaved * 0.7,
        dirt: dist * unpaved * 0.3,
        unknown: dist * 0.03,
      },
      legs,
    },
  };
});
on("POST", /^\/api\/v1\/elevation$/, (_m, b) => ({
  results: (b as { points: LonLat[] }).points.map((p) =>
    inInstalled(p) ? sample(p) : { lon: p[0], lat: p[1], elevation_m: null },
  ),
}));
on("POST", /^\/api\/v1\/elevation\/profile$/, (_m, b) => {
  const { path, interval_m, bridges } = b as {
    path: LonLat[];
    interval_m?: number;
    bridges?: "ground" | "deck";
  };
  return { ...profileOf(path, interval_m ?? 30), bridges: bridges ?? "ground" };
});

function stats(): Stats {
  const days = 30;
  const daily = Array.from({ length: days }, (_, i) => {
    const d = new Date(Date.now() - (days - 1 - i) * 86400000);
    const wk = d.getUTCDay() === 0 || d.getUTCDay() === 6 ? 0.45 : 1;
    const grow = 0.75 + (i / days) * 0.4;
    const w = (b: number, s: number) => Math.round(b * wk * grow * (1 + 0.15 * Math.sin(i * s)));
    return {
      date: d.toISOString().slice(0, 10),
      tiles: w(24000, 0.7),
      raster: w(6100, 1.1),
      routes: w(1900, 0.5),
      elevation: w(3400, 0.9),
      packs: w(40, 1.7),
    };
  });
  return {
    daily: S.regions.length ? daily : [],
    by_key: S.keys
      .filter((k) => k.usage.total > 0)
      .map((k) => ({
        key_id: k.id,
        label: k.label,
        requests: k.usage.daily.reduce((a, b) => a + b, 0),
      })),
    cache: [
      { name: "Raster tiles", hits: 412_000, misses: 61_200, size_bytes: 1.8e9 },
      { name: "Imagery proxy", hits: 238_000, misses: 92_100, size_bytes: 4.6e9 },
      { name: "Vector tiles (in-memory)", hits: 1_610_000, misses: 48_000, size_bytes: 2.6e8 },
    ],
    disk: {
      generations: S.generations.map((g) => ({ id: g.id, bytes: g.manifest.sizes.total })),
      regions: S.regions.map((r) => ({ id: r.id, name: r.name, bytes: r.disk_bytes })),
      downloads_bytes: S.regions.reduce((s, r) => s + r.extract_bytes, 0),
      elevation_cache_bytes: S.regions.length ? 7.4e9 : 0,
      imagery_cache_bytes: S.regions.length ? 4.6e9 : 0,
      free_bytes: 28.6e9,
      total_bytes: 202e9,
    },
    memory: { process_rss_bytes: 412e6, system_used_bytes: 21.3e9, system_total_bytes: 47e9 },
    processes: [
      {
        name: "valhalla_service",
        state: S.generations.length ? "running" : "stopped",
        pid: S.generations.length ? 2211 : null,
        uptime_s: 3.1 * 86400,
        restarts: 1,
        rss_bytes: 1.9e9,
      },
      {
        name: "renderer",
        state: S.generations.length ? "running" : "stopped",
        pid: S.generations.length ? 2214 : null,
        uptime_s: 3.1 * 86400,
        restarts: 0,
        rss_bytes: 640e6,
      },
    ],
  };
}

async function handle(input: RequestInfo | URL, init?: RequestInit): Promise<Response | null> {
  const url = new URL(
    typeof input === "string" ? input : input instanceof URL ? input.href : input.url,
    location.href,
  );
  if (url.origin !== location.origin) return null;
  const method = (init?.method ?? (input instanceof Request ? input.method : "GET")).toUpperCase();
  const path = url.pathname;
  for (const [m, re, h] of routes) {
    if (m !== method) continue;
    const match = path.match(re);
    if (!match) continue;
    const open = path === "/admin/api/login";
    if (!open && !S.authed)
      return json(401, { error: { code: "unauthorized", message: "Sign in required" } });
    let body: unknown;
    if (typeof init?.body === "string" && init.body) body = JSON.parse(init.body);
    await new Promise((r) => setTimeout(r, 60 + Math.random() * 120));
    const out = h(match, body, url);
    return out instanceof Response ? out : json(200, out);
  }
  return null;
}

class MockEventSource {
  readyState = 1;
  url: string;
  withCredentials = false;
  onopen: ((e: Event) => void) | null = null;
  onerror: ((e: Event) => void) | null = null;
  onmessage: ((e: MessageEvent) => void) | null = null;
  private l = new Map<string, Set<(e: MessageEvent) => void>>();
  private fn = (e: ServerEvent) => {
    const ev = new MessageEvent(e.type, { data: JSON.stringify(e.data) });
    for (const h of this.l.get(e.type) ?? []) h(ev);
  };
  constructor(url: string) {
    this.url = url;
    listeners.add(this.fn);
  }
  addEventListener(t: string, h: (e: MessageEvent) => void) {
    if (!this.l.has(t)) this.l.set(t, new Set());
    this.l.get(t)!.add(h);
  }
  removeEventListener(t: string, h: (e: MessageEvent) => void) {
    this.l.get(t)?.delete(h);
  }
  close() {
    this.readyState = 2;
    listeners.delete(this.fn);
  }
}

export function installMock(opts: MockOptions = {}) {
  resetMock(opts);
  const realFetch = globalThis.fetch?.bind(globalThis);
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const r = await handle(input, init);
    if (r) return r;
    if (!realFetch) throw new Error(`mock: no handler for ${String(input)}`);
    return realFetch(input, init);
  }) as typeof fetch;
  (globalThis as { EventSource: unknown }).EventSource = MockEventSource;
}

/** For tests: run the build simulation to completion synchronously. */
export function finishBuilds() {
  for (let i = 0; i < 200 && S.jobs.some((j) => j.status === "running"); i++) tick();
}
