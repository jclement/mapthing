package build

// Terrain build: DEM sources → terrain.pmtiles (256 px Terrarium PNG tiles in
// Web Mercator). See docs/elevation-sources.md for the sources and
// docs/architecture.md (Elevation) for the contract.
//
// Outline:
//  1. Pick a max zoom from the best source resolution (z13 for 10 m sources and
//     LiDAR, z12 for 30 m-only areas), lowered while the estimated tile count
//     exceeds the budget.
//  2. Cut the area into chunks of 16×16 max-zoom tiles (4096 px). For each
//     chunk, warp sources in priority order (gdalwarp → raw float32) until no
//     nodata remains; higher priority wins, nodata falls through. Source
//     windows are fetched lazily into the cache (1° cells, clipped to the area,
//     at about the target resolution, LERC-compressed with ≤5 cm error).
//  3. Encode max-zoom tiles with the per-pixel source index; build four lower
//     zooms from the chunk raster by 2×2 averaging of elevations (never of
//     encoded RGB), and the rest of the pyramid from the chunk-level tiles.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jclement/mapthing/internal/catalog"
	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/pmtiles"
)

func init() {
	RegisterTerrain(BuildTerrain)
	RegisterTerrainEstimate(EstimateTerrainBytes)
	RegisterTerrainFingerprint(TerrainFingerprint)
	RegisterElevationCheck(CheckTerrain)
}

// Terrain build tunables (environment variables).
const (
	envMaxZoom  = "MAPTHING_TERRAIN_MAXZOOM"   // force a max zoom
	envMaxTiles = "MAPTHING_TERRAIN_MAX_TILES" // tile budget at max zoom
	envWorkers  = "MAPTHING_TERRAIN_WORKERS"
	envQuantum  = "MAPTHING_TERRAIN_QUANTUM_M" // elevation rounding step, metres
	envCacheDir = "MAPTHING_ELEVATION_CACHE"   // default <MAPTHING_DATA>/elevation/cache

	// DefaultTerrainMaxZoom caps auto zoom selection (~16 m pixels at 30°N, 11 m at 55°N).
	DefaultTerrainMaxZoom = 13
	// DefaultTerrainMaxTiles is the default max-zoom tile budget. Measured
	// archive bytes per max-zoom tile (lower zooms included): 27 KB on plains,
	// 41–54 KB in hills and foothills, 72–74 KB in mountains and desert DSM;
	// planning figure 50 KB, so the default is about 20 GB.
	DefaultTerrainMaxTiles = 400000
	// chunkShift: chunks are 2^chunkShift max-zoom tiles square.
	chunkShift = 4
	// cacheResFactor: cache windows hold pixels this fraction of the max-zoom pixel size.
	cacheResFactor = 0.8
	// TerrainQuantumM: max-zoom elevations are rounded to this step before
	// Terrarium encoding (≤ 12.5 cm error, below DEM accuracy). Terrarium's
	// low byte at full 1/256 m precision is noise PNG cannot compress; 1/4 m
	// cuts max-zoom tiles 2.4–3× (measured in docs/elevation-sources.md).
	// Steps are powers of two so unused low bits stay zero; 0.1 m compresses
	// worse than 1/8 m. Each lower zoom doubles the step (constant slope
	// precision, so hillshade looks the same) up to terrainQuantumMaxM.
	TerrainQuantumM    = 0.25
	terrainQuantumMaxM = 8.0
	// estBytesPerTile is the planning figure for disk estimates.
	estBytesPerTile = 37500 // max-zoom tile alone; ×4/3 for lower zooms ≈ 50 KB (see DefaultTerrainMaxTiles)
)

