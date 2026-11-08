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
  imageryMaxzoom: 16, // USGS imagery ends at z16; Sentinel-2 cloudless at z14 (see docs/imagery.md)

  // field-use colours for the roads and labels this generator adds or retunes
  colors: {
    minor: "#ffffff",
    minor_casing: "#a89f92",
    service: "#fbfaf7",
    service_casing: "#b3aa9c",
    track: "#7a4a1c",
    track_casing: "rgba(255,255,255,0.75)",
    unpaved: "#a86f3a",
    road_label: "#5e5850",
    road_label_halo: "#ffffff",
  },

  roads: {
    minorWidth: [11, 0.4, 12, 1.2, 15, 2.6, 18, 11],
    minorCasingWidth: [11, 0.3, 12, 0.7, 14, 0.9, 18, 1.2],
    serviceWidth: [13, 0.6, 15, 1.5, 18, 8],
    serviceCasingWidth: [13, 0.5, 18, 1],
    trackMinzoom: 12,
    trackWidth: [12, 1.1, 14, 1.7, 18, 4],
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
    exaggeration: [4, 0.5, 10, 0.45, 15, 0.3],
    shadow: "rgba(60, 50, 40, 0.5)",
    highlight: "rgba(255, 255, 255, 0.25)",
    accent: "rgba(70, 60, 50, 0.12)",
  },

  // contour lines and labels (contours.pmtiles). Widths and opacities are linear zoom stops.
  // The z10-11 tiles carry 100 m major / 20 m minor lines, z12+ 50 m / 10 m.
  contourMaxzoom: 12, // the archive's max zoom; overzoomed beyond
  contours: {
    enabled: true,
    minorColor: "#9c7b5a",
    majorColor: "#8a6a4a",
    minorWidth: [11, 0.4, 14, 0.6, 17, 1],
    majorWidth: [10, 0.6, 14, 1, 17, 1.6],
    minorOpacity: [11, 0, 12, 0.3, 15, 0.4],
    majorOpacity: [10, 0.3, 12, 0.5, 15, 0.6],
    minorMinzoom: 11,
    labelMinzoom: 12,
    labelSize: [12, 10, 16, 12],
    labelSpacing: 350,
    labelColor: "#7a5c3e",
    labelHalo: "rgba(255, 255, 255, 0.85)",
  },

  // every theme has the raster layer "satellite"; only its visibility differs
  satellite: { visible: false, paint: {} },

  // set (see satellite.mjs) to draw roads and labels over imagery with no fills
  overImagery: null,

  hide: [], // layer ids to hide
  tweak: null, // (layers) => layers: last-resort hook for theme-specific edits
};
