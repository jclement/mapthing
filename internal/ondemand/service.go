// Package ondemand builds terrain tiles at request time by reading small
// windows of the public cloud-optimised GeoTIFF DEMs, for areas whose terrain
// was not built ahead of time. It is an option: nothing here runs unless the
// operator turns it on for a region or for the world outside built regions.
package ondemand

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jclement/mapthing/internal/cog"
	"github.com/jclement/mapthing/internal/elevation"
)

// Store is the disk cache the service keeps fetched bytes in. A nil Store
// means nothing is kept between requests.
type Store interface {
	Get(key string) ([]byte, bool)
	Put(key string, b []byte) error
	// RemovePrefix drops every entry whose key starts with prefix.
	RemovePrefix(prefix string)
}

// Options configure a Service.
type Options struct {
	Upstream *Upstream
	Store    Store
	URLs     URLs
	// VersionTTL is how long a file's version (ETag) is trusted before the
	// host is asked again (default 24 h).
	VersionTTL time.Duration
	// BlockMemory bounds decoded source blocks kept in memory, in bytes
	// (default 96 MB).
	BlockMemory int64
	// LevelSlack is how much coarser than a tile's pixels a source overview
	// may be and still be used for it (default 1.02: none to speak of). The
	// build raises it for low zooms.
	LevelSlack float64
	// StatePath, if set, is where file versions are remembered across
	// restarts, so cached tiles can still be served when the hosts are
	// unreachable.
	StatePath string
	Now       func() time.Time
}

// Service reads DEM sources on demand.
type Service struct {
	opt     Options
	up      *Upstream
	sources []*source
	book    *versionBook

	mu    sync.Mutex
	files map[string]*remoteFile // by fid

	blocks  blockLRU
	decoded decodedLRU
	flight  flight
	policy  policyBox
	n       counters
}

// New returns a Service.
func New(opt Options) *Service {
	if opt.Upstream == nil {
		opt.Upstream = NewUpstream(UpstreamOptions{})
	}
	if opt.URLs == (URLs{}) {
		opt.URLs = DefaultURLs()
	}
	if opt.VersionTTL <= 0 {
		opt.VersionTTL = 24 * time.Hour
	}
	if opt.BlockMemory <= 0 {
		opt.BlockMemory = 96 << 20
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.LevelSlack < 1 {
		opt.LevelSlack = 1.02
	}
	s := &Service{opt: opt, up: opt.Upstream, sources: buildSources(opt.URLs), files: map[string]*remoteFile{}}
	s.blocks.max = opt.BlockMemory
	s.decoded.cap = 256
	s.book = loadVersionBook(opt.StatePath)
	return s
}

// Upstream returns the door to the DEM hosts (for health and counters).
func (s *Service) Upstream() *Upstream { return s.up }

// candidate is one existing remote file that may hold data for a tile.
type candidate struct {
	src     *source
	url     string
	version Version
}

// candidates lists, in priority order, the files that may contribute to the
// lon/lat box, with their current versions. Files the hosts do not have are
// left out.
func (s *Service) candidates(ctx context.Context, box elevation.BBox) ([]candidate, error) {
	// Every source is asked at once (a cold tile should not wait on one host
	// after another); the answers are put back in priority order.
	type found struct {
		list []candidate
		err  error
	}
	per := make([]found, len(s.sources))
	var wg sync.WaitGroup
	for si, src := range s.sources {
		if !src.covers(box) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			urls, err := src.files(ctx, s, box)
			if err != nil {
				per[si].err = err
				return
			}
			vs := make([]Version, len(urls))
			errs := make([]error, len(urls))
			var inner sync.WaitGroup
			for i, u := range urls {
				inner.Add(1)
				go func() {
					defer inner.Done()
					vs[i], errs[i] = s.version(ctx, u)
				}()
			}
			inner.Wait()
			for i, u := range urls {
				if errs[i] != nil {
					per[si].err = errs[i]
					return
				}
				if vs[i].Exists {
					per[si].list = append(per[si].list, candidate{src: src, url: u, version: vs[i]})
				}
			}
		}()
	}
	wg.Wait()
	var out []candidate
	for _, f := range per {
		if f.err != nil {
			return nil, f.err
		}
		out = append(out, f.list...)
	}
	return out, nil
}

// Render builds tile z/x/y from the sources, without touching the tile cache.
func (s *Service) Render(ctx context.Context, z, x, y int) (*Rendered, error) {
	w, so, e, n := elevation.TileBounds(z, x, y)
	files, err := s.candidates(ctx, elevation.BBox{w, so, e, n})
	if err != nil {
		return nil, err
	}
	return s.render(ctx, z, x, y, files)
}

