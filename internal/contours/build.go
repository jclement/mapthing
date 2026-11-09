package contours

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/pmtiles"
)

// LayerName is the MVT layer the archive carries.
const LayerName = "contours"

// Defaults (see docs/architecture.md, Contours, for the measurements behind them).
const (
	DefaultMinZoom = 10
	DefaultMaxZoom = 12
	// DefaultGrid is the contouring grid (pixels per tile edge) below the max zoom;
	// DefaultGridMax is used at the max zoom, which is overzoomed to z16+ and needs the
	// finer line.
	DefaultGrid    = 256
	DefaultGridMax = 512
	// DefaultSimplify is the Douglas-Peucker tolerance in tile units (4096 per tile): 1.5
	// units is 1.5 px when a z12 tile is overzoomed to z15. At 3 the lines turn visibly
	// angular there, for 21% fewer bytes (measured: docs/architecture.md, Contours).
	DefaultSimplify = 1.5
	// DefaultMinRing drops closed lines shorter than this many grid pixels (single-pixel
	// bumps and pits in flat terrain).
	DefaultMinRing = 8.0
	// DefaultBlur is the number of 3×3 binomial smoothing passes on each grid.
	DefaultBlur = 1
)

// Interval returns the minor and major contour interval in metres for zoom z.
func Interval(z int) (minor, major int) {
	switch {
	case z >= 12:
		return 10, 50
	case z >= 10:
		return 20, 100
	default:
		return 50, 250
	}
}

// Options control a build. Zero values take the defaults.
type Options struct {
	MinZoom, MaxZoom int
	Grid, GridMax    int
	Simplify         float64
	MinRing          float64
	Blur             int // <0 disables
	Workers          int
	// Progress, if set, receives the fraction of blocks done.
	Progress func(frac float64, msg string)
	// Interval overrides the per-zoom intervals (tests).
	Interval func(z int) (minor, major int)
}

func (o *Options) defaults() {
	if o.MinZoom <= 0 {
		o.MinZoom = DefaultMinZoom
	}
	if o.MaxZoom <= 0 {
		o.MaxZoom = DefaultMaxZoom
	}
	if o.MaxZoom < o.MinZoom {
		o.MaxZoom = o.MinZoom
	}
	if o.Grid <= 0 {
		o.Grid = DefaultGrid
	}
	if o.GridMax <= 0 {
		o.GridMax = DefaultGridMax
	}
	if o.Simplify == 0 {
		o.Simplify = DefaultSimplify
	}
	if o.MinRing == 0 {
		o.MinRing = DefaultMinRing
	}
	if o.Blur == 0 {
		o.Blur = DefaultBlur
	}
	if o.Workers <= 0 {
		o.Workers = runtime.NumCPU()
	}
	if o.Interval == nil {
		o.Interval = Interval
	}
}

// Result summarises a build.
type Result struct {
	MinZoom     int            `json:"minzoom"`
	MaxZoom     int            `json:"maxzoom"`
	Tiles       int            `json:"tiles"`
	Bytes       int64          `json:"bytes"`
	TilesByZoom map[int]int    `json:"tiles_by_zoom"`
	BytesByZoom map[int]int64  `json:"bytes_by_zoom"` // gzipped tile bytes
	Intervals   map[int][2]int `json:"intervals"`     // zoom → [minor, major]
	DurationSec float64        `json:"duration_s"`
}

// Metadata is the archive's JSON metadata.
type Metadata struct {
	Name         string         `json:"name"`
	Format       string         `json:"format"`
	Type         string         `json:"type"`
	Description  string         `json:"description"`
	Attribution  string         `json:"attribution,omitempty"`
	Units        string         `json:"units"`
	Intervals    map[string]any `json:"intervals"`
	VectorLayers []vectorLayer  `json:"vector_layers"`
	Generator    string         `json:"generator"`
}

type vectorLayer struct {
	ID          string            `json:"id"`
	Description string            `json:"description"`
	Fields      map[string]string `json:"fields"`
	MinZoom     int               `json:"minzoom"`
	MaxZoom     int               `json:"maxzoom"`
}

