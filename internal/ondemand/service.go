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
	// MetaMemory bounds what open source files hold in memory (their header
	// and block-index pages), in bytes (default 32 MB). Files dropped from
	// memory are opened again from the pages in the store.
	MetaMemory int64
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

	files fileLRU // open source files, by file id

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
	if opt.MetaMemory <= 0 {
		opt.MetaMemory = 32 << 20
	}
	s := &Service{opt: opt, up: opt.Upstream, sources: buildSources(opt.URLs)}
	s.files.max = opt.MetaMemory
	s.up.Restrict(allowedHosts(opt.URLs)...)
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

// remoteFile is one release of one remote COG, opened.
type remoteFile struct {
	fid string
	url string
	cog *cog.File
}

func fileID(url, tag string) string {
	h := sha256.Sum256([]byte(url + "|" + tag))
	return hex.EncodeToString(h[:8])
}

// ReadRange implements cog.RangeReader for the file's metadata pages, which
// are kept in the store.
func (r *rangeReader) ReadRange(ctx context.Context, off, n int64) ([]byte, error) {
	key := "src/" + r.fid + "/m/" + strconv.FormatInt(off, 10)
	return r.s.cached(ctx, key, true, func(ctx context.Context) ([]byte, error) { return r.s.up.Range(ctx, r.url, off, n) })
}

type rangeReader struct {
	s        *Service
	fid, url string
}

// guard turns a panic in fn into an error. The COG reader checks what it
// reads, but it reads bytes from other people's servers on goroutines no HTTP
// handler's recovery covers: a bug there must fail one request, not the process.
func guard[T any](what string, fn func() (T, error)) (v T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %s: unexpected failure reading a source file: %v", ErrUpstream, what, r)
		}
	}()
	return fn()
}

