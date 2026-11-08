// Realistic fixtures for the mock backend (dev without a server, tests, screenshots).
import type { ApiKey, CatalogEntry, ElevationSource, Generation, Region, SystemInfo } from "../api";

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
      extract_bytes: 340 * MB,
      disk_bytes: 2.4 * GB,
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
        sizes: { basemap: 1.42 * GB, terrain: 3.1 * GB, routing: 1.9 * GB, total: 6.42 * GB },
        timings: {
          download: 212,
          merge: 48,
          basemap: 1460,
          routing: 1980,
          elevation: 2410,
          verify: 35,
          swap: 1,
        },
        checks,
      },
    },
    {
      id: "20261025-030009",
      created_at: iso(10.1 * day),
      status: "kept",
      manifest: {
        regions: regions.slice(0, 2).map((r) => ({ ...r, data_date: iso(10.4 * day) })),
        sizes: { basemap: 1.31 * GB, terrain: 2.7 * GB, routing: 1.76 * GB, total: 5.77 * GB },
        timings: {
          download: 198,
          merge: 44,
          basemap: 1392,
          routing: 1840,
          elevation: 2210,
          verify: 31,
          swap: 1,
        },
        checks,
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

export function apiKeys(): ApiKey[] {
  return [
    {
      id: "k_01",
      label: "Field app (mobile)",
      prefix: "mt_7Hq2",
      created_at: iso(41 * day),
      last_used_at: iso(4 * 60000),
      limits: { rpm: 600, rpd: null, origins: [] },
      usage: { today: 18240, total: 1_204_331, daily: daily(21000, 5000, 1) },
    },
    {
      id: "k_02",
      label: "Planning web app",
      prefix: "mt_c9Lk",
      created_at: iso(33 * day),
      last_used_at: iso(26 * 60000),
      limits: { rpm: null, rpd: 200000, origins: ["https://planner.example.com"] },
      usage: { today: 9412, total: 512_998, daily: daily(11000, 3000, 2) },
    },
    {
      id: "k_03",
      label: "Routing batch jobs",
      prefix: "mt_Zp04",
      created_at: iso(12 * day),
      last_used_at: iso(19 * 3600000),
      limits: { rpm: 120, rpd: 50000, origins: [] },
      usage: { today: 0, total: 48_210, daily: daily(3000, 2500, 3) },
    },
    {
      id: "k_04",
      label: "Old test key",
      prefix: "mt_Q1aa",
      created_at: iso(90 * day),
      last_used_at: null,
      limits: { rpm: null, rpd: null, origins: [] },
      usage: { today: 0, total: 0, daily: new Array(14).fill(0) },
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
