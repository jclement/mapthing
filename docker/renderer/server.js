// mapthing raster renderer: renders mapthing's GL styles to raster tiles with MapLibre Native.
//
//   GET /render/{style}/{size}/{z}/{x}/{y}[@2x].{png|jpg|webp}   size 256|512; @2x renders at
//       pixel ratio 2; jpg and webp are encoded at quality 80 (sharp)
//   GET /healthz                                       {"ok":true, ...}
//   POST /reload                                       drop cached styles and renderer pools
//
// It binds to 127.0.0.1 only; mapthing proxies /styles/{name}/{size}/{z}/{x}/{y}[@2x].{ext} here.
// Styles, tiles, glyphs and sprites are fetched from mapthing itself with the shared secret
// header X-Mapthing-Internal: $MAPTHING_INTERNAL_TOKEN.
//
// Environment:
//   MAPTHING_URL             mapthing base URL (default http://127.0.0.1:8080)
//   MAPTHING_INTERNAL_TOKEN  shared secret for internal requests (required in production)
//   RENDERER_LISTEN          host:port (default 127.0.0.1:8081)
//   RENDERER_POOL            renderers per style and pixel ratio (default 2)
//   RENDERER_STYLE_TTL       seconds a loaded style is reused before it is fetched again (default 300)

import http from "node:http";
import { createRequire } from "node:module";
import { classify, degradedResponse, EMPTY_TILE_URL, FORMATS, tileCenter, parseRenderPath, rewriteURL } from "./lib.js";

const require = createRequire(import.meta.url);
const mbgl = require("@maplibre/maplibre-gl-native");
const sharp = require("sharp");

const BASE = (process.env.MAPTHING_URL || "http://127.0.0.1:8080").replace(/\/$/, "");
const TOKEN = process.env.MAPTHING_INTERNAL_TOKEN || "";
const [HOST, PORT] = (() => {
  const l = process.env.RENDERER_LISTEN || "127.0.0.1:8081";
  const i = l.lastIndexOf(":");
  return [l.slice(0, i) || "127.0.0.1", Number(l.slice(i + 1))];
})();
const POOL_SIZE = Math.max(1, Number(process.env.RENDERER_POOL || 2));
const STYLE_TTL_MS = Math.max(0, Number(process.env.RENDERER_STYLE_TTL ?? 300)) * 1000;
const FETCH_TIMEOUT_MS = 30_000;

const log = (level, msg, extra = {}) =>
  process.stdout.write(JSON.stringify({ time: new Date().toISOString(), level, msg, ...extra }) + "\n");

if (!TOKEN) log("WARN", "MAPTHING_INTERNAL_TOKEN is empty; mapthing will refuse internal requests");

// ---------------------------------------------------------------------------------------------
// Resource fetching for MapLibre Native.

async function fetchResource(url) {
  if (url.startsWith(EMPTY_TILE_URL.slice(0, EMPTY_TILE_URL.indexOf("{")))) return null;
  const target = rewriteURL(url, BASE);
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), FETCH_TIMEOUT_MS);
  try {
    const res = await fetch(target, {
      headers: TOKEN ? { "X-Mapthing-Internal": TOKEN } : {},
      signal: ctl.signal,
    });
    if (res.status === 204 || res.status === 404) return null; // no data: tile outside coverage
    if (!res.ok) throw new Error(`GET ${target}: HTTP ${res.status}`);
    return Buffer.from(await res.arrayBuffer());
  } finally {
    clearTimeout(timer);
  }
}

let degradedCount = 0;

// requestFor returns the resource loader of one map. A failed sub-resource degrades (see
// lib.js) instead of failing the render; the map is flagged so its tile is marked degraded
// and mapthing does not cache it.
function requestFor(state) {
  return (req, callback) => {
    fetchResource(req.url).then(
      (data) => {
        if (data) return callback(null, { data });
        // 404/204: the resource does not exist in this generation (e.g. no terrain archive);
        // a source still needs a valid TileJSON, so it gets the empty one. Not degraded.
        const what = classify(req.url, req.kind);
        return what === "source" ? callback(null, { data: degradedResponse("source") }) : callback();
      },
      (err) => {
        const what = classify(req.url, req.kind);
        const stand = degradedResponse(what);
        if (stand === undefined) return callback(err);
        state.degraded = true;
        degradedCount++;
        log("WARN", "resource failed; rendering without it", { kind: what, url: req.url, error: String(err.message || err) });
        return stand ? callback(null, { data: stand }) : callback();
      },
    );
  };
}

// ---------------------------------------------------------------------------------------------
// Style cache and renderer pools, keyed by style name and pixel ratio.

const styles = new Map(); // name -> { json, loaded }
const pools = new Map(); // `${name}@${ratio}` -> { idle: [], all: [], waiters: [], loaded }

async function getStyle(name) {
  const cached = styles.get(name);
  if (cached && Date.now() - cached.loaded < STYLE_TTL_MS) return cached;
  const data = await fetchResource(`${BASE}/styles/${name}.json?internal=1`);
  if (!data) {
    const err = new Error(`style ${name} not found`);
    err.status = 404;
    throw err;
  }
  const entry = { json: JSON.parse(data.toString("utf8")), loaded: Date.now() };
  styles.set(name, entry);
  return entry;
}

function newMap(styleJSON, ratio) {
  const state = { degraded: false };
  const map = new mbgl.Map({ request: requestFor(state), ratio, mode: "tile" });
  map.state = state;
  map.load(styleJSON);
  return map;
}

