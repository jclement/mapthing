// Generates mapthing's GL styles (outdoors, outdoors-dark, satellite) from the Protomaps basemap
// layers (@protomaps/basemaps, BSD-3-Clause; see LICENSE-protomaps-basemaps) with changes for
// field use: unpaved/track/service roads readable from z12, earlier minor-road labels, a
// hillshade from the terrain source, and an imagery layer with id "satellite" in every style.
//
// Output must load in mapbox-gl 1.13.1 and current MapLibre GL JS; validate.mjs checks both specs
// plus a list of expressions newer than 1.13. Run `npm run build && npm run validate`.
//
// Placeholders filled in by mapthing when it serves a style: {{BASE_URL}} (scheme://host, no
// trailing slash) and {{KEY_QS}} ("?key=mt_..." or ""). Sprite URLs carry no {{KEY_QS}}: the
// server appends the query to the sprite URL itself (GL clients add .json/.png after it).
import { writeFileSync } from "node:fs";
import { layers, namedFlavor } from "@protomaps/basemaps";

const SRC = "protomaps";

const ATTR_OSM =
  '<a href="https://www.openstreetmap.org/copyright" target="_blank">© OpenStreetMap contributors</a>';
const ATTR_PROTOMAPS = '<a href="https://protomaps.com" target="_blank">Protomaps</a>';
const ATTR_TERRAIN = "Elevation: USGS 3DEP, NRCan, Copernicus DEM © DLR/Airbus";
// Default imagery chain (see docs/imagery.md). mapthing may replace this attribution at serve time
// when MAPTHING_IMAGERY_URL points somewhere else (tiles.ImageryAttribution()).
const ATTR_IMAGERY =
  'Imagery: <a href="https://www.usgs.gov/programs/national-geospatial-program/national-map" target="_blank">USGS The National Map</a>; ' +
  '<a href="https://s2maps.eu" target="_blank">Sentinel-2 cloudless by EOX IT Services GmbH</a> (contains modified Copernicus Sentinel data)';

const sources = () => ({
  [SRC]: {
    type: "vector",
    tiles: ["{{BASE_URL}}/tiles/basemap/{z}/{x}/{y}.mvt{{KEY_QS}}"],
    minzoom: 0,
    maxzoom: 15,
    attribution: `${ATTR_PROTOMAPS} ${ATTR_OSM}`,
  },
  terrain: {
    type: "raster-dem",
    encoding: "terrarium",
    tiles: ["{{BASE_URL}}/tiles/terrain/{z}/{x}/{y}.png{{KEY_QS}}"],
    tileSize: 256,
    minzoom: 0,
    maxzoom: 12,
    attribution: ATTR_TERRAIN,
  },
  imagery: {
    type: "raster",
    tiles: ["{{BASE_URL}}/tiles/imagery/{z}/{x}/{y}.jpg{{KEY_QS}}"],
    tileSize: 256,
    minzoom: 0,
    maxzoom: 18,
    attribution: ATTR_IMAGERY,
  },
});

// ---------------------------------------------------------------------------------------------
// Label simplification. Protomaps' multi-script label expressions are ~3 KB each and reference a
// Devanagari font stack. mapthing targets North America: English name, else the local name.
const NAME = ["coalesce", ["get", "name:en"], ["get", "name"]];
const NAME_OR_REF = ["coalesce", ["get", "name:en"], ["get", "name"], ["get", "ref"]];

function mentionsName(v) {
  return JSON.stringify(v).includes('"name');
}

function simplifyLabels(layer) {
  const lay = layer.layout;
  if (!lay || !lay["text-field"]) return;
  const tf = lay["text-field"];
  if (layer.id === "places_region") {
    // abbreviations ("TX", "AB") at low zoom, full name from z6
    lay["text-field"] = ["step", ["zoom"], ["coalesce", ["get", "ref:en"], ["get", "ref"], NAME], 6, NAME];
  } else if (layer.id.startsWith("roads_labels")) {
    lay["text-field"] = NAME_OR_REF;
  } else if (mentionsName(tf) && JSON.stringify(tf).length > 60) {
    lay["text-field"] = NAME;
  }
}

