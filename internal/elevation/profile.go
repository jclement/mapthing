package elevation

import (
	"context"
	"errors"
	"math"
	"sort"
)

const (
	// DefaultIntervalM is the default profile sample spacing.
	DefaultIntervalM = 30.0
	// DefaultHysteresisM is the default threshold for smoothed gain/loss.
	DefaultHysteresisM = 2.0
	// DefaultSmoothWindowM is the default running-median window for gain/loss.
	DefaultSmoothWindowM = 150.0
	// MaxProfileSamples caps a profile; the interval grows to fit.
	MaxProfileSamples = 10000
)

type sampler interface {
	Sample(lon, lat float64) (Sample, error)
}

// Densify returns the points at which a profile samples path, with their
// along-path distances (great-circle). Vertices are kept and each segment is
// split evenly so no gap exceeds intervalM. With samples > 0 the path is
// instead sampled at exactly that many evenly spaced distances (first and
// last at the ends), as the Google Elevation API does. Intermediate points lie
// on the great circle between vertices at the stated distance, so samples are
// exactly evenly spaced along the path however long a segment is.
func Densify(path []LonLat, intervalM float64, samples int) ([]LonLat, []float64) {
	if len(path) == 0 {
		return nil, nil
	}
	cum := make([]float64, len(path))
	for i := 1; i < len(path); i++ {
		cum[i] = cum[i-1] + HaversineM(path[i-1], path[i])
	}
	total := cum[len(cum)-1]
	if samples > 0 {
		pts := make([]LonLat, samples)
		dist := make([]float64, samples)
		seg := 0
		for k := 0; k < samples; k++ {
			d := 0.0
			if samples > 1 {
				d = total * float64(k) / float64(samples-1)
			}
			for seg < len(path)-2 && cum[seg+1] < d {
				seg++
			}
			pts[k], dist[k] = interp(path, cum, seg, d), d
		}
		return pts, dist
	}
	if intervalM <= 0 {
		intervalM = DefaultIntervalM
	}
	// Grow the interval if the profile would be too long.
	if est := total/intervalM + float64(len(path)); est > MaxProfileSamples {
		intervalM = total / float64(MaxProfileSamples-len(path))
		if intervalM <= 0 || math.IsInf(intervalM, 0) {
			intervalM = total
		}
	}
	pts := []LonLat{path[0]}
	dist := []float64{0}
	for i := 1; i < len(path); i++ {
		segLen := cum[i] - cum[i-1]
		n := int(math.Ceil(segLen / intervalM))
		for k := 1; k < n; k++ {
			f := float64(k) / float64(n)
			pts = append(pts, greatCircle(path[i-1], path[i], f))
			dist = append(dist, cum[i-1]+f*segLen)
		}
		pts = append(pts, path[i])
		dist = append(dist, cum[i])
	}
	return pts, dist
}

func interp(path []LonLat, cum []float64, seg int, d float64) LonLat {
	if len(path) == 1 {
		return path[0]
	}
	a, b := path[seg], path[seg+1]
	l := cum[seg+1] - cum[seg]
	if l <= 0 {
		return a
	}
	f := math.Max(0, math.Min(1, (d-cum[seg])/l))
	return greatCircle(a, b, f)
}

// greatCircle returns the point a fraction f of the way from a to b along the
// great circle (spherical linear interpolation).
func greatCircle(a, b LonLat, f float64) LonLat {
	if f <= 0 {
		return a
	}
	if f >= 1 {
		return b
	}
	rad := math.Pi / 180
	φ1, λ1, φ2, λ2 := a[1]*rad, a[0]*rad, b[1]*rad, b[0]*rad
	x1, y1, z1 := math.Cos(φ1)*math.Cos(λ1), math.Cos(φ1)*math.Sin(λ1), math.Sin(φ1)
	x2, y2, z2 := math.Cos(φ2)*math.Cos(λ2), math.Cos(φ2)*math.Sin(λ2), math.Sin(φ2)
	dot := math.Max(-1, math.Min(1, x1*x2+y1*y2+z1*z2))
	δ := math.Acos(dot)
	if δ < 1e-12 { // coincident points
		return a
	}
	s := math.Sin(δ)
	ka, kb := math.Sin((1-f)*δ)/s, math.Sin(f*δ)/s
	x, y, z := ka*x1+kb*x2, ka*y1+kb*y2, ka*z1+kb*z2
	return LonLat{math.Atan2(y, x) / rad, math.Atan2(z, math.Hypot(x, y)) / rad}
}

