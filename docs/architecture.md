# mapthing architecture

mapthing is a self-hosted map service in **one container**: vector and raster basemap tiles,
offline packs, routing, and elevation (points and profiles), with an admin web UI to pick
regions, build them, schedule refreshes, issue API keys and watch usage.

This document is the contract between the parts. Change it in the same commit as the code
that changes the contract.

## Process model

```
docker run -v data:/data -e ADMIN_USER -e ADMIN_PASSWORD -p 8080:8080 ghcr.io/jclement/mapthing
            │
            └─ mapthing (Go, PID 1 under tini)  ── HTTP :8080 (API, tiles, admin UI)
                 ├─ supervises: valhalla_service      (routing, localhost only)
                 ├─ supervises: renderer (Node + maplibre-native; raster tiles, localhost only)
                 └─ runs jobs:  download (Geofabrik) → osmium merge → planetiler (Protomaps
                                profile) → valhalla_build_tiles → elevation build → verify → swap
```

`mapthing` is the only process listening on a public port. Valhalla and the renderer bind to
127.0.0.1 and are restarted by the supervisor when they die or when a new generation is
swapped in.

## Data volume (`/data`, env `MAPTHING_DATA`)

```
/data/
  mapthing.db                 SQLite: api keys (hashes), regions, schedule, jobs, history, usage
  downloads/<region>/         latest source extract per region (<region>.osm.pbf + meta.json)
  elevation/cache/<source>/   raw DEM downloads (re-used across builds; MAPTHING_ELEVATION_CACHE overrides)
  cache/planetiler/           Planetiler auxiliary sources (Natural Earth, OSM water/land polygons,
                              landcover, QRank, pgf-encoding), downloaded once with retries, re-used
  cache/valhalla/             timezone database for valhalla_build_tiles
  catalog/                    cached Geofabrik index-v1.json (refreshed weekly) and extract sizes
  work/<gen-id>/              scratch for a build in progress (merged PBF, planetiler tmp, valhalla
                              loose tiles); deleted when the build ends
  generations/<gen-id>/       one complete, self-consistent build of all installed regions
      manifest.json           regions, source dates, sizes, timings, checks passed, and "ocean":
                              "osm" | "approximate:ne-10m" | "missing" (where ocean polygons came from)
      basemap.pmtiles         Protomaps v4 schema vector tiles (z0–15)
      terrain.pmtiles         elevation tiles (see Elevation) for profiles + hillshade
      valhalla/               tiles.tar + valhalla.json (point valhalla_service at this config)
  current -> generations/<gen-id>   atomic symlink swap
  imagery-cache/              cached satellite imagery tiles (bounded LRU)
  logs/
```

A **region** is something you download (a Geofabrik extract such as `us/texas` or
`canada/alberta`, or a custom bbox/polygon clipped from one). A **generation** is one build that
combines every installed region into a single basemap, routing graph and elevation set, so
routes and tiles cross region borders cleanly. Builds happen beside the live generation; the
`current` link only moves after the new generation passes its checks (tiles readable at known
points, a sample route per region succeeds, elevation answers inside each region). On failure
the last good generation stays live and the failure is recorded in history. The last two good
generations are kept; older ones are deleted.

## HTTP API

Everything under `/api/v1`, `/tiles`, `/styles`, `/packs` and the compatibility endpoints needs an
**API key** (`Authorization: Bearer mt_…`, or `?key=mt_…` for tile URLs that browsers fetch).
The admin UI and `/admin/api` use a session cookie from the single admin credential
(`ADMIN_USER` / `ADMIN_PASSWORD`). The admin session may also call every keyed endpoint, so the
UI's own map needs no key. `/healthz` and `/readyz` are open.

