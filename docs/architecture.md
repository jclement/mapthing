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
| GET | `/styles/{name}.json` | MapLibre/Mapbox GL v8 style (`name`: `outdoors`, `outdoors-dark`, `satellite`). Tile, glyph and sprite URLs inside carry the caller's key. Readable by mapbox-gl ≥1.13 and MapLibre. Every style has a layer with id `satellite` (hidden unless the style is `satellite`). |
| GET | `/tiles/basemap.json` | TileJSON for the vector basemap |
| GET | `/tiles/basemap/{z}/{x}/{y}.mvt` | vector tile (Protomaps v4 layers) |
| GET | `/tiles/basemap.pmtiles` | the whole archive, HTTP Range supported (206), for clients that read PMTiles directly |
| GET | `/tiles/terrain/{z}/{x}/{y}.png` | Terrarium-encoded elevation tiles (hillshade / 3D) |
| GET | `/tiles/imagery/{z}/{x}/{y}.jpg` | satellite imagery, proxied from the configured open source and cached |
| GET | `/styles/{name}/{tilesize}/{z}/{x}/{y}[@2x].png` | **raster** tiles rendered from a style (`tilesize` 256 or 512). Mirrors the Mapbox Static Tiles URL shape so swapping is a host change. |
| GET | `/packs/extract.pmtiles?bbox=w,s,e,n&minzoom=&maxzoom=` | offline pack (PMTiles subset of the basemap); `format=mbtiles` also |
| POST | `/api/v1/route` | `{"locations":[{"lat":..,"lon":..},..],"profile":"auto"|"truck","avoid_unpaved":false}` → GeoJSON LineString, `distance_m`, `duration_s`, legs, maneuvers, `surface_breakdown` |
| ANY | `/api/v1/valhalla/{action}` | pass-through to Valhalla (`route`, `isochrone`, `trace_route`, `locate`, …) |
| POST/GET | `/api/v1/elevation` | `{"points":[[lon,lat],..]}` → elevations (m) + source per point |
| POST | `/api/v1/elevation/profile` | `{"path":[[lon,lat],..] or "polyline":"…","interval_m":30}` → samples (distance, lon, lat, elevation, source), `gain_m`, `loss_m`, min/max |
| GET | `/maps/api/elevation/json` | **Google Elevation API compatible** (`locations=` or `path=` + `samples=`; plain or `enc:` polyline). Same response shape. |
| POST | `/directions/v2:computeRoutes` | **Google Routes API compatible** subset (origin, destination, intermediates; `travelMode` DRIVE; `polylineQuality`; returns `routes[].distanceMeters`, `duration`, `polyline.encodedPolyline`). Honours `X-Goog-FieldMask` loosely. |
| POST | `/ors/v2/directions/{profile}/geojson`, `/ors/v2/matrix/{profile}`, `/ors/v2/snap/{profile}`, `/ors/v2/isochrones/{profile}` | **OpenRouteService compatible** subset (`driving-car`, `driving-hgv`), `radiuses` honoured (snap up to 10 km), ORS error codes (2010 no routable point, 2009 no route) and response shapes. Key accepted as `Authorization: <key>` too. |
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

## Attribution and licences

Code: MIT. Data: © OpenStreetMap contributors (ODbL); USGS 3DEP (public domain); NRCan (Open
Government Licence – Canada); Province of BC (Open Government Licence – British Columbia); Alberta
(Open Government Licence – Alberta); Copernicus DEM (© DLR/Airbus, ESA licence). Imagery
attribution depends on the configured imagery source. Attributions appear in every style, the
UI footer and the README.

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
