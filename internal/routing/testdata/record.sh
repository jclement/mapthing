#!/usr/bin/env bash
# Re-record the valhalla_*.json fixtures from a running Valhalla serving the Delaware graph
# (docker/valhalla/dev-build.sh, then dev-serve.sh). Points are public places in Delaware
# plus farm tracks found in OSM (ways 17230913, 17232106, 17229878).
set -euo pipefail
cd "$(dirname "$0")"
V=${VALHALLA:-http://127.0.0.1:8002}
p() { curl -sf "$V/$1" -d "$2" | jq . >"$3"; echo "recorded $3"; }
TRACK='{"locations":[{"lat":38.690,"lon":-75.385},{"lat":38.4717950,"lon":-75.2588452}],"costing":"auto","costing_options":{"auto":{"use_tracks":0.3}},"directions_options":{"units":"kilometers"}}'
p route "$TRACK" valhalla_route_track.json
shape=$(jq -r '.trip.legs[0].shape' valhalla_route_track.json)
p trace_attributes "$(jq -nc --arg s "$shape" '{encoded_polyline:$s,shape_match:"edge_walk",costing:"auto",units:"kilometers",filters:{attributes:["edge.surface","edge.length","edge.begin_shape_index","edge.end_shape_index"],action:"include"}}')" valhalla_trace_track.json
p route '{"locations":[{"lat":39.7391,"lon":-75.5398},{"lat":39.1582,"lon":-75.5244},{"lat":38.690,"lon":-75.385}],"costing":"truck","costing_options":{"truck":{"use_tracks":0.3}},"directions_options":{"units":"kilometers"}}' valhalla_route_multi.json
p route '{"locations":[{"lat":39.7391,"lon":-75.5398},{"lat":39.6837,"lon":-75.7497}],"costing":"auto","costing_options":{"auto":{"use_tracks":0.3}},"alternates":2,"directions_options":{"units":"kilometers"}}' valhalla_route_alternates.json
# A route whose shape Valhalla cannot edge-walk (error 443), and its map-matched surfaces.
ROUTE443='{"locations":[{"lat":39.4473,"lon":-75.7163},{"lat":39.1582,"lon":-75.5244}],"costing":"auto","costing_options":{"auto":{"use_tracks":0.3}},"directions_options":{"units":"kilometers"}}'
p route "$ROUTE443" valhalla_route_443.json
shape=$(jq -r '.trip.legs[0].shape' valhalla_route_443.json)
curl -s "$V/trace_attributes" -d "$(jq -nc --arg s "$shape" '{encoded_polyline:$s,shape_match:"edge_walk",costing:"auto"}')" | jq . >valhalla_error_edgewalk.json
p trace_attributes "$(jq -nc --arg s "$shape" '{encoded_polyline:$s,shape_match:"map_snap",costing:"auto",units:"kilometers",filters:{attributes:["edge.surface","edge.length"],action:"include"}}')" valhalla_trace_mapsnap.json
curl -s "$V/route" -d '{"locations":[{"lat":38.5,"lon":-74.9,"search_cutoff":500},{"lat":39.1582,"lon":-75.5244}],"costing":"auto"}' | jq . >valhalla_error_noedge.json
p sources_to_targets '{"sources":[{"lat":39.7391,"lon":-75.5398},{"lat":39.1582,"lon":-75.5244}],"targets":[{"lat":38.690,"lon":-75.385},{"lat":39.1582,"lon":-75.5244}],"costing":"auto","units":"kilometers"}' valhalla_matrix.json
p locate '{"locations":[{"lat":38.4720,"lon":-75.2590,"search_cutoff":10000},{"lat":38.5,"lon":-74.9,"search_cutoff":500}],"costing":"auto","verbose":true}' valhalla_locate.json
p isochrone '{"locations":[{"lat":38.690,"lon":-75.385}],"costing":"auto","contours":[{"time":3},{"time":6}],"polygons":true,"generalize":150}' valhalla_isochrone.json
