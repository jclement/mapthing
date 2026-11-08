package build

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/catalog"
	"github.com/jclement/mapthing/internal/pmtiles"
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
		_ = os.WriteFile(flag("-o"), []byte("\x00\x00\x00\x0d\x0a\x09OSMHeader fake"), 0o644)
		fmt.Println("osmium " + args[0] + " ok")
	case "java":
		_ = os.WriteFile(flag("--output"), testPMTiles(), 0o644)
		fmt.Println("planetiler: done")
	case "valhalla_build_config":
		cfg := map[string]any{
			"mjolnir": map[string]any{"tile_dir": flag("--mjolnir-tile-dir"), "tile_extract": flag("--mjolnir-tile-extract"), "concurrency": 2},
			"httpd":   map[string]any{"service": map[string]any{"listen": flag("--httpd-service-listen")}},
		}
		_ = json.NewEncoder(os.Stdout).Encode(cfg)
	case "valhalla_build_admins", "valhalla_build_tiles":
		fmt.Fprintln(os.Stderr, tool+" ok")
	case "valhalla_build_extract":
		cfg := readCfg(flag("-c"))
		_ = os.WriteFile(cfg.Mjolnir.TileExtract, []byte("tar"), 0o644)
	case "ogr2ogr":
		// ... <dst.shp> <src> <layer>
		dst := args[len(args)-3]
		for _, ext := range []string{".shp", ".shx", ".dbf"} {
			_ = os.WriteFile(strings.TrimSuffix(dst, ".shp")+ext, []byte("x"), 0o644)
		}
	case "valhalla_build_timezones":
		fmt.Print("tzdb")
	case "valhalla_service":
		cfg := readCfg(args[0])
		addr := strings.TrimPrefix(cfg.HTTPD.Service.Listen, "tcp://")
		mux := http.NewServeMux()
		mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })
		mux.HandleFunc("/route", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"trip":{"summary":{"length":12.3}}}`))
		})
		_ = http.ListenAndServe(addr, mux)
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
	_ = json.Unmarshal(data, &c)
	return c
}

// testPMTiles is a valid archive (core's writer) with z12 tiles over the test regions.
func testPMTiles() []byte {
	w, err := pmtiles.NewWriter("")
	if err != nil {
		panic(err)
	}
	x0, y0, x1, y1 := pmtiles.TileRange(pmtiles.BBox{-77, 38, -74, 40.5}, 12)
	var ids []uint64
	for x := x0; x <= x1; x++ {
		for y := y0; y <= y1; y++ {
			ids = append(ids, pmtiles.ZxyToID(12, x, y))
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if err := w.Add(id, []byte("tile")); err != nil {
			panic(err)
		}
	}
	var buf bytes.Buffer
	h := pmtiles.Header{TileType: pmtiles.TypeMVT, TileCompression: pmtiles.CompressionGzip, MinZoom: 0, MaxZoom: 15}
	if err := w.Finish(&buf, h, []byte(`{}`)); err != nil {
		panic(err)
	}
	return buf.Bytes()
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
			_, _ = w.Write(e.pbf)
		case "/de-latest.osm.pbf.md5":
			_, _ = fmt.Fprintf(w, "%s  de-latest.osm.pbf\n", e.md5)
		case "/de-updates/state.txt":
			_, _ = fmt.Fprintf(w, "#x\nsequenceNumber=%d\ntimestamp=2026-11-07T20\\:21\\:02Z\n", e.sequence)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(e.srv.Close)

	bin := t.TempDir()
	exe, _ := os.Executable()
	tool := func(name string) string {
		p := filepath.Join(bin, name)
		_ = os.WriteFile(p, []byte(fmt.Sprintf("#!/bin/sh\nMAPTHING_FAKE_TOOL=%s exec %q \"$@\"\n", name, exe)), 0o755)
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
			ValhallaService: tool("valhalla_service"), Ogr2ogr: tool("ogr2ogr"),
		},
	}
	// Planetiler's auxiliary sources are already cached.
	_ = os.MkdirAll(e.b.sourcesDir(), 0o755)
	for _, src := range planetilerSources {
		_ = os.WriteFile(filepath.Join(e.b.sourcesDir(), src.file), []byte("cached"), 0o644)
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
	if disk.Artifacts["terrain.pmtiles"] == 0 || disk.TotalS <= 0 {
		t.Errorf("manifest artifacts %v total_s %v", disk.Artifacts, disk.TotalS)
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
		Custom: &catalog.Custom{Parents: []catalog.Region{e.region}},
	}
	rep := &testRep{}
	m, err := e.b.Run(context.Background(), rep, Options{Regions: []catalog.Region{custom}})
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(rep.logs, "\n"))
	}
	if !rep.has("extract --overwrite -o") || !rep.has("-b -75.75,38.8,-75.3,39.4") {
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

func TestPlanetilerMemory(t *testing.T) {
	tests := []struct {
		avail, pbf int64
		heapMB     int64
		storage    string
	}{
		{8 << 30, 25 << 20, 2198, "ram"},
		{8 << 30, 6 << 30, 4915, "mmap"},
		{1 << 30, 1 << 20, 1024, "ram"},
		{128 << 30, 1 << 30, 8192, "ram"},
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

func TestSourcesDownloadRetryAndOceanFallback(t *testing.T) {
	e := newEnv(t)
	old := sourceBackoff
	sourceBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { sourceBackoff = old })
	_ = os.RemoveAll(e.b.sourcesDir())

	var flaky atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/qrank":
			if flaky.Add(1) < 3 { // fails twice, then works
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("qrank"))
		case "/water", "/land":
			http.Error(w, "down", http.StatusBadGateway)
		default:
			_, _ = w.Write([]byte("data"))
		}
	}))
	defer srv.Close()
	for _, src := range planetilerSources {
		u := srv.URL + "/other"
		switch src.file {
		case "qrank.csv.gz":
			u = srv.URL + "/qrank"
		case "water-polygons-split-3857.zip":
			u = srv.URL + "/water"
		case "land-polygons-split-3857.zip":
			u = srv.URL + "/land"
		}
		t.Setenv(src.env, u)
	}
	rep := &testRep{}
	m, err := e.b.Run(context.Background(), rep, Options{Regions: []catalog.Region{e.region}})
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(rep.logs, "\n"))
	}
	if m.Ocean != OceanNaturalEarth {
		t.Errorf("ocean = %q", m.Ocean)
	}
	if flaky.Load() != 3 {
		t.Errorf("qrank attempts = %d", flaky.Load())
	}
	if !rep.has("--osm_water_path=") {
		t.Error("planetiler not pointed at fallback water polygons")
	}
	if _, err := os.Stat(filepath.Join(e.b.sourcesDir(), "water-polygons-split-3857.zip")); err == nil {
		t.Error("fallback written into the shared cache")
	}

	// Natural Earth fallback failing too: placeholders, ocean missing.
	t.Setenv("MAPTHING_FAKE_FAIL", "ogr2ogr")
	m, err = e.b.Run(context.Background(), &testRep{}, Options{Regions: []catalog.Region{e.region}})
	if err != nil || m.Ocean != OceanMissing {
		t.Fatalf("err=%v ocean=%q", err, m.Ocean)
	}

	// A required source that never downloads fails the build with the env var to set.
	_ = os.Remove(filepath.Join(e.b.sourcesDir(), "qrank.csv.gz"))
	t.Setenv("MAPTHING_QRANK_URL", srv.URL+"/water")
	_, err = e.b.Run(context.Background(), &testRep{}, Options{Regions: []catalog.Region{e.region}})
	if err == nil || !strings.Contains(err.Error(), "MAPTHING_QRANK_URL") {
		t.Fatalf("err = %v", err)
	}
}

func TestTinyShapefile(t *testing.T) {
	base := filepath.Join(t.TempDir(), "w")
	if err := writeTinyShapefile(base); err != nil {
		t.Fatal(err)
	}
	for ext, size := range map[string]int64{".shp": 236, ".shx": 108, ".dbf": 68} {
		st, err := os.Stat(base + ext)
		if err != nil || st.Size() != size {
			t.Errorf("%s size %v err %v", ext, st, err)
		}
	}
}

func TestOceanRetryBudgetAndFailedHost(t *testing.T) {
	e := newEnv(t)
	oldB, oldBudget := sourceBackoff, fallbackRetryBudget
	sourceBackoff = []time.Duration{50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond}
	fallbackRetryBudget = 80 * time.Millisecond
	t.Cleanup(func() { sourceBackoff, fallbackRetryBudget = oldB, oldBudget })

	var water atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		water.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	t.Setenv("MAPTHING_WATER_POLYGONS_URL", srv.URL+"/water")
	t.Setenv("MAPTHING_LAND_POLYGONS_URL", srv.URL+"/land")
	for _, f := range []string{"water-polygons-split-3857.zip", "land-polygons-split-3857.zip"} {
		_ = os.Remove(filepath.Join(e.b.sourcesDir(), f))
	}
	rep := &testRep{}
	m, err := e.b.Run(context.Background(), rep, Options{Regions: []catalog.Region{e.region}})
	if err != nil || m.Ocean != OceanNaturalEarth {
		t.Fatalf("err=%v ocean=%q", err, m.Ocean)
	}
	// Water: attempt at 0 and 50ms, then 100ms would exceed the 80ms budget. Land: same host,
	// already marked failed, so no request at all.
	if got := water.Load(); got != 2 {
		t.Errorf("requests in first build = %d, want 2", got)
	}
	if !rep.has("not retrying until") {
		t.Error("land polygons not skipped after the host failed")
	}
	if _, err := e.b.Run(context.Background(), &testRep{}, Options{Regions: []catalog.Region{e.region}}); err != nil {
		t.Fatal(err)
	}
	if got := water.Load(); got != 2 {
		t.Errorf("second build contacted the failed host (%d requests)", got)
	}
}

func TestMemoryBudget(t *testing.T) {
	dir := t.TempDir()
	w := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := []string{meminfoPath, cgroupV2Max, cgroupV2Stat, cgroupV1Limit, cgroupV1Stat}
	t.Cleanup(func() {
		meminfoPath, cgroupV2Max, cgroupV2Stat, cgroupV1Limit, cgroupV1Stat = old[0], old[1], old[2], old[3], old[4]
	})
	meminfoPath = w("meminfo", "MemTotal: 49000000 kB\nMemAvailable:   20971520 kB\n") // 20 GiB
	missing := filepath.Join(dir, "missing")
	tests := []struct {
		name          string
		v2max, v2stat string
		v1lim, v1stat string
		effective     int64
		source        string
	}{
		{"no cgroup", "", "", "", "", 20 << 30, "host"},
		{"v2 unlimited", "max\n", "anon 0\n", "", "", 20 << 30, "host"},
		{"v2 limit 3g", "3221225472\n", "anon 536870912\nfile 999999999\n", "", "", 2560 << 20, "cgroup v2"},
		{"v2 limit above host", "68719476736\n", "anon 0\n", "", "", 20 << 30, "host"},
		{"v1 limit 4g", "", "", "4294967296\n", "total_rss 1073741824\n", 3 << 30, "cgroup v1"},
		{"v1 unlimited", "", "", "9223372036854771712\n", "total_rss 0\n", 20 << 30, "host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cgroupV2Max, cgroupV2Stat, cgroupV1Limit, cgroupV1Stat = missing, missing, missing, missing
			if tt.v2max != "" {
				cgroupV2Max, cgroupV2Stat = w("v2max", tt.v2max), w("v2stat", tt.v2stat)
			}
			if tt.v1lim != "" {
				cgroupV1Limit, cgroupV1Stat = w("v1lim", tt.v1lim), w("v1stat", tt.v1stat)
			}
			b := memoryBudget()
			if b.Effective != tt.effective || b.Source != tt.source {
				t.Errorf("got %d (%s), want %d (%s)", b.Effective, b.Source, tt.effective, tt.source)
			}
		})
	}
}

func TestParseJavaSize(t *testing.T) {
	for in, want := range map[string]int64{"4g": 4 << 30, "4096m": 4 << 30, "512M": 512 << 20, "1073741824": 1 << 30} {
		if got, err := parseJavaSize(in); err != nil || got != want {
			t.Errorf("parseJavaSize(%q) = %d, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "g", "-1g", "lots"} {
		if _, err := parseJavaSize(bad); err == nil {
			t.Errorf("parseJavaSize(%q) accepted", bad)
		}
	}
}

func TestPlanetilerXmxOverride(t *testing.T) {
	e := newEnv(t)
	t.Setenv("MAPTHING_PLANETILER_XMX", "3g")
	rep := &testRep{}
	m, err := e.b.Run(context.Background(), rep, Options{Regions: []catalog.Region{e.region}})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.has("-Xmx3072m") {
		t.Error("override not passed to java")
	}
	if m.Memory == nil || m.Memory.Effective <= 0 {
		t.Errorf("manifest memory %+v", m.Memory)
	}
	t.Setenv("MAPTHING_PLANETILER_XMX", "lots")
	if _, err := e.b.Run(context.Background(), &testRep{}, Options{Regions: []catalog.Region{e.region}}); err == nil ||
		!strings.Contains(err.Error(), "MAPTHING_PLANETILER_XMX") {
		t.Fatalf("bad override accepted: %v", err)
	}
}

func TestManifestImage(t *testing.T) {
	e := newEnv(t)
	jar := filepath.Join(t.TempDir(), "p.jar")
	_ = os.WriteFile(jar, []byte("jar"), 0o644)
	e.b.Tools.PlanetilerJar = jar
	t.Setenv("MAPTHING_IMAGE_ID", "sha256:abc")
	t.Setenv("MAPTHING_IMAGE_REVISION", "deadbeef")
	t.Setenv("MAPTHING_VALHALLA_VERSION", "3.9.1")
	m, err := e.b.Run(context.Background(), &testRep{}, Options{Regions: []catalog.Region{e.region}})
	if err != nil {
		t.Fatal(err)
	}
	disk, _ := ReadManifest(filepath.Join(e.b.DataDir, "current"))
	img := disk.Image
	if img.ID != "sha256:abc" || img.Revision != "deadbeef" || img.Valhalla != "3.9.1" || len(img.PlanetilerJarSHA2) != 64 {
		t.Errorf("image %+v (run %+v)", img, m.Image)
	}
}

func TestValhallaMissingSpeedsFails(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	ov := filepath.Join(dir, "overrides.json")
	speeds := filepath.Join(dir, "default_speeds.json")
	_ = os.WriteFile(ov, []byte(`{"mjolnir":{"default_speeds_config":"`+speeds+`"}}`), 0o644)
	e.b.Tools.ValhallaOverrides = ov
	_, err := e.b.Run(context.Background(), &testRep{}, Options{Regions: []catalog.Region{e.region}})
	var se *StageError
	if !errors.As(err, &se) || se.Stage != StageValhalla || !strings.Contains(err.Error(), "default_speeds_config") {
		t.Fatalf("err = %v", err)
	}
	_ = os.WriteFile(speeds, []byte(`[]`), 0o644)
	if _, err := e.b.Run(context.Background(), &testRep{}, Options{Regions: []catalog.Region{e.region}}); err != nil {
		t.Fatalf("with speeds present: %v", err)
	}
}

func TestDiskGuardItemised(t *testing.T) {
	e := newEnv(t)
	// A 10°×10° "Texas" parent of 700 MiB and five 1°×1° custom boxes inside it.
	texas := catalog.Region{ID: "north-america/us/texas", Name: "Texas", BBox: catalog.BBox{-106, 26, -96, 36},
		PBFURL: "http://127.0.0.1:1/texas-latest.osm.pbf", SizeHint: 700 << 20,
		Polygon: [][][2]float64{{{-106, 26}, {-96, 26}, {-96, 36}, {-106, 36}, {-106, 26}}}}
	var regions []catalog.Region
	for i := 0; i < 5; i++ {
		w := -105 + float64(i)*2
		regions = append(regions, catalog.Region{ID: fmt.Sprintf("custom/box-%d", i), Name: "box",
			BBox: catalog.BBox{w, 31, w + 1, 32}, Custom: &catalog.Custom{Parents: []catalog.Region{texas}}})
	}
	est := e.b.Estimate(regions)
	if est.DownloadBytes != 700<<20 {
		t.Errorf("download %s, want texas once (700 MiB)", humanBytes(est.DownloadBytes))
	}
	// Each box is ~1% of the parent's area (cos-weighted), so ~7 MiB, not 700 MiB or 512 MiB.
	if est.ClipBytes < 25<<20 || est.ClipBytes > 50<<20 {
		t.Errorf("clips %s, want ≈ 5 × 7 MiB", humanBytes(est.ClipBytes))
	}
	if est.SourcesBytes != 0 {
		t.Errorf("sources %s although cached", humanBytes(est.SourcesBytes))
	}
	want := est.DownloadBytes + est.ClipBytes + est.MergeBytes + est.WorkBytes
	if est.NeedBytes != want || est.Factor != 1 {
		t.Errorf("need %d, want %d (factor %v)", est.NeedBytes, want, est.Factor)
	}
	if est.NeedBytes > 1<<30 {
		t.Errorf("five small Texas boxes need %s; the old guard said ~22 GiB", humanBytes(est.NeedBytes))
	}

	t.Setenv("MAPTHING_DISK_GUARD_FACTOR", "0.5")
	if half := e.b.Estimate(regions); half.NeedBytes != want/2 || half.Factor != 0.5 {
		t.Errorf("factor 0.5: need %d, want %d", half.NeedBytes, want/2)
	}
	t.Setenv("MAPTHING_DISK_GUARD_FACTOR", "nonsense")
	if bad := e.b.Estimate(regions); bad.Factor != 1 {
		t.Errorf("invalid factor used: %v", bad.Factor)
	}
}

func TestDiskGuardSourcesAndReuse(t *testing.T) {
	e := newEnv(t)
	for _, f := range []string{"water-polygons-split-3857.zip", "land-polygons-split-3857.zip"} {
		_ = os.Remove(filepath.Join(e.b.sourcesDir(), f))
	}
	est := e.b.Estimate([]catalog.Region{e.region})
	if est.SourcesBytes != (900+800)<<20 {
		t.Errorf("sources %s, want water+land", humanBytes(est.SourcesBytes))
	}
	// osmdata known down → the Natural Earth fallback will be used: no reservation.
	e.b.markHostFailed("osmdata.openstreetmap.de")
	est = e.b.Estimate([]catalog.Region{e.region})
	if est.SourcesBytes != 0 || !strings.Contains(strings.Join(est.Items, "\n"), "Natural Earth fallback") {
		t.Errorf("sources %s items %v", humanBytes(est.SourcesBytes), est.Items)
	}
	// An extract already on disk costs nothing to download unless refreshing.
	if _, err := e.b.Run(context.Background(), &testRep{}, Options{Regions: []catalog.Region{e.region}}); err != nil {
		t.Fatal(err)
	}
	if est := e.b.Estimate([]catalog.Region{e.region}); est.DownloadBytes != 0 {
		t.Errorf("reuse: download %d", est.DownloadBytes)
	}
	if est := e.b.EstimateFor(Options{Regions: []catalog.Region{e.region}, Refresh: true}); est.DownloadBytes == 0 {
		t.Error("refresh: download not counted")
	}
	rep := &testRep{}
	if _, err := e.b.Run(context.Background(), rep, Options{Regions: []catalog.Region{e.region}}); err != nil {
		t.Fatal(err)
	}
	if !rep.has("disk: reuse north-america/us/delaware") || !rep.has("disk: need ~") {
		t.Error("itemised estimate not in the job log")
	}
}

func TestCustomRegionMultiParentSharedDownloads(t *testing.T) {
	e := newEnv(t)
	// A second "state" served by the same fake Geofabrik URL under another id.
	md := e.region
	md.ID, md.Name = "north-america/us/maryland", "Maryland"
	acrossLine := catalog.Region{
		ID: "custom/de-md-line", Name: "DE/MD line", BBox: catalog.BBox{-75.9, 38.6, -75.6, 38.8},
		Custom: &catalog.Custom{Parents: []catalog.Region{e.region, md}},
	}
	insideDE := catalog.Region{
		ID: "custom/kent", Name: "Kent", BBox: catalog.BBox{-75.7, 38.9, -75.4, 39.2},
		Custom: &catalog.Custom{Parents: []catalog.Region{e.region}},
	}
	regions := []catalog.Region{acrossLine, insideDE, e.region}

	est := e.b.Estimate(regions)
	if est.DownloadBytes != 2*e.region.SizeHint { // delaware + maryland, each once
		t.Errorf("estimate counts %d bytes of extracts, want %d", est.DownloadBytes, 2*e.region.SizeHint)
	}

	rep := &testRep{}
	m, err := e.b.Run(context.Background(), rep, Options{Regions: regions, Refresh: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(rep.logs, "\n"))
	}
	// Two extracts (delaware, maryland), each fetched once although delaware feeds three regions.
	if got := e.pbfGets.Load(); got != 2 {
		t.Errorf("extract downloads = %d, want 2", got)
	}
	if !rep.has("custom/de-md-line: clipped from north-america/us/delaware + north-america/us/maryland") {
		t.Error("multi-parent clip not logged")
	}
	if !rep.has("osmium merge ok") {
		t.Error("clips not merged")
	}
	if got := m.Regions[0].Parents; len(got) != 2 {
		t.Errorf("manifest parents %v", got)
	}
	meta, err := readMeta(e.b.RegionDir(acrossLine.ID))
	if err != nil || !strings.Contains(meta.ParentMD5, "maryland:") || len(meta.Parents) != 2 {
		t.Errorf("clip meta %+v err %v", meta, err)
	}
	if _, err := os.Stat(filepath.Join(e.b.RegionDir(acrossLine.ID), "parts")); !os.IsNotExist(err) {
		t.Error("clip parts left behind")
	}
}
