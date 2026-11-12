package tiles_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	hosts  *ondemandtest.Hosts
	mgr    *ondemand.Manager
	mux    *http.ServeMux
	builtX int
	builtY int
}

func newODRig(t *testing.T, withArchive bool) *odRig {
	t.Helper()
	r := &odRig{hosts: ondemandtest.New()}
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
	if err := os.Symlink(gen, filepath.Join(data, "current")); err != nil {
		t.Fatal(err)
	}
	var err error
	r.mgr, err = ondemand.NewManager(context.Background(), ondemand.ManagerOptions{DataDir: data, Settings: memSettings{},
		Service: ondemand.Options{URLs: r.hosts.URLs(),
			Upstream: ondemand.NewUpstream(ondemand.UpstreamOptions{Attempts: 1, Timeout: 5 * time.Second})}})
	if err != nil {
		t.Fatal(err)
	}
	ts, err := tiles.New(tiles.Options{DataDir: data, Assets: fstest.MapFS{}, BasemapOptional: true, PackCacheBytes: 64 << 20,
		OnDemand: r.mgr, OnDemandMinZoom: ondemand.MinZoom, OnDemandMaxZoom: ondemand.MaxZoom})
	if err != nil {
		t.Fatal(err)
	}
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
	big := r.get("/packs/extract.pmtiles?tileset=terrain&minzoom=13&maxzoom=13&bbox=-98.95,29.1,-96.1,30.95")
	if big.Code != http.StatusConflict || !strings.Contains(big.Body.String(), "not_warm") || !strings.Contains(big.Body.String(), "warm the area first") {
		t.Fatalf("a pack needing thousands of cold tiles: %d %s", big.Code, big.Body.String())
	}
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