// Replace every font list with a single-font stack we ship glyphs for.
const FONTS = new Set(["Noto Sans Regular", "Noto Sans Medium", "Noto Sans Italic"]);
function fixFonts(v) {
  if (Array.isArray(v)) {
    if (v.length && v.every((x) => typeof x === "string") && v.some((x) => x.startsWith("Noto Sans"))) {
      const keep = v.filter((x) => FONTS.has(x));
      return keep.length ? [keep[0]] : ["Noto Sans Regular"];
    }
    return v.map(fixFonts);
  }
  if (v && typeof v === "object") {
    const o = {};
    for (const [k, x] of Object.entries(v)) o[k] = fixFonts(x);
    return o;
  }
  return v;
}

// ---------------------------------------------------------------------------------------------
// Field-use palettes on top of the Protomaps flavours.
const PALETTE = {
  light: {
    minor_low: "#ffffff", // minor road fill at z12 (on the warm earth colour)
    minor_casing: "#bdb6ab",
    service: "#fbfaf7",
    service_casing: "#c9c2b6",
    track: "#8a5a2b", // brown dashed: tracks / lease roads
    track_casing: "rgba(255,255,255,0.75)",
    unpaved: "#a86f3a",
    road_label: "#5e5850",
    road_label_halo: "#ffffff",
    hillshade_shadow: "rgba(70, 60, 50, 0.32)",
    hillshade_highlight: "rgba(255, 255, 255, 0.25)",
    hillshade_accent: "rgba(70, 60, 50, 0.12)",
  },
  dark: {
    minor_low: "#4a4a4a",
    minor_casing: "#2a2a2a",
    service: "#3e3e3e",
    service_casing: "#262626",
    track: "#c8955e",
    track_casing: "rgba(20,20,20,0.6)",
    unpaved: "#c8955e",
    road_label: "#bdbdbd",
    road_label_halo: "#1f1f1f",
    hillshade_shadow: "rgba(0, 0, 0, 0.45)",
    hillshade_highlight: "rgba(255, 255, 255, 0.06)",
    hillshade_accent: "rgba(0, 0, 0, 0.15)",
  },
};

// OSM surface values treated as unpaved. Only effective when the tile profile keeps `surface`
// (the stock Protomaps v4 profile does not; see NOTES in docs/styles.md).
const UNPAVED = [
  "unpaved", "gravel", "fine_gravel", "compacted", "dirt", "earth", "ground", "grass", "sand",
  "mud", "pebblestone", "rock", "shells", "woodchips", "clay",
];

const NOT_BRIDGE_TUNNEL = [["!has", "is_tunnel"], ["!has", "is_bridge"]];
const ex = (base) => ["interpolate", ["exponential", 1.6], ["zoom"], ...base];

function fieldRoadLayers(p) {
  return {
    // drawn where Protomaps draws casings
    casings: [
      {
        id: "roads_track_casing",
        type: "line",
        source: SRC,
        "source-layer": "roads",
        minzoom: 12,
        filter: ["all", ...NOT_BRIDGE_TUNNEL, ["==", "kind", "path"], ["==", "kind_detail", "track"]],
        layout: { "line-cap": "round", "line-join": "round" },
        paint: { "line-color": p.track_casing, "line-width": ex([12, 2, 14, 3, 18, 8]) },
      },
    ],
    // drawn after Protomaps' minor roads
    fills: [
      {
        id: "roads_track",
        type: "line",
        source: SRC,
        "source-layer": "roads",
        minzoom: 12,
        filter: ["all", ["!has", "is_tunnel"], ["==", "kind", "path"], ["==", "kind_detail", "track"]],
        layout: { "line-join": "round" },
        paint: {
          "line-color": p.track,
          "line-width": ex([12, 0.9, 14, 1.4, 18, 4]),
          "line-dasharray": [3, 1.5],
        },
      },
      {
        id: "roads_unpaved",
        type: "line",
        source: SRC,
        "source-layer": "roads",
        minzoom: 11,
        filter: [
          "all",
          ["!has", "is_tunnel"],
          ["in", "kind", "minor_road", "major_road"],
          ["in", "surface", ...UNPAVED],
        ],
        paint: {
          "line-color": p.unpaved,
          "line-width": ex([11, 0.6, 14, 1, 18, 3]),
          "line-dasharray": [2, 2],
        },
      },
    ],
  };
}

