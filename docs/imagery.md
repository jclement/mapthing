# Satellite imagery

`/tiles/imagery/{z}/{x}/{y}.jpg` is a caching proxy for an open imagery service. The choice of
upstream per tile is in `internal/tiles/imagery_sources.go` (`ImageryURL`). Every style has a
raster layer with id `satellite` on this source (hidden except in the `satellite` style).

## Default chain

| Tiles | Source | Licence | Max zoom |
|---|---|---|---|
| z0–8 everywhere; z9–16 inside the US (lower 48, Alaska, Hawaii, Puerto Rico) | USGS The National Map, `USGSImageryOnly` MapServer (`basemap.nationalmap.gov/arcgis/rest/services/USGSImageryOnly/MapServer/tile/{z}/{y}/{x}`). NAIP-derived 0.6–1 m in the lower 48. | US federal government work: public domain. Attribution "USGS The National Map" requested. | 16 (z17+ returns 404) |
| z9–14 outside the US (Canada, Mexico, elsewhere) | EOX **Sentinel-2 cloudless 2016** (`tiles.maps.eox.at/wmts/1.0.0/s2cloudless_3857/default/g/{z}/{y}/{x}.jpg`), 10 m | **CC BY 4.0**. Attribution: "Sentinel-2 cloudless - https://s2maps.eu by EOX IT Services GmbH (Contains modified Copernicus Sentinel data 2016)" | 14 |

Above a source's max zoom the proxy has nothing to fetch (the styles set the imagery source's
`maxzoom` to 16, so GL clients overzoom instead of asking). Outside the US z15–16 has no tile;
core may serve an upscaled z14 parent there.

USGS answers low zooms for the whole world, so z0–8 come from one source and look uniform.
Above z8 the tile centre decides (coarse US outline; a tile on the border may come from the
"wrong" side and show blank or low-resolution imagery).

## Sources considered

- **USGS The National Map imagery**: public domain, US only at useful zooms, no key, tile cache
  to z16. Used. For heavy use, the USGS asks clients to cache; mapthing does (bounded LRU in
  `/data/imagery-cache`).
- **USGS NAIP ImageServer** (`imagery.nationalmap.gov/.../USGSNAIPImagery/ImageServer`): same
  data at native 0.6 m, but `exportImage` only (no tile cache), slower per tile. A possible later
  upgrade for z17–18 in the US.
- **EOX Sentinel-2 cloudless**: the 2016 mosaic is CC BY 4.0 and is used. Later mosaics (2018
  onward) are **CC BY-NC-SA 4.0**: non-commercial only, so not used by default. Set
  `MAPTHING_IMAGERY_URL` to a later year only if your use is non-commercial (or you hold an EOX
  commercial licence). *Verify:* the per-year licence split is taken from EOX's published
  terms; the s2maps.eu licence pages render client-side and were not re-read when this was
  written.
- **Canada**: NRCan publishes no open, tiled national orthophoto service. BC's open WMS
  orthophoto layers are partial and WMS-only; Alberta's provincial orthophotos are licensed, not
  open. So Canada gets Sentinel-2 cloudless (10 m) unless the operator configures something
  better.
- **Esri World Imagery**: **not usable.** Its terms require an ArcGIS licence and forbid
  caching/proxying tiles for redistribution through another service. Same for Google, Bing and
  Mapbox satellite.

## Configuration

| Env | Meaning |
|---|---|
| `MAPTHING_IMAGERY_URL` | replaces the whole chain with one template: `{z}`, `{x}`, `{y}`, `{-y}` (TMS row), `{q}` (quadkey) |
| `MAPTHING_IMAGERY_ATTRIBUTION` | attribution for that source (shown in styles and the UI) |
| `MAPTHING_IMAGERY_MAXZOOM` | highest zoom to request from it (default 19) |

The operator is responsible for the terms of any source they configure.
