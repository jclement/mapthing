import type { BBox, LonLat } from "../api";

const R = 6371008.8;
const rad = (d: number) => (d * Math.PI) / 180;

export function haversine(a: LonLat, b: LonLat): number {
  const dLat = rad(b[1] - a[1]);
  const dLon = rad(b[0] - a[0]);
  const h =
    Math.sin(dLat / 2) ** 2 + Math.cos(rad(a[1])) * Math.cos(rad(b[1])) * Math.sin(dLon / 2) ** 2;
  return 2 * R * Math.asin(Math.sqrt(h));
}

export function lineLength(coords: LonLat[]): number {
  let d = 0;
  for (let i = 1; i < coords.length; i++) d += haversine(coords[i - 1], coords[i]);
  return d;
}

/** Point at `dist` metres along a line. */
export function along(coords: LonLat[], dist: number): LonLat {
  let acc = 0;
  for (let i = 1; i < coords.length; i++) {
    const seg = haversine(coords[i - 1], coords[i]);
    if (acc + seg >= dist) {
      const f = seg === 0 ? 0 : (dist - acc) / seg;
      const [x0, y0] = coords[i - 1];
      const [x1, y1] = coords[i];
      return [x0 + (x1 - x0) * f, y0 + (y1 - y0) * f];
    }
    acc += seg;
  }
  return coords[coords.length - 1];
}

export function bboxAreaKm2([w, s, e, n]: BBox): number {
  // Exact area of a longitude/latitude rectangle on a sphere: R² · Δλ · |sin φn − sin φs|.
  // Measured by longitude span, not by the distance between the edges: a box spanning
  // −180…180 (Geofabrik's North America, US and Alaska) has both edges on one meridian.
  const R = 6371.0088;
  let span = e - w;
  if (span < 0) span += 360; // crosses the antimeridian
  const rad = Math.PI / 180;
  return R * R * Math.min(span, 360) * rad * Math.abs(Math.sin(n * rad) - Math.sin(s * rad));
}

export function bboxPolygon([w, s, e, n]: BBox): LonLat[] {
  return [
    [w, s],
    [e, s],
    [e, n],
    [w, n],
    [w, s],
  ];
}

export function bboxOf(coords: LonLat[]): BBox {
  let w = 180;
  let s = 90;
  let e = -180;
  let n = -90;
  for (const [x, y] of coords) {
    w = Math.min(w, x);
    e = Math.max(e, x);
    s = Math.min(s, y);
    n = Math.max(n, y);
  }
  return [w, s, e, n];
}

export function bboxContains(outer: BBox, inner: BBox): boolean {
  return (
    inner[0] >= outer[0] && inner[1] >= outer[1] && inner[2] <= outer[2] && inner[3] <= outer[3]
  );
}

/**
 * Rough disk estimate for a region before it is installed: the source extract, the
 * vector basemap (about the extract size), the routing graph (about 1.6× the extract)
 * and terrain tiles (driven by area and DEM resolution). Shown as "≈".
 */
export function estimateDisk(opts: { areaKm2: number; extractBytes: number; bestResM: number }) {
  const download = opts.extractBytes;
  const basemap = opts.extractBytes * 1.05;
  const routing = opts.extractBytes * 1.6;
  // Terrarium PNG at a zoom matched to resolution: ~1.2 bytes per output pixel.
  const pxPerKm2 = 1e6 / Math.max(opts.bestResM, 5) ** 2;
  const terrain = Math.min(opts.areaKm2 * pxPerKm2 * 1.2, 60e9);
  return { download, basemap, routing, terrain, total: download + basemap + routing + terrain };
}

export function formatLonLat([lon, lat]: LonLat): string {
  return `${lat.toFixed(5)}, ${lon.toFixed(5)}`;
}

export interface Padding {
  top: number;
  right: number;
  bottom: number;
  left: number;
}

/**
 * Padding for fitting a route so it is not hidden under the route panel. `panel` is the panel's
 * rect relative to the map container. A narrow panel is the desktop side panel (pad its right
 * edge on the left); a full-width one is the mobile bottom sheet (pad its top edge at the
 * bottom). `topInset` clears the floating
 * toolbar. Padding is capped so at least 20% of the map stays for the route.
 */
export function routeFitPadding(
  map: { width: number; height: number },
  panel: { left: number; top: number; width: number; height: number } | null,
  margin = 48,
  topInset = 0,
): Padding {
  const p: Padding = { top: margin + topInset, right: margin, bottom: margin, left: margin };
  if (panel && panel.width > 0 && panel.height > 0) {
    if (panel.width < map.width * 0.8) p.left = panel.left + panel.width + margin;
    else p.bottom = map.height - panel.top + margin;
  }
  const capW = map.width * 0.8;
  const capH = map.height * 0.8;
  if (p.left + p.right > capW) {
    const k = capW / (p.left + p.right);
    p.left = Math.floor(p.left * k);
    p.right = Math.floor(p.right * k);
  }
  if (p.top + p.bottom > capH) {
    const k = capH / (p.top + p.bottom);
    p.top = Math.floor(p.top * k);
    p.bottom = Math.floor(p.bottom * k);
  }
  return p;
}
