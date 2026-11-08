// field: high-contrast, sunlight-readable light map for outdoor use on phones and tablets.
// Warm paper land, strong water and vegetation, roads coloured by rank (motorway orange; trunk,
// primary and secondary yellow; tertiary, minor and service white with dark casings) and also
// ranked by width so the order survives greyscale and colour-blindness; lease/service roads
// with a solid dark casing from z13; dark-brown dashed tracks from z11; strong hillshade;
// labels 20% larger with wide white halos (>= 4.5:1).
import { base } from "./_base.mjs";
import { ex } from "../layers.mjs";
import { scaleTextSize } from "./_text.mjs";
import {
  HIGHWAY_CASINGS, HIGHWAY_FILLS, MAJOR_CASINGS, MAJOR_FILLS, byClass, largerShields, setPaint, widthByClass,
} from "./_roads.mjs";

const c = {
  highway: "#ef7d3c",
  highwayCasing: "#8f3312",
  major: "#f2c230", // trunk, primary, secondary
  majorLow: "#d9a520", // secondary at z<=10, drawn without a casing (thin yellow + dark casing reads olive)
  majorCasing: "#a8801a",
  link: "#f5a565",
  linkCasing: "#9c4a14",
  minorCasing: "#5c5247", // tertiary, minor and service
};

// total widths (px) at z6, 10, 11, 12, 14, 16, 18; at z14: motorway 7, primary 5.5,
// secondary 4.5, tertiary 3.5 (residential 2.5, see roads.minorWidth)
const Z = [6, 10, 11, 12, 14, 16, 18];
const majorWidth = {
  "trunk,primary": [0.5, 1.6, 2, 2.8, 5.5, 9.5, 16],
  secondary: [0.3, 1.1, 1.5, 2.2, 4.5, 8, 14],
  default: [0, 0.6, 1, 1.6, 3.5, 6.5, 12],
};
const majorCasingWidth = {
  "trunk,primary": [0.3, 0.8, 0.9, 1, 1.1, 1.4, 1.8],
  secondary: [0, 0, 0.6, 0.8, 1, 1.3, 1.6],
  default: [0, 0, 0.5, 0.8, 1, 1.3, 1.6],
};
const highwayWidth = ex([3, 0, 6, 1.2, 10, 2, 12, 3.4, 14, 7, 16, 11, 18, 18]);
const majorFill = ["step", ["zoom"],
  byClass({ "trunk,primary": c.major, secondary: c.majorLow, default: "#ffffff" }),
  11, byClass({ "trunk,primary,secondary": c.major, default: "#ffffff" }),
];
const majorCasing = byClass({ "trunk,primary,secondary": c.majorCasing, default: c.minorCasing });

