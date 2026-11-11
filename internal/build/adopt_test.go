package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/catalog"
)

// writeGen fabricates a finished generation directory: what a build elsewhere would have
// written and an operator copied in.
func writeGen(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "valhalla"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"basemap.pmtiles":        testPMTiles(),
		"terrain.pmtiles":        testPMTiles(),
		"contours.pmtiles":       testPMTiles(),
		"valhalla/tiles.tar":     []byte("tar"),
		"valhalla/valhalla.json": []byte(`{"mjolnir":{"tile_extract":"/somewhere/else/tiles.tar"}}`),
	}
	created, err := time.Parse("20060102T150405Z", id)
	if err != nil {
		created = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	}
	m := &Manifest{Generation: id, OK: true, CreatedAt: created, Artifacts: map[string]int64{},
		Regions: []ManifestRegion{{ID: "north-america/us/delaware", Name: "Delaware", BBox: catalog.BBox{-75.79, 38.45, -75.05, 39.84}}}}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), body, 0o644); err != nil {
			t.Fatal(err)
		}
		if name != "valhalla/valhalla.json" {
			m.Artifacts[name] = int64(len(body))
		}
	}
	if err := writeManifest(dir, m); err != nil {
		t.Fatal(err)
	}
}

func newAdoptBuilder(t *testing.T) *Builder {
	t.Helper()
	b := &Builder{DataDir: t.TempDir()}
	if err := os.MkdirAll(b.generationsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *Builder) addGen(t *testing.T, id string) string {
	t.Helper()
	dir := filepath.Join(b.generationsDir(), id)
	writeGen(t, dir, id)
	return dir
}

// fakePrepared records what an adoption did with the service it prepared.
type fakePrepared struct{ log *[]string }

func (p fakePrepared) Commit() { *p.log = append(*p.log, "commit") }
func (p fakePrepared) Abort()  { *p.log = append(*p.log, "abort") }

func problems(t *testing.T, err error) string {
	t.Helper()
	var inv *InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("want *InvalidError, got %v", err)
	}
	return strings.Join(inv.Problems, " | ")
}

func TestValidateGeneration(t *testing.T) {
	const id = "20261101T000000Z"
	fresh := func() string {
		dir := filepath.Join(t.TempDir(), id)
		writeGen(t, dir, id)
		return dir
	}
	if m, err := Validate(fresh(), id, AllParts()); err != nil || m.Generation != id {
		t.Fatalf("complete generation: %v", err)
	}
	if _, err := Validate(filepath.Join(t.TempDir(), "nothing"), "", AllParts()); !strings.Contains(problems(t, err), "manifest.json is missing") {
		t.Errorf("missing manifest: %v", err)
	}

	cases := []struct {
		name   string
		damage func(dir string)
		id     string
		want   string
	}{
		{"a file not copied yet", func(d string) { _ = os.Remove(filepath.Join(d, "valhalla", "tiles.tar")) }, id, "valhalla/tiles.tar is missing"},
		{"a partial file", func(d string) { _ = os.WriteFile(filepath.Join(d, "terrain.pmtiles"), []byte("half"), 0o644) }, id, "terrain.pmtiles is 4 bytes"},
		{"no routing config", func(d string) { _ = os.Remove(filepath.Join(d, "valhalla", "valhalla.json")) }, id, "valhalla.json is missing"},
		{"a failed build", func(d string) {
			m, _ := ReadManifest(d)
			m.OK = false
			_ = writeManifest(d, m)
		}, id, "did not finish"},
		{"a renamed directory", func(string) {}, "20270101T000000Z", "name the directory after the manifest's generation"},
		{"a corrupt archive of the right size", func(d string) {
			n := len(testPMTiles())
			_ = os.WriteFile(filepath.Join(d, "basemap.pmtiles"), make([]byte, n), 0o644)
		}, id, "basemap.pmtiles does not open"},
		{"an artifact outside the directory", func(d string) {
			m, _ := ReadManifest(d)
			m.Artifacts["../../etc/passwd"] = 1
			_ = writeManifest(d, m)
		}, id, "not a path inside the generation"},
	}
	for _, c := range cases {
		dir := fresh()
		c.damage(dir)
		_, err := Validate(dir, c.id, AllParts())
		if got := problems(t, err); !strings.Contains(got, c.want) {
			t.Errorf("%s: problems %q, want %q", c.name, got, c.want)
		}
	}

	// A routing and elevation backend needs no basemap or contours...
	dir := fresh()
	_ = os.Remove(filepath.Join(dir, "basemap.pmtiles"))
	_ = os.Remove(filepath.Join(dir, "contours.pmtiles"))
	slim := Parts{Routing: true, Elevation: true}
	if _, err := Validate(dir, id, slim); err != nil {
		t.Errorf("slim backend without basemap: %v", err)
	}
	if got := problems(t, func() error { _, err := Validate(dir, id, AllParts()); return err }()); !strings.Contains(got, "basemap.pmtiles is missing") {
		t.Errorf("full server without basemap: %q", got)
	}
	// ...but still needs what it serves, and a basemap that is there must be whole.
	_ = os.Remove(filepath.Join(dir, "terrain.pmtiles"))
	if got := problems(t, func() error { _, err := Validate(dir, id, slim); return err }()); !strings.Contains(got, "terrain.pmtiles is missing") {
		t.Errorf("slim backend without terrain: %q", got)
	}
	dir = fresh()
	_ = os.WriteFile(filepath.Join(dir, "basemap.pmtiles"), []byte("half"), 0o644)
	if got := problems(t, func() error { _, err := Validate(dir, id, slim); return err }()); !strings.Contains(got, "basemap.pmtiles is 4 bytes") {
		t.Errorf("slim backend with a partial basemap: %q", got)
	}
}

