// outdoors: light, field-readable general map (warm earth, visible tracks/lease roads, hillshade).
// Main roads are coloured by rank so motorways and highways stand out in cities: motorway
// orange, trunk yellow, primary pale yellow, secondary and below white; every rank also steps in
// width (see _ranked.mjs). Route shields are drawn larger than the Protomaps default.
import { base } from "./_base.mjs";
import { rankedFlavor, rankedRoads } from "./_ranked.mjs";

const c = {
  highway: "#f39b55",
  highwayCasing: "#c4652a",
  link: "#f7b77f", // motorway links
  linkCasing: "#cf7f45",
  trunk: "#f7c955",
  trunkCasing: "#c49a2e",
  primary: "#fcd88a",
  primaryCasing: "#c9a65e",
  other: "#ffffff", // secondary and tertiary
  casing: "#cbc3b7",
  tunnelHighway: "#f8cfae",
};

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
    ...rankedFlavor(c),
  },
  tweak: (layers) => rankedRoads(layers, c),
};
