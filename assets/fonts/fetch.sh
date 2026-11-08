#!/usr/bin/env sh
# Fetch SDF glyph PBFs (Noto Sans, SIL OFL 1.1) from protomaps/basemaps-assets at a pinned commit.
#
#   assets/fonts/fetch.sh [--all] [DEST]
#
# Without --all only the ranges committed to this repository are fetched (Latin, Latin Extended,
# IPA, combining marks, Greek, Cyrillic, Latin Extended Additional, general punctuation): enough for
# North American and European place names. With --all every range 0-65535 is fetched (~6 MB per
# font); the container build may do this so other scripts render too. Ranges that are missing are
# simply not drawn by GL clients (they log a 404 and carry on).
set -eu
REF=028c18f713baecad011301ff7a69acc39bcc2ae7
BASE="https://raw.githubusercontent.com/protomaps/basemaps-assets/$REF/fonts"
ALL=0
if [ "${1:-}" = "--all" ]; then ALL=1; shift; fi
DEST=${1:-$(dirname "$0")}
FONTS="Noto Sans Regular|Noto Sans Medium|Noto Sans Italic"

ranges() {
  if [ "$ALL" = 1 ]; then
    i=0; while [ $i -lt 65536 ]; do echo "$i-$((i + 255))"; i=$((i + 256)); done
  else
    for s in 0 256 512 768 1024 7680 7936 8192; do echo "$s-$((s + 255))"; done
  fi
}

echo "$FONTS" | tr '|' '\n' | while read -r font; do
  mkdir -p "$DEST/$font"
  enc=$(printf %s "$font" | sed 's/ /%20/g')
  ranges | while read -r r; do
    out="$DEST/$font/$r.pbf"
    [ -s "$out" ] && continue
    curl -fsSL --retry 3 -o "$out" "$BASE/$enc/$r.pbf"
  done
done
curl -fsSL --retry 3 -o "$DEST/OFL.txt" "$BASE/OFL.txt"