func TestAdoptSwapsAndIsIdempotent(t *testing.T) {
	b := newAdoptBuilder(t)
	const g1, g2 = "20261101T000000Z", "20261102T000000Z"
	b.addGen(t, g1)
	ctx := context.Background()
	res, err := b.Adopt(ctx, g1, AdoptOptions{})
	if err != nil || res.AlreadyLive || res.Previous != "" || b.CurrentGeneration() != g1 {
		t.Fatalf("first adoption: %+v %v, current %q", res, err, b.CurrentGeneration())
	}

	b.addGen(t, g2)
	var order []string
	opt := AdoptOptions{
		Prepare: func(_ context.Context, genDir string) (Prepared, error) {
			// The routing graph is tried before anything is swapped.
			order = append(order, "prepare:"+filepath.Base(genDir)+":live="+b.CurrentGeneration())
			return fakePrepared{&order}, nil
		},
		Restart: func(context.Context) error {
			order = append(order, "restart:live="+b.CurrentGeneration())
			return nil
		},
	}
	res, err = b.Adopt(ctx, g2, opt)
	if err != nil || res.AlreadyLive || res.Previous != g1 || b.CurrentGeneration() != g2 {
		t.Fatalf("adopt g2: %+v %v, current %q", res, err, b.CurrentGeneration())
	}
	// Routing is committed only once current has moved, and never aborted on the way.
	if got := strings.Join(order, " "); got != "prepare:"+g2+":live="+g1+" commit restart:live="+g2 {
		t.Errorf("order %q", got)
	}
	if target, _ := os.Readlink(b.currentLink()); target != filepath.Join("generations", g2) {
		t.Errorf("current -> %q, want a relative link like a build's", target)
	}

	// Again: nothing happens, and it still succeeds.
	order = nil
	res, err = b.Adopt(ctx, g2, opt)
	if err != nil || !res.AlreadyLive || len(order) != 0 || b.CurrentGeneration() != g2 {
		t.Fatalf("second adoption: %+v %v order %v", res, err, order)
	}
	if res.Manifest == nil || res.Manifest.Generation != g2 {
		t.Errorf("manifest of the live generation: %+v", res.Manifest)
	}
}

func TestAdoptRefusalsLeaveLiveGeneration(t *testing.T) {
	b := newAdoptBuilder(t)
	const live, next = "20261101T000000Z", "20261102T000000Z"
	b.addGen(t, live)
	ctx := context.Background()
	if _, err := b.Adopt(ctx, live, AdoptOptions{}); err != nil {
		t.Fatal(err)
	}
	restarted := false
	opt := AdoptOptions{Restart: func(context.Context) error { restarted = true; return nil }}
	stillLive := func(what string) {
		t.Helper()
		if cur := b.CurrentGeneration(); cur != live || restarted {
			t.Fatalf("%s: current %q restarted %v; the live generation must be untouched", what, cur, restarted)
		}
	}

	for _, id := range []string{"20270101T000000Z", "../" + live, ".hidden", "a/b", ""} {
		if _, err := b.Adopt(ctx, id, opt); !errors.Is(err, ErrNoGeneration) {
			t.Errorf("adopt %q: %v, want ErrNoGeneration", id, err)
		}
	}
	stillLive("unknown ids")

	// A copy that has not finished: the manifest arrived, the routing graph has not.
	dir := b.addGen(t, next)
	_ = os.Remove(filepath.Join(dir, "valhalla", "tiles.tar"))
	_, err := b.Adopt(ctx, next, opt)
	if got := problems(t, err); !strings.Contains(got, "valhalla/tiles.tar is missing") {
		t.Errorf("incomplete: %q", got)
	}
	if !strings.Contains(err.Error(), "generation "+next+" is incomplete or invalid") {
		t.Errorf("message %q", err)
	}
	stillLive("incomplete generation")
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("a refused generation must stay on disk for the copy to finish: %v", err)
	}

	// Complete, but the routing graph will not load.
	_ = os.WriteFile(filepath.Join(dir, "valhalla", "tiles.tar"), []byte("tar"), 0o644)
	opt.Prepare = func(context.Context, string) (Prepared, error) {
		return nil, errors.New("valhalla: new instance not ready")
	}
	var prep *PrepareError
	if _, err := b.Adopt(ctx, next, opt); !errors.As(err, &prep) {
		t.Errorf("prepare failure: %v", err)
	}
	stillLive("routing graph that does not load")

	// The graph loads and is ready, and then current cannot be moved (a full or read-only
	// volume). The standby must be discarded, never committed: routing stays on the old graph
	// with everything else.
	var did []string
	opt.Prepare = func(context.Context, string) (Prepared, error) {
		did = append(did, "prepare")
		return fakePrepared{&did}, nil
	}
	b.symlink = func(string, string) error { return errors.New("no space left on device") }
	_, err = b.Adopt(ctx, next, opt)
	if err == nil || !strings.Contains(err.Error(), "no space left on device") || !strings.Contains(err.Error(), "live generation is unchanged") {
		t.Errorf("swap failure: %v", err)
	}
	if got := strings.Join(did, " "); got != "prepare abort" {
		t.Errorf("after a failed swap the prepared service saw %q, want prepare then abort", got)
	}
	stillLive("current that cannot be moved")
	if rec := b.loadRetention(); len(rec.Generations) != 1 || rec.Generations[0] != live {
		t.Errorf("a generation that never went live is recorded as owned: %v", rec.Generations)
	}
	b.symlink = nil

	// A build is running.
	opt.Prepare = nil
	b.mu.Lock()
	_, err = b.Adopt(ctx, next, opt)
	b.mu.Unlock()
	if !errors.Is(err, ErrBusy) {
		t.Errorf("during a build: %v", err)
	}
	stillLive("busy")

	if _, err := b.Adopt(ctx, next, opt); err != nil || b.CurrentGeneration() != next || !restarted {
		t.Fatalf("adopt after the copy finished: %v", err)
	}
}

