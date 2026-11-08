package build

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/catalog"
)

// The test binary doubles as every external tool: scripts in a temp dir re-exec it with
// MAPTHING_FAKE_TOOL set, and TestMain dispatches to a fake implementation.
func TestMain(m *testing.M) {
	if tool := os.Getenv("MAPTHING_FAKE_TOOL"); tool != "" {
		os.Exit(fakeTool(tool, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeTool(tool string, args []string) int {
	if os.Getenv("MAPTHING_FAKE_FAIL") == tool {
		fmt.Fprintln(os.Stderr, tool+": simulated failure")
		return 3
	}
	flag := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
			if v, ok := strings.CutPrefix(a, name+"="); ok {
				return v
			}
		}
		return ""
	}
	switch tool {
	case "osmium":
		os.WriteFile(flag("-o"), []byte("\x00\x00\x00\x0d\x0a\x09OSMHeader fake"), 0o644)
		fmt.Println("osmium " + args[0] + " ok")
	case "java":
		os.WriteFile(flag("--output"), testPMTiles(), 0o644)
		fmt.Println("planetiler: done")
	case "valhalla_build_config":
		cfg := map[string]any{
			"mjolnir": map[string]any{"tile_dir": flag("--mjolnir-tile-dir"), "tile_extract": flag("--mjolnir-tile-extract"), "concurrency": 2},
			"httpd":   map[string]any{"service": map[string]any{"listen": flag("--httpd-service-listen")}},
		}
		json.NewEncoder(os.Stdout).Encode(cfg)
	case "valhalla_build_admins", "valhalla_build_tiles":
		fmt.Fprintln(os.Stderr, tool+" ok")
	case "valhalla_build_extract":
		cfg := readCfg(flag("-c"))
		os.WriteFile(cfg.Mjolnir.TileExtract, []byte("tar"), 0o644)
	case "valhalla_build_timezones":
		fmt.Print("tzdb")
	case "valhalla_service":
		cfg := readCfg(args[0])
		addr := strings.TrimPrefix(cfg.HTTPD.Service.Listen, "tcp://")
		mux := http.NewServeMux()
		mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
		mux.HandleFunc("/route", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"trip":{"summary":{"length":12.3}}}`))
		})
		http.ListenAndServe(addr, mux)
	default:
		return 1
	}
	return 0
}

type fakeCfg struct {
	Mjolnir struct {
		TileExtract string `json:"tile_extract"`
	} `json:"mjolnir"`
	HTTPD struct {
		Service struct {
			Listen string `json:"listen"`
		} `json:"service"`
	} `json:"httpd"`
}

func readCfg(p string) fakeCfg {
	var c fakeCfg
	data, _ := os.ReadFile(p)
	json.Unmarshal(data, &c)
	return c
}

// testPMTiles is a valid v3 archive with one directory entry covering every tile id.
func testPMTiles() []byte {
	var dir bytes.Buffer
	put := func(v uint64) {
		b := make([]byte, binary.MaxVarintLen64)
		dir.Write(b[:binary.PutUvarint(b, v)])
	}
	put(1)         // entries
	put(0)         // tile id 0
	put(1<<32 - 1) // run length
	put(10)        // length
	put(1)         // offset 0 (+1)
	h := make([]byte, 127)
	copy(h, "PMTiles")
	h[7] = 3
	le := binary.LittleEndian
	le.PutUint64(h[8:], 127)
	le.PutUint64(h[16:], uint64(dir.Len()))
	le.PutUint64(h[56:], 127+uint64(dir.Len()))
	le.PutUint64(h[64:], 10)
	h[97], h[98], h[99], h[100], h[101] = 1, 2, 1, 0, 15
	out := append(h, dir.Bytes()...)
	return append(out, make([]byte, 10)...)
}

// env is a test data dir, fake tools and a fake Geofabrik.
type env struct {
	b        *Builder
	srv      *httptest.Server
	pbfGets  atomic.Int32
	mu       sync.Mutex
	pbf      []byte
	md5      string
	sequence int
	region   catalog.Region
}

func newEnv(t *testing.T) *env {
	t.Helper()
	resetHooks()
	t.Cleanup(resetHooks)
	e := &env{sequence: 100}
	e.setPBF([]byte("\x00\x00\x00\x0d\x0a\x09OSMHeader delaware v1"))
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		switch r.URL.Path {
		case "/de-latest.osm.pbf":
			e.pbfGets.Add(1)
			etag := fmt.Sprintf(`"%x"`, md5.Sum(e.pbf))
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			w.Write(e.pbf)
		case "/de-latest.osm.pbf.md5":
			fmt.Fprintf(w, "%s  de-latest.osm.pbf\n", e.md5)
		case "/de-updates/state.txt":
			fmt.Fprintf(w, "#x\nsequenceNumber=%d\ntimestamp=2026-11-07T20\\:21\\:02Z\n", e.sequence)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(e.srv.Close)

	bin := t.TempDir()
	exe, _ := os.Executable()
	tool := func(name string) string {
		p := filepath.Join(bin, name)
		os.WriteFile(p, []byte(fmt.Sprintf("#!/bin/sh\nMAPTHING_FAKE_TOOL=%s exec %q \"$@\"\n", name, exe)), 0o755)
		return p
	}
	tool("valhalla_build_timezones")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	e.b = &Builder{
		DataDir: t.TempDir(),
		HTTP:    e.srv.Client(),
		Tools: Tools{
			Java: tool("java"), PlanetilerJar: "/fake.jar", Osmium: tool("osmium"),
			ValhallaBuildConfig: tool("valhalla_build_config"), ValhallaBuildAdmins: tool("valhalla_build_admins"),
			ValhallaBuildTiles: tool("valhalla_build_tiles"), ValhallaBuildExtract: tool("valhalla_build_extract"),
			ValhallaService: tool("valhalla_service"),
		},
	}
	// Distinct generation ids per run.
	var tick atomic.Int64
	base := time.Date(2026, 11, 8, 3, 0, 0, 0, time.UTC)
	e.b.Now = func() time.Time { return base.Add(time.Duration(tick.Add(1)) * time.Minute) }
	e.region = catalog.Region{
		ID: "north-america/us/delaware", Name: "Delaware",
		BBox:       catalog.BBox{-75.79, 38.45, -75.05, 39.84},
		PBFURL:     e.srv.URL + "/de-latest.osm.pbf",
		UpdatesURL: e.srv.URL + "/de-updates",
		SizeHint:   1 << 20,
	}
	return e
}

func (e *env) setPBF(b []byte) {
	sum := md5.Sum(b)
	e.pbf, e.md5 = b, hex.EncodeToString(sum[:])
}

func resetHooks() {
	RegisterTerrain(nil)
	RegisterTerrainEstimate(nil)
	RegisterElevationCheck(nil)
}

type testRep struct {
	mu     sync.Mutex
	stages []string
	logs   []string
}

func (r *testRep) Stage(n string)           { r.mu.Lock(); r.stages = append(r.stages, n); r.mu.Unlock() }
func (r *testRep) Progress(float64, string) {}
func (r *testRep) Logf(f string, a ...any) {
	r.mu.Lock()
	r.logs = append(r.logs, fmt.Sprintf(f, a...))
	r.mu.Unlock()
}
func (r *testRep) has(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestRunSuccess(t *testing.T) {
	e := newEnv(t)
	var elevChecked atomic.Bool
	RegisterTerrain(func(ctx context.Context, r Reporter, regions []catalog.Region, workDir, out string) error {
		r.Logf("terrain for %d regions", len(regions))
		return os.WriteFile(out, testPMTiles(), 0o644)
	})
	RegisterElevationCheck(func(ctx context.Context, p string, pts [][2]float64) error {
		elevChecked.Store(true)
		return nil
	})
	restarted := false
	rep := &testRep{}
	m, err := e.b.Run(context.Background(), rep, Options{
		Regions: []catalog.Region{e.region},
		Restart: func(context.Context) error { restarted = true; return nil },
	})
	if err != nil {
		t.Fatalf("run: %v\nlogs:\n%s", err, strings.Join(rep.logs, "\n"))
	}
	want := []string{StageGuard, StageDownload, StageMerge, StageBasemap, StageValhalla, StageTerrain, StageVerify, StageSwap, StageRetention}
	if strings.Join(rep.stages, ",") != strings.Join(want, ",") {
		t.Errorf("stages %v", rep.stages)
	}
	if !m.OK || !restarted || !elevChecked.Load() {
		t.Errorf("ok=%v restarted=%v elev=%v", m.OK, restarted, elevChecked.Load())
	}
	if cur := e.b.CurrentGeneration(); cur != m.Generation {
		t.Errorf("current=%q want %q", cur, m.Generation)
	}
	disk, err := ReadManifest(filepath.Join(e.b.DataDir, "current"))
	if err != nil || disk.Regions[0].Sequence != 100 || disk.Regions[0].MD5 != e.md5 {
		t.Fatalf("manifest %+v err %v", disk, err)
	}
	for _, c := range disk.Checks {
		if !c.OK {
			t.Errorf("check failed: %+v", c)
		}
	}
	if len(disk.Checks) < 4 { // basemap, tile, route, elevation
		t.Errorf("checks %+v", disk.Checks)
	}
	for _, f := range []string{"basemap.pmtiles", "terrain.pmtiles", "valhalla/valhalla.json", "valhalla/tiles.tar"} {
		if _, err := os.Stat(filepath.Join(e.b.DataDir, "current", f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(e.b.DataDir, "work")); len(ents) != 0 {
		t.Errorf("work dir not cleaned: %v", ents)
	}
}

func TestRefreshSkipsUnchangedAndRetentionKeepsTwo(t *testing.T) {
	e := newEnv(t)
	opt := Options{Regions: []catalog.Region{e.region}, Refresh: true}
	var gens []string
	for i := 0; i < 3; i++ {
		if i == 2 {
			e.mu.Lock()
			e.sequence = 101
			e.setPBF([]byte("\x00\x00\x00\x0d\x0a\x09OSMHeader delaware v2"))
			e.mu.Unlock()
		}
		m, err := e.b.Run(context.Background(), &testRep{}, opt)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		gens = append(gens, m.Generation)
	}
	if got := e.pbfGets.Load(); got != 2 {
		t.Errorf("pbf GETs = %d, want 2 (initial + changed sequence)", got)
	}
	var ids []string
	for _, g := range e.b.Generations() {
		ids = append(ids, g.ID)
	}
	if len(ids) != 2 || ids[0] != gens[2] || ids[1] != gens[1] {
		t.Errorf("kept %v, want [%s %s]", ids, gens[2], gens[1])
	}
	meta, _ := readMeta(e.b.RegionDir(e.region.ID))
	if meta.Sequence != 101 || meta.SourceTimestamp != "2026-11-07T20:21:02Z" {
		t.Errorf("meta %+v", meta)
	}
}

func TestStageFailureKeepsLiveGeneration(t *testing.T) {
	e := newEnv(t)
	opt := Options{Regions: []catalog.Region{e.region}}
	good, err := e.b.Run(context.Background(), &testRep{}, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAPTHING_FAKE_FAIL", "valhalla_build_tiles")
	_, err = e.b.Run(context.Background(), &testRep{}, opt)
	var se *StageError
	if !errors.As(err, &se) || se.Stage != StageValhalla {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(strings.Join(se.LogTail, "\n"), "simulated failure") {
		t.Errorf("log tail lacks tool output: %v", se.LogTail)
	}
	if e.b.CurrentGeneration() != good.Generation {
		t.Errorf("current moved to %q", e.b.CurrentGeneration())
	}
	if gs := e.b.Generations(); len(gs) != 1 {
		t.Errorf("failed generation left behind: %+v", gs)
	}
}

func TestDownloadMD5MismatchKeepsPrevious(t *testing.T) {
	e := newEnv(t)
	opt := Options{Regions: []catalog.Region{e.region}, Refresh: true}
	if _, err := e.b.Run(context.Background(), &testRep{}, opt); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.sequence = 102
	e.pbf = []byte("\x00\x00\x00\x0d\x0a\x09OSMHeader corrupted") // md5 now stale
	e.mu.Unlock()
	_, err := e.b.Run(context.Background(), &testRep{}, opt)
	var se *StageError
	if !errors.As(err, &se) || se.Stage != StageDownload || !strings.Contains(err.Error(), "md5 mismatch") {
		t.Fatalf("err = %v", err)
	}
	data, _ := os.ReadFile(e.b.regionFile(e.region.ID))
	if !bytes.Contains(data, []byte("v1")) {
		t.Errorf("previous extract replaced: %q", data)
	}
}

func TestDiskGuardRefuses(t *testing.T) {
	e := newEnv(t)
	r := e.region
	r.SizeHint = 1 << 55
	_, err := e.b.Run(context.Background(), &testRep{}, Options{Regions: []catalog.Region{r}})
	var se *StageError
	if !errors.As(err, &se) || se.Stage != StageGuard || !strings.Contains(err.Error(), "not enough disk") {
		t.Fatalf("err = %v", err)
	}
	if e.pbfGets.Load() != 0 {
		t.Error("downloaded despite guard")
	}
}

func TestCustomRegionClipsFromParent(t *testing.T) {
	e := newEnv(t)
	custom := catalog.Region{
		ID: "custom/kent-county", Name: "Kent County", BBox: catalog.BBox{-75.75, 38.8, -75.3, 39.4},
		Custom: &catalog.Custom{Parent: e.region},
	}
	rep := &testRep{}
	m, err := e.b.Run(context.Background(), rep, Options{Regions: []catalog.Region{custom}})
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(rep.logs, "\n"))
	}
	if !rep.has("extract --overwrite -f pbf") || !rep.has("-b -75.75,38.8,-75.3,39.4") {
		t.Error("osmium extract not run with bbox")
	}
	if m.Regions[0].Source != e.region.PBFURL {
		t.Errorf("source %q", m.Regions[0].Source)
	}
	// A second build reuses the clip.
	rep2 := &testRep{}
	if _, err := e.b.Run(context.Background(), rep2, Options{Regions: []catalog.Region{custom}}); err != nil {
		t.Fatal(err)
	}
	if !rep2.has("clip up to date") {
		t.Error("clip redone")
	}
}

func TestMergeMultipleRegions(t *testing.T) {
	e := newEnv(t)
	r2 := e.region
	r2.ID, r2.Name = "north-america/us/delaware-copy", "Copy"
	rep := &testRep{}
	if _, err := e.b.Run(context.Background(), rep, Options{Regions: []catalog.Region{e.region, r2}}); err != nil {
		t.Fatal(err)
	}
	if !rep.has("osmium merge ok") {
		t.Error("merge not run")
	}
}

func TestBusy(t *testing.T) {
	b := &Builder{DataDir: t.TempDir()}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.Run(context.Background(), &testRep{}, Options{}); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
}

func TestZxyToID(t *testing.T) {
	tests := []struct {
		z    uint8
		x, y uint32
		id   uint64
	}{
		{0, 0, 0, 0}, {1, 0, 0, 1}, {1, 0, 1, 2}, {1, 1, 1, 3}, {1, 1, 0, 4},
		{2, 0, 0, 5}, {3, 0, 0, 21}, {20, 0, 0, 366503875925},
	}
	for _, tt := range tests {
		if got := zxyToID(tt.z, tt.x, tt.y); got != tt.id {
			t.Errorf("zxyToID(%d,%d,%d)=%d want %d", tt.z, tt.x, tt.y, got, tt.id)
		}
	}
	// Every tile at z3 maps to a distinct id in range.
	seen := map[uint64]bool{}
	for x := uint32(0); x < 8; x++ {
		for y := uint32(0); y < 8; y++ {
			id := zxyToID(3, x, y)
			if id < 21 || id >= 85 || seen[id] {
				t.Fatalf("bad id %d for 3/%d/%d", id, x, y)
			}
			seen[id] = true
		}
	}
}

func TestPlanetilerMemory(t *testing.T) {
	tests := []struct {
		avail, pbf int64
		heapMB     int64
		storage    string
	}{
		{8 << 30, 25 << 20, 4915, "ram"},
		{8 << 30, 6 << 30, 4915, "mmap"},
		{1 << 30, 1 << 20, 1024, "ram"},
		{128 << 30, 1 << 30, 32768, "ram"},
	}
	for _, tt := range tests {
		h, s := planetilerMemory(tt.avail, tt.pbf)
		if h != tt.heapMB || s != tt.storage {
			t.Errorf("planetilerMemory(%d,%d)=%d,%s want %d,%s", tt.avail, tt.pbf, h, s, tt.heapMB, tt.storage)
		}
	}
}

func TestDeepMerge(t *testing.T) {
	dst := map[string]any{"a": map[string]any{"b": 1.0, "c": 2.0}, "d": 3.0}
	deepMerge(dst, map[string]any{"a": map[string]any{"c": 9.0, "e": 5.0}, "f": "x"})
	got, _ := json.Marshal(dst)
	if string(got) != `{"a":{"b":1,"c":9,"e":5},"d":3,"f":"x"}` {
		t.Error(string(got))
	}
}