// Prime learns which files tile z/x/y needs, their versions and their
// headers, without reading any elevation. It is what a server already knows
// about an area it has served before.
func (s *Service) Prime(ctx context.Context, z, x, y int) error {
	w, so, e, n := elevation.TileBounds(z, x, y)
	files, err := s.candidates(ctx, elevation.BBox{w, so, e, n})
	if err != nil {
		return err
	}
	for _, c := range files {
		if _, err := s.open(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// remoteFile is one release of one remote COG.
type remoteFile struct {
	fid  string
	url  string
	once sync.Once
	cog  *cog.File
	err  error
}

func fileID(url, tag string) string {
	h := sha256.Sum256([]byte(url + "|" + tag))
	return hex.EncodeToString(h[:8])
}

// ReadRange implements cog.RangeReader for the file's metadata pages, which
// are kept in the store.
func (r *rangeReader) ReadRange(ctx context.Context, off, n int64) ([]byte, error) {
	key := "src/" + r.f.fid + "/m/" + strconv.FormatInt(off, 10)
	return r.s.cached(ctx, key, true, func() ([]byte, error) { return r.s.up.Range(ctx, r.f.url, off, n) })
}

type rangeReader struct {
	s *Service
	f *remoteFile
}

// cached returns the stored bytes for key, or fetches them (and stores them
// when keep is set). Concurrent callers for one key share one fetch; a failed
// fetch stores nothing.
func (s *Service) cached(ctx context.Context, key string, keep bool, fetch func() ([]byte, error)) ([]byte, error) {
	if s.opt.Store != nil {
		if b, ok := s.opt.Store.Get(key); ok {
			return b, nil
		}
	}
	v, err := s.flight.do(ctx, key, func(ctx context.Context) (any, error) {
		if s.opt.Store != nil {
			if b, ok := s.opt.Store.Get(key); ok {
				return b, nil
			}
		}
		b, err := fetch()
		if err != nil {
			return nil, err
		}
		if s.opt.Store != nil && keep {
			_ = s.opt.Store.Put(key, b)
		}
		return b, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

// open returns the parsed COG for a candidate, reading its header once.
func (s *Service) open(ctx context.Context, c candidate) (*remoteFile, error) {
	fid := fileID(c.url, c.version.Tag)
	s.mu.Lock()
	f, ok := s.files[fid]
	if !ok {
		f = &remoteFile{fid: fid, url: c.url}
		s.files[fid] = f
	}
	s.mu.Unlock()
	f.once.Do(func() {
		// The header read outlives the request that started it: others wait on it.
		hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		f.cog, f.err = cog.Open(hctx, &rangeReader{s: s, f: f})
	})
	if f.err != nil {
		// Do not remember a failed open: the next request tries again.
		s.mu.Lock()
		if s.files[fid] == f {
			delete(s.files, fid)
		}
		s.mu.Unlock()
		if errors.Is(f.err, cog.ErrFormat) {
			return nil, fmt.Errorf("%w: %s: %w", ErrUpstream, c.src.def.ID, f.err)
		}
		return nil, f.err
	}
	return f, nil
}

// block returns one decoded source block (nil when the file has no data there).
func (s *Service) block(ctx context.Context, f *remoteFile, level, col, row int) ([]float32, error) {
	key := "src/" + f.fid + "/b/" + strconv.Itoa(level) + "-" + strconv.Itoa(col) + "-" + strconv.Itoa(row)
	if b, ok := s.blocks.get(key); ok {
		return b, nil
	}
	v, err := s.flight.do(ctx, "decode:"+key, func(ctx context.Context) (any, error) {
		if b, ok := s.blocks.get(key); ok {
			return b, nil
		}
		ref, err := f.cog.Locate(ctx, level, col, row)
		if err != nil {
			return nil, err
		}
		var dec []float32
		if ref.Size > 0 {
			// A warm job keeps only the finished tiles: source blocks are many times
			// their size and would push the tiles it just made out of the cache.
			raw, err := s.cached(ctx, key, !warming(ctx), func() ([]byte, error) {
				b, err := s.up.Range(ctx, f.url, ref.Offset, ref.Size)
				if err == nil && int64(len(b)) != ref.Size {
					err = fmt.Errorf("%w: short read", ErrUpstream)
				}
				return b, err
			})
			if err != nil {
				return nil, err
			}
			if dec, err = f.cog.Decode(level, raw); err != nil {
				// Bytes that do not decode are not data: drop them.
				if s.opt.Store != nil {
					s.opt.Store.RemovePrefix(key)
				}
				return nil, fmt.Errorf("%w: %w", ErrUpstream, err)
			}
		}
		s.blocks.put(key, dec)
		return dec, nil
	})
	if err != nil {
		return nil, err
	}
	b, _ := v.([]float32)
	return b, nil
}

// blockLRU keeps decoded blocks in memory, bounded by bytes.
type blockLRU struct {
	mu    sync.Mutex
	max   int64
	size  int64
	lru   list.List
	items map[string]*list.Element
}

type blockEntry struct {
	key string
	b   []float32
}

func (c *blockLRU) get(key string) ([]float32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(el)
	return el.Value.(*blockEntry).b, true
}

func (c *blockLRU) put(key string, b []float32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]*list.Element{}
	}
	if _, ok := c.items[key]; ok {
		return
	}
	c.items[key] = c.lru.PushFront(&blockEntry{key, b})
	c.size += int64(len(b))*4 + 64
	for c.size > c.max && c.lru.Len() > 1 {
		el := c.lru.Back()
		e := el.Value.(*blockEntry)
		c.lru.Remove(el)
		delete(c.items, e.key)
		c.size -= int64(len(e.b))*4 + 64
	}
}

// flight merges concurrent work for one key. The work runs detached from any
// one caller, so a caller that goes away does not fail the others; each
// caller still stops waiting when its own context ends.
type flight struct {
	mu sync.Mutex
	m  map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	val  any
	err  error
}

func (f *flight) do(ctx context.Context, key string, fn func(ctx context.Context) (any, error)) (any, error) {
	f.mu.Lock()
	if f.m == nil {
		f.m = map[string]*flightCall{}
	}
	c, ok := f.m[key]
	if !ok {
		c = &flightCall{done: make(chan struct{})}
		f.m[key] = c
		go func() {
			wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			c.val, c.err = fn(wctx)
			f.mu.Lock()
			delete(f.m, key)
			f.mu.Unlock()
			close(c.done)
		}()
	}
	f.mu.Unlock()
	select {
	case <-c.done:
		return c.val, c.err
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrUpstream, ctx.Err())
	}
}
