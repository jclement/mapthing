// Realistic fixtures for the mock backend (dev without a server, tests, screenshots).
import type {
  ApiKey,
  CatalogEntry,
  ElevationSource,
  Generation,
  LimitsConfig,
  LogEntry,
  OnDemandStatus,
  Region,
  Setting,
  SettingsInfo,
  SystemInfo,
} from "../api";

const GB = 1e9;
const MB = 1e6;

export const elevationSources: ElevationSource[] = [
  {
    id: "usgs-3dep-1m",
    name: "USGS 3DEP 1 m",
    resolution_m: 1,
    license: "Public domain",
    attribution: "USGS 3DEP",
  },
  {
    id: "usgs-3dep-10m",
    name: "USGS 3DEP 1/3″",
    resolution_m: 10,
    license: "Public domain",
    attribution: "USGS 3DEP",
  },
  {
    id: "nrcan-hrdem",
    name: "NRCan HRDEM",
    resolution_m: 2,
    license: "OGL – Canada",
    attribution: "Natural Resources Canada",
  },
  {
    id: "nrcan-mrdem",
    name: "NRCan MRDEM",
    resolution_m: 30,
    license: "OGL – Canada",
    attribution: "Natural Resources Canada",
  },
  {
    id: "bc-trim",
    name: "BC TRIM DEM",
    resolution_m: 25,
    license: "OGL – British Columbia",
    attribution: "Province of British Columbia",
  },
  {
    id: "ab-dem",
    name: "Alberta provincial DEM",
    resolution_m: 25,
    license: "OGL – Alberta",
    attribution: "Government of Alberta",
  },
  {
    id: "copernicus-glo30",
    name: "Copernicus GLO-30",
    resolution_m: 30,
    license: "Copernicus DEM licence",
    attribution:
      "© DLR e.V. 2010–2014 and © Airbus Defence and Space GmbH 2014–2018, provided under COPERNICUS by the European Union and ESA",
  },
];

const US = ["usgs-3dep-10m", "copernicus-glo30"];
const CA = ["nrcan-mrdem", "copernicus-glo30"];
const COP = ["copernicus-glo30"];

type E = [
  id: string,
  name: string,
  parent: string | null,
  bbox: CatalogEntry["bbox"],
  mb: number,
  src?: string[],
];
const rows: E[] = [
  ["north-america", "North America", null, [-168, 14, -52, 72], 0, COP],
  ["north-america/us", "United States", "north-america", [-125, 24.5, -66.9, 49.4], 0, US],
  [
    "north-america/us/texas",
    "Texas",
    "north-america/us",
    [-106.65, 25.84, -93.51, 36.5],
    640,
    ["usgs-3dep-1m", ...US],
  ],
  [
    "north-america/us/new-mexico",
    "New Mexico",
    "north-america/us",
    [-109.05, 31.33, -103, 37],
    128,
    US,
  ],
  ["north-america/us/oklahoma", "Oklahoma", "north-america/us", [-103, 33.62, -94.43, 37], 168, US],
  [
    "north-america/us/louisiana",
    "Louisiana",
    "north-america/us",
    [-94.04, 28.93, -88.82, 33.02],
    152,
    ["usgs-3dep-1m", ...US],
  ],
  [
    "north-america/us/colorado",
    "Colorado",
    "north-america/us",
    [-109.06, 36.99, -102.04, 41],
    270,
    US,
  ],
  [
    "north-america/us/wyoming",
    "Wyoming",
    "north-america/us",
    [-111.06, 40.99, -104.05, 45.01],
    74,
    US,
  ],
  [
    "north-america/us/north-dakota",
    "North Dakota",
    "north-america/us",
    [-104.05, 45.93, -96.55, 49],
    58,
    US,
  ],
  [
    "north-america/us/delaware",
    "Delaware",
    "north-america/us",
    [-75.79, 38.45, -75.05, 39.84],
    25,
    ["usgs-3dep-1m", ...US],
  ],
  [
    "north-america/us/pennsylvania",
    "Pennsylvania",
    "north-america/us",
    [-80.52, 39.72, -74.69, 42.27],
    290,
    US,
  ],
  [
    "north-america/us/california",
    "California",
    "north-america/us",
    [-124.48, 32.53, -114.13, 42.01],
    1180,
    US,
  ],
  ["north-america/canada", "Canada", "north-america", [-141, 41.7, -52.6, 83.1], 0, CA],
  [
    "north-america/canada/alberta",
    "Alberta",
    "north-america/canada",
    [-120, 49, -110, 60],
    340,
    ["nrcan-hrdem", "ab-dem", ...CA],
  ],
  [
    "north-america/canada/british-columbia",
    "British Columbia",
    "north-america/canada",
    [-139.06, 48.3, -114.03, 60],
    380,
    ["nrcan-hrdem", "bc-trim", ...CA],
  ],
  [
    "north-america/canada/saskatchewan",
    "Saskatchewan",
    "north-america/canada",
    [-110, 49, -101.36, 60],
    150,
    ["nrcan-hrdem", ...CA],
  ],
  [
    "north-america/canada/manitoba",
    "Manitoba",
    "north-america/canada",
    [-102, 49, -88.94, 60],
    120,
    CA,
  ],
  ["north-america/mexico", "Mexico", "north-america", [-118.4, 14.5, -86.7, 32.7], 690, COP],
  ["europe", "Europe", null, [-25, 34, 45, 72], 0, COP],
  ["europe/norway", "Norway", "europe", [4.5, 57.9, 31.2, 71.2], 1150, COP],
  ["europe/united-kingdom", "United Kingdom", "europe", [-8.7, 49.8, 1.8, 60.9], 1900, COP],
  ["europe/netherlands", "Netherlands", "europe", [3.3, 50.75, 7.23, 53.6], 1250, COP],
  ["australia-oceania", "Australia and Oceania", null, [110, -50, 180, 0], 0, COP],
  [
    "australia-oceania/australia",
    "Australia",
    "australia-oceania",
    [112.9, -43.7, 153.7, -10.6],
    860,
    COP,
  ],
];

