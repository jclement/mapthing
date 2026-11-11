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

The build and the serving can also run on different machines: `mapthing-build -out` writes a
generation to a directory, a server adopts a copy of it, and a server can run without the
renderer or without a basemap (`MAPTHING_RENDERER`, `MAPTHING_SERVE`). See
[operations.md](operations.md).

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
      contours.pmtiles        contour lines (MVT, z10–12) built from terrain.pmtiles (see Contours)
      terrain.pmtiles.json    terrain build summary, with the fingerprint used to reuse it
      contours.pmtiles.json   which terrain and setting the contours were built from
      valhalla/               tiles.tar + valhalla.json (the build's config; the server writes a
                              per-instance copy pointing at this directory, wherever it is)
  generations/.<anything>/    not a generation: a copy in flight (never listed, activated or pruned)
  generations/.retention.json which generations this server owns (built here, or activated here);
                              retention never touches any other directory
  current -> generations/<gen-id>   atomic symlink swap
  imagery-cache/              cached satellite imagery tiles (bounded LRU)
  logs/                       the server's log: log-current.jsonl and rotated log-<id>.jsonl
                              segments, bounded in total by MAPTHING_LOG_MAX_MB (see Logs)
```

A **region** is something you download (a Geofabrik extract such as `us/texas` or
`canada/alberta`, or a custom bbox/polygon clipped from one). A **generation** is one build that
combines every installed region into a single basemap, routing graph and elevation set, so
routes and tiles cross region borders cleanly. Builds happen beside the live generation; the
`current` link only moves after the new generation passes its checks (tiles readable at known
points, a sample route per region succeeds, elevation answers inside each region). On failure
the last good generation stays live and the failure is recorded in history. The last two good
generations are kept; older ones are deleted.

A generation directory is self-contained and relocatable. One built on another machine and
copied to `generations/<gen-id>/` (the id in its manifest) becomes live through
`POST /admin/api/generations/{id}/activate` or `mapthing adopt`, which validate it first; the
same retention applies. Retention, after a build or an activation, only considers generations
this server owns (recorded in `generations/.retention.json`: built here to the end, or made
live here), so a directory being copied in is never deleted and never takes the rollback
slot. A generation must be a real directory of regular files: symbolic links are refused
([operations.md](operations.md)).

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
| GET | `/tiles/contours.json`, `/tiles/contours/{z}/{x}/{y}.mvt`, `/tiles/contours.pmtiles` | contour lines (layer `contours`: `ele` in metres, `major`), served like the basemap; 404 when the live generation has none |
| GET | `/tiles/imagery/{z}/{x}/{y}.jpg` | satellite imagery, proxied from the configured open source and cached |
| GET | `/styles/{name}/{tilesize}/{z}/{x}/{y}[@2x].{png\|jpg\|webp}` | **raster** tiles rendered from a style (`tilesize` 256 or 512; any style listed by `GET /styles`). Also the Mapbox Static Tiles shape `/styles/v1/{user}/{name}/tiles/[{tilesize}/]{z}/{x}/{y}[@2x][.{ext}]`, so swapping is a host change: without an extension it answers **jpg for `satellite`, png otherwise** (as Mapbox does). jpg/webp are encoded at quality 80 (a satellite 512@2x tile is 2–2.5 MB as PNG). Cached on disk per generation, style, the style's revision (a hash of its source, so an update that changes a style does not serve tiles drawn with the old one), size, scale and format. |
| GET | `/packs/extract.pmtiles?bbox=w,s,e,n&minzoom=&maxzoom=` | offline pack (PMTiles subset of the basemap); `format=mbtiles` also |
| POST | `/api/v1/route` | `{"locations":[{"lat":..,"lon":..,"radius_m":..},..],"profile":"auto\|truck","avoid_unpaved":false,"avoid_highways","avoid_tolls","avoid_ferries","shortest","alternates":n}` → GeoJSON Feature (LineString) with properties `distance_m`, `duration_s`, `legs[]` (maneuvers), `surface_breakdown` (metres by `paved`/`gravel`/`dirt`/`unknown`), `alternates[]`. Errors: `{"error":{"code":"no_road\|no_route\|limit\|bad_request\|unavailable","message","location_index"}}` (404 for no_road/no_route) |
| GET/POST | `/api/v1/valhalla/{action}` | pass-through to Valhalla; allowlisted actions: `route`, `optimized_route`, `sources_to_targets`, `isochrone`, `trace_route`, `trace_attributes`, `locate`, `height`, `expansion`, `centroid`, `status` |
| POST/GET | `/api/v1/elevation` | `{"points":[[lon,lat],..]}` → elevations (m) + source per point |
| POST | `/api/v1/elevation/profile` | `{"path":[[lon,lat],..] or "polyline":"…","interval_m":30}` → samples (distance, lon, lat, elevation, source), `gain_m`, `loss_m`, min/max |
| GET | `/maps/api/elevation/json` | **Google Elevation API compatible** (`locations=` or `path=` + `samples=`; plain or `enc:` polyline). Same response shape. |
| POST | `/directions/v2:computeRoutes` | **Google Routes API compatible** subset (origin, destination, intermediates as `location.latLng`; `travelMode` DRIVE; `polylineQuality`, `polylineEncoding`, `computeAlternativeRoutes`, `routeModifiers.avoidTolls/avoidHighways/avoidFerries`; returns `routes[].distanceMeters`, `duration`, `staticDuration`, `polyline`, `legs[]`, `viewport`, `routeLabels`; no route → `{}`). Honours `X-Goog-FieldMask` (or `$fields`); missing mask = all fields. `?profile=truck` selects truck costing (Google has no truck mode). |
| POST | `/ors/v2/directions/{profile}` (also `…/json`, `…/geojson`, and GET `?start=&end=`), `/ors/v2/matrix/{profile}[/json]`, `/ors/v2/snap/{profile}[/json]`, `/ors/v2/isochrones/{profile}[/geojson]` | **OpenRouteService compatible** subset (`driving-car`, `driving-hgv`), `radiuses` honoured (snap up to 10 km), ORS error codes (2010 no routable point, 2009 no route) and response shapes. Key accepted as `Authorization: <key>` too. |
| GET | `/healthz`, `/readyz` | liveness; readiness = a generation is live and the subprocesses are up. With `MAPTHING_RENDERER=off` there is no renderer to be up; with `MAPTHING_SERVE` only the served parts count (a routing and elevation backend is ready without a basemap). |

Admin (session cookie):

| Method | Path | What |
|---|---|---|
| POST | `/admin/api/login`, `/admin/api/logout` | session |
| GET | `/admin/api/catalog` | regions you can install (id, name, parent, bbox, extract size, elevation sources available) |
| POST | `/admin/api/catalog/estimate` | `{bbox}` or `{polygon:[[lon,lat],…]}` → the extracts a custom region would be clipped from: `{parents:[{id,name,bytes,estimated?}], total_bytes, estimated?, single_best?, elevation_sources}`; 400 `not_covered` |
| GET/POST/DELETE | `/admin/api/regions[/{id}]` | installed regions; POST `{id}` or `{name, bbox|polygon}` |
| POST | `/admin/api/build` | start a build now (refresh downloads optional) |
| GET | `/admin/api/jobs[/{id}]`, `/admin/api/events` (SSE) | job progress, log tail |
| GET | `/admin/api/generations` | every generation directory with its state (live, previous, kept, building, incoming, invalid), manifest and size, and the disk they use together |
| DELETE | `/admin/api/generations/{id}` | delete a generation directory for good. 204; 404 `no_generation`; 409 `live` (the one being served: activate another first) or `busy` (a build is writing it, or it is being activated). Unlike retention it also deletes a generation this server does not own. |
| POST | `/admin/api/generations/{id}/activate` | validate `<data>/generations/{id}` and make it live: routing graph up as a standby, the atomic swap, then the routing switch (the standby is discarded if the swap fails), then retention. 200 `{id, status:"live", already_live, previous, removed[]}`; idempotent. Refusals leave the live generation serving: 404 `no_generation`, 409 `busy`, 422 `invalid_generation` (`error.problems[]`) or `routing_failed`, 500 if `current` cannot be moved. |
| GET/PUT | `/admin/api/schedule` | refresh schedule (cron expression, default weekly Sunday 03:00 UTC; per-region enable) |
| GET/POST/DELETE | `/admin/api/keys[/{id}]` | API keys: label, created, last used, limits (requests/min, requests/day, allowed origins), usage. The secret is shown once at creation; only a SHA-256 hash is stored. |
| GET | `/admin/api/stats` | tiles/routes/elevation calls by key and by day, cache hit rates, disk per generation and region, RAM, subprocess state |
| GET | `/admin/api/logs`, `/admin/api/logs/stream` (SSE), `/admin/api/logs/download` | the server's log: filtered pages, a live tail, and the filtered view as a file (see Logs) |
| GET | `/admin/api/overview` | the admin home page in one call: what is live, process health, traffic and latency by kind, memory, CPU, disk against the build guard, next and last build |
| GET | `/admin/api/settings` | the effective configuration, read-only: every environment variable mapthing reads with its value, whether it was set or is the default, secrets masked; and the software versions |
| GET | `/admin/api/system` | versions, disk free, attribution, and the slim-mode settings (`renderer`: `on`/`off`, `serve`: the served parts) |

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
internal/logs         the one log: capture, redaction, ring buffer, rotating files, queries, live tail
internal/pmtiles      the one PMTiles v3 implementation: reader (leaf cache), writer (any order,
                      dedupe, in-place Create/Commit or spooled Finish to an io.Writer), extract
internal/tiles        z/x/y serving, TileJSON, styles, packs, imagery proxy, raster proxy
internal/routing      Router interface; Valhalla client; Google Routes compatibility
internal/elevation    Service interface; terrain tile reader; profiles; Google Elevation compatibility
internal/contours     terrain.pmtiles → contours.pmtiles (marching squares, MVT encoder)
internal/server       HTTP routes, embedding web/dist
web/                  admin UI (React + Vite + Tailwind + MapLibre GL JS), built into web/dist
docker/               Dockerfile, renderer sidecar, style assets, fonts
```

