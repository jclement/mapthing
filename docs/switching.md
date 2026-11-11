# Switching from Mapbox or Google

mapthing is meant to drop in where a commercial map service was: for the requests below you
change the host and the key, and nothing else. This page lists exactly which requests those
are, with the equivalent for the ones that are not, and what mapthing does not do at all.

Every example was run against a mapthing server while writing this page. `maps.example.com`
stands for your server (`MAPTHING_PUBLIC_URL`) and `mt_…` for an API key from the admin UI
(**API keys** → Create key).

## The key

One key works everywhere, sent whichever way your client already sends one:

| Your client sends | Works as is |
|---|---|
| `?access_token=…` (Mapbox) | yes |
| `?key=…` (Google, MapTiler) | yes |
| `X-Goog-Api-Key: …` (Google Routes) | yes |
| `?api_key=…` or `Authorization: …` (OpenRouteService) | yes |
| `Authorization: Bearer …` | yes |

Browser apps on another origin work with all of them: the endpoints answer CORS preflight
and expose the rate-limit headers. Restrict a key that ships in a browser to your site with
its **allowed origins**, and give it limits (`docs/architecture.md`, Rate limits); there is
no per-request price, so the limits are whatever you choose.

## Maps

### Mapbox GL JS or MapLibre GL JS (vector maps)

Replace the style URL:

```js
// before
new mapboxgl.Map({ container: "map", style: "mapbox://styles/mapbox/outdoors-v12", accessToken: "pk.…" });
// after
new maplibregl.Map({ container: "map", style: "https://maps.example.com/styles/outdoors.json?key=mt_…" });
```

The style carries the tile, font and sprite URLs with your key in them, so that one URL is
the whole change. Styles: `outdoors`, `outdoors-dark`, `satellite`, `overlay`, `overlay-dark`,
`field` (`GET /styles` lists them; `docs/themes.md` shows them).

- **Use MapLibre GL JS**, or Mapbox GL JS 1.13. Mapbox GL JS 2 and later refuses to start
  without a Mapbox token and is licensed for Mapbox's own services; MapLibre is the open
  continuation of 1.x with the same API (`mapboxgl.` → `maplibregl.`).
- The vector tiles use the **Protomaps v4** layer names, not Mapbox Streets. The styles above
  just work; a style you wrote yourself against Mapbox's layers (`road`, `poi_label`, …) has
  to be re-pointed at the Protomaps layers. That is the one part of a switch that is real work.
- Native apps (MapLibre Native for iOS and Android) take the same style URL.

### Raster tiles (Leaflet, OpenLayers, QGIS, anything that takes a tile URL)

The Mapbox Static Tiles URL works with only the host changed; the user name is ignored and the
style name is one of mapthing's:

```
before  https://api.mapbox.com/styles/v1/mapbox/outdoors-v12/tiles/512/{z}/{x}/{y}@2x?access_token=pk.…
after   https://maps.example.com/styles/v1/mapbox/outdoors/tiles/512/{z}/{x}/{y}@2x?access_token=mt_…
```

Without an extension it answers JPEG for `satellite` and PNG otherwise, as Mapbox does. The
shorter native form is `/styles/outdoors/512/{z}/{x}/{y}@2x.png?key=mt_…` (`256` or `512`,
`png`, `jpg` or `webp`).

### Terrain and hillshade

`https://maps.example.com/tiles/terrain/{z}/{x}/{y}.png?key=mt_…` is a `raster-dem` source in
**Terrarium** encoding. Coming from `mapbox.mapbox-terrain-dem-v1`, set `encoding: "terrarium"`
on the source; the layers using it stay as they are.

### Offline

There is no per-tile licence to count: `GET /tiles/basemap.pmtiles` is the whole basemap with
HTTP Range, and `GET /packs/extract.pmtiles?bbox=w,s,e,n&minzoom=&maxzoom=` (or
`format=mbtiles`) cuts a pack for an area.

## Routes

### Google Routes API (`computeRoutes`)

Host change only, same headers, same body:

```sh
# before: https://routes.googleapis.com/directions/v2:computeRoutes
curl -X POST https://maps.example.com/directions/v2:computeRoutes \
  -H 'Content-Type: application/json' \
  -H 'X-Goog-Api-Key: mt_…' \
  -H 'X-Goog-FieldMask: routes.distanceMeters,routes.duration' \
  -d '{"origin":{"location":{"latLng":{"latitude":31.8457,"longitude":-102.3676}}},
       "destination":{"location":{"latLng":{"latitude":31.9973,"longitude":-102.0779}}},
       "travelMode":"DRIVE"}'
# {"routes":[{"distanceMeters":36758,"duration":"1393s"}]}
```

