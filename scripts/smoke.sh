#!/usr/bin/env bash
# End-to-end smoke test: build the image, run it with a throwaway data dir, install Delaware,
# build a generation, then exercise tiles, raster, routing, elevation and packs with an API key.
#
#   scripts/smoke.sh                 build image + full run
#   SMOKE_SKIP_BUILD=1 scripts/smoke.sh    reuse mapthing:dev
#   SMOKE_KEEP=1 scripts/smoke.sh          leave the container and data dir for inspection
#   SMOKE_DATA=/path scripts/smoke.sh      reuse a data dir (downloads and planetiler sources cache)
#
# Needs: docker, curl, jq. Exits non-zero on the first failure.
set -euo pipefail

cd "$(dirname "$0")/.."
IMAGE=${SMOKE_IMAGE:-mapthing:dev}
NAME=mapthing-smoke-$$
PORT=${SMOKE_PORT:-}   # empty: Docker picks a free host port
REGION=${SMOKE_REGION:-north-america/us/delaware}
BUILD_TIMEOUT=${SMOKE_BUILD_TIMEOUT:-3600}
ADMIN_USER=smoke
ADMIN_PASSWORD=$(head -c 18 /dev/urandom | base64 | tr -d '/+=')
OWN_DATA=0
if [[ -z "${SMOKE_DATA:-}" ]]; then
  SMOKE_DATA=$(mktemp -d "${TMPDIR:-/tmp}/mapthing-smoke.XXXXXX")
  OWN_DATA=1
fi
JAR=$(mktemp)
FAILS=0

log()  { printf '\033[1m[smoke]\033[0m %s\n' "$*"; }
ok()   { printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILS=$((FAILS + 1)); }
die()  { bad "$*"; exit 1; }

cleanup() {
  local code=$?
  if [[ $code -ne 0 || $FAILS -ne 0 ]]; then
    log "container log tail:"
    docker logs --tail 60 "$NAME" 2>&1 | sed 's/^/    /' || true
  fi
  if [[ -z "${SMOKE_KEEP:-}" ]]; then
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    if [[ $OWN_DATA == 1 ]]; then
      # Files are owned by the container user; remove them from inside a container.
      docker run --rm -v "$SMOKE_DATA:/d" --entrypoint sh "$IMAGE" -c 'rm -rf /d/* /d/.[!.]*' >/dev/null 2>&1 || true
      rmdir "$SMOKE_DATA" 2>/dev/null || true
    fi
  else
    log "kept container $NAME and data $SMOKE_DATA"
  fi
  rm -f "$JAR"
  exit $code
}
trap cleanup EXIT

for t in docker curl jq; do command -v $t >/dev/null || die "$t is not installed"; done

if [[ -z "${SMOKE_SKIP_BUILD:-}" ]]; then
  log "building $IMAGE"
  docker build -t "$IMAGE" -f docker/Dockerfile .
fi

chmod 0777 "$SMOKE_DATA"
IMAGE_ID=$(docker image inspect -f '{{.Id}}' "$IMAGE")

# SMOKE_MODE=pipeline: only the data build (mapthing-build in the container), no server.
if [[ "${SMOKE_MODE:-full}" == pipeline ]]; then
  log "pipeline-only build of $REGION"
  start=$(date +%s)
  docker run --name "$NAME" -v "$SMOKE_DATA:/data" -e MAPTHING_IMAGE_ID="$IMAGE_ID" \
    ${SMOKE_MEMORY:+--memory "$SMOKE_MEMORY"} --entrypoint /usr/bin/tini "$IMAGE" -- \
    mapthing-build -data /data -region "$REGION" -refresh || true
  code=$(docker inspect -f '{{.State.ExitCode}}' "$NAME")
  [[ "$code" == 0 ]] || die "mapthing-build exited $code"
  M=$(docker run --rm -v "$SMOKE_DATA:/data" --entrypoint cat "$IMAGE" /data/current/manifest.json)
  jq -e '.ok and ([.checks[] | select(.ok | not)] | length == 0)' <<<"$M" >/dev/null || die "manifest checks"
  jq -r '.stages[] | "  \(.name): \(.duration_s|floor)s rss=\((.peak_rss_kb // 0)/1024|floor)MiB\(if .skipped then " (skipped)" else "" end)"' <<<"$M"
  jq -r '.artifacts | to_entries[] | "  \(.key): \(.value/1048576*10|floor/10) MiB"' <<<"$M"
  jq -r '.checks[] | "  check \(.name) \(.region // ""): \(.detail)"' <<<"$M"
  jq -r '"  image: \(.image)\n  memory: \(.memory)\n  ocean: \(.ocean)"' <<<"$M"
  ok "pipeline build in $(( $(date +%s) - start ))s"
  exit 0
