// overlay: a calm, low-contrast light basemap for drawing dense data on top (coloured points,
// pipelines, survey grids). Cool neutral paper, greyed water and vegetation so data blues and
// greens stay distinct, white roads ranked by width and casing (not hue), muted taupe tracks.
// Labels stay >= 4.5:1 for places and >= 3:1 for roads. POI icons are off.
import { base } from "./_base.mjs";
import { ex } from "../layers.mjs";
import {
  HIGHWAY_CASINGS, HIGHWAY_FILLS, MAJOR_CASINGS, MAJOR_FILLS, byClass, setPaint, streams, thinShields, widthByClass,
} from "./_roads.mjs";

// Rank is carried by width and casing darkness: every road is white, casings darken with rank.
const casing = {
  highway: "#8c969e",
  primary: "#97a1a9", // trunk and primary
  secondary: "#a9b2b9",
  tertiary: "#b6bec4", // and links
  link: "#abb4ba",
  minor: "#bcc4c9",
  service: "#b9c1c7",
};
// total widths (px) at z6, 10, 12, 14, 16, 18; at z14: motorway 6, primary 5, secondary 4,
// tertiary 3, residential 2
const Z = [6, 10, 12, 14, 16, 18];
const majorWidth = {
  "trunk,primary": [0.3, 1, 2.2, 5, 9, 14],
  secondary: [0, 0.6, 1.8, 4, 7.5, 12],
  default: [0, 0.4, 1.3, 3, 6, 10],
};
const majorCasingWidth = {
  "trunk,primary": [0, 0.6, 0.9, 1, 1.2, 1.5],
  secondary: [0, 0.4, 0.8, 0.9, 1.1, 1.4],
  default: [0, 0, 0.7, 0.8, 1, 1.3],
};
const highwayWidth = ex([3, 0, 6, 1.1, 10, 1.6, 12, 2.6, 14, 6, 16, 10, 18, 16]);
// neutral fill for schools, hospitals, industry etc.: no warm or pink notes in a cool theme
const neutralUse = "#e3e6e9";