// MedianFilter returns a running median of elev over a window of windowM
// metres centred on each sample (by the along-path distances dist), narrowed
// near the ends so it stays centred. Samples further apart than windowM are
// left unchanged.
func MedianFilter(elev, dist []float64, windowM float64) []float64 {
	out := make([]float64, len(elev))
	if windowM <= 0 || len(elev) < 3 {
		copy(out, elev)
		return out
	}
	first, last := dist[0], dist[len(dist)-1]
	var win []float64
	for i := range elev {
		// Symmetric window, shrunk near the ends so endpoints keep their own
		// value (a one-sided median would shave the start and end of a climb).
		half := math.Min(windowM/2, math.Min(dist[i]-first, last-dist[i]))
		lo, hi := i, i
		for lo > 0 && dist[lo-1] >= dist[i]-half {
			lo--
		}
		for hi+1 < len(elev) && dist[hi+1] <= dist[i]+half {
			hi++
		}
		win = append(win[:0], elev[lo:hi+1]...)
		sort.Float64s(win)
		if n := len(win); n%2 == 1 {
			out[i] = win[n/2]
		} else {
			out[i] = (win[n/2-1] + win[n/2]) / 2
		}
	}
	return out
}

// GainLoss returns the total ascent and descent of a series. With threshold
// > 0 it applies hysteresis: a change only counts once the elevation has moved
// more than threshold metres from the last turning point, which suppresses
// DEM noise on flat ground while keeping real climbs whole.
func GainLoss(elev []float64, threshold float64) (gain, loss float64) {
	if len(elev) < 2 {
		return 0, 0
	}
	if threshold <= 0 {
		for i := 1; i < len(elev); i++ {
			if d := elev[i] - elev[i-1]; d > 0 {
				gain += d
			} else {
				loss -= d
			}
		}
		return gain, loss
	}
	ref := elev[0] // last confirmed level
	dir := 0       // +1 climbing, -1 descending, 0 unknown
	ext := elev[0] // extreme reached in the current direction
	for _, e := range elev[1:] {
		switch dir {
		case 0:
			if e-ref > threshold {
				dir, ext = 1, e
			} else if ref-e > threshold {
				dir, ext = -1, e
			}
		case 1:
			if e > ext {
				ext = e
			} else if ext-e > threshold {
				gain += ext - ref
				ref, dir, ext = ext, -1, e
			}
		case -1:
			if e < ext {
				ext = e
			} else if e-ext > threshold {
				loss += ref - ext
				ref, dir, ext = ext, 1, e
			}
		}
	}
	switch dir {
	case 1:
		gain += ext - ref
	case -1:
		loss += ref - ext
	}
	return gain, loss
}

// ProfileArchive computes a profile directly from an archive (tools and tests).
func ProfileArchive(ctx context.Context, a *Archive, path []LonLat, opt ProfileOptions) (*Profile, error) {
	return profileFrom(ctx, a, path, opt)
}

func profileFrom(ctx context.Context, s sampler, path []LonLat, opt ProfileOptions) (*Profile, error) {
	if len(path) == 0 {
		return nil, errors.New("path is empty")
	}
	if opt.Samples < 0 {
		return nil, errors.New("samples must be positive")
	}
	pts, dist := Densify(path, opt.IntervalM, opt.Samples)
	p := &Profile{Samples: make([]Sample, len(pts)), MinM: math.Inf(1), MaxM: math.Inf(-1)}
	if len(dist) > 0 {
		p.LengthM = math.Round(dist[len(dist)-1]*100) / 100
	}
	var series, seriesDist []float64
	seen := map[string]bool{}
	for i, pt := range pts {
		if i%256 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		smp, err := s.Sample(pt[0], pt[1])
		if err != nil {
			return nil, err
		}
		smp.DistanceM = math.Round(dist[i]*100) / 100
		p.Samples[i] = smp
		if smp.Elevation != nil {
			e := *smp.Elevation
			series = append(series, e)
			seriesDist = append(seriesDist, dist[i])
			p.MinM = math.Min(p.MinM, e)
			p.MaxM = math.Max(p.MaxM, e)
		}
		if smp.Source != "" && !seen[smp.Source] {
			seen[smp.Source] = true
			p.Sources = append(p.Sources, smp.Source)
		}
	}
	if len(series) == 0 {
		p.MinM, p.MaxM = 0, 0
	}
	h := opt.HysteresisM
	if h == 0 {
		h = DefaultHysteresisM
	}
	if h < 0 {
		h = 0
	}
	w := opt.SmoothWindowM
	if w == 0 {
		w = DefaultSmoothWindowM
	}
	if w < 0 {
		w = 0
	}
	p.HysteresisM, p.SmoothWindowM = h, w
	p.GainRawM, p.LossRawM = GainLoss(series, 0)
	p.GainM, p.LossM = GainLoss(MedianFilter(series, seriesDist, w), h)
	for _, f := range []*float64{&p.GainM, &p.LossM, &p.GainRawM, &p.LossRawM} {
		*f = math.Round(*f*100) / 100
	}
	if p.Sources == nil {
		p.Sources = []string{}
	}
	return p, nil
}
