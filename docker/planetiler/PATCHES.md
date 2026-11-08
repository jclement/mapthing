# Patches to the Protomaps basemap profile

The image builds Planetiler's Protomaps profile from
[protomaps/basemaps](https://github.com/protomaps/basemaps) `tiles/` at the commit pinned by
`BASEMAPS_REF` in `docker/Dockerfile`, then applies the patches in this directory with
`git apply`. The upstream code is BSD-3-Clause (`LICENSE-protomaps-basemaps.md`, which must stay
with any redistribution); the patches are under the same licence.

Each patch is kept small so it rebases onto a newer upstream: bump `BASEMAPS_REF`, rebuild, and
if `git apply` fails, re-make the patch against the new `Roads.java`. Every change is marked with a
`mapthing:` comment in the code.

## `roads.patch` (`tiles/src/main/java/com/protomaps/basemap/layers/Roads.java`)

Rural and oilfield use needs minor roads (tracks, unpaved county roads, private lease roads)
visible and distinguishable earlier than an urban basemap shows them. All additions are extra
attributes or earlier minzooms: existing Protomaps v4 clients ignore attributes they do not know.

1. **Surface.** `roads` features of kind `major_road`, `minor_road` and `path` carry, from z11:
   - `surface`: `paved` or `unpaved` (normalised from OSM `surface`; absent when unknown),
   - `surface_raw`: the OSM `surface` value as tagged,
   - `tracktype`: OSM `tracktype` (`grade1`–`grade5`).
2. **Private access.** Upstream pushes `access=private|no` roads to z15. The patch keeps their
   normal minzoom (z12 for tracks, unclassified and service roads) and emits `access` from z12
   (upstream: z15) so styles can draw them de-emphasised instead of hiding them.
3. **Service roads.** Plain `highway=service` (no `service=*` subtype, typical of rural lease
   roads) from z12 instead of z13. Service roads tagged `driveway`, `parking_aisle`,
   `drive-through`, `alley` or `emergency_access` go to z13 (upstream left those at z12).