| Method | Path | What |
|---|---|---|
| GET | `/styles/{name}.json` | MapLibre/Mapbox GL v8 style (`name`: `outdoors`, `outdoors-dark`, `satellite`, `overlay`, `overlay-dark`, `field`; see docs/themes.md). Tile, glyph and sprite URLs inside carry the caller's key. Readable by mapbox-gl ≥1.13 and MapLibre. Every style has a layer with id `satellite` (hidden unless the style is `satellite`). |
| GET | `/tiles/basemap.json` | TileJSON for the vector basemap |
| GET | `/tiles/basemap/{z}/{x}/{y}.mvt` | vector tile (Protomaps v4 layers) |
| GET | `/tiles/basemap.pmtiles` | the whole archive, HTTP Range supported (206), for clients that read PMTiles directly |
| GET | `/tiles/terrain/{z}/{x}/{y}.png` | Terrarium-encoded elevation tiles (hillshade / 3D) |
| GET | `/tiles/imagery/{z}/{x}/{y}.jpg` | satellite imagery, proxied from the configured open source and cached |
| GET | `/styles/{name}/{tilesize}/{z}/{x}/{y}[@2x].{png\|jpg\|webp}` | **raster** tiles rendered from a style (`tilesize` 256 or 512; any style listed by `GET /styles`). Also the Mapbox Static Tiles shape `/styles/v1/{user}/{name}/tiles/[{tilesize}/]{z}/{x}/{y}[@2x][.{ext}]`, so swapping is a host change: without an extension it answers **jpg for `satellite`, png otherwise** (as Mapbox does). jpg/webp are encoded at quality 80 (a satellite 512@2x tile is 2–2.5 MB as PNG). Cached on disk per generation, style, size, scale and format. |
| GET | `/packs/extract.pmtiles?bbox=w,s,e,n&minzoom=&maxzoom=` | offline pack (PMTiles subset of the basemap); `format=mbtiles` also |
| POST | `/api/v1/route` | `{"locations":[{"lat":..,"lon":..,"radius_m":..},..],"profile":"auto\|truck","avoid_unpaved":false,"avoid_highways","avoid_tolls","avoid_ferries","shortest","alternates":n}` → GeoJSON Feature (LineString) with properties `distance_m`, `duration_s`, `legs[]` (maneuvers), `surface_breakdown` (metres by `paved`/`gravel`/`dirt`/`unknown`), `alternates[]`. Errors: `{"error":{"code":"no_road\|no_route\|limit\|bad_request\|unavailable","message","location_index"}}` (404 for no_road/no_route) |
| GET/POST | `/api/v1/valhalla/{action}` | pass-through to Valhalla; allowlisted actions: `route`, `optimized_route`, `sources_to_targets`, `isochrone`, `trace_route`, `trace_attributes`, `locate`, `height`, `expansion`, `centroid`, `status` |
| POST/GET | `/api/v1/elevation` | `{"points":[[lon,lat],..]}` → elevations (m) + source per point |
| POST | `/api/v1/elevation/profile` | `{"path":[[lon,lat],..] or "polyline":"…","interval_m":30}` → samples (distance, lon, lat, elevation, source), `gain_m`, `loss_m`, min/max |
| GET | `/maps/api/elevation/json` | **Google Elevation API compatible** (`locations=` or `path=` + `samples=`; plain or `enc:` polyline). Same response shape. |
| POST | `/directions/v2:computeRoutes` | **Google Routes API compatible** subset (origin, destination, intermediates as `location.latLng`; `travelMode` DRIVE; `polylineQuality`, `polylineEncoding`, `computeAlternativeRoutes`, `routeModifiers.avoidTolls/avoidHighways/avoidFerries`; returns `routes[].distanceMeters`, `duration`, `staticDuration`, `polyline`, `legs[]`, `viewport`, `routeLabels`; no route → `{}`). Honours `X-Goog-FieldMask` (or `$fields`); missing mask = all fields. `?profile=truck` selects truck costing (Google has no truck mode). |
| POST | `/ors/v2/directions/{profile}` (also `…/json`, `…/geojson`, and GET `?start=&end=`), `/ors/v2/matrix/{profile}[/json]`, `/ors/v2/snap/{profile}[/json]`, `/ors/v2/isochrones/{profile}[/geojson]` | **OpenRouteService compatible** subset (`driving-car`, `driving-hgv`), `radiuses` honoured (snap up to 10 km), ORS error codes (2010 no routable point, 2009 no route) and response shapes. Key accepted as `Authorization: <key>` too. |
| GET | `/healthz`, `/readyz` | liveness; readiness = a generation is live and the subprocesses are up |

Admin (session cookie):

| Method | Path | What |
|---|---|---|
| POST | `/admin/api/login`, `/admin/api/logout` | session |
| GET | `/admin/api/catalog` | regions you can install (id, name, parent, bbox, extract size, elevation sources available) |
| GET/POST/DELETE | `/admin/api/regions[/{id}]` | installed regions; POST `{id}` or `{name, bbox|polygon}` |
| POST | `/admin/api/build` | start a build now (refresh downloads optional) |
| GET | `/admin/api/jobs[/{id}]`, `/admin/api/events` (SSE) | job progress, log tail |
| GET | `/admin/api/generations` | live and kept generations, manifests |
| GET/PUT | `/admin/api/schedule` | refresh schedule (cron expression, default weekly Sunday 03:00 UTC; per-region enable) |
| GET/POST/DELETE | `/admin/api/keys[/{id}]` | API keys: label, created, last used, limits (requests/min, requests/day, allowed origins), usage. The secret is shown once at creation; only a SHA-256 hash is stored. |
| GET | `/admin/api/stats` | tiles/routes/elevation calls by key and by day, cache hit rates, disk per generation and region, RAM, subprocess state |
| GET | `/admin/api/system` | versions, disk free, attribution |