func TestAdoptRetention(t *testing.T) {
	b := newAdoptBuilder(t)
	ctx := context.Background()
	// A copy that was already waiting, complete, when this data directory saw its first
	// adoption. Older than everything: owned by accident, it would be the first one deleted.
	early := "20260901T000000Z"
	b.addGen(t, early)
	ids := []string{"20261101T000000Z", "20261102T000000Z", "20261103T000000Z", "20261104T000000Z"}
	for _, id := range ids[:3] {
		b.addGen(t, id)
		if _, err := b.Adopt(ctx, id, AdoptOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(b.generationsDir(), name))
		return err == nil
	}
	// Two stay, as after a build: the live one and the newest other good one.
	if exists(ids[0]) || !exists(ids[1]) || !exists(ids[2]) {
		t.Fatalf("after three adoptions: %v %v %v", exists(ids[0]), exists(ids[1]), exists(ids[2]))
	}

	// Copies from another machine, in every state, all newer than what is live: one with no
	// manifest yet, one whose manifest has arrived but whose files have not (it says ok, and is
	// not), one staged under a dot name, and one that is complete and waiting to be activated.
	// None is deleted, and none takes the place of the usable previous generation.
	arriving := "20261105T000000Z"
	if err := os.MkdirAll(filepath.Join(b.generationsDir(), arriving, "valhalla"), 0o755); err != nil {
		t.Fatal(err)
	}
	half := "20261107T000000Z"
	_ = os.Remove(filepath.Join(b.addGen(t, half), "terrain.pmtiles"))
	staged := ".20261106T000000Z.partial"
	writeGen(t, filepath.Join(b.generationsDir(), staged), "20261106T000000Z")
	waiting := "20261108T000000Z"
	b.addGen(t, waiting)
	// ...and a complete one older than everything, which retention would otherwise delete.
	oldCopy := "20261001T000000Z"
	b.addGen(t, oldCopy)

	b.addGen(t, ids[3])
	res, err := b.Adopt(ctx, ids[3], AdoptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != ids[1] {
		t.Errorf("removed %v, want only %s", res.Removed, ids[1])
	}
	for name, want := range map[string]bool{ids[3]: true, ids[2]: true, ids[1]: false,
		arriving: true, half: true, staged: true, waiting: true, oldCopy: true} {
		if exists(name) != want {
			t.Errorf("%s exists=%v, want %v", name, exists(name), want)
		}
	}
	for _, g := range b.Generations() {
		if strings.HasPrefix(g.ID, ".") {
			t.Errorf("a dot-prefixed directory is listed as generation %s", g.ID)
		}
	}

	// Going back to an older generation never deletes the one it is adopting, or the live one.
	res, err = b.Adopt(ctx, ids[2], AdoptOptions{})
	if err != nil || b.CurrentGeneration() != ids[2] || !exists(ids[2]) || !exists(ids[3]) || len(res.Removed) != 0 {
		t.Errorf("roll back: %+v %v", res, err)
	}

	// Activating the waiting copy is the handoff: from then on it is retained like any other,
	// and the oldest owned generation makes room for it.
	res, err = b.Adopt(ctx, waiting, AdoptOptions{})
	if err != nil || len(res.Removed) != 1 || res.Removed[0] != ids[2] {
		t.Errorf("adopt the waiting copy: %+v %v", res, err)
	}
	if !exists(early) {
		t.Errorf("a copy waiting since before the first adoption was deleted")
	}
	if !exists(waiting) || !exists(ids[3]) || exists(ids[2]) || !exists(oldCopy) || !exists(half) || !exists(arriving) {
		t.Errorf("after the handoff: waiting %v previous %v older %v", exists(waiting), exists(ids[3]), exists(ids[2]))
	}
}

// The brief allows a generation to be copied straight into generations/<id> while the server
// runs. A build that finishes during the copy must not delete it, whatever has arrived so
// far, and must not let it push out the generation kept for rollback.
func TestBuildRetentionLeavesIncomingCopies(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	opt := Options{Regions: []catalog.Region{e.region}}
	run := func() string {
		t.Helper()
		rep := &testRep{}
		m, err := e.b.Run(ctx, rep, opt)
		if err != nil {
			t.Fatalf("build: %v\n%s", err, strings.Join(rep.logs, "\n"))
		}
		return m.Generation
	}
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(e.b.generationsDir(), name))
		return err == nil
	}
	first, second := run(), run()

	// rsync at work while the next build runs. Build ids here are 20261108T03…; the copies are
	// older and newer than all of them.
	copies := map[string]func(dir, id string){
		"no manifest yet": func(dir, _ string) {
			if err := os.MkdirAll(filepath.Join(dir, "valhalla"), 0o755); err != nil {
				t.Fatal(err)
			}
			_ = os.WriteFile(filepath.Join(dir, "basemap.pmtiles"), []byte("half"), 0o644)
		},
		"manifest first, files later": func(dir, id string) {
			writeGen(t, dir, id)
			_ = os.Remove(filepath.Join(dir, "valhalla", "tiles.tar"))
		},
		"complete, not activated": func(dir, id string) { writeGen(t, dir, id) },
	}
	var incoming []string
	n := 0
	for what, make := range copies {
		for _, day := range []string{"202610", "202701"} {
			n++
			id := fmt.Sprintf("%s%02dT000000Z", day, n)
			make(filepath.Join(e.b.generationsDir(), id), id)
			incoming = append(incoming, id+" ("+what+")")
		}
	}
	third := run()
	for _, c := range incoming {
		if id, _, _ := strings.Cut(c, " "); !exists(id) {
			t.Errorf("a build deleted the incoming copy %s", c)
		}
	}
	// The build's own generations follow the usual rule, undisturbed by the copies.
	if exists(first) || !exists(second) || !exists(third) || e.b.CurrentGeneration() != third {
		t.Errorf("own generations: first %v second %v third %v, current %q", exists(first), exists(second), exists(third), e.b.CurrentGeneration())
	}

	// A build that was killed leaves its directory behind; the next build knows it is its own.
	killed := "20261108T020000Z"
	if err := os.MkdirAll(filepath.Join(e.b.generationsDir(), killed), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(e.b.generationsDir(), killed, "basemap.pmtiles"), []byte("half"), 0o644)
	if err := e.b.updateRetention(func(r *retentionRecord) { r.Building = append(r.Building, killed) }); err != nil {
		t.Fatal(err)
	}
	fourth := run()
	if exists(killed) {
		t.Errorf("the leftover of a killed build was not cleaned up")
	}
	if exists(second) || !exists(third) || !exists(fourth) {
		t.Errorf("after the fourth build: second %v third %v fourth %v", exists(second), exists(third), exists(fourth))
	}
	for _, c := range incoming {
		if id, _, _ := strings.Cut(c, " "); !exists(id) {
			t.Errorf("a later build deleted the incoming copy %s", c)
		}
	}
	if rec := e.b.loadRetention(); len(rec.Building) != 0 || strings.Join(rec.Generations, ",") != third+","+fourth {
		t.Errorf("retention record after builds: %+v", rec)
	}
}

