// Development stand-in for mapthing, so the renderer and styles can be exercised before core
// exists. Not shipped. Run with Bun from the repository root:
//
//   bun docker/renderer/dev/devserver.js data/delaware.pmtiles data/permian.pmtiles
//
// Serves on 127.0.0.1:$DEV_PORT (default 8080):
//   /styles/{name}.json          assets/styles/{name}.json with {{BASE_URL}}/{{KEY_QS}} filled in
//   /fonts/{stack}/{range}.pbf   assets/fonts (first font of a comma-separated stack)
//   /sprites/{name}[@2x].{json,png}
//   /tiles/basemap/{z}/{x}/{y}.mvt   from the PMTiles archives given (first one with the tile)
//   /tiles/terrain/{z}/{x}/{y}.png   proxied from the public AWS Terrarium tiles (dev only)
//   /tiles/imagery/{z}/{x}/{y}.jpg   proxied from USGS The National Map imagery (dev only)
// Requests must carry X-Mapthing-Internal when DEV_TOKEN is set, like mapthing's internal path.
import { openSync, readSync, readFileSync, existsSync } from "node:fs";
import { join, resolve } from "node:path";
import { PMTiles } from "pmtiles";

const ROOT = resolve(import.meta.dir, "../../..");
const PORT = Number(process.env.DEV_PORT || 8080);
const TOKEN = process.env.DEV_TOKEN || "";
const BASE = `http://127.0.0.1:${PORT}`;

class FileSource {
  constructor(path) {
    this.path = path;
    this.fd = openSync(path, "r");
  }
  getKey() {
    return this.path;
  }
  async getBytes(offset, length) {
    const buf = Buffer.alloc(length);
    readSync(this.fd, buf, 0, length, offset);
    return { data: buf.buffer.slice(buf.byteOffset, buf.byteOffset + length) };
  }
}

const archives = process.argv.slice(2).map((p) => new PMTiles(new FileSource(resolve(p))));
if (!archives.length) console.warn("no PMTiles archives given: basemap tiles will 404");

const proxied = new Map(); // tiny in-memory cache for the upstream proxies
async function proxy(url, type) {
  if (proxied.has(url)) return new Response(proxied.get(url), { headers: { "Content-Type": type, "Access-Control-Allow-Origin": "*" } });
  const r = await fetch(url, { headers: { "User-Agent": "mapthing-devserver" } });
  if (!r.ok) return new Response(null, { status: r.status === 404 ? 404 : 502 });
  const body = new Uint8Array(await r.arrayBuffer());
  if (proxied.size > 5000) proxied.clear();
  proxied.set(url, body);
  return new Response(body, { headers: { "Content-Type": type, "Access-Control-Allow-Origin": "*" } });
}

Bun.serve({
  hostname: "127.0.0.1",
  port: PORT,
  async fetch(req) {
    const url = new URL(req.url);
    const p = decodeURIComponent(url.pathname);
    if (TOKEN && req.headers.get("X-Mapthing-Internal") !== TOKEN && url.searchParams.get("internal")) {
      return new Response("forbidden", { status: 403 });
    }
    let m;
    if ((m = /^\/styles\/([a-z0-9-]+)\.json$/.exec(p))) {
      const f = join(ROOT, "assets/styles", `${m[1]}.json`);
      if (!existsSync(f)) return new Response("no style", { status: 404 });
      const body = readFileSync(f, "utf8").replaceAll("{{BASE_URL}}", BASE).replaceAll("{{KEY_QS}}", "");
      return new Response(body, { headers: { "Content-Type": "application/json", "Access-Control-Allow-Origin": "*" } });
    }
    if ((m = /^\/fonts\/([^/]+)\/(\d+-\d+)\.pbf$/.exec(p))) {
      const font = m[1].split(",")[0].trim();
      const f = join(ROOT, "assets/fonts", font, `${m[2]}.pbf`);
      if (!existsSync(f)) return new Response(null, { status: 404 });
      return new Response(Bun.file(f), { headers: { "Content-Type": "application/x-protobuf", "Access-Control-Allow-Origin": "*" } });
    }
    if ((m = /^\/sprites\/([a-z0-9-]+(?:@2x)?)\.(json|png)$/.exec(p))) {
      const f = join(ROOT, "assets/sprites", `${m[1]}.${m[2]}`);
      if (!existsSync(f)) return new Response(null, { status: 404 });
      return new Response(Bun.file(f), { headers: { "Access-Control-Allow-Origin": "*" } });
    }
    if ((m = /^\/tiles\/basemap\/(\d+)\/(\d+)\/(\d+)\.mvt$/.exec(p))) {
      const [z, x, y] = m.slice(1).map(Number);
      for (const a of archives) {
        const t = await a.getZxy(z, x, y);
        if (t?.data) {
          // pmtiles' JS reader returns decompressed tile data
          return new Response(t.data, { headers: { "Content-Type": "application/x-protobuf", "Access-Control-Allow-Origin": "*" } });
        }
      }
      return new Response(null, { status: 204, headers: { "Access-Control-Allow-Origin": "*" } });
    }
    if ((m = /^\/tiles\/terrain\/(\d+)\/(\d+)\/(\d+)\.png$/.exec(p))) {
      const [z, x, y] = m.slice(1);
      return proxy(`https://s3.amazonaws.com/elevation-tiles-prod/terrarium/${z}/${x}/${y}.png`, "image/png");
    }
    if ((m = /^\/tiles\/imagery\/(\d+)\/(\d+)\/(\d+)\.jpg$/.exec(p))) {
      const [z, x, y] = m.slice(1);
      return proxy(`https://basemap.nationalmap.gov/arcgis/rest/services/USGSImageryOnly/MapServer/tile/${z}/${y}/${x}`, "image/jpeg");
    }
    return new Response("not found", { status: 404 });
  },
});
console.log(`devserver on ${BASE} (${archives.length} archive(s))`);
