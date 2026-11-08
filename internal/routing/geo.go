package routing

import (
	"errors"
	"math"
	"strings"
)

// DecodePolyline decodes a Google-style encoded polyline into lon,lat pairs.
// precision is the number of decimal digits (5 for Google, 6 for Valhalla).
func DecodePolyline(s string, precision int) ([][2]float64, error) {
	factor := math.Pow10(precision)
	var out [][2]float64
	var lat, lon int64
	i := 0
	next := func() (int64, error) {
		var result int64
		var shift uint
		for {
			if i >= len(s) {
				return 0, errors.New("truncated polyline")
			}
			b := int64(s[i]) - 63
			i++
			if b < 0 || b > 63 {
				return 0, errors.New("invalid polyline character")
			}
			result |= (b & 0x1f) << shift
			shift += 5
			if b < 0x20 {
				break
			}
			if shift > 60 {
				return 0, errors.New("polyline value overflow")
			}
		}
		if result&1 != 0 {
			return ^(result >> 1), nil
		}
		return result >> 1, nil
	}
	for i < len(s) {
		dlat, err := next()
		if err != nil {
			return nil, err
		}
		dlon, err := next()
		if err != nil {
			return nil, err
		}
		lat += dlat
		lon += dlon
		out = append(out, [2]float64{float64(lon) / factor, float64(lat) / factor})
	}
	return out, nil
}

// EncodePolyline encodes lon,lat pairs as a Google-style polyline.
func EncodePolyline(pts [][2]float64, precision int) string {
	factor := math.Pow10(precision)
	var b strings.Builder
	var plat, plon int64
	enc := func(v int64) {
		u := v << 1
		if v < 0 {
			u = ^u
		}
		for u >= 0x20 {
			b.WriteByte(byte((0x20 | (u & 0x1f)) + 63))
			u >>= 5
		}
		b.WriteByte(byte(u + 63))
	}
	for _, p := range pts {
		lat := int64(math.Round(p[1] * factor))
		lon := int64(math.Round(p[0] * factor))
		enc(lat - plat)
		enc(lon - plon)
		plat, plon = lat, lon
	}
	return b.String()
}

const earthRadiusM = 6371008.8

// haversine is the great-circle distance in metres between two lon,lat points.
func haversine(a, b [2]float64) float64 {
	rad := math.Pi / 180
	dlat := (b[1] - a[1]) * rad
	dlon := (b[0] - a[0]) * rad
	h := math.Sin(dlat/2)*math.Sin(dlat/2) +
		math.Cos(a[1]*rad)*math.Cos(b[1]*rad)*math.Sin(dlon/2)*math.Sin(dlon/2)
	return 2 * earthRadiusM * math.Asin(math.Min(1, math.Sqrt(h)))
}

// pathLength is the length in metres of pts[from..to].
func pathLength(pts [][2]float64, from, to int) float64 {
	if from < 0 {
		from = 0
	}
	if to >= len(pts) {
		to = len(pts) - 1
	}
	var d float64
	for i := from; i < to; i++ {
		d += haversine(pts[i], pts[i+1])
	}
	return d
}

// bbox returns [minLon, minLat, maxLon, maxLat] of pts.
func bbox(pts [][2]float64) [4]float64 {
	if len(pts) == 0 {
		return [4]float64{}
	}
	b := [4]float64{pts[0][0], pts[0][1], pts[0][0], pts[0][1]}
	for _, p := range pts[1:] {
		b[0] = math.Min(b[0], p[0])
		b[1] = math.Min(b[1], p[1])
		b[2] = math.Max(b[2], p[0])
		b[3] = math.Max(b[3], p[1])
	}
	return b
}

// simplify reduces pts with Douglas-Peucker at roughly tolM metres tolerance.
func simplify(pts [][2]float64, tolM float64) [][2]float64 {
	if len(pts) < 3 || tolM <= 0 {
		return pts
	}
	keep := make([]bool, len(pts))
	keep[0], keep[len(pts)-1] = true, true
	// Work in a local equirectangular projection (metres); fine at route scale.
	lat0 := pts[0][1] * math.Pi / 180
	kx := earthRadiusM * math.Pi / 180 * math.Cos(lat0)
	ky := earthRadiusM * math.Pi / 180
	xy := func(p [2]float64) (float64, float64) { return p[0] * kx, p[1] * ky }
	type span struct{ a, b int }
	stack := []span{{0, len(pts) - 1}}
	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		ax, ay := xy(pts[s.a])
		bx, by := xy(pts[s.b])
		dx, dy := bx-ax, by-ay
		l2 := dx*dx + dy*dy
		maxD, idx := 0.0, -1
		for i := s.a + 1; i < s.b; i++ {
			px, py := xy(pts[i])
			var d float64
			if l2 == 0 {
				d = math.Hypot(px-ax, py-ay)
			} else {
				t := math.Max(0, math.Min(1, ((px-ax)*dx+(py-ay)*dy)/l2))
				d = math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
			}
			if d > maxD {
				maxD, idx = d, i
			}
		}
		if idx >= 0 && maxD > tolM {
			keep[idx] = true
			stack = append(stack, span{s.a, idx}, span{idx, s.b})
		}
	}
	out := make([][2]float64, 0, len(pts))
	for i, k := range keep {
		if k {
			out = append(out, pts[i])
		}
	}
	return out
}
