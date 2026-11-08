# Elevation sources

Which digital elevation models (DEMs) mapthing uses, why, and what they mean for accuracy.
Researched November 2026; URLs and licences were checked then. The code that encodes these
choices is `internal/build/terrain_sources.go`.

## Summary: what the terrain build uses

| Priority | Source id | Where | Native resolution | Vertical datum | Access used |
|---|---|---|---|---|---|
| 1 | `hrdem` | Canada, LiDAR-covered areas only | 2 m (read via overviews) | CGVD2013 | NRCan HRDEM mosaic COGs, found via the CCMEO STAC API |
| 2 | `3dep13` | United States | 1/3 arc-second (~10 m) | NAVD88 | USGS 3DEP 1°×1° COG tiles on S3 (`prd-tnm`) |
| 3 | `mrdem` | Canada (all of it) | 30 m | CGVD2013 | NRCan MRDEM-30 single national COG on S3 |
| 4 | `glo30` | Global fallback (incl. Mexico, ocean edges) | 1 arc-second (~30 m) | EGM2008 | Copernicus GLO-30 COG tiles on AWS open data |

Where sources overlap, the higher-priority source wins wherever it has data; its nodata falls
through to the next. Every source is a cloud-optimised GeoTIFF read with HTTP range requests, so
a build downloads only the window and overview level it needs. Nothing needs an account or key.

## Per area

### United States: USGS 3DEP

- **Seamless 1/3 arc-second DEM** (`3dep13`, default). Publisher: U.S. Geological Survey,
  3D Elevation Program. ~10 m (1/3″) in geographic NAD83 (EPSG:4269). Heights are NAVD88
  orthometric metres. It covers CONUS, Hawaii, Puerto Rico and parts of Alaska; the 1 arc-second
  product covers all of Alaska.
  - Format and access: 1°×1° tiles, each one a COG with overviews:
    `https://prd-tnm.s3.amazonaws.com/StagedProducts/Elevation/13/TIFF/current/n{LAT}w{LON}/USGS_13_n{LAT}w{LON}.tif`.
    `{LAT}{LON}` is the tile's **north-west** corner, so `n39w076` covers 38–39°N, 75–76°W.
    S3 listing works anonymously, and each object has an ETag. A tile is ~430 MB, which is why
    mapthing range-reads the window it needs rather than downloading tiles whole. The same
    tiles can also be found through the TNM Access API
    (`https://tnmaccess.nationalmap.gov/api/v1/products`).
  - Licence: public domain (U.S. Government work). Requested credit: "U.S. Geological Survey,
    3D Elevation Program". Automated bulk download is expressly supported (public S3 bucket).
  - Update cadence: continuous; tiles in `current/` are replaced as new LiDAR is integrated
    (the sampled tiles were last modified 2026-03). The ETag tells a refresh whether a tile changed.
- **1 m DEM (project-based)**: also LiDAR, as 10×10 km UTM tiles under
  `StagedProducts/Elevation/1m/Projects/…`. Coverage is patchy and a statewide area runs to
  terabytes. Not used by default: the seamless 1/3″ product is already resampled from this
  LiDAR, and our maxzoom (z13, ~16 m pixels at 31°N) cannot show the difference. A future
  "small area, z15" option could add it.
- **1 arc-second (~30 m)**: same layout under `Elevation/1/`, smaller tiles (~50 MB). Kept as a
  documented alternative; not used.

### Canada, national: NRCan CanElevation series

- **HRDEM mosaic** (`hrdem`). High Resolution DEM, LiDAR-derived, published by Natural
  Resources Canada (CCMEO). Mosaics at 1 m and 2 m; mapthing reads the **2 m DTM**, through
  its overviews, at our target resolution. Projection EPSG:3979 (Canada Atlas Lambert). Heights
  are CGVD2013. Coverage is wherever LiDAR has been collected and contributed (provincial,
  federal and municipal projects), so it is patchy, not a full grid.
  - Access: STAC API `https://datacube.services.geo.ca/stac/api`, collections `hrdem-mosaic-1m`
    and `hrdem-mosaic-2m`. Each item is a regional mosaic COG (tens of GB), for example
    `https://canelevation-dem.s3.ca-central-1.amazonaws.com/hrdem-mosaic-2m/3_4-mosaic-2m-dtm.tif`,
    with `extent` (GeoJSON) and `coverage` (GeoPackage) assets.
  - Licence: Open Government Licence – Canada 2.0. Attribution: "Contains information licensed
    under the Open Government Licence – Canada." Automated access is intended (public S3 bucket and STAC).
  - Update cadence: as new LiDAR projects arrive (the mosaics were last modified 2026-10).
