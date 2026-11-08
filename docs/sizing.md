# Sizing: build time, disk and memory

How long a build takes, and how much disk and memory it needs, for whole-country builds of the
**United States**, **Canada** and **Saudi Arabia**, per component and in total, plus offline pack
sizes for phones. Measured November 2026.

Every figure carries one of three labels:

- **measured**: built and measured on the reference machine below;
- **published**: taken or cut from published data (an HTTP HEAD, an S3 listing, the published
  Protomaps planet build);
- **extrapolated**: a ratio measured on the builds below, applied to a bigger input.

Ranges are the spread across the regions measured (smallest to largest ratio), so they say how
much regions differ. They are not statistical confidence intervals. No country-sized build was
run here: the disk was too small (see [What was not measured](#what-was-not-measured)).

## Summary

On the reference machine (10 vCPU, 47 GB RAM), building everything from scratch:

| | United States | Canada | Saudi Arabia |
|---|---|---|---|
| OSM extract (PBF) | 12.3 GB (published) | 6.5 GB (published) | 0.12 GB (published) |
| Vector basemap, z0–15 | **17 GB** (published) | **16 GB** (published) | **0.42 GB** (published) |
| Routing graph (Valhalla tar) | 20 GB (10–39) extrap. | 10.5 GB (5–21) extrap. | 0.2 GB (0.1–0.4) extrap. |
| Terrain, as built today | 18 GB at z11 extrap. | 41 GB at z11 extrap. | 3.7 GB at z12 extrap. |
| **One generation** | **55 GB** (45–77) | **68 GB** (59–81) | **4.3 GB** (3.8–4.9) |
| **Disk, steady state** (2 generations, PBF, DEM cache) | **140 GB** (120–187) | **198 GB** (155–251) | **11 GB** (10–13) |
| **Disk, peak during a build** | **265 GB** (215–370) | **300 GB** (240–390) | **16 GB** (14–19) |
| Planetiler (basemap) | 1 h 45 (1 h 25–1 h 55) extrap. | 55 min (45–60) extrap. | 3 min (measured on GCC) |
| Valhalla (routing) | 1 h 05 (45 min–2 h 05) extrap. | 35 min (25–65) extrap. | 1 min extrap. |
| Terrain, cold DEM cache | 5 h (4 h 25–5 h 10) extrap. | 9 h (5–13 h) extrap. | 1 h (50–70 min) extrap. |
| **Full build, from scratch** | **~8 h** | **~11 h** (7–15) | **~1 h 10** |
| Scheduled refresh (full rebuild, DEM cache warm) | ~6 h | ~6 h 45 | ~35 min |
| Peak memory needed | 16 GB min, 32–64 GB comfortable | 16 GB min, 32 GB comfortable | 8 GB min, 16 GB comfortable |

The terrain (elevation) build dominates both time and disk, and it is network-bound. "As built
today" means at the zoom the current code chooses: its 450,000-tile budget pushes both large
countries down to z11 (about 70 m pixels at 30°N). Full-resolution terrain, z13 for the US (10 m
3DEP) or z12 for Canada (30 m MRDEM), is about 100 GB and 27 h for the US, and 72 GB and 16 h for
Canada. See [Elevation](#elevation).

### Machine per country

| | Minimum | Comfortable |
|---|---|---|
| **United States** | 8 cores, 16 GB RAM, 300 GB SSD | 16 cores, 64 GB RAM, 500 GB NVMe (1 TB for full-resolution terrain) |
| **Canada** | 8 cores, 16 GB RAM, 320 GB SSD | 16 cores, 32 GB RAM, 500 GB NVMe |
| **Saudi Arabia** (or all GCC states) | 4 cores, 8 GB RAM, 25 GB SSD | 8 cores, 16 GB RAM, 50 GB SSD |

"Minimum" means the build completes, slowly. "Comfortable" means the build runs beside a live
server without starving it, and Planetiler keeps its working set in RAM. The memory minimums
assume Planetiler's memory-mapped mode for the two big countries, and include headroom for
`valhalla_service` and the raster renderer serving the previous generation while the next one
builds. The Valhalla memory figure for a country-sized graph is an assumption, not a measurement
(see [Memory](#memory)).

## Reference machine and method

| | |
|---|---|
| CPU | Intel Core i7-8700 @ 3.20 GHz, as a 10-vCPU virtual machine (QEMU) |
| RAM | 47 GB (shared with other work) |
| Disk | virtual disk (QEMU), SSD-backed host |
| Network | ~10 MB/s from Geofabrik (measured: 6.9–13.4 MB/s) |
| Image | `mapthing:dev`: Valhalla 3.9.1, Planetiler with the Protomaps basemap profile (4.15.x), GDAL from Ubuntu 24.04 |

The machine was shared with other builds during the measurements. Containers ran at a low CPU
weight (`--cpu-shares 512`), so they yielded to other work, and timings are, if anything,
pessimistic.

**Harness.** [`scripts/measure-build.sh`](../scripts/measure-build.sh) runs `mapthing-build` (the
same pipeline the server runs as a job) inside the image against a scratch data directory. It
samples from the host every 5 s: `du` of the data directory, and the container's cgroup
`memory.peak`, `memory.current` and `anon`. It appends one row per build to
[`sizing/measurements.csv`](sizing/measurements.csv): stage times, peak memory, peak disk, output
sizes, the input PBF size, the extract polygon's area, machine facts and the image id.
[`scripts/measure-terrain.sh`](../scripts/measure-terrain.sh) does the same for terrain builds of a
bounding box ([`sizing/terrain.csv`](sizing/terrain.csv)).
[`scripts/measure-packs.sh`](../scripts/measure-packs.sh) records offline pack sizes
([`sizing/packs.csv`](sizing/packs.csv)). [`scripts/measure-raster.sh`](../scripts/measure-raster.sh)
samples rendered raster tile sizes ([`sizing/raster.csv`](sizing/raster.csv)).
[`scripts/sizing-model.py`](../scripts/sizing-model.py) turns all of that, plus the published inputs
in [`sizing/inputs.json`](sizing/inputs.json), into the per-country figures.

**Measured builds** (whole Geofabrik extracts, built one at a time):

| Region | PBF | Basemap | Valhalla tar | Planetiler | Valhalla | Peak disk above caches | Peak anon memory |
|---|---|---|---|---|---|---|---|
| Delaware | 22 MB | 32 MB | 54 MB | 146 s | 13 s | 0.9 GB | — |
| New Mexico | 137 MB | 258 MB | 237 MB | 172 s | 51 s | 1.3 GB | — |
| Louisiana | 148 MB | 212 MB | 220 MB | 194 s | 41 s | 1.1 GB | — |
| GCC states (Saudi Arabia, UAE, Kuwait, Qatar, Bahrain, Oman) | 257 MB | 669 MB | 828 MB | 256 s | 157 s | 3.8 GB | 4.2 GB |
| Alberta | 352 MB | 914 MB | 281 MB | 281 s | 74 s | 3.3 GB | 4.6 GB |

Texas (727 MB) and British Columbia (1.25 GB) were planned but not built: the shared disk had
~12 GB free, with a 6 GB floor.

**Ocean polygons.** During the measurements, `osmdata.openstreetmap.de` (the only source of OSM
water and land polygons) was unreachable from the reference machine. The first four builds used
empty placeholder polygons. The last two (GCC, Alberta) used the pipeline's fallback, Natural Earth
ocean polygons. That changes coastal basemaps by about ±5% (GCC: 617 MB with no ocean, 669 MB with
Natural Earth, 652 MB in the published planet build). The time the pipeline spent retrying that
host (~5.5 min per build) is excluded: the CSV keeps it apart as `t_sources_s`.

### Why basemap sizes come from the published planet build

A power-law fit to five regions put the US basemap at 46 GB. That figure is wrong: small regions
carry low-zoom and landcover overhead that does not grow with the input. The Protomaps project
publishes a daily planet build with the same profile and zoom range (z0–15). On 2026-11-07 it was
139.4 GB, built from a 95.7 GB planet PBF. `pmtiles extract --dry-run` against it reports the
exact archive size for any polygon, and fetches only directories.

As a check, the same cut for each region built here:

| Region | Own build | Cut from the planet build |
|---|---|---|
| Delaware | 32 MB | 34 MB |
| New Mexico | 258 MB | 258 MB |
| Louisiana | 212 MB | 221 MB |
| GCC states | 617 MB (no ocean) / 669 MB (Natural Earth ocean) | 652 MB |
| Alberta | 914 MB | 897 MB |

They agree within 6%, so the country basemap sizes in this document are cut from the planet build
([`sizing/planet-extracts.csv`](sizing/planet-extracts.csv)). Saudi Arabia was cut with an
approximate hand-drawn outline, so treat its figure as ±10%.

## Components

### OSM download

| | US | Canada | Saudi Arabia |
|---|---|---|---|
| Extract | `north-america/us` 12.25 GB | `north-america/canada` 6.51 GB | 117 MB (openstreetmap.fr); Geofabrik only has `asia/gcc-states`, 257 MB |
| Download at 10 MB/s | 20 min (15–30) | 11 min (8–16) | under 1 min |
| Weekly change (Geofabrik daily diffs, last 7 days) | 92 MB | 71 MB | 3 MB (GCC) |

Sizes from HEAD requests on 2026-11-08. Geofabrik has no Saudi-only extract. mapthing installs
`asia/gcc-states`, or a custom polygon clipped from it. The GCC build here is therefore the honest
Saudi measurement: it is a superset, with 2.2× Saudi's PBF bytes.

### Vector basemap (Planetiler, Protomaps profile, z0–15)

| | US | Canada | Saudi Arabia |
|---|---|---|---|
| `basemap.pmtiles` z0–15 | 17 GB | 16 GB | 0.42 GB |
| same, capped at z14 | 8.0 GB | 8.4 GB | 0.21 GB |
| Planetiler time | 1 h 43 (1 h 24–1 h 53) | 56 min (46–61) | ~3 min |

Size: published (planet cut). Time: extrapolated. Planetiler on this machine costs a fixed
~2 min per build (JVM start plus re-reading the Natural Earth geopackage, measured 83–96 s), plus
**400–540 s per GB of PBF** (measured, 5 regions). For comparison, the Planetiler README reports
the planet (73 GB) in 3 h 35 on an 8-vCPU AWS c7gd.2xlarge with 16 GB (OpenMapTiles profile),
about 180 s per GB: a modern machine is ~2.5× faster per GB than this one. The US build would use
Planetiler's memory-mapped mode (the pipeline switches when 1.5× the PBF exceeds 70% of the heap)
unless the machine has ~32 GB free for the heap. That mode is slower on a slow disk; on NVMe the
Planetiler authors report little difference.

Canada's basemap is as large as the US one from half the input: lakes, wetlands, forest
landcover, and the Arctic, which Web Mercator stretches.

### Routing (Valhalla)

| | US | Canada | Saudi Arabia |
|---|---|---|---|
| `tiles.tar` | 20 GB (10–39) | 10.5 GB (5–21) | 0.2 GB (0.1–0.4) |
| Build time | 1 h 06 (43 min–2 h 05) | 35 min (23–66) | ~1 min |

Extrapolated from the ratio of tar to PBF (measured 0.80–3.2×, median 1.6×) and seconds per GB of
PBF (measured 210–610 s/GB). The spread is wide because road density per PBF byte varies:
Alberta's graph is sparse (0.8× its PBF), while the GCC's dense urban grids are 3.2×. The US
should sit near the middle; nothing here measured a graph above 0.83 GB.

### Elevation

The terrain build reads each DEM as a cloud-optimised GeoTIFF over HTTP range requests, at the
resolution it needs. It writes Terrarium PNG tiles with a per-pixel source index into
`terrain.pmtiles` (see [elevation-sources.md](elevation-sources.md)). It does not download source
tiles whole, so the full source volumes below are context, not what a build fetches.

**Source volumes (published):**

| Source | Coverage | Size |
|---|---|---|
| USGS 3DEP 1/3″ (~10 m), 1°×1° COGs | 1,447 tiles: CONUS 968 (365 GB), Alaska 442 (126 GB), Hawaii 14 (1.9 GB); Texas bbox 127 (50 GB) | 494 GB |
| USGS 3DEP 1 m | patchy, project-based; a state runs to terabytes | not used by default |
| NRCan MRDEM-30 | all of Canada, one COG | 84 GB |
| NRCan HRDEM 2 m mosaics | 64 mosaics where LiDAR exists; Alberta's four ~343 GB, BC's four ~107 GB | 2.3 TB |
| Copernicus GLO-30 | ~230 1°×1° cells over Saudi Arabia, ~39 MB each | ~9 GB |
| BC and Alberta provincial DEMs | not openly scriptable or superseded by MRDEM (see elevation-sources.md) | not used |

**Measured terrain builds** (1°×1° boxes, empty DEM cache, 4 workers):

| Box | Sources used | Max zoom | Max-zoom tiles | Archive | DEM fetched | Time | Peak memory |
|---|---|---|---|---|---|---|---|
| Permian, TX | 3DEP 1/3″ | 13 | 644 | 71 MB | 68 MB | 70 s | 1.3 GB |
| Haynesville, LA/TX | 3DEP 1/3″ | 13 | 672 | 84 MB | 83 MB | 64 s | 1.4 GB |
| Calgary, AB | HRDEM 34%, MRDEM 38%, GLO-30 29% | 13 | 851 | 124 MB | 85 MB | 51 s | 1.7 GB |
| Peace River, AB | HRDEM 97% | 13 | 966 | 120 MB | 259 MB | 151 s | 3.2 GB |
| Riyadh | GLO-30 | 12 | 156 | 24 MB | 16 MB | 17 s | 0.8 GB |
| Saudi desert | GLO-30 | 12 | 156 | 19 MB | 12 MB | 25 s | 0.6 GB |

Per max-zoom tile, lower zooms included: **110–150 KB of archive**, 80–270 KB fetched, and
0.06–0.16 s of wall time. Areas with HRDEM LiDAR fetch the most, because the 2 m source is read
through its overviews.

**Countries** (extrapolated: per-tile figures × tile counts):

| | US | Canada | Saudi Arabia |
|---|---|---|---|
| Zoom the current code picks | z11 (budget: 2.7 M z13 tiles over the Geofabrik polygon) | z11 (budget) | z12 (GLO-30 native) |
| Archive, as built today | 18 GB (18–21) | 41 GB (38–44) | 3.7 GB (3.3–4.1) |
| Build time, cold DEM cache | 5 h (4 h 25–5 h 10) | 9 h (5–13 h) | 1 h (48–71 min) |
| DEM fetched (kept as the cache) | 18 GB | 56 GB (30–82) | 2.4 GB |
| Full resolution, land only | z13: 896 k tiles, 99–112 GB, ~27 h, 95–110 GB fetched | z12: 534 k tiles, 72 GB, ~16 h, 98 GB fetched | as above |

The z11 choice is the current code's tile budget (450,000 max-zoom tiles), applied to the
Geofabrik polygon. That polygon includes a coastal buffer and, for Canada, the Arctic Ocean
between the islands. A land-only polygon would let the US build at z12 (~224 k tiles, ~26 GB).
Canada's land tile count is a rough estimate by latitude band (±30%). In Web Mercator the Arctic
islands dominate it.

The terrain build's time is mostly waiting on HTTP range reads (4 workers). More workers would
probably shorten it, but that was not measured.

### Raster tiles (on-demand cache, not pre-rendered)

Raster tiles are rendered on request from the vector basemap and cached, so their disk use grows
with what users look at, not with the country. The cache's growth is
(distinct tiles viewed) × (bytes per tile), so the useful numbers are the bytes per tile
(measured) and how many tiles an area has.

**Bytes per rendered tile** (measured: 12 random tiles per zoom, rendered by the MapLibre Native
renderer from a Planetiler basemap, `outdoors` style; satellite = the `satellite` style over
proxied imagery):

| Tile | z10 | z12 | z14 | z16 |
|---|---|---|---|---|
| 512 px @2x, rural (Permian lease roads) | 283 KB | 292 KB | 115 KB | 57 KB |
| 512 px @2x, small town (Delaware) | 917 KB | 640 KB | 244 KB | 116 KB |
| 256 px, rural | 37 KB | 25 KB | 15 KB | 8 KB |
| 256 px, small town | 88 KB | 70 KB | 37 KB | 18 KB |
| 512 px @2x satellite (PNG) | 2.3 MB | 2.5 MB | 2.4 MB | 2.0 MB |

Render time once warm: 15–350 ms per tile (mean per zoom). Low zooms are slower because they
carry more features.

**What an area costs if every tile in it is viewed once at z10–16** (extrapolated: tile counts ×
the bytes above, odd zooms interpolated):

| Area | Tiles | 512 @2x street | 256 street | 512 @2x satellite |
|---|---|---|---|---|
| County, 40×40 km | 7.9 k | 0.5 GB (rural) – 1.1 GB (town) | 0.07–0.17 GB | 16 GB |
| Basin, 100×150 km | 79 k | 5.2–10.9 GB | 0.7–1.7 GB | 162 GB |

Real use views a small fraction of an area's z15–16 tiles: roads, sites, the places people work.
At 10% of a basin, the street cache is ~0.5–1 GB per style and pixel size.

**Satellite tiles are served as PNG**: 2–2.5 MB per 512 @2x tile. Encoding them as JPEG would
cut that about tenfold. It was not measured here; it is the usual ratio for aerial imagery.

Size the raster cache by its configured bound, not by the country. A few GB holds a busy
basin's street tiles; satellite needs either JPEG or a much larger bound.

### Memory

Peak memory, by component (measured on the builds above unless noted):

| Component | Measured | Notes |
|---|---|---|
| Planetiler | 4.0–4.6 GB anonymous memory with a 4 GB heap (GCC, Alberta); cgroup peak 7–14 GB including page cache | The pipeline sets the heap to 60% of *available* memory (1–32 GB). Rule of thumb from the Planetiler docs: ≥0.5× the PBF in free RAM; ~1.5× the PBF in heap to keep everything in RAM. US: 6 GB minimum (mmap mode), ~18 GB heap for RAM mode. Canada: 3.5 GB minimum, ~10 GB for RAM mode. |
| Valhalla build | 0.8 GB (Alberta) and 4.2 GB (GCC) anonymous, in its graph-enhancing phase with 10 threads | Two points are not enough to fit a growth law. **Assumption, not measured:** budget 8–16 GB for a US or Canada graph. A Texas build (727 MB PBF) would settle it. |
| Terrain build | 0.6–3.2 GB (4 workers) | Does not grow with the region: it works chunk by chunk. |
| Server while serving | not measured here | `valhalla_service`, the renderer (MapLibre Native, 2 maps per style) and the Go server run next to the build. |

The build stages run one after another, so the peak is the largest stage, not the sum.

Process RSS overstates the requirement: Valhalla's RSS reached 7.8 GB on the GCC build, mostly
memory-mapped files the kernel can drop. The figures above are the cgroup's anonymous memory,
which cannot be reclaimed.

### Disk

| | US | Canada | Saudi Arabia |
|---|---|---|---|
| One generation (basemap + routing + terrain) | 55 GB (45–77) | 68 GB (59–81) | 4.3 GB |
| Steady state: PBF + 2 generations kept + DEM cache | 140 GB (120–187) | 198 GB (155–251) | 11 GB |
| Extra during a build: new generation + temp files | +123 GB (96–184) | +104 GB (86–138) | +5 GB |
| **Peak during a build** | **~265 GB** | **~300 GB** | **~16 GB** |

Plus small fixed items: Planetiler's auxiliary sources cache, 0.6 GB measured (Natural Earth,
landcover, QRank, without the unreachable ocean polygons; the pipeline's guard assumes up to 4 GB
with them). Plus the imagery and raster caches, which are bounded by configuration.

Temp files during a build were 4.1–8.7× the PBF (measured, beyond the input and outputs). They
are Planetiler's sort files and node map, and Valhalla's loose tiles, which sit beside the tar
until the work directory is removed. That matches the Planetiler README's "5–10× the PBF". The
architecture keeps the last two good generations; keeping only one would save one generation
(55 GB for the US).

### Scheduled refresh

The pipeline rebuilds everything on each refresh; nothing is incremental. The DEM cache survives
between builds, so a refresh's terrain stage reads no DEM over the network. Rebuilding the
Permian 1° box on a warm cache took 39 s against 72 s cold (measured, one box: 0.55×). The rest
of the terrain time is GDAL warping and PNG encoding.

| | US | Canada | Saudi Arabia |
|---|---|---|---|
| Full rebuild (DEM cache warm) | ~6 h (4 h 50–7 h 15) | ~6 h 45 (4 h–9 h 40) | ~35 min (30–45) |
| of which terrain | 2 h 45 | 5 h | 30 min |
| of which download | 20 min | 11 min | <1 min |

**What replication diffs would save.** Geofabrik publishes daily `.osc.gz` diffs. A week of
changes is 92 MB for the US and 71 MB for Canada. Updating the stored PBF from diffs (`osmium
apply-changes`, or `pyosmium-up-to-date`) instead of re-downloading saves the download, about
20 min for the US and 11 min for Canada at 10 MB/s, and ~12 GB of transfer. Applying a week of Louisiana diffs to its 148 MB PBF took 5.0 s
(`osmium apply-changes` in the image, measured): it rewrites the whole file, at ~34 s per GB.
That is ~7 min for the US and ~4 min for Canada (extrapolated), so the net saving is ~13 min and
~7 min. Neither
Planetiler nor Valhalla can update their output in place: both rebuild from the full PBF, so
diffs save the download step and nothing else. Terrain does not change with OSM at all. It only
needs rebuilding when a DEM source changes, which 3DEP ETags can detect. Skipping it on an
OSM-only refresh is the biggest available saving (4–9 h for the large countries).

## Offline packs (phones)

`/packs/extract.pmtiles` cuts a subset of the basemap. Sizes, measured from the built archives
(`pmtiles extract --dry-run`) and, for the countries, cut from the published planet build:

| Area | z12 | z13 | z14 | z15 (full detail; z16 = z15, clients overzoom) |
|---|---|---|---|---|
| County, ~40×40 km, rural (Eddy Co., NM) | 0.6 MB | 1.0 MB | 1.8 MB | 3.3 MB |
| County, ~40×40 km, mixed (Caddo Parish, LA) | 0.9 MB | 1.9 MB | 3.8 MB | 7.6 MB |
| County, ~40×40 km, suburban (New Castle, DE) | 1.3 MB | 2.6 MB | 5.0 MB | 9.4 MB |
| City, ~40×40 km (Calgary) | 2.9 MB | 6.8 MB | 16 MB | 40 MB |
| County, ~40×40 km, rural (Grande Prairie area, AB) | 0.8 MB | 1.4 MB | 2.5 MB | 4.8 MB |
| City, ~40×40 km (Riyadh) | 2.1 MB | 4.0 MB | 7.9 MB | 14 MB |
| Desert, ~40×40 km (Saudi interior) | 0.2 MB | 0.2 MB | 0.2 MB | 0.2 MB |
| Basin, ~100×150 km (Delaware Basin, NM side) | 2.5 MB | 5.4 MB | 10 MB | 19 MB |
| Basin, ~100×150 km (Haynesville, LA) | 2.5 MB | 4.9 MB | 9.6 MB | 19 MB |
| Basin, ~100×150 km (Montney, AB) | 2.9 MB | 4.8 MB | 8.7 MB | 17 MB |
| Basin, ~100×150 km (Eastern Province oilfields) | 0.8 MB | 1.2 MB | 1.9 MB | 3.1 MB |
| State: New Mexico | 25 MB | 60 MB | 127 MB | 258 MB |
| State: Louisiana | 25 MB | 53 MB | 107 MB | 212 MB |
| Province: Alberta | 131 MB | 251 MB | 470 MB | 910 MB |
| State: Texas (published cut) | | | 437 MB | 987 MB |
| Province: British Columbia (published cut) | | | 940 MB | 1.9 GB |
| Saudi Arabia (published cut) | | | 212 MB | 417 MB |
| United States (published cut) | | | 8.0 GB | 17 GB |
| Canada (published cut) | | | 8.4 GB | 16 GB |

A basin-sized pack at full detail is ~20 MB, and a state at z14 is 100–450 MB. A whole country
is not a reasonable phone download; a state or province at z14 is.

## What was not measured

- **No country-sized or Texas/BC build.** The shared disk had ~12 GB free with a 6 GB floor.
  Basemap sizes are cut from the published planet build (checked against five regions within 6%).
  Routing, times, memory and disk are extrapolated from regions of 22–352 MB of PBF, up to 35×
  beyond the largest one measured for the US.
- **Valhalla memory for a large graph** is an assumption (see Memory).
- **A full end-to-end build with terrain** inside `mapthing-build`: the terrain stage was not in
  the image yet, so it was measured separately with the elevation worker's `mt-terrain` on 1°
  boxes, then multiplied by tile counts.
- **Real ocean polygons**: unavailable during the measurements (about ±5% on coastal basemaps).
- **Canada's land tile count** for terrain is a rough estimate (±30%).
- **HRDEM coverage share** across Canada: the full-resolution Canada figure assumes MRDEM (z12)
  everywhere. LiDAR areas built at z13 would add to it.
- **The server's own memory while serving**, and render latency under load.