export const catalog: CatalogEntry[] = rows.map(([id, name, parent, bbox, mb, src]) => ({
  id,
  name,
  parent,
  bbox,
  extract_bytes: mb * MB,
  elevation_sources: src ?? COP,
}));

const day = 86400000;
const now = Date.now();
const iso = (msAgo: number) => new Date(now - msAgo).toISOString();

export function installedRegions(): Region[] {
  return [
    {
      id: "north-america/us/texas",
      name: "Texas",
      catalog_id: "north-america/us/texas",
      bbox: [-106.65, 25.84, -93.51, 36.5],
      status: "installed",
      extract_bytes: 640 * MB,
      disk_bytes: 3.9 * GB,
      data_date: iso(3.4 * day),
      downloaded_at: iso(3.2 * day),
      elevation_sources: ["usgs-3dep-10m"],
    },
    {
      id: "north-america/canada/alberta",
      name: "Alberta",
      catalog_id: "north-america/canada/alberta",
      bbox: [-120, 49, -110, 60],
      status: "installed",
      // One region of the demo data reads its terrain on demand.
      terrain_mode: "on_demand",
      extract_bytes: 340 * MB,
      disk_bytes: 0.9 * GB,
      data_date: iso(3.4 * day),
      downloaded_at: iso(3.2 * day),
      elevation_sources: ["nrcan-hrdem", "ab-dem", "nrcan-mrdem"],
    },
    {
      id: "custom/permian-core",
      name: "Permian core",
      catalog_id: null,
      bbox: [-104.2, 31.0, -101.2, 33.2],
      status: "installed",
      extract_bytes: 0,
      disk_bytes: 0.6 * GB,
      data_date: iso(3.4 * day),
      downloaded_at: iso(3.2 * day),
      elevation_sources: ["usgs-3dep-1m", "usgs-3dep-10m"],
    },
  ];
}

/**
 * On-demand terrain as the server reports it. A server that has used it for a few days: a
 * part-filled cache, nearly everything answered from it. failing: one source stopped
 * answering a minute ago.
 */
