// Helpers for theme `tweak` hooks that rank roads within Protomaps' kind=major_road, which holds
// trunk, primary, secondary and tertiary roads (and their links), told apart by kind_detail.
//
// A class table maps comma-separated kind_detail values to a value, plus `default` for anything
// else (tertiary and links, usually):
//   { "trunk,primary": 5, secondary: 4, default: 3 }

const DETAIL = ["get", "kind_detail"];

/** ["match", kind_detail, ...] over a class table */
export function byClass(table) {
  const { default: dflt, ...rest } = table;
  const m = ["match", DETAIL];
  for (const [k, v] of Object.entries(rest)) {
    const keys = k.split(",");
    m.push(keys.length === 1 ? keys[0] : keys, v);
  }
  m.push(dflt);
  return m;
}

/**
 * Width that varies by zoom and by class: exponential(1.6) over `zooms`, where each class row
 * holds one value per zoom.
 */
export function widthByClass(zooms, table) {
  const e = ["interpolate", ["exponential", 1.6], ["zoom"]];
  zooms.forEach((z, i) => {
    const t = Object.fromEntries(Object.entries(table).map(([k, row]) => [k, row[i]]));
    e.push(z, byClass(t));
  });
  return e;
}

/** set paint properties on every listed layer that exists */
export function setPaint(layers, ids, props) {
  for (const l of layers) if (ids.includes(l.id)) Object.assign((l.paint ||= {}), props);
}

export const MAJOR_FILLS = ["roads_major", "roads_bridges_major", "roads_tunnels_major"];
export const MAJOR_CASINGS = [
  "roads_major_casing_early",
  "roads_major_casing_late",
  "roads_bridges_major_casing",
  "roads_tunnels_major_casing",
];
export const HIGHWAY_FILLS = ["roads_highway", "roads_bridges_highway", "roads_tunnels_highway"];
export const HIGHWAY_CASINGS = [
  "roads_highway_casing_early",
  "roads_highway_casing_late",
  "roads_bridges_highway_casing",
  "roads_tunnels_highway_casing",
];

/**
 * Route shields: below `minzoom` keep only the listed networks (Interstate and US routes by
 * default), so a regional view is not covered in state and county shields.
 */
export function thinShields(layers, minzoom = 11, networks = ["US:I", "US:US"]) {
  const i = layers.findIndex((l) => l.id === "roads_shields");
  if (i < 0) return;
  const s = layers[i];
  const low = structuredClone(s);
  low.id = "roads_shields_major";
  low.maxzoom = minzoom;
  low.filter = [...s.filter, ["in", ["get", "network"], ["literal", networks]]];
  s.minzoom = minzoom;
  layers.splice(i, 0, low);
}

/** streams as visible lines from `minzoom` (Protomaps draws them from z14 at 0.5 px in the water colour) */
export function streams(layers, color, width, minzoom = 12) {
  const l = layers.find((x) => x.id === "water_stream");
  if (!l) return;
  l.minzoom = minzoom;
  Object.assign(l.paint, { "line-color": color, "line-width": width });
}
