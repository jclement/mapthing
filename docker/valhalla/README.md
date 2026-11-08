# Valhalla configuration

mapthing runs Valhalla **3.9.1** (`ghcr.io/valhalla/valhalla:3.9.1`). The build pipeline
generates the base config with `valhalla_build_config` and deep-merges `overrides.json` over it
(objects merge key by key; scalars and arrays replace). It then sets the per-generation paths
itself (see below). JSON has no comments, so this file explains each override.

| File | What |
|---|---|
| `overrides.json` | the merged config overrides |
| `default_speeds.json` | road speeds for tracks and service roads (see "Speeds") |
| `dev-build.sh` | builds a graph for one Geofabrik extract with these overrides: the reference for the pipeline's steps |
| `dev-serve.sh` | serves that graph on 127.0.0.1:8002 for development and the integration tests |

## What the pipeline must provide

The image must contain this directory at **`/usr/share/mapthing/valhalla/`**
(`COPY docker/valhalla/ /usr/share/mapthing/valhalla/`), because
`mjolnir.default_speeds_config` points there. If the file is missing, Valhalla logs
"Disabled default speeds assignment from config" and carries on with its stock speeds, and
lease-road ETAs become several times too long (see "Speeds").

Per generation, the pipeline sets these paths (they are not in `overrides.json`):

| Key | Value |
|---|---|
| `mjolnir.tile_dir` | `<gen>/valhalla/tiles` (build scratch; delete after the extract is made) |
| `mjolnir.tile_extract` | `<gen>/valhalla/tiles.tar` (what the service reads) |
| `mjolnir.admin` | `<gen>/valhalla/admin.sqlite` |
| `mjolnir.timezone` | a shared `tz_world.sqlite` (see below) |
| `mjolnir.traffic_extract`, `landmarks`, `transit_dir`, `transit_feeds_dir` | anywhere under `<gen>/valhalla/`. Unused, but they must not point at another generation's files |

Build steps, in order (all from the merged, path-filled config):

1. `valhalla_build_timezones > tz_world.sqlite`: **once**, then reuse it. It downloads the
   timezone-boundary-builder shapefile (about 60 MB) and writes about 120 MB. Only
   time-dependent routing (`date_time`) needs it. If the build fails, carry on without it.
2. `valhalla_build_admins --config valhalla.json <merged.osm.pbf>`: each build, from the same
   merged extract (a few MB, seconds). Valhalla uses it for country-specific access rules
   (`use_admin_db`, `apply_country_overrides`), driving side and border crossings. Without it,
   access falls back to generic defaults. When the extract is not the whole planet, expect
   warnings like "NOT NULL constraint failed: admin_access.admin_id" and "… is degenerate and will
   be skipped". They are harmless.
3. `valhalla_build_tiles --config valhalla.json <merged.osm.pbf>`
4. `valhalla_build_extract --config valhalla.json --overwrite`, then delete `tile_dir`.

The Delaware extract (22 MB PBF) builds in about 20 s and produces a 43 MB `tiles.tar` plus a
7 MB `admin.sqlite`.

Run the service as `valhalla_service <gen>/valhalla/valhalla.json 1` (one worker thread is
enough for a single-box service; raise it for heavier load).

## Overrides, one by one

### `httpd.service`

- `listen: tcp://127.0.0.1:8002`: the routing engine is reachable only from inside the
  container. mapthing is the only public listener (architecture.md). The Go client's
  default URL is `http://127.0.0.1:8002` (`MAPTHING_VALHALLA_URL` overrides it).
- `timeout_seconds: 120`: caps a single request (default: no cap), so a pathological
  2,000 km matrix cannot hold a worker forever. The Go client waits 130 s.

### `mjolnir` (graph build)

