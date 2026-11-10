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

It has three independent parts, and each is useful without the others:

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

## What these do not do

- There is no upload endpoint: getting the directory onto the server is rsync, a volume
  snapshot or an object-storage sync, whichever the deployment has.
- Region lists do not travel with a generation. A server that adopted a generation shows its
  regions in the generation's manifest, not under Regions. A build run on that server (by hand
  or by an enabled schedule) builds its own region list and replaces the adopted generation, so
  leave the schedule off on a server that adopts.
- API keys, usage and settings live in each server's `mapthing.db` and are not part of a
  generation.
