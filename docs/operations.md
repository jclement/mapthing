# Operations: build elsewhere, serve on a small machine

A build needs a large machine for a few hours: Planetiler, the Valhalla graph build and the
terrain build want many cores, tens of GB of RAM and several times the output in scratch disk
([sizing.md](sizing.md)). Serving needs very little. The default setup does both in one
container, which means the machine is sized for the build and idles the rest of the week.

This page describes the other arrangement:

```
build machine (large, short-lived)            serving machine (small, always on)
  mapthing-build -regions-file … -out DIR  →  copy  →  <data>/generations/<id>/
                                                       POST /admin/api/generations/<id>/activate
```

Terrain, the largest part of a build, has a third arrangement of its own:
[on-demand terrain](#on-demand-terrain) skips building it and reads it from the public sources as
places are asked for.

Building elsewhere has three independent parts, and each is useful without the others:

1. [`mapthing-build -out`](#1-build-a-generation-into-a-directory) writes a complete generation
   to a directory of your choosing, without a server and without making anything live.
2. [Adopting](#2-adopt-a-generation-built-elsewhere) validates a generation directory that was
   copied into a server's data volume and makes it live with the same atomic swap a build ends
   with.
3. [Slim mode](#3-run-as-a-slim-backend) runs a server without the raster renderer, and
   optionally without a basemap at all, for a machine that only answers routing and elevation
   behind an edge proxy or CDN that serves the tiles from object storage.

## 1. Build a generation into a directory

`mapthing-build` is in the image next to `mapthing`. It runs the same pipeline as the server's
build job.

```
docker run --rm -v build-data:/data -v /builds:/builds --entrypoint mapthing-build IMAGE \
  -regions-file /builds/regions.json -out /builds/next [-reuse-terrain /builds/last] [-refresh]
```

| Flag | What |
|---|---|
| `-regions-file FILE` | The regions to build, as JSON. Can be combined with `-region` and `-custom`. |
| `-out DIR` | Write the generation here instead of `<data>/generations/<id>/`. `DIR` must not exist or be empty. The generation is verified (tiles, a route and an elevation per region) and its manifest written; `current` is not moved and nothing is pruned. If the build fails, `DIR` is removed. |
| `-reuse-terrain DIR` | A previous generation directory. Its terrain is reused when it was built for the same areas with the same settings (below). |
| `-data DIR` | Still needed: downloads, the DEM cache, Planetiler's auxiliary sources and build scratch live there. Keep it between builds and only changed extracts are downloaded again (`-refresh` re-checks them). The disk guard measures free space on this volume, not on `-out`. |

The build prints the generation id at the end (it is also `generation` in
`DIR/manifest.json`).

### The regions file

The file has the shape `GET /admin/api/regions` answers, so the simplest way to make one is to
save that response from a server where the regions were picked in the admin UI. A bare array
works too. Fields a build has no use for (`status`, sizes, dates) are ignored.

```json
{"regions": [
  {"id": "north-america/us/delaware", "catalog_id": "north-america/us/delaware"},
  {"id": "custom/kent", "name": "Kent County", "catalog_id": null, "bbox": [-75.7, 38.9, -75.4, 39.2]},
  {"id": "custom/wedge", "name": "Wedge", "catalog_id": null,
   "polygon": [[-75.7, 38.5], [-75.2, 38.5], [-75.45, 38.9]], "refresh": false}
]}
```

- A region with a `catalog_id`, or with neither `bbox` nor `polygon`, is that catalog extract.
- A region with a `bbox` (`[w, s, e, n]`) or a `polygon` (`[[lon, lat], …]`, or a GeoJSON
  Polygon) is a custom area, clipped from the cheapest set of extracts that covers it, exactly
  as the server does. It keeps its `id`; without one the id is `custom/<slug of the name>`.
- `"refresh": false` excludes a region from `-refresh`, like the per-region switch in the UI.

### Reusing terrain

Terrain is the slow stage, and its input (public elevation models) changes far less often than
OpenStreetMap. A terrain build records a fingerprint next to the archive
(`terrain.pmtiles.json`, field `fingerprint`): the areas (ids, boxes and outlines), the zoom
range, the rounding step, and the list of elevation sources in priority order. With
`-reuse-terrain DIR`, a build whose fingerprint equals the one in `DIR` hard-links
`terrain.pmtiles` from it (or copies it, across filesystems) and skips the terrain stage's
work. `contours.pmtiles` is reused the same way when `MAPTHING_CONTOURS_MAXZOOM` is unchanged,
and is otherwise rebuilt from the reused terrain, which needs no downloads. The reused terrain
still goes through the verify stage.

When the fingerprint differs, or `DIR` has no fingerprint, the build says why in its log and
builds terrain as usual. Things to know:

- Generations built before this feature have no fingerprint, so the first build after an
  upgrade always builds terrain.
- The fingerprint covers what was asked for, not the elevation data itself. To pick up newly
  published elevation data, build once without `-reuse-terrain`.
- Adding, removing or reshaping any region changes the fingerprint and rebuilds the whole
  terrain archive. The DEM cache under `<data>/elevation/cache` still saves the downloads.

## 2. Adopt a generation built elsewhere

Copy the directory into the serving machine's data volume, named after the generation id, then
activate it:

```
ID=$(jq -r .generation /builds/next/manifest.json)
rsync -a /builds/next/ server:/srv/mapthing-data/generations/.$ID.partial/
ssh server mv /srv/mapthing-data/generations/.$ID.partial /srv/mapthing-data/generations/$ID
curl -b cookies -H "Origin: https://maps.example.com" -X POST \
  https://maps.example.com/admin/api/generations/$ID/activate
```

`POST /admin/api/generations/{id}/activate` (admin session) does, in this order:

1. **Validates the directory.** It must be a real directory (not a symbolic link) named after
   the generation in its manifest. The manifest must be of a build that finished. Every file
   the manifest lists must be there at its recorded size, as a regular file with no symbolic
   link anywhere in its path. The archives must open, and `valhalla/valhalla.json` must be a
   routing config (a JSON object with its graph section).
2. **Starts the routing graph** as the blue/green standby and waits for it to answer a status
   request and a test route. Routing is not switched yet.
3. **Moves `current`** to the generation (one atomic rename). Tiles and elevation reopen at
   once. If this fails (a full or read-only volume), the standby is stopped and nothing has
   changed: tiles, elevation and routing are all still the old generation.
4. **Switches routing** to the standby and retires the old instance.
5. **Applies retention**: the live generation and the newest other complete one stay; older
   ones are deleted. Only generations this server owns are considered (below).

It answers `200 {"id", "status": "live", "already_live", "previous", "removed": […]}`.

A refusal changes nothing, and the live generation keeps serving:

| Status | `error.code` | When |
|---|---|---|
| 404 | `no_generation` | there is no `<data>/generations/<id>` directory |
| 409 | `busy` | a build or another activation is running |
| 422 | `invalid_generation` | step 1 failed; `error.problems` lists every problem, e.g. `valhalla/tiles.tar is missing` or `terrain.pmtiles is 1048576 bytes, the manifest says 511705088 (partial copy?)` |
| 422 | `routing_failed` | step 2 failed: the graph did not load or route within 5 minutes |
| 500 | `internal` | step 3 failed: `current` could not be moved; the message says why |

It is **idempotent**: activating the live generation again answers 200 with
`"already_live": true` and does nothing. A script can retry it safely.

Activating any kept generation works the same way, so this is also how to **roll back** to the
previous one.

`GET /admin/api/generations` lists a copied generation as `kept` as soon as its directory is
there, and as `live` once activated.

### What retention may delete

`generations/` is shared between the server and whoever copies generations in, so the server
keeps a record (`generations/.retention.json`) of the generations it **owns**: the ones a
build on this server finished, and the ones an activation made live. Retention, after a build
and after an activation alike, only ever looks at those.

- A directory the server does not own is never deleted and never counts as the generation
  kept for rollback. That holds whatever state it is in: no manifest yet, a manifest but not
  all files, or complete and waiting to be activated.
- **Activating a generation is the handoff.** From then on it is retained like one built here.
- The live generation is never deleted.
- A copy you decide not to activate is yours to delete.
- The first start after upgrading from a version without the record takes every complete
  generation already in `generations/` as owned.

### Copying safely

- **Copying straight into `generations/<id>/` while the server runs is safe**, including while
  it builds: an unfinished copy is refused by activation (missing files, or sizes that do not
  match the manifest) and ignored by retention.
- Copying under a dot name and renaming, as in the example, additionally keeps a half-arrived
  generation out of `GET /admin/api/generations`. Directories whose name starts with a dot are
  not generations: they are not listed and cannot be activated.
- **No symbolic links.** A generation is a real directory under `generations/` holding its own
  regular files. A link in place of the directory or of any file in it is refused with
  `invalid_generation`. Use `rsync -a --copy-links` (or `-L`) if the source has links.
- A generation is relocatable. The `valhalla.json` inside it names the paths of the machine
  that built it; the server points each routing instance at the graph where the generation is
  now.
- The copy must be readable by the server's user (uid 10001 in the image).

### From a shell on the server: `mapthing adopt`

```
docker exec mapthing mapthing adopt /data/incoming/next      # a directory: moved in, then made live
docker exec mapthing mapthing adopt 20261109T130030Z          # an id already under generations/
```

Given a directory, it validates it where it is, moves it to `<data>/generations/<id>` (a
rename on the same filesystem, otherwise a copy under a dot name and a rename) and makes it
live. The running server notices within two seconds and follows: tiles at once, routing
blue/green. It reads `MAPTHING_DATA` and `MAPTHING_SERVE` from the environment, so run it
inside the container. Two differences from the API:

- It cannot try the routing graph before the swap (the files and the routing config are
  validated the same way). If the graph then does not load, tiles and elevation are already on
  the new generation while routing stays on the old graph, and the server logs
  `routing service not switched`. Prefer the API when the copy is unproven.
- It does not know whether a build is running in the server process. Do not run it during one.

## 3. Run as a slim backend

Two environment variables turn parts of the server off. Both are reported by
`GET /admin/api/system` (`renderer`, `serve`) and in the startup log line.

### `MAPTHING_RENDERER=off`

No renderer is started: no Xvfb, no Node, no MapLibre Native. Raster tile endpoints
(`/styles/{name}/{size}/{z}/{x}/{y}` and the Mapbox-shaped ones) answer
`404 {"error":{"code":"renderer_off", …}}` without touching anything else. `/readyz` and the
admin UI's readiness have no renderer to wait for. Vector tiles, styles, glyphs, sprites,
packs, routing and elevation are unaffected.

404 rather than 503 is deliberate: the tiles are not offered here, and a proxy in front should
neither retry nor mark the backend unhealthy because of them.

### `MAPTHING_SERVE`

A comma-separated list of what this server serves from a generation: `tiles`, `routing`,
`elevation`. The default is all three.

| Value | Effect |
|---|---|
| without `tiles` | A generation needs no `basemap.pmtiles` or `contours.pmtiles`: it validates, adopts and is ready without them. The renderer is off regardless of `MAPTHING_RENDERER`. Tile endpoints stay mounted and answer `404 no_tileset` for an archive that is not there. |
| without `routing` | No Valhalla is started, the routing APIs are not mounted (404), and a generation needs no `valhalla/`. |
| without `elevation` | The elevation APIs are not mounted (404), and a generation needs no `terrain.pmtiles`. |

A backend for routing and elevation behind an edge proxy:

```
docker run -v data:/data -e ADMIN_USER -e ADMIN_PASSWORD \
  -e MAPTHING_SERVE=routing,elevation -p 8080:8080 IMAGE
```

Then only part of each generation has to be copied to it:

```
rsync -a --include='manifest.json' --include='terrain.pmtiles*' --include='valhalla/***' \
  --exclude='*' /builds/next/ server:/srv/mapthing-data/generations/.$ID.partial/
```

The manifest still lists the basemap; validation only requires the files of the parts this
server serves (a file that *is* there must still be whole). Terrain is used by
`/api/v1/elevation` and is also still served as tiles at `/tiles/terrain/…`.

Such a server can still run builds from its admin UI, but there is little point: it would
build the basemap it does not serve.

### What it saves

Measured on one host with the same image, the same generation and no traffic, 90 seconds after
start (sum of the resident memory of every process in the container; method and caveats in
[sizing.md](sizing.md#a-slim-backend)):

| Mode | Idle memory |
|---|---|
| default (renderer on) | 247 MiB |
| `MAPTHING_RENDERER=off` | 99 MiB |

The renderer's share grows under load (it keeps a pool of maps per style), so the difference
in a busy server is larger than at idle. What is left is the Go server (about 28 MiB) and
`valhalla_service`, whose resident size is mostly the memory-mapped graph and follows the
graph's size and how much of it has been touched.

## On-demand terrain

Terrain is the largest and slowest part of a build ([sizing.md](sizing.md)). As an option, a
region's terrain, or terrain for everywhere outside your regions, can be **read on demand**
instead: the first time a tile or an elevation is asked for, mapthing reads the small window it
needs out of the same public cloud-optimised GeoTIFFs the build uses, answers, and keeps the
result in a cache. The second request for the same place is as fast as built terrain.

It is off unless you turn it on, and a server that never turns it on behaves exactly as before:
nothing is fetched at request time.

| | Built (default) | On demand |
|---|---|---|
| First view of a place | instant | about a second (0.7 to 2.2 s for one tile, 1 to 3.4 s for a screen) |
| Later views | instant | instant, from the cache |
| Internet at request time | not needed | needed for places not seen before |
| Disk | 110–150 KB per deepest-zoom tile, for the whole region | the region's zoomed-out terrain (about 1 % of that), plus a cache you size |
| Build time | most of a build | about a minute per Texas-sized region |
| Works when a DEM host is down | yes | what is cached, yes; anything new answers 504 |

### What it covers, and what it does not

- **Terrain tiles** (`/tiles/terrain/{z}/{x}/{y}.png`) at zooms 9 to 13, so hillshade in every
  style, in the browser and in rendered raster tiles.
- **Elevation**: `/api/v1/elevation`, `/api/v1/elevation/profile` and the Google-compatible
  `/maps/api/elevation/json`. They sample the same zoom-13 tiles built terrain is sampled from,
  so an on-demand elevation is the same kind of answer as a built one.
- **Not map tiles and not routing.** They are always built: the basemap comes from an OSM
  extract and Valhalla needs a graph, and neither can be fetched a piece at a time.
- **Not contours** (yet): contour lines exist only where terrain was built.
- **Below zoom 9**, terrain is always built. A region set to on demand still gets zooms 0 to 9
  made by the build (from the sources' overviews, without GDAL), so a zoomed-out map never
  waits. Outside every region there is nothing below zoom 9: zoomed out that far, terrain shows
  only where a region exists.

The sources and their order are those of the build ([elevation-sources.md](elevation-sources.md)):
NRCan HRDEM, USGS 3DEP 1/3″, NRCan MRDEM-30, Copernicus GLO-30. Where the best one has no data
for a pixel, the next one fills it. **Built terrain always wins**: a tile the live archive has is
served from it and the sources are never asked.

### Turning it on

- **For a region**: on the Regions page, under the region's elevation sources, choose *Terrain
  built* or *Terrain read on demand*; or when adding the region; or
  `PATCH /admin/api/regions/{id}` with `{"terrain_mode":"on_demand"}` (`"prebuilt"` to go back).
  Switching to on demand applies at once to places with no built terrain; terrain already built
  keeps being served until the next build, which leaves it out and frees its disk. Switching back
  takes effect with the next build; until that build has made the terrain, the region keeps
  being read on demand.
- **Outside built regions**: Settings → On-demand terrain, or
  `PUT /admin/api/ondemand` with `{"outside":"on_demand"}` (`"off"` is the default). With it on,
  any place on Earth has terrain and elevation from zoom 9 in.

Both are stored in the database and apply to the next request; there is nothing to restart and
no environment variable.

### The cache

One cache holds everything on-demand terrain fetches, under `<data>/ondemand-cache`: the finished
tiles, the source blocks they were made from (a neighbouring tile or the next zoom often needs
the same block) and the files' headers. It has one budget, 5 GB by default, set in Settings or
with `PUT /admin/api/ondemand` `{"cache_mb":20000}`. When it is full, what was used longest ago
is dropped first, which in practice means source blocks go before tiles. A finished tile is 17 to
60 KB, so 5 GB holds on the order of 100,000 tiles. The Overview shows how full it is, how much
is answered from it, and each source's health; Settings can empty it.

Cached data is **versioned**. mapthing remembers each source file's ETag and asks the host
again once a day; when a file has changed, tiles made from the old one are rebuilt the next time
they are asked for, and the old blocks are deleted. The versions are kept in
`<data>/ondemand/versions.json`, so after a restart, or with no internet, cached tiles are
still served.

### When a source does not answer

A request that needs data a host will not give is answered with an error, and **nothing is
cached**, so the next request tries again:

- terrain tile: `504` with `{"error":{"code":"terrain_source_unavailable",…}}`,
  `Cache-Control: no-store` and `Retry-After: 30`;
- `/api/v1/elevation…`: `504` with `{"error":"…"}`;
- `/maps/api/elevation/json`: `504` with `{"status":"UNKNOWN_ERROR","error_message":"…","results":[]}`
  (Google's own status for "try again").

A raster tile drawn while its terrain could not be read is served without hillshade and not
cached, as with any other source that fails. The Overview names the host that is not answering
and when it last did.

### Limits and manners

mapthing reads other people's servers here, so it is careful by design:

- at most 16 requests in flight to the DEM hosts, 8 to any one host; each attempt times out
  after 20 s; a failed request is retried twice, with a growing pause;
- requests for the same tile, block or header that arrive together share one fetch;
- every request carries `User-Agent: mapthing (+https://github.com/jclement/mapthing)`;
- only public, anonymous files are read (USGS and NRCan publish theirs as public data, the
  Copernicus DEM under its own free licence): the attribution is the same as for built terrain
  and is in every style and in the terrain TileJSON.

A request that has to read from the hosts also **costs its API key more**: on top of its own
kind it is charged one `ondemand` request (20 units by default, against 1 for a tile and 5 for an
elevation request), and the server caps such requests across all keys (20 per second by
default). Both figures are with the other limits on the Keys page. A cached answer pays nothing
extra. A key that cannot pay is answered `429` with `Retry-After` before anything is fetched.
One elevation request may need at most 600 on-demand tiles (about 2,000 km of profile); a
larger one is refused with `413` and nothing is fetched.

### What a first request costs

Measured from one server against the live hosts with nothing cached ([sizing.md](sizing.md) has
the full tables). "Later" means the files' headers are already known, as they are once a server
has served anything nearby.

| | First ever (p50 / p95) | Later, new tile (p50 / p95) | Read from the hosts |
|---|---|---|---|
| One tile, Texas (3DEP) | 0.9 / 1.2 s | 0.5 / 0.6 s | 2.7 MB |
| One tile, Calgary (HRDEM + MRDEM) | 1.6 / 1.9 s | 0.7 / 0.9 s | 3.8 MB |
| One tile, northern Alberta (MRDEM) | 1.7 / 2.1 s | 0.8 / 1.1 s | 3.1 MB |
| One tile, Alps (GLO-30) | 0.8 / 1.7 s | 0.4 / 1.4 s | 3.4 MB |
| A screen of 12 tiles | 1.0 to 3.4 s | | 4 to 24 MB |
| A 100 km profile at 30 m | 1.9 to 4.8 s | | 16 to 61 MB |

The megabytes matter more than the milliseconds on a slow line: the sources store elevation in
blocks much larger than a tile, so a cold screen is 4 to 24 MB of download. On a 20 Mbit/s
uplink that alone is 2 to 10 s.

## What these do not do

- There is no upload endpoint: getting the directory onto the server is rsync, a volume
  snapshot or an object-storage sync, whichever the deployment has.
- Region lists do not travel with a generation. A server that adopted a generation shows its
  regions in the generation's manifest, not under Regions. A build run on that server (by hand
  or by an enabled schedule) builds its own region list and replaces the adopted generation, so
  leave the schedule off on a server that adopts.
- API keys, usage and settings live in each server's `mapthing.db` and are not part of a
  generation.
