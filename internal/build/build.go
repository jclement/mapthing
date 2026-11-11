// Package build produces generations: it refreshes region downloads, merges them, runs
// Planetiler (Protomaps profile), builds the Valhalla graph and the terrain and contour archives, verifies the
// result and atomically swaps /data/current to it. See docs/architecture.md.
package build

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jclement/mapthing/internal/catalog"
)

// Reporter receives progress and log lines for a build. The jobs runner implements it.
type Reporter interface {
	Stage(name string)                 // a new stage begins
	Progress(frac float64, msg string) // 0..1 within the current stage
	Logf(format string, args ...any)   // one log line
}

// Stage names, in order.
const (
	StageGuard     = "guard"
	StageDownload  = "download"
	StageMerge     = "merge"
	StageBasemap   = "basemap"
	StageValhalla  = "valhalla"
	StageTerrain   = "terrain"
	StageVerify    = "verify"
	StageSwap      = "swap"
	StageRetention = "retention"
)

// StageError is returned when a stage fails; LogTail holds the last lines of output for history.
type StageError struct {
	Stage   string
	Err     error
	LogTail []string
}

func (e *StageError) Error() string { return fmt.Sprintf("build stage %s: %v", e.Stage, e.Err) }
func (e *StageError) Unwrap() error { return e.Err }

// ErrBusy means another build is running.
var ErrBusy = errors.New("build: another build is already running")

// Options controls one build.
type Options struct {
	Regions []catalog.Region
	// Refresh re-checks every download for a newer extract; false only downloads missing files.
	Refresh bool
	// SkipRefresh lists region ids excluded from Refresh (their refresh is disabled).
	SkipRefresh map[string]bool
	// Restart is called after the swap so the supervisor restarts valhalla and the renderer.
	Restart func(ctx context.Context) error
	// KeepWork leaves the work directory for debugging.
	KeepWork bool
	// StopAfter ends the build cleanly after the named stage (development: e.g. "merge" to check
	// downloads and clips without building tiles). Nothing is swapped in.
	StopAfter string
	// OutDir writes the generation to this directory instead of <data>/generations/<id>. The
	// directory must be absent or empty. The generation is verified and its manifest written,
	// but it is not made live and nothing is pruned: copy it to a server and adopt it there
	// (Builder.Adopt, docs/operations.md).
	OutDir string
	// ReuseTerrain is a previous generation's directory. When its terrain was built for the
	// same areas with the same settings, terrain.pmtiles (and contours.pmtiles) are hard-linked
	// or copied from it instead of being rebuilt.
	ReuseTerrain string
}

// Builder runs builds against one data directory. Safe for concurrent use; builds serialise.
type Builder struct {
	DataDir string
	Tools   Tools
	// HTTP is used for downloads; nil uses a default client.
	HTTP *http.Client
	// Now is the clock (tests).
	Now func() time.Time

	// Need is which parts of a generation this server serves (the zero value means all): what
	// a generation must carry to count as complete for retention and adoption.
	Need Parts

	mu sync.Mutex
	// busyMu is the short coordination lock for which generation is in use: it guards
	// building, activating and deleting, and a deletion's checks. A generation reserved by
	// one of the three cannot be taken by another, so a delete can never remove a directory
	// a build is writing or an activation is validating, preparing or has just made live
	// (inventory.go). It is never held across slow work.
	busyMu sync.Mutex
	// building is the generation id of the build running now; activating the one an adoption
	// is making live; deleting those a DeleteGeneration is removing.
	building   string
	activating string
	deleting   map[string]bool
	// retMu serialises every read-modify-write of generations/.retention.json (retention.go).
	retMu sync.Mutex
	// removeAll replaces os.RemoveAll for a manual delete (tests pause it).
	removeAll func(string) error
	// symlink replaces the current swap (tests inject a failure).
	symlink func(target, link string) error

	hostMu      sync.Mutex
	failedHosts map[string]time.Time // source hosts that exhausted their retry budget
}

// New returns a Builder with tool paths from the environment.
func New(dataDir string) *Builder {
	return &Builder{DataDir: dataDir, Tools: ToolsFromEnv()}
}

