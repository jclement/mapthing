# Map themes

Besides `outdoors`, `outdoors-dark` and `satellite`, mapthing ships three themes built by the
same generator (`assets/styles/src/themes/`). Every theme keeps the hidden `satellite` layer, so
a client can toggle imagery under the roads and labels.

| Theme | For | Character |
|---|---|---|
| `overlay` | light apps that draw dense data on the map (points, pipelines, survey grids) | cool pale-grey land, greyed water and vegetation, white roads ranked by width and casing, muted taupe tracks, no POI icons, gentle hillshade |
| `overlay-dark` | the same, in dark application chrome | charcoal land, dark slate water, roads lighten with rank, warm-grey tracks, no POI icons |
| `field` | outdoor use in sunlight | warm paper land, strong blue water and green vegetation, orange highways, yellow major roads, dark-cased white minor and service roads, dark-brown dashed tracks from z11, strong hillshade, labels 20% larger with 2 px halos |

## Design rules

- **Overlay themes recede.** Basemap hue is kept away from the colours data layers tend to use:
  water is a desaturated grey-blue (so cyan and blue data stay distinct), vegetation is nearly
  neutral (so green data stays distinct), tracks are taupe rather than orange. Road rank is
  carried by width and casing, not hue.
- **Labels stay legible.** Place and road labels are ≥ 3:1 against their halos in the overlay
  themes (most ≥ 4.5:1) and ≥ 4.5:1 in `field`.
- **Colour-blind safe hierarchy.** In `field` highway (orange) and major (yellow) roads differ
  in lightness and width as well as hue (ΔE76 ≈ 21 under deuteranopia simulation, 32 under
  protanopia); minor and service roads are white with dark casings; tracks are dashed.
- **Data contrast.** No single basemap gives every data colour ≥ 3:1: very light data colours
  need a dark casing on the light themes, very dark ones a light halo on the dark theme.