// A data directory from before the retention record: its complete generations are the
// server's own, so the first build after an upgrade still prunes them and nothing leaks.
func TestRetentionRecordSeededOnUpgrade(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	opt := Options{Regions: []catalog.Region{e.region}}
	var gens []string
	for range 2 {
		m, err := e.b.Run(ctx, &testRep{}, opt)
		if err != nil {
			t.Fatal(err)
		}
		gens = append(gens, m.Generation)
	}
	if err := os.Remove(e.b.retentionPath()); err != nil {
		t.Fatal(err)
	}
	m, err := e.b.Run(ctx, &testRep{}, opt)
	if err != nil {
		t.Fatal(err)
	}
	left := e.b.Generations()
	if len(left) != 2 || left[0].ID != m.Generation || left[1].ID != gens[1] {
		t.Fatalf("after the first build with a new record: %+v", left)
	}
	// A record that does not read owns nothing, and so deletes nothing.
	if err := os.WriteFile(e.b.retentionPath(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if removed, _ := e.b.pruneGenerations(""); len(removed) != 0 || len(e.b.Generations()) != 2 {
		t.Errorf("a corrupt record led to deletions: %v", removed)
	}
}

// A generation is a real directory holding its own files. Links are refused at adoption, so a
// name can never resolve outside generations/, into a copy still being staged, or onto
// another generation that retention sees under a different name.
func TestAdoptRefusesSymlinks(t *testing.T) {
	b := newAdoptBuilder(t)
	ctx := context.Background()
	const live = "20261101T000000Z"
	b.addGen(t, live)
	if _, err := b.Adopt(ctx, live, AdoptOptions{}); err != nil {
		t.Fatal(err)
	}
	link := func(target, name string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(b.generationsDir(), name)); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(id, want string) {
		t.Helper()
		_, err := b.Adopt(ctx, id, AdoptOptions{})
		if got := problems(t, err); !strings.Contains(got, want) {
			t.Errorf("adopt %s: %q, want %q", id, got, want)
		}
		if b.CurrentGeneration() != live {
			t.Fatalf("adopt %s changed the live generation to %q", id, b.CurrentGeneration())
		}
	}

	// The directory entry is a link: to outside the data directory...
	const outsideID = "20261102T000000Z"
	outside := filepath.Join(t.TempDir(), outsideID)
	writeGen(t, outside, outsideID)
	link(outside, outsideID)
	refused(outsideID, "is a symbolic link")
	// ...into a copy staged under a dot name...
	const stagedID = "20261103T000000Z"
	writeGen(t, filepath.Join(b.generationsDir(), "."+stagedID+".partial"), stagedID)
	link("."+stagedID+".partial", stagedID)
	refused(stagedID, "is a symbolic link")
	// ...and an alias: A is a link to the real directory B, whose manifest says it is A.
	const aliasA, realB = "20261105T000000Z", "20261104T000000Z"
	writeGen(t, filepath.Join(b.generationsDir(), realB), aliasA)
	link(realB, aliasA)
	refused(aliasA, "is a symbolic link")
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("the link target outside the data directory was touched: %v", err)
	}

	// Links inside a real directory: an artifact, a directory on an artifact's path, the
	// routing config, the manifest. Each would make the generation depend on something else.
	elsewhere := t.TempDir()
	writeGen(t, elsewhere, "x")
	cases := map[string]struct {
		rel  string
		want string
	}{
		"20261111T000000Z": {"basemap.pmtiles", "basemap.pmtiles is a symbolic link"},
		"20261112T000000Z": {"terrain.pmtiles", "terrain.pmtiles is a symbolic link"},
		"20261113T000000Z": {"valhalla", "valhalla is a symbolic link"},
		"20261114T000000Z": {"valhalla/valhalla.json", "valhalla/valhalla.json is a symbolic link"},
		"20261115T000000Z": {"valhalla/tiles.tar", "valhalla/tiles.tar is a symbolic link"},
		"20261116T000000Z": {"manifest.json", "manifest.json is a symbolic link"},
	}
	for id, c := range cases {
		dir := b.addGen(t, id)
		p := filepath.Join(dir, filepath.FromSlash(c.rel))
		target := filepath.Join(elsewhere, filepath.FromSlash(c.rel))
		if c.rel == "manifest.json" {
			target = filepath.Join(dir, "manifest.real.json")
			if err := os.Rename(p, target); err != nil {
				t.Fatal(err)
			}
		} else if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		refused(id, c.want)
	}
}

// Even when a data directory already holds an alias (made by hand, or by an older version),
// retention must not delete the directory the live generation resolves to.
func TestRetentionProtectsTheResolvedLiveTarget(t *testing.T) {
	b := newAdoptBuilder(t)
	const aliasA, realB = "20261103T000000Z", "20261101T000000Z"
	writeGen(t, filepath.Join(b.generationsDir(), realB), aliasA)
	if err := os.Symlink(realB, filepath.Join(b.generationsDir(), aliasA)); err != nil {
		t.Fatal(err)
	}
	if err := atomicSymlink(filepath.Join("generations", aliasA), b.currentLink()); err != nil {
		t.Fatal(err)
	}
	// B is owned, complete, not named by current, and older than two other owned generations:
	// by name alone it is the one to delete.
	newer := []string{"20261104T000000Z", "20261105T000000Z"}
	for _, id := range newer {
		b.addGen(t, id)
	}
	if err := b.saveRetention(&retentionRecord{Generations: append([]string{realB}, newer...)}); err != nil {
		t.Fatal(err)
	}
	// Make B validate under its own name, as a candidate would.
	m, _ := ReadManifest(filepath.Join(b.generationsDir(), realB))
	m.Generation = realB
	_ = writeManifest(filepath.Join(b.generationsDir(), realB), m)

	removed, err := b.pruneGenerations("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.generationsDir(), realB, "basemap.pmtiles")); err != nil {
		t.Fatalf("retention deleted the directory the live generation resolves to (removed %v)", removed)
	}
	if len(removed) != 1 || removed[0] != newer[0] {
		t.Errorf("removed %v, want only %s", removed, newer[0])
	}
}

// valhalla.json must be the object the routing service is configured from. null, numbers and
// arrays are valid JSON too, and used to get through.
func TestValidateRoutingConfig(t *testing.T) {
	const id = "20261101T000000Z"
	for body, ok := range map[string]bool{
		`{"mjolnir":{"tile_extract":"/x/tiles.tar"}}`:                          true,
		`{"mjolnir":{},"httpd":{"service":{"listen":"tcp://127.0.0.1:8002"}}}`: true,
		`null`:                                 false,
		`3`:                                    false,
		`"valhalla"`:                           false,
		`[]`:                                   false,
		`[{"mjolnir":{}}]`:                     false,
		`{}`:                                   false,
		`{"mjolnir":null}`:                     false,
		`{"mjolnir":"tiles"}`:                  false,
		`{"mjolnir":{},"httpd":null}`:          false,
		`{"mjolnir":{},"httpd":[1]}`:           false,
		`{"mjolnir":{},"httpd":{"service":7}}`: false,
		`{"mjolnir":`:                          false,
	} {
		dir := filepath.Join(t.TempDir(), id)
		writeGen(t, dir, id)
		if err := os.WriteFile(filepath.Join(dir, "valhalla", "valhalla.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Validate(dir, id, AllParts())
		if ok && err != nil {
			t.Errorf("%s: refused: %v", body, err)
		}
		if !ok {
			if err == nil {
				t.Errorf("%s: accepted as a routing config", body)
			} else if got := problems(t, err); !strings.Contains(got, "valhalla/valhalla.json is not a routing config") {
				t.Errorf("%s: problems %q", body, got)
			}
			// A server that serves no routing does not care.
			if _, err := Validate(dir, id, Parts{Tiles: true, Elevation: true}); err != nil {
				t.Errorf("%s: refused by a server without routing: %v", body, err)
			}
		}
	}
}

// A build with OutDir writes a whole generation somewhere else and leaves this data
// directory's live generation alone; that directory is then adoptable as it is.
func TestOutDirBuildsWithoutSwapping(t *testing.T) {
	e := newEnv(t)
	RegisterTerrain(func(ctx context.Context, r Reporter, regions []catalog.Region, workDir, out string) error {
		return os.WriteFile(out, testPMTiles(), 0o644)
	})
	RegisterContours(func(ctx context.Context, r Reporter, terrainPath, out string) error {
		return os.WriteFile(out, testPMTiles(), 0o644)
	})
	ctx := context.Background()
	live, err := e.b.Run(ctx, &testRep{}, Options{Regions: []catalog.Region{e.region}})
	if err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "next")
	restarted := false
	rep := &testRep{}
	m, err := e.b.Run(ctx, rep, Options{Regions: []catalog.Region{e.region}, OutDir: out,
		Restart: func(context.Context) error { restarted = true; return nil }})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, strings.Join(rep.logs, "\n"))
	}
	if !m.OK || restarted || e.b.CurrentGeneration() != live.Generation {
		t.Errorf("ok=%v restarted=%v current=%q; -out must not touch the live generation", m.OK, restarted, e.b.CurrentGeneration())
	}
	if gens := e.b.Generations(); len(gens) != 1 || gens[0].ID != live.Generation {
		t.Errorf("generations in the data dir: %+v", gens)
	}
	for _, f := range []string{"manifest.json", "basemap.pmtiles", "terrain.pmtiles", "contours.pmtiles", "valhalla/valhalla.json", "valhalla/tiles.tar"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("missing %s in the output directory", f)
		}
	}
	for _, st := range m.Stages {
		if st.Name == StageRetention && !st.Skipped {
			t.Errorf("retention ran for an -out build")
		}
	}
	if ents, _ := os.ReadDir(e.b.workRoot()); len(ents) != 0 {
		t.Errorf("work dir not cleaned: %v", ents)
	}
	if _, err := Validate(out, m.Generation, AllParts()); err != nil {
		t.Fatalf("the output is not a valid generation: %v", err)
	}

	// Copied into a server's data directory under its id, it adopts.
	if err := os.Rename(out, filepath.Join(e.b.generationsDir(), m.Generation)); err != nil {
		t.Fatal(err)
	}
	if res, err := e.b.Adopt(ctx, m.Generation, AdoptOptions{}); err != nil || res.Previous != live.Generation {
		t.Fatalf("adopt the -out build: %+v %v", res, err)
	}

	// A directory with something in it is refused before anything runs, and is left alone.
	used := t.TempDir()
	keep := filepath.Join(used, "keep.txt")
	_ = os.WriteFile(keep, []byte("mine"), 0o644)
	_, err = e.b.Run(ctx, &testRep{}, Options{Regions: []catalog.Region{e.region}, OutDir: used})
	var se *StageError
	if !errors.As(err, &se) || se.Stage != StageGuard || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("non-empty out dir: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the refused output directory was modified")
	}
}

