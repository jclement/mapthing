# syntax=docker/dockerfile:1.10
#
# mapthing: one container with the Go server, Valhalla, Planetiler (Protomaps basemap profile),
# osmium, GDAL and the MapLibre Native raster renderer.
#
#   docker build -t mapthing:dev -f docker/Dockerfile .
#   docker buildx build --platform linux/amd64,linux/arm64 -f docker/Dockerfile .
#
# Base: the official Valhalla image (Ubuntu 24.04, prebuilt for amd64 and arm64). Ubuntu's own
# valhalla-bin is 3.4 (2023); the official image gives a current Valhalla without compiling it,
# which would take hours per architecture under QEMU.
#
# Architecture-independent work (Go cross-compile, web build, Planetiler jar) runs on the build
# platform; only package installs and the renderer's prebuilt download run per target.

ARG VALHALLA_VERSION=3.9.1
ARG GO_VERSION=1.27.3
ARG NODE_VERSION=24.11.1
ARG BUN_VERSION=1.4.4
ARG MAVEN_IMAGE=maven:3.9.11-eclipse-temurin-21
ARG JRE_IMAGE=eclipse-temurin:21-jre-noble
# github.com/protomaps/basemaps, tiles/ at "tiles 4.14.11" plus fixes (2026-08-11).
ARG BASEMAPS_REF=ca93fc06efbaff1c5f069d06edbe5de18839c1c8

# ---------------------------------------------------------------- web UI (build platform)
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-bookworm-slim AS web
WORKDIR /src
# web/ may not exist yet: build it when present, otherwise ship a placeholder page.
RUN --mount=type=bind,source=.,target=/ctx \
    --mount=type=cache,target=/root/.npm \
    mkdir -p /out/dist && \
    if [ -f /ctx/web/package.json ]; then \
      cp -r /ctx/web /src/web && cd /src/web && rm -rf node_modules dist && \
      npm ci --no-audit --no-fund && npm run build && cp -r dist/. /out/dist/ ; \
    else \
      printf '<!doctype html><title>mapthing</title><p>The admin UI was not built into this image.</p>\n' > /out/dist/index.html ; \
    fi

# ---------------------------------------------------------------- Go binaries (cross-compiled)
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS go
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.su[m] ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN rm -rf web/dist && mkdir -p web
COPY --from=web /out/dist web/dist
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p /out && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ ./cmd/...

# ---------------------------------------------------------------- Planetiler + Protomaps profile
FROM --platform=$BUILDPLATFORM ${MAVEN_IMAGE} AS planetiler
ARG BASEMAPS_REF
ADD https://github.com/protomaps/basemaps.git#${BASEMAPS_REF} /basemaps
WORKDIR /basemaps/tiles
RUN --mount=type=cache,target=/root/.m2 \
    mvn -B -q -DskipTests -Dspotless.check.skip=true package && \
    cp target/protomaps-basemap-HEAD-with-deps.jar /protomaps-basemap.jar

# ---------------------------------------------------------------- renderer deps (target arch)
FROM oven/bun:${BUN_VERSION} AS renderer
WORKDIR /opt/renderer
# docker/renderer/ is provided by the style work; skip cleanly when absent.
RUN --mount=type=bind,source=docker,target=/ctx \
    if [ -f /ctx/renderer/package.json ]; then \
      cd /ctx/renderer && \
      find . -path ./node_modules -prune -o -path ./dev -prune -o -type f -print | \
        while read -r f; do mkdir -p "/opt/renderer/$(dirname "$f")" && cp "$f" "/opt/renderer/$f"; done && \
      cd /opt/renderer && bun install --frozen-lockfile --production ; \
    fi

FROM node:${NODE_VERSION}-bookworm-slim AS node
FROM ${JRE_IMAGE} AS jre

# ---------------------------------------------------------------- final image
FROM ghcr.io/valhalla/valhalla:${VALHALLA_VERSION}
ARG VERSION=dev
ARG REVISION=unknown
ARG CREATED=unknown
LABEL org.opencontainers.image.title="mapthing" \
      org.opencontainers.image.description="Self-hosted map service: vector and raster tiles, offline packs, routing and elevation in one container" \
      org.opencontainers.image.source="https://github.com/jclement/mapthing" \
      org.opencontainers.image.url="https://github.com/jclement/mapthing" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.authors="" \
      org.opencontainers.image.vendor=""

# tini (PID 1), osmium, GDAL for the elevation build, and what MapLibre Native needs to render in
# software on an Xvfb display (GLX + Mesa llvmpipe, libuv, libcurl, ICU 74, libjpeg-turbo 8).
RUN export DEBIAN_FRONTEND=noninteractive && apt-get update && \
    apt-get install -y --no-install-recommends \
      tini ca-certificates osmium-tool gdal-bin \
      xvfb xauth libopengl0 libglx0 libglx-mesa0 libgl1-mesa-dri libegl1 \
      libcurl4t64 libjpeg-turbo8 libuv1t64 libwebp7 libicu74 libpng16-16t64 libx11-6 libxext6 && \
    apt-get clean && rm -rf /var/lib/apt/lists/* /usr/share/doc/* /usr/share/man/*

COPY --from=jre /opt/java/openjdk /opt/java/openjdk
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=planetiler /protomaps-basemap.jar /opt/mapthing/protomaps-basemap.jar
COPY --from=renderer /opt/renderer /opt/renderer
COPY --from=go /out/ /usr/local/bin/

# Routing overrides (docker/valhalla/, routing work) and style assets, when present.
RUN --mount=type=bind,source=.,target=/ctx \
    mkdir -p /usr/share/mapthing/valhalla && \
    if [ -d /ctx/docker/valhalla ]; then cp -r /ctx/docker/valhalla/. /usr/share/mapthing/valhalla/; fi && \
    if [ -d /ctx/assets ]; then cp -r /ctx/assets /usr/share/mapthing/assets; fi

ENV JAVA_HOME=/opt/java/openjdk \
    PATH=/opt/java/openjdk/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    MAPTHING_DATA=/data \
    MAPTHING_LISTEN=:8080 \
    MAPTHING_PLANETILER_JAR=/opt/mapthing/protomaps-basemap.jar \
    MAPTHING_VALHALLA_OVERRIDES=/usr/share/mapthing/valhalla/overrides.json \
    MAPTHING_RENDERER_DIR=/opt/renderer \
    MAPTHING_ASSETS=/usr/share/mapthing/assets

RUN useradd --system --uid 10001 --user-group --home-dir /data --shell /usr/sbin/nologin mapthing && \
    mkdir -p /data && chown mapthing:mapthing /data && \
    java -version 2>&1 | head -1 && osmium --version | head -1 && gdalwarp --version && \
    valhalla_service --version 2>&1 | head -1 && node --version

USER mapthing
WORKDIR /data
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD ["mapthing", "healthcheck"]
ENTRYPOINT ["/usr/bin/tini", "--", "mapthing"]
