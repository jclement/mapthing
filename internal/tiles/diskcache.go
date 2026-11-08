package tiles

import (
	"container/list"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DiskCache is a size-bounded, least-recently-used cache of files under a directory. Keys are
// slash-separated relative paths. The index lives in memory and is rebuilt from the
// directory at start (ordered by modification time).
type DiskCache struct {
	dir string
	max int64

	mu    sync.Mutex
	lru   *list.List // front = most recent; values are *cacheEntry
	index map[string]*list.Element
	size  int64

	flight flight
}

type cacheEntry struct {
	key  string
	size int64
}

// NewDiskCache opens (creating) dir with a byte budget (<= 0 disables storing).
func NewDiskCache(dir string, maxBytes int64) (*DiskCache, error) {
	c := &DiskCache{dir: dir, max: maxBytes, lru: list.New(), index: map[string]*list.Element{}}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	type found struct {
		key  string
		size int64
		mod  time.Time
	}
	var all []found
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".tmp-") {
			_ = os.Remove(p)
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		all = append(all, found{filepath.ToSlash(rel), fi.Size(), fi.ModTime()})
		return nil
	})
	sort.Slice(all, func(i, j int) bool { return all[i].mod.After(all[j].mod) })
	for _, f := range all {
		c.index[f.key] = c.lru.PushBack(&cacheEntry{f.key, f.size})
		c.size += f.size
	}
	c.mu.Lock()
	c.evictLocked()
	c.mu.Unlock()
	return c, nil
}

// Dir returns the cache directory.
func (c *DiskCache) Dir() string { return c.dir }

// Size returns the bytes currently cached.
func (c *DiskCache) Size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size
}

func (c *DiskCache) path(key string) string { return filepath.Join(c.dir, filepath.FromSlash(key)) }

// Get returns the file path for key if cached, marking it recently used.
func (c *DiskCache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[key]
	if !ok {
		return "", false
	}
	c.lru.MoveToFront(el)
	return c.path(key), true
}

// Put stores the output of write under key and returns its path. write must write the
// complete content; a failed write leaves nothing behind.
func (c *DiskCache) Put(key string, write func(w io.Writer) error) (string, error) {
	if !validKey(key) {
		return "", errors.New("invalid cache key")
	}
	dst := c.path(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return "", err
	}
	if err := write(f); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	fi, err := f.Stat()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := os.Rename(f.Name(), dst); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[key]; ok {
		c.size -= el.Value.(*cacheEntry).size
		c.lru.Remove(el)
	}
	c.index[key] = c.lru.PushFront(&cacheEntry{key, fi.Size()})
	c.size += fi.Size()
	c.evictLocked()
	return dst, nil
}

// PutBytes stores b under key.
func (c *DiskCache) PutBytes(key string, b []byte) (string, error) {
	return c.Put(key, func(w io.Writer) error { _, err := w.Write(b); return err })
}

// Do runs fill once per key across concurrent callers: if key is cached it returns at once
// (hit=true); otherwise one caller runs fill (which must Put the key) and the rest wait.
func (c *DiskCache) Do(key string, fill func() error) (path string, hit bool, err error) {
	if p, ok := c.Get(key); ok {
		return p, true, nil
	}
	err = c.flight.do(key, func() error {
		if _, ok := c.Get(key); ok {
			return nil
		}
		return fill()
	})
	if err != nil {
		return "", false, err
	}
	p, ok := c.Get(key)
	if !ok {
		return "", false, errors.New("cache fill did not store the entry")
	}
	return p, false, nil
}

// RemoveIf deletes every entry for which match returns true.
func (c *DiskCache) RemoveIf(match func(key string) bool) {
	c.mu.Lock()
	var gone []string
	for k, el := range c.index {
		if match(k) {
			c.size -= el.Value.(*cacheEntry).size
			c.lru.Remove(el)
			delete(c.index, k)
			gone = append(gone, k)
		}
	}
	c.mu.Unlock()
	for _, k := range gone {
		_ = os.Remove(c.path(k))
	}
	removeEmptyDirs(c.dir)
}

func (c *DiskCache) evictLocked() {
	for c.size > c.max && c.lru.Len() > 0 {
		el := c.lru.Back()
		e := el.Value.(*cacheEntry)
		c.lru.Remove(el)
		delete(c.index, e.key)
		c.size -= e.size
		_ = os.Remove(c.path(e.key))
	}
}

func validKey(k string) bool {
	if k == "" || strings.HasPrefix(k, "/") || strings.Contains(k, "\\") {
		return false
	}
	for _, part := range strings.Split(k, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func removeEmptyDirs(root string) {
	var dirs []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != root {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // fails (harmlessly) when not empty
	}
}

// flight deduplicates concurrent work per key.
type flight struct {
	mu sync.Mutex
	m  map[string]*call
}

type call struct {
	wg  sync.WaitGroup
	err error
}

func (f *flight) do(key string, fn func() error) error {
	f.mu.Lock()
	if f.m == nil {
		f.m = map[string]*call{}
	}
	if c, ok := f.m[key]; ok {
		f.mu.Unlock()
		c.wg.Wait()
		return c.err
	}
	c := &call{}
	c.wg.Add(1)
	f.m[key] = c
	f.mu.Unlock()
	c.err = fn()
	c.wg.Done()
	f.mu.Lock()
	delete(f.m, key)
	f.mu.Unlock()
	return c.err
}