// Build writes contours.pmtiles at outPath from the terrain archive at terrainPath.
func Build(ctx context.Context, terrainPath, outPath string, o Options) (*Result, error) {
	o.defaults()
	t0 := time.Now()
	src, err := pmtiles.Open(terrainPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = src.Close() }()
	if src.Header.TileType != pmtiles.TypePNG {
		return nil, fmt.Errorf("%s: not a PNG terrain archive", terrainPath)
	}
	var tmeta elevation.Metadata
	_ = src.MetadataJSON(&tmeta)

	b := &builder{o: o, src: src, maxz: int(src.Header.MaxZoom)}
	b.blockZ = min(o.MinZoom, b.maxz)
	b.span = 1 << (b.maxz - b.blockZ) // max-zoom tiles per block edge

	// Blocks: block-zoom tiles inside the archive bounds that the archive has.
	hb := src.Header.Bounds()
	x0, y0, x1, y1 := elevation.TileRange(b.blockZ, hb[0], hb[1], hb[2], hb[3])
	var blocks [][2]int
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			if _, ok, err := src.Get(uint8(b.blockZ), uint32(x), uint32(y)); err != nil {
				return nil, err
			} else if ok {
				blocks = append(blocks, [2]int{x, y})
			}
		}
	}

	w, err := pmtiles.Create(outPath)
	if err != nil {
		return nil, err
	}
	res := &Result{MinZoom: o.MinZoom, MaxZoom: o.MaxZoom, TilesByZoom: map[int]int{}, BytesByZoom: map[int]int64{}, Intervals: map[int][2]int{}}
	for z := o.MinZoom; z <= o.MaxZoom; z++ {
		mi, ma := o.Interval(z)
		res.Intervals[z] = [2]int{mi, ma}
	}
	var mu sync.Mutex
	var done atomic.Int64
	jobs := make(chan [2]int)
	errc := make(chan error, o.Workers)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < o.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for blk := range jobs {
				err := b.block(cctx, blk, func(z, x, y int, data []byte) error {
					mu.Lock()
					res.Tiles++
					res.TilesByZoom[z]++
					res.BytesByZoom[z] += int64(len(data))
					mu.Unlock()
					return w.AddTile(uint8(z), uint32(x), uint32(y), data)
				})
				if err != nil {
					errc <- err
					cancel()
					return
				}
				n := done.Add(1)
				if o.Progress != nil {
					o.Progress(float64(n)/float64(len(blocks)), fmt.Sprintf("contours: %d/%d blocks", n, len(blocks)))
				}
			}
		}()
	}