func TestReuseTerrain(t *testing.T) {
	e := newEnv(t)
	var terrainBuilds, contourBuilds atomic.Int32
	fingerprint := func(regions []catalog.Region) string {
		ids := make([]string, len(regions))
		for i, r := range regions {
			ids[i] = r.ID
		}
		return "fp:" + strings.Join(ids, "+") + ":" + os.Getenv("MAPTHING_TERRAIN_MAXZOOM")
	}
	RegisterTerrainFingerprint(fingerprint)
	RegisterTerrain(func(ctx context.Context, r Reporter, regions []catalog.Region, workDir, out string) error {
		terrainBuilds.Add(1)
		side, _ := json.Marshal(TerrainResult{Fingerprint: fingerprint(regions)})
		if err := os.WriteFile(out+".json", side, 0o644); err != nil {
			return err
		}
		return os.WriteFile(out, testPMTiles(), 0o644)
	})
	RegisterContours(func(ctx context.Context, r Reporter, terrainPath, out string) error {
		contourBuilds.Add(1)
		return os.WriteFile(out, testPMTiles(), 0o644)
	})
	elevChecks := 0
	RegisterElevationCheck(func(context.Context, string, [][2]float64) error { elevChecks++; return nil })

	ctx := context.Background()
	base := t.TempDir()
	build := func(name string, opt Options) (*Manifest, *testRep) {
		t.Helper()
		opt.Regions, opt.OutDir = []catalog.Region{e.region}, filepath.Join(base, name)
		rep := &testRep{}
		m, err := e.b.Run(ctx, rep, opt)
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, strings.Join(rep.logs, "\n"))
		}
		return m, rep
	}
	same := func(a, b string) bool {
		fa, err1 := os.Stat(a)
		fb, err2 := os.Stat(b)
		return err1 == nil && err2 == nil && os.SameFile(fa, fb)
	}
	first := filepath.Join(base, "first")
	build("first", Options{})
	if terrainBuilds.Load() != 1 || contourBuilds.Load() != 1 {
		t.Fatalf("first build: terrain %d contours %d", terrainBuilds.Load(), contourBuilds.Load())
	}

	// Same areas, same settings: nothing is rebuilt, and the manifest still lists the files.
	m, rep := build("second", Options{ReuseTerrain: first})
	if terrainBuilds.Load() != 1 || contourBuilds.Load() != 1 {
		t.Errorf("reuse rebuilt: terrain %d contours %d\n%s", terrainBuilds.Load(), contourBuilds.Load(), strings.Join(rep.logs, "\n"))
	}
	second := filepath.Join(base, "second")
	for _, f := range []string{"terrain.pmtiles", "contours.pmtiles"} {
		if !same(filepath.Join(first, f), filepath.Join(second, f)) {
			t.Errorf("%s was not hard-linked from the previous generation", f)
		}
		if m.Artifacts[f] == 0 {
			t.Errorf("%s missing from the manifest of a reusing build: %v", f, m.Artifacts)
		}
	}
	if !rep.has("terrain: reused from") || elevChecks != 2 {
		t.Errorf("reuse not logged or the reused terrain not verified (checks %d)", elevChecks)
	}
	if _, err := Validate(second, m.Generation, AllParts()); err != nil {
		t.Errorf("reusing build is not a valid generation: %v", err)
	}
	// A third build can reuse the second: the fingerprint travels with the archive.
	build("third", Options{ReuseTerrain: second})
	if terrainBuilds.Load() != 1 || contourBuilds.Load() != 1 {
		t.Errorf("reuse of a reused generation rebuilt: terrain %d contours %d", terrainBuilds.Load(), contourBuilds.Load())
	}

	// A different contour setting: the terrain is reused, contours are rebuilt from it.
	t.Setenv(envContoursMaxZoom, "11")
	build("contours", Options{ReuseTerrain: first})
	if terrainBuilds.Load() != 1 || contourBuilds.Load() != 2 {
		t.Errorf("contour setting changed: terrain %d contours %d", terrainBuilds.Load(), contourBuilds.Load())
	}
	if same(filepath.Join(first, "contours.pmtiles"), filepath.Join(base, "contours", "contours.pmtiles")) {
		t.Errorf("contours with another setting were reused")
	}
	t.Setenv(envContoursMaxZoom, "")

	// Different terrain settings: built again, and the build says why.
	t.Setenv("MAPTHING_TERRAIN_MAXZOOM", "11")
	_, rep = build("settings", Options{ReuseTerrain: first})
	if terrainBuilds.Load() != 2 || !rep.has("built for other areas or settings") {
		t.Errorf("settings changed: terrain builds %d\n%s", terrainBuilds.Load(), strings.Join(rep.logs, "\n"))
	}
	t.Setenv("MAPTHING_TERRAIN_MAXZOOM", "")

	// Nothing usable there (no terrain, or one from before fingerprints): built again.
	_, rep = build("empty", Options{ReuseTerrain: t.TempDir()})
	if terrainBuilds.Load() != 3 || !rep.has("no terrain.pmtiles there") {
		t.Errorf("empty reuse dir: terrain builds %d", terrainBuilds.Load())
	}
	old := t.TempDir()
	_ = os.WriteFile(filepath.Join(old, "terrain.pmtiles"), testPMTiles(), 0o644)
	_, rep = build("old", Options{ReuseTerrain: old})
	if terrainBuilds.Load() != 4 || !rep.has("no recorded fingerprint") {
		t.Errorf("unfingerprinted reuse dir: terrain builds %d", terrainBuilds.Load())
	}
}

