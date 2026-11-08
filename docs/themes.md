# Map themes

Besides `outdoors`, `outdoors-dark` and `satellite`, mapthing ships three themes built by the
same generator (`assets/styles/src/themes/`). Every theme keeps the hidden `satellite` layer, so
a client can toggle imagery under the roads and labels.

| Theme | For | Character |
|---|---|---|
| `overlay` | light apps that draw dense data on the map (points, pipelines, survey grids) | cool pale-grey land, greyed water and vegetation, neutral (never warm) land-use fills, well pads (`industrial`) in neutral grey, white roads ranked by width and casing darkness, white-cored service roads, muted taupe tracks, streams from z12, Interstate and US shields only below z11, no POI icons, soft hillshade |
| `overlay-dark` | the same, in dark application chrome | charcoal land, dark slate water, roads lighten with rank, service roads with a body over a dark casing, warm-grey tracks, streams from z12, grey route tags instead of black shields, highlight-led hillshade, no POI icons |
| `field` | outdoor use in sunlight | warm paper land, strong blue water and green vegetation, orange motorways, yellow trunk/primary/secondary roads, dark-cased white tertiary, minor and lease/service roads (about 3 px at z13, 5.5 px at z15), dark-brown dashed tracks from z11, strong hillshade, labels 20% larger with 2 px halos, route shields 1.4x the Protomaps size |

## Design rules

- **Overlay themes recede.** Basemap hue is kept away from the colours data layers tend to use:
  water is a desaturated grey-blue (so cyan and blue data stay distinct), vegetation is nearly
  neutral (so green data stays distinct), tracks are taupe rather than orange. Road rank is
  carried by width and casing, not hue. Service and lease roads are always drawn as a road (a
  core with a casing), never as a single thin grey line that could be read as a survey grid.
- **Labels stay legible.** Place and road labels are ≥ 3:1 against their halos in the overlay
  themes (most ≥ 4.5:1) and ≥ 4.5:1 in `field`.
- **Colour-blind safe hierarchy.** In `field` motorways (orange) and major roads (yellow) differ
  in lightness and width as well as hue (ΔE76 ≈ 21 under deuteranopia simulation, 36 under
  protanopia), and every rank steps in width (z14: motorway 7 px, primary 5.5, secondary 4.5,
  tertiary 3.5, residential 2.5), so the order survives greyscale; yellow is kept for secondary
  and above; tracks are dashed.
- **Data contrast.** No single basemap gives every data colour ≥ 3:1: very light data colours
  need a dark casing on the light themes, very dark ones a light halo on the dark theme.

Per-class road widths and colours within `major_road` (trunk, primary, secondary, tertiary) are
set in each theme's `tweak` hook with the helpers in `assets/styles/src/themes/_roads.mjs`.