export function onDemandStatus(
  config: OnDemandStatus["config"],
  regions: string[],
  cleared = false,
  failing = false,
): OnDemandStatus {
  const enabled = regions.length > 0 || config.outside === "on_demand";
  const used = enabled && !cleared;
  const s3 = "s3.amazonaws.com";
  const nrcan = "canelevation-dem.s3.ca-central-1.amazonaws.com";
  const src = (id: string, name: string, resolution_m: number, license: string, host: string) => ({
    id,
    name,
    resolution_m,
    license,
    attribution: "",
    host,
  });
  return {
    config,
    enabled,
    regions,
    minzoom: 9,
    maxzoom: 13,
    cache: {
      bytes: used ? Math.min(1.34 * GB, config.cache_mb * MB) : 0,
      max_bytes: config.cache_mb * MB,
      entries: used ? 41872 : 0,
    },
    counters: {
      hits: used ? 268431 : 0,
      built: used ? 18904 : 0,
      failed: used ? (failing ? 37 : 3) : 0,
      limited: used ? 12 : 0,
      requests: used ? 61250 : 0,
      bytes: used ? 9.8 * GB : 0,
    },
    upstream: used
      ? [
          {
            host: nrcan,
            requests: 48210,
            failures: failing ? 41 : 2,
            bytes: 7.9 * GB,
            last_ok: iso(failing ? 70_000 : 4_000),
            ...(failing
              ? {
                  last_error: iso(9_000),
                  error: "answered 503 Service Unavailable",
                }
              : {}),
            healthy: !failing,
          },
          {
            host: `prd-tnm.${s3}`,
            requests: 13040,
            failures: 1,
            bytes: 1.9 * GB,
            last_ok: iso(310_000),
            healthy: true,
          },
        ]
      : [],
    sources: [
      src("hrdem", "NRCan HRDEM mosaic (LiDAR, 2 m)", 2, "OGL-Canada-2.0", nrcan),
      src("3dep13", "USGS 3DEP 1/3 arc-second DEM", 10, "Public domain", `prd-tnm.${s3}`),
      src("mrdem", "NRCan MRDEM-30", 30, "OGL-Canada-2.0", nrcan),
      src(
        "glo30",
        "Copernicus DEM GLO-30",
        30,
        "Copernicus DEM licence",
        `copernicus-dem-30m.${s3}`,
      ),
    ],
    healthy: !(used && failing),
    warm: {
      state: "idle",
      total: 0,
      done: 0,
      fetched: 0,
      cached: 0,
      empty: 0,
      failed: 0,
      bytes: 0,
    },
  };
}

export function generations(): Generation[] {
  const regions = [
    { id: "north-america/us/texas", name: "Texas", data_date: iso(3.4 * day) },
    { id: "north-america/canada/alberta", name: "Alberta", data_date: iso(3.4 * day) },
    { id: "custom/permian-core", name: "Permian core", data_date: iso(3.4 * day) },
  ];
  const checks = [
    { name: "tiles readable at known points", ok: true, detail: "9/9 tiles" },
    { name: "sample route per region", ok: true, detail: "3/3 routes" },
    { name: "elevation inside each region", ok: true, detail: "3/3 regions" },
  ];
  return [
    {
      id: "20261101-030012",
      created_at: iso(3.1 * day),
      status: "live",
      manifest: {
        regions,
        sizes: {
          basemap: 1.42 * GB,
          terrain: 3.1 * GB,
          contours: 0.21 * GB,
          routing: 1.9 * GB,
          total: 6.63 * GB,
        },
        timings: {
          download: 212,
          merge: 48,
          basemap: 1460,
          valhalla: 1980,
          terrain: 2410,
          verify: 35,
          swap: 1,
        },
        checks,
      },
    },
    {
      id: "20261025-030009",
      created_at: iso(10.1 * day),
      status: "previous",
      manifest: {
        regions: regions.slice(0, 2).map((r) => ({ ...r, data_date: iso(10.4 * day) })),
        sizes: { basemap: 1.31 * GB, terrain: 2.7 * GB, routing: 1.76 * GB, total: 5.77 * GB },
        timings: {
          download: 198,
          merge: 44,
          basemap: 1392,
          valhalla: 1840,
          terrain: 2210,
          verify: 31,
          swap: 1,
        },
        checks,
      },
    },
  ];
}

/** The states a healthy single-machine setup rarely shows: a copy from elsewhere, a damaged one. */
export function oddGenerations(): Generation[] {
  const [live] = generations();
  return [
    {
      id: "20261110-181500",
      created_at: iso(0.6 * day),
      status: "incoming",
      owned: false,
      manifest: { ...live.manifest, timings: { ...live.manifest.timings, terrain: 0 } },
    },
    {
      id: "20261018-030007",
      created_at: iso(17.1 * day),
      status: "invalid",
      problems: [
        "valhalla/tiles.tar is missing",
        "terrain.pmtiles is 1.2 GB but the manifest records 2.7 GB (the copy is not finished)",
      ],
      manifest: {
        regions: live.manifest.regions.slice(0, 2),
        sizes: { basemap: 1.3 * GB, terrain: 1.2 * GB, routing: 0, total: 2.5 * GB },
        timings: {},
        checks: [],
      },
    },
  ];
}

