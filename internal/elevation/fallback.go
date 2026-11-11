package elevation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// Fallback supplies terrain where the live archive has none: on-demand
// terrain (internal/ondemand), read from the DEM hosts at request time and
// cached. Built terrain always wins; the fallback is only asked about points
// the archive cannot answer.
type Fallback interface {
	// Zoom is the zoom of the tiles Tile returns.
	Zoom() int
	// Covers reports whether the fallback applies at a point.
	Covers(lon, lat float64) bool
	// Prepare fetches the given tiles (x, y at Zoom) together, so that the
	// Tile calls that follow are quick.
	Prepare(ctx context.Context, tiles [][2]int) error
	// Tile returns one decoded tile, nil when no source has data there. Its
	// Src indices are 1-based positions in KnownSources.
	Tile(ctx context.Context, x, y int) (*Tile, error)
}

// ErrSourceUnavailable means elevation had to be read from a DEM host and the
// host did not answer. The request can be tried again; nothing was cached.
var ErrSourceUnavailable = errors.New("elevation source unavailable")

// LimitedError means a request that needed data from the DEM hosts was
// refused by a rate limit.
type LimitedError struct {
	RetryAfter time.Duration
	Message    string
}

func (e *LimitedError) Error() string { return e.Message }

// MaxFallbackTiles bounds how much one request may fetch on demand: about
// 2000 km of profile. Past it the request is refused rather than leaning on
// the DEM hosts for minutes; warming the area or building the region is the
// answer for such requests.
const MaxFallbackTiles = 600

// ErrTooLarge is returned for a request that would need more than
// MaxFallbackTiles on-demand tiles.
var ErrTooLarge = fmt.Errorf("the request covers too much ground to read on demand (more than %d terrain tiles); warm the area first or build the region", MaxFallbackTiles)

// grid is a tile source that can be sampled: the archive or the fallback.
type grid struct {
	zoom  int
	pixel func(px, py int) (float32, uint8, error)
	// source describes a per-pixel source index.
	source func(idx uint8) (SourceInfo, bool)
}

// sample returns the bilinearly interpolated elevation at a point. Pixel
// centres sit at (i+0.5, j+0.5); neighbours without data are dropped and the
// remaining weights renormalised. The source is the nearest pixel's source.
func (g grid) sample(lon, lat float64) (Sample, error) {
	s := Sample{Lon: lon, Lat: lat}
	if math.IsNaN(lon) || math.IsNaN(lat) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return s, fmt.Errorf("invalid coordinate %v,%v", lon, lat)
	}
	if math.Abs(lat) > maxMercLat {
		return s, nil
	}
	px, py := LonLatToWorldPixel(lon, lat, g.zoom)
	fx, fy := px-0.5, py-0.5
	x0, y0 := int(math.Floor(fx)), int(math.Floor(fy))
	tx, ty := fx-float64(x0), fy-float64(y0)
	var sum, wsum float64
	for _, c := range [4]struct {
		dx, dy int
		w      float64
	}{{0, 0, (1 - tx) * (1 - ty)}, {1, 0, tx * (1 - ty)}, {0, 1, (1 - tx) * ty}, {1, 1, tx * ty}} {
		e, _, err := g.pixel(x0+c.dx, y0+c.dy)
		if err != nil {
			return s, err
		}
		if math.IsNaN(float64(e)) || c.w == 0 {
			continue
		}
		sum += float64(e) * c.w
		wsum += c.w
	}
	if wsum == 0 {
		return s, nil
	}
	v := math.Round(sum/wsum*100) / 100 // centimetres are plenty
	s.Elevation = &v
	_, src, err := g.pixel(int(math.Floor(px)), int(math.Floor(py)))
	if err != nil {
		return s, err
	}
	if src == 0 { // nearest pixel empty but a neighbour has data: take any neighbour's source
		for _, d := range [4][2]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
			if _, src, _ = g.pixel(x0+d[0], y0+d[1]); src != 0 {
				break
			}
		}
	}
	if info, ok := g.source(src); ok {
		s.Source = info.ID
		s.Resolution = math.Round(math.Max(info.ResolutionM, GroundResolution(g.zoom, lat))*10) / 10
	}
	return s, nil
}

