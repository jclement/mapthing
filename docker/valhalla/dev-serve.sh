#!/usr/bin/env bash
# Serve a graph built by dev-build.sh on 127.0.0.1:${PORT:-8002}. Long-running: start it in its
# own terminal (herdr) tab, never in the background of an agent shell.
set -euo pipefail
IMAGE=${VALHALLA_IMAGE:-ghcr.io/valhalla/valhalla:3.9.1}
NAME=${1:-delaware}
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OUT=$ROOT/data/valhalla/$NAME
PORT=${PORT:-8002}
# Inside the container valhalla listens on 127.0.0.1:8002 (the override); host networking makes
# that the host's loopback too, so nothing is exposed beyond localhost.
exec docker run --rm --name "valhalla-dev-$NAME" --network host -v "$OUT:/v:ro" "$IMAGE" \
  valhalla_service /v/valhalla.json 1