function daily(base: number, wobble: number, seed: number) {
  return Array.from({ length: 14 }, (_, i) =>
    Math.max(
      0,
      Math.round(
        base + wobble * Math.sin(i * 0.9 + seed) + (i % 7 === 0 || i % 7 === 6 ? -base * 0.4 : 0),
      ),
    ),
  );
}

export function limitsConfig(): LimitsConfig {
  const weights = {
    tile: 1,
    raster: 10,
    route: 10,
    matrix: 20,
    isochrone: 20,
    snap: 2,
    valhalla: 10,
    elevation: 5,
    pack: 50,
    ondemand: 20,
  };
  const global = {
    raster: { rps: 100, burst: 200 },
    route: { rps: 60, burst: 120 },
    matrix: { rps: 10, burst: 20 },
    isochrone: { rps: 10, burst: 20 },
    valhalla: { rps: 60, burst: 120 },
    elevation: { rps: 100, burst: 200 },
    pack: { rps: 2, burst: 4 },
    ondemand: { rps: 20, burst: 40 },
  };
  return {
    weights: { ...weights },
    global: structuredClone(global),
    kinds: Object.keys(weights),
    defaults: { weights, global },
  };
}

const noRefusals = {
  limited_today: 0,
  limited_daily: new Array(14).fill(0),
  limited_scopes: {},
  recent_limited: [],
};

export function apiKeys(): ApiKey[] {
  return [
    {
      id: "k_01",
      label: "Field app (mobile)",
      prefix: "mt_7Hq2",
      created_at: iso(41 * day),
      last_used_at: iso(4 * 60000),
      limits: {
        rpm: 1200,
        rpd: 250000,
        rps: 20,
        burst: 200,
        daily_quota: 250000,
        monthly_quota: 5000000,
        ip_rps: 5,
        ip_burst: 50,
        origins: [],
      },
      usage: {
        today: 18240,
        total: 1_204_331,
        daily: daily(21000, 5000, 1),
        units_today: 212400,
        units_month: 2_310_000,
        rate_remaining: 164,
        limited_today: 37,
        limited_daily: [0, 0, 4, 0, 0, 0, 12, 0, 0, 0, 0, 3, 0, 37],
        limited_scopes: { ip: 31, rate: 6 },
        recent_limited: [
          { t: iso(42 * 60000), kind: "raster", scope: "rate" },
          { t: iso(9 * 60000), kind: "tile", scope: "ip", ip: "203.0.113.9" },
          { t: iso(8 * 60000), kind: "tile", scope: "ip", ip: "203.0.113.9" },
          { t: iso(3 * 60000), kind: "route", scope: "ip", ip: "203.0.113.9" },
        ],
      },
    },
    {
      id: "k_02",
      label: "Planning web app",
      prefix: "mt_c9Lk",
      created_at: iso(33 * day),
      last_used_at: iso(26 * 60000),
      limits: {
        rpm: null,
        rpd: 20000,
        daily_quota: 20000,
        origins: ["https://planner.example.com"],
      },
      usage: {
        today: 9412,
        total: 512_998,
        daily: daily(11000, 3000, 2),
        units_today: 20000,
        units_month: 431_000,
        rate_remaining: null,
        limited_today: 214,
        limited_daily: [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 214],
        limited_scopes: { daily: 214 },
        recent_limited: [{ t: iso(26 * 60000), kind: "tile", scope: "daily" }],
      },
    },
    {
      id: "k_03",
      label: "Routing batch jobs",
      prefix: "mt_Zp04",
      created_at: iso(12 * day),
      last_used_at: iso(19 * 3600000),
      limits: { rpm: 120, rpd: null, rps: 2, burst: 120, origins: [] },
      usage: {
        today: 0,
        total: 48_210,
        daily: daily(3000, 2500, 3),
        units_today: 0,
        units_month: 310_400,
        rate_remaining: 120,
        ...noRefusals,
      },
    },
    {
      id: "k_04",
      label: "Old test key",
      prefix: "mt_Q1aa",
      created_at: iso(90 * day),
      last_used_at: null,
      limits: { rpm: null, rpd: null, origins: [] },
      usage: {
        today: 0,
        total: 0,
        daily: new Array(14).fill(0),
        units_today: 0,
        units_month: 0,
        rate_remaining: null,
        ...noRefusals,
      },
    },
  ];
}