## Go layout

```
cmd/mapthing/         main: config, wiring, supervisor start
internal/config       env → Config
internal/store        SQLite (modernc.org/sqlite, CGO off), migrations
internal/auth         admin session + API key middleware, rate limits
internal/keys         key generation, hashing, limits, usage counters
internal/stats        counters, flushed to SQLite
internal/catalog      region catalog (Geofabrik index + custom), elevation coverage per region
internal/jobs         job runner, progress, logs, SSE fan-out
internal/build        generation builds (download, merge, planetiler, valhalla, elevation, verify, swap)
internal/scheduler    cron → build jobs
internal/supervisor   child processes (valhalla_service, renderer)
internal/tiles        PMTiles reader + z/x/y, TileJSON, styles, packs, imagery proxy, raster proxy
internal/routing      Router interface; Valhalla client; Google Routes compatibility
internal/elevation    Service interface; terrain tile reader; profiles; Google Elevation compatibility
internal/server       HTTP routes, embedding web/dist
web/                  admin UI (React + Vite + Tailwind + MapLibre GL JS), built into web/dist
docker/               Dockerfile, renderer sidecar, style assets, fonts
```

## Elevation

Sources, in priority order where they overlap. A higher-priority source wins wherever it has
data; its nodata falls through to the next. Choices, licences and datums are in
[elevation-sources.md](elevation-sources.md).

1. `hrdem`: NRCan HRDEM 2 m mosaic (LiDAR, patchy), Canada.
2. `3dep13`: USGS 3DEP 1/3 arc-second (~10 m), US.
3. `mrdem`: NRCan MRDEM-30 (30 m, all of Canada; supersedes CDEM, and in practice the open
   Alberta and BC provincial grids).
4. `glo30`: Copernicus GLO-30 (30 m DSM), everywhere else and as gap filler.

Provincial products (BC TRIM/CDED 25 m, Alberta 25 m, LidarBC 1 m) are not used in v1: they
are either not anonymously automatable or not better than the national products at our zoom.

**Build** (`internal/build/terrain*.go`). The pipeline calls the registered
`build.TerrainFunc(ctx, r build.Reporter, regions []catalog.Region, workDir, outPath string)`
(`RegisterTerrain` from `init()`), plus `RegisterTerrainEstimate` (bytes, for the disk guard)
and `RegisterElevationCheck(ctx, terrainPath, pts)` (verify). It needs the GDAL CLI
(`gdalwarp` ≥ 3.6 with LERC, which the official GDAL builds include) in the container. Steps:

- Max zoom: the coarsest zoom whose pixel is ≤ 1.7 × the best source resolution at the area's
  latitude, capped at 13. That gives z13 for 3DEP and HRDEM areas and z12 for 30 m-only areas.
  The zoom is lowered while the max-zoom tile count exceeds `MAPTHING_TERRAIN_MAX_TILES`
  (default 400 000 ≈ 20 GB at the measured ~50 KB per max-zoom tile including lower zooms).
  `MAPTHING_TERRAIN_MAXZOOM` forces a zoom.
- Sources are COGs read by HTTP range. Windows (1° cells clipped to the region bbox plus one max-zoom tile,
  at 0.8 × the max-zoom pixel size, LERC-compressed with ≤ 5 cm error) are cached in
  `/data/elevation/cache/<source>/<cell>_<hash>.tif`, with a `.json` sidecar holding the
  upstream URL, ETag and Last-Modified. A rebuild reuses a window whose ETag is unchanged.
  When the upstream is unreachable, any cached copy is used. Windows are fetched lazily: a
  lower-priority source is fetched only where higher ones left holes.
- Chunks of 16×16 max-zoom tiles are warped to EPSG:3857 (bilinear, float32), composited by
  priority, and cut into tiles. The next four zooms are 2×2 averages of the chunk raster; the
  rest of the pyramid (down to z0) is averaged from those tiles. Averaging is always done on
  elevations, never on encoded RGB. Only tiles touching a region (its polygon if it has one,
  else its bbox) are written.
- Besides the archive, it writes `<outPath>.json`: zooms, tile count, bytes, per-source pixel
  counts, pieces fetched or reused, and duration.

