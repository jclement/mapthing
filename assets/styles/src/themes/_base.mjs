// Shared defaults for themes. A theme spreads these and overrides what it needs:
//   export default { ...base, name: "x", roads: { ...base.roads, minorWidth: [...] } }
// Zoom arrays are [zoom, value, zoom, value, ...] stops (exponential 1.6 for widths, linear for
// hillshade exaggeration).
export const base = {
  // name: style id (file name and /styles/{name}.json); title: human name
  flavor: "light", // Protomaps flavour to start from: light | dark | white | grayscale | black
  flavorOverrides: {}, // any Protomaps Flavor colour key, e.g. { earth: "#efe9df" }
  sprite: "outdoors", // assets/sprites/<sprite>{,@2x}.{json,png}
  terrainMaxzoom: 12,
  imageryMaxzoom: 18,

  // field-use colours for the roads and labels this generator adds or retunes
  colors: {
    minor: "#ffffff",
    minor_casing: "#bdb6ab",
    service: "#fbfaf7",
    service_casing: "#c9c2b6",
    track: "#8a5a2b",
    track_casing: "rgba(255,255,255,0.75)",
    unpaved: "#a86f3a",
    road_label: "#5e5850",
    road_label_halo: "#ffffff",
  },

  roads: {
    minorWidth: [11, 0, 12, 1, 15, 2.5, 18, 11],
    minorCasingWidth: [11, 0, 12, 0.6, 14, 0.8, 18, 1.2],
    serviceWidth: [13, 0.6, 15, 1.5, 18, 8],
    serviceCasingWidth: [13, 0.5, 18, 1],
    trackMinzoom: 12,
    trackWidth: [12, 0.9, 14, 1.4, 18, 4],
    trackCasingWidth: [12, 2, 14, 3, 18, 8],
    trackDash: [3, 1.5],
    unpavedWidth: [11, 0.6, 14, 1, 18, 3],
    unpavedDash: [2, 2],
  },

  labels: {
    minorRoadMinzoom: 13, // Protomaps default is 15
    haloWidth: 1.5,
    pois: true, // false hides the POI layer
  },

  hillshade: {
    enabled: true,
    exaggeration: [4, 0.45, 10, 0.35, 15, 0.25],
    shadow: "rgba(70, 60, 50, 0.32)",
    highlight: "rgba(255, 255, 255, 0.25)",
    accent: "rgba(70, 60, 50, 0.12)",
  },

  // every theme has the raster layer "satellite"; only its visibility differs
  satellite: { visible: false, paint: {} },

  // set (see satellite.mjs) to draw roads and labels over imagery with no fills
  overImagery: null,

  hide: [], // layer ids to hide
  tweak: null, // (layers) => layers: last-resort hook for theme-specific edits
};