// A build on a server (in place, no OutDir) offered the live generation's terrain, as the
// build job does: the second build links the terrain instead of rebuilding it, the disk guard
// stops counting it, and both generations stay valid when retention later removes one.
func TestInPlaceBuildReusesLiveTerrain(t *testing.T) {
	e := newEnv(t)
	var terrainBuilds atomic.Int32
	fingerprint := func(regions []catalog.Region) string { return "fp:" + regions[0].ID }
	RegisterTerrainFingerprint(fingerprint)
	RegisterTerrainEstimate(func([]catalog.Region) int64 { return 7 << 30 })
	RegisterTerrain(func(_ context.Context, _ Reporter, regions []catalog.Region, _, out string) error {
		terrainBuilds.Add(1)
		side, _ := json.Marshal(TerrainResult{Fingerprint: fingerprint(regions)})
		if err := os.WriteFile(out+".json", side, 0o644); err != nil {
			return err
		}
		return os.WriteFile(out, testPMTiles(), 0o644)
	})
	ctx := context.Background()
	regions := []catalog.Region{e.region}
	run := func(opt Options) (*Manifest, *testRep) {
		t.Helper()
		opt.Regions = regions
		rep := &testRep{}
		m, err := e.b.Run(ctx, rep, opt)
		if err != nil {
			t.Fatalf("build: %v\n%s", err, strings.Join(rep.logs, "\n"))
		}
		return m, rep
	}
	liveDir := func() string {
		id := e.b.CurrentGeneration()
		if id == "" {
			return ""
		}
		return filepath.Join(e.b.generationsDir(), id)
	}

	// No generation yet: the guard counts the terrain, and it is built.
	if est := e.b.EstimateFor(Options{Regions: regions, ReuseTerrain: liveDir()}); est.TerrainBytes != 7<<30 {
		t.Fatalf("first estimate counts terrain %d, want the estimator's", est.TerrainBytes)
	}
	first, _ := run(Options{ReuseTerrain: liveDir()})
	if terrainBuilds.Load() != 1 || e.b.CurrentGeneration() != first.Generation {
		t.Fatalf("first build: terrain builds %d, live %q", terrainBuilds.Load(), e.b.CurrentGeneration())
	}

	// The refresh build: same regions, the live generation's terrain on offer.
	if est := e.b.EstimateFor(Options{Regions: regions, ReuseTerrain: liveDir()}); est.TerrainBytes != 0 {
		t.Errorf("the guard still counts %d bytes of terrain that will be reused", est.TerrainBytes)
	}
	second, rep := run(Options{Refresh: true, ReuseTerrain: liveDir()})
	if terrainBuilds.Load() != 1 {
		t.Errorf("the refresh build rebuilt the terrain\n%s", strings.Join(rep.logs, "\n"))
	}
	if !rep.has("terrain: reused from") || e.b.CurrentGeneration() != second.Generation {
		t.Errorf("reuse not logged, or the second build is not live (%q)", e.b.CurrentGeneration())
	}
	a, err1 := os.Stat(filepath.Join(e.b.generationsDir(), first.Generation, "terrain.pmtiles"))
	b, err2 := os.Stat(filepath.Join(e.b.generationsDir(), second.Generation, "terrain.pmtiles"))
	if err1 != nil || err2 != nil || !os.SameFile(a, b) {
		t.Errorf("terrain is not shared between the two generations (%v, %v)", err1, err2)
	}
	_, disk := e.b.Inventory()
	gens, _ := e.b.Inventory()
	var sum int64
	for _, g := range gens {
		sum += g.Bytes
	}
	if sum-disk < a.Size() {
		t.Errorf("inventory: sum %d, on disk %d; the shared terrain (%d) should count once", sum, disk, a.Size())
	}

	// A third build removes the first generation; the second keeps its (shared) terrain.
	third, _ := run(Options{Refresh: true, ReuseTerrain: liveDir()})
	if terrainBuilds.Load() != 1 {
		t.Errorf("third build rebuilt the terrain")
	}
	if _, err := os.Stat(filepath.Join(e.b.generationsDir(), first.Generation)); !os.IsNotExist(err) {
		t.Errorf("retention kept the first generation")
	}
	for _, id := range []string{second.Generation, third.Generation} {
		if _, err := Validate(filepath.Join(e.b.generationsDir(), id), id, AllParts()); err != nil {
			t.Errorf("generation %s after retention: %v", id, err)
		}
	}

	// Other regions: not reusable, so it is built and counted again.
	other := []catalog.Region{{ID: "elsewhere", Name: "Elsewhere", BBox: e.region.BBox}}
	if est := e.b.EstimateFor(Options{Regions: other, ReuseTerrain: liveDir()}); est.TerrainBytes == 0 {
		t.Errorf("the guard counts no terrain for regions the live terrain does not cover")
	}
}

