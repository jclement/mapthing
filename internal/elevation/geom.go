package elevation

// Small polygon tests shared by the terrain build and on-demand terrain, for
// region outlines given as lon/lat rings read with the even-odd rule.

// RingsIntersectBox reports whether the outline overlaps box b.
func RingsIntersectBox(rings [][][2]float64, b BBox) bool {
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
		if PointInRings(rings, c[0], c[1]) {
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

// PointInRings reports whether the point lies inside the outline.
func PointInRings(rings [][][2]float64, x, y float64) bool {
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
