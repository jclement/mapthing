# mapthing renderer

Renders mapthing's GL styles to PNG raster tiles with MapLibre Native
(`@maplibre/maplibre-gl-native` 6.4.1) and `sharp`. mapthing supervises it and proxies
`/styles/{name}/{size}/{z}/{x}/{y}[@2x].png` to it.

```
GET  /render/{style}/{size}/{z}/{x}/{y}[@2x].png   size 256|512; @2x = pixel ratio 2 (512/1024 px)
GET  /healthz                                       {"ok":true,"runtime":...,"rendered":n,"pools":[...]}
POST /reload                                        drop cached styles and renderers (after a generation swap)
```

Binds `127.0.0.1:8081`. It fetches `GET $MAPTHING_URL/styles/{style}.json?internal=1`, then every
tile, glyph and sprite the style names, all with `X-Mapthing-Internal: $MAPTHING_INTERNAL_TOKEN`.
Any absolute URL whose path starts with `/tiles/`, `/fonts/`, `/sprites/` or `/styles/` is sent
to `MAPTHING_URL` whatever host the style used, so the style may carry the public URL. A 204 or
404 for a resource means "no data" (a blank area), not an error.

| Env | Default | |
|---|---|---|
| `MAPTHING_URL` | `http://127.0.0.1:8080` | mapthing's internal base |
| `MAPTHING_INTERNAL_TOKEN` | (empty: warns) | shared secret, same value mapthing has |
| `RENDERER_LISTEN` | `127.0.0.1:8081` | |
| `RENDERER_POOL` | `2` | MapLibre Native maps per style and pixel ratio |
| `RENDERER_STYLE_TTL` | `300` | seconds before a style is fetched again |
| `RENDERER_DISPLAY` | `99` | Xvfb display number used by `start.sh` |

Tiles render in MapLibre Native's `tile` mode: a 512 tile at z renders at zoom z, a 256 tile at
zoom z-1 (z0 at 256 renders 512 and is scaled down). Measured on a 10-core x86 test machine with the dev stand-in:
~100–300 ms per 512 tile once warm, 1–2 s for the first tile of a style or for satellite tiles
(upstream imagery fetch).

## Runtime: Node, not Bun

Bun was tried first (one runtime less in the image). It cannot load the addon:
`@maplibre/maplibre-gl-native` is built on NAN (the V8 C++ API, 58 `v8::` imports, no N-API)
and links libuv directly. Under Bun 1.4.4 `require` fails at once (after also working around
the ABI path: Bun reports `process.versions.modules` 147, the package only ships 115/127/137):

```
error: .../lib/node-v137/mbgl.node: undefined symbol:
  _ZN2v84JSON9StringifyENS_5LocalINS_7ContextEEENS1_INS_5ValueEEENS1_INS_6StringEEE
  code: "ERR_DLOPEN_FAILED"
```

So the renderer runs on **Node 24** (ABI 137 prebuilt). The pure helpers (`lib.js`) and their
tests run under Bun or Node; the dev tools in `dev/` use Bun.

## What the mapthing image needs (for docker/Dockerfile)

The prebuilt `mbgl.node` (linux x64 and arm64 both published) links GLX/OpenGL, libuv, libcurl,
**ICU 74** and libjpeg-turbo 8, which is **Ubuntu 24.04**. On a Debian base the ICU/libjpeg
sonames don't match. Rendering is software (Mesa llvmpipe) on a private Xvfb display; no GPU.

```dockerfile
# runtime stage, ubuntu:24.04
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates xvfb xauth \
      libopengl0 libglx0 libglx-mesa0 libgl1-mesa-dri libegl1 \
      libcurl4t64 libjpeg-turbo8 libuv1t64 libwebp7 libicu74 libpng16-16t64 libx11-6 libxext6 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=node:24.11.1-bookworm-slim /usr/local/bin/node /usr/local/bin/node
# deps: either `npm ci --omit=dev` with node, or `bun install --frozen-lockfile --production`
# (bun.lock is committed; the addon's postinstall downloads the prebuilt with node-pre-gyp,
# so node must be on PATH during install either way)
COPY docker/renderer/package.json docker/renderer/bun.lock /opt/renderer/
RUN cd /opt/renderer && bun install --frozen-lockfile --production
COPY docker/renderer/server.js docker/renderer/lib.js docker/renderer/start.sh /opt/renderer/
```

Start command (what the supervisor runs): `/opt/renderer/start.sh` with `MAPTHING_URL`,
`MAPTHING_INTERNAL_TOKEN` (and optionally `RENDERER_*`) in the environment. `start.sh` starts
Xvfb on `:$RENDERER_DISPLAY`, then `node server.js`, and exits when either dies so the
supervisor restarts the pair. (`xvfb-run` is not used: it hangs as PID 1 in a container.)
Health: `GET http://127.0.0.1:8081/healthz`. After swapping a generation, `POST /reload` (or
restart it).

`Dockerfile` in this directory builds a standalone renderer image the same way, for
development: `docker build -t mapthing-renderer:dev docker/renderer`.

## Development without core

`dev/devserver.js` is a stand-in for mapthing (styles with placeholders filled, fonts, sprites,
basemap tiles from local PMTiles, terrain and imagery proxied from public sources):

```sh
(cd docker/renderer/dev && bun install)
bun docker/renderer/dev/devserver.js data/delaware.pmtiles      # in its own terminal, :8080 (DEV_PORT)
docker run --rm --network host -e MAPTHING_INTERNAL_TOKEN=dev mapthing-renderer:dev
bun docker/renderer/dev/screens.js http://127.0.0.1:8081 data/screens
```

A small PMTiles for testing: `pmtiles extract https://build.protomaps.com/<YYYYMMDD>.pmtiles
data/delaware.pmtiles --bbox=-75.79,38.45,-75.05,39.84` (~50 MB, Range requests only).