- **MRDEM-30** (`mrdem`). Medium Resolution DEM, 30 m, released 2024. It replaces CDEM: one
  seamless national DTM, also published as a DSM, built from the best available sources
  (HRDEM resampled where LiDAR exists, otherwise CDEM, ArcticDEM and others). EPSG:3979,
  CGVD2013.
  - Access: one national COG,
    `https://canelevation-dem.s3.ca-central-1.amazonaws.com/mrdem-30/mrdem-30-dtm.tif` (84 GB;
    range-read only), also listed in STAC collection `mrdem-30` (asset `dtm`). A per-pixel
    `source` raster is published alongside it.
  - Licence: OGL – Canada 2.0, attribution as above.
- **CDEM** (Canadian DEM, ~20 m, 0.75″). Superseded by MRDEM and not used.

### Alberta

- **What is open and automatable:** the national products above (HRDEM where LiDAR exists, and
  MRDEM everywhere). Both are OGL-Canada.
- **Alberta Provincial 25 m DEM** (Government of Alberta; distributed via AltaLIS and GeoDiscover
  Alberta). Catalogue records list the Open Government Licence – Alberta for many provincial
  records, but downloading the 25 m DEM still goes through an account-based portal (historically
  AltaLIS, on a fee basis). I found no anonymous, scriptable endpoint. **Not used.** MRDEM-30
  already incorporates the provincial data at roughly the same resolution.
- **Alberta LiDAR** (provincial, 1 m) is licensed, not open. Not used.
- Older Alberta Geological Survey derived DEMs (for example 250 m, OGL-Alberta) are too coarse to matter.

### British Columbia

- **LidarBC** (GeoBC): 1 m DEM tiles from provincial LiDAR, released under the **Open Government
  Licence – British Columbia** (attribution: "Contains information licensed under the Open
  Government Licence – British Columbia"). Access is through the LidarBC portal and the
  provincial object store (~58,000 GeoTIFF tiles, no official STAC or tile index API that I
  found). **Not used directly in v1.** Much BC LiDAR also reaches the national HRDEM mosaic,
  which we do read. Whether every LidarBC project is in HRDEM has not been verified.
  Integrating LidarBC directly is a follow-up: build a tile index once, then treat it as
  another tiled source above `hrdem`.
- **CDED 1:20,000 gridded DEM, 25 m** (derived from the TRIM DEM). It is downloadable free as
  zipped CDED files by map letterblock under `https://pub.data.gov.bc.ca/datasets/175624/`. The
  data is old photogrammetry (1980s–90s) and MRDEM-30 supersedes it at comparable resolution,
  so **not used**.
- **TRIM DEM, USGS-format 25 m**: sold through the Base Map Online Store. Not open, not used.

### Rest of Canada

MRDEM-30 everywhere; HRDEM on top wherever LiDAR exists. No provincial sources.

### Fallback everywhere else: Copernicus GLO-30

- **Copernicus DEM GLO-30** (`glo30`). Publisher: ESA / Copernicus, produced by Airbus (from
  TanDEM-X). 1 arc-second (~30 m), EPSG:4326, heights EGM2008. **It is a DSM** (top of
  canopy and buildings), not bare earth. Expect it to read high in forest, so it is last in
  priority. It fills Mexico, the parts of a Texas bbox outside the US, and any source gaps.
  - Access: AWS open data, bucket `copernicus-dem-30m`, one COG per 1°×1° cell:
    `https://copernicus-dem-30m.s3.amazonaws.com/Copernicus_DSM_COG_10_N55_00_W119_00_DEM/Copernicus_DSM_COG_10_N55_00_W119_00_DEM.tif`.
    Cell names are **south-west** corners. Pure-ocean cells do not exist (HTTP 404/403 means
    no data).
  - Licence: free for any use, including commercial, with attribution: "© DLR e.V. 2010-2014 and
    © Airbus Defence and Space GmbH 2014-2018 provided under COPERNICUS by the European Union and
    ESA; all rights reserved". The GLO-30 public release excludes a small number of tiles
    (Armenia and Azerbaijan); our regions are unaffected.
  - Update cadence: static (the 2021 release).

## Vertical datums and the offset against Google

Every source is in an orthometric (geoid-based) datum, but not the same one:

| Source | Datum |
|---|---|
| 3DEP | NAVD88 |
| HRDEM, MRDEM | CGVD2013 |
| Copernicus GLO-30 | EGM2008 |
| Google Elevation API | not documented; behaves like a global geoid model (EGM96-like) |

mapthing **does not convert between datums**. It returns each source's heights as published,
and the response's `source` field says which datum applies. How big the differences are (geoid
grids from cdn.proj.org, sampled with PROJ 9.6 / GDAL 3.11):

