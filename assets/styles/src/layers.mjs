// The layer builder: buildStyle(theme) turns one theme object (src/themes/*.mjs) into a complete
// GL style v8. Everything a theme can vary is in the theme object; see src/themes/README.md for
// the fields and src/themes/outdoors.mjs for a fully commented example.
//
// Base layers come from @protomaps/basemaps (BSD-3-Clause, see ../LICENSE-protomaps-basemaps.md)
// for the Protomaps v4 tile schema. On top of them this file adds: field road layers (tracks,
// unpaved overlay), tuned minor/service road widths and casings, simplified English labels with
// fonts we ship, a hillshade from the terrain source and a raster layer with id "satellite" from
// the imagery source (present in every theme; visible or not per theme).
//
// Output must load in mapbox-gl 1.13.1 and current MapLibre GL JS: validate.mjs enforces it.
//
// Placeholders filled in by mapthing when it serves a style: {{BASE_URL}} (scheme://host, no
// trailing slash) and {{KEY_QS}} ("?key=mt_..." or ""). Sprite URLs carry no {{KEY_QS}}: the
// server appends the query to the sprite URL itself.
import { layers as protomapsLayers, namedFlavor } from "@protomaps/basemaps";

export const SRC = "protomaps";

const ATTR_OSM =
  '<a href="https://www.openstreetmap.org/copyright" target="_blank">© OpenStreetMap contributors</a>';
const ATTR_PROTOMAPS = '<a href="https://protomaps.com" target="_blank">Protomaps</a>';
const ATTR_TERRAIN = "Elevation: USGS 3DEP, NRCan, Copernicus DEM © DLR/Airbus";
// Default imagery chain (see docs/imagery.md). mapthing may replace this attribution at serve
// time when MAPTHING_IMAGERY_URL points somewhere else (tiles.ImageryAttribution()).
export const ATTR_IMAGERY =
  'Imagery: <a href="https://www.usgs.gov/programs/national-geospatial-program/national-map" target="_blank">USGS The National Map</a>; ' +
  '<a href="https://s2maps.eu" target="_blank">Sentinel-2 cloudless</a> by EOX IT Services GmbH (contains modified Copernicus Sentinel data 2016, CC BY 4.0)';

export const FONTS = ["Noto Sans Regular", "Noto Sans Medium", "Noto Sans Italic"];

// OSM surface values treated as unpaved. Only effective when the tile profile keeps `surface`
// (the stock Protomaps v4 profile does not; see docs/styles.md).
export const UNPAVED = [
  "unpaved", "gravel", "fine_gravel", "compacted", "dirt", "earth", "ground", "grass", "sand",
  "mud", "pebblestone", "rock", "shells", "woodchips", "clay",
];

function sources(theme) {
  return {
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
      maxzoom: theme.terrainMaxzoom ?? 12,
      attribution: ATTR_TERRAIN,
    },
    imagery: {
      type: "raster",
      tiles: ["{{BASE_URL}}/tiles/imagery/{z}/{x}/{y}.jpg{{KEY_QS}}"],
      tileSize: 256,
      minzoom: 0,
      maxzoom: theme.imageryMaxzoom ?? 16,
      attribution: ATTR_IMAGERY,
    },
  };
}

// ---------------------------------------------------------------------------------------------
// Helpers.

const NAME = ["coalesce", ["get", "name:en"], ["get", "name"]];
const NAME_OR_REF = ["coalesce", ["get", "name:en"], ["get", "name"], ["get", "ref"]];
const NOT_BRIDGE_TUNNEL = [["!has", "is_tunnel"], ["!has", "is_bridge"]];

/** exponential(1.6) zoom interpolation over [zoom, value, zoom, value, ...] */
export const ex = (stops) => ["interpolate", ["exponential", 1.6], ["zoom"], ...stops];
/** linear zoom interpolation over [zoom, value, ...] */
export const lin = (stops) => ["interpolate", ["linear"], ["zoom"], ...stops];

function deepMap(v, fn) {
  const r = fn(v);
  if (r !== undefined) return r;
  if (Array.isArray(v)) return v.map((x) => deepMap(x, fn));
  if (v && typeof v === "object") return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, deepMap(x, fn)]));
  return v;
}

