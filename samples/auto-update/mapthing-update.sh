#!/bin/sh
# Opt-in automatic updates for the sample deployment: pull the image and replace the container
# when it changed, but never while a map build is running (replacing the container loses it).
#
# Run it from a timer (mapthing-update.timer here, or cron). It uses nothing but docker compose
# and curl, runs as whoever may run docker on this host, and touches only the mapthing service.
# ./data is a bind mount and is never touched: maps, keys, settings and logs survive.
set -eu

DIR=${MAPTHING_DIR:-/docker/maps}                       # the folder with docker-compose.yaml
HEALTH=${MAPTHING_HEALTH:-http://127.0.0.1:8080/healthz}

cd "$DIR"

# /healthz says {"ok":true,"version":"…","building":true|false} without a login.
state=$(curl -fsS --max-time 10 "$HEALTH" 2>/dev/null || true)
case "$state" in
  *'"building":true'*)
    echo "mapthing is building a map; leaving it alone until the next run"
    exit 0 ;;
esac
# No answer at all: the server is down or still starting. Updating may be what fixes it, and
# there is no build to lose, so carry on.

docker compose pull --quiet mapthing
# Recreates the container only when the image it runs differs from the one just pulled.
docker compose up -d mapthing