## Logs

Everything the container prints goes through one sink (`internal/logs`), wired in
`cmd/mapthing/main.go`:

| Source | What |
|---|---|
| `mapthing` | the server's own `slog` output (the sink's `slog.Handler` is the default logger) |
| `renderer`, `valhalla@<port>` | each supervised child's stdout and stderr (`supervisor.Supervisor.Output`), plus a line from the supervisor when the child starts, cannot start, or exits (level `error`, with the reason and the restart delay) |
| `xvfb` | the renderer's X server; `docker/renderer/start.sh` prefixes its lines with `[xvfb] ` |
| `build` | build-job output (`jobs.Runner.Log`), tagged with the job id |

Each entry is `{id, t, source, level, job?, msg}`. `id` orders entries and is unique across
restarts (the time in nanoseconds, nudged forward on a tie; a string in JSON). `level` is
`debug`, `info`, `warn` or `error`: the server's own lines carry theirs, a child's JSON log line
(`{"level":…,"msg":…}`) is unpacked, and other lines are classified by the conventions the tools
use (`[ERROR]`, ` WAR `, `Error:` …; unknown is `info`). Valhalla's per-request chatter (five
lines for every health probe) is `debug`. Debug lines are dropped unless `MAPTHING_DEBUG` is set.
Terminal colour codes are stripped.

