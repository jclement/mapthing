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

## Licence and data attribution

Code: [MIT](LICENSE).

Map data © [OpenStreetMap](https://www.openstreetmap.org/copyright) contributors, available
under the Open Database Licence (ODbL). Elevation: USGS 3D Elevation Program (public domain);
Natural Resources Canada (Open Government Licence – Canada); Province of British Columbia (Open
Government Licence – British Columbia); Government of Alberta (Open Government Licence –
Alberta); Copernicus DEM © DLR e.V. 2010–2014 and © Airbus Defence and Space GmbH 2014–2018,
provided under COPERNICUS by the European Union and ESA. The attribution shown in the map
follows the data actually built into your instance.
