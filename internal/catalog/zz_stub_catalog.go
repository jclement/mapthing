// Package catalog is owned by the pipeline worker.
//
// STUB: the minimal declarations the terrain build codes against until
// feature/pipeline merges. Delete this file at merge. The terrain build reads
// regions only through internal/build/terrain_region.go, so a different field
// layout needs a change there alone.
package catalog

// BBox is west, south, east, north in degrees.
type BBox [4]float64

// Region is an installable region.
type Region struct {
	ID   string
	Name string
	BBox BBox
	// Polygon is optional: rings of lon/lat (even-odd rule). When present the
	// terrain build skips tiles outside it.
	Polygon [][][2]float64
}
