// Helpers for theme tweak() hooks.

// Multiply every label size by `factor`: plain numbers, and the output stops of
// interpolate/step expressions (including nested case outputs). Layers in `except` keep their size.
export function scaleTextSize(layers, factor, except = []) {
  const scale = (v) => {
    if (typeof v === "number") return Math.round(v * factor * 10) / 10;
    if (!Array.isArray(v)) return v;
    const [op] = v;
    if (op === "interpolate") return [v[0], v[1], v[2], ...v.slice(3).map((x, i) => (i % 2 ? scale(x) : x))];
    if (op === "step") return [v[0], v[1], scale(v[2]), ...v.slice(3).map((x, i) => (i % 2 ? scale(x) : x))];
    if (op === "case") return [v[0], ...v.slice(1).map((x, i, a) => (i % 2 || i === a.length - 1 ? scale(x) : x))];
    if (op === "match") return [v[0], v[1], ...v.slice(2).map((x, i, a) => (i % 2 || i === a.length - 1 ? scale(x) : x))];
    return v;
  };
  for (const l of layers) {
    if (except.includes(l.id) || !l.layout || l.layout["text-size"] === undefined) continue;
    l.layout["text-size"] = scale(l.layout["text-size"]);
  }
  return layers;
}
