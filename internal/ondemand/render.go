package ondemand

import (
	"context"
	"math"
	"sync"

	"github.com/jclement/mapthing/internal/cog"
	"github.com/jclement/mapthing/internal/elevation"
)

const (
	tileN = elevation.TileSize
	// gridStep is the spacing, in tile pixels, of the nodes at which tile
	// positions are projected exactly into a source's CRS; positions between
	// nodes are interpolated. Over one tile of zoom 8 or deeper the error is
	// far below a source pixel.
	gridStep = 8
	gridN    = tileN/gridStep + 1
)

// Rendered is a terrain tile built from the sources.
type Rendered struct {
	// Elev holds TileSize² elevations in metres, NaN where no source has data.
	Elev []float32
	// Src holds the per-pixel source number (0 = none; otherwise the 1-based
	// position in elevation.KnownSources).
	Src []uint8
	// Filled is the number of pixels with data.
	Filled int
	// Files lists the versions of every remote file that was a candidate for
	// this tile, used or not: the tile is current for as long as these are.
	Files []FileVersion
}

// FileVersion pins one remote file at one release.
type FileVersion struct {
	URL string
	Tag string
}

// render builds tile z/x/y by asking each source in priority order for the
// pixels still empty. Any upstream failure fails the whole tile: a tile built
// without a source that should have contributed must not be mistaken for the
// real thing.
func (s *Service) render(ctx context.Context, z, x, y int, files []candidate) (*Rendered, error) {
	out := &Rendered{Elev: make([]float32, tileN*tileN), Src: make([]uint8, tileN*tileN)}
	nan := float32(math.NaN())
	for i := range out.Elev {
		out.Elev[i] = nan
	}
	for _, c := range files {
		out.Files = append(out.Files, FileVersion{URL: c.url, Tag: c.version.Tag})
	}
	// Longitude and latitude of the grid nodes (pixel corners every gridStep pixels).
	var lon [gridN]float64
	var lat [gridN]float64
	for i := 0; i < gridN; i++ {
		lon[i], _ = elevation.WorldPixelToLonLat(float64(x*tileN+i*gridStep), 0, z)
		_, lat[i] = elevation.WorldPixelToLonLat(0, float64(y*tileN+i*gridStep), z)
	}
	// Read every candidate's header at once: where the best source has holes
	// the next one is needed straight away, and a header is one small read,
	// kept from then on.
	var wg sync.WaitGroup
	for _, c := range files {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.open(ctx, c) // an error surfaces in fill, if the file is needed
		}()
	}
	wg.Wait()
	for _, c := range files {
		if out.Filled == tileN*tileN {
			break
		}
		if err := s.fill(ctx, out, c, &lon, &lat); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// fill samples one file into the pixels of out that are still empty.
func (s *Service) fill(ctx context.Context, out *Rendered, c candidate, lon, lat *[gridN]float64) error {
	f, err := s.open(ctx, c)
	if err != nil {
		return err
	}
	// Source CRS position of every grid node.
	var gx, gy [gridN * gridN]float64
	for j := 0; j < gridN; j++ {
		for i := 0; i < gridN; i++ {
			px, py := lon[i], lat[j]
			if c.src.project != nil {
				px, py = c.src.project(px, py)
			}
			gx[j*gridN+i], gy[j*gridN+i] = px, py
		}
	}
	// Size of one tile pixel in source units, at the tile centre.
	mid := (gridN/2)*gridN + gridN/2
	dx := math.Hypot(gx[mid+1]-gx[mid], gy[mid+1]-gy[mid]) / gridStep
	dy := math.Hypot(gx[mid+gridN]-gx[mid], gy[mid+gridN]-gy[mid]) / gridStep
	level, ss := pickLevel(f.cog, math.Min(dx, dy), s.opt.LevelSlack)
	lv := &f.cog.Levels[level]

	// Positions (in level pixels, centre-based) of every sub-sample of every
	// empty pixel; and the blocks they touch.
	type key struct{ col, row int }
	need := map[key]bool{}
	pos := make([]float64, 0, 2*ss*ss*(tileN*tileN-out.Filled))
	idx := make([]int32, 0, tileN*tileN-out.Filled)
	for j := 0; j < tileN; j++ {
		for i := 0; i < tileN; i++ {
			if out.Src[j*tileN+i] != 0 {
				continue
			}
			inside := false
			for sj := 0; sj < ss; sj++ {
				for si := 0; si < ss; si++ {
					u := float64(i) + (float64(si)+0.5)/float64(ss)
					v := float64(j) + (float64(sj)+0.5)/float64(ss)
					sx, sy := gridAt(&gx, &gy, u, v)
					fx := (sx-f.cog.OriginX)/lv.PixelW - 0.5
					fy := (f.cog.OriginY-sy)/lv.PixelH - 0.5
					pos = append(pos, fx, fy)
					if fx < -0.5 || fy < -0.5 || fx > float64(lv.Width)-0.5 || fy > float64(lv.Height)-0.5 {
						continue
					}
					inside = true
					x0, y0 := int(math.Floor(fx)), int(math.Floor(fy))
					for _, d := range [4][2]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
						px, py := x0+d[0], y0+d[1]
						if px >= 0 && py >= 0 && px < lv.Width && py < lv.Height {
							need[key{px / lv.TileW, py / lv.TileH}] = true
						}
					}
				}
			}
			if !inside {
				pos = pos[:len(pos)-2*ss*ss]
				continue
			}
			idx = append(idx, int32(j*tileN+i))
		}
	}
	if len(idx) == 0 {
		return nil
	}
	// Fetch the blocks together; the Upstream caps how many go out at once.
	blocks := make(map[key][]float32, len(need))
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		ferr error
	)
	for k := range need {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := s.block(ctx, f, level, k.col, k.row)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && ferr == nil {
				ferr = err
			}
			blocks[k] = b
		}()
	}
	wg.Wait()
	if ferr != nil {
		return ferr
	}
	pixel := func(px, py int) float32 {
		if px < 0 || py < 0 || px >= lv.Width || py >= lv.Height {
			return float32(math.NaN())
		}
		b := blocks[key{px / lv.TileW, py / lv.TileH}]
		if b == nil {
			return float32(math.NaN())
		}
		return b[(py%lv.TileH)*lv.TileW+px%lv.TileW]
	}
	per := 2 * ss * ss
	for n, at := range idx {
		p := pos[n*per : (n+1)*per]
		var sum, wsum float64
		for k := 0; k < per; k += 2 {
			fx, fy := p[k], p[k+1]
			x0, y0 := int(math.Floor(fx)), int(math.Floor(fy))
			tx, ty := fx-float64(x0), fy-float64(y0)
			for _, q := range [4]struct {
				dx, dy int
				w      float64
			}{{0, 0, (1 - tx) * (1 - ty)}, {1, 0, tx * (1 - ty)}, {0, 1, (1 - tx) * ty}, {1, 1, tx * ty}} {
				if q.w == 0 {
					continue
				}
				e := pixel(x0+q.dx, y0+q.dy)
				if e != e {
					continue
				}
				sum += float64(e) * q.w
				wsum += q.w
			}
		}
		// A pixel counts as covered when at least half of its footprint has
		// data; thinner slivers are left for the next source.
		if wsum < float64(ss*ss)/2 {
			continue
		}
		out.Elev[at] = float32(sum / wsum)
		out.Src[at] = c.src.index
		out.Filled++
	}
	return nil
}

