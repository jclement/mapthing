// Package contours turns a terrain archive (terrain.pmtiles, Terrarium PNG) into contour
// lines in a vector-tile archive (contours.pmtiles, MVT layer "contours" with ele and major).
//
// Each contour tile is contoured on its own elevation grid, resampled from the terrain
// archive's max-zoom tiles with a one-pixel border taken from the neighbours, so lines meet
// across tile edges without any clipping. See Build and docs/architecture.md (Contours).
package contours

import (
	"math"
)

// Point is a position in grid pixel units (grid point (i, j) is at (i, j)).
type Point struct{ X, Y float64 }

// Isolines traces the contour lines of a w×h grid (row-major, NaN = no data) at each level.
// Grid points are cell corners; a cell with a NaN corner is skipped, so lines end at the
// edge of the data. Lines are returned per level index, each as a polyline; closed lines
// repeat their first point at the end. A value equal to a level counts as above it.
func Isolines(v []float32, w, h int, levels []float64) [][][]Point {
	out := make([][][]Point, len(levels))
	if w < 2 || h < 2 || len(levels) == 0 {
		return out
	}
	segs := make([][]seg, len(levels))
	for j := 0; j < h-1; j++ {
		for i := 0; i < w-1; i++ {
			a, b := float64(v[j*w+i]), float64(v[j*w+i+1])
			d, c := float64(v[(j+1)*w+i]), float64(v[(j+1)*w+i+1])
			if math.IsNaN(a) || math.IsNaN(b) || math.IsNaN(c) || math.IsNaN(d) {
				continue
			}
			lo := math.Min(math.Min(a, b), math.Min(c, d))
			hi := math.Max(math.Max(a, b), math.Max(c, d))
			// levels is ascending: find the first level > lo.
			k := firstAbove(levels, lo)
			for ; k < len(levels) && levels[k] <= hi; k++ {
				segs[k] = cellSegments(segs[k], levels[k], i, j, w, a, b, c, d)
			}
		}
	}
	for k, ss := range segs {
		if len(ss) == 0 {
			continue
		}
		out[k] = chain(ss, func(e int32) Point { return edgePoint(e, w, v, levels[k]) })
	}
	return out
}

// firstAbove returns the index of the first level > x (levels ascending).
func firstAbove(levels []float64, x float64) int {
	lo, hi := 0, len(levels)
	for lo < hi {
		m := (lo + hi) / 2
		if levels[m] > x {
			hi = m
		} else {
			lo = m + 1
		}
	}
	return lo
}

// seg joins two grid edges. Edge ids: 2*(j*w+i) is the horizontal edge (i,j)-(i+1,j),
// 2*(j*w+i)+1 the vertical edge (i,j)-(i,j+1).
type seg struct{ a, b int32 }

// cellSegments appends the segments of cell (i,j) at level l. Corners: a top-left,
// b top-right, c bottom-right, d bottom-left.
func cellSegments(ss []seg, l float64, i, j, w int, a, b, c, d float64) []seg {
	n := 0
	if a >= l {
		n |= 8
	}
	if b >= l {
		n |= 4
	}
	if c >= l {
		n |= 2
	}
	if d >= l {
		n |= 1
	}
	if n == 0 || n == 15 {
		return ss
	}
	top := int32(2 * (j*w + i))
	bottom := int32(2 * ((j+1)*w + i))
	left := int32(2*(j*w+i) + 1)
	right := int32(2*(j*w+i+1) + 1)
	switch n {
	case 1, 14:
		ss = append(ss, seg{left, bottom})
	case 2, 13:
		ss = append(ss, seg{bottom, right})
	case 3, 12:
		ss = append(ss, seg{left, right})
	case 4, 11:
		ss = append(ss, seg{top, right})
	case 6, 9:
		ss = append(ss, seg{top, bottom})
	case 7, 8:
		ss = append(ss, seg{left, top})
	case 5, 10: // saddle: the cell centre decides which diagonal pair is connected
		centreAbove := (a+b+c+d)/4 >= l
		if (n == 5) == centreAbove { // above corners b, d joined (5) or below corners b, d joined (10): cut a and c off
			ss = append(ss, seg{left, top}, seg{bottom, right})
		} else {
			ss = append(ss, seg{top, right}, seg{left, bottom})
		}
	}
	return ss
}