// Replace every font list with a single-font stack we ship glyphs for.
function fixFonts(v) {
  return deepMap(v, (x) => {
    if (Array.isArray(x) && x.length && x.every((s) => typeof s === "string") && x.some((s) => s.startsWith("Noto Sans"))) {
      const keep = x.filter((s) => FONTS.includes(s));
      return keep.length ? [keep[0]] : ["Noto Sans Regular"];
    }
    return undefined;
  });
}

// Protomaps' multi-script label expressions are ~3 KB each and use a Devanagari font stack.
// mapthing targets North America: English name, else the local name.
function simplifyLabels(layer) {
  const lay = layer.layout;
  if (!lay || !lay["text-field"]) return;
  const tf = JSON.stringify(lay["text-field"]);
  if (layer.id === "places_region") {
    lay["text-field"] = ["step", ["zoom"], ["coalesce", ["get", "ref:en"], ["get", "ref"], NAME], 6, NAME];
  } else if (layer.id.startsWith("roads_labels")) {
    lay["text-field"] = NAME_OR_REF;
  } else if (tf.includes('"name') && tf.length > 60) {
    lay["text-field"] = NAME;
  }
}

function insertAt(list, id, items, after) {
  const i = list.findIndex((l) => l.id === id);
  if (i < 0) throw new Error(`no layer ${id}`);
  list.splice(after ? i + 1 : i, 0, ...items);
}

// ---------------------------------------------------------------------------------------------
// Theme-driven pieces.

function fieldRoadLayers(t) {
  const r = t.roads;
  const c = t.colors;
  return {
    casings: [
      {
        id: "roads_track_casing",
        type: "line",
        source: SRC,
        "source-layer": "roads",
        minzoom: r.trackMinzoom,
        filter: ["all", ...NOT_BRIDGE_TUNNEL, ["==", "kind", "path"], ["==", "kind_detail", "track"]],
        layout: { "line-cap": "round", "line-join": "round" },
        paint: { "line-color": c.track_casing, "line-width": ex(r.trackCasingWidth) },
      },
    ],
    fills: [
      {
        id: "roads_track",
        type: "line",
        source: SRC,
        "source-layer": "roads",
        minzoom: r.trackMinzoom,
        filter: ["all", ["!has", "is_tunnel"], ["==", "kind", "path"], ["==", "kind_detail", "track"]],
        layout: { "line-join": "round" },
        paint: { "line-color": c.track, "line-width": ex(r.trackWidth), "line-dasharray": r.trackDash },
      },
      {
        id: "roads_unpaved",
        type: "line",
        source: SRC,
        "source-layer": "roads",
        minzoom: 11,
        filter: ["all", ["!has", "is_tunnel"], ["in", "kind", "minor_road", "major_road"], ["in", "surface", ...UNPAVED]],
        paint: { "line-color": c.unpaved, "line-width": ex(r.unpavedWidth), "line-dasharray": r.unpavedDash },
      },
    ],
  };
}

function tuneLayer(layer, t) {
  const r = t.roads;
  const c = t.colors;
  const paint = (layer.paint ||= {});
  switch (layer.id) {
    case "roads_other":
      layer.filter = ["all", ...NOT_BRIDGE_TUNNEL, ["in", "kind", "other", "path"], ["!in", "kind_detail", "pier", "track"]];
      break;
    case "roads_bridges_other":
    case "roads_tunnels_other":
      layer.filter = [...layer.filter, ["!=", "kind_detail", "track"]];
      break;
    case "roads_minor_casing":
      paint["line-color"] = c.minor_casing;
      paint["line-gap-width"] = ex(r.minorWidth);
      paint["line-width"] = ex(r.minorCasingWidth);
      break;
    case "roads_minor":
      paint["line-color"] = c.minor;
      paint["line-width"] = ex(r.minorWidth);
      break;
    case "roads_minor_service_casing":
      paint["line-color"] = c.service_casing;
      paint["line-gap-width"] = ex(r.serviceWidth);
      paint["line-width"] = ex(r.serviceCasingWidth);
      break;
    case "roads_minor_service":
      paint["line-color"] = c.service;
      paint["line-width"] = ex(r.serviceWidth);
      break;
    case "roads_labels_minor":
      layer.minzoom = t.labels.minorRoadMinzoom;
      paint["text-color"] = c.road_label;
      paint["text-halo-color"] = c.road_label_halo;
      paint["text-halo-width"] = t.labels.haloWidth;
      break;
    case "roads_labels_major":
      paint["text-halo-width"] = t.labels.haloWidth;
      break;
    case "pois":
      if (t.labels.pois === false) layer.layout = { ...(layer.layout || {}), visibility: "none" };
      break;
  }
}