| Point | NAVD88 → EGM96 | NAVD88 → EGM2008 |
|---|---|---|
| Delaware (39.0, −75.5) | +1.02 m | +0.90 m |
| Midland TX (31.9, −102.0) | +1.06 m | +0.86 m |
| Austin TX (30.3, −97.7) | +1.02 m | +1.01 m |
| Shreveport LA (32.3, −93.8) | +1.11 m | +0.95 m |

That is: a point at NAVD88 height 0 has an EGM96 height of about +1.0 m. **Expect mapthing's
US elevations to read roughly 1 m lower than a Google-like EGM96 answer.** This is the geoid
difference only; PROJ treats NAD83/WGS84 as a ballpark (null) transformation here, so the
~1 m frame offset between NAD83(2011) and WGS84 is not in these numbers.

In Canada, CGVD2013 is defined to match the global geoid closely. The CGG2013a and EGM96 geoid
heights differ by −0.15 m at Grande Prairie (55.17, −118.8), −0.15 m at Fort St. John
(56.25, −120.85) and +0.41 m at Calgary (51.05, −114.07). **Expect Canadian answers within about
±0.5 m of an EGM96-based answer.**

These offsets are below the vertical accuracy of 10–30 m DEMs in rough terrain (metres). They
are about the size of the accuracy of LiDAR-derived products on flat ground. A future option
could shift everything to EGM2008 at build time with `gdalwarp -t_srs EPSG:3857+3855` and the
PROJ geoid grids (~100 MB). That is why datums are recorded per source in the manifest.

## Resolution versus zoom

Terrain tiles are 256 px Terrarium tiles in Web Mercator. One pixel is
`156543.03 / 2^z × cos(lat)` metres on the ground:

| z | 30°N | 45°N | 55°N |
|---|---|---|---|
| 11 | 66 m | 54 m | 44 m |
| 12 | 33 m | 27 m | 22 m |
| 13 | 16.5 m | 13.5 m | 11.0 m |
| 14 | 8.3 m | 6.8 m | 5.5 m |

The build picks the maxzoom from the best source's resolution (z13 for 10 m sources such as
3DEP and for read-down LiDAR; z12 for 30 m-only areas), then drops a zoom while the estimated
archive exceeds the tile budget. See `internal/build/terrain.go`.

## Tile size

Terrarium stores elevation as a 24-bit integer in 1/256 m units, so at full precision the low
byte is sub-centimetre noise that PNG cannot compress. The build therefore rounds elevations
before encoding:

- **0.25 m at max zoom.** The worst-case rounding error, 12.5 cm, is below the vertical
  accuracy of any source used here.
- **Doubling for each zoom out** (0.5 m, 1 m, …, capped at 8 m). The step stays in proportion
  to the pixel size, so hillshading looks the same at every zoom.

The steps are powers of two, so the unused low bits are exactly zero: 0.1 m compresses
*worse* than 1/8 m. At 3× vertical exaggeration over Haynesville, a 0.25 m hillshade is
indistinguishable from full precision; 0.5 m shows faint texture on the flattest ground.

Archive bytes per max-zoom tile, lower zooms included (256 px PNG, z13 except Riyadh at z12),
measured on a 10-core x86 test machine:

| Area (source) | Full precision | 1/8 m | 1/4 m, doubling per zoom out (default) |
|---|---|---|---|
| Delaware coastal plain (3DEP) | ~105 KB (71 KB max-zoom tile alone) | 40.4 KB | 27.7 KB |
| Permian, 1° (3DEP) | – | 41.0 KB | 26.7 KB |
| Haynesville (3DEP) | 129.7 KB | – | 40.6 KB |
| Grande Prairie (HRDEM) | – | 57.6 KB | 35.7 KB |
| Hinton foothills (HRDEM/MRDEM) | 151.4 KB | 77.1 KB | 54.4 KB |
| BC Coast Mountains (MRDEM) | 170.2 KB | – | 73.9 KB |
| Riyadh (GLO-30 DSM) | – | 96.2 KB | 71.9 KB |

The planning figure is ~50 KB per max-zoom tile. After rounding, the values still match the
native source rasters within 0.15 m: Delaware, Haynesville and Grande Prairie checked with
`gdallocationinfo -r bilinear` on the original COGs.

**Lossless WebP was tried and rejected.** A pure-Go VP8L encoder was compared with Go's PNG
encoder (best compression) on the same rounded tiles. WebP saved at most 4% on plains and was
5–25% *larger* in hills, mountains and desert. MapLibre GL JS and Mapbox GL 1.13 hand raster-dem images
to the browser to decode, so both should accept WebP (not tested), but the saving does not
justify a second format.
