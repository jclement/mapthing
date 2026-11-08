#!/usr/bin/env bash
# Build a Valhalla graph for one Geofabrik extract with mapthing's overrides, for local
# development and for recording test fixtures. The production build lives in internal/build;
# this script is the reference for the steps and flags it should run.
#
#   docker/valhalla/dev-build.sh [geofabrik-path]     default: north-america/us/delaware
#
# Output: data/valhalla/<name>/ (tiles.tar, admin.sqlite, tz_world.sqlite, valhalla.json).
# Serve it with docker/valhalla/dev-serve.sh (in its own terminal tab; it stays up).
# Heavy: run as `heavy docker/valhalla/dev-build.sh`.
set -euo pipefail

IMAGE=${VALHALLA_IMAGE:-ghcr.io/valhalla/valhalla:3.9.1}
REGION=${1:-north-america/us/delaware}
NAME=$(basename "$REGION")
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OUT=$ROOT/data/valhalla/$NAME
mkdir -p "$OUT"

PBF=$OUT/$NAME-latest.osm.pbf
if [[ ! -s $PBF ]]; then
  curl -fL --retry 3 -o "$PBF.part" "https://download.geofabrik.de/$REGION-latest.osm.pbf"
  mv "$PBF.part" "$PBF"
fi

# Base config from the image, then mapthing's overrides, then this build's paths.
docker run --rm "$IMAGE" valhalla_build_config >"$OUT/base.json"
jq -s '.[0] * .[1] * .[2]' "$OUT/base.json" "$ROOT/docker/valhalla/overrides.json" - >"$OUT/valhalla.json" <<'JSON'
{"mjolnir": {
  "tile_dir": "/v/tiles",
  "tile_extract": "/v/tiles.tar",
  "traffic_extract": "/v/traffic.tar",
  "admin": "/v/admin.sqlite",
  "timezone": "/v/tz_world.sqlite",
  "landmarks": "/v/landmarks.sqlite",
  "transit_dir": "/v/transit",
  "transit_feeds_dir": "/v/transit_feeds",
  "logging": {"type": "std_out", "color": false}
}}
JSON

run() { docker run --rm --user "$(id -u):$(id -g)" -v "$OUT:/v" -w /v "$IMAGE" "$@"; }
pbf=/v/$(basename "$PBF")

# Timezones: downloaded from timezone-boundary-builder by the tool itself; optional (only
# time-dependent routing needs it), so a failure is reported and the build goes on.
if [[ ! -s $OUT/tz_world.sqlite ]]; then
  run sh -c 'valhalla_build_timezones > /v/tz_world.sqlite' || { echo "timezones: build failed, continuing without"; rm -f "$OUT/tz_world.sqlite"; }
fi
run valhalla_build_admins --config /v/valhalla.json "$pbf"
rm -rf "$OUT/tiles"
run valhalla_build_tiles --config /v/valhalla.json "$pbf"
run valhalla_build_extract --config /v/valhalla.json --overwrite
rm -rf "$OUT/tiles"   # the extract replaces the tile tree
ls -la "$OUT"