feed:
	for _, blk := range blocks {
		select {
		case jobs <- blk:
		case <-cctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	close(errc)
	if err := <-errc; err != nil {
		w.Abort()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		w.Abort()
		return nil, err
	}

	meta := Metadata{
		Name:        "contours",
		Format:      "pbf",
		Type:        "overlay",
		Description: "Contour lines in metres, generated from terrain.pmtiles",
		Attribution: tmeta.Attribution,
		Units:       "m",
		Intervals:   map[string]any{},
		VectorLayers: []vectorLayer{{
			ID:          LayerName,
			Description: "One feature per contour level per tile (multi-line); ele in metres, major on index contours",
			Fields:      map[string]string{"ele": "Number", "major": "Boolean"},
			MinZoom:     o.MinZoom,
			MaxZoom:     o.MaxZoom,
		}},
		Generator: "mapthing internal/contours",
	}
	for z, iv := range res.Intervals {
		meta.Intervals[fmt.Sprint(z)] = map[string]int{"minor": iv[0], "major": iv[1]}
	}
	mj, _ := json.Marshal(meta)
	h := pmtiles.Header{
		TileType:            pmtiles.TypeMVT,
		TileCompression:     pmtiles.CompressionGzip,
		InternalCompression: pmtiles.CompressionGzip,
		MinZoom:             uint8(o.MinZoom),
		MaxZoom:             uint8(o.MaxZoom),
		MinLonE7:            src.Header.MinLonE7,
		MinLatE7:            src.Header.MinLatE7,
		MaxLonE7:            src.Header.MaxLonE7,
		MaxLatE7:            src.Header.MaxLatE7,
		CenterLonE7:         src.Header.CenterLonE7,
		CenterLatE7:         src.Header.CenterLatE7,
		CenterZoom:          uint8(max(o.MinZoom, min(int(src.Header.CenterZoom), o.MaxZoom))),
	}
	if err := w.Commit(h, mj); err != nil {
		return nil, err
	}
	for _, n := range res.BytesByZoom {
		res.Bytes += n
	}
	res.DurationSec = time.Since(t0).Seconds()
	return res, nil
}

type builder struct {
	o      Options
	src    *pmtiles.Reader
	maxz   int
	blockZ int
	span   int
}

// block contours every tile of one block, for each zoom, from a mosaic of the block's
// max-zoom terrain tiles plus a one-tile ring (for the borders).
func (b *builder) block(ctx context.Context, blk [2]int, emit func(z, x, y int, data []byte) error) error {
	ts := elevation.TileSize
	m := b.span + 2
	size := m * ts
	mos := make([]float32, size*size)
	nan := float32(math.NaN())
	for i := range mos {
		mos[i] = nan
	}
	ox, oy := blk[0]*b.span-1, blk[1]*b.span-1 // mosaic origin in max-zoom tiles
	n := 1 << b.maxz
	any := false
	for ty := 0; ty < m; ty++ {
		for tx := 0; tx < m; tx++ {
			x, y := ox+tx, oy+ty
			if y < 0 || y >= n {
				continue
			}
			x = ((x % n) + n) % n
			data, ok, err := b.src.Get(uint8(b.maxz), uint32(x), uint32(y))
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			t, err := elevation.DecodeTile(data)
			if err != nil {
				return fmt.Errorf("terrain %d/%d/%d: %w", b.maxz, x, y, err)
			}
			if t.Src == nil {
				// No source chunk: a foreign Terrarium archive. 0 m is its nodata.
				for i, v := range t.Elev {
					if v == 0 {
						t.Elev[i] = nan
					}
				}
			}
			inside := tx >= 1 && tx <= b.span && ty >= 1 && ty <= b.span
			for j := 0; j < ts; j++ {
				row := (ty*ts+j)*size + tx*ts
				copy(mos[row:row+ts], t.Elev[j*ts:(j+1)*ts])
			}
			if inside {
				any = true
			}
		}
	}
	if !any {
		return nil
	}
	for z := b.o.MinZoom; z <= b.o.MaxZoom; z++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		k := z - b.blockZ // tiles per block edge at z: 1<<k
		for ty := 0; ty < 1<<k; ty++ {
			for tx := 0; tx < 1<<k; tx++ {
				x, y := blk[0]<<k+tx, blk[1]<<k+ty
				data, err := b.tile(mos, size, ox, oy, z, x, y)
				if err != nil {
					return err
				}
				if data == nil {
					continue
				}
				if err := emit(z, x, y, data); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// tile builds one gzipped MVT tile, or nil when it has no contour.
func (b *builder) tile(mos []float32, size, ox, oy, z, x, y int) ([]byte, error) {
	ts := float64(elevation.TileSize)
	N := b.o.Grid
	if z == b.o.MaxZoom {
		N = b.o.GridMax
	}
	scale := math.Exp2(float64(b.maxz - z)) // max-zoom tiles per tile edge
	s := scale * ts / float64(N)            // mosaic pixels per grid pixel
	// tile origin in mosaic pixels
	tox := (float64(x)*scale - float64(ox)) * ts
	toy := (float64(y)*scale - float64(oy)) * ts
	const pad = 2 // one border pixel for contouring, one more for the blur
	G := N + 2*pad
	g := resample(mos, size, tox-pad*s, toy-pad*s, s, G)
	for i := 0; i < b.o.Blur; i++ {
		g = blur(g, G)
	}
	// crop to the contouring grid: N+2 points, the tile's pixel centres plus one beyond
	W := N + 2
	grid := make([]float32, W*W)
	lo, hi := math.Inf(1), math.Inf(-1)
	for j := 0; j < W; j++ {
		copy(grid[j*W:(j+1)*W], g[(j+pad-1)*G+pad-1:])
		for _, v := range grid[j*W : (j+1)*W] {
			if f := float64(v); !math.IsNaN(f) {
				lo, hi = math.Min(lo, f), math.Max(hi, f)
			}
		}
	}
	if math.IsInf(lo, 1) {
		return nil, nil
	}
	minor, major := b.o.Interval(z)
	var levels []float64
	var elevs []int
	for e := int(math.Ceil(lo/float64(minor))) * minor; float64(e) <= hi; e += minor {
		if e == 0 {
			continue // sea level: coastlines and no-data edges, the water layer draws it
		}
		elevs = append(elevs, e)
		// 1 cm above the level: quantised elevations never sit exactly on it, so lines
		// do not pass through grid points (where chains would break)
		levels = append(levels, float64(e)+0.01)
	}
	if len(levels) == 0 {
		return nil, nil
	}
	lines := Isolines(grid, W, W, levels)
	// grid point (i, j) is the centre of tile pixel i-1: tile x = (i - 0.5) * Extent/N
	k := float64(Extent) / float64(N)
	var feats []Feature
	for li, ls := range lines {
		f := Feature{Ele: elevs[li], Major: elevs[li]%major == 0}
		for _, ln := range ls {
			closed := len(ln) > 2 && ln[0] == ln[len(ln)-1]
			if closed && Length(ln) < b.o.MinRing {
				continue
			}
			pts := make([]Point, len(ln))
			for i, p := range ln {
				pts[i] = Point{(p.X - 0.5) * k, (p.Y - 0.5) * k}
			}
			pts = Simplify(pts, b.o.Simplify)
			q := make([][2]int32, len(pts))
			for i, p := range pts {
				q[i] = [2]int32{int32(math.Round(p.X)), int32(math.Round(p.Y))}
			}
			f.Lines = append(f.Lines, q)
		}
		if len(f.Lines) > 0 {
			feats = append(feats, f)
		}
	}
	raw := EncodeLayer(LayerName, feats)
	if raw == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// resample returns a G×G grid whose pixel (i, j) covers mosaic pixels
// [x0+i·s, x0+(i+1)·s) × [y0+j·s, …). For s ≥ 1 it averages the valid mosaic pixels in
// that box; for s < 1 it interpolates bilinearly between valid neighbours. NaN = no data.
func resample(mos []float32, size int, x0, y0, s float64, G int) []float32 {
	out := make([]float32, G*G)
	nan := float32(math.NaN())
	at := func(x, y int) float64 {
		if x < 0 || y < 0 || x >= size || y >= size {
			return math.NaN()
		}
		return float64(mos[y*size+x])
	}
	if s >= 1 {
		si := int(math.Round(s))
		for j := 0; j < G; j++ {
			by := int(math.Round(y0 + float64(j)*s))
			for i := 0; i < G; i++ {
				bx := int(math.Round(x0 + float64(i)*s))
				var sum float64
				var n int
				for yy := by; yy < by+si; yy++ {
					for xx := bx; xx < bx+si; xx++ {
						if v := at(xx, yy); !math.IsNaN(v) {
							sum += v
							n++
						}
					}
				}
				// at least half the box must have data, so coverage edges stay put
				if n*2 >= si*si && n > 0 {
					out[j*G+i] = float32(sum / float64(n))
				} else {
					out[j*G+i] = nan
				}
			}
		}
		return out
	}
	for j := 0; j < G; j++ {
		cy := y0 + (float64(j)+0.5)*s - 0.5
		y := int(math.Floor(cy))
		fy := cy - float64(y)
		for i := 0; i < G; i++ {
			cx := x0 + (float64(i)+0.5)*s - 0.5
			x := int(math.Floor(cx))
			fx := cx - float64(x)
			var sum, wsum float64
			for _, c := range [4]struct {
				dx, dy int
				w      float64
			}{{0, 0, (1 - fx) * (1 - fy)}, {1, 0, fx * (1 - fy)}, {0, 1, (1 - fx) * fy}, {1, 1, fx * fy}} {
				if v := at(x+c.dx, y+c.dy); !math.IsNaN(v) && c.w > 0 {
					sum += v * c.w
					wsum += c.w
				}
			}
			if wsum >= 0.5 {
				out[j*G+i] = float32(sum / wsum)
			} else {
				out[j*G+i] = nan
			}
		}
	}
	return out
}

// blur applies one 3×3 binomial (1 2 1) pass over valid pixels; NaN pixels stay NaN and
// do not contribute.
func blur(g []float32, G int) []float32 {
	out := make([]float32, len(g))
	w := [3]float64{1, 2, 1}
	for j := 0; j < G; j++ {
		for i := 0; i < G; i++ {
			c := g[j*G+i]
			if math.IsNaN(float64(c)) {
				out[j*G+i] = c
				continue
			}
			var sum, ws float64
			for dy := -1; dy <= 1; dy++ {
				y := j + dy
				if y < 0 || y >= G {
					continue
				}
				for dx := -1; dx <= 1; dx++ {
					x := i + dx
					if x < 0 || x >= G {
						continue
					}
					if v := float64(g[y*G+x]); !math.IsNaN(v) {
						wt := w[dx+1] * w[dy+1]
						sum += v * wt
						ws += wt
					}
				}
			}
			out[j*G+i] = float32(sum / ws)
		}
	}
	return out
}