// Adjust stock Protomaps road layers for field readability.
function tuneRoads(layer, p) {
  const paint = layer.paint || {};
  switch (layer.id) {
    case "roads_other":
      // tracks get their own layer
      layer.filter = ["all", ...NOT_BRIDGE_TUNNEL, ["in", "kind", "other", "path"], ["!in", "kind_detail", "pier", "track"]];
      break;
    case "roads_bridges_other":
    case "roads_tunnels_other":
      layer.filter = [...layer.filter, ["!=", "kind_detail", "track"]];
      break;
    case "roads_minor_casing":
      paint["line-color"] = p.minor_casing;
      paint["line-gap-width"] = ex([11, 0, 12, 1, 15, 2.5, 18, 11]);
      paint["line-width"] = ex([11, 0, 12, 0.6, 14, 0.8, 18, 1.2]);
      break;
    case "roads_minor":
      paint["line-color"] = p.minor_low;
      paint["line-width"] = ex([11, 0, 12, 1, 15, 2.5, 18, 11]);
      break;
    case "roads_minor_service_casing":
      paint["line-color"] = p.service_casing;
      paint["line-gap-width"] = ex([13, 0.6, 15, 1.5, 18, 8]);
      paint["line-width"] = ex([13, 0.5, 18, 1]);
      break;
    case "roads_minor_service":
      paint["line-color"] = p.service;
      paint["line-width"] = ex([13, 0.6, 15, 1.5, 18, 8]);
      break;
    case "roads_labels_minor":
      layer.minzoom = 13;
      paint["text-color"] = p.road_label;
      paint["text-halo-color"] = p.road_label_halo;
      paint["text-halo-width"] = 1.5;
      break;
    case "roads_labels_major":
      paint["text-halo-width"] = 1.5;
      break;
  }
  layer.paint = paint;
}

function hillshade(p) {
  return {
    id: "hillshade",
    type: "hillshade",
    source: "terrain",
    minzoom: 0,
    paint: {
      "hillshade-exaggeration": ["interpolate", ["linear"], ["zoom"], 4, 0.45, 10, 0.35, 15, 0.25],
      "hillshade-shadow-color": p.hillshade_shadow,
      "hillshade-highlight-color": p.hillshade_highlight,
      "hillshade-accent-color": p.hillshade_accent,
      "hillshade-illumination-direction": 335,
    },
  };
}

const satelliteLayer = (visible) => ({
  id: "satellite",
  type: "raster",
  source: "imagery",
  layout: { visibility: visible ? "visible" : "none" },
  paint: { "raster-fade-duration": 100 },
});

function insertAfter(list, id, items) {
  const i = list.findIndex((l) => l.id === id);
  if (i < 0) throw new Error(`no layer ${id}`);
  list.splice(i + 1, 0, ...items);
}
function insertBefore(list, id, items) {
  const i = list.findIndex((l) => l.id === id);
  if (i < 0) throw new Error(`no layer ${id}`);
  list.splice(i, 0, ...items);
}

function baseStyle(name, title) {
  return {
    version: 8,
    name: title,
    metadata: { "mapthing:style": name, "mapthing:generator": "assets/styles/build.mjs" },
    center: [-102.08, 31.85],
    zoom: 9,
    glyphs: "{{BASE_URL}}/fonts/{fontstack}/{range}.pbf{{KEY_QS}}",
    sources: sources(),
    layers: [],
  };
}