**`terrain.pmtiles`**: tile type PNG, **256 px Terrarium** (`elevation = R·256 + G + B/256 −
32768`). Elevations are rounded before encoding: **0.25 m at max zoom**, doubling for each zoom
out (0.5 m at max−1, 1 m at max−2, …, capped at 8 m), which keeps slope precision per pixel
constant. The rounding step is `MAPTHING_TERRAIN_QUANTUM_M` (default 0.25). Full 1/256 m
precision is incompressible noise and cost 2.3–3.2× more disk; see
[elevation-sources.md](elevation-sources.md#tile-size). Pixels with no source are encoded as 0 m so hillshading stays calm at
coverage edges. Max-zoom tiles also carry a private ancillary PNG chunk `mtSr`
(zlib-compressed 256×256 bytes: per-pixel source index, 0 = no data). Browsers and MapLibre
ignore it; the elevation API uses it for nodata and per-sample `source`. The PMTiles JSON
metadata is `elevation.Metadata`: `format`, `encoding: "terrarium"`, `tile_size: 256`,
`minzoom`, `maxzoom`, `bounds`, `regions`, `attribution`, and `sources[]` (index order = chunk
index − 1: id, name, resolution, licence, attribution, datum, kind dtm/dsm, footprint boxes,
pixel count).

**API** (`internal/elevation`; `Handler` is a `server.Mountable` and wraps every route with usage
kind `elevation`; wired in `cmd/mapthing/features.go`. `elevation.NewTerrainForData(dataDir)`
reads `/data/current/terrain.pmtiles` and reopens it within 2 s of a `current` swap.
`GET /api/v1/elevation/sources` and `/admin/api/system` list only sources that contributed pixels):

- Points and profiles sample the max zoom with bilinear interpolation on pixel centres. A
  neighbour without data is dropped and the remaining weights renormalised; if there are no
  valid neighbours the result is `elevation_m: null`. `resolution_m` = max(source resolution,
  max-zoom pixel size at that latitude).
- `GET /api/v1/elevation?points=lon,lat|lon,lat` and `POST {"points":[[lon,lat],…]}` (≤ 10 000)
  → `{"results":[{lon,lat,elevation_m,source,resolution_m}],"sources":[…]}`.
- `POST /api/v1/elevation/profile` with `{"path":[[lon,lat],…]}` or `{"polyline":"…",
  "precision":5|6}`, plus `interval_m` (default 30, vertices kept), or `samples` (exactly N,
  Google semantics), and `hysteresis_m` (default 2; negative = off). Returns `samples[]` (with
  `distance_m`), `length_m`, `gain_m`/`loss_m` (hysteresis-smoothed), `gain_raw_m`/`loss_raw_m`,
  `min_m`, `max_m`, `sources`. Distances are great-circle; at most 10 000 samples (the interval
  grows to fit).
- `GET /api/v1/elevation/sources` → sources in the live archive.
- `GET /maps/api/elevation/json`: Google-compatible (`locations=` / `path=`+`samples=` ≤ 512,
  `lat,lng|…` or `enc:`). Errors are `INVALID_REQUEST` (HTTP 400). Google's shape has no null,
  so an uncovered point returns `elevation: 0, resolution: -1`; if no point is covered (or no
  generation is live) the status is `DATA_NOT_AVAILABLE`.
- Heights are each source's native orthometric datum (NAVD88 / CGVD2013 / EGM2008), with no
  geoid conversion. US values read ≈ 1 m below EGM96-based answers; Canadian values are within
  ≈ ±0.5 m.
- `elevation.CoverageIDs(bbox)` / `CoverageFor(bbox)`: static, offline list of the sources
  expected in a bbox, for the catalog.

## Routing

Valhalla, built from the same merged extract. Profiles: `auto` and `truck` by default;
`track` and `unpaved` roads are allowed (oilfield lease roads matter), with penalties instead of
exclusions, and the response reports a surface breakdown so a client can tell paved from
gravel/dirt.

- Engine: Valhalla 3.9.1. Config = `valhalla_build_config` output deep-merged with
  `docker/valhalla/overrides.json`; the image carries `docker/valhalla/` at
  `/usr/share/mapthing/valhalla/` (the overrides reference `default_speeds.json` there).
  Build steps, paths and the reason for each override: `docker/valhalla/README.md`.
- valhalla_service listens on `127.0.0.1:8002`; the Go client reads `MAPTHING_VALHALLA_URL`
  (default `http://127.0.0.1:8002`).
- Tracks get `use_tracks` 0.3 (penalised, still routable); `avoid_unpaved` sets
  `exclude_unpaved` (unpaved still allowed at the first and last edge, so a lease site stays
  reachable). Private tracks are destination-only.
- Surface breakdown: `trace_attributes` over the route's own shape (edge walk, falling back to
  map matching), Valhalla surfaces mapped to paved / gravel (compacted, gravel) / dirt (dirt,
  path) / unknown.
- Wiring: `routing.NewHandlers(routing.New(url)).Mount(mux, wrap)` where
  `wrap func(kind string, h http.Handler) http.Handler` applies auth and accounting; kinds are
  `route`, `matrix`, `isochrone`, `snap`, `valhalla`.

## Attribution and licences