fi

log "starting container (data $SMOKE_DATA)"
docker run -d --name "$NAME" -p "127.0.0.1:${PORT}:8080" -v "$SMOKE_DATA:/data" -e MAPTHING_IMAGE_ID="$IMAGE_ID" \
  ${SMOKE_MEMORY:+--memory "$SMOKE_MEMORY"} \
  -e ADMIN_USER="$ADMIN_USER" -e ADMIN_PASSWORD="$ADMIN_PASSWORD" "$IMAGE" >/dev/null

PORT=$(docker port "$NAME" 8080/tcp | head -1 | sed 's/.*://')
BASE=http://127.0.0.1:$PORT
log "listening on $BASE"
for i in $(seq 1 60); do
  curl -fsS "$BASE/healthz" >/dev/null 2>&1 && break
  sleep 1
  [[ $i == 60 ]] && die "/healthz never answered"
done
ok "healthz"

# admin: session cookie; Origin satisfies the same-origin check on unsafe methods.
admin() {
  local method=$1 path=$2; shift 2
  curl -fsS -b "$JAR" -c "$JAR" -X "$method" -H "Origin: $BASE" -H 'Content-Type: application/json' "$BASE$path" "$@"
}

admin POST /admin/api/login -d "$(jq -n --arg u "$ADMIN_USER" --arg p "$ADMIN_PASSWORD" '{username:$u,password:$p}')" >/dev/null \
  || die "admin login"
ok "admin login"

admin GET /admin/api/catalog | jq -e --arg r "$REGION" '[.. | objects | select(.id? == $r)] | length > 0' >/dev/null \
  || die "catalog lists $REGION"
ok "catalog lists $REGION"

code=$(curl -sS -o /dev/null -w '%{http_code}' -b "$JAR" -c "$JAR" -X POST -H "Origin: $BASE" -H 'Content-Type: application/json' \
  "$BASE/admin/api/regions" -d "$(jq -n --arg r "$REGION" '{id:$r}')")
case "$code" in
  200|201) ok "region added" ;;
  409) ok "region already installed (reused data dir)" ;;
  *) die "add region: HTTP $code" ;;
esac

JOB=$(admin POST /admin/api/build -d '{"refresh":true}' | jq -r '.id // empty')   # 202 + job row
[[ -n "$JOB" ]] || die "start build (no job id)"
ok "build started (job $JOB)"

log "waiting for build (timeout ${BUILD_TIMEOUT}s)"
start=$(date +%s)
last=""
while :; do
  J=$(admin GET "/admin/api/jobs/$JOB?tail=40")   # {"job": row, "log": [{line}]}
  status=$(jq -r '.job.status' <<<"$J")
  stage=$(jq -r '.job.stage // ""' <<<"$J")
  [[ "$stage" != "$last" ]] && { log "  stage: $stage"; last=$stage; }
  case "$status" in
    succeeded) break ;;
    failed|cancelled)
      jq -r '.job.error, (.log[]?.line)' <<<"$J" | tail -40 | sed 's/^/    /'
      die "build $status at stage $stage" ;;
  esac
  (( $(date +%s) - start > BUILD_TIMEOUT )) && die "build timed out"
  sleep 5