function prep(flavorName) {
  const flavor = namedFlavor(flavorName);
  const ls = fixFonts(layers(SRC, flavor, { lang: "en" }));
  ls.forEach(simplifyLabels);
  return ls;
}

function outdoors(flavorName, name, title, spriteName) {
  const p = PALETTE[flavorName];
  const ls = prep(flavorName);
  ls.forEach((l) => tuneRoads(l, p));
  const road = fieldRoadLayers(p);
  // hillshade over land cover / land use, under water
  insertBefore(ls, "water", [hillshade(p)]);
  // imagery (hidden) above all fills, under roads and buildings: toggling it gives a hybrid map
  insertBefore(ls, "roads_tunnels_other_casing", [satelliteLayer(false)]);
  insertBefore(ls, "roads_minor_service_casing", road.casings);
  insertAfter(ls, "roads_minor", road.fills);
  const s = baseStyle(name, title);
  s.sprite = `{{BASE_URL}}/sprites/${spriteName}`;
  s.layers = ls;
  return s;
}

// Satellite: imagery with roads, boundaries and labels from the light flavour, restyled for
// contrast on photos. Fills (earth, water, land use, buildings) are dropped.
function satellite() {
  const p = PALETTE.light;
  const ls = prep("light");
  ls.forEach((l) => tuneRoads(l, p));
  const road = fieldRoadLayers({ ...p, track: "#ffd27f", track_casing: "rgba(0,0,0,0.45)", unpaved: "#ffd27f" });
  insertBefore(ls, "roads_minor_service_casing", road.casings);
  insertAfter(ls, "roads_minor", road.fills);
  const keep = ls.filter((l) => {
    if (l.type === "fill" || l.type === "background" || l.type === "fill-extrusion") return false;
    if (l.id === "water_stream" || l.id === "water_river") return false;
    return true;
  });
  for (const l of keep) {
    const paint = l.paint || (l.paint = {});
    if (l.type === "symbol" && l.layout && l.layout["text-field"]) {
      if (l.id === "roads_shields") continue; // dark text on the shield icon stays
      paint["text-color"] = "#ffffff";
      paint["text-halo-color"] = "rgba(0,0,0,0.85)";
      paint["text-halo-width"] = 1.5;
    } else if (l.type === "line" && l.id.startsWith("roads_") && !l.id.includes("track") && l.id !== "roads_unpaved") {
      // thin out the road network so the photo shows through
      paint["line-opacity"] = l.id.includes("casing") ? 0.5 : 0.75;
    } else if (l.type === "line" && l.id.startsWith("boundaries")) {
      paint["line-color"] = "#f2f2f2";
      paint["line-opacity"] = 0.8;
    }
  }
  const s = baseStyle("satellite", "mapthing satellite");
  s.sprite = "{{BASE_URL}}/sprites/outdoors";
  s.layers = [
    { id: "background", type: "background", paint: { "background-color": "#1d2a1f" } },
    satelliteLayer(true),
    ...keep,
  ];
  return s;
}

const out = {
  outdoors: outdoors("light", "outdoors", "mapthing outdoors", "outdoors"),
  "outdoors-dark": outdoors("dark", "outdoors-dark", "mapthing outdoors dark", "outdoors-dark"),
  satellite: satellite(),
};

for (const [name, style] of Object.entries(out)) {
  const ids = new Set();
  for (const l of style.layers) {
    if (ids.has(l.id)) throw new Error(`${name}: duplicate layer id ${l.id}`);
    ids.add(l.id);
  }
  if (!ids.has("satellite")) throw new Error(`${name}: no satellite layer`);
  writeFileSync(new URL(`./${name}.json`, import.meta.url), JSON.stringify(style, null, 1) + "\n");
  console.log(`${name}.json: ${style.layers.length} layers`);
}