// edgePoint is where level l crosses edge e (linear interpolation along the edge).
func edgePoint(e int32, w int, v []float32, l float64) Point {
	p := int(e >> 1)
	i, j := p%w, p/w
	v0 := float64(v[p])
	var v1 float64
	if e&1 == 0 {
		v1 = float64(v[p+1])
	} else {
		v1 = float64(v[p+w])
	}
	t := 0.5
	if v1 != v0 {
		t = (l - v0) / (v1 - v0)
	}
	t = math.Max(0, math.Min(1, t))
	if e&1 == 0 {
		return Point{float64(i) + t, float64(j)}
	}
	return Point{float64(i), float64(j) + t}
}

// chain joins segments that share edges into polylines. Every edge is shared by at most two
// segments (one per neighbouring cell), so the segments form paths and cycles.
func chain(ss []seg, pt func(int32) Point) [][]Point {
	type ends struct{ s [2]int32 } // segment indices touching an edge (-1 = none)
	adj := make(map[int32]*ends, len(ss)*2)
	add := func(e int32, si int32) {
		x := adj[e]
		if x == nil {
			adj[e] = &ends{[2]int32{si, -1}}
			return
		}
		x.s[1] = si
	}
	for si, s := range ss {
		add(s.a, int32(si))
		add(s.b, int32(si))
	}
	used := make([]bool, len(ss))
	other := func(e int32, from int32) int32 {
		x := adj[e]
		if x.s[0] == from {
			return x.s[1]
		}
		return x.s[0]
	}
	walk := func(start int32, e int32) []Point {
		// start at edge e of segment start, walking away from e
		line := []Point{pt(e)}
		si := start
		for si >= 0 && !used[si] {
			used[si] = true
			s := ss[si]
			next := s.a
			if next == e {
				next = s.b
			}
			line = append(line, pt(next))
			e = next
			si = other(e, si)
		}
		return line
	}
	var lines [][]Point
	// open paths start at edges touched once
	for si, s := range ss {
		if used[si] {
			continue
		}
		for _, e := range [2]int32{s.a, s.b} {
			if adj[e].s[1] < 0 {
				lines = append(lines, walk(int32(si), e))
				break
			}
		}
	}
	// what remains are cycles
	for si, s := range ss {
		if !used[si] {
			lines = append(lines, walk(int32(si), s.a))
		}
	}
	return lines
}

// Simplify reduces a polyline with Douglas-Peucker at tolerance tol (same units as the
// points). The first and last points are kept.
func Simplify(pts []Point, tol float64) []Point {
	if len(pts) < 3 || tol <= 0 {
		return pts
	}
	keep := make([]bool, len(pts))
	keep[0], keep[len(pts)-1] = true, true
	type span struct{ a, b int }
	stack := []span{{0, len(pts) - 1}}
	tol2 := tol * tol
	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		best, bi := -1.0, -1
		for k := s.a + 1; k < s.b; k++ {
			if d := segDist2(pts[k], pts[s.a], pts[s.b]); d > best {
				best, bi = d, k
			}
		}
		if bi >= 0 && best > tol2 {
			keep[bi] = true
			stack = append(stack, span{s.a, bi}, span{bi, s.b})
		}
	}
	out := make([]Point, 0, len(pts)/2+2)
	for k, p := range pts {
		if keep[k] {
			out = append(out, p)
		}
	}
	return out
}

// segDist2 is the squared distance from p to segment ab.
func segDist2(p, a, b Point) float64 {
	dx, dy := b.X-a.X, b.Y-a.Y
	l2 := dx*dx + dy*dy
	t := 0.0
	if l2 > 0 {
		t = math.Max(0, math.Min(1, ((p.X-a.X)*dx+(p.Y-a.Y)*dy)/l2))
	}
	ex, ey := a.X+t*dx-p.X, a.Y+t*dy-p.Y
	return ex*ex + ey*ey
}

// Length is the length of a polyline.
func Length(pts []Point) float64 {
	var s float64
	for k := 1; k < len(pts); k++ {
		s += math.Hypot(pts[k].X-pts[k-1].X, pts[k].Y-pts[k-1].Y)
	}
	return s
}
