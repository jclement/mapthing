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

// plainFile looks at genDir/rel without following symbolic links. A generation is
// self-contained: every file is a regular file inside the directory, and no part of its path
// is a link (which could point at another generation, a copy in flight, or anywhere else, and
// would then be served, counted and deleted under the wrong name).
func plainFile(genDir, rel string) (size int64, exists bool, problem string) {
	p := genDir
	parts := strings.Split(filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel))), "/")
	for i, part := range parts {
		p = filepath.Join(p, part)
		fi, err := os.Lstat(p)
		if err != nil {
			return 0, false, ""
		}
		at := strings.Join(parts[:i+1], "/")
		if fi.Mode()&os.ModeSymlink != 0 {
			return 0, true, at + " is a symbolic link; a generation must hold its own files"
		}
		last := i == len(parts)-1
		if !last && !fi.IsDir() {
			return 0, true, at + " is not a directory"
		}
		if last {
			if !fi.Mode().IsRegular() {
				return 0, true, at + " is not a regular file"
			}
			return fi.Size(), true, ""
		}
	}
	return 0, false, ""
}

// ReadValhallaConfig reads a generation's valhalla.json as the JSON object the routing
// service is configured from. Anything else that happens to be valid JSON (null, a number, an
// array) is an error, as is a config without the graph section.
func ReadValhallaConfig(genDir string) (map[string]any, error) {
	data, err := os.ReadFile(ValhallaConfig(genDir))
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}
	cfg, ok := v.(map[string]any)
	if !ok || cfg == nil {
		return nil, errors.New("not a JSON object")
	}
	if mj, ok := cfg["mjolnir"].(map[string]any); !ok || mj == nil {
		return nil, errors.New(`has no "mjolnir" object (the graph settings)`)
	}
	if h, present := cfg["httpd"]; present {
		hm, ok := h.(map[string]any)
		if !ok || hm == nil {
			return nil, errors.New(`"httpd" is not an object`)
		}
		if svc, present := hm["service"]; present {
			if sm, ok := svc.(map[string]any); !ok || sm == nil {
				return nil, errors.New(`"httpd.service" is not an object`)
			}
		}
	}
	return cfg, nil
}

// Validate checks that genDir holds a finished generation with everything need asks for:
// a manifest of a build that succeeded, every artifact the manifest lists (for a served part)
// present at its recorded size as a regular file with no symbolic link in its path, archives
// that open, and a routing graph with a usable config. id, when given, must be the generation
// the manifest names. It returns an *InvalidError listing every problem found.
func Validate(genDir, id string, need Parts) (*Manifest, error) {
	name := id
	if name == "" {
		name = filepath.Base(genDir)
	}
	bad := func(problems ...string) (*Manifest, error) {
		return nil, &InvalidError{ID: name, Problems: problems}
	}
	if _, ok, problem := plainFile(genDir, "manifest.json"); problem != "" {
		return bad(problem)
	} else if !ok {
		return bad("manifest.json is missing (the copy is not finished, or this is not a generation)")
	}
	m, err := ReadManifest(genDir)
	if err != nil {
		return bad("manifest.json does not read: " + err.Error())
	}
	var problems []string
	seen := map[string]bool{}
	// size reports a file of the generation, recording once anything wrong with its path.
	size := func(rel string) (int64, bool) {
		n, ok, problem := plainFile(genDir, rel)
		if problem != "" {
			if !seen[problem] {
				seen[problem] = true
				problems = append(problems, problem)
			}
			return 0, false
		}
		return n, ok
	}
	wrongPath := func(rel string) bool {
		_, _, problem := plainFile(genDir, rel)
		return problem != ""
	}
	if !m.OK {
		problems = append(problems, "the manifest is of a build that did not finish (ok is false)")
	}
	if id != "" && m.Generation != id {
		problems = append(problems, fmt.Sprintf("the directory is named %s but its manifest is generation %q; name the directory after the manifest's generation", id, m.Generation))
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
		case wrongPath(n):
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
		if _, ok := size("basemap.pmtiles"); !ok && !wrongPath("basemap.pmtiles") {
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
			if _, listed := m.Artifacts["valhalla/tiles.tar"]; !listed && !wrongPath("valhalla/tiles.tar") {
				problems = append(problems, "valhalla/tiles.tar is missing")
			}
		} else if n == 0 {
			problems = append(problems, "valhalla/tiles.tar is empty")
		}
		if _, ok := size("valhalla/valhalla.json"); !ok {
			if !wrongPath("valhalla/valhalla.json") {
				problems = append(problems, "valhalla/valhalla.json is missing")
			}
		} else if _, err := ReadValhallaConfig(genDir); err != nil {
			problems = append(problems, "valhalla/valhalla.json is not a routing config: "+err.Error())
		}
	}
	if len(problems) > 0 {
		return bad(problems...)
	}
	return m, nil
}

