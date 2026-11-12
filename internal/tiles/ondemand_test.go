package tiles_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/ondemand"
	"github.com/jclement/mapthing/internal/ondemand/ondemandtest"
	"github.com/jclement/mapthing/internal/pmtiles"
	"github.com/jclement/mapthing/internal/tiles"
)

type memSettings map[string]string

func (m memSettings) Setting(_ context.Context, k string) (string, error) {
	v, ok := m[k]
	if !ok {
		return "", os.ErrNotExist
	}
	return v, nil
}
func (m memSettings) SetSetting(_ context.Context, k, v string) error { m[k] = v; return nil }

// flatTile is a built terrain tile: 77 m everywhere.
func flatTile(t *testing.T) []byte {
	t.Helper()
	e := make([]float32, 256*256)
	for i := range e {
		e[i] = 77
	}
	b, err := elevation.EncodeTile(e, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func tileXY(lon, lat float64, z int) (int, int) {
	px, py := elevation.LonLatToWorldPixel(lon, lat, z)
	return int(px) / 256, int(py) / 256
}

// rig: a live generation whose terrain archive holds one z12 tile (built), a
// tile server, and on-demand terrain for the world against fake DEM hosts.
type odRig struct {
	now    atomic.Int64 // the on-demand service's clock, unix seconds
	hosts  *ondemandtest.Hosts
	mgr    *ondemand.Manager
	mux    *http.ServeMux
	builtX int
	builtY int
}

func newODRig(t *testing.T, withArchive bool) *odRig {
	t.Helper()
	r := &odRig{hosts: ondemandtest.New()}
	r.now.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix())
	t.Cleanup(r.hosts.Close)
	for lon := -99; lon <= -97; lon++ {
		r.hosts.Put3DEP(lon, 30, ondemandtest.Plane(1000), "v1")
	}
	data := t.TempDir()
	gen := filepath.Join(data, "generations", "g1")
	if err := os.MkdirAll(gen, 0o755); err != nil {
		t.Fatal(err)
	}
	r.builtX, r.builtY = tileXY(-97.5, 30.5, 12)
	if withArchive {
		w, err := pmtiles.Create(filepath.Join(gen, "terrain.pmtiles"))
		if err != nil {
			t.Fatal(err)
		}
		if err := w.AddTile(12, uint32(r.builtX), uint32(r.builtY), flatTile(t)); err != nil {
			t.Fatal(err)
		}
		meta, _ := json.Marshal(elevation.Metadata{Name: "t", Format: "png", Encoding: "terrarium", TileSize: 256, MinZoom: 0, MaxZoom: 13})
		if err := w.Commit(pmtiles.Header{TileType: pmtiles.TypePNG, TileCompression: pmtiles.CompressionNone, MinZoom: 0, MaxZoom: 13,
			MinLonE7: -980000000, MinLatE7: 300000000, MaxLonE7: -970000000, MaxLatE7: 310000000}, meta); err != nil {
			t.Fatal(err)
		}
	}
	if withArchive {
		// One built contour tile, recognisable by its bytes.
		w, err := pmtiles.Create(filepath.Join(gen, "contours.pmtiles"))
		if err != nil {
			t.Fatal(err)
		}
		cx, cy := tileXY(-97.5, 30.5, 11)
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write([]byte("built contours"))
		_ = zw.Close()
		if err := w.AddTile(11, uint32(cx), uint32(cy), gz.Bytes()); err != nil {
			t.Fatal(err)
		}
		if err := w.Commit(pmtiles.Header{TileType: pmtiles.TypeMVT, TileCompression: pmtiles.CompressionGzip, MinZoom: 10, MaxZoom: 12,
			MinLonE7: -980000000, MinLatE7: 300000000, MaxLonE7: -970000000, MaxLatE7: 310000000}, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(gen, filepath.Join(data, "current")); err != nil {
		t.Fatal(err)
	}
	var err error
	r.mgr, err = ondemand.NewManager(context.Background(), ondemand.ManagerOptions{DataDir: data, Settings: memSettings{},
		Service: ondemand.Options{URLs: r.hosts.URLs(), Now: func() time.Time { return time.Unix(r.now.Load(), 0) },
			Upstream: ondemand.NewUpstream(ondemand.UpstreamOptions{Attempts: 1, Timeout: 5 * time.Second})}})
	if err != nil {
		t.Fatal(err)
	}
	ts, err := tiles.New(tiles.Options{DataDir: data, Assets: fstest.MapFS{}, BasemapOptional: true, PackCacheBytes: 64 << 20,
		OnDemand: r.mgr, OnDemandMinZoom: ondemand.MinZoom, OnDemandMaxZoom: ondemand.MaxZoom,
		OnDemandContourMinZoom: ondemand.ContourMinZoom, OnDemandContourMaxZoom: ondemand.ContourMaxZoom})
	if err != nil {
		t.Fatal(err)
	}
	r.mgr.SetBuiltTerrain(ondemand.BuiltTerrain{Tile: ts.TerrainTile, Generation: ts.Generation})
	r.mux = http.NewServeMux()
	ts.Mount(r.mux, func(_ string, h http.Handler) http.Handler { return h })
	return r
}

func (r *odRig) get(path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func (r *odRig) tile(z, x, y int) *httptest.ResponseRecorder {
	return r.get("/tiles/terrain/" + itoa(z) + "/" + itoa(x) + "/" + itoa(y) + ".png")
}

func itoa(v int) string { b, _ := json.Marshal(v); return string(b) }

func elevAt(t *testing.T, png []byte, i int) float32 {
	t.Helper()
	tile, err := elevation.DecodeTile(png)
	if err != nil {
		t.Fatal(err)
	}
	return tile.Elev[i]
}

func TestOnDemandIsOffByDefault(t *testing.T) {
	r := newODRig(t, true)
	if rec := r.tile(12, r.builtX+1, r.builtY); rec.Code != http.StatusNoContent {
		t.Fatalf("a tile outside built terrain, with on demand off: %d", rec.Code)
	}
	if n := r.hosts.Heads.Load() + r.hosts.Ranges.Load(); n != 0 {
		t.Fatalf("%d requests went to the DEM hosts with on demand off", n)
	}
}

func TestBuiltTerrainWinsOverOnDemand(t *testing.T) {
	r := newODRig(t, true)
	if err := r.mgr.SetConfig(context.Background(), ondemand.Config{Outside: ondemand.OutsideOnDemand, CacheMB: 64}); err != nil {
		t.Fatal(err)
	}
	// The built tile is served as built, and the hosts are not asked about it.
	rec := r.tile(12, r.builtX, r.builtY)
	if rec.Code != http.StatusOK || elevAt(t, rec.Body.Bytes(), 0) != 77 {
		t.Fatalf("built tile: %d", rec.Code)
	}
	if n := r.hosts.Heads.Load() + r.hosts.Ranges.Load(); n != 0 {
		t.Fatalf("%d requests went to the DEM hosts for a built tile", n)
	}
	// Its neighbour is not in the archive: read on demand, from the synthetic ground.
	rec = r.tile(12, r.builtX+1, r.builtY)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("on-demand tile: %d %s", rec.Code, rec.Body.String())
	}
	lon, lat := elevation.WorldPixelToLonLat(float64((r.builtX+1)*256)+0.5, float64(r.builtY*256)+0.5, 12)
	if got, want := float64(elevAt(t, rec.Body.Bytes(), 0)), ondemandtest.Plane(1000)(lon, lat); !ondemandtest.Near(got, want, 0.5) {
		t.Fatalf("on-demand tile pixel = %v, want %v", got, want)
	}
	etag := rec.Header().Get("ETag")
	// Again: from the cache, and a conditional request is not re-sent.
	before := r.hosts.Ranges.Load()
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/tiles/terrain/12/"+itoa(r.builtX+1)+"/"+itoa(r.builtY)+".png", nil)
	req.Header.Set("If-None-Match", etag)
	r.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified || r.hosts.Ranges.Load() != before {
		t.Fatalf("cached tile: %d, %d new data requests", rec.Code, r.hosts.Ranges.Load()-before)
	}
	// Zooms on demand does not serve are left as they were.
	x8, y8 := tileXY(-97.3, 30.5, 8)
	if rec := r.tile(8, x8, y8); rec.Code != http.StatusNoContent {
		t.Fatalf("low zoom outside built terrain: %d", rec.Code)
	}
	// TileJSON says terrain reaches beyond the archive.
	var tj struct {
		Bounds  []float64 `json:"bounds"`
		MaxZoom int       `json:"maxzoom"`
	}
	if err := json.Unmarshal(r.get("/tiles/terrain.json").Body.Bytes(), &tj); err != nil || len(tj.Bounds) != 4 || tj.Bounds[0] != -180 || tj.MaxZoom != 13 {
		t.Fatalf("tilejson: %+v %v", tj, err)
	}
}

func TestOnDemandUpstreamDownIs504AndNotCached(t *testing.T) {
	r := newODRig(t, true)
	if err := r.mgr.SetConfig(context.Background(), ondemand.Config{Outside: ondemand.OutsideOnDemand, CacheMB: 64}); err != nil {
		t.Fatal(err)
	}
	r.hosts.Down.Store(true)
	rec := r.tile(12, r.builtX+1, r.builtY)
	if rec.Code != http.StatusGatewayTimeout || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("hosts down: %d cache-control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error.Code != "terrain_source_unavailable" {
		t.Fatalf("error body: %s", rec.Body.String())
	}
	if st := r.mgr.Status(); st.Cache.Entries != 0 || st.Healthy || st.Counters.Failed == 0 {
		t.Fatalf("after a failure: %+v", st)
	}
	r.hosts.Down.Store(false)
	if rec := r.tile(12, r.builtX+1, r.builtY); rec.Code != http.StatusOK {
		t.Fatalf("hosts back: %d", rec.Code)
	}
	if st := r.mgr.Status(); !st.Healthy || st.Cache.Entries == 0 {
		t.Fatalf("after recovery: %+v", st)
	}
}

func TestOnDemandRegionOnly(t *testing.T) {
	r := newODRig(t, false) // no built terrain at all
	areas := []ondemand.Area{{ID: "custom/west", BBox: elevation.BBox{-99, 30, -98, 31}}}
	r.mgr.Svc.SetPolicy(ondemand.Policy{Areas: areas})
	xin, yin := tileXY(-98.5, 30.5, 12)
	if rec := r.tile(12, xin, yin); rec.Code != http.StatusOK {
		t.Fatalf("inside the on-demand region: %d %s", rec.Code, rec.Body.String())
	}
	xout, yout := tileXY(-97.5, 30.5, 12)
	if rec := r.tile(12, xout, yout); rec.Code == http.StatusOK {
		t.Fatalf("outside the on-demand region: %d", rec.Code)
	}
	if rec := r.get("/tiles/terrain.json"); rec.Code != http.StatusOK {
		t.Fatalf("tilejson with no archive: %d", rec.Code)
	}
}

func TestOnDemandColdGate(t *testing.T) {
	r := newODRig(t, false)
	r.mgr.Svc.SetPolicy(ondemand.Policy{Outside: true})
	x, y := tileXY(-98.5, 30.5, 12)
	// A request whose gate refuses is answered 429 and fetches nothing.
	ctx := ondemand.WithGate(context.Background(), func() error {
		return &ondemand.ErrLimited{RetryAfter: 2 * time.Second, Message: "rate limit reached"}
	})
	if _, _, err := r.mgr.Tile(ctx, 12, x, y); err == nil {
		t.Fatal("gate did not refuse")
	}
	if r.hosts.Ranges.Load() != 0 {
		t.Fatal("a refused cold request fetched data")
	}
}

// A terrain pack over on-demand ground is filled from the cache, fetching what is missing
// first; built tiles in it stay as built.
func TestTerrainPackCoversOnDemandAreas(t *testing.T) {
	r := newODRig(t, true)
	if err := r.mgr.SetConfig(context.Background(), ondemand.Config{Outside: ondemand.OutsideOnDemand, CacheMB: 64}); err != nil {
		t.Fatal(err)
	}
	// A box around the built z12 tile and its neighbours.
	w, s, e, n := elevation.TileBounds(12, r.builtX, r.builtY)
	bbox := "bbox=" + ftoa(w-0.05) + "," + ftoa(s+0.01) + "," + ftoa(e+0.05) + "," + ftoa(n-0.01)
	rec := r.get("/packs/extract.pmtiles?tileset=terrain&minzoom=12&maxzoom=12&" + bbox)
	if rec.Code != http.StatusOK {
		t.Fatalf("pack: %d %s", rec.Code, rec.Body.String())
	}
	path := filepath.Join(t.TempDir(), "pack.pmtiles")
	if err := os.WriteFile(path, rec.Body.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	pk, err := pmtiles.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pk.Close() }()
	data, ok, err := pk.Get(12, uint32(r.builtX), uint32(r.builtY))
	if err != nil || !ok || elevAt(t, data, 0) != 77 {
		t.Fatalf("the built tile in the pack: ok=%v err=%v", ok, err)
	}
	data, ok, err = pk.Get(12, uint32(r.builtX+1), uint32(r.builtY))
	if err != nil || !ok {
		t.Fatalf("the on-demand tile in the pack: ok=%v err=%v", ok, err)
	}
	lon, lat := elevation.WorldPixelToLonLat(float64((r.builtX+1)*256)+0.5, float64(r.builtY*256)+0.5, 12)
	if got, want := float64(elevAt(t, data, 0)), ondemandtest.Plane(1000)(lon, lat); !ondemandtest.Near(got, want, 0.5) {
		t.Fatalf("on-demand tile in the pack = %v, want %v", got, want)
	}
	// The same pack again comes from the pack cache: the hosts are not asked.
	before := r.hosts.Ranges.Load() + r.hosts.Heads.Load()
	if rec := r.get("/packs/extract.mbtiles?tileset=terrain&minzoom=12&maxzoom=12&" + bbox); rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("mbtiles pack: %d", rec.Code)
	}
	if r.hosts.Ranges.Load()+r.hosts.Heads.Load() != before {
		t.Fatal("a second pack over cached tiles asked the hosts again")
	}
	// Too much to fetch inside a download request: refused, with what to do.
	defer func(n int) { tiles.MaxPackFetch = n }(tiles.MaxPackFetch)
	tiles.MaxPackFetch = 50
	big := r.get("/packs/extract.pmtiles?tileset=terrain&minzoom=13&maxzoom=13&bbox=-98.9,30.1,-98.1,30.9")
	if big.Code != http.StatusConflict || !strings.Contains(big.Body.String(), "not_warm") || !strings.Contains(big.Body.String(), "warm the area first") {
		t.Fatalf("a pack needing hundreds of cold tiles with a limit of 50: %d %.200s", big.Code, big.Body.String())
	}
	tiles.MaxPackFetch = 2000
	// A source that does not answer fails the pack; nothing half-made is kept.
	r.hosts.Down.Store(true)
	down := r.get("/packs/extract.pmtiles?tileset=terrain&minzoom=11&maxzoom=11&bbox=-98.6,30.3,-98.4,30.5")
	if down.Code != http.StatusGatewayTimeout {
		t.Fatalf("pack with the hosts down: %d %s", down.Code, down.Body.String())
	}
	r.hosts.Down.Store(false)
	if rec := r.get("/packs/extract.pmtiles?tileset=terrain&minzoom=11&maxzoom=11&bbox=-98.6,30.3,-98.4,30.5"); rec.Code != http.StatusOK {
		t.Fatalf("the same pack once the hosts are back: %d", rec.Code)
	}
}

func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }

// A pack cut from on-demand terrain is a pack of what on demand serves now: a new release
// of a source file or a change to where on demand applies makes a new pack, and the limit on
// what one pack may fetch holds for tiles that went stale as much as for tiles never seen.
func TestOnDemandPacksFollowSourcesAndPolicy(t *testing.T) {
	r := newODRig(t, false)
	ctx := context.Background()
	r.mgr.Svc.SetPolicy(ondemand.Policy{Outside: true})
	url := "/packs/extract.pmtiles?tileset=terrain&minzoom=12&maxzoom=12&bbox=-98.40,30.40,-98.10,30.60"
	first := r.get(url)
	if first.Code != http.StatusOK {
		t.Fatalf("pack: %d %s", first.Code, first.Body.String())
	}
	again := r.get(url)
	if again.Header().Get("ETag") != first.Header().Get("ETag") || again.Body.Len() != first.Body.Len() {
		t.Fatal("the same pack twice differs")
	}
	// Where on demand applies changes, inside the same overall bounds: a different pack.
	r.mgr.Svc.SetPolicy(ondemand.Policy{Outside: true, Areas: []ondemand.Area{{ID: "custom/x", BBox: elevation.BBox{-98.3, 30.45, -98.2, 30.55}}}})
	if p := r.get(url); p.Code != http.StatusOK || p.Header().Get("ETag") == first.Header().Get("ETag") {
		t.Fatalf("after a policy change: %d, ETag unchanged = %v", p.Code, p.Header().Get("ETag") == first.Header().Get("ETag"))
	}
	r.mgr.Svc.SetPolicy(ondemand.Policy{Outside: true})
	// The source publishes a new release; a day later the pack is made of it.
	for lon := -99; lon <= -97; lon++ {
		r.hosts.Put3DEP(lon, 30, ondemandtest.Plane(2000), "v2")
	}
	r.now.Add(25 * 3600)
	second := r.get(url)
	if second.Code != http.StatusOK || second.Header().Get("ETag") == first.Header().Get("ETag") {
		t.Fatalf("after a new source release: %d, same ETag = %v", second.Code, second.Header().Get("ETag") == first.Header().Get("ETag"))
	}
	if second.Body.String() == first.Body.String() {
		t.Fatal("the pack after a new source release has the old bytes")
	}
	// Another release. Every tile's cache file is still there, and every one is stale: the
	// limit must count them where they are rebuilt, not by what the cache directory holds.
	for lon := -99; lon <= -97; lon++ {
		r.hosts.Put3DEP(lon, 30, ondemandtest.Plane(3000), "v3")
	}
	r.now.Add(25 * 3600)
	defer func(n int) { tiles.MaxPackFetch = n }(tiles.MaxPackFetch)
	tiles.MaxPackFetch = 2
	before := r.mgr.Status().Counters.Built
	stale := r.get(url)
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "not_warm") {
		t.Fatalf("a pack whose tiles all went stale, with a limit of 2: %d %.200s", stale.Code, stale.Body.String())
	}
	if built := r.mgr.Status().Counters.Built - before; built > 2 {
		t.Fatalf("%d tiles were rebuilt for a pack limited to 2", built)
	}
	_ = ctx
}

