#!/usr/bin/env bash
# Measure a terrain (elevation) build for one bounding box: wall time, peak container memory,
# max-zoom tile count, archive size and DEM bytes fetched into the cache. Appends a row to
# docs/sizing/terrain.csv.
#
#   scripts/measure-terrain.sh <label> <w,s,e,n> [maxzoom]
#
# Runs the elevation worker's `mt-terrain build` (MEASURE_TERRAIN_BIN, a static Linux binary)
# inside the mapthing image, so GDAL is the one the server uses. Each run starts with an empty
# DEM cache, so "cache_bytes" is what a first build downloads for that box. With MEASURE_WARM=1
# it then builds the same box again on the warm cache (what a scheduled refresh costs) and
# records a second row noted "warm cache".
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$(pwd)
LABEL=${1:?usage: $0 label w,s,e,n [maxzoom]}
BBOX=${2:?usage: $0 label w,s,e,n [maxzoom]}
MAXZOOM=${3:-0}
IMAGE=${MEASURE_IMAGE:-mapthing:dev}
BIN=${MEASURE_TERRAIN_BIN:-$ROOT/data/bin/mt-terrain}
DATA=${MEASURE_DATA:-$ROOT/data/measure-terrain}
CSV=${MEASURE_TERRAIN_CSV:-$ROOT/docs/sizing/terrain.csv}
MIN_FREE_GB=${MEASURE_MIN_FREE_GB:-6}
NAME=mapthing-terrain-$$
[[ -x $BIN ]] || { echo "mt-terrain binary not found at $BIN" >&2; exit 1; }

mkdir -p "$DATA"; chmod 0777 "$DATA"
cleanup() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker run --rm -v "$DATA:/d" --entrypoint sh "$IMAGE" -c 'rm -rf /d/*' >/dev/null 2>&1 || true
}
trap cleanup EXIT
(( $(df -Pk "$DATA" | awk 'NR==2{print $4}') > MIN_FREE_GB * 1048576 )) || { echo "below ${MIN_FREE_GB} GiB free" >&2; exit 3; }

run_once() {
  args=(build -bbox "$BBOX" -id "$LABEL" -out /data/terrain.pmtiles -work /data/work -cache /data/cache)
  (( MAXZOOM > 0 )) && args+=(-maxzoom "$MAXZOOM")
  start=$(date +%s.%N)
  docker run -d --name "$NAME" -v "$DATA:/data" -v "$BIN:/usr/local/bin/mt-terrain:ro" \
    --entrypoint /usr/local/bin/mt-terrain "$IMAGE" "${args[@]}" >/dev/null
  CG=/sys/fs/cgroup/system.slice/docker-$(docker inspect -f '{{.Id}}' "$NAME").scope
  peak_mem=0 peak_du=0
  while docker inspect -f '{{.State.Running}}' "$NAME" 2>/dev/null | grep -q true; do
    m=$(cat "$CG/memory.peak" 2>/dev/null || echo 0); (( m > peak_mem )) && peak_mem=$m
    d=$(du -sk "$DATA" 2>/dev/null | cut -f1); (( ${d:-0} > peak_du )) && peak_du=$d
    sleep 2
  done
  wall=$(echo "$(date +%s.%N) - $start" | bc)
  code=$(docker inspect -f '{{.State.ExitCode}}' "$NAME")
  log=$(docker logs "$NAME" 2>&1)
  mkdir -p "$ROOT/data/logs"; echo "$log" > "$ROOT/data/logs/terrain-$LABEL.log"
  [[ $code == 0 ]] || { echo "$log" | tail -20 >&2; exit 1; }
  docker rm -f "$NAME" >/dev/null 2>&1 || true

  # "terrain: 1 region(s), max zoom 13, 1234 max-zoom tiles (budget ...)"
  z=$(sed -n 's/.*max zoom \([0-9]*\), \([0-9]*\) max-zoom tiles.*/\1/p' <<<"$log" | head -1)
  n=$(sed -n 's/.*max zoom \([0-9]*\), \([0-9]*\) max-zoom tiles.*/\2/p' <<<"$log" | head -1)
  # "terrain: wrote ...: N tiles, X MB, z8–13, sources: 3dep13 98.5%, glo30 1.5%, pieces fetched ..."
  src=$(sed -n 's/.*sources: \(.*\), pieces fetched.*/\1/p' <<<"$log" | tail -1 | sed 's/, /+/g; s/ /:/g')
  arch=$(stat -c %s "$DATA/terrain.pmtiles")
  cache=$(( $(du -sk "$DATA/cache" | cut -f1) * 1024 ))
  [[ -s $CSV ]] || echo "date,label,bbox,source,maxzoom,maxzoom_tiles,archive_bytes,cache_bytes,peak_disk_bytes,t_s,peak_rss_mb,cores,note" > "$CSV"
  echo "$(date -u +%FT%TZ),$LABEL,${BBOX//,/ },${MEASURE_SOURCE:-${src:-unknown}},$z,$n,$arch,$cache,$((peak_du * 1024)),$wall,$((peak_mem / 1048576)),$(nproc),$1" >> "$CSV"
  tail -1 "$CSV" >&2
}
run_once "${MEASURE_NOTE:-cold cache}"
if [[ ${MEASURE_WARM:-0} == 1 ]]; then
  docker run --rm -v "$DATA:/d" --entrypoint sh "$IMAGE" -c 'rm -rf /d/work /d/terrain.pmtiles*' >/dev/null 2>&1 || true
  run_once "warm cache"
fi
