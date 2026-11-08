#!/usr/bin/env bash
# Sample rendered raster tile sizes from a running mapthing (or renderer) for the raster-cache
# growth model. Fetches N random tiles per zoom inside a bbox and appends one row per
# (style, size, zoom) with mean / median / p90 bytes and mean render time to docs/sizing/raster.csv.
#
#   scripts/measure-raster.sh <base-url> <label> <w,s,e,n> [style ...]
#
# <base-url> is the prefix under which `{style}/{size}/{z}/{x}/{y}[@2x].png` lives: mapthing's
# `http://host:8080/styles`, or a bare renderer's `http://127.0.0.1:8081/render`. For a keyed
# server set RASTER_QS="?key=..." in the environment (never commit a key).
# Env: RASTER_N (tiles per zoom, default 20), RASTER_ZOOMS (default "8 10 12 14 16"),
#      RASTER_SIZES (default "512@2x 256"), RASTER_CSV.
set -euo pipefail
cd "$(dirname "$0")/.."
BASE=${1:?usage: $0 base-url label w,s,e,n [style...]}
LABEL=${2:?}
BBOX=${3:?}
shift 3
STYLES=(outdoors)
[[ $# -gt 0 ]] && STYLES=("$@")
N=${RASTER_N:-20}
ZOOMS=${RASTER_ZOOMS:-8 10 12 14 16}
SIZES=${RASTER_SIZES:-512@2x 256}
CSV=${RASTER_CSV:-docs/sizing/raster.csv}
QS=${RASTER_QS:-}
[[ -s $CSV ]] || echo "label,style,size,zoom,n,mean_bytes,median_bytes,p90_bytes,mean_ms" > "$CSV"

IFS=, read -r W S E NN <<<"$BBOX"
for style in "${STYLES[@]}"; do
  for size in $SIZES; do
    px=${size%@2x}; hi=""; [[ $size == *@2x ]] && hi="@2x"
    for z in $ZOOMS; do
      tiles=$(python3 - "$W" "$S" "$E" "$NN" "$z" "$N" <<'EOF'
import math, random, sys
w, s, e, n, z, k = map(float, sys.argv[1:]); z, k = int(z), int(k)
def tx(lon): return int((lon + 180) / 360 * 2**z)
def ty(lat): return int((1 - math.asinh(math.tan(math.radians(lat))) / math.pi) / 2 * 2**z)
xs, ys = range(tx(w), tx(e) + 1), range(ty(n), ty(s) + 1)
random.seed(f"{w}{s}{z}")
cells = [(x, y) for x in xs for y in ys]
for x, y in random.sample(cells, min(k, len(cells))): print(x, y)
EOF
)
      sizes=() times=()
      while read -r x y; do
        r=$(curl -fsS -o /dev/null -w '%{size_download} %{time_total}' "$BASE/$style/$px/$z/$x/$y$hi.png$QS" || echo "0 0")
        sizes+=("${r% *}"); times+=("${r#* }")
      done <<<"$tiles"
      stats=$(printf '%s\n' "${sizes[@]}" | sort -n | awk -v t="$(printf '%s ' "${times[@]}")" '
        {a[NR]=$1; s+=$1} END{split(t,tt," "); for(i in tt) ts+=tt[i];
        printf "%d,%.0f,%.0f,%.0f,%.0f", NR, s/NR, a[int((NR+1)/2)], a[int(NR*0.9)>0?int(NR*0.9):1], ts/NR*1000}')
      echo "$LABEL,$style,$size,$z,$stats" | tee -a "$CSV" >&2
    done
  done
done
