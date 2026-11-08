#!/usr/bin/env bash
# Measure a mapthing build: wall time per stage, peak memory, peak disk and output sizes for one
# region (or a set of regions built together), appended as one row to docs/sizing/measurements.csv.
#
#   scripts/measure-build.sh north-america/us/delaware
#   scripts/measure-build.sh -l "texas+louisiana" north-america/us/texas north-america/us/louisiana
#
# It runs `mapthing-build` inside the mapthing image (the same pipeline the server runs as a job)
# against a scratch data directory, and samples from the host while it runs:
#   - disk: `du` of the data directory every MEASURE_INTERVAL seconds (peak, and peak above the
#     baseline of cached downloads and Planetiler sources);
#   - memory: the container's cgroup memory.peak / memory.current (whole container, page cache
#     included) and the per-stage peak RSS the build records in its manifest.
#
# Environment:
#   MEASURE_IMAGE      image to run (default mapthing:dev)
#   MEASURE_DATA       scratch data dir (default ./data/measure). Planetiler's auxiliary sources
#                      (water/land polygons, Natural Earth, ...) are cached here across runs.
#   MEASURE_CSV        output CSV (default docs/sizing/measurements.csv)
#   MEASURE_MIN_FREE_GB  refuse to start, and abort a running build, if free space on the data
#                      volume would drop below this (default 6)
#   MEASURE_KEEP       1: keep the generation and downloads after measuring (default: delete them,
#                      keep only the Planetiler sources cache)
#   MEASURE_INTERVAL   sampling interval in seconds (default 5)
#   MEASURE_CPU_SHARES docker --cpu-shares (default 512: yields to other work on a shared box
#                      without capping the build when the machine is idle)
#   MEASURE_NOTE       free-text note recorded in the CSV row
#   MEASURE_AFTER      command run after a successful build, before cleanup, with the built
#                      basemap.pmtiles path and the label as arguments (e.g. to measure pack sizes)
#
# Needs: docker, python3 (for the region area from the Geofabrik index), jq.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)
IMAGE=${MEASURE_IMAGE:-mapthing:dev}
DATA=${MEASURE_DATA:-$ROOT/data/measure}
CSV=${MEASURE_CSV:-$ROOT/docs/sizing/measurements.csv}
MIN_FREE_GB=${MEASURE_MIN_FREE_GB:-6}
INTERVAL=${MEASURE_INTERVAL:-5}
SHARES=${MEASURE_CPU_SHARES:-512}
LABEL=""

while getopts "l:" o; do
  case $o in
    l) LABEL=$OPTARG ;;
    *) echo "usage: $0 [-l label] region [region...]" >&2; exit 2 ;;
  esac
