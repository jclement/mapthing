# Style themes

Each `*.mjs` file here (except `_*.mjs` helpers) is one style. Its default export is a theme
object; `src/layers.mjs` (`buildStyle(theme)`) turns it into a full GL style v8 and
`src/build.mjs` writes it to `assets/styles/<name>.json` after validating it against the
mapbox-gl 1.13 and MapLibre specs. Regenerate with `mise run styles`; `mise run lint` fails if a
committed style is out of date.

Add a theme: copy `outdoors.mjs` to `<name>.mjs`, set `name` (must equal the file name) and
`title`, override what differs from `_base.mjs`, run `mise run styles`. The server serves any
`assets/styles/<name>.json` as `/styles/<name>.json`.

| Field | What it controls |
|---|---|
| `name`, `title` | style id (file name, URL) and display name |
| `flavor`, `flavorOverrides` | the Protomaps colour flavour to start from (`light`, `dark`, `white`, `grayscale`, `black`) and any of its colour keys to replace (see `Flavor` in `@protomaps/basemaps`) |
| `sprite` | sprite sheet in `assets/sprites/` (`outdoors`, `outdoors-dark`) |
| `colors` | colours of the roads and labels this generator adds or retunes: minor, service, track, unpaved, minor-road labels |
| `roads` | widths (zoom stops), casings, track min zoom and dash pattern |
| `labels` | minor-road label min zoom, halo width, `pois: false` to hide POIs |
| `hillshade` | `enabled`, exaggeration stops, shadow/highlight/accent colours |
| `contours` | contour lines and labels (`contours.pmtiles`): `enabled`, minor/major colours, width and opacity stops, `minorMinzoom`, label min zoom, size, spacing, colour and halo |
| `satellite` | `visible` and extra raster paint for the **required** `satellite` layer (source `imagery`) |
| `overImagery` | when set, the style is drawn over imagery: fills dropped, roads translucent, light labels |
| `terrainMaxzoom`, `contourMaxzoom`, `imageryMaxzoom` | source max zooms (overzoomed beyond) |
| `hide` | layer ids to hide |
| `tweak(layers)` | last-resort hook to edit the final layer list |

Rules every theme keeps: the `satellite` layer exists; only `Noto Sans Regular|Medium|Italic`
fonts; no expressions or properties newer than mapbox-gl 1.13 (the validator enforces both).
