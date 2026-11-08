// outdoors: light, field-readable general map (warm earth, visible tracks/lease roads, hillshade).
// Main roads are coloured by rank so motorways and highways stand out in cities: motorway
// orange, trunk yellow, primary pale yellow, secondary and below white; every rank also steps in
// width. Route shields are drawn larger than the Protomaps default.
import { base } from "./_base.mjs";
import { ex } from "../layers.mjs";
import {
  HIGHWAY_CASINGS, HIGHWAY_FILLS, MAJOR_CASINGS, MAJOR_FILLS, byClass, largerShields, setPaint, widthByClass,
} from "./_roads.mjs";

const c = {
  highway: "#f39b55",
  highwayCasing: "#c4652a",
  link: "#f7b77f", // motorway links
  linkCasing: "#cf7f45",
  trunk: "#f7c955",
  trunkCasing: "#c49a2e",
  primary: "#fcd88a",
  primaryCasing: "#c9a65e",
  casing: "#cbc3b7", // secondary and tertiary
};

// total widths (px) at z6, 10, 12, 14, 16, 18; at z14: trunk 5, primary 4.5, secondary 3.5,
// tertiary 2.6 (motorway 6, residential ~2)
const Z = [6, 10, 12, 14, 16, 18];
const majorWidth = {
  trunk: [0.6, 1.6, 2.8, 5, 9, 15],
  primary: [0.3, 1.2, 2.4, 4.5, 8, 14],
  secondary: [0, 0.8, 1.8, 3.5, 7, 12],
  default: [0, 0.5, 1.4, 2.6, 6, 11],
};
const majorCasingWidth = {
  "trunk,primary": [0, 0.5, 0.8, 1, 1.2, 1.5],
  default: [0, 0, 0.6, 0.8, 1, 1.3],
};
const highwayWidth = ex([3, 0, 6, 1.4, 10, 2.2, 12, 3.2, 14, 6, 16, 10, 18, 17]);
const majorFill = byClass({ trunk: c.trunk, primary: c.primary, default: "#ffffff" });
const majorCasing = byClass({ trunk: c.trunkCasing, primary: c.primaryCasing, default: c.casing });

export default {
  ...base,
  name: "outdoors",
  title: "mapthing outdoors",
  flavor: "light",
  sprite: "outdoors",
  flavorOverrides: {
    // industrial includes well pads and compressor sites in oilfields; the Protomaps pale blue
    // (#d1dde1) reads as water there, so use cleared-ground grey-tan instead
    industrial: "#dad4ca",
    highway: c.highway,
    highway_casing_early: c.highwayCasing,
    highway_casing_late: c.highwayCasing,
    bridges_highway: c.highway,
    bridges_highway_casing: c.highwayCasing,
    tunnel_highway: "#f8cfae",
    major_casing_early: c.casing,
    major_casing_late: c.casing,
    bridges_major_casing: c.casing,
  },
  tweak: (layers) => {
    setPaint(layers, MAJOR_FILLS.slice(0, 2), { "line-color": majorFill }); // tunnels stay pale
    setPaint(layers, MAJOR_FILLS, { "line-width": widthByClass(Z, majorWidth) });
    setPaint(layers, MAJOR_CASINGS, {
      "line-gap-width": widthByClass(Z, majorWidth),
      "line-width": widthByClass(Z, majorCasingWidth),
    });
    setPaint(layers, MAJOR_CASINGS.slice(0, 3), { "line-color": majorCasing });
    setPaint(layers, HIGHWAY_FILLS, { "line-width": highwayWidth });
    setPaint(layers, HIGHWAY_CASINGS, { "line-gap-width": highwayWidth });
    // links: motorway links orange, trunk/primary/secondary links white like their class
    const isMotorway = ["==", ["get", "kind"], "highway"];
    setPaint(layers, ["roads_link", "roads_bridges_link"], { "line-color": ["case", isMotorway, c.link, "#ffffff"] });
    setPaint(layers, ["roads_link_casing", "roads_bridges_link_casing"], {
      "line-color": ["case", isMotorway, c.linkCasing, c.casing],
    });
    largerShields(layers);
    return layers;
  },
};