// neighbours lists the tiles (at zoom z) that sampling a point reads.
func neighbours(lon, lat float64, z int, add func(x, y int)) {
	if math.Abs(lat) > maxMercLat {
		return
	}
	px, py := LonLatToWorldPixel(lon, lat, z)
	x0, y0 := int(math.Floor(px-0.5)), int(math.Floor(py-0.5))
	n := 1 << z
	for _, d := range [4][2]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
		tx, ty := floorDiv(x0+d[0], TileSize), floorDiv(y0+d[1], TileSize)
		if ty < 0 || ty >= n {
			continue
		}
		add(((tx%n)+n)%n, ty)
	}
}

// mixed samples the archive first and the fallback where the archive has
// nothing. Either may be absent.
type mixed struct {
	ctx context.Context
	arc *Archive
	fb  Fallback
}

func (m *mixed) fallbackGrid() grid {
	n := 1 << m.fb.Zoom()
	return grid{zoom: m.fb.Zoom(),
		pixel: func(px, py int) (float32, uint8, error) {
			ty := floorDiv(py, TileSize)
			if ty < 0 || ty >= n {
				return float32(math.NaN()), 0, nil
			}
			t, err := m.fb.Tile(m.ctx, ((floorDiv(px, TileSize)%n)+n)%n, ty)
			if err != nil || t == nil {
				return float32(math.NaN()), 0, err
			}
			i := mod(py, TileSize)*TileSize + mod(px, TileSize)
			var s uint8
			if t.Src != nil {
				s = t.Src[i]
			}
			return t.Elev[i], s, nil
		},
		source: func(idx uint8) (SourceInfo, bool) {
			if int(idx) < 1 || int(idx) > len(KnownSources) {
				return SourceInfo{}, false
			}
			return KnownSources[idx-1].SourceInfo, true
		},
	}
}

// Sample implements sampler.
func (m *mixed) Sample(lon, lat float64) (Sample, error) {
	s := Sample{Lon: lon, Lat: lat}
	if m.arc != nil {
		var err error
		if s, err = m.arc.Sample(lon, lat); err != nil || s.Elevation != nil {
			return s, err
		}
	}
	if m.fb == nil || !m.fb.Covers(lon, lat) {
		return s, nil
	}
	return m.fallbackGrid().sample(lon, lat)
}

// prepare fetches, together, the fallback tiles that sampling pts will need.
func (m *mixed) prepare(pts []LonLat) error {
	if m.fb == nil {
		return nil
	}
	seen := map[[2]int]bool{}
	var tiles [][2]int
	for i, p := range pts {
		if i%256 == 0 && m.ctx.Err() != nil {
			return m.ctx.Err()
		}
		if math.IsNaN(p[0]) || math.IsNaN(p[1]) || p[1] < -90 || p[1] > 90 || p[0] < -180 || p[0] > 180 {
			continue // Sample reports it
		}
		if m.arc != nil {
			if s, err := m.arc.Sample(p[0], p[1]); err != nil || s.Elevation != nil {
				continue
			}
		}
		if !m.fb.Covers(p[0], p[1]) {
			continue
		}
		neighbours(p[0], p[1], m.fb.Zoom(), func(x, y int) {
			if k := [2]int{x, y}; !seen[k] {
				seen[k] = true
				tiles = append(tiles, k)
			}
		})
		if len(tiles) > MaxFallbackTiles {
			return ErrTooLarge
		}
	}
	if len(tiles) == 0 {
		return nil
	}
	return m.fb.Prepare(m.ctx, tiles)
}
