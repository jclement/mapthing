// overlay-dark: the overlay basemap for dark application chrome. Cool charcoal land, dark slate
// water, near-neutral vegetation; roads lighten with rank. Built so light and saturated data
// colours (cyan, orange, light grey, green) read on top. POI icons are off.
import { base } from "./_base.mjs";
import { ex } from "../layers.mjs";
import { streams, thinShields } from "./_roads.mjs";

const casing = "#111213";

export default {
  ...base,
  name: "overlay-dark",
  title: "mapthing overlay dark",
  flavor: "dark",
  sprite: "outdoors-dark",
  flavorOverrides: {
    background: "#141617",
    earth: "#1b1d1f",
    park_a: "#1c211e",
    park_b: "#1e2621",
    wood_a: "#1d2420",
    wood_b: "#1f2621",
    scrub_a: "#1d201d",
    scrub_b: "#1f231f",
    glacier: "#25282b",
    sand: "#22221f",
    beach: "#242421",
    hospital: "#201e1f",
    industrial: "#1e2022",
    school: "#201f1d",
    pedestrian: "#212325",
    aerodrome: "#1f2123",
    runway: "#2c2f32",
    zoo: "#1c211e",
    military: "#1f2021",
    water: "#25313b",
    buildings: "#26292c",
    pier: "#2a2d30",
    tunnel_other_casing: casing,
    tunnel_minor_casing: casing,
    tunnel_link_casing: casing,
    tunnel_major_casing: casing,
    tunnel_highway_casing: casing,
    tunnel_other: "#24272a",
    tunnel_minor: "#24272a",
    tunnel_link: "#24272a",
    tunnel_major: "#24272a",
    tunnel_highway: "#24272a",
    minor_service_casing: casing,
    minor_casing: casing,
    link_casing: casing,
    major_casing_late: casing,
    highway_casing_late: casing,
    major_casing_early: casing,
    highway_casing_early: casing,
    other: "#33373b",
    minor_service: "#4a4f56",
    minor_a: "#464b51",
    minor_b: "#464b51",
    link: "#5a6066",
    major: "#5a6066",
    highway: "#6a7178",
    railway: "#43494e",
    boundaries: "#5a636b",
    bridges_other_casing: casing,
    bridges_minor_casing: casing,
    bridges_link_casing: casing,
    bridges_major_casing: casing,
    bridges_highway_casing: casing,
    bridges_other: "#33373b",
    bridges_minor: "#464b51",
    bridges_link: "#5a6066",
    bridges_major: "#5a6066",
    bridges_highway: "#6a7178",
    roads_label_minor: "#8c959b",
    roads_label_minor_halo: "#141617",
    roads_label_major: "#9ba4aa",
    roads_label_major_halo: "#141617",
    ocean_label: "#7890a0",
    subplace_label: "#848d94",
    subplace_label_halo: "#141617",
    city_label: "#b8c0c6",
    city_label_halo: "#111213",
    state_label: "#6a737a",
    state_label_halo: "#141617",
    country_label: "#848d94",
    address_label: "#7a838a",
    address_label_halo: "#141617",
    landcover: {
      grassland: "rgba(29, 33, 30, 1)",
      barren: "rgba(34, 34, 32, 1)",
      urban_area: "rgba(31, 33, 35, 1)",
      farmland: "rgba(30, 33, 31, 1)",
      glacier: "rgba(40, 43, 46, 1)",
      scrub: "rgba(31, 33, 29, 1)",
      forest: "rgba(29, 37, 32, 1)",
    },
  },
  roads: {
    ...base.roads,
    // lease and service roads get a body (1.25 px over a dark casing) that the client's 1 px
    // survey-grid lines do not have
    serviceWidth: [13, 1.25, 15, 2, 18, 8],
    serviceCasingWidth: [13, 0.5, 18, 1],
  },
  colors: {
    minor: "#464b51", // residential a step below service/lease roads
    minor_casing: casing,
    service: "#4a4f56",
    service_casing: casing,
    track: "#8a7f71",
    track_casing: "rgba(15, 16, 17, 0.6)",
    unpaved: "#978a74",
    road_label: "#8c959b",
    road_label_halo: "#141617",
  },
  labels: { ...base.labels, pois: false },
  // on charcoal a shadow has almost nothing to darken: relief has to come from the highlight
  hillshade: {
    ...base.hillshade,
    exaggeration: [4, 0.7, 10, 0.65, 15, 0.45],
    shadow: "rgba(0, 0, 0, 0.6)",
    highlight: "rgba(255, 255, 255, 0.2)",
    accent: "rgba(255, 255, 255, 0.03)",
  },
  tweak: (layers) => {
    thinShields(layers);
    // route numbers as grey rounded tags, not the sprite's black pill with a white ring, which
    // looks like a junction point in the client's data
    for (const l of layers.filter((x) => x.id.startsWith("roads_shields"))) {
      delete l.layout["icon-image"];
      delete l.layout["icon-size"];
      delete l.layout["icon-rotation-alignment"];
      l.layout["text-size"] = 10;
      l.layout["text-padding"] = 4;
      l.paint = { "text-color": "#c4c8cc", "text-halo-color": "#3a3e44", "text-halo-width": 2.4 };
    }
    streams(layers, "#3b5466", ex([12, 1, 15, 2, 18, 4]));
    return layers;
  },
};