// cached returns the stored bytes for key, or fetches them (and stores them
// when keep is set). Concurrent callers for one key share one fetch; a failed
// fetch stores nothing.
func (s *Service) cached(ctx context.Context, key string, keep bool, fetch func(ctx context.Context) ([]byte, error)) ([]byte, error) {
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
		b, err := fetch(ctx)
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

// open returns the parsed COG for a candidate. Open files are kept, most
// recently used first, within Options.MetaMemory: a file dropped from memory
// is opened again from the header pages in the store.
func (s *Service) open(ctx context.Context, c candidate) (*remoteFile, error) {
	fid := fileID(c.url, c.version.Tag)
	if f, ok := s.files.get(fid); ok {
		return f, nil
	}
	v, err := s.flight.do(ctx, "open:"+fid, func(ctx context.Context) (any, error) {
		if f, ok := s.files.get(fid); ok {
			return f, nil
		}
		cf, err := guard(c.src.def.ID, func() (*cog.File, error) {
			return cog.Open(ctx, &rangeReader{s: s, fid: fid, url: c.url})
		})
		if err != nil {
			if errors.Is(err, cog.ErrFormat) {
				return nil, fmt.Errorf("%w: %s: %w", ErrUpstream, c.src.def.ID, err)
			}
			return nil, err
		}
		f := &remoteFile{fid: fid, url: c.url, cog: cf}
		s.files.put(f)
		return f, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*remoteFile), nil
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
		ref, err := guard("index", func() (cog.BlockRef, error) { return f.cog.Locate(ctx, level, col, row) })
		s.files.touch(f) // its index pages may have grown
		if err != nil {
			if errors.Is(err, cog.ErrFormat) {
				err = fmt.Errorf("%w: %w", ErrUpstream, err)
			}
			return nil, err
		}
		var dec []float32
		if ref.Size > 0 {
			// A warm job keeps only the finished tiles: source blocks are many times
			// their size and would push the tiles it just made out of the cache.
			raw, err := s.cached(ctx, key, !warming(ctx), func(ctx context.Context) ([]byte, error) {
				b, err := s.up.Range(ctx, f.url, ref.Offset, ref.Size)
				if err == nil && int64(len(b)) != ref.Size {
					err = fmt.Errorf("%w: short read", ErrUpstream)
				}
				return b, err
			})
			if err != nil {
				return nil, err
			}
			if dec, err = guard("block", func() ([]float32, error) { return f.cog.Decode(level, raw) }); err != nil {
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

// fileLRU keeps open files in memory, bounded by the bytes they hold (their
// header and index pages), least recently used first out.
type fileLRU struct {
	mu    sync.Mutex
	max   int64
	lru   list.List // of *remoteFile
	items map[string]*list.Element
}

func (c *fileLRU) get(fid string) (*remoteFile, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[fid]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(el)
	return el.Value.(*remoteFile), true
}

func (c *fileLRU) put(f *remoteFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]*list.Element{}
	}
	if _, ok := c.items[f.fid]; !ok {
		c.items[f.fid] = c.lru.PushFront(f)
	}
	c.trim()
}

// touch re-checks the budget after a file's pages grew.
func (c *fileLRU) touch(f *remoteFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[f.fid]; ok {
		c.lru.MoveToFront(el)
	}
	c.trim()
}

func (c *fileLRU) remove(fid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[fid]; ok {
		c.lru.Remove(el)
		delete(c.items, fid)
	}
}

// bytes and count report what is held. Caller holds no lock.
func (c *fileLRU) stats() (files int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for el := c.lru.Front(); el != nil; el = el.Next() {
		bytes += el.Value.(*remoteFile).cog.MemBytes()
	}
	return c.lru.Len(), bytes
}

// trim drops least recently used files until the rest fit. Caller holds mu.
func (c *fileLRU) trim() {
	var total int64
	for el := c.lru.Front(); el != nil; el = el.Next() {
		total += el.Value.(*remoteFile).cog.MemBytes()
	}
	for total > c.max && c.lru.Len() > 1 {
		el := c.lru.Back()
		f := el.Value.(*remoteFile)
		total -= f.cog.MemBytes()
		c.lru.Remove(el)
		delete(c.items, f.fid)
	}
}

// MemStats reports what the service holds in memory besides decoded tiles:
// open source files and their pages, decoded source blocks, and work in flight.
type MemStats struct {
	OpenFiles  int   `json:"open_files"`
	FileBytes  int64 `json:"file_bytes"`
	BlockBytes int64 `json:"block_bytes"`
	InFlight   int   `json:"in_flight"`
}

// MemStats returns the current figures.
func (s *Service) MemStats() MemStats {
	n, b := s.files.stats()
	s.blocks.mu.Lock()
	bb := s.blocks.size
	s.blocks.mu.Unlock()
	return MemStats{OpenFiles: n, FileBytes: b, BlockBytes: bb, InFlight: s.flight.active()}
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

// flight merges concurrent work for one key. The work is shared: it goes on
// for as long as at least one caller is waiting for it, so one caller going
// away does not fail the others, and it is cancelled when the last one leaves
// (a stopped warm job or a closed connection leaves nothing running behind).
type flight struct {
	mu sync.Mutex
	m  map[string]*flightCall
	n  int // calls running
	nw int // of them, started by a warm job
}

type flightCall struct {
	done    chan struct{}
	val     any
	err     error
	waiters int
	cancel  context.CancelFunc
	dead    bool // cancelled because every waiter left: not to be joined
}

// workTimeout bounds one piece of shared work.
const workTimeout = 2 * time.Minute

func (f *flight) active() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// warmActive is how many running calls a warm job started.
func (f *flight) warmActive() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nw
}

func (f *flight) do(ctx context.Context, key string, fn func(ctx context.Context) (any, error)) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUpstream, err)
	}
	f.mu.Lock()
	if f.m == nil {
		f.m = map[string]*flightCall{}
	}
	c, ok := f.m[key]
	if !ok || c.dead {
		c = &flightCall{done: make(chan struct{})}
		// The work keeps the starter's values (its gate, its budget) but not its
		// cancellation: its life is tied to the waiters, below.
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workTimeout)
		c.cancel = cancel
		f.m[key] = c
		f.n++
		warm := warming(ctx)
		if warm {
			f.nw++
		}
		go func() {
			defer cancel()
			func() {
				defer func() {
					if r := recover(); r != nil {
						c.err = fmt.Errorf("%w: unexpected failure: %v", ErrUpstream, r)
					}
				}()
				c.val, c.err = fn(wctx)
			}()
			f.mu.Lock()
			if f.m[key] == c {
				delete(f.m, key)
			}
			f.n--
			if warm {
				f.nw--
			}
			f.mu.Unlock()
			close(c.done)
		}()
	}
	c.waiters++
	f.mu.Unlock()
	select {
	case <-c.done:
		return c.val, c.err
	case <-ctx.Done():
		f.mu.Lock()
		c.waiters--
		if c.waiters == 0 {
			c.dead = true
			c.cancel()
		}
		f.mu.Unlock()
		return nil, fmt.Errorf("%w: %w", ErrUpstream, ctx.Err())
	}
}