done
ok "build succeeded in $(( $(date +%s) - start ))s"

for i in $(seq 1 60); do
  curl -fsS "$BASE/readyz" >/dev/null 2>&1 && break
  sleep 2
  [[ $i == 60 ]] && die "/readyz not ready after build"
done
ok "readyz"

KEY=$(admin POST /admin/api/keys -d '{"label":"smoke"}' | jq -r '.secret // empty')   # {"key":…, "secret":"mt_…"}
[[ "$KEY" == mt_* ]] || die "create API key"
ok "API key created"
AUTH=(-H "Authorization: Bearer $KEY")

# Dover and Wilmington, Delaware.
DOVER='{"lat":39.1582,"lon":-75.5244}'
WILM='{"lat":39.7391,"lon":-75.5398}'
# z14 tile over Dover.
Z=14; X=4754; Y=6252

check() { # name, expected content-type prefix, curl args...
  local name=$1 want=$2; shift 2
  local out code ctype
  out=$(mktemp)
  read -r code ctype < <(curl -sS -o "$out" -w '%{http_code} %{content_type}\n' "$@" || echo "000 -")
  local size; size=$(stat -c %s "$out")
  if [[ "$code" =~ ^20[06]$ && "$ctype" == $want* && $size -gt 0 ]]; then
    ok "$name ($code, $ctype, $size bytes)"
  else
    bad "$name: HTTP $code, type '$ctype', $size bytes: $(head -c 200 "$out" | tr -d '\0')"
  fi
  rm -f "$out"
}

check "vector tile z$Z/$X/$Y"   "application/"  "${AUTH[@]}" "$BASE/tiles/basemap/$Z/$X/$Y.mvt"
check "tilejson"                "application/json" "${AUTH[@]}" "$BASE/tiles/basemap.json"
check "pmtiles range"           "application/"  "${AUTH[@]}" -H 'Range: bytes=0-126' "$BASE/tiles/basemap.pmtiles"
check "style outdoors"          "application/json" "$BASE/styles/outdoors.json?key=$KEY"
check "raster tile 512@2x"      "image/png"     "$BASE/styles/outdoors/512/$Z/$X/$Y@2x.png?key=$KEY"

ROUTE=$(curl -fsS "${AUTH[@]}" -H 'Content-Type: application/json' -X POST "$BASE/api/v1/route" \
  -d "{\"locations\":[$DOVER,$WILM],\"profile\":\"auto\"}" || true)
D=$(jq -r '.properties.distance_m // empty' <<<"$ROUTE" 2>/dev/null || true)
if [[ -n "$D" ]] && awk "BEGIN{exit !($D > 60000 && $D < 120000)}"; then
  ok "route Dover → Wilmington: $(awk "BEGIN{printf \"%.1f\", $D/1000}") km"
else
  bad "route Dover → Wilmington: $(head -c 300 <<<"$ROUTE")"
fi

PROF=$(curl -fsS "${AUTH[@]}" -H 'Content-Type: application/json' -X POST "$BASE/api/v1/elevation/profile" \
  -d '{"path":[[-75.5244,39.1582],[-75.5398,39.7391]],"interval_m":500}' || true)
N=$(jq -r '[.samples[]? | select(.elevation_m != null)] | length' <<<"$PROF" 2>/dev/null || echo 0)
if [[ "$N" -gt 10 ]]; then
  ok "elevation profile: $N samples, gain $(jq -r .gain_m <<<"$PROF") m"
else
  bad "elevation profile: $(head -c 300 <<<"$PROF")"
fi

check "google elevation compat" "application/json" "$BASE/maps/api/elevation/json?locations=39.1582,-75.5244&key=$KEY"
check "offline pack extract" "application/" "${AUTH[@]}" "$BASE/packs/extract.pmtiles?bbox=-75.56,39.14,-75.50,39.18&minzoom=10&maxzoom=14"

if [[ $FAILS -ne 0 ]]; then
  log "$FAILS check(s) failed"
  exit 1
fi
log "all checks passed"
