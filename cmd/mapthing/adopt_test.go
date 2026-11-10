package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/build"
	"github.com/jclement/mapthing/internal/config"
	"github.com/jclement/mapthing/internal/pmtiles"
	"github.com/jclement/mapthing/internal/routing"
	"github.com/jclement/mapthing/internal/server"
	"github.com/jclement/mapthing/internal/supervisor"
	"github.com/jclement/mapthing/internal/tiles"
)

// The test binary doubles as valhalla_service: with FAKE_VALHALLA set it serves the config it
// is given, and refuses a graph whose tar says "broken" (as Valhalla exits on a bad extract).
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_VALHALLA") != "" {
		fakeValhalla(os.Args[1])
		return
	}
	os.Exit(m.Run())
}

func fakeValhalla(configPath string) {
	var cfg struct {
		Mjolnir struct {
			TileExtract string `json:"tile_extract"`
		} `json:"mjolnir"`
		HTTPD struct {
			Service struct {
				Listen string `json:"listen"`
			} `json:"service"`
		} `json:"httpd"`
	}
	b, _ := os.ReadFile(configPath)
	_ = json.Unmarshal(b, &cfg)
	graph, err := os.ReadFile(cfg.Mjolnir.TileExtract)
	if err != nil || string(graph) == "broken" {
		fmt.Fprintln(os.Stderr, "fake valhalla: cannot load graph", cfg.Mjolnir.TileExtract, err)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
	mux.HandleFunc("/route", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"trip":{}}`) })
	mux.HandleFunc("/graph", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, cfg.Mjolnir.TileExtract) })
	srv := &http.Server{Addr: strings.TrimPrefix(cfg.HTTPD.Service.Listen, "tcp://"), Handler: mux, ReadHeaderTimeout: time.Second}
	_ = srv.ListenAndServe()
	os.Exit(1)
}

func testArchive(t *testing.T) []byte {
	t.Helper()
	w, err := pmtiles.NewWriter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add(pmtiles.ZxyToID(0, 0, 0), []byte("tile")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	h := pmtiles.Header{TileType: pmtiles.TypeMVT, TileCompression: pmtiles.CompressionGzip, MaxZoom: 15}
	if err := w.Finish(&buf, h, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeGeneration fabricates a generation as a build on another machine wrote it: its
// valhalla.json names that machine's paths. parts lists the files to include.
func writeGeneration(t *testing.T, dir, id, graph string, parts ...string) {
	t.Helper()
	if len(parts) == 0 {
		parts = []string{"basemap.pmtiles", "terrain.pmtiles", "valhalla/tiles.tar"}
	}
	if err := os.MkdirAll(filepath.Join(dir, "valhalla"), 0o755); err != nil {
		t.Fatal(err)
	}
	archive := testArchive(t)
	arts := map[string]int64{}
	for _, p := range parts {
		body := archive
		if p == "valhalla/tiles.tar" {
			body = []byte(graph)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(p)), body, 0o644); err != nil {
			t.Fatal(err)
		}
		arts[p] = int64(len(body))
	}
	vcfg := `{"mjolnir":{"tile_extract":"/build-machine/out/valhalla/tiles.tar","tile_dir":"/build-machine/work"},"httpd":{"service":{"listen":"tcp://127.0.0.1:8002"}}}`
	if err := os.WriteFile(filepath.Join(dir, "valhalla", "valhalla.json"), []byte(vcfg), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := json.Marshal(map[string]any{
		"generation": id, "ok": true, "created_at": "2026-11-01T00:00:00Z", "artifacts": arts,
		"regions": []map[string]any{{"id": "us/delaware", "name": "Delaware", "bbox": []float64{-75.8, 38.4, -75.0, 39.9}}},
	})
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), m, 0o644); err != nil {
		t.Fatal(err)
	}
}

func current(dataDir string) string {
	id, _, _ := tiles.CurrentGeneration(dataDir)
	return id
}

func activateErr(t *testing.T, err error) *server.ActivateError {
	t.Helper()
	var ae *server.ActivateError
	if !errors.As(err, &ae) {
		t.Fatalf("want *server.ActivateError, got %v", err)
	}
	return ae
}

// A generation's valhalla.json carries the paths of wherever it was built. The instance
// config must point the service at the graph where the generation is now.
func TestValhallaInstanceConfigFollowsTheGeneration(t *testing.T) {
	data := t.TempDir()
	gen := filepath.Join(data, "generations", "g1")
	writeGeneration(t, gen, "g1", "tar")
	p, err := valhallaInstanceConfig(data, gen, 18002)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Mjolnir map[string]any `json:"mjolnir"`
		HTTPD   struct {
			Service map[string]string `json:"service"`
		} `json:"httpd"`
	}
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Mjolnir["tile_extract"]; got != filepath.Join(gen, "valhalla", "tiles.tar") {
		t.Errorf("tile_extract %v, want the copied generation's tar", got)
	}
	if cfg.HTTPD.Service["listen"] != "tcp://127.0.0.1:18002" {
		t.Errorf("listen %q", cfg.HTTPD.Service["listen"])
	}
}

// End to end with a supervised (fake) valhalla_service: the routing graph of an adopted
// generation is running and switched to before the generation goes live, and one whose graph
// does not load is refused with the old generation, and the old graph, still serving.
func TestActivateGenerationBlueGreen(t *testing.T) {
	if testing.Short() {
		t.Skip("starts child processes")
	}
	t.Setenv("FAKE_VALHALLA", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	gens := filepath.Join(data, "generations")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sup := supervisor.New("")
	sup.Start(ctx)
	var mu sync.Mutex
	var swaps []string
	f := &features{
		cfg:     config.Config{DataDir: data, ValhallaCmd: []string{exe, "{config}"}, Serve: config.Serve{Tiles: true, Routing: true, Elevation: true}, RendererOff: true},
		sup:     sup,
		builder: build.New(data),
	}
	f.watcher = &tiles.Watcher{DataDir: data, OnSwap: func(ctx context.Context, _, newID string) {
		mu.Lock()
		swaps = append(swaps, newID)
		mu.Unlock()
		f.swap(ctx, newID)
	}}
	router := routing.New("http://127.0.0.1:1")
	f.valhalla = f.newValhalla(router)
	f.valhalla.Ports = freePorts(t)
	f.valhalla.ReadyTimeout = 20 * time.Second
	var base string
	f.valhalla.Switch = func(u string) { mu.Lock(); base = u; mu.Unlock() }
	graph := func() string {
		mu.Lock()
		u := base
		mu.Unlock()
		resp, err := http.Get(u + "/graph")
		if err != nil {
			return "error: " + err.Error()
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	if _, err := f.activateGeneration(ctx, "g0"); activateErr(t, err).Status != http.StatusNotFound || activateErr(t, err).Code != "no_generation" {
		t.Fatalf("unknown generation: %v", err)
	}

	writeGeneration(t, filepath.Join(gens, "g1"), "g1", "graph one")
	res, err := f.activateGeneration(ctx, "g1")
	if err != nil || res.ID != "g1" || res.AlreadyLive || res.Previous != nil {
		t.Fatalf("activate g1: %+v %v", res, err)
	}
	if current(data) != "g1" || !strings.HasSuffix(graph(), filepath.Join("g1", "valhalla", "tiles.tar")) {
		t.Fatalf("after g1: current %q, routing serves %q", current(data), graph())
	}

	// Idempotent.
	res, err = f.activateGeneration(ctx, "g1")
	if err != nil || !res.AlreadyLive {
		t.Fatalf("activate g1 again: %+v %v", res, err)
	}

	// Incomplete copy: refused with what is missing.
	writeGeneration(t, filepath.Join(gens, "g2"), "g2", "graph two")
	tar := filepath.Join(gens, "g2", "valhalla", "tiles.tar")
	_ = os.Remove(tar)
	ae := activateErr(t, func() error { _, err := f.activateGeneration(ctx, "g2"); return err }())
	if ae.Status != http.StatusUnprocessableEntity || ae.Code != "invalid_generation" || len(ae.Problems) != 1 || !strings.Contains(ae.Problems[0], "valhalla/tiles.tar is missing") {
		t.Fatalf("incomplete g2: %+v", ae)
	}

	// Complete files, but a graph the routing service cannot load.
	f.valhalla.ReadyTimeout = 3 * time.Second
	_ = os.WriteFile(tar, []byte("broken"), 0o644)
	m, _ := os.ReadFile(filepath.Join(gens, "g2", "manifest.json"))
	_ = os.WriteFile(filepath.Join(gens, "g2", "manifest.json"), bytes.Replace(m, []byte(`"valhalla/tiles.tar":9`), []byte(`"valhalla/tiles.tar":6`), 1), 0o644)
	ae = activateErr(t, func() error { _, err := f.activateGeneration(ctx, "g2"); return err }())
	if ae.Status != http.StatusUnprocessableEntity || ae.Code != "routing_failed" || !strings.Contains(ae.Message, "live generation is unchanged") {
		t.Fatalf("broken graph g2: %+v", ae)
	}
	if current(data) != "g1" || !strings.HasSuffix(graph(), filepath.Join("g1", "valhalla", "tiles.tar")) {
		t.Fatalf("after refusals: current %q, routing serves %q; g1 must still be serving", current(data), graph())
	}
	mu.Lock()
	if strings.Join(swaps, ",") != "g1" {
		t.Errorf("swap hooks fired: %v", swaps)
	}
	mu.Unlock()

	// The copy is fixed, its graph loads and routes, and then current cannot be moved: the
	// data directory takes no new entries (a read-only or full volume). The standby must be
	// thrown away. Before the fix routing had already switched to g2 and g1's graph was being
	// stopped, while tiles stayed on g1.
	f.valhalla.ReadyTimeout = 20 * time.Second
	_ = os.WriteFile(tar, []byte("graph two"), 0o644)
	_ = os.WriteFile(filepath.Join(gens, "g2", "manifest.json"), m, 0o644)
	if os.Geteuid() != 0 { // root ignores directory permissions
		oldBase := func() string { mu.Lock(); defer mu.Unlock(); return base }()
		if err := os.Chmod(data, 0o555); err != nil {
			t.Fatal(err)
		}
		_, err := f.activateGeneration(ctx, "g2")
		_ = os.Chmod(data, 0o755)
		if err == nil || !strings.Contains(err.Error(), "live generation is unchanged") {
			t.Fatalf("activation with an unwritable data directory: %v", err)
		}
		if current(data) != "g1" {
			t.Fatalf("current %q after a failed swap", current(data))
		}
		if now := func() string { mu.Lock(); defer mu.Unlock(); return base }(); now != oldBase {
			t.Fatalf("routing was switched to %s although the swap failed", now)
		}
		if _, gen := f.valhalla.Active(); filepath.Base(gen) != "g1" {
			t.Fatalf("active routing instance serves %q after a failed swap", gen)
		}
		// The old instance was not retired: it still answers after the stop timeout would
		// have started, and the standby is gone.
		time.Sleep(300 * time.Millisecond)
		if g := graph(); !strings.HasSuffix(g, filepath.Join("g1", "valhalla", "tiles.tar")) {
			t.Fatalf("after a failed swap routing serves %q; g1's graph must still be running", g)
		}
		if sts := sup.Statuses(); len(sts) != 1 || sts[0].Standby || sts[0].State != supervisor.StateRunning {
			t.Fatalf("routing instances after a failed swap: %+v", sts)
		}
		mu.Lock()
		if strings.Join(swaps, ",") != "g1" {
			t.Errorf("swap hooks fired on a failed swap: %v", swaps)
		}
		mu.Unlock()
	}

	// With the volume writable again it goes live: current first, then routing.
	res, err = f.activateGeneration(ctx, "g2")
	if err != nil || res.Previous == nil || *res.Previous != "g1" {
		t.Fatalf("activate g2: %+v %v", res, err)
	}
	if current(data) != "g2" || !strings.HasSuffix(graph(), filepath.Join("g2", "valhalla", "tiles.tar")) {
		t.Fatalf("after g2: current %q, routing serves %q", current(data), graph())
	}
	if _, gen := f.valhalla.Active(); filepath.Base(gen) != "g2" {
		t.Errorf("active routing instance serves %q", gen)
	}
}

func freePorts(t *testing.T) [2]int {
	t.Helper()
	var out [2]int
	for i := range out {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		out[i] = l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
	}
	return out
}

// A backend that serves only routing and elevation adopts a generation without a basemap; a
// full server refuses the same directory.
func TestActivateGenerationSlimBackend(t *testing.T) {
	data := t.TempDir()
	writeGeneration(t, filepath.Join(data, "generations", "g1"), "g1", "tar", "terrain.pmtiles", "valhalla/tiles.tar")
	f := &features{cfg: config.Config{DataDir: data, Serve: config.Serve{Tiles: true, Routing: true, Elevation: true}}, builder: build.New(data),
		watcher: &tiles.Watcher{DataDir: data}}
	ae := activateErr(t, func() error { _, err := f.activateGeneration(t.Context(), "g1"); return err }())
	if ae.Code != "invalid_generation" || !strings.Contains(strings.Join(ae.Problems, ";"), "basemap.pmtiles is missing") {
		t.Fatalf("full server: %+v", ae)
	}
	if current(data) != "" {
		t.Fatalf("a refused generation went live")
	}
	f.cfg.Serve = config.Serve{Routing: true, Elevation: true}
	f.builder.Need = needParts(f.cfg.Serve) // as wirePipeline sets it
	if res, err := f.activateGeneration(t.Context(), "g1"); err != nil || res.ID != "g1" {
		t.Fatalf("slim backend: %+v %v", res, err)
	}
	if current(data) != "g1" {
		t.Fatalf("current %q", current(data))
	}
}

func TestAdoptCommand(t *testing.T) {
	data := t.TempDir()
	t.Setenv("MAPTHING_SERVE", "")
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := adopt(append([]string{"-data", data}, args...), &out)
		return out.String(), err
	}

	// A directory from a build machine, named anything: moved in under its manifest's id.
	incoming := filepath.Join(t.TempDir(), "next")
	writeGeneration(t, incoming, "20261101T000000Z", "tar")
	out, err := run(incoming)
	if err != nil || !strings.Contains(out, "generation 20261101T000000Z is live") {
		t.Fatalf("adopt dir: %q %v", out, err)
	}
	if current(data) != "20261101T000000Z" {
		t.Fatalf("current %q", current(data))
	}
	if _, err := os.Stat(incoming); !os.IsNotExist(err) {
		t.Errorf("the directory was not moved into the data directory")
	}
	if _, err := os.Stat(filepath.Join(data, "generations", "20261101T000000Z", "valhalla", "tiles.tar")); err != nil {
		t.Errorf("moved generation is incomplete: %v", err)
	}

	// By id, for a directory already copied into place; and again, which changes nothing.
	writeGeneration(t, filepath.Join(data, "generations", "20261102T000000Z"), "20261102T000000Z", "tar")
	if out, err = run("20261102T000000Z"); err != nil || !strings.Contains(out, "is live (was 20261101T000000Z)") {
		t.Fatalf("adopt id: %q %v", out, err)
	}
	if out, err = run("20261102T000000Z"); err != nil || !strings.Contains(out, "already live") {
		t.Fatalf("adopt id again: %q %v", out, err)
	}
	if out, err = run(filepath.Join(data, "generations", "20261102T000000Z")); err != nil || !strings.Contains(out, "already live") {
		t.Fatalf("adopt the live directory by path: %q %v", out, err)
	}

	// An incomplete directory is refused where it is, and nothing changes.
	bad := filepath.Join(t.TempDir(), "bad")
	writeGeneration(t, bad, "20261103T000000Z", "tar")
	_ = os.Remove(filepath.Join(bad, "basemap.pmtiles"))
	if _, err = run(bad); err == nil || !strings.Contains(err.Error(), "basemap.pmtiles is missing") {
		t.Fatalf("incomplete dir: %v", err)
	}
	if _, err := os.Stat(bad); err != nil || current(data) != "20261102T000000Z" {
		t.Fatalf("a refused directory was moved, or the live generation changed (%q)", current(data))
	}
	// ...unless this is a backend that serves no tiles.
	t.Setenv("MAPTHING_SERVE", "routing,elevation")
	if out, err = run(bad); err != nil || current(data) != "20261103T000000Z" {
		t.Fatalf("slim adopt: %q %v", out, err)
	}

	if _, err = run("nope"); !errors.Is(err, build.ErrNoGeneration) {
		t.Errorf("unknown id: %v", err)
	}
	if _, err = run(); err == nil {
		t.Errorf("no argument accepted")
	}
}

// A generation whose valhalla.json is valid JSON but not a config object used to panic here
// (assignment to a nil map), in the server's watcher goroutine when adopted from the shell.
func TestValhallaInstanceConfigRefusesNonObjects(t *testing.T) {
	data := t.TempDir()
	gen := filepath.Join(data, "generations", "g1")
	writeGeneration(t, gen, "g1", "tar")
	for _, body := range []string{`null`, `3`, `"x"`, `[]`, `[{"mjolnir":{}}]`, `{}`, `{"mjolnir":null}`, `{"mjolnir":{},"httpd":null}`, `{"mjolnir":{},"httpd":{"service":[]}}`, `{`} {
		if err := os.WriteFile(filepath.Join(gen, "valhalla", "valhalla.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := valhallaInstanceConfig(data, gen, 18002)
		if err == nil || p != "" || !strings.Contains(err.Error(), "no usable routing config") {
			t.Errorf("%s: path %q err %v", body, p, err)
		}
		// And adoption refuses it before it gets that far.
		f := &features{cfg: config.Config{DataDir: data}, builder: build.New(data), watcher: &tiles.Watcher{DataDir: data}}
		ae := activateErr(t, func() error { _, err := f.activateGeneration(t.Context(), "g1"); return err }())
		if ae.Code != "invalid_generation" || !strings.Contains(strings.Join(ae.Problems, ";"), "valhalla/valhalla.json is not a routing config") {
			t.Errorf("%s: activation answered %+v", body, ae)
		}
	}
	if current(data) != "" {
		t.Errorf("a generation with a bad routing config went live")
	}
}

// The command moves the directory it is given, not a link to it, and refuses a link sitting
// under generations/.
func TestAdoptCommandSymlinks(t *testing.T) {
	data := t.TempDir()
	t.Setenv("MAPTHING_SERVE", "")
	run := func(arg string) (string, error) {
		var out bytes.Buffer
		err := adopt([]string{"-data", data, arg}, &out)
		return out.String(), err
	}
	realDir := filepath.Join(t.TempDir(), "real")
	writeGeneration(t, realDir, "20261101T000000Z", "tar")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	if out, err := run(link); err != nil || current(data) != "20261101T000000Z" {
		t.Fatalf("adopt through a link: %q %v", out, err)
	}
	if fi, err := os.Lstat(filepath.Join(data, "generations", "20261101T000000Z")); err != nil || !fi.IsDir() {
		t.Fatalf("generations/<id> is not a real directory: %v", err)
	}

	outside := filepath.Join(t.TempDir(), "20261102T000000Z")
	writeGeneration(t, outside, "20261102T000000Z", "tar")
	if err := os.Symlink(outside, filepath.Join(data, "generations", "20261102T000000Z")); err != nil {
		t.Fatal(err)
	}
	if _, err := run("20261102T000000Z"); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("adopt a linked generation by id: %v", err)
	}
	if current(data) != "20261101T000000Z" {
		t.Fatalf("current %q", current(data))
	}
}

func TestCopyTree(t *testing.T) {
	src := filepath.Join(t.TempDir(), "g")
	writeGeneration(t, src, "g", "tar")
	dst := filepath.Join(t.TempDir(), "copy")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := build.Validate(dst, "g", build.AllParts()); err != nil {
		t.Fatalf("copy is not a valid generation: %v", err)
	}
}