// TerrainResult summarises a build; it is also written to <out>.json.
type TerrainResult struct {
	Regions     []string               `json:"regions"`
	MinZoom     int                    `json:"minzoom"`
	MaxZoom     int                    `json:"maxzoom"`
	TileSize    int                    `json:"tile_size"`
	Tiles       int                    `json:"tiles"`
	Bytes       int64                  `json:"bytes"`
	Sources     []elevation.MetaSource `json:"sources"`
	Fetched     int                    `json:"pieces_fetched"`
	Reused      int                    `json:"pieces_reused"`
	DurationSec float64                `json:"duration_s"`
	BuiltAt     string                 `json:"built_at"`
	// OnDemand lists the regions whose terrain is on demand (low zooms only in this archive).
	OnDemand []string `json:"on_demand,omitempty"`
	// Fingerprint identifies the areas and settings this archive was built for
	// (terrain_reuse.go); a later build with the same fingerprint may reuse the archive.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// BuildTerrain is the TerrainFunc registered with the pipeline.
func BuildTerrain(ctx context.Context, r Reporter, regions []catalog.Region, workDir, outPath string) error {
	_, err := buildTerrain(ctx, r, areasFromRegions(regions), workDir, outPath, terrainOptsFromEnv())
	return err
}

type terrainOpts struct {
	maxZoom  int // 0 = auto
	minZoom  int
	maxTiles int
	workers  int
	quantum  float64 // rounding step before encoding, metres
	cacheDir string
}

func terrainOptsFromEnv() terrainOpts {
	o := terrainOpts{maxTiles: DefaultTerrainMaxTiles}
	if v, err := strconv.Atoi(os.Getenv(envMaxZoom)); err == nil && v > 0 && v <= 15 {
		o.maxZoom = v
	}
	if v, err := strconv.Atoi(os.Getenv(envMaxTiles)); err == nil && v > 0 {
		o.maxTiles = v
	}
	if v, err := strconv.Atoi(os.Getenv(envWorkers)); err == nil && v > 0 {
		o.workers = v
	}
	o.quantum = TerrainQuantumM
	if v, err := strconv.ParseFloat(os.Getenv(envQuantum), 64); err == nil && v > 0 && v <= 10 {
		o.quantum = v
	}
	o.cacheDir = os.Getenv(envCacheDir)
	if o.cacheDir == "" {
		data := os.Getenv("MAPTHING_DATA")
		if data == "" {
			data = "/data"
		}
		o.cacheDir = filepath.Join(data, "elevation", "cache")
	}
	return o
}

// bestResM returns the finest source resolution expected over an area.
func bestResM(a area) float64 {
	best := math.Inf(1)
	for _, s := range elevation.CoverageFor(a.bbox) {
		best = math.Min(best, s.ResolutionM)
	}
	return best
}

// chooseMaxZoom picks the coarsest zoom whose pixels are no larger than 1.7×
// the best source resolution at the area's mid-latitude, capped at
// DefaultTerrainMaxZoom; then lowers it while the tile count exceeds maxTiles.
func chooseMaxZoom(areas []area, o terrainOpts) (int, int) {
	areas = fullAreas(areas) // on-demand regions have no deep tiles to count or to choose a zoom for
	z := o.maxZoom
	if z == 0 {
		for _, a := range areas {
			res := bestResM(a)
			lat := (a.bbox[1] + a.bbox[3]) / 2
			az := DefaultTerrainMaxZoom
			for zz := 8; zz <= DefaultTerrainMaxZoom; zz++ {
				if elevation.GroundResolution(zz, lat) <= 1.7*res {
					az = zz
					break
				}
			}
			z = max(z, az)
		}
		if z == 0 {
			z = DefaultTerrainMaxZoom
		}
	}
	n := countTiles(areas, z)
	if o.maxZoom == 0 {
		for z > 8 && n > o.maxTiles {
			z--
			n = countTiles(areas, z)
		}
	}
	return z, n
}

// countTiles counts max-zoom tiles touching any area.
func countTiles(areas []area, z int) int {
	seen := map[[2]int]bool{}
	for _, a := range areas {
		x0, y0, x1, y1 := elevation.TileRange(z, a.bbox[0], a.bbox[1], a.bbox[2], a.bbox[3])
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				if seen[[2]int{x, y}] {
					continue
				}
				if len(a.polygon) > 0 {
					w, s, e, n := elevation.TileBounds(z, x, y)
					if !a.intersectsBox(elevation.BBox{w, s, e, n}) {
						continue
					}
				}
				seen[[2]int{x, y}] = true
			}
		}
	}
	return len(seen)
}