func (b *Builder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Paths under the data directory.
func (b *Builder) downloadsDir() string   { return filepath.Join(b.DataDir, "downloads") }
func (b *Builder) generationsDir() string { return filepath.Join(b.DataDir, "generations") }
func (b *Builder) workRoot() string       { return filepath.Join(b.DataDir, "work") }
func (b *Builder) currentLink() string    { return filepath.Join(b.DataDir, "current") }
func (b *Builder) sourcesDir() string     { return filepath.Join(b.DataDir, "cache", "planetiler") }

// Run performs a whole build. On success the new generation is live and its manifest is
// returned. On failure the live generation is untouched, the new one is deleted and a
// *StageError says which stage failed.
func (b *Builder) Run(ctx context.Context, rep Reporter, opt Options) (*Manifest, error) {
	if !b.mu.TryLock() {
		return nil, ErrBusy
	}
	defer b.mu.Unlock()
	if len(opt.Regions) == 0 {
		return nil, &StageError{Stage: StageGuard, Err: errors.New("no regions installed")}
	}

	start := b.now().UTC()
	genID := start.Format("20060102T150405Z")
	genDir := filepath.Join(b.generationsDir(), genID)
	if opt.OutDir != "" {
		var err error
		if genDir, err = prepareOutDir(opt.OutDir); err != nil {
			return nil, &StageError{Stage: StageGuard, Err: err}
		}
	}
	workDir := filepath.Join(b.workRoot(), genID)
	m := &Manifest{Generation: genID, CreatedAt: start, Artifacts: map[string]int64{}, Image: imageInfo(b.Tools)}
	for _, r := range opt.Regions {
		m.Regions = append(m.Regions, ManifestRegion{ID: r.ID, Name: r.Name, BBox: r.BBox})
	}
	st := &state{b: b, rep: rep, opt: opt, genDir: genDir, workDir: workDir, m: m, tail: newRing(60)}
	if opt.OutDir == "" {
		// So that if this process is killed, a later build knows the leftover directory is its
		// own and may delete it (retention.go).
		b.EnsureRetentionRecord()
		b.reserveBuild(genID)
		_ = b.updateRetention(func(r *retentionRecord) { r.Building = append(r.Building, genID) })
		defer func() {
			b.reserveBuild("")
			_ = b.updateRetention(func(r *retentionRecord) { r.Building = without(r.Building, genID) })
		}()
	}

	err := st.run(ctx)
	stoppedEarly := errors.Is(err, errStoppedEarly)
	if stoppedEarly {
		err = nil
	}
	m.TotalS = b.now().Sub(start).Seconds()
	if !opt.KeepWork {
		_ = os.RemoveAll(workDir)
	}
	if stoppedEarly {
		_ = os.RemoveAll(genDir)
		return m, nil
	}
	if err != nil {
		_ = os.RemoveAll(genDir)
		var se *StageError
		if errors.As(err, &se) && se.LogTail == nil {
			se.LogTail = st.tail.lines()
		}
		return m, err
	}
	return m, nil
}

// state is one build in progress.
type state struct {
	b       *Builder
	rep     Reporter
	opt     Options
	genDir  string
	workDir string
	m       *Manifest
	tail    *ring

	sources  []string // per region pbf path, aligned with opt.Regions
	merged   string
	peakKB   int64 // peak RSS of subprocesses in the current stage
	terrain  bool
	extracts map[string]extractResult // per-build download memo
}

func (s *state) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	s.tail.add(line)
	s.rep.Logf("%s", line)
}

func (s *state) run(ctx context.Context) error {
	stages := []struct {
		name string
		fn   func(context.Context) error
	}{
		{StageGuard, s.guard},
		{StageDownload, s.download},
		{StageMerge, s.merge},
		{StageBasemap, s.basemap},
		{StageValhalla, s.valhalla},
		{StageTerrain, s.terrainStage},
		{StageVerify, s.verify},
		{StageSwap, s.swap},
		{StageRetention, s.retention},
	}
	for _, stg := range stages {
		if err := ctx.Err(); err != nil {
			return &StageError{Stage: stg.name, Err: err}
		}
		s.rep.Stage(stg.name)
		s.peakKB = 0
		t0 := time.Now()
		s.logf("== stage %s", stg.name)
		err := stg.fn(ctx)
		res := StageResult{Name: stg.name, DurationS: time.Since(t0).Seconds(), PeakRSSKB: s.peakKB}
		if errors.Is(err, errSkipped) {
			res.Skipped = true
			err = nil
		}
		if err != nil {
			res.Error = err.Error()
		}
		s.m.Stages = append(s.m.Stages, res)
		if err != nil {
			s.logf("stage %s failed: %v", stg.name, err)
			return &StageError{Stage: stg.name, Err: err}
		}
		s.logf("stage %s done in %.1fs", stg.name, res.DurationS)
		if stg.name == s.opt.StopAfter {
			s.logf("stopping after %s as asked; nothing swapped in", stg.name)
			return errStoppedEarly
		}
	}
	return nil
}

