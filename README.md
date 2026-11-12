# mapthing

A self-hosted map service in one container: vector and raster basemap tiles, offline packs,
routing, and elevation profiles, with an admin web UI for picking regions, scheduling refreshes
and issuing API keys.

> **Status: under active construction.** The design contract is in
> [docs/architecture.md](docs/architecture.md).

```sh
docker run -d --name mapthing -p 8080:8080 -v mapthing-data:/data \
  -e ADMIN_USER=admin -e ADMIN_PASSWORD='change-me' \
  ghcr.io/jclement/mapthing:latest
```

Open http://localhost:8080, sign in, add a region (a US state, a Canadian province, or a box you
draw), and build it. When the build finishes the map, routing and elevation for that region are
live. To put it on the internet under your own hostname, follow
[docs/deploy.md](docs/deploy.md).

## What it serves

- **Vector tiles** (Protomaps v4 schema) as z/x/y MVT and as a PMTiles archive with HTTP Range
  support, plus MapLibre / Mapbox GL styles (light, dark, satellite).
- **Raster tiles** rendered from those styles, 256 or 512 px, @2x.
- **Offline packs**: a PMTiles or MBTiles extract for any bounding box.
- **Routing** (Valhalla): car and truck, with sensible handling of gravel and unpaved roads.
- **Elevation** at points and along paths, from the best open DEM for each area (USGS 3DEP in
  the US, NRCan and provincial data in Canada, Copernicus elsewhere).