// Contour tiles follow the same rules as terrain: what was built is served as built, the
// rest is drawn on demand where it applies, a source that is down is a 504 that is not
// cached, and packs cover them.
func TestOnDemandContoursThroughTheTileServer(t *testing.T) {
	r := newODRig(t, true)
	hill := func(_, lat float64) float64 { return 500 + 2000*(lat-30) }
	for lon := -99; lon <= -97; lon++ {
		r.hosts.Put3DEP(lon, 30, hill, "h1")
	}
	if err := r.mgr.SetConfig(context.Background(), ondemand.Config{Outside: ondemand.OutsideOnDemand, CacheMB: 64}); err != nil {
		t.Fatal(err)
	}
	get := func(path string, gz bool) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		if gz {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		r.mux.ServeHTTP(rec, req)
		return rec
	}
	// The built tile, as built, with nothing asked of the hosts.
	bx, by := tileXY(-97.5, 30.5, 11)
	rec := get("/tiles/contours/11/"+itoa(bx)+"/"+itoa(by)+".mvt", false)
	if rec.Code != http.StatusOK || rec.Body.String() != "built contours" {
		t.Fatalf("built contour tile: %d %q", rec.Code, rec.Body.String())
	}
	if n := r.hosts.Heads.Load() + r.hosts.Ranges.Load(); n != 0 {
		t.Fatalf("%d requests went to the hosts for a built contour tile", n)
	}
	// A tile the archive does not have: drawn on demand, gzipped for a client that takes it.
	x, y := tileXY(-98.5, 30.5, 11)
	path := "/tiles/contours/11/" + itoa(x) + "/" + itoa(y) + ".mvt"
	rec = get(path, true)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Content-Type") != "application/vnd.mapbox-vector-tile" {
		t.Fatalf("on-demand contour tile: %d %v", rec.Code, rec.Header())
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	if rec2 := get(path, false); rec2.Code != http.StatusOK || rec2.Header().Get("Content-Encoding") != "" || !bytes.Equal(rec2.Body.Bytes(), plain) || len(plain) < 100 {
		t.Fatalf("the same tile for a client without gzip: %d, %d bytes against %d", rec2.Code, rec2.Body.Len(), len(plain))
	}
	// TileJSON reaches beyond the archive.
	var tj struct {
		Bounds []float64 `json:"bounds"`
	}
	if err := json.Unmarshal(r.get("/tiles/contours.json").Body.Bytes(), &tj); err != nil || len(tj.Bounds) != 4 || tj.Bounds[0] != -180 {
		t.Fatalf("contours tilejson: %+v %v", tj, err)
	}
	// A pack of contours over on-demand ground.
	if p := r.get("/packs/extract.pmtiles?tileset=contours&minzoom=11&maxzoom=11&bbox=-98.6,30.4,-98.4,30.6"); p.Code != http.StatusOK || p.Body.Len() < 500 {
		t.Fatalf("contours pack: %d, %d bytes", p.Code, p.Body.Len())
	}
	// The source goes away: a tile not drawn yet is a 504, and stays undrawn.
	r.hosts.Down.Store(true)
	x2, y2 := tileXY(-98.5, 30.9, 12)
	down := get("/tiles/contours/12/"+itoa(x2)+"/"+itoa(y2)+".mvt", true)
	if down.Code != http.StatusGatewayTimeout || down.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("hosts down: %d %q", down.Code, down.Header().Get("Cache-Control"))
	}
	if r.mgr.HasContour(12, x2, y2) {
		t.Fatal("a failed contour tile was cached")
	}
	if rec := get(path, true); rec.Code != http.StatusOK {
		t.Fatalf("a cached contour tile with the hosts down: %d", rec.Code)
	}
}
