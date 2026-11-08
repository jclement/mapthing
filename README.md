# mapthing

A self-hosted map service in one container: vector and raster basemap tiles, offline packs,
routing, and elevation profiles, with an admin web UI for picking regions, scheduling refreshes
and issuing API keys.

> **Status: under active construction.** The design contract is in
> [docs/architecture.md](docs/architecture.md).

```sh
docker run -d --name mapthing -p 8080:8080 -v mapthing-data:/data \
  -e ADMIN_USER=admin -e ADMIN_PASSWORD='change-me' \
  ghcr.io/jclement/mapthing:latest
```

Open http://localhost:8080, sign in, add a region (a US state, a Canadian province, or a box you
draw), and build it. When the build finishes the map, routing and elevation for that region are
live.

## What it serves

- **Vector tiles** (Protomaps v4 schema) as z/x/y MVT and as a PMTiles archive with HTTP Range
  support, plus MapLibre / Mapbox GL styles (light, dark, satellite).
- **Raster tiles** rendered from those styles, 256 or 512 px, @2x.
- **Offline packs**: a PMTiles or MBTiles extract for any bounding box.
- **Routing** (Valhalla): car and truck, with sensible handling of gravel and unpaved roads.
- **Elevation** at points and along paths, from the best open DEM for each area (USGS 3DEP in
  the US, NRCan and provincial data in Canada, Copernicus elsewhere).
- **Compatibility endpoints**, so existing clients can switch by changing a URL: Google
  Elevation API, Google Routes `computeRoutes`, and OpenRouteService v2.

## Sizing

Whole-country builds, from measurements of regional builds plus published data
([docs/sizing.md](docs/sizing.md) has the method, every figure's basis and the per-component tables):

| | United States | Canada | Saudi Arabia |
|---|---|---|---|
| OSM extract | 12.3 GB | 6.5 GB | 0.12 GB |
| Vector basemap (z0–15) | 17 GB | 16 GB | 0.4 GB |
| One generation (basemap + routing + terrain) | ~55 GB | ~68 GB | ~4.3 GB |
| Disk, steady / peak during a build | ~140 / ~265 GB | ~200 / ~300 GB | ~11 / ~16 GB |
| Full build from scratch (10 vCPU) | ~8 h | ~11 h | ~1 h |
| Machine, comfortable | 16 cores, 64 GB, 500 GB NVMe | 16 cores, 32 GB, 500 GB NVMe | 8 cores, 16 GB, 50 GB |

Terrain (elevation) dominates both build time and disk. An offline pack for a ~100×150 km basin
at full detail is ~20 MB; a state or province at z14 is 100–470 MB.

## Stack

- **Go** for the service itself: one static binary that serves the API, tiles and admin UI,
  supervises the other processes and runs the build pipeline.
- **Valhalla** (C++) for routing. It has truck and car costing, handles unpaved and track roads
  with penalties rather than exclusion, and includes matrix, isochrone and snapping.
- **Planetiler** with the Protomaps profile for vector tiles, **GDAL** for the elevation build,
  and **osmium** for OSM extracts. These are bundled tools, each the best at its job.
- **MapLibre Native** for the raster renderer, and a React + MapLibre GL JS admin UI that is
  compiled into the Go binary.

## Licence and data attribution

Code: [MIT](LICENSE).

Map data © [OpenStreetMap](https://www.openstreetmap.org/copyright) contributors, available
under the Open Database Licence (ODbL). Elevation: USGS 3D Elevation Program (public domain);
Natural Resources Canada (Open Government Licence – Canada); Province of British Columbia (Open
Government Licence – British Columbia); Government of Alberta (Open Government Licence –
Alberta); Copernicus DEM © DLR e.V. 2010–2014 and © Airbus Defence and Space GmbH 2014–2018,
provided under COPERNICUS by the European Union and ESA. The attribution shown in the map
follows the data actually built into your instance.
