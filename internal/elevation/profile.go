package elevation

import (
	"context"
	"errors"
	"math"
)

const (
	// DefaultIntervalM is the default profile sample spacing.
	DefaultIntervalM = 30.0
	// DefaultHysteresisM is the default threshold for smoothed gain/loss.
	DefaultHysteresisM = 2.0
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
// last at the ends), as the Google Elevation API does. Intermediate points
// are interpolated linearly in lon/lat, which is indistinguishable from the
// great circle at sample spacing.
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
			pts = append(pts, LonLat{path[i-1][0] + f*(path[i][0]-path[i-1][0]), path[i-1][1] + f*(path[i][1]-path[i-1][1])})
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
	return LonLat{a[0] + f*(b[0]-a[0]), a[1] + f*(b[1]-a[1])}
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
	var series []float64
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
	p.HysteresisM = h
	p.GainRawM, p.LossRawM = GainLoss(series, 0)
	p.GainM, p.LossM = GainLoss(series, h)
	for _, f := range []*float64{&p.GainM, &p.LossM, &p.GainRawM, &p.LossRawM} {
		*f = math.Round(*f*100) / 100
	}
	if p.Sources == nil {
		p.Sources = []string{}
	}
	return p, nil
}
