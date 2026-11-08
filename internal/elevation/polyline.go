package elevation

import (
	"errors"
	"math"
	"strings"
)

// DecodePolyline decodes a Google encoded polyline (precision 5 for Google,
// 6 for Valhalla/OSRM "polyline6") into lon/lat points.
func DecodePolyline(s string, precision int) ([]LonLat, error) {
	if precision <= 0 {
		precision = 5
	}
	factor := math.Pow10(precision)
	var pts []LonLat
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
			if shift > 60 {
				return 0, errors.New("invalid polyline")
			}
			if b < 0x20 {
				break
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
		pts = append(pts, LonLat{float64(lon) / factor, float64(lat) / factor})
	}
	return pts, nil
}

// EncodePolyline encodes lon/lat points as a Google encoded polyline.
func EncodePolyline(pts []LonLat, precision int) string {
	if precision <= 0 {
		precision = 5
	}
	factor := math.Pow10(precision)
	var sb strings.Builder
	var plat, plon int64
	enc := func(v int64) {
		u := v << 1
		if v < 0 {
			u = ^u
		}
		for u >= 0x20 {
			sb.WriteByte(byte((0x20 | (u & 0x1f)) + 63))
			u >>= 5
		}
		sb.WriteByte(byte(u + 63))
	}
	for _, p := range pts {
		lat := int64(math.Round(p[1] * factor))
		lon := int64(math.Round(p[0] * factor))
		enc(lat - plat)
		enc(lon - plon)
		plat, plon = lat, lon
	}
	return sb.String()
}