Code: MIT. Data: © OpenStreetMap contributors (ODbL); USGS 3DEP (public domain); NRCan (Open
Government Licence – Canada); Province of BC (Open Government Licence – British Columbia); Alberta
(Open Government Licence – Alberta); Copernicus DEM (© DLR/Airbus, ESA licence). Imagery
attribution depends on the configured imagery source. Attributions appear in every style, the
UI footer and the README.

## Go contracts between parts

**Jobs** (`internal/jobs`). A job is `func(ctx context.Context, j *jobs.Job) error`
registered by name with `runner.Register(name, f)`; one job runs at a time, the rest queue.
`*jobs.Job` satisfies the pipeline's `build.Reporter`: `Stage(name string)` starts a stage
(progress resets to 0), `Progress(frac float64, msg string)` reports 0..1 within the stage,
`Logf(format, args...)` appends a persisted log line. `j.Context()`, `j.Decode(&params)` and
`j.Params()` give the context and the JSON params it was enqueued with. Progress and log lines
stream to the admin UI over `/admin/api/events` (SSE events `job`, `log`). A job error is
recorded in `history`; `runner.Details` turns a structured error (such as `*build.StageError`)
into history details. The build job is named `build`, params `{"refresh": bool}`; the
scheduler enqueues it with `{"refresh": true}`.

**Mounting feature handlers** (`internal/server`). Routing and elevation expose a value with
`Mount(mux *http.ServeMux, wrap func(kind string, h http.Handler) http.Handler)` and register
each keyed path as `mux.Handle(pattern, wrap(kind, h))`; `wrap` applies API-key/session auth,
per-key limits, and usage counting under `kind` (`route`, `elevation`, `compat`). Patterns use
Go 1.22 mux syntax; a wildcard is a whole path segment.

**Generation swaps.** Core polls the `current` symlink (every 2 s) and, when its target
changes, reopens the PMTiles archives, drops raster caches of old generations and restarts the
supervised children. The build's `Options.Restart` hook is wired to the same swap handler, so a
swap takes effect immediately.

**Imagery source** (`internal/tiles/imagery_sources.go`, style worker):
`func ImageryUpstream(z, x, y int) (url string, ok bool)` returns the upstream tile URL
(`ok=false` = not configured / outside coverage) and `func ImageryAttribution() string`.

**Feature wiring.** `cmd/mapthing/features.go` is the single place that imports the build
pipeline, catalog, routing and elevation packages and plugs them into `server.Deps`
(`Mounts`, `Catalog`, `ValidateRegion`, `ElevationSources`, `ReadyChecks`) and the job runner
(`Register("build", …)`, `runner.Details` for `*build.StageError`).

**Internal requests (renderer).** The renderer fetches `/styles/{name}.json?internal=1`,
tiles, glyphs and sprites from `http://127.0.0.1:<port>` with header
`X-Mapthing-Internal: <token>`. Core accepts such requests only from a loopback address with
the right token, needs no API key for them, templates no key into URLs, uses the request's own
host (not `MAPTHING_PUBLIC_URL`) as base URL, and counts them as usage kind
`raster-internal`. The token is `MAPTHING_INTERNAL_TOKEN`, or random per start; it reaches the
renderer child as env `MAPTHING_INTERNAL_TOKEN`, with `MAPTHING_URL` (the local base URL).

**Styles.** `GET /styles` (keyed) and `/admin/api/system` (`styles`) list
`[{id, name, description, dark}]` from `assets/styles/*.json` (`metadata["mapthing:name"]`,
`["mapthing:description"]`, optional `["mapthing:dark"]`, else an id ending in `-dark`). The raster
endpoints accept any embedded style. Core appends the key query to the style's `sprite` URL
when the style does not template it, and with `MAPTHING_IMAGERY_URL` set replaces
`sources.imagery.attribution` with `tiles.ImageryAttribution()`. Glyph ranges missing from
every font in the stack answer 404.

**Configuration** (environment): `MAPTHING_DATA`, `MAPTHING_LISTEN`, `ADMIN_USER`,
`ADMIN_PASSWORD`, `MAPTHING_PUBLIC_URL`, `MAPTHING_VALHALLA_URL` (default
`http://127.0.0.1:8002`), `MAPTHING_VALHALLA_CMD`, `MAPTHING_RENDERER_URL` (default
`http://127.0.0.1:8081`), `MAPTHING_RENDERER_CMD` (default `node /opt/renderer/server.js`),
`MAPTHING_RENDERER_HEALTH_URL` (default renderer URL + `/health`), `MAPTHING_RASTER_CACHE_MB`
(2048), `MAPTHING_IMAGERY_CACHE_MB` (4096), `MAPTHING_PACK_CACHE_MB` (4096),
`MAPTHING_PACK_MAX_TILES` (2000000), `MAPTHING_INTERNAL_TOKEN`, `MAPTHING_IMAGERY_URL`,
`MAPTHING_DEBUG`. Caches live in `/data/raster-cache`, `/data/imagery-cache`, `/data/pack-cache`.