done
shift $((OPTIND - 1))
[[ $# -ge 1 ]] || { echo "usage: $0 [-l label] region [region...]" >&2; exit 2; }
REGIONS=("$@")
[[ -n $LABEL ]] || LABEL=$(IFS=+; echo "${REGIONS[*]##*/}")

for t in docker python3 jq; do command -v $t >/dev/null || { echo "$t is not installed" >&2; exit 1; }; done

log() { printf '\033[1m[measure]\033[0m %s\n' "$*" >&2; }
free_kb() { df -Pk "$DATA" | awk 'NR==2{print $4}'; }

mkdir -p "$DATA" "$(dirname "$CSV")"
chmod 0777 "$DATA"
RUN=$(mktemp -d "${TMPDIR:-/tmp}/measure.XXXXXX")
NAME=mapthing-measure-$$

cleanup() {
  local code=$?
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  [[ -n ${SAMPLER:-} ]] && kill "$SAMPLER" 2>/dev/null || true
  if [[ ${MEASURE_KEEP:-0} != 1 ]]; then
    # Files belong to the container user: remove from inside a container. Keep sources/ (the
    # Planetiler auxiliary data and the timezone DB), which every build reuses.
    docker run --rm -v "$DATA:/d" --entrypoint sh "$IMAGE" -c \
      'rm -rf /d/generations /d/work /d/downloads /d/current /d/catalog* /d/mapthing.db' >/dev/null 2>&1 || true
  fi
  rm -rf "$RUN"
  exit $code
}
trap cleanup EXIT

# ------------------------------------------------------------------ region facts (Geofabrik index)
INDEX=$DATA/../research/index-v1.json
if [[ ! -s $INDEX ]]; then
  mkdir -p "$(dirname "$INDEX")"
  curl -fsS -o "$INDEX" https://download.geofabrik.de/index-v1.json
fi
read -r AREA_KM2 PBF_URLS < <(python3 - "$INDEX" "${REGIONS[@]}" <<'EOF'
import json, math, sys
idx = json.load(open(sys.argv[1]))
byid = {f["properties"]["id"]: f for f in idx["features"]}
R = 6371.0088
def ring_area(ring):
    # spherical excess approximation (good to <0.5% at extract scale)
    a = 0.0
    for (x1, y1), (x2, y2) in zip(ring, ring[1:] + ring[:1]):
        a += math.radians(x2 - x1) * (2 + math.sin(math.radians(y1)) + math.sin(math.radians(y2)))
    return abs(a) * R * R / 2
total, urls = 0.0, []
for rid in sys.argv[2:]:
    short = rid.split("/")[-1] if rid.split("/")[-1] in byid else rid
    key = next((k for k in (rid, rid.replace("north-america/", ""), rid.split("/")[-1]) if k in byid), None)
    if key is None:
        sys.exit(f"region {rid} not in Geofabrik index")
    f = byid[key]
    g = f["geometry"]
    polys = g["coordinates"] if g["type"] == "MultiPolygon" else [g["coordinates"]]
    for p in polys:
        total += ring_area(p[0]) - sum(ring_area(h) for h in p[1:])
    urls.append(f["properties"]["urls"]["pbf"])
print(round(total), ",".join(urls))
EOF
)
PBF_BYTES=0
for u in ${PBF_URLS//,/ }; do
  s=$(curl -fsSIL "$u" | awk 'tolower($1)=="content-length:"{v=$2} END{print v}' | tr -d '\r')
  PBF_BYTES=$((PBF_BYTES + ${s:-0}))
done
log "$LABEL: extract polygon area ${AREA_KM2} km², PBF $((PBF_BYTES / 1048576)) MiB"

# ------------------------------------------------------------------ preflight
have_sources=0
[[ -s $DATA/sources/planetiler/water-polygons-split-3857.zip ]] && have_sources=1
need_kb=$(( PBF_BYTES * 6 / 1024 + (have_sources ? 0 : 4 * 1048576) ))
free=$(free_kb)
if (( free - need_kb < MIN_FREE_GB * 1048576 )); then
  log "refusing: free $((free / 1048576)) GiB, estimated need $((need_kb / 1048576)) GiB, floor ${MIN_FREE_GB} GiB"
  exit 3
fi
log "free $((free / 1048576)) GiB, estimated need $((need_kb / 1048576)) GiB"

CPU_MODEL=$(awk -F': ' '/model name/{print $2; exit}' /proc/cpuinfo)
CORES=$(nproc)
RAM_GB=$(awk '/MemTotal/{printf "%.0f", $2/1048576}' /proc/meminfo)
DISK=$(lsblk -dno ROTA,MODEL "$(df -P "$DATA" | awk 'NR==2{print $1}' | sed 's/[0-9]*$//')" 2>/dev/null | awk '{t=($1==0)?"ssd/virtual":"hdd"; $1=""; print t":"$0}' | xargs || echo unknown)
LOAD0=$(cut -d' ' -f1 /proc/loadavg)
BASE_DU=$(du -sk "$DATA" 2>/dev/null | cut -f1)

# ------------------------------------------------------------------ run
args=()
for r in "${REGIONS[@]}"; do args+=(-region "$r"); done
log "running mapthing-build ${args[*]}"
start=$(date +%s)
docker run -d --name "$NAME" --cpu-shares "$SHARES" -v "$DATA:/data" \
  --entrypoint /usr/bin/tini "$IMAGE" -- mapthing-build -data /data "${args[@]}" -refresh >/dev/null
CID=$(docker inspect -f '{{.Id}}' "$NAME")
CG=/sys/fs/cgroup/system.slice/docker-$CID.scope

# Sampler: peak du, peak cgroup memory, abort on low disk.
(
  peak_du=0 peak_mem=0
  while docker inspect -f '{{.State.Running}}' "$NAME" 2>/dev/null | grep -q true; do
    d=$(du -sk "$DATA" 2>/dev/null | cut -f1); d=${d:-0}
    (( d > peak_du )) && peak_du=$d
    m=0
    [[ -r $CG/memory.peak ]] && m=$(cat "$CG/memory.peak")
    [[ $m == 0 && -r $CG/memory.current ]] && m=$(cat "$CG/memory.current")
    (( m > peak_mem )) && peak_mem=$m
    echo "$peak_du $peak_mem" > "$RUN/peaks"
    echo "$(date +%s) $d $m" >> "$RUN/samples"
    if (( $(free_kb) < MIN_FREE_GB * 1048576 )); then
      echo "free space below ${MIN_FREE_GB} GiB: aborting" > "$RUN/aborted"
      docker kill "$NAME" >/dev/null 2>&1 || true
    fi
    sleep "$INTERVAL"
  done
) &
SAMPLER=$!

docker wait "$NAME" >/dev/null
wait "$SAMPLER" 2>/dev/null || true
SAMPLER=
WALL=$(( $(date +%s) - start ))
CODE=$(docker inspect -f '{{.State.ExitCode}}' "$NAME")
docker logs "$NAME" > "$RUN/log" 2>&1 || true
mkdir -p "$DATA/../logs"
cp "$RUN/log" "$DATA/../logs/$LABEL-$(date +%Y%m%dT%H%M%S).log"
[[ -f $RUN/aborted ]] && { log "$(cat "$RUN/aborted")"; exit 4; }
[[ $CODE == 0 ]] || { log "mapthing-build exited $CODE"; tail -40 "$RUN/log" >&2; exit 1; }

read -r PEAK_DU PEAK_MEM < "$RUN/peaks"
# The manifest is the pretty-printed JSON object at the end of the log.
python3 - "$RUN/log" > "$RUN/manifest.json" <<'EOF'
import json, sys
text = open(sys.argv[1]).read()
i = text.rfind('\n{\n')
print(json.dumps(json.loads(text[i + 1:])))
EOF
M=$RUN/manifest.json
stage() { jq -r --arg n "$1" --arg f "$2" '[.stages[] | select(.name == $n)][0][$f] // 0' "$M"; }
art() { jq -r --arg n "$1" '.artifacts[$n] // 0' "$M"; }
GEN_KB=$(du -sk "$DATA/current/" 2>/dev/null | cut -f1)
LOAD1=$(cut -d' ' -f1 /proc/loadavg)

if [[ ! -s $CSV ]]; then
  echo "date,label,regions,pbf_bytes,area_km2,t_download_s,t_merge_s,t_basemap_s,t_valhalla_s,t_terrain_s,t_verify_s,t_total_s,wall_s,rss_basemap_mb,rss_valhalla_mb,rss_terrain_mb,cgroup_mem_peak_mb,peak_disk_mb,peak_disk_above_base_mb,basemap_bytes,valhalla_tar_bytes,terrain_bytes,generation_bytes,sources_cached,cpu,cores,ram_gb,disk,load_start,load_end,image,note" > "$CSV"
fi
row=(
  "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$LABEL" "$(IFS=' '; echo "${REGIONS[*]}")" "$PBF_BYTES" "$AREA_KM2"
  "$(stage download duration_s)" "$(stage merge duration_s)" "$(stage basemap duration_s)"
  "$(stage valhalla duration_s)" "$(stage terrain duration_s)" "$(stage verify duration_s)"
  "$(jq -r '.total_s // 0' "$M")" "$WALL"
  "$(( $(stage basemap peak_rss_kb) / 1024 ))" "$(( $(stage valhalla peak_rss_kb) / 1024 ))" "$(( $(stage terrain peak_rss_kb) / 1024 ))"
  "$(( PEAK_MEM / 1048576 ))" "$(( PEAK_DU / 1024 ))" "$(( (PEAK_DU - BASE_DU) / 1024 ))"
  "$(art basemap.pmtiles)" "$(art valhalla/tiles.tar)" "$(art terrain.pmtiles)" "$(( ${GEN_KB:-0} * 1024 ))"
  "$have_sources" "$CPU_MODEL" "$CORES" "$RAM_GB" "$DISK" "$LOAD0" "$LOAD1" "$IMAGE" "${MEASURE_NOTE:-}"
)
( IFS=,; for i in "${!row[@]}"; do row[$i]=${row[$i]//,/;}; done; echo "${row[*]}" ) >> "$CSV"
cp "$M" "$DATA/../logs/$LABEL-manifest.json"
cp "$RUN/samples" "$DATA/../logs/$LABEL-samples.txt"
log "done in ${WALL}s; row appended to $CSV"
if [[ -n ${MEASURE_AFTER:-} ]]; then
  $MEASURE_AFTER "$DATA/current/basemap.pmtiles" "$LABEL" || log "MEASURE_AFTER failed ($?)"
fi
tail -1 "$CSV" >&2