export function systemInfo(ready: boolean, gen: string | null): SystemInfo {
  return {
    version: "0.1.0-dev",
    commit: "d7f6801",
    build_date: iso(1 * day),
    components: [
      { name: "Go", version: "1.27.1" },
      { name: "Valhalla", version: "3.6.1" },
      { name: "Planetiler", version: "0.9.2" },
      { name: "maplibre-native (renderer)", version: "6.1.0" },
      { name: "osmium-tool", version: "1.18.0" },
      { name: "GDAL", version: "3.11.4" },
    ],
    ready,
    readiness: [
      { name: "live generation", ok: !!gen, detail: gen ?? "none yet: add a region and build" },
      {
        name: "valhalla_service",
        ok: ready,
        detail: ready ? "listening on 127.0.0.1:8002" : "waiting for a generation",
      },
      {
        name: "renderer",
        ok: ready,
        detail: ready ? "listening on 127.0.0.1:8090" : "waiting for a generation",
      },
      { name: "database", ok: true, detail: "mapthing.db, schema v4" },
    ],
    disk: { free_bytes: 28.6 * GB, total_bytes: 202 * GB },
    current_generation: gen,
    attribution: [
      {
        name: "OpenStreetMap",
        license: "ODbL 1.0",
        url: "https://www.openstreetmap.org/copyright",
        note: "© OpenStreetMap contributors",
      },
      {
        name: "USGS 3DEP",
        license: "Public domain",
        url: "https://www.usgs.gov/3d-elevation-program",
      },
      {
        name: "Natural Resources Canada (HRDEM, MRDEM)",
        license: "Open Government Licence – Canada",
        url: "https://open.canada.ca/en/open-government-licence-canada",
      },
      {
        name: "Province of British Columbia",
        license: "Open Government Licence – British Columbia",
        url: "https://www2.gov.bc.ca/gov/content/data/open-data/open-government-licence-bc",
      },
      {
        name: "Government of Alberta",
        license: "Open Government Licence – Alberta",
        url: "https://open.alberta.ca/licence",
      },
      {
        name: "Copernicus DEM GLO-30",
        license: "Copernicus DEM licence",
        note: "© DLR e.V. 2010–2014 and © Airbus Defence and Space GmbH 2014–2018, provided under COPERNICUS by the European Union and ESA",
      },
      { name: "Protomaps basemap schema", license: "BSD-3-Clause / CC0 styles" },
    ],
    imagery_source: {
      name: "Example open imagery",
      attribution: "Imagery © its providers",
      url: "https://example.com",
    },
  };
}