var (
	errSkipped      = errors.New("skipped")
	errStoppedEarly = errors.New("stopped early")
)

func (s *state) merge(ctx context.Context) error {
	if err := os.MkdirAll(s.workDir, 0o755); err != nil {
		return err
	}
	if len(s.sources) == 1 {
		s.merged = s.sources[0]
		s.logf("single region: using %s directly", s.merged)
		return nil
	}
	s.merged = filepath.Join(s.workDir, "merged.osm.pbf")
	args := append([]string{"merge", "--overwrite", "-o", s.merged}, s.sources...)
	if err := s.exec(ctx, s.workDir, s.b.Tools.Osmium, args...); err != nil {
		return err
	}
	return nil
}

// prepareOutDir returns the absolute path of an Options.OutDir, refusing one that already
// holds files: a build never writes into, or cleans up, somebody else's directory.
func prepareOutDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	ents, err := os.ReadDir(abs)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("output directory: %w", err)
	}
	if len(ents) > 0 {
		return "", fmt.Errorf("output directory %s is not empty; give a new or empty directory", abs)
	}
	return abs, nil
}

func (s *state) terrainStage(ctx context.Context) error {
	f := hooks.terrain()
	if f == nil {
		s.logf("no terrain builder registered; skipping")
		return errSkipped
	}
	out := filepath.Join(s.genDir, "terrain.pmtiles")
	cout := filepath.Join(s.genDir, "contours.pmtiles")
	record := func() {
		for _, p := range []string{out, cout} {
			if st, err := os.Stat(p); err == nil {
				s.m.Artifacts[filepath.Base(p)] = st.Size()
			}
		}
	}
	reused := false
	if dir := s.opt.ReuseTerrain; dir != "" {
		why, err := s.reuseTerrain(dir, out)
		if err != nil {
			return err
		}
		if reused = why == ""; !reused {
			s.logf("terrain: not reusing %s: %s; building it", dir, why)
		}
	}
	if !reused {
		wd := filepath.Join(s.workDir, "terrain")
		if err := os.MkdirAll(wd, 0o755); err != nil {
			return err
		}
		if err := f(ctx, &stageReporter{s}, s.opt.Regions, wd, out); err != nil {
			return err
		}
	}
	s.terrain = true
	cf := hooks.contoursFunc()
	if cf == nil {
		record()
		return nil
	}
	if reused && s.reuseContours(s.opt.ReuseTerrain, out, cout) {
		record()
		return nil
	}
	if err := cf(ctx, &stageReporter{s}, out, cout); err != nil {
		return fmt.Errorf("contours: %w", err)
	}
	writeContoursInfo(out, cout)
	record()
	return nil
}

// stageReporter forwards a hook's output into the build's log tail.
type stageReporter struct{ s *state }

func (r *stageReporter) Stage(name string)               { r.s.logf("-- %s", name) }
func (r *stageReporter) Progress(f float64, msg string)  { r.s.rep.Progress(f, msg) }
func (r *stageReporter) Logf(format string, args ...any) { r.s.logf(format, args...) }

// ring keeps the last n lines.
type ring struct {
	mu  sync.Mutex
	buf []string
	n   int
}

func newRing(n int) *ring { return &ring{n: n} }

func (r *ring) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, strings.TrimRight(line, "\r\n"))
	if len(r.buf) > r.n {
		r.buf = r.buf[len(r.buf)-r.n:]
	}
}

func (r *ring) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.buf...)
}
