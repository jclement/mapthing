package elevation

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jclement/mapthing/internal/pmtiles"
)

// ErrUnavailable means no terrain archive is live.
var ErrUnavailable = errors.New("elevation: no terrain data available")

// Archive samples one terrain.pmtiles file.
type Archive struct {
	path    string
	r       *pmtiles.Reader
	meta    Metadata
	maxZoom int

	mu    sync.Mutex
	lru   *list.List // of *cacheEntry, front = most recent
	items map[tileKey]*list.Element
	cap   int
}

type tileKey struct{ x, y int }

type cacheEntry struct {
	key  tileKey
	tile *Tile // nil = tile absent from the archive
}

// DefaultTileCache is the number of decoded max-zoom tiles kept per archive
// (each is ~320 KB: float32 elevations plus source indices).
const DefaultTileCache = 256

// OpenArchive opens a terrain archive written by the terrain build.
func OpenArchive(path string, cacheTiles int) (*Archive, error) {
	r, err := pmtiles.Open(path)
	if err != nil {
		return nil, err
	}
	a := &Archive{path: path, r: r, lru: list.New(), items: map[tileKey]*list.Element{}, cap: cacheTiles}
	if a.cap <= 0 {
		a.cap = DefaultTileCache
	}
	if err := r.MetadataJSON(&a.meta); err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("%s: metadata: %w", path, err)
	}
	if a.meta.Encoding != "" && a.meta.Encoding != "terrarium" {
		_ = r.Close()
		return nil, fmt.Errorf("%s: unsupported encoding %q", path, a.meta.Encoding)
	}
	a.maxZoom = int(r.Header.MaxZoom)
	return a, nil
}

// Close releases the archive file.
func (a *Archive) Close() error { return a.r.Close() }

// Meta returns the archive's metadata.
func (a *Archive) Meta() Metadata { return a.meta }

// MaxZoom is the zoom that point and profile queries sample.
func (a *Archive) MaxZoom() int { return a.maxZoom }