export default {
  ...base,
  name: "field",
  title: "mapthing field",
  flavor: "light",
  sprite: "outdoors",
  flavorOverrides: {
    background: "#f2ecdf",
    earth: "#f6f1e4",
    park_a: "#cfe5c2",
    park_b: "#b5d9a3",
    wood_a: "#c4dcb1",
    wood_b: "#a9cf93",
    scrub_a: "#dfe7c6",
    scrub_b: "#d0dfaf",
    glacier: "#ffffff",
    sand: "#efe2bf",
    beach: "#f2e5bd",
    hospital: "#f1dcdc",
    industrial: "#e6dfd6",
    school: "#efe5d0",
    pedestrian: "#ece5d8",
    aerodrome: "#e3dfd8",
    runway: "#b8b2a9",
    zoo: "#cfe5c2",
    military: "#ead9d9",
    water: "#5b9bd5",
    buildings: "#d5cab8",
    pier: "#d9d2c6",
    tunnel_other_casing: "#a39b8f",
    tunnel_minor_casing: "#a39b8f",
    tunnel_link_casing: "#a39b8f",
    tunnel_major_casing: "#a39b8f",
    tunnel_highway_casing: "#a39b8f",
    tunnel_other: "#e8e2d6",
    tunnel_minor: "#e8e2d6",
    tunnel_link: "#e8e2d6",
    tunnel_major: "#f3e6b8",
    tunnel_highway: "#f2c7a8",
    minor_service_casing: c.minorCasing,
    minor_casing: c.minorCasing,
    link_casing: c.linkCasing,
    major_casing_late: c.majorCasing,
    highway_casing_late: c.highwayCasing,
    major_casing_early: c.majorCasing,
    highway_casing_early: c.highwayCasing,
    other: "#ffffff",
    minor_service: "#ffffff",
    minor_a: "#ffffff",
    minor_b: "#ffffff",
    link: c.link,
    major: c.major,
    highway: c.highway,
    railway: "#55504a",
    boundaries: "#86699a",
    bridges_other_casing: c.minorCasing,
    bridges_minor_casing: c.minorCasing,
    bridges_link_casing: c.linkCasing,
    bridges_major_casing: c.majorCasing,
    bridges_highway_casing: c.highwayCasing,
    bridges_other: "#ffffff",
    bridges_minor: "#ffffff",
    bridges_link: c.link,
    bridges_major: c.major,
    bridges_highway: c.highway,
    roads_label_minor: "#2a2a2a",
    roads_label_minor_halo: "#ffffff",
    roads_label_major: "#1c1c1c",
    roads_label_major_halo: "#ffffff",
    ocean_label: "#0c2f54",
    subplace_label: "#333333",
    subplace_label_halo: "#ffffff",
    city_label: "#111111",
    city_label_halo: "#ffffff",
    state_label: "#555555",
    state_label_halo: "#ffffff",
    country_label: "#3f3f3f",
    address_label: "#3a3a3a",
    address_label_halo: "#ffffff",
    landcover: {
      grassland: "rgba(222, 236, 205, 1)",
      barren: "rgba(240, 228, 198, 1)",
      urban_area: "rgba(234, 229, 220, 1)",
      farmland: "rgba(237, 238, 214, 1)",
      glacier: "rgba(255, 255, 255, 1)",
      scrub: "rgba(226, 232, 196, 1)",
      forest: "rgba(190, 218, 172, 1)",
    },
  },
  colors: {
    minor: "#ffffff",
    // residential/unclassified: a lighter casing up to z12 so suburbs are not a dark lattice
    minor_casing: ["step", ["zoom"], "#8a7f72", 13, c.minorCasing],
    service: "#ffffff",
    service_casing: c.minorCasing,
    track: "#5a3a1e",
    track_casing: "rgba(255, 255, 255, 0.9)",
    unpaved: "#8a4b17",
    road_label: "#2a2a2a",
    road_label_halo: "#ffffff",
  },
  roads: {
    ...base.roads,
    minorWidth: [11, 0.5, 12, 1.4, 14, 2.5, 15, 3.3, 18, 13],
    minorCasingWidth: [11, 0.4, 12, 0.8, 14, 1, 18, 1.6],
    // lease and service roads, the theme's headline job: ~3 px at z13, ~5.5 px at z15
    serviceWidth: [13, 1.5, 14, 2.5, 15, 3.5, 16, 5, 18, 9],
    serviceCasingWidth: [13, 0.75, 14, 1, 15, 1, 16, 1.25, 18, 1.6],
    trackMinzoom: 11,
    trackWidth: [11, 0.7, 12, 1, 13, 1.5, 15, 2.5, 18, 5],
    trackCasingWidth: [11, 2, 12, 2.6, 13, 3.4, 15, 5, 18, 9],
    trackDash: [2.5, 1.5],
    unpavedWidth: [11, 0.8, 14, 1.4, 18, 3.5],
  },
  labels: { ...base.labels, haloWidth: 2 },
  hillshade: {
    ...base.hillshade,
    exaggeration: [4, 0.6, 10, 0.5, 15, 0.35],
    shadow: "rgba(60, 40, 20, 0.45)",
    highlight: "rgba(255, 255, 255, 0.35)",
    accent: "rgba(60, 40, 20, 0.18)",
  },
  tweak: (layers) => {
    setPaint(layers, MAJOR_FILLS.slice(0, 2), { "line-color": majorFill });
    setPaint(layers, MAJOR_FILLS, { "line-width": widthByClass(Z, majorWidth) });
    setPaint(layers, MAJOR_CASINGS, {
      "line-gap-width": widthByClass(Z, majorWidth),
      "line-width": widthByClass(Z, majorCasingWidth),
    });
    setPaint(layers, MAJOR_CASINGS.slice(0, 3), { "line-color": majorCasing }); // tunnels keep their pale casing
    setPaint(layers, HIGHWAY_FILLS, { "line-width": highwayWidth });
    setPaint(layers, HIGHWAY_CASINGS, { "line-gap-width": highwayWidth });
    scaleTextSize(layers, 1.2, ["roads_shields"]);
    largerShields(layers, 1.1, 11);
    for (const l of layers) {
      if (l.type === "symbol" && l.paint && l.paint["text-halo-width"] !== undefined) l.paint["text-halo-width"] = 2;
    }
    return layers;
  },
};