**Errors.** Keyed and admin errors are JSON `{"error":{"code","message"}}`, except under
`/maps/api/` (Google Elevation shape), `/directions/v2` (Google RPC shape) and `/ors/` (ORS
shape). An API key is also accepted as `?api_key=` (ORS GET style). Admin shapes: next section.
## Renderer and internal requests

The renderer (`docker/renderer/`, Node 24 + MapLibre Native on a private Xvfb display; Bun cannot
load the addon, see its README) listens on `127.0.0.1:8081`:
`GET /render/{style}/{size}/{z}/{x}/{y}[@2x].{png|jpg|webp}` (encoded with sharp; jpeg and webp
quality 80), `GET /healthz` (the supervisor's health probe), `POST /reload`. mapthing proxies
the raster endpoints to it.

The renderer reads styles, tiles, glyphs and sprites from mapthing itself. Those requests come
from localhost with the header `X-Mapthing-Internal: $MAPTHING_INTERNAL_TOKEN` and need no API
key. `MAPTHING_INTERNAL_TOKEN` is a secret shared by mapthing and the renderer: the supervisor
generates a random one at start when it is unset and passes it to the child. mapthing accepts
the header only from loopback addresses. `GET /styles/{name}.json?internal=1` returns the style
with `{{KEY_QS}}` empty. The renderer sends every `/tiles/`, `/fonts/`, `/sprites/`, `/styles/`
URL to `MAPTHING_URL` (default `http://127.0.0.1:8080`) whatever host the style names.

Styles are generated from themes (`assets/styles/src/`, `mise run styles`); see `docs/styles.md`.
Imagery sources and licences: `docs/imagery.md`.
## Admin UI and admin API shapes

The admin UI is served at **`/admin/`** (Vite `base: "/admin/"`, React Router basename `/admin`)
from the embedded `web/dist`: files as they are (`/admin/assets/…` hashed, `Cache-Control:
public, max-age=31536000, immutable`), and `index.html` (`no-cache`) for any other extension-less
path under `/admin/` (client routes such as `/admin/keys`); `/admin/api/…` is the API. `/` and
`/admin` redirect (302) to `/admin/`. The UI probes the session with `GET /admin/api/system`
(401 → login form) and lists map styles with `GET /styles`. Style thumbnails are one raster tile:
`/styles/{id}/256/{z}/{x}/{y}.png`.

`web/src/api.ts` is the typed client; this list and it must agree. Ids of keys and jobs are
strings. Fields marked *extra* are additions the UI ignores. JSON bodies ignore unknown fields.

- Errors: non-2xx with `{"error":{"code":"…","message":"…"}}`.
- `POST /admin/api/login` `{username,password}` → `{user,expires_at}` + HttpOnly session cookie;
  401 bad credentials, 429 after 5 failures in 5 min, 403 cross-origin. `POST /admin/api/logout` → 204.
  `GET /admin/api/session` (open) → `{authenticated}`.
- `GET /admin/api/catalog` → `{entries:[{id,name,parent,bbox:[w,s,e,n],extract_bytes,elevation_sources:[id…]}], elevation_sources:[{id,name,resolution_m,license,attribution}]}`
  (pipeline's catalog; 503 when not wired). Grouping nodes have `extract_bytes: 0`.
- `GET /admin/api/regions` → `{regions:[region]}`; region = `{id,name,catalog_id|null,bbox,polygon?:[[lon,lat]…],status:"pending"|"installed"|"failed",extract_bytes,disk_bytes,data_date|null,downloaded_at|null,elevation_sources:[id…]}` + *extra* `parent,refresh,added_at`.
  `status`: installed = in the live generation's manifest; failed = not live and the latest finished build failed; else pending.
  `data_date`/`downloaded_at`/`disk_bytes` come from `downloads/<id>/meta.json` and the download size; `extract_bytes`/`elevation_sources` from the catalog at add time.
  `POST` `{id}` (catalog extract) or `{name,bbox}` / `{name,polygon:[[lon,lat]…]}` (custom; id `custom/<slug>`) → 201 region; 409 if the id exists.
  `GET`/`PATCH {refresh}`/`DELETE` `/admin/api/regions/{id}` (ids contain `/`, may be URL-encoded) → region / region / 204.
- `POST /admin/api/build` `{refresh_downloads}` → 202 job; 409 if a build is queued or running; 503 if the build job is not registered.
- Job: `{id,name,status:"queued"|"running"|"succeeded"|"failed"|"cancelled",trigger:"manual"|"schedule",refresh_downloads,created_at,started_at?,finished_at?,progress:0..1,stages:[{name,status:"pending"|"running"|"done"|"failed"|"skipped",progress,detail?,started_at?,finished_at?}],error?,generation_id?}`.
  Stages are the pipeline's, all listed from the moment a build is queued: `guard, download, merge, basemap, valhalla, terrain, verify, swap, retention`;
  stages never reached in a successful build end `skipped`. `progress` is the mean over stages.
  `GET /admin/api/jobs` → `{jobs:[…]}` newest first; `GET /admin/api/jobs/{id}` → job + `log:[{job_id,t,level:"debug"|"info"|"warn"|"error",msg}]` (tail, `?tail=n`);
  `POST /admin/api/jobs/{id}/cancel` → 204 (409 if not queued/running).
- `GET /admin/api/events` (SSE): named events `job` (data: job), `log` (data: log line), `generation` (data: `{id,previous}` when a generation goes live). The current job is sent on connect.
- `GET /admin/api/generations` → `{generations:[{id,created_at,status:"live"|"kept",manifest:{regions:[{id,name,data_date,bbox}],sizes:{basemap,terrain,routing,total},timings:{stage:seconds},checks:[{name,ok,detail?}],raw}}]}`
  (generations on disk; `manifest` is normalised from the pipeline's `manifest.json`, which is in `raw`; sizes are measured on disk).
- `GET/PUT /admin/api/schedule` ↔ `{cron,enabled,refresh_downloads,regions:[{id,enabled}]}` (cron 5-field, UTC; PUT fields optional, 400 `bad_cron`)
  + *extra* `next_run,last_run,upcoming:[5 times],default_cron,timezone,history`.
- `GET /admin/api/keys` → `{keys:[{id,label,prefix,created_at,last_used_at|null,limits:{rpm|null,rpd|null,origins:[]},usage:{today,total,daily:[14 numbers, oldest first]}}]}` (revoked keys left out; `?revoked=1` includes them with `revoked_at`).
  `POST` `{label,rpm?,rpd?,origins?}` (null = unlimited) → 201 `{key,secret}` (the secret only here); `GET`/`PATCH` (same fields) `/admin/api/keys/{id}` → key; `DELETE` → 204 (revoked at once).
- `GET /admin/api/stats?days=30` → `{daily:[{date,tiles,raster,routes,elevation,packs}],by_key:[{key_id,label,requests}],cache:[{name,hits,misses,size_bytes}],disk:{generations:[{id,bytes}],regions:[{id,name,bytes}],downloads_bytes,elevation_cache_bytes,imagery_cache_bytes,free_bytes,total_bytes},memory:{process_rss_bytes,system_used_bytes,system_total_bytes},processes:[{name,state:"running"|"starting"|"restarting"|"stopped",pid|null,uptime_s,restarts,rss_bytes}]}`
  (+ *extra* disk `raster_cache_bytes,pack_cache_bytes,database_bytes`, process `detail,healthy`). Usage kinds map to buckets: tile→tiles, raster→raster, pack→packs, elevation→elevation, everything else (route, matrix, snap, isochrone, valhalla, compat)→routes; `raster-internal` is not shown. `key_id` "0" is the admin UI.
- `GET /admin/api/system` → `{version,commit,build_date,components:[{name,version}],ready,readiness:[{name,ok,detail?}],disk:{free_bytes,total_bytes},current_generation|null,attribution:[{name,license,url?,note?}],imagery_source:{name,attribution}|null}`
  + *extra* `styles:[{id,name,description,dark}],elevation_sources,platform,data_dir,process`.
- `GET /admin/api/history?kind=&limit=` → `{history:[{id,at,kind,ok,message,job_id?,generation?,details?}]}`; `POST /admin/api/system/restart/{name}` → 204.
- Keyed endpoints as the UI calls them: `POST /api/v1/route` → GeoJSON Feature (`geometry` LineString, `properties` = `{distance_m,duration_s,surface_breakdown_m,legs}`); the client flattens it.
  Route errors use codes `no_road` / `no_route`. `POST /api/v1/elevation` → `{results:[sample]}`; `POST /api/v1/elevation/profile` → the `elevation.Profile` JSON.

- Errors: non-2xx with `{"error":{"code":"…","message":"…"}}` (a plain string `error` is accepted too).
- `POST /admin/api/login` `{username,password}` → `{user}`; 401 on bad credentials. `POST /admin/api/logout` → 204.
- `GET /admin/api/catalog` → `{entries:[{id,name,parent,bbox:[w,s,e,n],extract_bytes,elevation_sources:[id…]}], elevation_sources:[{id,name,resolution_m,license,attribution}]}`.
  Grouping nodes (continent, country without its own extract) have `extract_bytes: 0`.
- `GET /admin/api/regions` → `{regions:[{id,name,catalog_id|null,bbox,polygon?,status:"pending"|"installed"|"failed",extract_bytes,disk_bytes,data_date|null,downloaded_at|null,elevation_sources:[id…]}]}`;
  `POST` `{id}` or `{name,bbox|polygon}` → 201 region (409 if already installed); `DELETE /{id}` → 204 (ids contain `/`, URL-encoded).
- `POST /admin/api/build` `{refresh_downloads}` → 202 job.
- Job: `{id,status:"queued"|"running"|"succeeded"|"failed"|"cancelled",trigger:"manual"|"schedule",refresh_downloads,created_at,started_at?,finished_at?,progress:0..1,stages:[{name,status:"pending"|"running"|"done"|"failed"|"skipped",progress,detail?,started_at?,finished_at?}],error?,generation_id?}`;
  stage names `download, merge, basemap, routing, elevation, verify, swap`.
  `GET /admin/api/jobs` → `{jobs:[…]}` newest first; `GET /admin/api/jobs/{id}` → job + `log:[{job_id,t,level,msg}]` (tail).
- `GET /admin/api/events` (SSE): named events `job` (data: job), `log` (data: log line), `generation` (data: `{id}` when a generation goes live).
- `GET /admin/api/generations` → `{generations:[{id,created_at,status:"live"|"kept",manifest:{regions:[{id,name,data_date}],sizes:{basemap,terrain,routing,total},timings:{stage:seconds},checks:[{name,ok,detail?}]}}]}`.
- `GET/PUT /admin/api/schedule` ↔ `{cron,enabled,refresh_downloads,regions:[{id,enabled}]}` (cron is 5-field, UTC).
- `GET /admin/api/keys` → `{keys:[{id,label,prefix,created_at,last_used_at|null,limits:{rpm|null,rpd|null,origins:[]},usage:{today,total,daily:[14 numbers, oldest first]}}]}`;
  `POST` `{label,rpm?,rpd?,origins?}` → 201 `{key,secret}`; `DELETE /{id}` → 204.
- `GET /admin/api/stats` → `{daily:[{date,tiles,raster,routes,elevation,packs}],by_key:[{key_id,label,requests}],cache:[{name,hits,misses,size_bytes}],disk:{generations:[{id,bytes}],regions:[{id,name,bytes}],downloads_bytes,elevation_cache_bytes,imagery_cache_bytes,free_bytes,total_bytes},memory:{process_rss_bytes,system_used_bytes,system_total_bytes},processes:[{name,state,pid|null,uptime_s,restarts,rss_bytes}]}`.
- `GET /admin/api/system` → `{version,commit,build_date,components:[{name,version}],ready,readiness:[{name,ok,detail?}],disk:{free_bytes,total_bytes},current_generation|null,attribution:[{name,license,url?,note?}],imagery_source:{name,attribution,url?}|null}`.
- Keyed endpoints as the UI calls them: `POST /api/v1/route` returns `geometry` as a GeoJSON LineString plus `surface_breakdown_m` (`paved|gravel|dirt|unknown` → metres);
  route errors use codes `no_road` / `no_route`. `POST /api/v1/elevation` → `{results:[sample]}`; `POST /api/v1/elevation/profile` → the `elevation.Profile` JSON.


## Build pipeline interfaces

`internal/build.Builder.Run(ctx, rep, Options)` runs one generation build; `rep` is a
`build.Reporter` (`Stage`, `Progress`, `Logf`; `*jobs.Job` satisfies it). Failure returns a
`*build.StageError{Stage, Err, LogTail}`. Regions are `catalog.Region{ID, Name, BBox, Polygon, …}`;
`Polygon` is lon/lat rings read with the even-odd rule (Geofabrik boundary, or the drawn custom
polygon). The elevation package plugs in with `build.RegisterTerrain`,
`build.RegisterTerrainEstimate` and `build.RegisterElevationCheck` from an `init()` in
`internal/build/terrain*.go`. Valhalla settings: `docker/valhalla/overrides.json` is deep-merged over
`valhalla_build_config` output. Planetiler runs the Protomaps profile with the patches in
`docker/planetiler/` (see PATCHES.md: `surface`, `surface_raw`, `tracktype`, `access` attributes on
roads; private and rural service roads at their normal zooms).

When osmdata.openstreetmap.de is unreachable after retries (or `MAPTHING_WATER_POLYGONS_URL` /
`MAPTHING_LAND_POLYGONS_URL` mirrors fail), the build uses Natural Earth 10m ocean/land clipped to
the regions (manifest `ocean: approximate:ne-10m`); if that fails too it builds without high-zoom
ocean (`ocean: missing`). The admin UI should surface a non-`osm` value.
