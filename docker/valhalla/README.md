# Valhalla configuration

mapthing runs Valhalla **3.9.1** (`ghcr.io/valhalla/valhalla:3.9.1`). The build pipeline
generates the base config with `valhalla_build_config` and deep-merges `overrides.json` over it
(objects merge key by key; scalars and arrays replace). It then sets the per-generation paths
itself (see below). JSON has no comments, so this file explains each override.

| File | What |
|---|---|
| `overrides.json` | the merged config overrides |
| `default_speeds.json` | rural road speeds by road class, plus tracks and service roads (see "Speeds") |
| `dev-build.sh` | builds a graph for one Geofabrik extract, or a local `.osm.pbf` such as an osmium clip, with these overrides: the reference for the pipeline's steps |
| `dev-serve.sh` | serves that graph on 127.0.0.1:8002 (or `PORT=…`, to run several side by side) for development and the integration tests |

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

`default_speeds.json` is Valhalla's `default_speeds_config` (schema from
OpenStreetMapSpeeds). It holds one table with no `iso3166` keys, so it applies everywhere.
Only the **rural** entries listed below are set. Every other slot is `null`, which keeps
Valhalla's own speed: the `maxspeed` tag if the road has one, otherwise a per-class default.

| Rural road class | Stock speed with no `maxspeed` | mapthing |
|---|---|---|
| primary | 75 km/h | **90** |
| secondary | 60 | **80** |
| tertiary | 50 (25 if unpaved) | **65** |
| unclassified | 40 (20 if unpaved) | **55** |
| `service_other` (tracks, service roads other than driveways or parking aisles) | 5 for an ungraded track (2 if unpaved); 25 for service (12 if unpaved) | **30** (suburban **25**) |

Motorway, trunk, residential, links and roundabouts keep Valhalla's speeds, as do all
suburban and urban roads (except suburban `service_other`).

### How Valhalla applies the table (3.9.1, measured)

- **Rural / suburban / urban** comes from the edge's road density, 0–15. Density 0–5 is
  rural, 6–10 suburban, 11 and above urban. The density does not depend on the speed table: a
  build without the table gives the same density on every edge (checked on 672 edges).
- **A table value replaces the `maxspeed` tag** and is not capped by it (`speed_assigner.h`:
  the config lookup runs before the tagged-speed branch, and the edge becomes `classified`).
  A rural primary tagged 121 km/h gets 90, and a rural one tagged 60 km/h also gets 90.
  That is why only rural classes are set and trunk is left alone (trunk roads are well tagged).
- **Table values are not halved for unpaved surfaces**, so a gravel unclassified road gets
  the same 55 km/h as a paved one. The `use_tracks` and `exclude_unpaved` costing options
  still apply.
- `iso3166-1` / `iso3166-2` entries (per country or state) only match when the build's
  `admin.sqlite` contains that boundary. A custom clip usually cuts the boundary relations,
  so its admin database comes out empty (the Permian and Grande Prairie dev builds have zero
  admin rows). That is why the table has no per-region entries.

### Why these values

Without the table, stock speeds were badly off in rural areas:

- An untagged gravel track ran at 2 km/h, so a 0.8 km lease road added about 24 minutes.
- An untagged rural primary ran at 75 km/h. US two-lane state highways are posted
  65–75 mph (105–121 km/h). Midland, TX → Carlsbad, NM took I-20 and US 285 (295 km, 165 min)
  instead of TX/NM 176 (Google: 242 km, 155 min).
- An untagged gravel range road ran at 20 km/h, while one tagged `maxspeed=80` ran at
  70 km/h. In the Peace Country grid, Grande Prairie → a lease road south-west of town went
  out of its way to the tagged range roads: 49.7 km and 52 min, against Google's 39.9 km
  and 33 min. The graph is connected (forcing the route along Range Roads 85/90 works), so
  this was speed, not missing data.

Tagging differs a lot by region. In the dev extracts, the share of primary roads with a
`maxspeed` tag is 98% in the Grande Prairie area (mostly 100 km/h), 61% in Delaware (mostly
80 or 72) and 27% in the Permian (mostly 121 or 113). The values are a compromise: close to
the posted limit on the West's rural highways, a little above Delaware's 50 mph roads, and
a realistic speed on gravel for unclassified roads.

Measured on 2026-11-08 against Google on the comparison cases (auto; km / min):

| Case | Google | Before | After |
|---|---|---|---|
| Midland → Carlsbad | 241.7 / 155 | 295.4 / 165 | **244.5 / 159** |
| Grande Prairie → Montney lease | 39.9 / 33 | 49.7 / 52 | **40.0 / 32** |
| Fox Creek → Duvernay lease (gravel) | 32.6 / 40 | 32.8 / 32 | 32.6 / 31 |
| Pecos → Reeves Co. pad | 34.0 / 31 | 40.0 / 32 | 40.0 / 32 |
| Jal → Lea Co. pad | 33.4 / 58 | 28.7 / 30 | 28.7 / 30 |
| Dover → Hartly (DE county roads) | 18.0 / 20 | 18.1 / 16 | 18.1 / 14 |
| Wilmington → Lewes (DE highway) | 142.8 / 92 | 143.0 / 94 | 143.0 / 93 |

On town-to-town routes (each ordered pair of 8 towns, auto and truck):
- **Delaware (112 routes):** 4 routes changed distance by more than 2%, and all 4 are
  shorter. The median time change is −0.7%, and nothing uses unpaved road away from the ends.
- **Permian (112 routes):** 39 routes changed. Most are shorter and faster, and trucks now
  take TX/NM 176 as well.
- **Grande Prairie area (60 routes):** 2 routes changed, both shorter. No unpaved road
  away from the ends.

**Known cost:** a well-tagged fast rural road loses its tag. Midland → Hobbs used FM 1788
(a secondary tagged 121 km/h) and now rates it at 80, so the route went from 100 to
108 min (+3.5 km).

Tracks still carry the `use_tracks` costing penalty (0.3 by default, set per request by the
Go client), and private tracks can only be used at the start or end of a route.

Caveats:
- A `maxspeed` tag on any rural primary, secondary, tertiary, unclassified, track or service
  road is ignored (see above).
- Farm tracks get the same 30 km/h as lease roads.
- The best fix for regional differences is per-state and per-province entries. That needs
  admin boundaries in the build, which today means a full Geofabrik state or province
  extract rather than a clip.