function hillshadeLayer(t) {
  const h = t.hillshade;
  return {
    id: "hillshade",
    type: "hillshade",
    source: "terrain",
    layout: h.enabled === false ? { visibility: "none" } : {},
    paint: {
      "hillshade-exaggeration": lin(h.exaggeration),
      "hillshade-shadow-color": h.shadow,
      "hillshade-highlight-color": h.highlight,
      "hillshade-accent-color": h.accent,
      "hillshade-illumination-direction": 335,
    },
  };
}

function satelliteLayer(t) {
  return {
    id: "satellite",
    type: "raster",
    source: "imagery",
    layout: { visibility: t.satellite.visible ? "visible" : "none" },
    paint: { "raster-fade-duration": 100, ...(t.satellite.paint || {}) },
  };
}

// Restyle for drawing over photos: no fills, translucent roads, light labels with dark halos.
function overImagery(ls, t) {
  const o = t.overImagery;
  const keep = ls.filter((l) => {
    if (["fill", "background", "fill-extrusion"].includes(l.type)) return false;
    return !(l.id === "water_stream" || l.id === "water_river");
  });
  for (const l of keep) {
    const paint = (l.paint ||= {});
    if (l.type === "symbol" && l.layout?.["text-field"]) {
      if (l.id === "roads_shields") continue; // dark text on the shield icon stays
      paint["text-color"] = o.labelColor;
      paint["text-halo-color"] = o.labelHalo;
      paint["text-halo-width"] = t.labels.haloWidth;
    } else if (l.type === "line" && l.id.startsWith("roads_") && !l.id.includes("track") && l.id !== "roads_unpaved") {
      paint["line-opacity"] = l.id.includes("casing") ? o.roadCasingOpacity : o.roadOpacity;
    } else if (l.type === "line" && l.id.startsWith("boundaries")) {
      paint["line-color"] = o.boundaryColor;
      paint["line-opacity"] = 0.8;
    }
  }
  return keep;
}

// ---------------------------------------------------------------------------------------------

/**
 * Build a full GL style v8 from a theme object.
 * @param {object} t theme (see src/themes/README.md)
 */
export function buildStyle(t) {
  const flavor = { ...namedFlavor(t.flavor), ...(t.flavorOverrides || {}) };
  let ls = fixFonts(protomapsLayers(SRC, flavor, { lang: "en" }));
  ls.forEach(simplifyLabels);
  ls.forEach((l) => tuneLayer(l, t));
  const road = fieldRoadLayers(t);
  insertAt(ls, "roads_minor_service_casing", road.casings, false);
  insertAt(ls, "roads_minor", road.fills, true);

  if (t.overImagery) {
    ls = overImagery(ls, t);
    ls.unshift(
      { id: "background", type: "background", paint: { "background-color": t.overImagery.background } },
      satelliteLayer(t),
    );
  } else {
    // hillshade over land cover / land use, under water; imagery above every fill and under
    // roads, so toggling "satellite" on gives a hybrid map
    insertAt(ls, "water", [hillshadeLayer(t)], false);
    insertAt(ls, "roads_tunnels_other_casing", [satelliteLayer(t)], false);
  }

  for (const id of t.hide || []) {
    const l = ls.find((x) => x.id === id);
    if (!l) throw new Error(`${t.name}: hide: no layer ${id}`);
    l.layout = { ...(l.layout || {}), visibility: "none" };
  }
  if (t.tweak) ls = t.tweak(ls) ?? ls;

  return {
    version: 8,
    name: t.title,
    metadata: { "mapthing:style": t.name, "mapthing:generator": "assets/styles/src/build.mjs" },
    center: [-102.08, 31.85],
    zoom: 9,
    sprite: `{{BASE_URL}}/sprites/${t.sprite}`,
    glyphs: "{{BASE_URL}}/fonts/{fontstack}/{range}.pbf{{KEY_QS}}",
    sources: sources(t),
    layers: ls,
  };
}
