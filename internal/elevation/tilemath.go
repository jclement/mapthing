package elevation

import "math"

// Web Mercator helpers. Pixel coordinates are "world pixels" at zoom z: the
// world is 256·2^z pixels square, x grows east from 180°W, y grows south from
// ~85.0511°N. Pixel (i, j) covers [i, i+1) × [j, j+1); its centre is i+0.5.

const (
	earthRadius  = 6378137.0
	maxMercLat   = 85.05112877980659
	originShiftM = math.Pi * earthRadius // 20037508.34 m
)

// LonLatToWorldPixel returns the fractional world-pixel position at zoom z.
func LonLatToWorldPixel(lon, lat float64, z int) (px, py float64) {
	lat = math.Max(-maxMercLat, math.Min(maxMercLat, lat))
	n := float64(TileSize) * math.Exp2(float64(z))
	px = (lon + 180) / 360 * n
	s := math.Sin(lat * math.Pi / 180)
	py = (0.5 - math.Log((1+s)/(1-s))/(4*math.Pi)) * n
	return px, py
}

// WorldPixelToLonLat is the inverse of LonLatToWorldPixel.
func WorldPixelToLonLat(px, py float64, z int) (lon, lat float64) {
	n := float64(TileSize) * math.Exp2(float64(z))
	lon = px/n*360 - 180
	lat = math.Atan(math.Sinh(math.Pi*(1-2*py/n))) * 180 / math.Pi
	return lon, lat
}

// TileBounds returns the lon/lat bounds (west, south, east, north) of tile z/x/y.
func TileBounds(z int, x, y int) (w, s, e, n float64) {
	w, n = WorldPixelToLonLat(float64(x*TileSize), float64(y*TileSize), z)
	e, s = WorldPixelToLonLat(float64((x+1)*TileSize), float64((y+1)*TileSize), z)
	return
}

// TileMercBounds returns the EPSG:3857 bounds (minx, miny, maxx, maxy) in metres.
func TileMercBounds(z int, x, y int) (minx, miny, maxx, maxy float64) {
	size := 2 * originShiftM / math.Exp2(float64(z))
	minx = -originShiftM + float64(x)*size
	maxx = minx + size
	maxy = originShiftM - float64(y)*size
	miny = maxy - size
	return
}

// TileRange returns the inclusive tile range at zoom z covering a lon/lat box.
func TileRange(z int, w, s, e, n float64) (x0, y0, x1, y1 int) {
	px0, py0 := LonLatToWorldPixel(w, n, z)
	px1, py1 := LonLatToWorldPixel(e, s, z)
	max := 1<<z - 1
	clamp := func(v int) int { return int(math.Max(0, math.Min(float64(max), float64(v)))) }
	x0 = clamp(int(math.Floor(px0 / TileSize)))
	y0 = clamp(int(math.Floor(py0 / TileSize)))
	x1 = clamp(int(math.Ceil(px1/TileSize)) - 1)
	y1 = clamp(int(math.Ceil(py1/TileSize)) - 1)
	if x1 < x0 {
		x1 = x0
	}
	if y1 < y0 {
		y1 = y0
	}
	return
}

// GroundResolution is the ground size in metres of one pixel at zoom z and latitude lat.
func GroundResolution(z int, lat float64) float64 {
	return 2 * originShiftM / (TileSize * math.Exp2(float64(z))) * math.Cos(lat*math.Pi/180)
}

// HaversineM is the great-circle distance in metres between two points.
func HaversineM(a, b LonLat) float64 {
	const r = 6371008.8 // mean Earth radius
	φ1 := a[1] * math.Pi / 180
	φ2 := b[1] * math.Pi / 180
	dφ := φ2 - φ1
	dλ := (b[0] - a[0]) * math.Pi / 180
	h := math.Sin(dφ/2)*math.Sin(dφ/2) + math.Cos(φ1)*math.Cos(φ2)*math.Sin(dλ/2)*math.Sin(dλ/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(h)))
}