// A few days of log lines: the server, both routing instances, the renderer and two builds
// (j_0103 succeeded, j_0102 failed in the basemap stage).
export function logEntries(): LogEntry[] {
  const out: LogEntry[] = [];
  const now = Date.now();
  let seq = 0;
  const add = (
    minsAgo: number,
    source: string,
    level: LogEntry["level"],
    msg: string,
    job?: string,
  ) => {
    const ms = now - minsAgo * 60000;
    out.push({
      id: `${ms}${String(seq++ % 1000000).padStart(6, "0")}`,
      t: new Date(ms).toISOString(),
      source,
      level,
      msg,
      ...(job ? { job } : {}),
    });
  };
  const d = 1440;
  let m = 0; // minutes ago; step() moves it towards now
  const step = (
    delta: number,
    source: string,
    level: LogEntry["level"],
    msg: string,
    job?: string,
  ) => {
    m -= delta;
    add(m, source, level, msg, job);
  };
  // The failed build, 6.2 days ago.
  m = 6.2 * d;
  step(0, "mapthing", "info", "job start job=j_0102 name=build trigger=manual");
  step(0, "build", "info", "== stage: guard", "j_0102");
  step(1, "build", "info", "disk: need 61 GB, 74 GB free", "j_0102");
  step(1, "build", "info", "== stage: download", "j_0102");
  step(4, "build", "info", "texas-latest.osm.pbf: unchanged (2026-11-03)", "j_0102");
  step(1, "build", "info", "== stage: basemap", "j_0102");
  step(1, "build", "info", "$ java -Xmx12g -jar /opt/mapthing/protomaps-basemap.jar", "j_0102");
  for (let i = 0; i < 30; i++)
    step(
      0.4,
      "build",
      "info",
      `0:${String(i).padStart(2, "0")}:12 INF [osm_pass2] - nodes: [ ${(i * 3.1).toFixed(1)}M ${i * 3}% ] ways: [ 0 ]`,
      "j_0102",
    );
  step(
    0.2,
    "build",
    "warn",
    "0:12:40 WAR [osm_pass2] - GC overhead is high: 41% of the last minute",
    "j_0102",
  );
  step(
    0.2,
    "build",
    "error",
    "0:12:58 ERR [osm_pass2] - java.lang.OutOfMemoryError: Java heap space",
    "j_0102",
  );
  step(0.1, "build", "error", "stage basemap failed: java: exit status 1", "j_0102");
  step(
    0,
    "mapthing",
    "error",
    'job end job=j_0102 status=failed err="build stage basemap: java: exit status 1"',
  );
  // The last good build, 3.1 days ago, and the swap.
  m = 3.1 * d;
  step(0, "mapthing", "info", "job start job=j_0103 name=build trigger=schedule");
  for (const st of [
    "guard",
    "download",
    "merge",
    "basemap",
    "valhalla",
    "terrain",
    "verify",
    "swap",
    "retention",
  ]) {
    step(9, "build", "info", `== stage: ${st}`, "j_0103");
    step(1, "build", "info", `stage ${st} done`, "j_0103");
  }
  step(1, "valhalla@8003", "info", "valhalla@8003 started (pid 2211)");
  step(
    0.1,
    "valhalla@8003",
    "info",
    "2026/11/08 03:41:10.118 [INFO] Tile extract successfully loaded with 48213 tiles",
  );
  step(0.1, "mapthing", "info", "generation live id=20261108T030012Z previous=20261101T030009Z");
  step(0.1, "valhalla@8002", "info", "valhalla@8002 stopped");
  step(0, "mapthing", "info", "job end job=j_0103 status=succeeded");
  // A renderer crash yesterday.
  m = 1.2 * d;
  step(
    0,
    "renderer",
    "warn",
    "render overlay-dark/512/14/3518/6630@2x: 4 tiles failed to load (degraded)",
  );
  step(
    2,
    "renderer",
    "error",
    "FATAL ERROR: Reached heap limit Allocation failed - JavaScript heap out of memory",
  );
  step(0.01, "renderer", "error", "renderer exited after 2d 1h (signal: aborted); restart 1 in 1s");
  step(
    0.02,
    "mapthing",
    "info",
    'child state child=renderer state=backoff msg="signal: aborted; restarting in 1s"',
  );
  step(
    0.02,
    "xvfb",
    "info",
    "The XKEYBOARD keymap compiler (xkbcomp) reports: Could not resolve keysym XF86CameraAccessEnable",
  );
  step(0.02, "renderer", "info", "renderer started (pid 2214)");
  step(0.05, "renderer", "info", "renderer listening on 127.0.0.1:8081 (pool 4)");
  step(0.01, "mapthing", "info", 'child state child=renderer state=running msg=""');
  // Today.
  add(190, "mapthing", "warn", "admin login failed ip=203.0.113.7");
  add(
    188,
    "mapthing",
    "info",
    "http method=GET path=/styles/overlay-dark.json?key=[redacted] status=401 bytes=71 dur=0s ip=203.0.113.7",
  );
  for (let i = 0; i < 40; i++)
    add(
      170 - i * 4,
      "valhalla@8003",
      i % 13 === 5 ? "warn" : "info",
      i % 13 === 5
        ? "2026/11/11 14:02:11.402 [WARN] Exceeded max time for route: 5.2s"
        : `2026/11/11 14:${String(i).padStart(2, "0")}:07.311 [INFO] 200 POST /route 0.0${(i % 7) + 2}s`,
    );
  add(6, "mapthing", "info", "catalog: Geofabrik index refreshed (512 extracts)");
  add(2, "mapthing", "info", "stats flushed rows=14");
  out.sort((a, b) => (a.t < b.t ? -1 : a.t > b.t ? 1 : 0));
  return out;
}

