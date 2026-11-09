# Styles, glyphs, sprites

GL styles (`/styles/{name}.json`) are generated: one theme object per style in
`assets/styles/src/themes/` goes through one layer builder (`assets/styles/src/layers.mjs`) and
`mise run styles` writes `assets/styles/<name>.json`, validated against the Mapbox GL style spec
v13 (mapbox-gl 1.13.x) and the current MapLibre spec, plus a ban-list of newer expressions and
properties. `mise run lint` fails if a committed style is stale. Fields:
`assets/styles/src/themes/README.md`.

Themes: `outdoors` (light, field use), `outdoors-dark`, `satellite` (imagery with roads and
labels). Every style has the raster layer `satellite` (source `imagery`), hidden unless the
theme shows it, so Mapbox GL 1.13 clients can toggle it.

Base layers: `@protomaps/basemaps` 5.7.2 (BSD-3-Clause, `assets/styles/LICENSE-protomaps-basemaps.md`)
for the Protomaps v4 tile schema, with field changes:

- `highway=track` (`kind=path`, `kind_detail=track`) drawn brown and dashed with a light casing
  from z12, the zoom the tiles start carrying them;
- service roads cased from z13; minor roads wider and cased from z12; minor road names from z13;
- an `roads_unpaved` dashed overlay on minor/major roads whose `surface` is unpaved. The stock
  Protomaps profile does not emit `surface`, so this layer is inert until the tile build keeps
  it;
- hillshade from the `terrain` raster-dem source (Terrarium, 256 px, maxzoom 12);
- contour lines and `"750 m"` labels from the `contours` vector source (layer `contours`, z10–12,
  overzoomed). The source carries 100/20 m lines at z10–11 and 50/10 m from z12. Minor lines fade
  in from z11; labels on major lines start at z12. Colours and opacities are per theme: subtle in
  `overlay`, stronger in `field`;
- labels: English name, else local name; single-font stacks only;
- `outdoors` and `outdoors-dark`: main roads coloured by rank and stepped in width (z14:
  motorway 6 px, trunk 5, primary 4.5, secondary 3.5, tertiary 2.6; `src/themes/_ranked.mjs`),
  route shields 1.25x the Protomaps size. Light: motorway orange, trunk yellow, primary pale
  yellow, secondary and below white. Dark, muted: motorway dim orange, trunk ochre, primary
  khaki, secondary and below grey, rank also stepping in lightness;
- `landuse kind=industrial` (which includes oilfield well pads and compressor sites) is a
  grey-tan in `outdoors` and neutral grey in `overlay`/`overlay-dark`, never the Protomaps pale
  blue, which reads as water on a lease.

Placeholders filled in by the server: `{{BASE_URL}}` and `{{KEY_QS}}`.

Glyphs: `assets/fonts/<Noto Sans Regular|Medium|Italic>/<range>.pbf`, SDF PBFs from
protomaps/basemaps-assets (SIL OFL 1.1, `assets/fonts/OFL.txt`). Only Latin, Latin Extended,
IPA, combining marks, Greek, Cyrillic and general punctuation are committed (~2.5 MB);
`assets/fonts/fetch.sh --all [dest]` fetches every range (~18 MB) at a pinned commit, e.g. at
image build time.

Sprites: `assets/sprites/outdoors{,@2x}` and `outdoors-dark{,@2x}`, the Protomaps v4 light/dark
sprite sheets (derived from MIT-licensed tangrams/icons; `assets/sprites/LICENSE-tangrams-icons.md`).
