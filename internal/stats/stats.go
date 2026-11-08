// Package stats counts requests per key and kind, and cache hits, in memory and flushes them
// to SQLite every 30 seconds.
package stats

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jclement/mapthing/internal/store"
)

// Kinds of counted requests.
const (
	KindTile      = "tile"
	KindRaster    = "raster"
	KindRoute     = "route"
	KindElevation = "elevation"
	KindPack      = "pack"
	KindCompat    = "compat"
)

// Stats holds unflushed counters.
type Stats struct {
	st      *store.Store
	now     func() time.Time
	started time.Time

	mu    sync.Mutex
	usage map[store.UsageKey]int64
	cache map[store.CacheKey][2]int64
	used  map[int64]time.Time

	// lifetime totals since start (for the UI), never reset by flush
	total     map[string]int64
	cacheLife map[string][2]int64
}

// New returns a Stats bound to st.
func New(st *store.Store) *Stats {
	return &Stats{
		st: st, now: time.Now, started: time.Now(),
		usage: map[store.UsageKey]int64{}, cache: map[store.CacheKey][2]int64{}, used: map[int64]time.Time{},
		total: map[string]int64{}, cacheLife: map[string][2]int64{},
	}
}

// Started returns the process start time.
func (s *Stats) Started() time.Time { return s.started }

func (s *Stats) day() string { return s.now().UTC().Format("2006-01-02") }

// Count records one request by keyID (0 = admin session) of kind.
func (s *Stats) Count(keyID int64, kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage[store.UsageKey{KeyID: keyID, Day: s.day(), Kind: kind}]++
	s.total[kind]++
	if keyID > 0 {
		s.used[keyID] = s.now()
	}
}

// Cache records a hit or miss for a named cache (raster, imagery, pack).
func (s *Stats) Cache(name string, hit bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := store.CacheKey{Cache: name, Day: s.day()}
	v := s.cache[k]
	l := s.cacheLife[name]
	if hit {
		v[0]++
		l[0]++
	} else {
		v[1]++
		l[1]++
	}
	s.cache[k] = v
	s.cacheLife[name] = l
}

// Pending returns a copy of unflushed usage (for merging into reports).
func (s *Stats) Pending() (map[store.UsageKey]int64, map[store.CacheKey][2]int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := make(map[store.UsageKey]int64, len(s.usage))
	for k, v := range s.usage {
		u[k] = v
	}
	c := make(map[store.CacheKey][2]int64, len(s.cache))
	for k, v := range s.cache {
		c[k] = v
	}
	return u, c
}

// Totals returns lifetime-since-start request totals per kind and cache hit/miss totals.
func (s *Stats) Totals() (map[string]int64, map[string][2]int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := make(map[string]int64, len(s.total))
	for k, v := range s.total {
		t[k] = v
	}
	c := make(map[string][2]int64, len(s.cacheLife))
	for k, v := range s.cacheLife {
		c[k] = v
	}
	return t, c
}

// Flush writes unflushed counters to the database. On failure they are kept for next time.
func (s *Stats) Flush(ctx context.Context) error {
	s.mu.Lock()
	u, c, used := s.usage, s.cache, s.used
	s.usage, s.cache, s.used = map[store.UsageKey]int64{}, map[store.CacheKey][2]int64{}, map[int64]time.Time{}
	s.mu.Unlock()
	if err := s.st.AddUsage(ctx, u, c); err != nil {
		s.mu.Lock()
		for k, v := range u {
			s.usage[k] += v
		}
		for k, v := range c {
			o := s.cache[k]
			s.cache[k] = [2]int64{o[0] + v[0], o[1] + v[1]}
		}
		s.restoreUsedLocked(used)
		s.mu.Unlock()
		return err
	}
	if err := s.st.TouchKeys(ctx, used); err != nil {
		s.mu.Lock()
		s.restoreUsedLocked(used)
		s.mu.Unlock()
		return err
	}
	return nil
}

func (s *Stats) restoreUsedLocked(used map[int64]time.Time) {
	for k, t := range used {
		if t.After(s.used[k]) {
			s.used[k] = t
		}
	}
}

// Run flushes every interval until ctx is done, then flushes once more.
func (s *Stats) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.Flush(fctx); err != nil {
				slog.Warn("stats final flush", "err", err)
			}
			cancel()
			return
		case <-t.C:
			if err := s.Flush(ctx); err != nil {
				slog.Warn("stats flush", "err", err)
			}
		}
	}
}
