package build

import (
	"github.com/jclement/mapthing/internal/catalog"
	"github.com/jclement/mapthing/internal/elevation"
)

// area is the part of a region the terrain build covers.
type area struct {
	id      string
	bbox    elevation.BBox
	polygon [][][2]float64 // optional, lon/lat rings
	// lowOnly: the region's terrain is on demand, so only its low zooms are built.
	lowOnly bool
}

// fullAreas returns the areas whose terrain is built at every zoom.
func fullAreas(areas []area) []area {
	var out []area
	for _, a := range areas {
		if !a.lowOnly {
			out = append(out, a)
		}
	}
	return out
}

// lowAreas returns the areas whose terrain is on demand.
func lowAreas(areas []area) []area {
	var out []area
	for _, a := range areas {
		if a.lowOnly {
			out = append(out, a)
		}
	}
	return out
}

// areasFromRegions is the only place the terrain build looks inside
// catalog.Region.
func areasFromRegions(regions []catalog.Region) []area {
	out := make([]area, 0, len(regions))
	for _, r := range regions {
		out = append(out, area{id: r.ID, bbox: elevation.BBox(r.BBox), polygon: r.Polygon, lowOnly: r.TerrainOnDemand})
	}
	return out
}

// intersectsBox reports whether the area overlaps box b.
func (a area) intersectsBox(b elevation.BBox) bool {
	if !a.bbox.Intersects(b) {
		return false
	}
	if len(a.polygon) == 0 {
		return true
	}
	return elevation.RingsIntersectBox(a.polygon, b)
}