// gridAt interpolates the source position of tile pixel position (u, v).
func gridAt(gx, gy *[gridN * gridN]float64, u, v float64) (float64, float64) {
	i, j := int(u)/gridStep, int(v)/gridStep
	if i >= gridN-1 {
		i = gridN - 2
	}
	if j >= gridN-1 {
		j = gridN - 2
	}
	tx, ty := u/gridStep-float64(i), v/gridStep-float64(j)
	a, b, c, d := j*gridN+i, j*gridN+i+1, (j+1)*gridN+i, (j+1)*gridN+i+1
	w00, w10, w01, w11 := (1-tx)*(1-ty), tx*(1-ty), (1-tx)*ty, tx*ty
	return gx[a]*w00 + gx[b]*w10 + gx[c]*w01 + gx[d]*w11, gy[a]*w00 + gy[b]*w10 + gy[c]*w01 + gy[d]*w11
}

// pickLevel chooses the coarsest level that is still at least as fine as the
// tile (so nothing is lost), and how many sub-samples per axis to average
// when that level is clearly finer than the tile (so nothing aliases).
func pickLevel(f *cog.File, tilePixel, slack float64) (level, ss int) {
	for i := range f.Levels {
		if math.Max(f.Levels[i].PixelW, f.Levels[i].PixelH) <= tilePixel*slack {
			level = i
		}
	}
	ss = 1
	if tilePixel/math.Max(f.Levels[level].PixelW, f.Levels[level].PixelH) > 1.2 {
		ss = 2
	}
	return level, ss
}
