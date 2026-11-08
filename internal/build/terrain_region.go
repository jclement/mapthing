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
}

// areasFromRegions is the only place the terrain build looks inside
// catalog.Region.
func areasFromRegions(regions []catalog.Region) []area {
	out := make([]area, 0, len(regions))
	for _, r := range regions {
		out = append(out, area{id: r.ID, bbox: elevation.BBox(r.BBox), polygon: r.Polygon})
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
	return polygonIntersectsBox(a.polygon, b)
}

func polygonIntersectsBox(rings [][][2]float64, b elevation.BBox) bool {
	// Any vertex inside the box, any box corner inside the polygon, or any
	// edge crossing a box edge.
	for _, ring := range rings {
		for _, p := range ring {
			if b.Contains(p[0], p[1]) {
				return true
			}
		}
	}
	corners := [4][2]float64{{b[0], b[1]}, {b[2], b[1]}, {b[2], b[3]}, {b[0], b[3]}}
	for _, c := range corners {
		if pointInRings(rings, c[0], c[1]) {
			return true
		}
	}
	for _, ring := range rings {
		for i := range ring {
			p, q := ring[i], ring[(i+1)%len(ring)]
			for k := 0; k < 4; k++ {
				if segmentsCross(p, q, corners[k], corners[(k+1)%4]) {
					return true
				}
			}
		}
	}
	return false
}

func pointInRings(rings [][][2]float64, x, y float64) bool {
	in := false
	for _, ring := range rings {
		n := len(ring)
		for i, j := 0, n-1; i < n; j, i = i, i+1 {
			xi, yi, xj, yj := ring[i][0], ring[i][1], ring[j][0], ring[j][1]
			if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
				in = !in
			}
		}
	}
	return in
}

func segmentsCross(p1, p2, p3, p4 [2]float64) bool {
	d := func(a, b, c [2]float64) float64 { return (b[0]-a[0])*(c[1]-a[1]) - (b[1]-a[1])*(c[0]-a[0]) }
	d1, d2 := d(p3, p4, p1), d(p3, p4, p2)
	d3, d4 := d(p1, p2, p3), d(p1, p2, p4)
	return ((d1 > 0) != (d2 > 0)) && ((d3 > 0) != (d4 > 0))
}