func TestTerrainFingerprint(t *testing.T) {
	region := func(id string, b catalog.BBox) catalog.Region { return catalog.Region{ID: id, BBox: b} }
	a := []catalog.Region{region("x", catalog.BBox{-75.8, 38.4, -75.0, 39.9})}
	fp := TerrainFingerprint(a)
	if len(fp) != 64 || fp != TerrainFingerprint(a) {
		t.Fatalf("fingerprint %q is not stable", fp)
	}
	if fp == TerrainFingerprint([]catalog.Region{region("x", catalog.BBox{-75.8, 38.4, -75.0, 39.8})}) {
		t.Errorf("a different box has the same fingerprint")
	}
	if fp == TerrainFingerprint([]catalog.Region{region("y", catalog.BBox{-75.8, 38.4, -75.0, 39.9})}) {
		t.Errorf("a different region id has the same fingerprint (ids are in the archive's metadata)")
	}
	poly := a[0]
	poly.Polygon = [][][2]float64{{{-75.8, 38.4}, {-75.0, 38.4}, {-75.4, 39.9}}}
	if fp == TerrainFingerprint([]catalog.Region{poly}) {
		t.Errorf("an outline does not change the fingerprint")
	}
	for _, kv := range [][2]string{{envQuantum, "1"}, {envMaxZoom, "9"}} {
		t.Setenv(kv[0], kv[1])
		if fp == TerrainFingerprint(a) {
			t.Errorf("%s=%s does not change the fingerprint", kv[0], kv[1])
		}
		t.Setenv(kv[0], "")
	}
	// Settings that do not decide the content do not change it.
	t.Setenv(envWorkers, "7")
	t.Setenv(envCacheDir, t.TempDir())
	if fp != TerrainFingerprint(a) {
		t.Errorf("worker count or cache directory changed the fingerprint")
	}
}
