package build

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jclement/mapthing/internal/catalog"
)

// Manifest is generations/<id>/manifest.json.
type Manifest struct {
	Generation string           `json:"generation"`
	CreatedAt  time.Time        `json:"created_at"`
	OK         bool             `json:"ok"`
	Regions    []ManifestRegion `json:"regions"`
	Artifacts  map[string]int64 `json:"artifacts"` // relative path → bytes
	Stages     []StageResult    `json:"stages"`
	Checks     []Check          `json:"checks"`
	Disk       *DiskEstimate    `json:"disk,omitempty"`
	Memory     *MemBudget       `json:"memory,omitempty"`
	Image      ImageInfo        `json:"image"`
	// Ocean is where ocean/land polygons came from: osm, approximate:ne-10m or missing.
	Ocean  string  `json:"ocean,omitempty"`
	TotalS float64 `json:"total_s"`
}

// ManifestRegion records which source a region was built from.
type ManifestRegion struct {
	ID              string       `json:"id"`
	Name            string       `json:"name"`
	BBox            catalog.BBox `json:"bbox"`
	Source          string       `json:"source,omitempty"`
	SourceTimestamp string       `json:"source_timestamp,omitempty"`
	Sequence        int64        `json:"sequence,omitempty"`
	SizeBytes       int64        `json:"size_bytes"`
	MD5             string       `json:"md5,omitempty"`
	Parents         []string     `json:"parents,omitempty"` // custom regions: extracts clipped from
}

// StageResult is the timing of one stage.
type StageResult struct {
	Name      string  `json:"name"`
	DurationS float64 `json:"duration_s"`
	PeakRSSKB int64   `json:"peak_rss_kb,omitempty"` // largest subprocess max RSS
	Skipped   bool    `json:"skipped,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// ReadManifest loads a generation's manifest.
func ReadManifest(genDir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(genDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func writeManifest(genDir string, m *Manifest) error {
	data, _ := json.MarshalIndent(m, "", "  ")
	tmp := filepath.Join(genDir, "manifest.json.part")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(genDir, "manifest.json"))
}

// swap writes the manifest, then atomically points current at the new generation and asks the
// supervisor to restart the children.
func (s *state) swap(ctx context.Context) error {
	s.m.OK = true
	s.m.TotalS = s.b.now().Sub(s.m.CreatedAt).Seconds() // up to the swap; Run updates the returned copy
	// Stage timings for swap/retention are appended after this write; the manifest on disk
	// records everything up to and including verify, which is what matters.
	if err := writeManifest(s.genDir, s.m); err != nil {
		return err
	}
	if s.opt.OutDir != "" {
		s.logf("generation %s written to %s; not made live here (copy it to <data>/generations/%s on a server and activate it)",
			s.m.Generation, s.genDir, s.m.Generation)
		return nil
	}
	if err := atomicSymlink(filepath.Join("generations", filepath.Base(s.genDir)), s.b.currentLink()); err != nil {
		return err
	}
	s.logf("current -> generations/%s", filepath.Base(s.genDir))
	if s.opt.Restart != nil {
		if err := s.opt.Restart(ctx); err != nil {
			// The swap happened; a failed restart is the supervisor's problem to retry.
			s.logf("restart after swap failed: %v", err)
		}
	}
	return nil
}

// atomicSymlink makes link point at target, replacing any existing link in one rename.
func atomicSymlink(target, link string) error {
	tmp := fmt.Sprintf("%s.tmp-%d", link, time.Now().UnixNano())
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// CurrentGeneration returns the id current points at, or "".
func (b *Builder) CurrentGeneration() string {
	t, err := os.Readlink(b.currentLink())
	if err != nil {
		return ""
	}
	return filepath.Base(t)
}

// Generation describes one kept generation.
type Generation struct {
	ID       string    `json:"id"`
	Current  bool      `json:"current"`
	Bytes    int64     `json:"bytes"`
	Manifest *Manifest `json:"manifest,omitempty"`
}

// Generations lists generation directories, newest first.
func (b *Builder) Generations() []Generation {
	ents, _ := os.ReadDir(b.generationsDir())
	cur := b.CurrentGeneration()
	var out []Generation
	for _, e := range ents {
		// Dot-prefixed directories are copies in flight (docs/operations.md): not generations.
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(b.generationsDir(), e.Name())
		m, _ := ReadManifest(dir)
		out = append(out, Generation{ID: e.Name(), Current: e.Name() == cur, Bytes: dirSize(dir), Manifest: m})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// KeepGenerations is how many good generations retention keeps (current included).
const KeepGenerations = 2

func (s *state) retention(ctx context.Context) error {
	if s.opt.OutDir != "" {
		return errSkipped // nothing was made live, so nothing is superseded
	}
	keep := ""
	if s.opt.KeepWork {
		keep = filepath.Base(s.workDir)
	}
	removed, err := s.b.prune(keep)
	for _, id := range removed {
		s.logf("removed generation %s", id)
	}
	return err
}

// prune keeps the current generation and the newest other good one, and deletes every other
// generation (failed or older) and any stale work directory.
func (b *Builder) prune(keepWork string) ([]string, error) {
	removed, firstErr := b.pruneGenerations(false)
	// Work dirs belong to builds; Run holds the lock, so any left now are stale.
	if ents, err := os.ReadDir(b.workRoot()); err == nil {
		for _, e := range ents {
			if e.Name() == keepWork {
				continue
			}
			_ = os.RemoveAll(filepath.Join(b.workRoot(), e.Name()))
		}
	}
	return removed, firstErr
}

// pruneGenerations applies the retention rule: the current generation and the newest other
// good one stay. goodOnly leaves directories without a finished manifest alone (adoption:
// such a directory may be a copy still arriving, and is not this process's to delete).
func (b *Builder) pruneGenerations(goodOnly bool) ([]string, error) {
	cur := b.CurrentGeneration()
	kept := 0
	var removed []string
	var firstErr error
	for _, g := range b.Generations() { // newest first
		good := g.Manifest != nil && g.Manifest.OK
		if g.ID == cur || (good && kept < KeepGenerations-1 && cur != "") {
			if g.ID != cur {
				kept++
			}
			continue
		}
		if goodOnly && !good {
			continue
		}
		if err := os.RemoveAll(filepath.Join(b.generationsDir(), g.ID)); err != nil && firstErr == nil {
			firstErr = err
		}
		removed = append(removed, g.ID)
	}
	return removed, firstErr
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}
