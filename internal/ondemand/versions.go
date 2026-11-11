package ondemand

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// versionBook remembers which release of each remote file is current, so a
// tile or block cached from an older release is not served once the host
// publishes a new one, and so the hosts are asked about a file only once per
// VersionTTL.
type versionBook struct {
	path string

	mu      sync.Mutex
	Files   map[string]versionEntry `json:"files"`
	Index   []indexItem             `json:"hrdem_index,omitempty"`
	IndexAt time.Time               `json:"hrdem_index_at,omitzero"`
	dirty   bool
	saving  bool
}

type versionEntry struct {
	Version
	Checked time.Time `json:"checked"`
}

func loadVersionBook(path string) *versionBook {
	b := &versionBook{path: path, Files: map[string]versionEntry{}}
	if path == "" {
		return b
	}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, b)
		if b.Files == nil {
			b.Files = map[string]versionEntry{}
		}
	}
	return b
}

// touch schedules a save (at most one write every few seconds).
func (b *versionBook) touch() {
	if b.path == "" {
		return
	}
	b.dirty = true
	if b.saving {
		return
	}
	b.saving = true
	time.AfterFunc(3*time.Second, b.save)
}

func (b *versionBook) save() {
	b.mu.Lock()
	raw, _ := json.Marshal(b)
	b.dirty, b.saving = false, false
	b.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(b.path), 0o755); err != nil {
		return
	}
	tmp := b.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		_ = os.Rename(tmp, b.path)
	}
}

// version returns the current version of a remote file. A fresh remembered
// answer is used as is; a stale one is refreshed from the host, and kept if
// the host cannot be reached (cached data stays usable offline).
func (s *Service) version(ctx context.Context, url string) (Version, error) {
	now := s.opt.Now()
	s.book.mu.Lock()
	e, ok := s.book.Files[url]
	s.book.mu.Unlock()
	if ok && now.Sub(e.Checked) < s.opt.VersionTTL {
		return e.Version, nil
	}
	v, err := s.flight.do(ctx, "head:"+url, func(ctx context.Context) (any, error) {
		v, err := s.up.Head(ctx, url)
		if err != nil {
			return nil, err
		}
		s.book.mu.Lock()
		old, had := s.book.Files[url]
		s.book.Files[url] = versionEntry{Version: v, Checked: s.opt.Now()}
		s.book.touch()
		s.book.mu.Unlock()
		if had && old.Exists && old.Tag != v.Tag {
			s.forget(url, old.Tag)
		}
		return v, nil
	})
	if err != nil {
		if ok && errors.Is(err, ErrUpstream) {
			return e.Version, nil
		}
		return Version{}, err
	}
	return v.(Version), nil
}

// forget drops everything cached from a superseded release of a file.
func (s *Service) forget(url, tag string) {
	fid := fileID(url, tag)
	s.mu.Lock()
	delete(s.files, fid)
	s.mu.Unlock()
	if s.opt.Store != nil {
		s.opt.Store.RemovePrefix("src/" + fid + "/")
	}
}

// hrdemIndex returns the list of HRDEM mosaics, refreshed once per VersionTTL.
func (s *Service) hrdemIndex(ctx context.Context, base string) ([]indexItem, error) {
	now := s.opt.Now()
	s.book.mu.Lock()
	items, at := s.book.Index, s.book.IndexAt
	s.book.mu.Unlock()
	if len(items) > 0 && now.Sub(at) < s.opt.VersionTTL {
		return items, nil
	}
	v, err := s.flight.do(ctx, "hrdem-index", func(ctx context.Context) (any, error) {
		got, err := fetchHRDEMIndex(ctx, s.up, base)
		if err != nil {
			return nil, err
		}
		s.book.mu.Lock()
		s.book.Index, s.book.IndexAt = got, s.opt.Now()
		s.book.touch()
		s.book.mu.Unlock()
		return got, nil
	})
	if err != nil {
		if len(items) > 0 && errors.Is(err, ErrUpstream) {
			return items, nil
		}
		return nil, err
	}
	return v.([]indexItem), nil
}
