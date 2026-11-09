// Main roads coloured and widened by rank, for the general-purpose themes (outdoors,
// outdoors-dark): motorway, trunk and primary each get their own colour; secondary and tertiary
// share `other`; every rank also steps in width, so the order survives greyscale and
// colour-blindness. Route shields are drawn larger than the Protomaps default.
//
// A palette: { highway, highwayCasing, link, linkCasing (motorway links), trunk, trunkCasing,
// primary, primaryCasing, other (secondary/tertiary fill), casing (secondary/tertiary casing),
// tunnelHighway }
import { ex } from "../layers.mjs";
import {
  HIGHWAY_CASINGS, HIGHWAY_FILLS, MAJOR_CASINGS, MAJOR_FILLS, byClass, largerShields, setPaint, widthByClass,
} from "./_roads.mjs";

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

/** Protomaps flavour overrides for a palette (motorway colours and casings) */
export function rankedFlavor(c) {
  return {
    highway: c.highway,
    highway_casing_early: c.highwayCasing,
    highway_casing_late: c.highwayCasing,
    bridges_highway: c.highway,
    bridges_highway_casing: c.highwayCasing,
    tunnel_highway: c.tunnelHighway,
    major_casing_early: c.casing,
    major_casing_late: c.casing,
    bridges_major_casing: c.casing,
  };
}

/** per-class colours and widths for major roads and links, and larger shields */
export function rankedRoads(layers, c) {
  const majorFill = byClass({ trunk: c.trunk, primary: c.primary, default: c.other });
  const majorCasing = byClass({ trunk: c.trunkCasing, primary: c.primaryCasing, default: c.casing });
  setPaint(layers, MAJOR_FILLS.slice(0, 2), { "line-color": majorFill }); // tunnels stay pale
  setPaint(layers, MAJOR_FILLS, { "line-width": widthByClass(Z, majorWidth) });
  setPaint(layers, MAJOR_CASINGS, {
    "line-gap-width": widthByClass(Z, majorWidth),
    "line-width": widthByClass(Z, majorCasingWidth),
  });
  setPaint(layers, MAJOR_CASINGS.slice(0, 3), { "line-color": majorCasing });
  setPaint(layers, HIGHWAY_FILLS, { "line-width": highwayWidth });
  setPaint(layers, HIGHWAY_CASINGS, { "line-gap-width": highwayWidth });
  // links: motorway links in the motorway hue, trunk/primary/secondary links like `other`
  const isMotorway = ["==", ["get", "kind"], "highway"];
  setPaint(layers, ["roads_link", "roads_bridges_link"], { "line-color": ["case", isMotorway, c.link, c.other] });
  setPaint(layers, ["roads_link_casing", "roads_bridges_link_casing"], {
    "line-color": ["case", isMotorway, c.linkCasing, c.casing],
  });
  largerShields(layers);
  return layers;
}