- **On-demand terrain**, as an option: a region's terrain and contours, or those of the whole
  world outside your regions, read from the public DEM files the first time a place is asked
  for and cached, instead of built ahead. Off unless you turn it on
  ([docs/operations.md](docs/operations.md#on-demand-terrain)).
- **Compatibility endpoints**, so existing clients can switch by changing a URL: Google
  Elevation API, Google Routes `computeRoutes`, Mapbox Static Tiles URLs, and OpenRouteService
  v2. [docs/switching.md](docs/switching.md) says exactly what switches by changing the host,
  what has an equivalent, and what is not served at all.

## Sizing

Whole-country builds, from measurements of regional builds plus published data
([docs/sizing.md](docs/sizing.md) has the method, every figure's basis and the per-component tables):

| | United States | Canada | Saudi Arabia |
|---|---|---|---|
| OSM extract | 12.3 GB | 6.5 GB | 0.12 GB |
| Vector basemap (z0–15) | 17 GB | 16 GB | 0.4 GB |
| One generation (basemap + routing + terrain) | ~55 GB | ~68 GB | ~4.3 GB |
| Disk, steady / peak during a build | ~140 / ~265 GB | ~200 / ~300 GB | ~11 / ~16 GB |
| Full build from scratch (10 vCPU) | ~8 h | ~11 h | ~1 h |
| Machine, comfortable | 16 cores, 64 GB, 500 GB NVMe | 16 cores, 32 GB, 500 GB NVMe | 8 cores, 16 GB, 50 GB |

Terrain (elevation) dominates both build time and disk. An offline pack for a ~100×150 km basin
at full detail is ~20 MB; a state or province at z14 is 100–470 MB.

A region whose terrain is set to **on demand** skips almost all of that: the build makes only its
zoomed-out terrain (Texas: 12 MB in about a minute, instead of gigabytes over hours), and the
rest is fetched when first viewed, about a second per screen, into a cache you size. Map tiles
and routing are always built.

## Stack

- **Go** for the service itself: one static binary that serves the API, tiles and admin UI,
  supervises the other processes and runs the build pipeline.
- **Valhalla** (C++) for routing. It has truck and car costing, handles unpaved and track roads
  with penalties rather than exclusion, and includes matrix, isochrone and snapping.
- **Planetiler** with the Protomaps profile for vector tiles, **GDAL** for the elevation build,
  and **osmium** for OSM extracts. These are bundled tools, each the best at its job.
- **MapLibre Native** for the raster renderer, and a React + MapLibre GL JS admin UI that is
  compiled into the Go binary.

## Running

The image is published for `linux/amd64` and `linux/arm64`:

```sh
docker run -d --name mapthing --restart unless-stopped \
  -p 8080:8080 -v mapthing-data:/data \
  -e ADMIN_USER=admin -e ADMIN_PASSWORD='change-me' \
  -e MAPTHING_PUBLIC_URL=https://maps.example.com \
  ghcr.io/jclement/mapthing:latest
```

**Deploying for real:** [docs/deploy.md](docs/deploy.md) is the step-by-step guide for Docker
Compose behind a Cloudflare Tunnel (no open ports): creating the tunnel, the `.env`, the data
folder, the first build, then updating, rolling back and backups. The files it uses are in
[`samples/`](samples/).

Everything mapthing keeps lives in `/data`: downloaded extracts, the built generations, the
SQLite database (keys, schedule, history) and caches. The container runs as UID 10001; a named
volume just works, and for a bind mount make the directory writable by that UID
(`chown 10001:10001 /srv/mapthing`) or pass `--user`.

**Sizing.** A build needs roughly six times the size of the selected extracts in free disk, plus
the elevation data, plus about 4 GB once for Planetiler's world-wide land, water and Natural Earth
layers (downloaded on the first build and reused). Two good generations are kept. Planetiler is
given 60% of available memory; large regions (a big US state or province) build comfortably with
8–16 GB of RAM, and the build switches to disk-backed storage when memory is short. The build
refuses to start, with a clear message, when the data volume is too small.

**Environment.**

| Variable | Default | |
|---|---|---|
| `ADMIN_USER`, `ADMIN_PASSWORD` | (required) | the admin login |
| `MAPTHING_PUBLIC_URL` | from the request | external base URL written into styles, TileJSON and the key snippets; set it behind a proxy or tunnel |
| `MAPTHING_TRUSTED_PROXY` | `off` | `on` when every request arrives through a reverse proxy or tunnel: the client address is then taken from `CF-Connecting-IP`, else the first `X-Forwarded-For` entry, for login throttling, rate limits and logs |
| `MAPTHING_DATA` | `/data` | data directory |
| `MAPTHING_LISTEN` | `:8080` | listen address |
| `MAPTHING_WATER_POLYGONS_URL`, `MAPTHING_LAND_POLYGONS_URL` | osmdata.openstreetmap.de | mirrors for Planetiler's ocean/land polygons |
| `MAPTHING_RENDERER` | `on` | `off` runs without the raster renderer (raster tiles answer 404); saves about 150 MiB at idle |
| `MAPTHING_UPDATE_CHECK` | `on` | Once a day the server asks the image registry whether a newer image exists and says so on Overview (it never updates itself). `off` stops that |
| `MAPTHING_UPDATE_IMAGE` | `ghcr.io/jclement/mapthing:<MAPTHING_TAG or latest>` | The image and tag to compare with, if you run a copy from another registry |
| `MAPTHING_LOG_MAX_MB` | `50` | total size of the log files kept in `/data/logs` (the admin Logs page reads them) |
| `MAPTHING_SERVE` | `tiles,routing,elevation` | what this server serves; `routing,elevation` makes a backend that needs no basemap |
| `MAPTHING_MEMORY_GUARD_FACTOR` | `1` | a build asks for 768 MiB plus twice its OSM input (1–2 GiB) of free memory, swap included; below 1 accepts the risk, `0` turns the check off |
| `MAPTHING_DISK_GUARD_FACTOR` | `1` | multiplies the free disk a build asks for; below 1 accepts the risk |

**Building on one machine and serving on another.** `mapthing-build -regions-file … -out DIR`
writes a complete generation to a directory; a server adopts a copy of it with
`POST /admin/api/generations/{id}/activate`. See [docs/operations.md](docs/operations.md).

The image contains Valhalla 3.9.1, Planetiler with the Protomaps basemap profile (lightly patched
for rural roads: see [docker/planetiler/PATCHES.md](docker/planetiler/PATCHES.md)), osmium, GDAL
and the MapLibre Native renderer. Build it yourself with
`docker build -t mapthing:dev -f docker/Dockerfile .`.

## Licence and data attribution

Code: [MIT](LICENSE).

Map data © [OpenStreetMap](https://www.openstreetmap.org/copyright) contributors, available
under the Open Database Licence (ODbL). Elevation: USGS 3D Elevation Program (public domain);
Natural Resources Canada (Open Government Licence – Canada); Province of British Columbia (Open
Government Licence – British Columbia); Government of Alberta (Open Government Licence –
Alberta); Copernicus DEM © DLR e.V. 2010–2014 and © Airbus Defence and Space GmbH 2014–2018,
provided under COPERNICUS by the European Union and ESA. The attribution shown in the map
follows the data actually built into your instance.