export function settingsInfo(): SettingsInfo {
  const set = (name: string, group: string, value: string, def: string, help: string): Setting => ({
    name,
    group,
    value,
    default: def,
    source: "env",
    set: true,
    help,
  });
  const dflt = (name: string, group: string, def: string, help: string): Setting => ({
    name,
    group,
    value: def,
    default: def,
    source: "default",
    set: false,
    help,
  });
  return {
    versions: [
      { name: "mapthing", version: "0.9.0", detail: "commit 7efee20a1b2c, built 2026-11-10" },
      { name: "Go", version: "1.27.3", detail: "the language mapthing is built with" },
      { name: "Valhalla", version: "3.9.1", detail: "routing engine and graph builder" },
      {
        name: "Planetiler (Protomaps basemap)",
        version: "basemaps ca93fc06efba",
        detail: "vector basemap builder, protomaps-basemap.jar",
      },
      {
        name: "Renderer (MapLibre Native)",
        version: "6.4.1",
        detail: "raster tiles, Node 24.11.1",
      },
      { name: "GDAL", version: "3.8.4", detail: "elevation build" },
      { name: "osmium", version: "1.16.0", detail: "merging and clipping extracts" },
      { name: "Java", version: "21.0.8", detail: "runs Planetiler" },
    ],
    settings: [
      set("ADMIN_USER", "Access", "admin", "(required)", "The admin login name."),
      {
        name: "ADMIN_PASSWORD",
        group: "Access",
        value: "••••••••",
        default: "(required)",
        source: "env",
        secret: true,
        set: true,
        help: "The admin password.",
      },
      set(
        "MAPTHING_PUBLIC_URL",
        "Access",
        "https://maps.example.com",
        "taken from each request",
        "The address clients use. Written into styles, TileJSON and key snippets. Set it behind a proxy or tunnel.",
      ),
      set(
        "MAPTHING_TRUSTED_PROXY",
        "Access",
        "on",
        "off",
        "on: every request arrives through a proxy or tunnel, so the client address comes from CF-Connecting-IP or X-Forwarded-For.",
      ),
      {
        ...dflt("MAPTHING_LISTEN", "Access", ":8080", "The address the server listens on."),
        source: "image",
        set: true,
      },
      {
        name: "MAPTHING_INTERNAL_TOKEN",
        group: "Access",
        value: "",
        default: "generated at each start",
        source: "default",
        secret: true,
        set: false,
        help: "Shared with the renderer so it can read tiles without an API key.",
      },
      dflt(
        "MAPTHING_DATA",
        "Serving",
        "/data",
        "The data folder: generations, downloads, caches, logs and the database.",
      ),
      dflt(
        "MAPTHING_SERVE",
        "Serving",
        "tiles,routing,elevation",
        "Which parts this server serves.",
      ),
      dflt(
        "MAPTHING_RENDERER",
        "Serving",
        "on",
        "off runs without the raster renderer; raster tile requests then answer 404.",
      ),
      dflt(
        "MAPTHING_LOG_MAX_MB",
        "Serving",
        "50",
        "Total size of the log files kept in the data folder.",
      ),
      set(
        "MAPTHING_RASTER_CACHE_MB",
        "Caches and limits",
        "8192",
        "2048",
        "Disk cache of rendered raster tiles.",
      ),
      dflt(
        "MAPTHING_IMAGERY_CACHE_MB",
        "Caches and limits",
        "4096",
        "Disk cache of satellite imagery tiles.",
      ),
      dflt(
        "MAPTHING_DISK_GUARD_FACTOR",
        "Builds",
        "1",
        "Multiplies the free disk a build asks for before it starts.",
      ),
      dflt(
        "MAPTHING_PLANETILER_XMX",
        "Builds",
        "60% of available memory",
        "Java heap for the basemap build, for example 12g.",
      ),
      dflt(
        "MAPTHING_CONTOURS_MAXZOOM",
        "Terrain and contours",
        "12",
        "Deepest contour zoom. 0 turns contours off.",
      ),
      {
        name: "MAPTHING_TRUSTED_PROXI",
        group: "Not recognised",
        value: "on",
        source: "env",
        set: true,
        help: "mapthing does not read this variable. Check the name.",
      },
    ],
  };
}
