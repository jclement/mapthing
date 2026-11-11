package logs

import (
	"slices"
	"strings"
	"time"
)

// Filter selects entries. The zero value matches everything.
type Filter struct {
	Sources  []string  // any of these (empty = all)
	MinLevel string    // this level and above ("" = all)
	Text     string    // case-insensitive substring of the message
	Job      string    // build job id
	Since    time.Time // at or after
	Until    time.Time // before
	Before   int64     // id below this (paging back)
	After    int64     // id above this (catching up)
}

// Match reports whether e passes the filter.
func (f *Filter) Match(e Entry) bool {
	if f.Before != 0 && e.ID >= f.Before {
		return false
	}
	if f.After != 0 && e.ID <= f.After {
		return false
	}
	if len(f.Sources) > 0 && !slices.Contains(f.Sources, e.Source) {
		return false
	}
	if f.MinLevel != "" && levelRank(e.Level) < levelRank(f.MinLevel) {
		return false
	}
	if f.Job != "" && e.Job != f.Job {
		return false
	}
	if !f.Since.IsZero() && e.Time.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !e.Time.Before(f.Until) {
		return false
	}
	if f.Text != "" && !strings.Contains(strings.ToLower(e.Msg), strings.ToLower(f.Text)) {
		return false
	}
	return true
}

// ValidLevel reports whether l is a level name (or empty).
func ValidLevel(l string) bool {
	switch l {
	case "", LevelDebug, LevelInfo, LevelWarn, LevelError:
		return true
	}
	return false
}

// Query returns the newest limit entries matching f, oldest first, and whether older matching
// entries exist beyond them. It answers from memory when the ring holds enough, and reads the
// files otherwise, so it reaches as far back as they go.
func (s *Sink) Query(f Filter, limit int) (entries []Entry, more bool) {
	if limit <= 0 {
		limit = 500
	}
	ring := s.snapshot()
	var got []Entry // newest first
	for i := len(ring) - 1; i >= 0; i-- {
		if f.Match(ring[i]) {
			if len(got) == limit {
				slices.Reverse(got)
				return got, true
			}
			got = append(got, ring[i])
		}
	}
	// The ring ran out before the limit. If it starts after the filter's own lower bound there
	// may be more in the files.
	if s.opt.Dir == "" || (len(ring) > 0 && f.After != 0 && ring[0].ID <= f.After) ||
		(len(ring) > 0 && !f.Since.IsZero() && ring[0].Time.Before(f.Since)) {
		slices.Reverse(got)
		return got, false
	}
	got = got[:0]
	s.fileMu.RLock()
	defer s.fileMu.RUnlock()
	files := s.files()
	for i := len(files) - 1; i >= 0; i-- {
		var seg []Entry
		readFile(files[i], func(e Entry) {
			if f.Match(e) {
				seg = append(seg, e)
			}
		})
		for j := len(seg) - 1; j >= 0; j-- {
			if len(got) == limit {
				slices.Reverse(got)
				return got, true
			}
			got = append(got, seg[j])
		}
	}
	slices.Reverse(got)
	return got, false
}

// Each calls fn for every entry matching f, oldest first (a download of the filtered view).
// It stops when fn returns false.
func (s *Sink) Each(f Filter, fn func(Entry) bool) {
	if s.opt.Dir == "" {
		for _, e := range s.snapshot() {
			if f.Match(e) && !fn(e) {
				return
			}
		}
		return
	}
	s.fileMu.RLock()
	defer s.fileMu.RUnlock()
	stop := false
	for _, file := range s.files() {
		readFile(file, func(e Entry) {
			if !stop && f.Match(e) && !fn(e) {
				stop = true
			}
		})
		if stop {
			return
		}
	}
}