Supported: origin, destination and intermediates as `location.latLng`; `travelMode: DRIVE`;
`polylineQuality`, `polylineEncoding`; `computeAlternativeRoutes`;
`routeModifiers.avoidTolls`, `avoidHighways`, `avoidFerries`; the field mask (header or
`$fields`). Returned: `distanceMeters`, `duration`, `staticDuration`, `polyline`, `legs[]`,
`viewport`, `routeLabels`. Extra: `?profile=truck` routes as a truck.

Not supported, and answered with a Google-style `INVALID_ARGUMENT` rather than a wrong
route: waypoints given as an `address` or `placeId` (there is no geocoder: send coordinates),
and travel modes other than `DRIVE`. There is no live traffic, so `duration` equals
`staticDuration` and `routingPreference` changes nothing.

### Google Directions API (legacy), Mapbox Directions, OSRM

Their URL shapes (`/maps/api/directions/json`, `/directions/v5/mapbox/driving/…`,
`/route/v1/driving/…`) are **not** served: they answer 404. Use one of these instead; all
take coordinates and return distance, duration, geometry and turn-by-turn steps:

| | |
|---|---|
| `POST /api/v1/route` | the native API: GeoJSON out, car or truck, `avoid_unpaved`, surface breakdown, alternates |
| `POST /directions/v2:computeRoutes` | if you would rather target Google's current API |
| `POST`/`GET /ors/v2/directions/{driving-car\|driving-hgv}` | OpenRouteService v2 shapes, with matrix, snap and isochrones beside it (`/ors/v2/matrix/…`, `/ors/v2/snap/…`, `/ors/v2/isochrones/…`): an ORS client library works with its base URL set to `https://maps.example.com/ors` |
| `GET`/`POST /api/v1/valhalla/{action}` | Valhalla's own API, for clients that already speak it |

### Google Distance Matrix, Mapbox Matrix and Isochrone

No compatible URL shape; the same answers come from `/ors/v2/matrix/{profile}`,
`/ors/v2/isochrones/{profile}`, or Valhalla's `sources_to_targets` and `isochrone` through the
pass-through.

## Elevation

### Google Elevation API

Host change only:

```sh
# before: https://maps.googleapis.com/maps/api/elevation/json
curl 'https://maps.example.com/maps/api/elevation/json?locations=31.8457,-102.3676|31.9686,-102.0779&key=mt_…'
# {"results":[{"elevation":883.25,"location":{"lat":31.8457,"lng":-102.3676},"resolution":16.2}, …],"status":"OK"}
```

`locations=` and `path=` with `samples=` both work, plain or as an `enc:` polyline. Errors
come back in Google's shape (`status`, `error_message`).

### Mapbox Tilequery (terrain)

Not served. `POST /api/v1/elevation` (`{"points":[[lon,lat],…]}`) answers the same question,
and `POST /api/v1/elevation/profile` returns a sampled profile with gain and loss.

## What mapthing does not do

So you can decide before you start rather than halfway through:

- **Geocoding, search, places, autocomplete.** No Google Geocoding or Places, no Mapbox
  Search. Keep your current provider for these, or run a geocoder (Nominatim, Photon, Pelias)
  beside mapthing.
- **The Google Maps JavaScript API** (`google.maps.Map`) and the Google Maps SDKs. They draw
  Google's map and cannot be pointed elsewhere; the map itself moves to MapLibre.
- **Static map images** (Mapbox Static Images, Google Static Maps): single tiles only.
- **Live traffic**, transit, walking and cycling routes. Car and truck only.
- **Street-level and proprietary imagery.** `satellite` uses open imagery (`docs/imagery.md`).
- **Coverage beyond the regions you install.** A request outside them finds no road or an
  empty tile; add the region on the Regions page.

## Checking a switch

```sh
curl -fsS https://maps.example.com/readyz                                         # ready?
curl -fsS 'https://maps.example.com/styles/outdoors.json?key=mt_…' | head -c 200  # the style
curl -fsS -o tile.png 'https://maps.example.com/styles/outdoors/512/3/1/3.png?key=mt_…'
```

Then watch **Overview** → Traffic while your app runs: requests appear by kind within a
minute, with failures and refusals counted apart, and **API keys** shows each key's use
against its limits.