export default {
  ...base,
  name: "overlay",
  title: "mapthing overlay",
  flavor: "light",
  sprite: "outdoors",
  flavorOverrides: {
    background: "#e6e9ea",
    earth: "#eaeded",
    park_a: "#e3e9e5",
    park_b: "#d9e3dc",
    wood_a: "#e2e8e3",
    wood_b: "#d8e2da",
    scrub_a: "#e6eae5",
    scrub_b: "#dde5dd",
    glacier: "#f6f7f7",
    sand: neutralUse,
    beach: neutralUse,
    hospital: neutralUse,
    industrial: neutralUse,
    school: neutralUse,
    pedestrian: neutralUse,
    aerodrome: "#e4e7e9",
    runway: "#d6dadd",
    zoo: "#e3e9e5",
    military: neutralUse,
    water: "#c3d1da",
    buildings: "#dadee1",
    pier: "#e2e5e7",
    tunnel_other_casing: "#dfe3e5",
    tunnel_minor_casing: "#dfe3e5",
    tunnel_link_casing: "#dfe3e5",
    tunnel_major_casing: "#dfe3e5",
    tunnel_highway_casing: "#dfe3e5",
    tunnel_other: "#e9ecec",
    tunnel_minor: "#e9ecec",
    tunnel_link: "#e9ecec",
    tunnel_major: "#e9ecec",
    tunnel_highway: "#e9ecec",
    minor_service_casing: casing.service,
    minor_casing: casing.minor,
    link_casing: casing.link,
    major_casing_late: casing.primary,
    highway_casing_late: casing.highway,
    major_casing_early: casing.primary,
    highway_casing_early: casing.highway,
    other: "#f8f9f9",
    minor_service: "#fbfcfc",
    minor_a: "#fbfcfc",
    minor_b: "#ffffff",
    link: "#ffffff",
    major: "#ffffff",
    highway: "#ffffff",
    railway: "#b5bcc1",
    boundaries: "#a3acb2",
    bridges_other_casing: casing.service,
    bridges_minor_casing: casing.minor,
    bridges_link_casing: casing.link,
    bridges_major_casing: casing.primary,
    bridges_highway_casing: casing.highway,
    bridges_other: "#f8f9f9",
    bridges_minor: "#ffffff",
    bridges_link: "#ffffff",
    bridges_major: "#ffffff",
    bridges_highway: "#ffffff",
    roads_label_minor: "#677076",
    roads_label_minor_halo: "#ffffff",
    roads_label_major: "#59626a",
    roads_label_major_halo: "#ffffff",
    ocean_label: "#4f6e82",
    subplace_label: "#69737a",
    subplace_label_halo: "#f2f4f4",
    city_label: "#3b4349",
    city_label_halo: "#f4f6f6",
    state_label: "#7b858c",
    state_label_halo: "#f2f4f4",
    country_label: "#7c868d",
    address_label: "#6f787e",
    address_label_halo: "#ffffff",
    landcover: {
      grassland: "rgba(230, 236, 230, 1)",
      barren: "rgba(240, 238, 233, 1)",
      urban_area: "rgba(229, 232, 233, 1)",
      farmland: "rgba(234, 238, 233, 1)",
      glacier: "rgba(248, 249, 249, 1)",
      scrub: "rgba(234, 237, 230, 1)",
      forest: "rgba(222, 231, 225, 1)",
    },
  },
  roads: {
    ...base.roads,
    minorWidth: [11, 0.4, 12, 0.9, 14, 2, 15, 2.6, 18, 11],
    minorCasingWidth: [11, 0.3, 12, 0.6, 14, 0.75, 18, 1.2],
    // lease and service roads: at z13 one solid warm-grey line (a hue survey grids never use;
    // a 1 px core does not resolve as cored there), from z14 a white-cored road
    serviceWidth: [13, 1, 14, 1.5, 15, 2, 18, 8],
    serviceCasingWidth: [13, 0.6, 15, 0.75, 18, 1],
  },
  colors: {
    minor: "#ffffff",
    minor_casing: casing.minor,
    service: "#ffffff",
    service_casing: casing.service,
    track: "#7c7165", // taupe, not orange: orange belongs to the data (trucks, batteries)
    track_casing: "rgba(255,255,255,0.7)",
    unpaved: "#8f8373",
    road_label: "#677076",
    road_label_halo: "#ffffff",
  },
  labels: { ...base.labels, pois: false },
  hillshade: {
    ...base.hillshade,
    exaggeration: [4, 0.55, 10, 0.5, 15, 0.35],
    shadow: "rgba(70, 82, 95, 0.5)",
    highlight: "rgba(255, 255, 255, 0.4)",
    accent: "rgba(70, 82, 95, 0.12)",
  },
  // subtle: cool grey, thin, low opacity; data drawn on top must dominate
  contours: {
    ...base.contours,
    minorColor: "#98a3ab",
    majorColor: "#8a959d",
    minorOpacity: [11, 0, 12, 0.2, 15, 0.3],
    majorOpacity: [10, 0.25, 12, 0.35, 15, 0.45],
    minorWidth: [11, 0.3, 14, 0.5, 17, 0.8],
    majorWidth: [10, 0.5, 14, 0.8, 17, 1.2],
    labelColor: "#7b858c",
    labelHalo: "rgba(242, 244, 244, 0.9)",
  },
  tweak: (layers) => {
    setPaint(layers, MAJOR_FILLS, { "line-width": widthByClass(Z, majorWidth) });
    setPaint(layers, MAJOR_CASINGS, {
      "line-gap-width": widthByClass(Z, majorWidth),
      "line-width": widthByClass(Z, majorCasingWidth),
    });
    // tunnels keep their pale casing
    setPaint(layers, MAJOR_CASINGS.slice(0, 3), {
      "line-color": byClass({ "trunk,primary": casing.primary, secondary: casing.secondary, default: casing.tertiary }),
    });
    setPaint(layers, HIGHWAY_FILLS, { "line-width": highwayWidth });
    setPaint(layers, HIGHWAY_CASINGS, { "line-gap-width": highwayWidth });
    const service = layers.find((l) => l.id === "roads_minor_service");
    service.minzoom = 12.5;
    Object.assign(service.paint, {
      "line-color": ["step", ["zoom"], "#b9b2a8", 14, "#ffffff"],
      "line-opacity": ["interpolate", ["linear"], ["zoom"], 12.5, 0, 13, 1],
    });
    setPaint(layers, ["roads_minor_service_casing"], { "line-opacity": ["step", ["zoom"], 0, 14, 1] });
    thinShields(layers);
    streams(layers, "#a7b8c4", ex([12, 1, 15, 2, 18, 4]));
    return layers;
  },
};
