// Renders 2x2-tile mosaics through a running renderer for visual checks. Not shipped.
//   bun docker/renderer/dev/screens.js [renderer-base] [out-dir]
import { createRequire } from "node:module";
import { mkdirSync } from "node:fs";

const require = createRequire(new URL("../package.json", import.meta.url));
const sharp = require("sharp");

const BASE = process.argv[2] || "http://127.0.0.1:18081";
const OUT = process.argv[3] || "data/screens";
mkdirSync(OUT, { recursive: true });

const PLACES = [
  { name: "dover-de-town", lon: -75.524, lat: 39.158 },
  { name: "permian-lease-roads", lon: -102.25, lat: 31.9 },
  { name: "odessa-tx-town", lon: -102.37, lat: 31.85 },
];
const JOBS = [];
for (const p of PLACES) {
  for (const z of [12, 14, 16]) JOBS.push({ ...p, z, style: "outdoors" });
  JOBS.push({ ...p, z: 14, style: "outdoors-dark" });
  JOBS.push({ ...p, z: 14, style: "satellite" });
  JOBS.push({ ...p, z: 16, style: "satellite" });
}
// hillshade only (no basemap tiles there in the dev extracts): Guadalupe Mountains, TX
JOBS.push({ name: "guadalupe-hillshade", lon: -104.86, lat: 31.9, z: 11, style: "outdoors" });
// the 256 px and @2x paths
JOBS.push({ name: "dover-de-town-256", lon: -75.524, lat: 39.158, z: 13, style: "outdoors", size: 256, hi: true });

function tileXY(lon, lat, z) {
  const n = 2 ** z;
  const x = ((lon + 180) / 360) * n;
  const y = ((1 - Math.asinh(Math.tan((lat * Math.PI) / 180)) / Math.PI) / 2) * n;
  return [x, y];
}

for (const j of JOBS) {
  const size = j.size || 512;
  const [fx, fy] = tileXY(j.lon, j.lat, j.z);
  const x0 = Math.round(fx) - 1;
  const y0 = Math.round(fy) - 1;
  const comps = [];
  const t0 = Date.now();
  let px = size;
  for (let dy = 0; dy < 2; dy++) {
    for (let dx = 0; dx < 2; dx++) {
      const url = `${BASE}/render/${j.style}/${size}/${j.z}/${x0 + dx}/${y0 + dy}${j.hi ? "@2x" : ""}.png`;
      const r = await fetch(url);
      if (!r.ok) throw new Error(`${url}: ${r.status} ${await r.text()}`);
      const buf = Buffer.from(await r.arrayBuffer());
      px = (await sharp(buf).metadata()).width;
      comps.push({ input: buf, left: dx * px, top: dy * px });
    }
  }
  const file = `${OUT}/${j.name}-${j.style}-z${j.z}${j.size ? `-${size}${j.hi ? "@2x" : ""}` : ""}.png`;
  await sharp({ create: { width: px * 2, height: px * 2, channels: 4, background: "#ffffff" } })
    .composite(comps)
    .png()
    .toFile(file);
  console.log(`${file} (${Date.now() - t0} ms)`);
}
