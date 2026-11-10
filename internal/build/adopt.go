package build

// Adopting a generation built elsewhere.
//
// A generation directory is self-contained: mapthing-build -out writes one on a build machine,
// and a copy of it under <data>/generations/<id>/ can be validated and made live on a server
// that never ran the build. Adopt does what the end of a build does (the atomic current swap,
// the restart hook, retention) for a directory that is already there. See docs/operations.md.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jclement/mapthing/internal/pmtiles"
)

// Parts says which parts of a generation a server serves, and so which files a generation
// must carry to be valid there. A routing and elevation backend needs no basemap.
type Parts struct {
	Tiles     bool // basemap.pmtiles (and contours.pmtiles when the build made them)
	Routing   bool // valhalla/
	Elevation bool // terrain.pmtiles when the build made one
}

// AllParts is a full server: everything a build writes must be there.
func AllParts() Parts { return Parts{Tiles: true, Routing: true, Elevation: true} }

func (p Parts) String() string {
	var out []string
	for _, x := range []struct {
		on   bool
		name string
	}{{p.Tiles, "tiles"}, {p.Routing, "routing"}, {p.Elevation, "elevation"}} {
		if x.on {
			out = append(out, x.name)
		}
	}
	return strings.Join(out, ",")
}

// ErrNoGeneration means there is no such generation directory.
var ErrNoGeneration = errors.New("no such generation")

// InvalidError lists why a generation directory cannot be served.
type InvalidError struct {
	ID       string
	Problems []string
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("generation %s is incomplete or invalid: %s", e.ID, strings.Join(e.Problems, "; "))
}

// PrepareError means the generation's files are valid but a service could not load them (the
// routing graph did not start); nothing was swapped.
type PrepareError struct{ Err error }

func (e *PrepareError) Error() string { return e.Err.Error() }
func (e *PrepareError) Unwrap() error { return e.Err }

var generationIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// ValidGenerationID reports whether id can name a generation directory (no path separators,
// no leading dot: dot-prefixed directories are copies in flight).
func ValidGenerationID(id string) bool { return generationIDRE.MatchString(id) }

// artifactPart maps a manifest artifact to the part of the service that needs it.
func artifactPart(name string, p Parts) bool {
	switch {
	case name == "terrain.pmtiles":
		return p.Elevation
	case strings.HasPrefix(name, "valhalla/"):
		return p.Routing
	default: // basemap.pmtiles, contours.pmtiles and anything a later build adds
		return p.Tiles
	}
}

// Validate checks that genDir holds a finished generation with everything need asks for:
// a manifest of a build that succeeded, every artifact the manifest lists (for a served part)
// present at its recorded size, archives that open, and a routing graph with its config. id,
// when given, must be the generation the manifest names. It returns an *InvalidError listing
// every problem found.
func Validate(genDir, id string, need Parts) (*Manifest, error) {
	name := id
	if name == "" {
		name = filepath.Base(genDir)
	}
	bad := func(problems ...string) (*Manifest, error) {
		return nil, &InvalidError{ID: name, Problems: problems}
	}
	m, err := ReadManifest(genDir)
	if os.IsNotExist(err) {
		return bad("manifest.json is missing (the copy is not finished, or this is not a generation)")
	}
	if err != nil {
		return bad("manifest.json does not read: " + err.Error())
	}
	var problems []string
	if !m.OK {
		problems = append(problems, "the manifest is of a build that did not finish (ok is false)")
	}
	if id != "" && m.Generation != id {
		problems = append(problems, fmt.Sprintf("the directory is named %s but its manifest is generation %q; name the directory after the manifest's generation", id, m.Generation))
	}
	size := func(rel string) (int64, bool) {
		st, err := os.Stat(filepath.Join(genDir, filepath.FromSlash(rel)))
		if err != nil || st.IsDir() {
			return 0, false
		}
		return st.Size(), true
	}
	names := make([]string, 0, len(m.Artifacts))
	for n := range m.Artifacts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !filepath.IsLocal(filepath.FromSlash(n)) {
			problems = append(problems, fmt.Sprintf("manifest artifact %q is not a path inside the generation", n))
			continue
		}
		got, ok := size(n)
		switch {
		case !ok && artifactPart(n, need):
			problems = append(problems, n+" is missing")
		case ok && got != m.Artifacts[n]:
			problems = append(problems, fmt.Sprintf("%s is %d bytes, the manifest says %d (partial copy?)", n, got, m.Artifacts[n]))
		}
	}
	opens := func(rel string, mvt bool) {
		if _, ok := size(rel); !ok {
			return
		}
		r, err := pmtiles.Open(filepath.Join(genDir, rel))
		if err != nil {
			problems = append(problems, rel+" does not open: "+err.Error())
			return
		}
		if mvt && r.Header.TileType != pmtiles.TypeMVT {
			problems = append(problems, rel+" is not a vector tile archive")
		}
		_ = r.Close()
	}
	if need.Tiles {
		if _, ok := size("basemap.pmtiles"); !ok {
			if _, listed := m.Artifacts["basemap.pmtiles"]; !listed {
				problems = append(problems, "basemap.pmtiles is missing")
			}
		}
		opens("basemap.pmtiles", true)
		opens("contours.pmtiles", true)
	}
	if need.Elevation {
		opens("terrain.pmtiles", false)
	}
	if need.Routing {
		if n, ok := size("valhalla/tiles.tar"); !ok {
			if _, listed := m.Artifacts["valhalla/tiles.tar"]; !listed {
				problems = append(problems, "valhalla/tiles.tar is missing")
			}
		} else if n == 0 {
			problems = append(problems, "valhalla/tiles.tar is empty")
		}
		b, err := os.ReadFile(ValhallaConfig(genDir))
		switch {
		case err != nil:
			problems = append(problems, "valhalla/valhalla.json is missing")
		case !json.Valid(b):
			problems = append(problems, "valhalla/valhalla.json is not valid JSON")
		}
	}
	if len(problems) > 0 {
		return bad(problems...)
	}
	return m, nil
}

