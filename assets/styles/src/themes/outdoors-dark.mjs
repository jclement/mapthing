// outdoors-dark: the outdoors map on the Protomaps dark flavour.
import { base } from "./_base.mjs";

export default {
  ...base,
  name: "outdoors-dark",
  title: "mapthing outdoors dark",
  flavor: "dark",
  sprite: "outdoors-dark",
  colors: {
    minor: "#4a4a4a",
    minor_casing: "#2a2a2a",
    service: "#3e3e3e",
    service_casing: "#262626",
    track: "#c8955e",
    track_casing: "rgba(20,20,20,0.6)",
    unpaved: "#c8955e",
    road_label: "#bdbdbd",
    road_label_halo: "#1f1f1f",
  },
  // light lines on dark land
  contours: {
    ...base.contours,
    minorColor: "#b8a68c",
    majorColor: "#cdbb9f",
    minorOpacity: [11, 0, 12, 0.18, 15, 0.25],
    majorOpacity: [10, 0.25, 12, 0.35, 15, 0.45],
    labelColor: "#b8a68c",
    labelHalo: "rgba(20, 22, 23, 0.85)",
  },
  hillshade: {
    ...base.hillshade,
    shadow: "rgba(0, 0, 0, 0.45)",
    highlight: "rgba(255, 255, 255, 0.06)",
    accent: "rgba(0, 0, 0, 0.15)",
  },
};