// EstimateTerrainBytes estimates disk needed for a build (archive plus new
// cache), for the pipeline's disk guard.
func EstimateTerrainBytes(regions []catalog.Region) int64 {
	areas := areasFromRegions(regions)
	if len(areas) == 0 {
		return 0
	}
	z, n := chooseMaxZoom(areas, terrainOptsFromEnv())
	archive := int64(float64(n) * estBytesPerTile * 4 / 3)
	// On-demand regions: only their low zooms, a tile of the chunk level for every 256 deep ones.
	archive += int64(float64(countTiles(lowAreas(areas), max(0, z-chunkShift))) * estBytesPerTile * 2 * 4 / 3)
	// Cache: ~1.6 px per tile pixel (0.8 res factor) at ~0.6 B/px with LERC
	// (measured ~0.45 B/px on 3DEP west Texas at MAX_Z_ERROR 0.05).
	cache := int64(float64(n) * elevation.TileSize * elevation.TileSize * 1.56 * 0.6)
	return archive + cache
}

// CheckTerrain verifies that the archive answers at every point.
func CheckTerrain(ctx context.Context, terrainPath string, pts [][2]float64) error {
	a, err := elevation.OpenArchive(terrainPath, 64)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()
	var missing []string
	for _, p := range pts {
		s, err := a.Sample(p[0], p[1])
		if err != nil {
			return err
		}
		if s.Elevation == nil {
			missing = append(missing, fmt.Sprintf("%.5f,%.5f", p[0], p[1]))
		} else if *s.Elevation < -500 || *s.Elevation > 9000 {
			missing = append(missing, fmt.Sprintf("%.5f,%.5f (implausible %.1f m)", p[0], p[1], *s.Elevation))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("terrain: no elevation at %s", strings.Join(missing, "; "))
	}
	return nil
}

// chunk is a block of 2^chunkShift × 2^chunkShift max-zoom tiles.
type chunk struct{ cx, cy int }

type builder struct {
	o       terrainOpts
	areas   []area
	z       int // max zoom
	minZoom int
	shift   int // pyramid levels built inside a chunk
	srcs    []*demSource
	fetch   *fetcher
	gdal    *gdalRunner
	w       *pmtiles.Writer
	workDir string
	report  Reporter

	mu        sync.Mutex
	base      map[[2]int]*elevTile // tiles at zoom z-shift, for the low-zoom pyramid
	pixels    []int64              // per source
	footprint []map[[2]int]bool    // per source: chunks with data
}

type elevTile struct {
	elev []float32
}

func buildTerrain(ctx context.Context, r Reporter, areas []area, workDir, outPath string, o terrainOpts) (*TerrainResult, error) {
	start := time.Now()
	if len(areas) == 0 {
		return nil, errors.New("terrain: no regions")
	}
	r.Stage("terrain")
	workDir, o.cacheDir = absPath(workDir), absPath(o.cacheDir)
	z, n := chooseMaxZoom(areas, o)
	if o.workers <= 0 {
		o.workers = max(1, min(4, runtime.NumCPU()/2))
	}
	for _, d := range []string{workDir, o.cacheDir, filepath.Dir(outPath)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	fingerprint := terrainFingerprint(areas, o)
	g := newGDALRunner(absPath(workDir), absPath(o.cacheDir))
	if len(fullAreas(areas)) > 0 { // on-demand regions' low zooms are read without GDAL
		if err := g.available(ctx); err != nil {
			return nil, err
		}
	}
	r.Logf("terrain: %d region(s), max zoom %d, %d max-zoom tiles (budget %d), %d workers, cache %s",
		len(areas), z, n, o.maxTiles, o.workers, o.cacheDir)

	low := lowAreas(areas)
	b := &builder{o: o, areas: fullAreas(areas), z: z, minZoom: o.minZoom, srcs: demSources(), gdal: g, workDir: workDir, report: r,
		base: map[[2]int]*elevTile{}}
	b.shift = min(chunkShift, z-b.minZoom)
	b.pixels = make([]int64, len(b.srcs))
	b.footprint = make([]map[[2]int]bool, len(b.srcs))
	for i := range b.footprint {
		b.footprint[i] = map[[2]int]bool{}
	}
	b.fetch = newFetcher(o.cacheDir, g, r, func(lat float64) float64 {
		return cacheResFactor * elevation.GroundResolution(z, lat)
	})
	w, err := pmtiles.Create(outPath)
	if err != nil {
		return nil, err
	}
	b.w = w
	ok := false
	defer func() {
		if !ok {
			w.Abort()
		}
	}()

	chunks := b.chunks()
	r.Logf("terrain: %d chunks of %d×%d tiles", len(chunks), 1<<chunkShift, 1<<chunkShift)
	var done atomic.Int64
	jobs := make(chan chunk)
	errc := make(chan error, o.workers)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < o.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range jobs {
				if err := b.processChunk(cctx, c); err != nil {
					errc <- err
					cancel()
					return
				}
				d := done.Add(1)
				r.Progress(0.95*float64(d)/float64(len(chunks)), fmt.Sprintf("terrain chunk %d/%d", d, len(chunks)))
			}
		}()
	}
feed:
	for _, c := range chunks {
		select {
		case jobs <- c:
		case <-cctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errc:
		return nil, err
	default:
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if len(low) > 0 {
		r.Progress(0.95, "terrain: low zooms of on-demand regions")
		if err := b.onDemandBase(ctx, low); err != nil {
			return nil, err
		}
	}
	r.Progress(0.96, "terrain: low zooms")
	if err := b.lowZooms(); err != nil {
		return nil, err
	}

	// Metadata: only sources that contributed, in priority order; the
	// per-pixel indices written in processChunk are 1-based positions in b.srcs,
	// so keep every source in the table and mark unused ones with 0 pixels.
	bounds := areas[0].bbox
	var ids, onDemand []string
	for _, a := range areas {
		bounds = elevation.BBox{math.Min(bounds[0], a.bbox[0]), math.Min(bounds[1], a.bbox[1]), math.Max(bounds[2], a.bbox[2]), math.Max(bounds[3], a.bbox[3])}
		ids = append(ids, a.id)
		if a.lowOnly {
			onDemand = append(onDemand, a.id)
		}
	}
	meta := elevation.Metadata{Name: "mapthing terrain", Format: "png", Encoding: "terrarium", TileSize: elevation.TileSize,
		MinZoom: b.minZoom, MaxZoom: z, Bounds: bounds, Regions: ids, OnDemand: onDemand, BuiltAt: time.Now().UTC().Format(time.RFC3339)}
	var attrs []string
	for i, s := range b.srcs {
		ms := elevation.MetaSource{SourceInfo: s.def.SourceInfo, Datum: s.def.Datum, Kind: s.def.Kind, Pixels: b.pixels[i]}
		ms.Footprint = b.footprintBoxes(i)
		meta.Sources = append(meta.Sources, ms)
		if b.pixels[i] > 0 {
			attrs = append(attrs, s.def.Attribution)
		}
	}
	meta.Attribution = strings.Join(attrs, "; ")
	h := pmtiles.Header{TileType: pmtiles.TypePNG, TileCompression: pmtiles.CompressionNone,
		MinZoom: uint8(b.minZoom), MaxZoom: uint8(z),
		MinLonE7: int32(bounds[0] * 1e7), MinLatE7: int32(bounds[1] * 1e7), MaxLonE7: int32(bounds[2] * 1e7), MaxLatE7: int32(bounds[3] * 1e7),
		CenterZoom: uint8(max(b.minZoom, z-4)), CenterLonE7: int32((bounds[0] + bounds[2]) / 2 * 1e7), CenterLatE7: int32((bounds[1] + bounds[3]) / 2 * 1e7)}
	tiles := w.Count()
	if tiles == 0 {
		return nil, errors.New("terrain: no tiles produced (no source covers the regions?)")
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	if err := w.Commit(h, metaJSON); err != nil {
		return nil, err
	}
	ok = true
	fi, _ := os.Stat(outPath)
	res := &TerrainResult{Regions: ids, MinZoom: b.minZoom, MaxZoom: z, TileSize: elevation.TileSize, Tiles: int(tiles), Sources: meta.Sources,
		Fetched: b.fetch.fetched, Reused: b.fetch.reused, DurationSec: math.Round(time.Since(start).Seconds()), BuiltAt: meta.BuiltAt,
		Fingerprint: fingerprint}
	if fi != nil {
		res.Bytes = fi.Size()
	}
	js, _ := json.MarshalIndent(res, "", "  ")
	res.OnDemand = onDemand
	js, _ = json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(outPath+".json", js, 0o644); err != nil {
		return nil, err
	}
	var used []string
	for _, s := range meta.Sources {
		if s.Pixels > 0 {
			used = append(used, fmt.Sprintf("%s %.1f%%", s.ID, 100*float64(s.Pixels)/float64(max(1, totalPixels(meta.Sources)))))
		}
	}
	r.Logf("terrain: wrote %s: %d tiles, %.1f MB, z%d–%d, sources: %s, pieces fetched %d / reused %d, %s",
		outPath, tiles, float64(res.Bytes)/1e6, b.minZoom, z, strings.Join(used, ", "), res.Fetched, res.Reused, time.Since(start).Round(time.Second))
	r.Progress(1, "terrain done")
	return res, nil
}

func totalPixels(s []elevation.MetaSource) int64 {
	var t int64
	for _, x := range s {
		t += x.Pixels
	}
	return t
}

func absPath(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

// chunks lists the chunks that contain at least one tile touching an area.
func (b *builder) chunks() []chunk {
	seen := map[chunk]bool{}
	var out []chunk
	for _, a := range b.areas {
		x0, y0, x1, y1 := elevation.TileRange(b.z, a.bbox[0], a.bbox[1], a.bbox[2], a.bbox[3])
		for cy := y0 >> chunkShift; cy <= y1>>chunkShift; cy++ {
			for cx := x0 >> chunkShift; cx <= x1>>chunkShift; cx++ {
				c := chunk{cx, cy}
				if seen[c] {
					continue
				}
				if b.chunkTouches(c) {
					seen[c] = true
					out = append(out, c)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].cy != out[j].cy {
			return out[i].cy < out[j].cy
		}
		return out[i].cx < out[j].cx
	})
	return out
}

func (b *builder) chunkTouches(c chunk) bool {
	cz := b.z - chunkShift
	w, s, e, n := elevation.TileBounds(cz, c.cx, c.cy)
	for _, a := range b.areas {
		if a.intersectsBox(elevation.BBox{w, s, e, n}) {
			return true
		}
	}
	return false
}

// tileWanted reports whether tile z/x/y touches an area.
func (b *builder) tileWanted(z, x, y int) bool {
	w, s, e, n := elevation.TileBounds(z, x, y)
	box := elevation.BBox{w, s, e, n}
	for _, a := range b.areas {
		if a.intersectsBox(box) {
			return true
		}
	}
	return false
}

// cellsTouching returns the SW corners of 1° cells overlapping box, each with
// the window to fetch: the cell clipped to the (slightly buffered) areas.
func (b *builder) cellsTouching(box elevation.BBox) []cellWindow {
	var out []cellWindow
	for lat := int(math.Floor(box[1])); lat < int(math.Ceil(box[3])); lat++ {
		for lon := int(math.Floor(box[0])); lon < int(math.Ceil(box[2])); lon++ {
			cell := elevation.BBox{float64(lon), float64(lat), float64(lon + 1), float64(lat + 1)}
			// Window: union of (area bbox buffered) ∩ cell over all areas. The
			// window is derived from areas, not the chunk, so every chunk
			// asks for the same window and the cache is shared.
			var win elevation.BBox
			have := false
			for _, a := range b.areas {
				// Buffer by one max-zoom tile (plus a margin) so edge tiles are
				// complete rather than half nodata.
				buf := 1.2 * 360 / math.Exp2(float64(b.z))
				ab := elevation.BBox{a.bbox[0] - buf, a.bbox[1] - buf, a.bbox[2] + buf, a.bbox[3] + buf}
				if !ab.Intersects(cell) {
					continue
				}
				iw := elevation.BBox{math.Max(ab[0], cell[0]), math.Max(ab[1], cell[1]), math.Min(ab[2], cell[2]), math.Min(ab[3], cell[3])}
				if !have {
					win, have = iw, true
				} else {
					win = elevation.BBox{math.Min(win[0], iw[0]), math.Min(win[1], iw[1]), math.Max(win[2], iw[2]), math.Max(win[3], iw[3])}
				}
			}
			if have && win.Intersects(box) {
				out = append(out, cellWindow{lon: lon, lat: lat, window: win})
			}
		}
	}
	return out
}

type cellWindow struct {
	lon, lat int
	window   elevation.BBox
}

// processChunk composites the sources for one chunk and writes its tiles.
func (b *builder) processChunk(ctx context.Context, c chunk) error {
	cz := b.z - chunkShift
	size := elevation.TileSize << chunkShift
	n := size * size
	comp := make([]float32, n)
	src := make([]uint8, n)
	nan := float32(math.NaN())
	for i := range comp {
		comp[i] = nan
	}
	// Only pixels of tiles that will be written need filling; once those
	// are done, lower-priority sources are not fetched for this chunk.
	need := make([]bool, n)
	remaining := 0
	tilesPer := 1 << chunkShift
	for ty := 0; ty < tilesPer; ty++ {
		for tx := 0; tx < tilesPer; tx++ {
			if !b.tileWanted(b.z, c.cx<<chunkShift+tx, c.cy<<chunkShift+ty) {
				continue
			}
			for j := 0; j < elevation.TileSize; j++ {
				row := (ty*elevation.TileSize+j)*size + tx*elevation.TileSize
				for i := 0; i < elevation.TileSize; i++ {
					need[row+i] = true
				}
			}
			remaining += elevation.TileSize * elevation.TileSize
		}
	}
	w, s, e, nn := elevation.TileBounds(cz, c.cx, c.cy)
	box := elevation.BBox{w, s, e, nn}
	minx, miny, maxx, maxy := elevation.TileMercBounds(cz, c.cx, c.cy)
	cells := b.cellsTouching(box)

	for si, ds := range b.srcs {
		if remaining == 0 {
			break
		}
		var files []string
		for _, cw := range cells {
			cellBox := elevation.BBox{float64(cw.lon), float64(cw.lat), float64(cw.lon + 1), float64(cw.lat + 1)}
			if !ds.inCoverage(cellBox) {
				continue
			}
			refs, err := ds.cellURLs(ctx, b.fetch, cw.lon, cw.lat)
			if err != nil {
				return fmt.Errorf("terrain: %s cell %d,%d: %w", ds.def.ID, cw.lon, cw.lat, err)
			}
			for _, ref := range refs {
				path, err := b.fetch.fetch(ctx, ds, piece{source: ds.def.ID, key: ref.key, url: ref.url, window: cw.window})
				if err != nil {
					return fmt.Errorf("terrain: %w", err)
				}
				files = append(files, path)
			}
		}
		if len(files) == 0 {
			continue
		}
		vals, err := b.warp(ctx, files, minx, miny, maxx, maxy, size, fmt.Sprintf("%d_%d_%d", c.cx, c.cy, si))
		if err != nil {
			return err
		}
		var took int64
		for i, v := range vals {
			if src[i] == 0 && !math.IsNaN(float64(v)) && v > -1000 && v < 10000 {
				comp[i] = v
				src[i] = uint8(si + 1)
				took++
				if need[i] {
					remaining--
				}
			}
		}
		if took > 0 {
			b.mu.Lock()
			b.pixels[si] += took
			b.footprint[si][[2]int{c.cx, c.cy}] = true
			b.mu.Unlock()
		}
	}

	// Max-zoom tiles, then lower levels inside the chunk.
	level := comp
	levelSize := size
	for l := 0; l <= b.shift; l++ {
		z := b.z - l
		tilesPer := levelSize / elevation.TileSize
		for ty := 0; ty < tilesPer; ty++ {
			for tx := 0; tx < tilesPer; tx++ {
				x := c.cx<<(chunkShift-l) + tx
				y := c.cy<<(chunkShift-l) + ty
				te, ts, any := cutTile(level, levelSize, tx, ty, src, l == 0)
				if !any {
					continue
				}
				if l == b.shift {
					b.mu.Lock()
					b.base[[2]int{x, y}] = &elevTile{elev: te}
					b.mu.Unlock()
				}
				if !b.tileWanted(z, x, y) {
					continue
				}
				data, err := elevation.EncodeTile(quantize(te, b.stepFor(z)), ts)
				if err != nil {
					return err
				}
				if err := b.w.AddTile(uint8(z), uint32(x), uint32(y), data); err != nil {
					return err
				}
			}
		}
		if l < b.shift {
			level = downsample(level, levelSize)
			levelSize /= 2
		}
	}
	return nil
}

// cutTile copies tile (tx, ty) out of a square raster. With withSrc it also
// returns the source indices (max zoom only). any is false for all-nodata tiles.
func cutTile(r []float32, size, tx, ty int, src []uint8, withSrc bool) ([]float32, []uint8, bool) {
	ts := elevation.TileSize
	te := make([]float32, ts*ts)
	var tsrc []uint8
	if withSrc {
		tsrc = make([]uint8, ts*ts)
	}
	any := false
	for j := 0; j < ts; j++ {
		row := (ty*ts+j)*size + tx*ts
		copy(te[j*ts:(j+1)*ts], r[row:row+ts])
		if withSrc {
			copy(tsrc[j*ts:(j+1)*ts], src[row:row+ts])
		}
	}
	for _, v := range te {
		if !math.IsNaN(float64(v)) {
			any = true
			break
		}
	}
	return te, tsrc, any
}

// stepFor returns the rounding step for zoom z: the base step at max zoom,
// doubling per zoom out, capped.
func (b *builder) stepFor(z int) float64 {
	base := b.o.quantum
	if base <= 0 {
		base = TerrainQuantumM
	}
	return math.Min(base*math.Exp2(float64(b.z-z)), math.Max(base, terrainQuantumMaxM))
}

// quantize returns a copy of e with valid elevations rounded to step metres
// (the input stays unrounded: it also feeds the lower zooms).
func quantize(e []float32, step float64) []float32 {
	if step <= 0 {
		step = TerrainQuantumM
	}
	out := make([]float32, len(e))
	for i, v := range e {
		if math.IsNaN(float64(v)) {
			out[i] = v
			continue
		}
		out[i] = float32(math.Round(float64(v)/step) * step)
	}
	return out
}

// downsample halves a square raster by averaging valid pixels of each 2×2 block.
func downsample(r []float32, size int) []float32 {
	h := size / 2
	out := make([]float32, h*h)
	for y := 0; y < h; y++ {
		for x := 0; x < h; x++ {
			var sum float64
			var n int
			for _, i := range [4]int{(2*y)*size + 2*x, (2*y)*size + 2*x + 1, (2*y+1)*size + 2*x, (2*y+1)*size + 2*x + 1} {
				if v := r[i]; !math.IsNaN(float64(v)) {
					sum += float64(v)
					n++
				}
			}
			if n > 0 {
				out[y*h+x] = float32(sum / float64(n))
			} else {
				out[y*h+x] = float32(math.NaN())
			}
		}
	}
	return out
}

// lowZooms builds zooms below the chunk level from the chunk-level tiles.
func (b *builder) lowZooms() error {
	level := b.base
	ts := elevation.TileSize
	for z := b.z - b.shift - 1; z >= b.minZoom; z-- {
		parents := map[[2]int]*elevTile{}
		for k := range level {
			parents[[2]int{k[0] >> 1, k[1] >> 1}] = nil
		}
		for p := range parents {
			big := make([]float32, 4*ts*ts)
			for i := range big {
				big[i] = float32(math.NaN())
			}
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					child := level[[2]int{p[0]*2 + dx, p[1]*2 + dy}]
					if child == nil {
						continue
					}
					for j := 0; j < ts; j++ {
						copy(big[(dy*ts+j)*2*ts+dx*ts:], child.elev[j*ts:(j+1)*ts])
					}
				}
			}
			t := &elevTile{elev: downsample(big, 2*ts)}
			parents[p] = t
			data, err := elevation.EncodeTile(quantize(t.elev, b.stepFor(z)), nil)
			if err != nil {
				return err
			}
			if err := b.w.AddTile(uint8(z), uint32(p[0]), uint32(p[1]), data); err != nil {
				return err
			}
		}
		level = parents
	}
	return nil
}

// warp reprojects source files onto a chunk grid and returns float32 pixels
// (NaN = nodata). Later files in the list win where they overlap; all files
// here come from one source, so order does not matter.
func (b *builder) warp(ctx context.Context, files []string, minx, miny, maxx, maxy float64, size int, tag string) ([]float32, error) {
	tmpDir := filepath.Join(absPath(b.workDir), "terrain-tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, err
	}
	out := filepath.Join(tmpDir, "chunk_"+tag+".bin")
	defer func() {
		_ = os.Remove(out)
		_ = os.Remove(strings.TrimSuffix(out, ".bin") + ".hdr")
		_ = os.Remove(out + ".aux.xml")
	}()
	args := []string{"-q", "-overwrite", "-of", "ENVI", "-ot", "Float32", "-t_srs", "EPSG:3857",
		"-te", fmt.Sprintf("%.4f", minx), fmt.Sprintf("%.4f", miny), fmt.Sprintf("%.4f", maxx), fmt.Sprintf("%.4f", maxy),
		"-ts", strconv.Itoa(size), strconv.Itoa(size), "-r", "bilinear", "-dstnodata", "nan",
		"-wo", "NUM_THREADS=2", "-wm", "512"}
	args = append(args, files...)
	args = append(args, out)
	if err := b.gdal.run(ctx, "gdalwarp", args...); err != nil {
		return nil, fmt.Errorf("terrain: warp: %w", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	if len(raw) != size*size*4 {
		return nil, fmt.Errorf("terrain: warp output is %d bytes, want %d", len(raw), size*size*4)
	}
	order := binary.ByteOrder(binary.LittleEndian)
	if hdr, err := os.ReadFile(strings.TrimSuffix(out, ".bin") + ".hdr"); err == nil && strings.Contains(string(hdr), "byte order = 1") {
		order = binary.BigEndian
	}
	vals := make([]float32, size*size)
	for i := range vals {
		vals[i] = math.Float32frombits(order.Uint32(raw[i*4:]))
	}
	return vals, nil
}

// footprintBoxes returns lon/lat boxes of the chunks where source i contributed.
func (b *builder) footprintBoxes(i int) []elevation.BBox {
	cz := b.z - chunkShift
	var out []elevation.BBox
	for k := range b.footprint[i] {
		w, s, e, n := elevation.TileBounds(cz, k[0], k[1])
		out = append(out, elevation.BBox{round6(w), round6(s), round6(e), round6(n)})
	}
	sort.Slice(out, func(a, c int) bool {
		if out[a][1] != out[c][1] {
			return out[a][1] < out[c][1]
		}
		return out[a][0] < out[c][0]
	})
	return out
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