// AdoptOptions controls Adopt.
type AdoptOptions struct {
	// Need is what this server serves; the zero value means everything.
	Need Parts
	// Prepare runs after validation and before the swap, with the generation's directory. A
	// server starts its standby routing instance on the new graph here; an error leaves the
	// live generation untouched.
	Prepare func(ctx context.Context, genDir string) error
	// Restart is called after the swap (as after a build) so services follow at once.
	Restart func(ctx context.Context) error
}

// AdoptResult describes an adoption.
type AdoptResult struct {
	ID          string    `json:"id"`
	Previous    string    `json:"previous,omitempty"` // the generation that was live before
	AlreadyLive bool      `json:"already_live"`
	Removed     []string  `json:"removed,omitempty"` // generations retention deleted
	Manifest    *Manifest `json:"-"`
}

// Adopt validates <data>/generations/<id> and makes it the live generation with the same
// atomic swap a build ends with, then applies the retention rule: the live generation and the
// newest other complete one stay. A directory that does not validate is left alone and does
// not count as the one kept, since it may be a copy still arriving. Adopting the live generation again is a no-op that
// succeeds. A failed validation or Prepare changes nothing. ErrBusy while a build runs.
func (b *Builder) Adopt(ctx context.Context, id string, opt AdoptOptions) (*AdoptResult, error) {
	if !ValidGenerationID(id) {
		return nil, fmt.Errorf("%w: %q is not a generation id", ErrNoGeneration, id)
	}
	if !b.mu.TryLock() {
		return nil, ErrBusy
	}
	defer b.mu.Unlock()
	genDir := filepath.Join(b.generationsDir(), id)
	if fi, err := os.Stat(genDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: there is no directory %s", ErrNoGeneration, genDir)
	}
	cur := b.CurrentGeneration()
	if cur == id {
		m, _ := ReadManifest(genDir)
		return &AdoptResult{ID: id, AlreadyLive: true, Manifest: m}, nil
	}
	need := opt.Need
	if need == (Parts{}) {
		need = AllParts()
	}
	m, err := Validate(genDir, id, need)
	if err != nil {
		return nil, err
	}
	res := &AdoptResult{ID: id, Manifest: m}
	res.Previous = cur
	if opt.Prepare != nil {
		if err := opt.Prepare(ctx, genDir); err != nil {
			return nil, &PrepareError{Err: err}
		}
	}
	if err := atomicSymlink(filepath.Join("generations", id), b.currentLink()); err != nil {
		return nil, err
	}
	if opt.Restart != nil {
		_ = opt.Restart(ctx) // the swap happened; services retry on their own
	}
	res.Removed, _ = b.pruneGenerations(func(g Generation) bool {
		_, err := Validate(filepath.Join(b.generationsDir(), g.ID), g.ID, need)
		return err == nil
	})
	return res, nil
}
