// Pure helpers for the renderer (no native modules), unit-tested in lib.test.js.

const STYLE_RE = /^[a-z0-9][a-z0-9-]{0,63}$/;
const PATH_RE = /^\/render\/([^/]+)\/(256|512)\/(\d{1,2})\/(\d{1,8})\/(\d{1,8})(@2x)?\.png$/;

// parseRenderPath("/render/outdoors/512/12/917/1663@2x.png") ->
//   { style: "outdoors", size: 512, z: 12, x: 917, y: 1663, ratio: 2 } or null.
export function parseRenderPath(path) {
  const m = PATH_RE.exec(path);
  if (!m) return null;
  const [, style, size, z, x, y, hi] = m;
  if (!STYLE_RE.test(style)) return null;
  const zi = Number(z);
  const n = 2 ** zi;
  const xi = Number(x);
  const yi = Number(y);
  if (zi > 22 || xi >= n || yi >= n) return null;
  return { style, size: Number(size), z: zi, x: xi, y: yi, ratio: hi ? 2 : 1 };
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
