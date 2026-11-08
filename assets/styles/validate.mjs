// Validates the generated styles against the Mapbox GL style spec v13 (what mapbox-gl 1.13.x
// implements) and the current MapLibre style spec, and rejects expressions, properties and
// placeholders that mapbox-gl 1.13 does not understand. Exits non-zero on any problem.
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
const mapboxSpec = createRequire(import.meta.url)("@mapbox/mapbox-gl-style-spec");
import * as maplibreSpec from "@maplibre/maplibre-gl-style-spec";

const STYLES = ["outdoors", "outdoors-dark", "satellite"];

// Expression operators added after mapbox-gl 1.13 (or MapLibre-only).
const BANNED_OPS = new Set([
  "slice", "within", "distance", "global-state", "config", "image", "index-of", "in-range",
  "split", "join", "to-rgba-array", "measure-light", "pitch", "distance-from-center",
  "raster-value", "raster-particle-speed", "sky-radial-progress",
  "elevation", "random", "worldview", "at-interpolated", "number-format-v2",
]);
// "image" was added in mapbox-gl 1.4 but only as a plain sprite-name lookup; we do not need it,
// so it stays banned to keep the styles boring. "line-progress" is fine (0.45) but unused.
const BANNED_PROPS = new Set([
  "text-variable-anchor-offset", "icon-overlap", "text-overlap", "symbol-z-elevate",
  "fill-extrusion-edge-radius",
  "line-occlusion-opacity", "line-emissive-strength", "fill-emissive-strength",
  "icon-emissive-strength", "text-emissive-strength", "circle-emissive-strength",
  "hillshade-method", "color-relief-color", "color-relief-opacity", "resampling",
  "line-trim-offset", "line-z-offset", "fill-z-offset", "line-cutout-opacity",
  "symbol-elevation-reference", "text-occlusion-opacity", "icon-occlusion-opacity",
]);
const ALLOWED_LAYER_TYPES = new Set(["background", "fill", "line", "symbol", "raster", "hillshade", "circle", "fill-extrusion", "heatmap"]);

function walkOps(v, path, errs) {
  if (Array.isArray(v)) {
    if (typeof v[0] === "string" && BANNED_OPS.has(v[0])) errs.push(`${path}: expression "${v[0]}" is newer than mapbox-gl 1.13`);
    v.forEach((x, i) => walkOps(x, `${path}[${i}]`, errs));
  } else if (v && typeof v === "object") {
    for (const [k, x] of Object.entries(v)) walkOps(x, `${path}.${k}`, errs);
  }
}

let failed = false;
for (const name of STYLES) {
  const file = new URL(`./${name}.json`, import.meta.url);
  const text = readFileSync(file, "utf8");
  // the spec validators need real URLs; substitute the placeholders the way mapthing does
  const style = JSON.parse(text.replaceAll("{{BASE_URL}}", "http://127.0.0.1:8080").replaceAll("{{KEY_QS}}", "?key=mt_test"));
  const errs = [];

  for (const e of mapboxSpec.validate(style)) errs.push(`mapbox-gl-style-spec@13: ${e.message}`);
  for (const e of maplibreSpec.validateStyleMin(style)) errs.push(`maplibre-gl-style-spec: ${e.message}`);

  const ids = new Set(style.layers.map((l) => l.id));
  if (!ids.has("satellite")) errs.push("no layer with id satellite");
  const sat = style.layers.find((l) => l.id === "satellite");
  if (sat && (sat.type !== "raster" || sat.source !== "imagery")) errs.push("satellite layer must be raster from source imagery");
  const vis = sat?.layout?.visibility ?? "visible";
  if (name === "satellite" ? vis !== "visible" : vis !== "none") errs.push(`satellite layer visibility ${vis} wrong for ${name}`);

  for (const l of style.layers) {
    if (!ALLOWED_LAYER_TYPES.has(l.type)) errs.push(`${l.id}: layer type ${l.type}`);
    for (const group of ["layout", "paint"]) {
      for (const k of Object.keys(l[group] || {})) if (BANNED_PROPS.has(k)) errs.push(`${l.id}: property ${k} is newer than mapbox-gl 1.13`);
    }
    walkOps(l.filter, `${l.id}.filter`, errs);
    walkOps(l.layout, `${l.id}.layout`, errs);
    walkOps(l.paint, `${l.id}.paint`, errs);
    for (const f of JSON.stringify(l.layout?.["text-font"] ?? []).match(/Noto Sans[^"]*/g) ?? []) {
      if (!["Noto Sans Regular", "Noto Sans Medium", "Noto Sans Italic"].includes(f)) errs.push(`${l.id}: font ${f} has no glyphs`);
    }
  }
  for (const [id, src] of Object.entries(style.sources)) {
    if (!src.attribution) errs.push(`source ${id} has no attribution`);
  }
  if (!text.includes("{{KEY_QS}}") || !text.includes("{{BASE_URL}}")) errs.push("placeholders missing");
  if (/\{\{(?!BASE_URL|KEY_QS)[^}]*\}\}/.test(text)) errs.push("unknown {{placeholder}}");

  if (errs.length) {
    failed = true;
    console.error(`${name}.json: ${errs.length} problem(s)`);
    for (const e of errs) console.error("  " + e);
  } else {
    console.log(`${name}.json: ok (${style.layers.length} layers)`);
  }
}
process.exit(failed ? 1 : 0);
