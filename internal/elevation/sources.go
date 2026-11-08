package elevation

// Known DEM sources, in priority order (first wins where several have data).
// docs/elevation-sources.md explains each choice; internal/build/terrain_sources.go
// knows how to fetch them.

// BBox is west, south, east, north in degrees.
type BBox [4]float64

// Intersects reports whether two boxes overlap (touching edges do not count).
func (b BBox) Intersects(o BBox) bool {
	return b[0] < o[2] && o[0] < b[2] && b[1] < o[3] && o[1] < b[3]
}

// Contains reports whether the point lies inside the box (edges inclusive).
func (b BBox) Contains(lon, lat float64) bool {
	return lon >= b[0] && lon <= b[2] && lat >= b[1] && lat <= b[3]
}

// SourceDef is the static description of a DEM source.
type SourceDef struct {
	SourceInfo
	Datum    string // vertical datum of the heights
	Kind     string // "dtm" (bare earth) or "dsm" (surface incl. canopy)
	Coverage []BBox // coarse extent, for the catalog; actual data may be patchier
}

const (
	attrUSGS  = "U.S. Geological Survey, 3D Elevation Program"
	attrNRCan = "Contains information licensed under the Open Government Licence – Canada (Natural Resources Canada)"
	attrCop   = "© DLR e.V. 2010-2014 and © Airbus Defence and Space GmbH 2014-2018 provided under COPERNICUS by the European Union and ESA; all rights reserved"
)

var (
	canada = []BBox{{-141.01, 41.67, -52.6, 83.12}}
	us     = []BBox{
		{-125.0, 24.3, -66.9, 49.5},   // CONUS
		{-179.2, 51.2, -129.9, 71.45}, // Alaska (1/3″ coverage is partial)
		{-160.3, 18.9, -154.8, 22.3},  // Hawaii
		{-67.95, 17.88, -65.2, 18.53}, // Puerto Rico
	}
	world = []BBox{{-180, -90, 180, 90}}
)

// Sources lists every source the build knows, highest priority first.
var KnownSources = []SourceDef{
	{
		SourceInfo: SourceInfo{ID: "hrdem", Name: "NRCan HRDEM mosaic (LiDAR, 2 m)", ResolutionM: 2,
			License: "OGL-Canada-2.0", Attribution: attrNRCan},
		Datum: "CGVD2013", Kind: "dtm", Coverage: canada,
	},
	{
		SourceInfo: SourceInfo{ID: "3dep13", Name: "USGS 3DEP 1/3 arc-second DEM", ResolutionM: 10,
			License: "Public domain", Attribution: attrUSGS},
		Datum: "NAVD88", Kind: "dtm", Coverage: us,
	},
	{
		SourceInfo: SourceInfo{ID: "mrdem", Name: "NRCan MRDEM-30", ResolutionM: 30,
			License: "OGL-Canada-2.0", Attribution: attrNRCan},
		Datum: "CGVD2013", Kind: "dtm", Coverage: canada,
	},
	{
		SourceInfo: SourceInfo{ID: "glo30", Name: "Copernicus DEM GLO-30", ResolutionM: 30,
			License: "Copernicus DEM licence (free, attribution required)", Attribution: attrCop},
		Datum: "EGM2008", Kind: "dsm", Coverage: world,
	},
}

// SourceByID returns the definition of a known source.
func SourceByID(id string) (SourceDef, bool) {
	for _, s := range KnownSources {
		if s.ID == id {
			return s, true
		}
	}
	return SourceDef{}, false
}

// CoverageFor returns the sources expected to have data inside bbox, highest
// priority first. It uses coarse static extents and needs no network, so it
// suits the region catalog; the build discovers the exact footprint.
func CoverageFor(bbox BBox) []SourceInfo {
	var out []SourceInfo
	for _, s := range KnownSources {
		for _, c := range s.Coverage {
			if c.Intersects(bbox) {
				out = append(out, s.SourceInfo)
				break
			}
		}
	}
	return out
}

// CoverageIDs is CoverageFor returning only source ids (the shape the catalog wants).
func CoverageIDs(bbox [4]float64) []string {
	var ids []string
	for _, s := range CoverageFor(BBox(bbox)) {
		ids = append(ids, s.ID)
	}
	return ids
}

// Metadata is the JSON metadata stored in terrain.pmtiles.
type Metadata struct {
	Name     string `json:"name"`
	Format   string `json:"format"`    // "png"
	Encoding string `json:"encoding"`  // "terrarium"
	TileSize int    `json:"tile_size"` // pixels, 256
	// Sources is indexed by the per-pixel source index minus one
	// (index 0 means no data).
	Sources     []MetaSource `json:"sources"`
	MinZoom     int          `json:"minzoom"`
	MaxZoom     int          `json:"maxzoom"`
	Bounds      BBox         `json:"bounds"`
	Regions     []string     `json:"regions,omitempty"`
	BuiltAt     string       `json:"built_at"`
	Attribution string       `json:"attribution"`
}

// MetaSource is a source as recorded in an archive.
type MetaSource struct {
	SourceInfo
	Datum string `json:"datum"`
	Kind  string `json:"kind"`
	// Footprint lists the 1°-cell boxes (clipped to the build area) where this
	// source contributed data.
	Footprint []BBox `json:"footprint,omitempty"`
	Pixels    int64  `json:"pixels"` // max-zoom pixels taken from this source
}