// AdoptOptions controls Adopt. What a generation must carry is Builder.Need.
type AdoptOptions struct {
	// Prepare runs after validation and before the swap, with the generation's directory. A
	// server brings its routing instance for the new graph up as a standby here, without
	// switching to it; an error leaves the live generation untouched. The Prepared it returns
	// (nil for nothing to do) is committed once current has moved, or aborted if that fails.
	Prepare func(ctx context.Context, genDir string) (Prepared, error)
	// Restart is called after the swap (as after a build) so services follow at once.
	Restart func(ctx context.Context) error
}

// Prepared is a service made ready for a generation that is not live yet. Commit makes it
// serve (and retires what served before); Abort discards it, leaving the service as it was.
type Prepared interface {
	Commit()
	Abort()
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
// atomic swap a build ends with, then applies the retention rule (retention.go): the live
// generation and the newest other complete one this data directory owns stay. Adopting is what
// makes a copied directory owned; other copies, arriving or waiting, are not touched.
// Adopting the live generation again is a no-op that succeeds. A failure at any step (the
// validation, Prepare, or moving current) leaves the live generation serving as before, with
// whatever Prepare started discarded. ErrBusy while a build runs.
func (b *Builder) Adopt(ctx context.Context, id string, opt AdoptOptions) (*AdoptResult, error) {
	if !ValidGenerationID(id) {
		return nil, fmt.Errorf("%w: %q is not a generation id", ErrNoGeneration, id)
	}
	if !b.mu.TryLock() {
		return nil, ErrBusy
	}
	defer b.mu.Unlock()
	// From here to the end nobody may delete this generation: it is about to be validated,
	// have its routing graph started, and become live. One that is being deleted is refused.
	if !b.reserveActivation(id) {
		return nil, ErrBusy
	}
	defer b.releaseActivation()
	genDir := filepath.Join(b.generationsDir(), id)
	fi, err := os.Lstat(genDir)
	if err != nil {
		return nil, fmt.Errorf("%w: there is no directory %s", ErrNoGeneration, genDir)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, &InvalidError{ID: id, Problems: []string{
			"generations/" + id + " is a symbolic link; a generation must be a real directory under generations/ (copy or move it there)"}}
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrNoGeneration, genDir)
	}
	cur := b.CurrentGeneration()
	if cur == id {
		m, _ := ReadManifest(genDir)
		_ = b.own(id)
		return &AdoptResult{ID: id, AlreadyLive: true, Manifest: m}, nil
	}
	m, err := Validate(genDir, id, b.need())
	if err != nil {
		return nil, err
	}
	res := &AdoptResult{ID: id, Manifest: m, Previous: cur}
	var prepared Prepared
	if opt.Prepare != nil {
		if prepared, err = opt.Prepare(ctx, genDir); err != nil {
			return nil, &PrepareError{Err: err}
		}
	}
	// The one step that changes what is live. Until it succeeds nothing has been switched:
	// the old routing instance is still the one serving, and the standby is thrown away.
	if err := b.swapCurrent(id); err != nil {
		if prepared != nil {
			prepared.Abort()
		}
		return nil, fmt.Errorf("could not make generation %s live (the live generation is unchanged): %w", id, err)
	}
	if prepared != nil {
		prepared.Commit()
	}
	if opt.Restart != nil {
		_ = opt.Restart(ctx) // the swap happened; services retry on their own
	}
	_ = b.own(id)
	res.Removed, _ = b.pruneGenerations("")
	return res, nil
}
