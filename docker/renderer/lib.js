// Pure helpers for the renderer (no native modules), unit-tested in lib.test.js.

const STYLE_RE = /^[a-z0-9][a-z0-9-]{0,63}$/;
const PATH_RE = /^\/render\/([^/]+)\/(256|512)\/(\d{1,2})\/(\d{1,8})\/(\d{1,8})(@2x)?\.(png|jpg|webp)$/;

// Output formats: file extension → Content-Type.
export const FORMATS = { png: "image/png", jpg: "image/jpeg", webp: "image/webp" };

// parseRenderPath("/render/outdoors/512/12/917/1663@2x.png") ->
//   { style: "outdoors", size: 512, z: 12, x: 917, y: 1663, ratio: 2, format: "png" } or null.
export function parseRenderPath(path) {
  const m = PATH_RE.exec(path);
  if (!m) return null;
  const [, style, size, z, x, y, hi, format] = m;
  if (!STYLE_RE.test(style)) return null;
  const zi = Number(z);
  const n = 2 ** zi;
  const xi = Number(x);
  const yi = Number(y);
  if (zi > 22 || xi >= n || yi >= n) return null;
  return { style, size: Number(size), z: zi, x: xi, y: yi, ratio: hi ? 2 : 1, format };
}

// Centre of an XYZ tile as [lon, lat] (Web Mercator).
export function tileCenter(z, x, y) {
  const n = 2 ** z;
  const lon = ((x + 0.5) / n) * 360 - 180;
  const lat = (Math.atan(Math.sinh(Math.PI * (1 - (2 * (y + 0.5)) / n))) * 180) / Math.PI;
  return [lon, lat];
}

// rewriteURL points every resource URL at mapthing's internal base, whatever host the style
// template used (MAPTHING_PUBLIC_URL may be an external name the container cannot reach), and
// repairs sprite URLs where MapLibre Native appended "@2x.json" after a query string.
export function rewriteURL(url, base) {
  let u;
  try {
    u = new URL(url);
  } catch {
    return url;
  }
  // "...sprites/outdoors?key=x@2x.json" -> path "...sprites/outdoors@2x.json", query "?key=x"
  const m = /^(.*?)(@2x)?\.(json|png)$/.exec(u.search);
  if (u.search && m && u.pathname.includes("/sprites/")) {
    u.pathname += `${m[2] || ""}.${m[3]}`;
    u.search = m[1];
  }
  const b = new URL(base);
  if (/^\/(tiles|fonts|sprites|styles)\//.test(u.pathname)) {
    u.protocol = b.protocol;
    u.host = b.host;
  }
  return u.toString();
}

// Degrading per source: a failed sub-resource must not fail the whole raster tile (a missing
// terrain archive or an imagery upstream outage must never 503 the basemap). classify() says
// what a resource request is; degradedResponse() is what MapLibre gets instead of an error.

// mbgl.Resource kinds (MapLibre Native): 1 style, 2 source, 3 tile, 4 glyphs, 5 sprite image,
// 6 sprite json, 7 image.
const KIND_NAMES = { 1: "style", 2: "source", 3: "tile", 4: "glyphs", 5: "sprite", 6: "sprite", 7: "image" };

export function classify(url, kind) {
  if (kind && KIND_NAMES[kind]) return KIND_NAMES[kind];
  const path = (() => {
    try {
      return new URL(url).pathname;
    } catch {
      return url;
    }
  })();
  if (/^\/styles\/[^/]+\.json$/.test(path)) return "style";
  if (/^\/tiles\/[^/]+\.json$/.test(path)) return "source";
  if (path.startsWith("/fonts/")) return "glyphs";
  if (path.startsWith("/sprites/")) return "sprite";
  return "tile";
}

// An empty TileJSON whose tiles never resolve: the source loads and draws nothing.
export const EMPTY_TILE_URL = "mapthing-empty://tile/{z}/{x}/{y}";
const EMPTY_TILEJSON = JSON.stringify({ tilejson: "2.2.0", tiles: [EMPTY_TILE_URL], minzoom: 0, maxzoom: 0 });

// degradedResponse returns the stand-in for a failed resource: undefined = fail (styles are
// essential), null = "no data" (tiles, glyphs, sprites), or a Buffer (an empty TileJSON).
export function degradedResponse(what) {
  if (what === "style") return undefined;
  if (what === "source") return Buffer.from(EMPTY_TILEJSON);
  return null;
}
