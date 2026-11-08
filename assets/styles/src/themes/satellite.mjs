// satellite: imagery with roads, boundaries and labels drawn over it (no land/water fills).
import { base } from "./_base.mjs";

export default {
  ...base,
  name: "satellite",
  title: "mapthing satellite",
  flavor: "light",
  sprite: "outdoors",
  colors: { ...base.colors, track: "#ffd27f", track_casing: "rgba(0,0,0,0.45)", unpaved: "#ffd27f" },
  satellite: { visible: true, paint: {} },
  overImagery: {
    background: "#1d2a1f", // shown where imagery has no tile
    labelColor: "#ffffff",
    labelHalo: "rgba(0,0,0,0.85)",
    roadOpacity: 0.75,
    roadCasingOpacity: 0.5,
    boundaryColor: "#f2f2f2",
  },
};
