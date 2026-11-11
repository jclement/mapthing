package ondemand

import "math"

// Lambert conformal conic (two standard parallels) on GRS80, forward only:
// what the tile builder needs to find a longitude/latitude in the NRCan
// rasters, which are in EPSG:3979 (NAD83(CSRS) / Canada Atlas Lambert).
// NAD83(CSRS) and WGS84 differ by under two metres, below what a 2 m DEM
// sampled into terrain tiles can show; the build's gdalwarp treats them as
// equal too.
type lcc struct {
	a, e, n, f, rho0, lon0 float64
}

func newLCC(lat1, lat2, lat0, lon0 float64) *lcc {
	const (
		a  = 6378137.0
		fl = 1 / 298.257222101
	)
	e := math.Sqrt(2*fl - fl*fl)
	rad := math.Pi / 180
	m := func(phi float64) float64 {
		s := math.Sin(phi)
		return math.Cos(phi) / math.Sqrt(1-e*e*s*s)
	}
	p := &lcc{a: a, e: e, lon0: lon0 * rad}
	m1, m2 := m(lat1*rad), m(lat2*rad)
	t1, t2, t0 := p.t(lat1*rad), p.t(lat2*rad), p.t(lat0*rad)
	p.n = (math.Log(m1) - math.Log(m2)) / (math.Log(t1) - math.Log(t2))
	p.f = m1 / (p.n * math.Pow(t1, p.n))
	p.rho0 = a * p.f * math.Pow(t0, p.n)
	return p
}

func (p *lcc) t(phi float64) float64 {
	s := p.e * math.Sin(phi)
	return math.Tan(math.Pi/4-phi/2) / math.Pow((1-s)/(1+s), p.e/2)
}

// forward returns easting and northing in metres.
func (p *lcc) forward(lon, lat float64) (x, y float64) {
	rad := math.Pi / 180
	rho := p.a * p.f * math.Pow(p.t(lat*rad), p.n)
	theta := p.n * (lon*rad - p.lon0)
	return rho * math.Sin(theta), p.rho0 - rho*math.Cos(theta)
}

// canadaAtlas is EPSG:3979.
var canadaAtlas = newLCC(49, 77, 49, -95)