**Redaction** happens once, on the way in, so no secret reaches memory, the files, the console
or the UI. `logs.Redact` masks (as `[redacted]`):

- the whole value of `Authorization`, `Proxy-Authorization`, `Cookie` and `Set-Cookie` (any
  scheme, Digest fields, every cookie), in header, `key=value`, JSON and Go map forms; a quoted
  or bracketed value ends at its closing quote or bracket, any other at the end of the line;
- `X-Goog-Api-Key`, `X-Mapthing-Internal` and `X-Api-Key` values;
- bearer tokens anywhere;
- query parameters `key`, `sig` and any whose name contains `password`, `secret`, `token`,
  `api_key`, `access_key`, `credential` or `signature`;
- `name=value` and `"name": "value"` pairs with such a name (`ADMIN_PASSWORD=…`, `TUNNEL_TOKEN=…`);
- credentials in URLs (`scheme://user:pass@host`);
- mapthing keys (`mt_…`, also when cut short), Google API keys (`AIza…`) and JWT-shaped blobs
  wherever they appear;
- all of the above behind percent-encoding (`%6bey=…`, an encoded header dump, up to three
  layers): when decoding a line reveals a secret, the line is kept decoded and masked.

The admin password and the internal token are also registered as literals (`Sink.AddSecret`) and
removed whatever surrounds them, percent-encoded or not. Redaction runs on the whole line (up
to 1 MiB) before the line is cut to the 16 KiB that is kept, so a secret lying across the cut is
not shown in part. It errs on the side of masking. Request logging records the path without
its query string.

Build output has other readers than the log: the job log in SQLite, the `job` and `log` events
on `/admin/api/events`, the job's `error`, and the failure details in history. The job runner
applies the same redactor before any of them (`jobs.Runner.Redact`), and the admin API masks
job errors, job log lines and history again on the way out, for rows an older version stored.

**Storage.** The last 5 000 entries stay in memory. Every entry is also appended to
`<data>/logs/log-current.jsonl` (one JSON object per line). At a tenth of
`MAPTHING_LOG_MAX_MB` (default 50, in MB of 10^6 bytes) the file is renamed `log-<last id>.jsonl`
and the oldest segments are deleted so the total stays under the bound. Every line is still
printed to stderr as `time LEVEL [source #job] message`, so `docker logs` shows everything the
Logs page does. If the directory cannot be written the server runs on, memory only, and the
Logs page says so. (Older versions wrote `<data>/logs/<child>.log`; those files are no longer
written or read and can be deleted.)

**API** (admin session). The three endpoints share one filter in the query string: `source=a,b`,
`level` (that level and above), `q` (case-insensitive text), `job`, `since` and `until`
(RFC 3339), `before` and `after` (entry ids).

- `GET /admin/api/logs?…&limit=500` → `{entries (oldest first), more, sources, store:{bytes,
  max_bytes, oldest, file_error?}}`: the newest `limit` (≤ 5 000) matches. Page back with
  `before=<oldest id shown>`. It answers from memory when that holds enough and reads the files
  otherwise, so it reaches as far back as they go (`store.oldest`).
- `GET /admin/api/logs/stream?…&after=<id>` (SSE): event `entry` per matching line as it is
  written, after first replaying the matches newer than `after`, so a page that loaded history
  and then connects misses nothing. A reader that falls behind loses lines rather than slowing
  the writer, and is told with event `dropped` `{count}`. Every write has a 20 s deadline: a
  client that stops reading is disconnected and unsubscribed. At most 16 streams are open at
  once (503 `too_many_streams` beyond that).
- `GET /admin/api/logs/download?…[&format=jsonl]` → every match, oldest first, as a text file
  (the console format) or JSON lines. It covers what existed when it started, reads the files
  through handles opened at that moment (so it never holds up rotation, however slow the
  client), and is cut off if the client stops reading.

