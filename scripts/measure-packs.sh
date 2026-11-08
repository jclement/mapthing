#!/usr/bin/env bash
# Measure offline pack sizes from a built basemap.pmtiles: for each named box and zoom cap, run
# `pmtiles extract --dry-run` (nothing is written) and append the archive size to
# docs/sizing/packs.csv.
#
#   scripts/measure-packs.sh path/to/basemap.pmtiles <label>
#
# Boxes come from PACK_BOXES (one per line, "name w,s,e,n"; "name all" = the whole archive) or,
# when unset, from the defaults below keyed by label. Zoom caps come from PACK_MAXZOOMS
# (default "12 13 14 15"). The basemap stops at z15; clients overzoom beyond it, so a z16 cap
# is the same archive as z15.
#
# Needs the go-pmtiles CLI as `pmtiles` on PATH or at $PMTILES.
set -euo pipefail
cd "$(dirname "$0")/.."
SRC=${1:?usage: $0 basemap.pmtiles label}
LABEL=${2:?usage: $0 basemap.pmtiles label}
PMTILES=${PMTILES:-pmtiles}
CSV=${PACKS_CSV:-docs/sizing/packs.csv}
ZOOMS=${PACK_MAXZOOMS:-12 13 14 15}

default_boxes() {
  # ~40x40 km "county", ~100x150 km "basin", and the whole build ("all").
  case $1 in
    delaware) printf '%s\n' "county -75.79,39.45,-75.45,39.80" "all all" ;;
    gcc-states) printf '%s\n' "city-40km 46.50,24.50,46.90,24.86" "desert-40km 48.00,22.00,48.40,22.36" \
      "basin-100x150km 49.00,24.00,50.00,25.35" "all all" ;;
    louisiana) printf '%s\n' "county-40km -94.00,32.30,-93.60,32.66" "basin-100x150km -94.00,31.80,-93.00,33.15" "all all" ;;
    new-mexico) printf '%s\n' "county-40km -104.40,32.30,-104.00,32.66" "basin-100x150km -104.50,32.00,-103.40,33.35" "all all" ;;
    alberta) printf '%s\n' "city-40km -114.30,50.90,-113.75,51.26" "county-40km -118.90,55.00,-118.27,55.36" \
      "basin-100x150km -119.50,54.50,-118.00,55.85" "all all" ;;
    texas) printf '%s\n' "county-40km -102.30,31.80,-101.88,32.16" "basin-100x150km -102.60,31.30,-101.40,32.65" "all all" ;;
    *) echo "all all" ;;
  esac
}

BOXES=${PACK_BOXES:-$(default_boxes "$LABEL")}
[[ -s $CSV ]] || echo "label,box,bbox,maxzoom,tiles,bytes" > "$CSV"
while read -r name bbox; do
  [[ -n $name ]] || continue
  for z in $ZOOMS; do
    a=(extract "$SRC" /dev/null --dry-run --maxzoom="$z")
    [[ $bbox == all ]] || a+=(--bbox="$bbox")
    out=$("$PMTILES" "${a[@]}" 2>&1)
    tiles=$(sed -n 's/.*result tile entries \([0-9]*\).*/\1/p' <<<"$out" | tail -1)
    size=$(sed -n 's/.*archive size of \([0-9.]*\) \([kMG]*B\).*/\1 \2/p' <<<"$out" | tail -1)
    bytes=$(awk -v s="$size" 'BEGIN{split(s,a," "); m=1; if(a[2]=="kB")m=1e3; if(a[2]=="MB")m=1e6; if(a[2]=="GB")m=1e9; printf "%.0f", a[1]*m}')
    echo "$LABEL,$name,${bbox//,/ },$z,${tiles:-0},$bytes" >> "$CSV"
    printf '%-12s %-18s z%-2s %8s tiles %10s\n' "$LABEL" "$name" "$z" "${tiles:-?}" "$size" >&2
  done
done <<<"$BOXES"
