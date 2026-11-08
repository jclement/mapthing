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