async function acquire(name, ratio) {
  const style = await getStyle(name);
  const key = `${name}@${ratio}`;
  let pool = pools.get(key);
  if (pool && pool.loaded !== style.loaded) {
    // style was refreshed: retire this pool once its renders finish
    retire(pool);
    pool = undefined;
  }
  if (!pool) {
    pool = { idle: [], all: [], waiters: [], loaded: style.loaded, retired: false, json: style.json };
    pools.set(key, pool);
  }
  if (pool.idle.length) return { pool, map: pool.idle.pop() };
  if (pool.all.length < POOL_SIZE) {
    const map = newMap(pool.json, ratio);
    pool.all.push(map);
    return { pool, map };
  }
  return new Promise((resolve) => pool.waiters.push((map) => resolve({ pool, map })));
}

function release(pool, map, broken) {
  if (broken || pool.retired) {
    pool.all = pool.all.filter((m) => m !== map);
    try {
      map.release();
    } catch {}
    if (!pool.retired && pool.waiters.length) {
      const fresh = newMap(pool.json, map.ratio ?? 1);
      pool.all.push(fresh);
      pool.waiters.shift()(fresh);
    }
    return;
  }
  const next = pool.waiters.shift();
  if (next) next(map);
  else pool.idle.push(map);
}

function retire(pool) {
  pool.retired = true;
  for (const m of pool.idle) {
    try {
      m.release();
    } catch {}
  }
  pool.all = pool.all.filter((m) => !pool.idle.includes(m));
  pool.idle = [];
}

function renderRaw(map, opts) {
  return new Promise((resolve, reject) => map.render(opts, (err, buf) => (err ? reject(err) : resolve(buf))));
}

// Render one tile. MapLibre Native's zoom is in 512 px tile units, so a 512 tile at z renders at
// zoom z, and a 256 tile at z renders 256 px at zoom z-1. z0 at 256 renders 512 px and scales down.
async function renderTile({ style, size, z, x, y, ratio, format }) {
  const center = tileCenter(z, x, y);
  let zoom = size === 512 ? z : z - 1;
  let px = size;
  let downscale = false;
  if (zoom < 0) {
    zoom = 0;
    px = 512;
    downscale = true;
  }
  const { pool, map } = await acquire(style, ratio);
  map.ratio = ratio;
  map.state.degraded = false;
  let raw;
  try {
    raw = await renderRaw(map, { zoom, center, width: px, height: px });
  } catch (err) {
    release(pool, map, true);
    throw err;
  }
  // A failed TileJSON is cached inside the map: drop a degraded map so the next render retries.
  const degraded = map.state.degraded;
  release(pool, map, degraded);
  const w = px * ratio;
  let img = sharp(Buffer.from(raw.buffer, raw.byteOffset, raw.byteLength), { raw: { width: w, height: w, channels: 4 } });
  if (downscale) img = img.resize(size * ratio, size * ratio);
  const encoded = await encode(img, format);
  return { body: encoded, degraded };
}

function encode(img, format) {
  switch (format) {
    case "jpg":
      // Satellite at 512@2x is ~2-2.5 MB as PNG; JPEG q80 is a fraction of that.
      return img.flatten({ background: "#ffffff" }).jpeg({ quality: 80, mozjpeg: true }).toBuffer();
    case "webp":
      return img.webp({ quality: 80 }).toBuffer();
    default:
      return img.png({ compressionLevel: 6, adaptiveFiltering: false }).toBuffer();
  }
}

// ---------------------------------------------------------------------------------------------
// HTTP.

let rendered = 0;
let failures = 0;
const started = Date.now();

function send(res, status, body, type = "application/json") {
  res.writeHead(status, { "Content-Type": type, "Content-Length": Buffer.byteLength(body) });
  res.end(body);
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, "http://localhost");
  if (url.pathname === "/healthz") {
    return send(
      res,
      200,
      JSON.stringify({
        ok: true,
        runtime: typeof Bun !== "undefined" ? `bun ${Bun.version}` : `node ${process.versions.node}`,
        uptime_s: Math.round((Date.now() - started) / 1000),
        rendered,
        failures,
        degraded_resources: degradedCount,
        styles: [...styles.keys()],
        pools: [...pools.entries()].map(([k, p]) => ({ key: k, size: p.all.length, idle: p.idle.length, waiting: p.waiters.length })),
      }),
    );
  }
  if (url.pathname === "/reload" && req.method === "POST") {
    styles.clear();
    for (const p of pools.values()) retire(p);
    pools.clear();
    log("INFO", "reloaded");
    return send(res, 200, '{"ok":true}');
  }
  if (req.method !== "GET" && req.method !== "HEAD") return send(res, 405, '{"error":"method not allowed"}');
  const params = parseRenderPath(url.pathname);
  if (!params) return send(res, 404, '{"error":"not found"}');
  const t0 = Date.now();
  try {
    const { body, degraded } = await renderTile(params);
    rendered++;
    const headers = { "Content-Type": FORMATS[params.format], "Content-Length": body.length, "Cache-Control": "public, max-age=86400" };
    if (degraded) {
      // drawn without one or more sources: serve it, but nobody should cache it
      headers["Cache-Control"] = "no-store";
      headers["X-Mapthing-Degraded"] = "1";
    }
    res.writeHead(200, headers);
    res.end(req.method === "HEAD" ? undefined : body);
    log("DEBUG", "rendered", { path: url.pathname, ms: Date.now() - t0 });
  } catch (err) {
    failures++;
    const status = err.status || 502;
    log("ERROR", "render failed", { path: url.pathname, error: String(err.message || err) });
    send(res, status, JSON.stringify({ error: String(err.message || err) }));
  }
});

server.listen(PORT, HOST, () => log("INFO", "renderer listening", { addr: `${HOST}:${PORT}`, base: BASE, pool: POOL_SIZE }));

for (const sig of ["SIGINT", "SIGTERM"]) {
  process.on(sig, () => {
    server.close();
    for (const p of pools.values()) retire(p);
    process.exit(0);
  });
}