The admin **Logs** page (`/admin/logs`) is a client of these: live tail with pause and
follow, source chips, level, text search, time range, download. Its URL carries the filter, so
`/admin/logs?job=12` shows one build and `/admin/logs?source=renderer` one process; a failed
build's history row links to the first.

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
- **Reuse.** A build on a server (manual or scheduled) offers the live generation's terrain to
  the new one (`build.Options.ReuseTerrain`, as `mapthing-build -reuse-terrain` does). When the
  fingerprint recorded in `terrain.pmtiles.json` (areas, zooms, rounding step, sources) matches
  what this build would make, `terrain.pmtiles` and `contours.pmtiles` are hard-linked into the
  new generation instead of being rebuilt, the disk guard does not count them, and the
  verification still samples them. A refresh build is then the basemap plus the routing graph.
  Regions added, removed or redrawn, or a changed terrain setting, change the fingerprint and
  the terrain is built. The DEM data itself is not part of the fingerprint: to pick up newer
  elevation data, start a build with `rebuild_terrain` (the "Rebuild terrain" switch on Builds).
  The two generations share the file, so deleting either leaves the other intact, and
  `/admin/api/generations` counts it once in `disk.bytes`.
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
  "precision":5|6}`. Sampling is either `interval_m` (default 30; vertices are kept) or
  `samples` (exactly N, Google semantics). Intermediate points lie on the great circle, and
  distances are great-circle. At most 10 000 samples; the interval grows to fit. Returns:
  - `samples[]` with unsmoothed elevations and `distance_m`.
  - `length_m`, `min_m`, `max_m`, `sources`.
  - `gain_raw_m` / `loss_raw_m`: every sample-to-sample change.
  - `gain_m` / `loss_m`: the elevation series is first passed through a running median over
    `smooth_window_m` (default **150 m**, centred, narrowed at the path ends), then a
    `hysteresis_m` threshold (default **2 m**); negative turns either step off. The median
    removes features narrower than about half the window that a vehicle or pipe does not
    climb: bridge and culvert dips in a bare-earth DEM, embankment edges, single-pixel
    noise. With these defaults, gain is nearly independent of sample spacing (10 m vs 30 m
    agree within ~1% on the test routes; see REPORT).
  - `bridges`: `"ground"` (default) samples the terrain everywhere. That is right for buried
    pipe, which goes under a creek or canal rather than over it. `"deck"` map-matches the
    profile's sample points onto the road graph (Valhalla `trace_attributes`, `map_snap`,
    `edge.bridge`/`edge.tunnel`), then interpolates elevation linearly across each run of
    bridge or tunnel samples. The interpolation runs between the nearest ground samples, or is
    held level when a structure reaches the path's end. Gain, loss, min and max then come from
    that road-level series. The response echoes `bridges` and lists `structure_spans`
    (`start_m`, `end_m`, `kind`); interpolated samples carry `structure`. If Valhalla is
    unavailable, the path does not map-match, or fewer than 50% of points match, the answer
    is the ground profile with `bridges: "ground"` and a `warning`. The admin map's route tool
    uses deck by default, with a toggle. The Google-compatible endpoint always samples the
    ground. `POST /api/v1/route` does not return a profile, so it takes no `bridges` option.
- `GET /api/v1/elevation/sources` → sources in the live archive.
- `GET /maps/api/elevation/json`: Google-compatible (`locations=` / `path=`+`samples=` ≤ 512,
  `lat,lng|…` or `enc:`). `path` + `samples=N` returns N points equidistant along the whole
  path (great circle), ends included, with **unsmoothed** elevations, as Google does, so gain
  computed client-side from it is comparable with Google's. Errors are `INVALID_REQUEST`
  (HTTP 400). Google's shape has no null,
  so an uncovered point returns `elevation: 0, resolution: -1`; if no point is covered (or no
  generation is live) the status is `DATA_NOT_AVAILABLE`.
- Heights are each source's native orthometric datum (NAVD88 / CGVD2013 / EGM2008), with no
  geoid conversion. US values read ≈ 1 m below EGM96-based answers; Canadian values are within
  ≈ ±0.5 m.
- `elevation.CoverageIDs(bbox)` / `CoverageFor(bbox)`: static, offline list of the sources
  expected in a bbox, for the catalog.

## Contours

`internal/contours.Build(terrainPath, outPath)` turns the generation's `terrain.pmtiles` into
`contours.pmtiles`. The terrain stage runs it right after the terrain archive is written
(`build.RegisterContours`, from `internal/build/contours.go`); a failure fails the stage. There is
no extra download and no GDAL: it reads only the terrain archive.
`MAPTHING_CONTOURS_MAXZOOM` changes the max zoom (default 12); `0` turns contours off.
The dev CLI runs the same code on any terrain archive:
`mt-terrain contours -in terrain.pmtiles -out contours.pmtiles`.

- **Archive**: MVT, gzip, z10–12, one layer `contours`. A tile holds one multi-line feature per
  level, with `ele` (integer metres, the terrain's native datum) and `major` (boolean, index line).
  The z12 tiles are overzoomed past z12, like the terrain source.
- **Intervals** (metric): z10–11 every **20 m**, major every **100 m**; z12 (and overzoomed) every
  **10 m**, major every **50 m**. The 0 m level is skipped, because the water layer draws coastlines.
- **Grid**: each contour tile is contoured on its own grid resampled from the terrain's
  **max-zoom** tiles, which carry the `mtSr` no-data mask. Lower terrain zooms write no-data
  as 0 m, which would draw cliffs along coverage edges.
  - Below the max zoom the grid is 256 px per tile, from a box average of max-zoom pixels.
  - At z12 it is 512 px (z13 terrain at full resolution), because z12 is overzoomed to z16 and
    needs the finer line.
  - Each grid gets one 3×3 binomial smoothing pass.
  - A one-pixel border from the neighbouring tiles makes lines meet across tile edges, with no
    clipping.
  - Cells with a no-data corner are skipped, so lines stop at the edge of coverage.
- **Lines**:
  - Marching squares with linear interpolation; saddles are resolved by the cell-centre average.
  - Levels are offset by 1 cm, so quantised elevations never sit exactly on a level.
  - Douglas–Peucker simplification at 1.5 of 4 096 tile units. At 3 units the lines look angular
    at z15 and the archive is 21% smaller.
  - Closed rings shorter than 8 grid px are dropped (one-pixel bumps in flat ground).
- **Work and memory**: the archive is processed in z10 blocks: the 8×8 max-zoom tiles of a block
  plus a one-tile ring, about 26 MB per worker.
- **Accuracy**: vertices re-sampled from the terrain at max zoom are within the following median
  |terrain − ele| at z12 (opt-in test `MAPTHING_TEST_TERRAIN=… go test ./internal/contours -run Real`):

  | Area | Median | p90 |
  |---|---|---|
  | Delaware | 0.06 m | 0.37 m |
  | Permian 1° | 0.03 m | 0.25 m |
  | BC mountains | 0.25 m | 0.72 m |

  z10–11 lines are generalised by their coarser grid, which matters in steep terrain only.
- **Size** (z10–12, defaults; terrain archive for scale):

  | Area | Contours | Terrain | Time |
  |---|---|---|---|
  | Delaware (whole state) | 0.70 MB | 21 MB | 1.3 s |
  | Permian 1° box (−104…−103, 31…32) | 0.70 MB | 18 MB | 1.2 s |
  | BC mountains test box | 2.1 MB | 7.6 MB | 2 s |
  | Nine-region comparison set | 35 MB | 524 MB | 25 s |

  Contours run at about 3–7% of terrain on plains and about 27% in mountains.
- **Styles**: every theme has `contours_minor` and `contours_major` (lines over the hillshade,
  under water) and `contours_labels` (`"750 m"` along major lines from z12, under every other
  label). Themes set colours, widths and opacities; see `assets/styles/src/themes/README.md`.

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

**Generation swaps, without downtime.** Core polls the `current` symlink (every 2 s) and, when
its target changes, reopens the PMTiles archives and drops raster caches and packs of old
generations. The build's `Options.Restart` hook is wired to the same swap handler, so a swap
takes effect immediately. Nothing is restarted:

- **valhalla runs blue/green** (`supervisor.BlueGreen`, `cmd/mapthing/valhalla.go`) on two localhost
  ports, the port of `MAPTHING_VALHALLA_URL` and the next (8002/8003). Each instance runs from
  `<data>/run/valhalla-<port>.json`: the generation's `valhalla.json` with its own listen port and
  IPC endpoints (its tile paths point into that generation's directory). On a swap the new instance
  starts as a *standby* child (not counted by `/readyz`), and once it answers `/status` and a test
  route inside the generation the routing client switches its base URL (atomically) and the old
  instance is told to stop (the swap does not wait for it to exit). If the new one never gets ready (5 min), it is stopped and the old one keeps
  serving. `MAPTHING_VALHALLA_CMD` uses `{config}` for the instance config
  (default `valhalla_service {config} 1`).
- **The renderer** is not restarted: core calls its `POST /reload` (drop styles and pools); it reads
  tiles from core, which has already switched.

**Degraded raster tiles.** A raster tile never fails because one of its sources does: the renderer
draws a failed tile/glyph/sprite as no data and a failed TileJSON as an empty source (only the
style is essential), and marks such a tile `X-Mapthing-Degraded: 1` with `no-store`; core serves
it with the same headers and does not cache it. A tileset missing from the live generation (e.g.
built without terrain) answers 404 (503 only while nothing is built), which the renderer treats as
an empty source without degrading.

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
`ADMIN_PASSWORD`, `MAPTHING_PUBLIC_URL`, `MAPTHING_TRUSTED_PROXY` (`off`), `MAPTHING_VALHALLA_URL` (default
`http://127.0.0.1:8002`; blue/green also uses the next port), `MAPTHING_VALHALLA_CMD` (`{config}`), `MAPTHING_RENDERER_URL` (default
`http://127.0.0.1:8081`), `MAPTHING_RENDERER_CMD` (default `node /opt/renderer/server.js`),
`MAPTHING_RENDERER_HEALTH_URL` (default renderer URL + `/health`), `MAPTHING_RASTER_CACHE_MB`
(2048), `MAPTHING_IMAGERY_CACHE_MB` (4096), `MAPTHING_PACK_CACHE_MB` (4096),
`MAPTHING_PACK_MAX_TILES` (2000000), `MAPTHING_INTERNAL_TOKEN`, `MAPTHING_IMAGERY_URL`,
`MAPTHING_DEBUG`, `MAPTHING_LOG_MAX_MB` (50: total size of the log files), `MAPTHING_RENDERER` (`on`; `off` starts no renderer and raster tiles answer
404 `renderer_off`), `MAPTHING_SERVE` (`tiles,routing,elevation`; a part left out is not served
and its files are not required of a generation: [operations.md](operations.md)). Caches live in `/data/raster-cache`, `/data/imagery-cache`, `/data/pack-cache`.

