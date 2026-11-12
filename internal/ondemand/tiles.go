package ondemand

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// ErrBudget is returned when a request has used up the number of tiles it
// may read from the DEM hosts (WithBudget).
var ErrBudget = errors.New("ondemand: this request may not read more tiles from the sources")

type budgetKey struct{}

// WithBudget returns a context for one request that may build at most n tiles
// from the DEM hosts. The count is taken where a tile is actually built, so it
// holds however the cache changes while the request runs.
func WithBudget(ctx context.Context, n int) context.Context {
	b := new(atomic.Int64)
	b.Store(int64(n))
	return context.WithValue(ctx, budgetKey{}, b)
}

func takeBudget(ctx context.Context) bool {
	b, _ := ctx.Value(budgetKey{}).(*atomic.Int64)
	return b == nil || b.Add(-1) >= 0
}

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
	// Contour tiles answered from the cache, and drawn from on-demand terrain.
	ContourHits   int64 `json:"contour_hits"`
	ContoursBuilt int64 `json:"contours_built"`
}

type counters struct {
	hits, built, failed, limited atomic.Int64
	contourHits, contoursBuilt   atomic.Int64
}

// countFailure records a request that could not be answered: refused by a
// rate limit, or failed.
func countFailure(s *Service, err error) {
	var lim *ErrLimited
	switch {
	case errors.As(err, &lim):
		s.n.limited.Add(1)
	case errors.Is(err, ErrBudget):
	default:
		s.n.failed.Add(1)
	}
}

// each runs fn over items, a few at a time, and stops at the first error.
func each[T any](ctx context.Context, items []T, fn func(T) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	jobs := make(chan T)
	for i := 0; i < min(prepareWorkers, len(items)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				if err := fn(it); err != nil {
					mu.Lock()
					if first == nil {
						first = err
						cancel()
					}
					mu.Unlock()
				}
			}
		}()
	}
feed:
	for _, it := range items {
		select {
		case jobs <- it:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	return first
}

// Counters returns the totals.
func (s *Service) Counters() Counters {
	req, by, _ := s.up.Counters()
	return Counters{Hits: s.n.hits.Load(), Built: s.n.built.Load(), Failed: s.n.failed.Load(), Limited: s.n.limited.Load(), Requests: req, Bytes: by,
		ContourHits: s.n.contourHits.Load(), ContoursBuilt: s.n.contoursBuilt.Load()}
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
		var lim *ErrLimited
		if errors.As(err, &lim) {
			s.n.limited.Add(1)
		} else {
			s.n.failed.Add(1)
		}
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
	if !takeBudget(ctx) {
		return nil, true, ErrBudget
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

// errOffline refuses upstream work for a caller that only wants to know what
// can be answered without it.
var errOffline = &ErrLimited{Message: "not asking the sources"}

// offline returns a context under which nothing is asked of the DEM hosts:
// file versions are taken as last known, and an unknown one is an error.
func offline(ctx context.Context) context.Context {
	return WithGate(ctx, func() error { return errOffline })
}

// CachedNow is Cached without asking the hosts anything: true only when the
// tile is in the cache at the last known release of its sources.
func (s *Service) CachedNow(z, x, y int) bool {
	return s.Cached(offline(context.Background()), z, x, y)
}

// MaxRevisionArea bounds, in square degrees, the box Revision will work out
// the source releases of (a 1° cell is one or two files to ask about).
const MaxRevisionArea = 150.0

// ErrTooWide is returned by Revision for a box larger than MaxRevisionArea.
var ErrTooWide = errors.New("ondemand: that box is too large to cut from on-demand terrain")

// Revision identifies what on-demand terrain would serve for a box right
// now: where it applies (the whole policy) and which release of every source
// file could contribute. Something cut from on-demand terrain (an offline
// pack) is current for as long as its box's revision stays the same.
func (s *Service) Revision(ctx context.Context, box elevation.BBox) (string, error) {
	if (box[2]-box[0])*(box[3]-box[1]) > MaxRevisionArea {
		return "", ErrTooWide
	}
	files, err := s.candidates(ctx, box)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	p := s.Policy()
	_, _ = fmt.Fprintf(h, "rev %d outside %v\n", renderRev, p.Outside)
	for _, a := range p.Areas {
		_, _ = fmt.Fprintf(h, "%s %v %v\n", a.ID, a.BBox, a.Rings)
	}
	for _, c := range files {
		_, _ = fmt.Fprintf(h, "%s\n%s\n", c.url, c.version.Tag)
	}
	return hex.EncodeToString(h.Sum(nil)[:8]), nil
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
