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
  elevation/cache/<source>/   raw DEM downloads (re-used across builds)
  generations/<gen-id>/       one complete, self-consistent build of all installed regions
      manifest.json           regions, source dates, sizes, timings, checks passed
      basemap.pmtiles         Protomaps v4 schema vector tiles (z0–15)
      terrain.pmtiles         elevation tiles (see Elevation) for profiles + hillshade
      valhalla/               valhalla tiles + valhalla.json
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
| GET | `/styles/{name}/{tilesize}/{z}/{x}/{y}[@2x].png` | **raster** tiles rendered from a style (`tilesize` 256 or 512). Mirrors the Mapbox Static Tiles URL shape so swapping is a host change. |
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

Sources, in priority order where they overlap (the build records which source each terrain tile
came from, and the API returns the source for each sample):

- **US:** USGS 3DEP. The seamless 1/3 arc-second DEM (~10 m) everywhere; 1 m where it exists
  for small regions (optional, large).
- **Canada, national:** NRCan HRDEM (LiDAR-derived, 1–2 m) where it exists; otherwise MRDEM
  (30 m) or CDEM.
- **Alberta / BC:** provincial open DEMs where they beat the national grid (BC TRIM 25 m, LidarBC
  where open; Alberta's open products).
- **Everywhere else:** Copernicus GLO-30.

The build reprojects the chosen sources to Web Mercator and writes `terrain.pmtiles` as
Terrarium PNG tiles at a zoom matched to the best source resolution available in the region.
Profiles sample terrain tiles with bilinear interpolation. The same archive drives hillshade in
the styles.

## Routing

Valhalla, built from the same merged extract. Profiles: `auto` and `truck` by default;
`track` and `unpaved` roads are allowed (oilfield lease roads matter), with penalties instead of
exclusions, and the response reports a surface breakdown so a client can tell paved from
gravel/dirt.

- Engine: Valhalla 3.9.1. Config = `valhalla_build_config` output deep-merged with
  `docker/valhalla/overrides.json`; the image carries `docker/valhalla/` at
  `/usr/local/share/mapthing/valhalla/` (the overrides reference `default_speeds.json` there).
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

**Admin API additions.** `GET /admin/api/session` (open; `{authenticated}`),
`PATCH /admin/api/keys/{id}` (label, limits), `DELETE /admin/api/keys/{id}` revokes (the row
stays for history), `POST /admin/api/jobs/{id}/cancel`, `GET /admin/api/history?kind=`,
`PATCH /admin/api/regions/{id}` (`{refresh}`), `POST /admin/api/system/restart/{name}`.
Region ids may contain `/` (`us/texas`). Keyed errors are JSON `{"error":{"code","message"}}`,
except under `/maps/api/` (Google Elevation shape), `/directions/v2` (Google RPC shape) and
`/ors/` (ORS shape). An API key is also accepted as `?api_key=` (ORS GET style).
## Renderer and internal requests

The renderer (`docker/renderer/`, Node 24 + MapLibre Native on a private Xvfb display; Bun cannot
load the addon, see its README) listens on `127.0.0.1:8081`:
`GET /render/{style}/{size}/{z}/{x}/{y}[@2x].png`, `GET /healthz`, `POST /reload`. mapthing
proxies `/styles/{name}/{size}/{z}/{x}/{y}[@2x].png` to it.

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

The admin UI is served at **`/admin/`** (Vite `base: "/admin/"`, React Router basename `/admin`).
The Go server serves `web/dist` there and falls back to `web/dist/index.html` for any unknown path
under `/admin/` that is not `/admin/api/…` (client-side routes such as `/admin/keys`); `/` may
redirect to `/admin/`. The UI probes the session with `GET /admin/api/system` (401 → login form)
and lists map styles with `GET /styles` (`[{id,name,description,dark}]`). Style thumbnails are one
256 px raster tile: `/styles/{id}/256/{z}/{x}/{y}.png`.

`web/src/api.ts` is the typed client and the reference for these JSON shapes. Summary:

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
