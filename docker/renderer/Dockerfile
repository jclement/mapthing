# Standalone image for developing and testing the renderer. The shipped image is built by
# docker/Dockerfile (pipeline); README.md lists what that image needs from here.
#
#   docker build -t mapthing-renderer:dev docker/renderer
#   docker run --rm --network host -e MAPTHING_INTERNAL_TOKEN=... mapthing-renderer:dev
FROM oven/bun:1.4.4 AS bun
FROM node:24.11.1-bookworm-slim AS node

FROM ubuntu:24.04
# The maplibre-gl-native prebuilt for linux links GLX/OpenGL, libuv, libcurl, ICU 74 and
# libjpeg-turbo 8: Ubuntu 24.04's versions. Rendering is software (Mesa llvmpipe) on an Xvfb display.
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates xvfb xauth \
      libopengl0 libglx0 libglx-mesa0 libgl1-mesa-dri libegl1 \
      libcurl4t64 libjpeg-turbo8 libuv1t64 libwebp7 libicu74 libpng16-16t64 libx11-6 libxext6 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun
COPY --from=node /usr/local/bin/node /usr/local/bin/node
WORKDIR /opt/renderer
COPY package.json bun.lock ./
RUN bun install --frozen-lockfile --production
COPY server.js lib.js start.sh ./
ENV RENDERER_LISTEN=127.0.0.1:8081 MAPTHING_URL=http://127.0.0.1:8080
CMD ["./start.sh"]