- `include_driveways: true` (Valhalla's default, kept explicit): lease roads and well-pad
  access are often `highway=service` (+ `service=driveway`). They stay in the graph.
- `include_construction: false`: roads under construction are not routable.
- `include_bicycle: false`, `include_pedestrian: false`: mapthing routes vehicles only.
  This drops ways that only cyclists or pedestrians may use, which makes tiles smaller and
  builds faster. Ways open to cars keep all their access flags, so pass-through
  `pedestrian`/`bicycle` costing still works on shared roads, but not on footpaths or cycle
  tracks.
- `include_platforms: false`: no transit platforms.
- `hierarchy`, `shortcuts: true` (defaults, explicit): needed for fast long routes (2,000 km).
- `data_processing.use_admin_db`, `apply_country_overrides: true`: use `admin.sqlite`
  for country access rules (see build step 2).
- `data_processing.allow_alt_name: true`: index `alt_name` so rural roads known by two
  names (a county road number and a local name) show both.
- `default_speeds_config`: see "Speeds".

### `loki.service_defaults` (snapping)

- `minimum_reachability: 30` (default 50): a candidate edge must reach this many other
  edges to count as connected. A dead-end lease road network off a county road can have fewer
  than 50, and at 50 the point would snap to the county road instead of the lease road. At
  30, small components (parking lots, isolated mapping errors) are still rejected.
- `radius: 0`, `search_cutoff: 35000` (defaults, explicit): the client sets a
  per-location `search_cutoff` from the caller's radius (ORS `radiuses`, native `radius_m`).

### `service_limits`

- `auto` / `truck`
  - `max_distance: 5000000` (5,000 km, the default, explicit): comfortably above the
    2,000 km target.
  - `max_locations: 50` (default 20): waypoint lists for multi-site trips.
  - `max_matrix_distance: 2000000` (default 400 km): matrices across a basin or a province.
  - `max_matrix_location_pairs: 2500`: 50 × 50.
- `isochrone`
  - `max_contours: 5` (default 4), `max_time_contour: 120` minutes (7,200 s).
  - `max_distance_contour: 200` km (default): the largest distance contour. (`max_distance`
    is the straight-line spread between input locations and stays at its default, since
    `max_locations` is 1.)
  - `max_locations: 1`: Valhalla merges several origins into one reachability. ORS-style
    "one polygon set per origin" is done by the Go layer, one request per origin.
- `max_radius: 10000` (default 200 m): the largest `radius` a location may carry. ORS clients
  send `radiuses` up to 10 km.
- `max_alternates: 3`.
- `trace.max_distance: 2000000`, `trace.max_shape: 100000`: the surface breakdown runs
  `trace_attributes` over the route's own shape, so a 2,000 km route (tens of thousands of
  shape points) must fit.

## Speeds

Valhalla 3.9.1's Lua tag handler gives `highway=track` a speed of **5 km/h** (10–20 km/h only
with a `tracktype` grade1–4). The graph enhancer then halves the speed of anything unpaved,
so an untagged gravel track ends up at **2 km/h**, and an unpaved `highway=service` road at
about 12 km/h. Those speeds suit farm tracks. Oilfield lease roads are maintained gravel that
trucks drive at 30–60 km/h, and at 2 km/h a 1 km lease road adds about 30 minutes to the ETA.

`default_speeds.json` (Valhalla's `default_speeds_config`, schema from
OpenStreetMapSpeeds) sets **only the `service_other` road class** (tracks and service roads
that are not driveways, alleys, parking aisles or drive-throughs) to 30 km/h in rural areas
and 25 km/h in suburban areas. Every other entry is `null`, so all other roads keep
Valhalla's normal speeds, including the urban ones. Configured speeds are not halved for
surface.

This changes ETAs only, not which roads a route prefers. Measured on Delaware (2026-11-08):
the same paths before and after; the route to a 0.79 km private gravel track dropped from
2,785 s to 1,455 s (the track leg alone from about 1,430 s to 95 s). In 112 town-to-town auto
and truck routes, no route used a track or unpaved road anywhere except at its two ends. Tracks
still carry the costing penalty `use_tracks` (0.3 by default, set per request by the Go
client), and private tracks are destination-only.

Caveats: a `maxspeed` tag on a track or service road is overridden by this table (rare on such
roads), and farm tracks get the same 30 km/h as lease roads.