**Behind a proxy or tunnel.** The usual deployment is a tunnel or reverse proxy that terminates
TLS and reaches the container over plain HTTP on a private network (`samples/`).

- `MAPTHING_PUBLIC_URL` is the base of every URL the server writes: tile, glyph and sprite URLs
  in styles, `tiles` in TileJSON, and the snippets the admin UI shows for a new key
  (`public_url` in `/admin/api/system`). Without it the base is taken from each request
  (`X-Forwarded-Proto` and `X-Forwarded-Host` when present, else the request's own scheme and
  host), which is right only when the proxy passes the public host through. The renderer's
  internal requests and the admin UI's own map (a session, no key) always get the host they
  came in on, so the admin map also works on `http://127.0.0.1:8080`.
- `MAPTHING_TRUSTED_PROXY=on` makes `auth.Auth.ClientIP` take the client address from
  `CF-Connecting-IP`, else the first `X-Forwarded-For` entry (values that are not an IP
  address are skipped), else the connection. It is what the login throttle (5 failures in
  5 minutes per address), per-client rate limits and request log lines (`ip=`) use. Off by
  default, and the headers are then ignored: a directly reachable server must not let a client
  name its own address. Turn it on only when the port is not published to clients (the
  compose sample binds it to 127.0.0.1; anyone on the host itself can still claim an address).