func (a *Archive) tile(x, y int) (*Tile, error) {
	n := 1 << a.maxZoom
	if y < 0 || y >= n {
		return nil, nil
	}
	x = ((x % n) + n) % n // wrap the antimeridian
	k := tileKey{x, y}
	a.mu.Lock()
	if el, ok := a.items[k]; ok {
		a.lru.MoveToFront(el)
		t := el.Value.(*cacheEntry).tile
		a.mu.Unlock()
		return t, nil
	}
	a.mu.Unlock()

	data, ok, err := a.r.Get(uint8(a.maxZoom), uint32(x), uint32(y))
	if err != nil {
		return nil, err
	}
	var t *Tile
	if ok {
		if t, err = DecodeTile(data); err != nil {
			return nil, fmt.Errorf("tile %d/%d/%d: %w", a.maxZoom, x, y, err)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if el, ok := a.items[k]; ok { // raced with another reader
		return el.Value.(*cacheEntry).tile, nil
	}
	a.items[k] = a.lru.PushFront(&cacheEntry{key: k, tile: t})
	for a.lru.Len() > a.cap {
		old := a.lru.Back()
		a.lru.Remove(old)
		delete(a.items, old.Value.(*cacheEntry).key)
	}
	return t, nil
}

// pixel returns elevation (NaN if none) and source index of a world pixel at max zoom.
func (a *Archive) pixel(px, py int) (float32, uint8, error) {
	t, err := a.tile(floorDiv(px, TileSize), floorDiv(py, TileSize))
	if err != nil || t == nil {
		return float32(math.NaN()), 0, err
	}
	i := mod(py, TileSize)*TileSize + mod(px, TileSize)
	var s uint8 = 1
	if t.Src != nil {
		s = t.Src[i]
	}
	return t.Elev[i], s, nil
}

// Sample returns the bilinearly interpolated elevation at a point (see
// grid.sample).
func (a *Archive) Sample(lon, lat float64) (Sample, error) {
	return grid{zoom: a.maxZoom, pixel: a.pixel, source: func(idx uint8) (SourceInfo, bool) {
		if int(idx) < 1 || int(idx) > len(a.meta.Sources) {
			return SourceInfo{}, false
		}
		return a.meta.Sources[idx-1].SourceInfo, true
	}}.sample(lon, lat)
}

func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func mod(a, b int) int { return ((a % b) + b) % b }

// Terrain implements Service on top of the live generation's terrain.pmtiles,
// reopening the archive when the generation is swapped.
type Terrain struct {
	path       string // e.g. /data/current/terrain.pmtiles (through the symlink)
	cacheTiles int
	checkEvery time.Duration

	mu        sync.Mutex
	arc       *Archive
	resolved  string // real path of the open archive
	modTime   time.Time
	lastCheck time.Time
	finder    StructureFinder // for bridges=deck; nil = unavailable
	fallback  Fallback        // on-demand terrain; nil = none
}

// SetFallback sets where elevation comes from when the live archive has none
// for a point (on-demand terrain). nil turns it off.
func (t *Terrain) SetFallback(f Fallback) {
	t.mu.Lock()
	t.fallback = f
	t.mu.Unlock()
}

// mixed returns a sampler over the live archive and the fallback. With
// neither there is nothing to answer from.
func (t *Terrain) mixed(ctx context.Context) (*mixed, error) {
	a, err := t.archive()
	t.mu.Lock()
	fb := t.fallback
	t.mu.Unlock()
	if err != nil {
		if fb == nil {
			return nil, err
		}
		a = nil
	}
	return &mixed{ctx: ctx, arc: a, fb: fb}, nil
}

// SetStructureFinder enables bridges=deck profiles (normally the routing client).
func (t *Terrain) SetStructureFinder(f StructureFinder) {
	t.mu.Lock()
	t.finder = f
	t.mu.Unlock()
}

// NewTerrain returns a Service reading path, which normally goes through the
// data volume's "current" symlink. A missing file is not an error: queries
// return ErrUnavailable until a generation appears.
func NewTerrain(path string, cacheTiles int) *Terrain {
	return &Terrain{path: path, cacheTiles: cacheTiles, checkEvery: 2 * time.Second}
}

// NewTerrainForData is NewTerrain for <dataDir>/current/terrain.pmtiles.
func NewTerrainForData(dataDir string) *Terrain {
	return NewTerrain(filepath.Join(dataDir, "current", "terrain.pmtiles"), DefaultTileCache)
}

// archive returns the open archive, reopening it if the symlink now points
// at another generation (checked at most every checkEvery).
func (t *Terrain) archive() (*Archive, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.arc != nil && time.Since(t.lastCheck) < t.checkEvery {
		return t.arc, nil
	}
	t.lastCheck = time.Now()
	real, err := filepath.EvalSymlinks(t.path)
	if err != nil {
		if t.arc != nil {
			return t.arc, nil // keep serving the old one if the link is mid-swap
		}
		return nil, ErrUnavailable
	}
	fi, err := os.Stat(real)
	if err != nil {
		if t.arc != nil {
			return t.arc, nil
		}
		return nil, ErrUnavailable
	}
	if t.arc != nil && real == t.resolved && fi.ModTime().Equal(t.modTime) {
		return t.arc, nil
	}
	a, err := OpenArchive(real, t.cacheTiles)
	if err != nil {
		if t.arc != nil {
			return t.arc, nil
		}
		return nil, err
	}
	old := t.arc
	t.arc, t.resolved, t.modTime = a, real, fi.ModTime()
	if old != nil {
		// Readers may still hold the old archive; close it a little later.
		time.AfterFunc(30*time.Second, func() { _ = old.Close() })
	}
	return a, nil
}

// Points samples each point.
func (t *Terrain) Points(ctx context.Context, pts []LonLat) ([]Sample, error) {
	m, err := t.mixed(ctx)
	if err != nil {
		return nil, err
	}
	if err := m.prepare(pts); err != nil {
		return nil, err
	}
	out := make([]Sample, len(pts))
	for i, p := range pts {
		if i%256 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if out[i], err = m.Sample(p[0], p[1]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Profile densifies path and samples it.
func (t *Terrain) Profile(ctx context.Context, path []LonLat, opt ProfileOptions) (*Profile, error) {
	m, err := t.mixed(ctx)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	finder := t.finder
	t.mu.Unlock()
	return profileFrom(ctx, m, finder, path, opt)
}

// Sources lists the sources present in the live archive, followed by the
// other known sources when on-demand terrain may read them.
func (t *Terrain) Sources() []SourceInfo {
	t.mu.Lock()
	fb := t.fallback
	t.mu.Unlock()
	var out []SourceInfo
	if a, err := t.archive(); err == nil {
		// List the sources that contributed pixels; archives without pixel
		// counts (hand-made, tests) list every source.
		counted := false
		for _, s := range a.meta.Sources {
			counted = counted || s.Pixels > 0
		}
		for _, s := range a.meta.Sources {
			if !counted || s.Pixels > 0 {
				out = append(out, s.SourceInfo)
			}
		}
	}
	if fb != nil {
		have := map[string]bool{}
		for _, s := range out {
			have[s.ID] = true
		}
		for _, s := range KnownSources {
			if !have[s.ID] {
				out = append(out, s.SourceInfo)
			}
		}
	}
	return out
}

// Meta returns the live archive's metadata (ok=false if none is live).
func (t *Terrain) Meta() (Metadata, bool) {
	a, err := t.archive()
	if err != nil {
		return Metadata{}, false
	}
	return a.meta, true
}

// Close closes the open archive.
func (t *Terrain) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.arc != nil {
		err := t.arc.Close()
		t.arc = nil
		return err
	}
	return nil
}

var _ Service = (*Terrain)(nil)
