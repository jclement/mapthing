package ondemand

import (
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/jclement/mapthing/internal/elevation"
)

// renderRev is part of every cached tile's version: raise it when the way
// tiles are made changes, and tiles cached by older code are rebuilt.
const renderRev = 1

// ErrLimited is returned when the request's gate refuses a cold fetch (a rate
// limit). RetryAfter says when to try again.
type ErrLimited = elevation.LimitedError

type gateKey struct{}

type gate struct {
	once sync.Once
	fn   func() error
	err  error
}

// WithGate returns a context for one request. The first time that request
// needs data from the DEM hosts (not from the cache) fn is called, once; an
// error from it refuses the request. This is how a cold request is charged
// more than a cached one.
func WithGate(ctx context.Context, fn func() error) context.Context {
	return context.WithValue(ctx, gateKey{}, &gate{fn: fn})
}

func passGate(ctx context.Context) error {
	g, _ := ctx.Value(gateKey{}).(*gate)
	if g == nil {
		return nil
	}
	g.once.Do(func() { g.err = g.fn() })
	return g.err
}

// Counters are totals since start, for the Overview.
type Counters struct {
	Hits     int64 `json:"hits"`     // tiles answered from the cache
	Built    int64 `json:"built"`    // tiles built from the DEM hosts
	Failed   int64 `json:"failed"`   // tiles that could not be built
	Limited  int64 `json:"limited"`  // cold requests refused by a rate limit
	Requests int64 `json:"requests"` // requests sent to the DEM hosts
	Bytes    int64 `json:"bytes"`    // bytes received from them
}

type counters struct {
	hits, built, failed, limited atomic.Int64
}

// Counters returns the totals.
func (s *Service) Counters() Counters {
	req, by, _ := s.up.Counters()
	return Counters{Hits: s.n.hits.Load(), Built: s.n.built.Load(), Failed: s.n.failed.Load(), Limited: s.n.limited.Load(), Requests: req, Bytes: by}
}

// tileKey is where a finished tile lives in the store.
func tileKey(z, x, y int) string { return fmt.Sprintf("tile/%d/%d/%d", z, x, y) }

// versionOf condenses the releases of a tile's candidate files into the 8
// bytes a cached tile is stamped with.
func versionOf(files []candidate) [8]byte {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "rev %d\n", renderRev)
	for _, c := range files {
		_, _ = fmt.Fprintf(h, "%s\n%s\n", c.url, c.version.Tag)
	}
	var v [8]byte
	copy(v[:], h.Sum(nil))
	return v
}

// Tile returns tile z/x/y as a Terrarium PNG, from the cache when the cached
// copy was made from the current release of every source file, and from the
// DEM hosts otherwise. png is nil when no source has data there. cold reports
// whether the hosts had to be asked.
//
// The caller decides whether on demand applies to the tile (Policy).
func (s *Service) Tile(ctx context.Context, z, x, y int) (png []byte, cold bool, err error) {
	if z < 0 || z > 24 || x < 0 || y < 0 || x >= 1<<z || y >= 1<<z {
		return nil, false, errors.New("ondemand: no such tile")
	}
	w, so, e, n := elevation.TileBounds(z, x, y)
	files, err := s.candidates(ctx, elevation.BBox{w, so, e, n})
	if err != nil {
		s.n.failed.Add(1)
		return nil, true, err
	}
	if len(files) == 0 { // open ocean: nothing to fetch, nothing to store
		return nil, false, nil
	}
	ver := versionOf(files)
	key := tileKey(z, x, y)
	if png, ok := s.stored(key, ver); ok {
		s.n.hits.Add(1)
		return png, false, nil
	}
	if err := passGate(ctx); err != nil {
		s.n.limited.Add(1)
		return nil, true, err
	}
	v, err := s.flight.do(ctx, fmt.Sprintf("%s@%x", key, ver), func(ctx context.Context) (any, error) {
		if png, ok := s.stored(key, ver); ok {
			return png, nil
		}
		r, err := s.render(ctx, z, x, y, files)
		if err != nil {
			s.n.failed.Add(1)
			return nil, err
		}
		var png []byte
		if r.Filled > 0 {
			if png, err = r.Encode(z); err != nil {
				return nil, err
			}
		}
		if s.opt.Store != nil {
			// Only a tile every source answered for gets here; a failure
			// returned above stores nothing.
			_ = s.opt.Store.Put(key, append(ver[:], png...))
		}
		s.n.built.Add(1)
		return png, nil
	})
	if err != nil {
		return nil, true, err
	}
	return v.([]byte), true, nil
}

// stored returns the cached tile if it carries version ver.
func (s *Service) stored(key string, ver [8]byte) ([]byte, bool) {
	if s.opt.Store == nil {
		return nil, false
	}
	b, ok := s.opt.Store.Get(key)
	if !ok || len(b) < 8 || [8]byte(b[:8]) != ver {
		return nil, false
	}
	return b[8:], true
}

// Cached reports whether tile z/x/y can be answered without asking the DEM
// hosts for elevation data.
func (s *Service) Cached(ctx context.Context, z, x, y int) bool {
	w, so, e, n := elevation.TileBounds(z, x, y)
	files, err := s.candidates(ctx, elevation.BBox{w, so, e, n})
	if err != nil {
		return false
	}
	if len(files) == 0 {
		return true
	}
	_, ok := s.stored(tileKey(z, x, y), versionOf(files))
	return ok
}

// Decoded returns the SampleZoom tile x/y decoded for sampling, nil when no
// source has data there.
func (s *Service) Decoded(ctx context.Context, x, y int) (*elevation.Tile, error) {
	w, so, e, n := elevation.TileBounds(SampleZoom, x, y)
	files, err := s.candidates(ctx, elevation.BBox{w, so, e, n})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}
	// Keyed by version as well, so a decoded tile does not outlive its sources' release.
	k := decodedKey{x, y, versionOf(files)}
	if t, ok := s.decoded.get(k); ok {
		return t, nil
	}
	png, _, err := s.Tile(ctx, SampleZoom, x, y)
	if err != nil {
		return nil, err
	}
	var t *elevation.Tile
	if len(png) > 0 {
		if t, err = elevation.DecodeTile(png); err != nil {
			return nil, err
		}
	}
	s.decoded.put(k, t)
	return t, nil
}

type decodedKey struct {
	x, y int
	ver  [8]byte
}

// decodedLRU keeps decoded sample tiles (about 330 KB each).
type decodedLRU struct {
	mu    sync.Mutex
	cap   int
	lru   list.List
	items map[decodedKey]*list.Element
}

type decodedEntry struct {
	k decodedKey
	t *elevation.Tile
}

func (c *decodedLRU) get(k decodedKey) (*elevation.Tile, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(el)
	return el.Value.(*decodedEntry).t, true
}

func (c *decodedLRU) put(k decodedKey, t *elevation.Tile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[decodedKey]*list.Element{}
	}
	if el, ok := c.items[k]; ok {
		el.Value.(*decodedEntry).t = t
		return
	}
	c.items[k] = c.lru.PushFront(&decodedEntry{k, t})
	for c.lru.Len() > c.cap {
		el := c.lru.Back()
		c.lru.Remove(el)
		delete(c.items, el.Value.(*decodedEntry).k)
	}
}

func (c *decodedLRU) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lru.Init()
	c.items = nil
}