- The renderer's internal token is accepted only from a loopback **connection**, never from a
  forwarded address, whatever this setting is.
- The session cookie is `HttpOnly`, `SameSite=Lax`, and `Secure` when the browser came over
  HTTPS: a TLS connection, `X-Forwarded-Proto: https`, Cloudflare's `CF-Visitor`, or a request
  for the host of an `https` `MAPTHING_PUBLIC_URL`. Over plain HTTP on the box itself
  (`http://127.0.0.1:8080/admin`) it is not `Secure`, so that still works.
- The CSRF check accepts the request's host, `X-Forwarded-Host` and the public URL's host.
- Server-sent events (`/admin/api/events`, `/admin/api/logs/stream`) send a comment every 15 s
  and `X-Accel-Buffering: no`, so proxies neither buffer nor time them out.

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
- `GET /admin/api/catalog` → `{entries:[{id,name,parent|null,bbox:[w,s,e,n],extract_bytes,downloadable,elevation_sources:[id…]}], elevation_sources:[{id,name,resolution_m,license,attribution}]}`
  (the pipeline's Geofabrik catalog; 503 when it cannot load). `downloadable`: the entry has its own extract (selectable);
  `extract_bytes` is 0 for grouping nodes and until the size is known (HEAD requests run in the background at start).
  US states appear as `north-america/us/<state>` named "Delaware" under `north-america/us`.
- `GET /admin/api/regions` → `{regions:[region]}`; region = `{id,name,catalog_id|null,bbox,polygon?:[[lon,lat]…],status:"pending"|"installed"|"failed",extract_bytes,disk_bytes,data_date|null,downloaded_at|null,elevation_sources:[id…]}` + *extra* `parent,refresh,added_at`.
  `status`: installed = in the live generation's manifest; failed = not live and the latest finished build failed; else pending.
  `data_date`/`downloaded_at`/`disk_bytes` come from `downloads/<id>/meta.json` and the download size; `extract_bytes`/`elevation_sources` from the catalog at add time.
  `POST` `{id}` (catalog extract) or `{name,bbox}` / `{name,polygon:[[lon,lat]…]}` (custom; id `custom/<slug>`) → 201 region; 409 if the id exists.
  `GET`/`PATCH {refresh}`/`DELETE` `/admin/api/regions/{id}` (ids contain `/`, may be URL-encoded) → region / region / 204.
- `POST /admin/api/build` `{refresh_downloads, rebuild_terrain}` → 202 job (`rebuild_terrain`: do not reuse the live generation's terrain, see Elevation); 409 if a build is queued or running; 503 if the build job is not registered.
- Job: `{id,name,status:"queued"|"running"|"succeeded"|"failed"|"cancelled",trigger:"manual"|"schedule",refresh_downloads,created_at,started_at?,finished_at?,progress:0..1,stages:[{name,status:"pending"|"running"|"done"|"failed"|"skipped",progress,detail?,started_at?,finished_at?}],error?,generation_id?}`.
  Stages are the pipeline's, all listed from the moment a build is queued: `guard, download, merge, basemap, valhalla, terrain, verify, swap, retention`;
  stages never reached in a successful build end `skipped`. `progress` is the mean over stages.
  `GET /admin/api/jobs` → `{jobs:[…]}` newest first; `GET /admin/api/jobs/{id}` → job + `log:[{job_id,t,level:"debug"|"info"|"warn"|"error",msg}]` (tail, `?tail=n`);
  `POST /admin/api/jobs/{id}/cancel` → 204 (409 if not queued/running).
- `GET /admin/api/events` (SSE): named events `job` (data: job), `log` (data: log line), `generation` (data: `{id,previous}` when a generation goes live). The current job is sent on connect.
- `GET /admin/api/generations` → `{generations:[{id,created_at,status,owned,problems?,bytes,can_activate,can_delete,manifest:{regions:[{id,name,data_date,bbox}],sizes:{basemap,terrain,contours,routing,total},timings:{stage:seconds},checks:[{name,ok,detail?}],raw}}],disk:{bytes,sum_bytes,free_bytes,total_bytes}}`,
  newest first (generations on disk; `manifest` is normalised from the pipeline's `manifest.json`, which is in `raw`; sizes are measured on disk).
  `status` (`build.Builder.Inventory`): `live`; `previous` (the newest other complete generation this server owns: the one retention keeps, the rollback);
  `kept` (owned and complete, beyond that); `building` (a build is writing it); `incoming` (not owned: copied in and never activated here; `problems` is set while the copy is incomplete);
  `invalid` (owned, or left by a build that was killed, and failing validation: `problems` says why).
  `can_activate`: it validates, is not live or building, and no build is running. `can_delete`: it is not live and not being built.
  `disk.bytes` counts a file shared by hard link (terrain reused from the previous generation) once; `disk.sum_bytes` is the generations' sizes added up.
  Rolling back is activating the `previous` generation. `DELETE /admin/api/generations/{id}` → 204.
  A build, an activation and a delete each claim their generation under one short lock
  (`build.Builder.busyMu`) before touching it, and `current` only moves to a claimed generation,
  so a delete cannot remove a directory that is being written, validated or made live, and an
  activation of one being deleted answers 409 `busy`. Updates of `.retention.json` are serialised. Activations and deletions by hand are recorded in history (`kind` `generation`).
- `GET/PUT /admin/api/schedule` ↔ `{cron,enabled,refresh_downloads,regions:[{id,enabled}]}` (cron 5-field, UTC; PUT fields optional, 400 `bad_cron`)
  + *extra* `next_run,last_run,upcoming:[5 times],default_cron,timezone,history`.
- `GET /admin/api/keys` → `{keys:[{id,label,prefix,created_at,last_used_at|null,limits:{rpm|null,rpd|null,origins:[]},usage:{today,total,daily:[14 numbers, oldest first]}}]}` (revoked keys left out; `?revoked=1` includes them with `revoked_at`).
  `POST` `{label,rpm?,rpd?,origins?}` (null = unlimited) → 201 `{key,secret}` (the secret only here); `GET`/`PATCH` (same fields) `/admin/api/keys/{id}` → key; `DELETE` → 204 (revoked at once).
- `GET /admin/api/stats?days=30` → `{daily:[{date,tiles,raster,routes,elevation,packs}],by_key:[{key_id,label,requests}],cache:[{name,hits,misses,size_bytes}],disk:{generations:[{id,bytes}],regions:[{id,name,bytes}],downloads_bytes,elevation_cache_bytes,imagery_cache_bytes,free_bytes,total_bytes},memory:{process_rss_bytes,system_used_bytes,system_total_bytes},processes:[{name,state:"running"|"starting"|"restarting"|"stopped",pid|null,uptime_s,restarts,rss_bytes}]}`
  (+ *extra* disk `raster_cache_bytes,pack_cache_bytes,database_bytes`, process `detail,healthy`). Usage kinds map to buckets: tile→tiles, raster→raster, pack→packs, elevation→elevation, everything else (route, matrix, snap, isochrone, valhalla, compat)→routes; `raster-internal` is not shown. `key_id` "0" is the admin UI.
- `GET /admin/api/overview` → `{version,commit,started_at,ready,readiness,generation:{id,created_at,bytes,sizes,regions:[{id,name,data_date,bbox}]}|null,regions_installed,processes:[process],system:{cpus,load1,memory_used_bytes,memory_total_bytes,platform},traffic:{since,kinds:[{kind,hour:window,day:window,series:[60 per-minute counts]}]},disk:{total_bytes,free_bytes,data_bytes,parts:[{name,bytes}],guard:{known,need_bytes?,ok?}},builds:{schedule_enabled,cron,next_run,active:job|null,last:{id,status,trigger,started_at,finished_at,duration_s,error,generation_id}|null}}`.
  - `process` = `{name,state,detail?,pid,uptime_s,restarts,rss_bytes,cpu_percent|null,healthy,standby?,role?}`: this server first, then each supervised child, then `build tools` while a build's tools run.
    `rss_bytes` and `cpu_percent` (of one core, between two readings at least a second apart) cover a child's whole process group, so the renderer's include Node and Xvfb. Read from `/proc`; zero elsewhere.
  - `window` = `{requests,errors (5xx),rejected (401/403/429),per_min,p50_ms,p95_ms}` for the last hour and the last 24 hours. Kinds: `tiles` (vector tiles and archives), `raster`, `route` (every routing endpoint, native and compatible), `elevation`, `style` (styles, glyphs, sprites), `pack`.
    Measured by the request middleware for keyed endpoints (`stats.Traffic`: per kind, one slot per minute for 24 hours with a 96-bucket latency histogram, so a quantile is the upper bound of its bucket, at most 19% above the true value).
    In memory only: it starts empty at every start. The renderer's own requests are not counted. Time is to the last byte written.
  - `disk.guard`: what the next build needs free (`build.Builder.EstimateFor` for the installed regions, refreshed in the background every 5 minutes; `known` is false until then or with no regions).
- `GET /admin/api/system` → `{version,commit,build_date,components:[{name,version}],ready,readiness:[{name,ok,detail?}],disk:{free_bytes,total_bytes},current_generation|null,attribution:[{name,license,url?,note?}],imagery_source:{name,attribution}|null}`
  + *extra* `styles:[{id,name,description,dark}],elevation_sources,platform,data_dir,process,public_url,trusted_proxy`. `components` is the same list as `versions` in `/admin/api/settings`.
- `GET /admin/api/settings` → `{settings:[{name,group,value,default?,source:"env"|"default",secret?,set,help}],versions:[{name,version,detail?}]}`.
  `settings` is `config.Describe`: one entry for every `MAPTHING_*` variable the code reads (a test fails when one is read that is not listed), in page order by group.
  `source` is `env` when the variable is set and not empty in the container's environment, `image` when it holds exactly the value the published image sets (`docker/Dockerfile` `ENV`; keep `config.imageValues` in step), else `default`, and `value` is then the default in words.
  A secret (`ADMIN_PASSWORD`, `MAPTHING_INTERNAL_TOKEN`) is never returned: `value` is a fixed mask when set, empty when not. URL-valued settings pass through the log redactor.
  A `MAPTHING_*` variable that is set but not one mapthing reads is listed in group `Not recognised` (a misspelt name), masked if its name looks like a secret. Variables without the prefix (a tunnel token in the same `.env`) are never listed.
  `versions`: mapthing (with commit and build date), Go, then what the image carries: Valhalla, Planetiler (the Protomaps basemap commit), the renderer (MapLibre Native, Node), GDAL, osmium, Java. Tool versions are asked of each tool once, in the background; `version` is empty until then or when a tool is absent.
- `GET /admin/api/history?kind=&limit=` → `{history:[{id,at,kind,ok,message,job_id?,generation?,details?}]}`; `POST /admin/api/system/restart/{name}` → 204.
- Keyed endpoints as the UI calls them: `POST /api/v1/route` → GeoJSON Feature (`geometry` LineString, `properties` = `{distance_m,duration_s,surface_breakdown,legs}`); the client flattens it (to `surface_breakdown_m`).
  Route errors use codes `no_road` / `no_route`. `POST /api/v1/elevation` → `{results:[sample]}`; `POST /api/v1/elevation/profile` → the `elevation.Profile` JSON.

## Build pipeline interfaces

**Custom regions are clipped from several extracts when they cross a boundary.** The catalog
picks the cheapest set of Geofabrik extracts whose boundaries together cover the area (greedy
weighted set cover on a dense sample of the area, pruned, compared with the best single
extract), using HEAD sizes. A box across the Texas/New Mexico line downloads texas + new-mexico,
not all of `us`. `catalog.Region.Custom.Parents` lists them. The build clips each parent with
`osmium extract` and `osmium merge`s the clips. Each parent is downloaded once per build however
many regions share it, and the disk guard counts it once. Custom regions in the regions API carry
`parents` and `extract_bytes` (the total download).

`internal/build.Builder.Run(ctx, rep, Options)` runs one generation build; `rep` is a
`build.Reporter` (`Stage`, `Progress`, `Logf`; `*jobs.Job` satisfies it). Failure returns a
`*build.StageError{Stage, Err, LogTail}`. Regions are `catalog.Region{ID, Name, BBox, Polygon, …}`;
`Polygon` is lon/lat rings read with the even-odd rule (Geofabrik boundary, or the drawn custom
polygon). The elevation package plugs in with `build.RegisterTerrain`,
`build.RegisterTerrainEstimate` and `build.RegisterElevationCheck` from an `init()` in
`internal/build/terrain*.go`; contours with `build.RegisterContours` (`internal/build/contours.go`),
run inside the terrain stage after a successful terrain build. Valhalla settings: `docker/valhalla/overrides.json` is deep-merged over
`valhalla_build_config` output. Planetiler runs the Protomaps profile with the patches in
`docker/planetiler/` (see PATCHES.md: `surface`, `surface_raw`, `tracktype`, `access` attributes on
roads; private and rural service roads at their normal zooms).

When osmdata.openstreetmap.de is unreachable after retries (or `MAPTHING_WATER_POLYGONS_URL` /
`MAPTHING_LAND_POLYGONS_URL` mirrors fail), the build uses Natural Earth 10m ocean/land clipped to
the regions (manifest `ocean: approximate:ne-10m`); if that fails too it builds without high-zoom
ocean (`ocean: missing`). The admin UI should surface a non-`osm` value.
