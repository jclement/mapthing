package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jclement/mapthing/internal/auth"
	"github.com/jclement/mapthing/internal/jobs"
	"github.com/jclement/mapthing/internal/keys"
	"github.com/jclement/mapthing/internal/pmtiles"
	"github.com/jclement/mapthing/internal/scheduler"
	"github.com/jclement/mapthing/internal/stats"
	"github.com/jclement/mapthing/internal/store"
	"github.com/jclement/mapthing/internal/supervisor"
	"github.com/jclement/mapthing/internal/tiles"
)

const (
	adminUser = "admin"
	adminPass = "correct horse battery staple"
	token     = "internal-test-token"
)

type env struct {
	t     *testing.T
	ts    *httptest.Server
	st    *store.Store
	stats *stats.Stats
	jobs  *jobs.Runner
	dir   string
	tiles *tiles.Server
}

var testAssets = fstest.MapFS{
	"styles/outdoors.json": {Data: []byte(`{"version":8,"name":"Outdoors","metadata":{"mapthing:name":"Outdoors","mapthing:description":"Topo"},
"sprite":"{{BASE_URL}}/sprites/outdoors",
"glyphs":"{{BASE_URL}}/fonts/{fontstack}/{range}.pbf{{KEY_QS}}",
"sources":{"protomaps":{"type":"vector","url":"{{BASE_URL}}/tiles/basemap.json{{KEY_QS}}"},"imagery":{"type":"raster","tiles":["{{BASE_URL}}/tiles/imagery/{z}/{x}/{y}.jpg{{KEY_QS}}"],"attribution":"built-in"}},
"layers":[{"id":"satellite","type":"raster","source":"imagery","layout":{"visibility":"none"}}]}`)},
	"styles/outdoors-dark.json":         {Data: []byte(`{"version":8,"layers":[]}`)},
	"styles/package.json":               {Data: []byte(`{"name":"styles","version":"1.0.0","type":"module"}`)},
	"styles/satellite.json":             {Data: []byte(`{"version":8,"layers":[]}`)},
	"fonts/Noto Sans Regular/0-255.pbf": {Data: []byte("glyphs")},
	"sprites/outdoors@2x.png":           {Data: []byte("\x89PNG")},
	"sprites/outdoors.json":             {Data: []byte(`{}`)},
}

// fixtureTile returns the uncompressed body of a fixture tile; empty at z6 x>=32.
func fixtureTile(z uint8, x, y uint32) []byte {
	if z == 6 && x >= 32 {
		return nil
	}
	return fmt.Appendf(nil, "mvt %d/%d/%d", z, x, y)
}

// writeFixture writes a small gzip-MVT PMTiles archive (z0–6) to path.
func writeFixture(t *testing.T, path string) {
	t.Helper()
	w, err := pmtiles.NewWriter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var ids []uint64
	for z := uint8(0); z <= 6; z++ {
		for x := range uint32(1) << z {
			for y := range uint32(1) << z {
				ids = append(ids, pmtiles.ZxyToID(z, x, y))
			}
		}
	}
	sortIDs(ids)
	for _, id := range ids {
		z, x, y := pmtiles.IDToZxy(id)
		body := fixtureTile(z, x, y)
		if body == nil {
			continue
		}
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write(body)
		_ = zw.Close()
		if err := w.Add(id, gz.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	h := pmtiles.Header{TileType: pmtiles.TypeMVT, TileCompression: pmtiles.CompressionGzip, MinZoom: 0, MaxZoom: 6,
		MinLonE7: -1800000000, MinLatE7: -850000000, MaxLonE7: 1800000000, MaxLatE7: 850000000, CenterZoom: 2}
	if err := w.Finish(f, h, []byte(`{"vector_layers":[{"id":"roads"}],"attribution":"© OSM"}`)); err != nil {
		t.Fatal(err)
	}
}

func sortIDs(a []uint64) {
	// small helper to avoid importing slices in two places
	for i := 1; i < len(a); i++ {
		v := a[i]
		j := i - 1
		for j >= 0 && a[j] > v {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = v
	}
}

func newEnv(t *testing.T, withGeneration bool, opts ...func(*Deps)) *env {
	t.Helper()
	dir := t.TempDir()
	if withGeneration {
		gdir := filepath.Join(dir, "generations", "g1")
		if err := os.MkdirAll(gdir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, filepath.Join(gdir, "basemap.pmtiles"))
		if err := os.WriteFile(filepath.Join(gdir, "manifest.json"), []byte(`{"generation":"g1","ok":true,"regions":[{"id":"us/delaware","name":"Delaware","bbox":[-75.8,38.4,-75.0,39.9],"source_timestamp":"2026-11-07T20:21:02Z"}],"stages":[{"name":"basemap","duration_s":12.5}],"checks":[{"name":"tile","region":"us/delaware","ok":true}]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("generations", "g1"), filepath.Join(dir, "current")); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, err := store.Open(ctx, filepath.Join(dir, "mapthing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ks := keys.NewService(st)
	ss := stats.New(st)
	au := auth.New(adminUser, adminPass, st, ks, ss, "")
	au.SetInternalToken(token)
	runner := jobs.NewRunner(st)
	sched := scheduler.New(st, runner)
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ts, err := tiles.New(tiles.Options{DataDir: dir, Assets: testAssets, Stats: ss, RendererURL: "http://127.0.0.1:1",
		RasterCacheBytes: 1 << 20, ImageryCacheBytes: 1 << 20, PackCacheBytes: 10 << 20})
	if err != nil {
		t.Fatal(err)
	}
	sup := supervisor.New()
	deps := Deps{DataDir: dir, Store: st, Auth: au, Keys: ks, Stats: ss, Jobs: runner, Scheduler: sched,
		Supervisor: sup, Tiles: ts, Assets: testAssets, UI: fstest.MapFS{"index.html": {Data: []byte("<html>ui</html>")}, "assets/app-123.js": {Data: []byte("js")}}}
	for _, o := range opts {
		o(&deps)
	}
	srv := New(deps)
	if err := runner.Start(ctx); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return &env{t: t, ts: hs, st: st, stats: ss, jobs: runner, dir: dir, tiles: ts}
}

// client returns an HTTP client with a cookie jar.
func (e *env) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func (e *env) do(c *http.Client, method, path string, body any, hdr map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (e *env) login() *http.Client {
	e.t.Helper()
	c := e.client()
	resp, b := e.do(c, "POST", "/admin/api/login", map[string]string{"username": adminUser, "password": adminPass}, nil)
	if resp.StatusCode != 200 {
		e.t.Fatalf("login: %d %s", resp.StatusCode, b)
	}
	return c
}

func (e *env) createKey(c *http.Client, body map[string]any) (int64, string) {
	e.t.Helper()
	resp, b := e.do(c, "POST", "/admin/api/keys", body, map[string]string{"Origin": e.ts.URL})
	if resp.StatusCode != 201 {
		e.t.Fatalf("create key: %d %s", resp.StatusCode, b)
	}
	var out struct {
		Key struct {
			ID string `json:"id"`
		} `json:"key"`
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal(b, &out)
	id, _ := strconv.ParseInt(out.Key.ID, 10, 64)
	return id, out.Secret
}

func TestHealth(t *testing.T) {
	e := newEnv(t, false)
	if r, _ := e.do(nil, "GET", "/healthz", nil, nil); r.StatusCode != 200 {
		t.Fatalf("healthz %d", r.StatusCode)
	}
	if r, b := e.do(nil, "GET", "/readyz", nil, nil); r.StatusCode != 503 {
		t.Fatalf("readyz without generation: %d %s", r.StatusCode, b)
	}
	if r, _ := e.do(nil, "GET", "/tiles/basemap.json", nil, map[string]string{"X-Mapthing-Internal": token}); r.StatusCode != 503 {
		t.Fatalf("tilejson before any build: %d", r.StatusCode)
	}
	e2 := newEnv(t, true)
	if r, b := e2.do(nil, "GET", "/readyz", nil, nil); r.StatusCode != 200 {
		t.Fatalf("readyz with generation: %d %s", r.StatusCode, b)
	}
}

func TestLogin(t *testing.T) {
	e := newEnv(t, false)
	c := e.client()
	if r, _ := e.do(c, "GET", "/admin/api/keys", nil, nil); r.StatusCode != 401 {
		t.Fatalf("unauthenticated admin: %d", r.StatusCode)
	}
	if r, _ := e.do(c, "POST", "/admin/api/login", map[string]string{"username": adminUser, "password": "nope"}, nil); r.StatusCode != 401 {
		t.Fatalf("bad login: %d", r.StatusCode)
	}
	resp, _ := e.do(c, "POST", "/admin/api/login", map[string]string{"username": adminUser, "password": adminPass}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	var ck *http.Cookie
	for _, k := range resp.Cookies() {
		if k.Name == auth.CookieName {
			ck = k
		}
	}
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie flags: %+v", ck)
	}
	if r, _ := e.do(c, "GET", "/admin/api/keys", nil, nil); r.StatusCode != 200 {
		t.Fatalf("session admin: %d", r.StatusCode)
	}
	if _, b := e.do(c, "GET", "/admin/api/session", nil, nil); !strings.Contains(string(b), "true") {
		t.Fatalf("session: %s", b)
	}
	e.do(c, "POST", "/admin/api/logout", nil, nil)
	if r, _ := e.do(c, "GET", "/admin/api/keys", nil, nil); r.StatusCode != 401 {
		t.Fatalf("after logout: %d", r.StatusCode)
	}
	// the stored session is gone, not just the cookie
	if r, _ := e.do(nil, "GET", "/admin/api/keys", nil, map[string]string{"Cookie": auth.CookieName + "=" + ck.Value}); r.StatusCode != 401 {
		t.Fatalf("replayed cookie after logout: %d", r.StatusCode)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t, false)
	for i := range 5 {
		if r, _ := e.do(nil, "POST", "/admin/api/login", map[string]string{"username": "x", "password": "y"}, nil); r.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, r.StatusCode)
		}
	}
	r, _ := e.do(nil, "POST", "/admin/api/login", map[string]string{"username": adminUser, "password": adminPass}, nil)
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("expected 429 with Retry-After, got %d", r.StatusCode)
	}
}

func TestCSRF(t *testing.T) {
	e := newEnv(t, false)
	c := e.login()
	body := map[string]any{"label": "x"}
	if r, _ := e.do(c, "POST", "/admin/api/keys", body, map[string]string{"Origin": "https://evil.example"}); r.StatusCode != 403 {
		t.Fatalf("cross-origin POST: %d", r.StatusCode)
	}
	if r, _ := e.do(c, "POST", "/admin/api/keys", body, map[string]string{"Sec-Fetch-Site": "cross-site"}); r.StatusCode != 403 {
		t.Fatalf("cross-site POST: %d", r.StatusCode)
	}
	if r, _ := e.do(c, "POST", "/admin/api/keys", body, map[string]string{"Referer": "https://evil.example/page"}); r.StatusCode != 403 {
		t.Fatalf("cross-origin Referer POST: %d", r.StatusCode)
	}
	if r, _ := e.do(c, "POST", "/admin/api/keys", body, map[string]string{"Origin": e.ts.URL}); r.StatusCode != 201 {
		t.Fatalf("same-origin POST: %d", r.StatusCode)
	}
	if r, _ := e.do(c, "POST", "/admin/api/keys", body, map[string]string{"Sec-Fetch-Site": "same-origin"}); r.StatusCode != 201 {
		t.Fatalf("same-origin (fetch metadata) POST: %d", r.StatusCode)
	}
	// login CSRF
	if r, _ := e.do(nil, "POST", "/admin/api/login", map[string]string{"username": adminUser, "password": adminPass}, map[string]string{"Origin": "https://evil.example"}); r.StatusCode != 403 {
		t.Fatalf("cross-origin login: %d", r.StatusCode)
	}
}

func TestKeysLifecycle(t *testing.T) {
	e := newEnv(t, true)
	c := e.login()
	id, secret := e.createKey(c, map[string]any{"label": "field app"})
	if !keys.LooksLikeKey(secret) {
		t.Fatalf("secret shape %q", secret)
	}
	// only the hash is stored
	var hash, prefix string
	if err := e.st.DB.QueryRow(`SELECT hash, prefix FROM api_keys WHERE id = ?`, id).Scan(&hash, &prefix); err != nil {
		t.Fatal(err)
	}
	if hash != keys.Hash(secret) || strings.Contains(hash, secret[3:]) || !strings.HasPrefix(secret, prefix) {
		t.Fatalf("stored hash/prefix wrong")
	}
	// list/get never reveal the secret
	_, list := e.do(c, "GET", "/admin/api/keys", nil, nil)
	if strings.Contains(string(list), secret) || !strings.Contains(string(list), "field app") {
		t.Fatalf("list: %s", list)
	}
	// works on keyed endpoints, all four ways
	for _, h := range []map[string]string{
		{"Authorization": "Bearer " + secret},
		{"Authorization": secret},
		{"X-Goog-Api-Key": secret},
	} {
		if r, b := e.do(nil, "GET", "/tiles/basemap.json", nil, h); r.StatusCode != 200 {
			t.Fatalf("%v: %d %s", h, r.StatusCode, b)
		}
	}
	if r, _ := e.do(nil, "GET", "/tiles/basemap.json?key="+secret, nil, nil); r.StatusCode != 200 {
		t.Fatalf("?key=: %d", r.StatusCode)
	}
	if r, _ := e.do(nil, "GET", "/tiles/basemap.json", nil, nil); r.StatusCode != 401 {
		t.Fatalf("no key: %d", r.StatusCode)
	}
	if r, _ := e.do(nil, "GET", "/tiles/basemap.json?key=mt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil, nil); r.StatusCode != 401 {
		t.Fatalf("unknown key: %d", r.StatusCode)
	}
	// edit
	if r, b := e.do(c, "PATCH", fmt.Sprintf("/admin/api/keys/%d", id), map[string]any{"label": "renamed"}, map[string]string{"Origin": e.ts.URL}); r.StatusCode != 200 || !strings.Contains(string(b), "renamed") {
		t.Fatalf("edit: %d %s", r.StatusCode, b)
	}
	// revoke
	if r, _ := e.do(c, "DELETE", fmt.Sprintf("/admin/api/keys/%d", id), nil, map[string]string{"Origin": e.ts.URL}); r.StatusCode != 204 {
		t.Fatalf("revoke: %d", r.StatusCode)
	}
	if _, b := e.do(c, "GET", "/admin/api/keys", nil, nil); strings.Contains(string(b), "renamed") {
		t.Fatalf("revoked key still listed: %s", b)
	}
	if _, b := e.do(c, "GET", "/admin/api/keys?revoked=1", nil, nil); !strings.Contains(string(b), `"revoked_at"`) {
		t.Fatalf("revoked=1: %s", b)
	}
	if r, _ := e.do(nil, "GET", "/tiles/basemap.json?key="+secret, nil, nil); r.StatusCode != 401 {
		t.Fatalf("revoked key: %d", r.StatusCode)
	}
}

func TestKeyLimits(t *testing.T) {
	e := newEnv(t, true)
	c := e.login()
	_, secret := e.createKey(c, map[string]any{"label": "rpm", "rpm": 3})
	for i := range 3 {
		if r, _ := e.do(nil, "GET", "/tiles/basemap.json?key="+secret, nil, nil); r.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, r.StatusCode)
		}
	}
	r, b := e.do(nil, "GET", "/tiles/basemap.json?key="+secret, nil, nil)
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" || !strings.Contains(string(b), "rate") {
		t.Fatalf("rpm exceeded: %d %s", r.StatusCode, b)
	}

	_, daily := e.createKey(c, map[string]any{"label": "rpd", "rpd": 2, "rpm": nil})
	e.do(nil, "GET", "/tiles/basemap.json?key="+daily, nil, nil)
	e.do(nil, "GET", "/tiles/basemap.json?key="+daily, nil, nil)
	if r, _ := e.do(nil, "GET", "/tiles/basemap.json?key="+daily, nil, nil); r.StatusCode != 429 {
		t.Fatalf("rpd exceeded: %d", r.StatusCode)
	}

	_, origin := e.createKey(c, map[string]any{"label": "web", "origins": []string{"https://app.example.com", "*.example.org"}})
	cases := []struct {
		hdr  map[string]string
		want int
	}{
		{map[string]string{"Origin": "https://app.example.com"}, 200},
		{map[string]string{"Origin": "https://maps.example.org"}, 200},
		{map[string]string{"Referer": "https://app.example.com/page"}, 200},
		{map[string]string{"Origin": "https://evil.example"}, 403},
		{nil, 403},
	}
	for _, tc := range cases {
		if r, b := e.do(nil, "GET", "/tiles/basemap.json?key="+origin, nil, tc.hdr); r.StatusCode != tc.want {
			t.Errorf("origin %v: %d want %d (%s)", tc.hdr, r.StatusCode, tc.want, b)
		}
	}
}

func TestCompatErrorShapes(t *testing.T) {
	e := newEnv(t, false)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux := http.NewServeMux()
	// emulate feature mounts
	au := auth.New(adminUser, adminPass, e.st, keys.NewService(e.st), e.stats, "")
	mux.Handle("GET /maps/api/elevation/json", au.Wrap("compat", ok))
	mux.Handle("POST /directions/v2:computeRoutes", au.Wrap("compat", ok))
	mux.Handle("POST /ors/v2/snap/{profile}", au.Wrap("compat", ok))
	hs := httptest.NewServer(mux)
	defer hs.Close()
	check := func(method, path, want string) {
		req, _ := http.NewRequest(method, hs.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 401 || !strings.Contains(string(b), want) {
			t.Errorf("%s: %d %s (want %s)", path, resp.StatusCode, b, want)
		}
	}
	// Google clients switch by configuration only: the key works as X-Goog-Api-Key and as key=.
	_, secret, err := keys.NewService(e.st).Create(context.Background(), "google client", store.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path, hdr string }{
		{"GET", "/maps/api/elevation/json?locations=39.1,-75.5&key=" + secret, ""},
		{"GET", "/maps/api/elevation/json?locations=39.1,-75.5", secret},
		{"POST", "/directions/v2:computeRoutes?key=" + secret, ""},
		{"POST", "/directions/v2:computeRoutes", secret},
	} {
		req, _ := http.NewRequest(c.method, hs.URL+c.path, nil)
		if c.hdr != "" {
			req.Header.Set("X-Goog-Api-Key", c.hdr)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s %s (header %v): %d", c.method, c.path, c.hdr != "", resp.StatusCode)
		}
	}
	check("GET", "/maps/api/elevation/json", `"status":"REQUEST_DENIED"`)
	check("POST", "/directions/v2:computeRoutes", `"status":"UNAUTHENTICATED"`)
	check("POST", "/ors/v2/snap/driving-hgv", `"code":401`)
}

func TestTiles(t *testing.T) {
	e := newEnv(t, true)
	c := e.login()
	_, secret := e.createKey(c, map[string]any{"label": "tiles"})
	q := "?key=" + secret

	// gzip passthrough
	r, b := e.do(nil, "GET", "/tiles/basemap/2/1/1.mvt"+q, nil, map[string]string{"Accept-Encoding": "gzip"})
	if r.StatusCode != 200 || r.Header.Get("Content-Encoding") != "gzip" || r.Header.Get("Content-Type") != "application/vnd.mapbox-vector-tile" {
		t.Fatalf("gzip tile: %d %v", r.StatusCode, r.Header)
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	if string(plain) != "mvt 2/1/1" {
		t.Fatalf("tile body %q", plain)
	}
	etag := r.Header.Get("ETag")
	if etag == "" || !strings.Contains(r.Header.Get("Cache-Control"), "max-age") {
		t.Fatalf("cache headers %v", r.Header)
	}
	if r, _ := e.do(nil, "GET", "/tiles/basemap/2/1/1.mvt"+q, nil, map[string]string{"Accept-Encoding": "gzip", "If-None-Match": etag}); r.StatusCode != 304 {
		t.Fatalf("conditional: %d", r.StatusCode)
	}
	// client without gzip gets plain bytes
	req, _ := http.NewRequest("GET", e.ts.URL+"/tiles/basemap/2/1/1.mvt"+q, nil)
	req.Header.Set("Accept-Encoding", "identity")
	tr := &http.Transport{DisableCompression: true}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "" || string(pb) != "mvt 2/1/1" {
		t.Fatalf("identity tile: %q %v", pb, resp.Header)
	}
	// empty tile → 204, out of range → 400, unknown tileset → 404
	if r, _ := e.do(nil, "GET", "/tiles/basemap/6/40/3.mvt"+q, nil, nil); r.StatusCode != 204 {
		t.Fatalf("empty tile: %d", r.StatusCode)
	}
	if r, _ := e.do(nil, "GET", "/tiles/basemap/7/0/0.mvt"+q, nil, nil); r.StatusCode != 204 {
		t.Fatalf("beyond maxzoom: %d", r.StatusCode)
	}
	if r, _ := e.do(nil, "GET", "/tiles/basemap/2/9/0.mvt"+q, nil, nil); r.StatusCode != 400 {
		t.Fatalf("bad address: %d", r.StatusCode)
	}
	if r, _ := e.do(nil, "GET", "/tiles/nope/0/0/0.mvt"+q, nil, nil); r.StatusCode != 404 {
		t.Fatalf("unknown tileset: %d", r.StatusCode)
	}
	if r, _ := e.do(nil, "GET", "/tiles/terrain/0/0/0.png"+q, nil, nil); r.StatusCode != 404 {
		t.Fatalf("missing terrain archive: %d", r.StatusCode)
	}
	if r, _ := e.do(nil, "GET", "/tiles/terrain.json"+q, nil, nil); r.StatusCode != 404 {
		t.Fatalf("missing terrain tilejson: %d", r.StatusCode)
	}

	// TileJSON carries the key and metadata
	_, tj := e.do(nil, "GET", "/tiles/basemap.json"+q, nil, nil)
	var doc map[string]any
	if err := json.Unmarshal(tj, &doc); err != nil {
		t.Fatal(err)
	}
	tl := doc["tiles"].([]any)[0].(string)
	if !strings.HasSuffix(tl, "/tiles/basemap/{z}/{x}/{y}.mvt"+q) || doc["vector_layers"] == nil || doc["maxzoom"].(float64) != 6 {
		t.Fatalf("tilejson: %s", tj)
	}
	// admin session: no key in URLs
	_, tj2 := e.do(c, "GET", "/tiles/basemap.json", nil, nil)
	if strings.Contains(string(tj2), "key=") {
		t.Fatalf("admin tilejson has a key: %s", tj2)
	}
}

// TestContoursTileset: contours.pmtiles in the live generation is served like the basemap
// (z/x/y MVT and TileJSON); absent, it is a 404.
func TestContoursTileset(t *testing.T) {
	e := newEnv(t, true)
	c := e.login()
	_, secret := e.createKey(c, map[string]any{"label": "contours"})
	q := "?key=" + secret
	if r, _ := e.do(nil, "GET", "/tiles/contours/2/1/1.mvt"+q, nil, nil); r.StatusCode != 404 {
		t.Fatalf("missing contours archive: %d", r.StatusCode)
	}
	writeFixture(t, filepath.Join(e.dir, "generations", "g1", "contours.pmtiles"))
	e.tiles.Reload()
	r, _ := e.do(nil, "GET", "/tiles/contours/2/1/1.mvt"+q, nil, map[string]string{"Accept-Encoding": "gzip"})
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != "application/vnd.mapbox-vector-tile" {
		t.Fatalf("contours tile: %d %v", r.StatusCode, r.Header)
	}
	_, tj := e.do(nil, "GET", "/tiles/contours.json"+q, nil, nil)
	var doc map[string]any
	if err := json.Unmarshal(tj, &doc); err != nil {
		t.Fatal(err)
	}
	if tl := doc["tiles"].([]any)[0].(string); !strings.Contains(tl, "/tiles/contours/{z}/{x}/{y}.mvt?key=") {
		t.Fatalf("tiles url %q", tl)
	}
	if doc["vector_layers"] == nil {
		t.Fatalf("no vector_layers: %s", tj)
	}
}

func TestArchiveRange(t *testing.T) {
	e := newEnv(t, true)
	c := e.login()
	_, secret := e.createKey(c, map[string]any{"label": "flutter"})
	path := "/tiles/basemap.pmtiles?key=" + secret
	r, b := e.do(nil, "GET", path, nil, map[string]string{"Range": "bytes=0-126"})
	if r.StatusCode != 206 || len(b) != 127 || string(b[:7]) != "PMTiles" {
		t.Fatalf("range: %d len=%d", r.StatusCode, len(b))
	}
	if r.Header.Get("Accept-Ranges") != "bytes" || r.Header.Get("Content-Encoding") != "" || !strings.Contains(r.Header.Get("Cache-Control"), "public") {
		t.Fatalf("archive headers %v", r.Header)
	}
	if !strings.HasPrefix(r.Header.Get("Content-Range"), "bytes 0-126/") {
		t.Fatalf("content-range %q", r.Header.Get("Content-Range"))
	}
	etag := r.Header.Get("ETag")
	// If-Range with the right ETag still ranges; a wrong one gets the full body
	if r, _ := e.do(nil, "GET", path, nil, map[string]string{"Range": "bytes=0-9", "If-Range": etag}); r.StatusCode != 206 {
		t.Fatalf("if-range match: %d", r.StatusCode)
	}
	if r, _ := e.do(nil, "GET", path, nil, map[string]string{"Range": "bytes=0-9", "If-Range": `"other"`}); r.StatusCode != 200 {
		t.Fatalf("if-range mismatch: %d", r.StatusCode)
	}
	// the whole archive parses
	_, full := e.do(nil, "GET", path, nil, nil)
	rd, err := pmtiles.NewReader(bytes.NewReader(full), int64(len(full)))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := rd.Get(3, 2, 2); !ok {
		t.Fatal("tile missing from downloaded archive")
	}
}

func TestStylesAndAssets(t *testing.T) {
	e := newEnv(t, true)
	c := e.login()
	_, secret := e.createKey(c, map[string]any{"label": "gl"})
	q := "?key=" + secret
	r, b := e.do(nil, "GET", "/styles/outdoors.json"+q, nil, nil)
	if r.StatusCode != 200 {
		t.Fatalf("style %d %s", r.StatusCode, b)
	}
	s := string(b)
	if strings.Contains(s, "{{") || !strings.Contains(s, e.ts.URL+"/tiles/basemap.json"+q) {
		t.Fatalf("templating: %s", s)
	}
	var st map[string]any
	_ = json.Unmarshal(b, &st)
	if st["sprite"] != e.ts.URL+"/sprites/outdoors"+q {
		t.Fatalf("sprite url %v", st["sprite"])
	}
	// Mapbox-shaped style URL
	if r, _ := e.do(nil, "GET", "/styles/v1/someone/outdoors"+q, nil, nil); r.StatusCode != 200 {
		t.Fatalf("v1 style: %d", r.StatusCode)
	}
	for _, p := range []string{"/styles/nope.json", "/styles/package.json", "/styles/package/256/1/0/0.png"} {
		if r, _ := e.do(nil, "GET", p+q, nil, nil); r.StatusCode != 404 {
			t.Fatalf("%s: %d", p, r.StatusCode)
		}
	}
	// list
	_, lb := e.do(nil, "GET", "/styles"+q, nil, nil)
	var list []tiles.StyleInfo
	_ = json.Unmarshal(lb, &list)
	if len(list) != 3 || list[0].ID != "outdoors" || list[0].Description != "Topo" || !list[1].Dark || list[2].ID != "satellite" {
		t.Fatalf("style list: %s", lb)
	}
	// admin system lists them too
	if _, sb := e.do(c, "GET", "/admin/api/system", nil, nil); !strings.Contains(string(sb), `"outdoors-dark"`) {
		t.Fatalf("system styles: %s", sb)
	}
	// glyphs: first font in the stack that exists; 404 when none
	if r, b := e.do(nil, "GET", "/fonts/Missing,Noto%20Sans%20Regular/0-255.pbf"+q, nil, nil); r.StatusCode != 200 || string(b) != "glyphs" {
		t.Fatalf("glyphs: %d %q", r.StatusCode, b)
	}
	if r, _ := e.do(nil, "GET", "/fonts/Noto%20Sans%20Regular/256-511.pbf"+q, nil, nil); r.StatusCode != 404 {
		t.Fatalf("missing glyphs: %d", r.StatusCode)
	}
	if r, _ := e.do(nil, "GET", "/fonts/..%2F..%2Fetc/0-255.pbf"+q, nil, nil); r.StatusCode != 404 {
		t.Fatalf("traversal: %d", r.StatusCode)
	}
	if r, _ := e.do(nil, "GET", "/sprites/outdoors@2x.png"+q, nil, nil); r.StatusCode != 200 || r.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("sprite: %d", r.StatusCode)
	}
	// renderer-internal requests: loopback + token, no key, counted as raster-internal
	r, b = e.do(nil, "GET", "/styles/outdoors.json?internal=1", nil, map[string]string{auth.InternalHeader: token})
	if r.StatusCode != 200 || strings.Contains(string(b), "key=") {
		t.Fatalf("internal style: %d %s", r.StatusCode, b)
	}
	if r, _ := e.do(nil, "GET", "/styles/outdoors.json", nil, map[string]string{auth.InternalHeader: "wrong"}); r.StatusCode != 401 {
		t.Fatalf("wrong internal token: %d", r.StatusCode)
	}
	if tot, _ := e.stats.Totals(); tot[auth.KindInternal] != 1 {
		t.Fatalf("internal count %v", tot)
	}
}

func TestRasterProxyAndCache(t *testing.T) {
	calls := 0
	var paths []string
	renderer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		paths = append(paths, r.URL.Path)
		switch {
		case r.URL.Path == "/render/outdoors/256/4/2/1.png": // rendered without a failed source
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set(tiles.DegradedHeader, "1")
			_, _ = w.Write([]byte("\x89PNG partial"))
		case r.URL.Path == "/render/outdoors/512/3/2/1@2x.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("\x89PNG fake"))
		case strings.HasSuffix(r.URL.Path, ".jpg"):
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("\xff\xd8\xff fake"))
		case strings.HasSuffix(r.URL.Path, ".webp"):
			w.Header().Set("Content-Type", "image/webp")
			_, _ = w.Write([]byte("RIFF fake"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer renderer.Close()
	e := newEnv(t, true)
	// swap in a tiles server pointing at the fake renderer
	ts, err := tiles.New(tiles.Options{DataDir: e.dir, Assets: testAssets, Stats: e.stats, RendererURL: renderer.URL, RasterCacheBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	au := auth.New(adminUser, adminPass, e.st, keys.NewService(e.st), e.stats, "")
	ts.Mount(mux, au.Wrap)
	hs := httptest.NewServer(mux)
	defer hs.Close()
	k, secret, err := keys.NewService(e.st).Create(context.Background(), "r", store.Limits{})
	if err != nil || k == nil {
		t.Fatal(err)
	}
	get := func(p string) (*http.Response, []byte) {
		resp, err := http.Get(hs.URL + p + "?key=" + secret)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	for _, p := range []string{"/styles/outdoors/512/3/2/1@2x.png", "/styles/v1/u/outdoors/tiles/512/3/2/1@2x", "/styles/v1/u/outdoors/tiles/3/2/1@2x"} {
		r, b := get(p)
		if r.StatusCode != 200 || string(b) != "\x89PNG fake" || r.Header.Get("Content-Type") != "image/png" {
			t.Fatalf("%s: %d %q", p, r.StatusCode, b)
		}
	}
	if calls != 1 {
		t.Fatalf("renderer called %d times, want 1 (cache)", calls)
	}
	calls = 0
	// formats: explicit extension; the Mapbox-shaped URL defaults to jpg for satellite
	formats := []struct{ path, ct, upstream string }{
		{"/styles/v1/u/satellite/tiles/512/3/2/1@2x", "image/jpeg", "/render/satellite/512/3/2/1@2x.jpg"},
		{"/styles/outdoors/256/3/2/1.webp", "image/webp", "/render/outdoors/256/3/2/1.webp"},
		{"/styles/outdoors/256/3/2/1.jpg", "image/jpeg", "/render/outdoors/256/3/2/1.jpg"},
		{"/styles/outdoors/256/3/2/1.jpeg", "image/jpeg", "/render/outdoors/256/3/2/1.jpg"},
	}
	for _, f := range formats {
		r, _ := get(f.path)
		if r.StatusCode != 200 || r.Header.Get("Content-Type") != f.ct || paths[len(paths)-1] != f.upstream {
			t.Fatalf("%s: %d %s via %v", f.path, r.StatusCode, r.Header.Get("Content-Type"), paths)
		}
	}
	// the png of the same tile is a different cache entry (renderer asked again)
	before := len(paths)
	get("/styles/v1/u/satellite/tiles/512/3/2/1@2x.png")
	if len(paths) != before+1 || !strings.HasSuffix(paths[before], "1@2x.png") {
		t.Fatalf("png variant: %v", paths)
	}
	// a degraded render is served (200, no-store) but never cached: asked twice, rendered twice
	for i := range 2 {
		before := len(paths)
		r, b := get("/styles/outdoors/256/4/2/1.png")
		if r.StatusCode != 200 || string(b) != "\x89PNG partial" || r.Header.Get("Cache-Control") != "no-store" ||
			r.Header.Get(tiles.DegradedHeader) != "1" || len(paths) != before+1 {
			t.Fatalf("degraded %d: %d %q %v %v", i, r.StatusCode, b, r.Header, paths)
		}
	}
	if r, _ := get("/styles/outdoors/256/3/2/1.gif"); r.StatusCode != 400 {
		t.Fatalf("bad format: %d", r.StatusCode)
	}
	if r, _ := get("/styles/outdoors/300/3/2/1.png"); r.StatusCode != 400 {
		t.Fatalf("bad size: %d", r.StatusCode)
	}
	if r, _ := get("/styles/nope/256/3/2/1.png"); r.StatusCode != 404 {
		t.Fatalf("unknown style: %d", r.StatusCode)
	}
	if _, c := e.stats.Totals(); c["raster"] != [2]int64{3, 7} {
		t.Fatalf("raster cache stats %v", c["raster"])
	}
}

func TestPacks(t *testing.T) {
	e := newEnv(t, true)
	c := e.login()
	_, secret := e.createKey(c, map[string]any{"label": "packs"})
	q := "&key=" + secret
	r, b := e.do(nil, "GET", "/packs/extract.pmtiles?bbox=-10,-10,10,10&minzoom=1&maxzoom=4"+q, nil, nil)
	if r.StatusCode != 200 {
		t.Fatalf("pack: %d %s", r.StatusCode, b)
	}
	rd, err := pmtiles.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	if rd.Header.MinZoom != 1 || rd.Header.MaxZoom != 4 {
		t.Fatalf("pack zooms %d-%d", rd.Header.MinZoom, rd.Header.MaxZoom)
	}
	x0, y0, _, _ := pmtiles.TileRange(pmtiles.BBox{-10, -10, 10, 10}, 4)
	if _, ok, _ := rd.Get(4, x0, y0); !ok {
		t.Fatal("tile missing in pack")
	}
	// Range on a pack (resumable download), served from cache the second time
	if r, _ := e.do(nil, "GET", "/packs/extract.pmtiles?bbox=-10,-10,10,10&minzoom=1&maxzoom=4"+q, nil, map[string]string{"Range": "bytes=0-6"}); r.StatusCode != 206 {
		t.Fatalf("pack range: %d", r.StatusCode)
	}
	if _, cs := e.stats.Totals(); cs["pack"] != [2]int64{1, 1} {
		t.Fatalf("pack cache stats %v", cs["pack"])
	}
	// MBTiles
	r, b = e.do(nil, "GET", "/packs/extract.pmtiles?bbox=-10,-10,10,10&minzoom=0&maxzoom=2&format=mbtiles"+q, nil, nil)
	if r.StatusCode != 200 || !bytes.HasPrefix(b, []byte("SQLite format 3")) {
		t.Fatalf("mbtiles: %d %q", r.StatusCode, b[:min(len(b), 20)])
	}
	// validation
	if r, _ := e.do(nil, "GET", "/packs/extract.pmtiles?bbox=10,10,-10,-10"+q, nil, nil); r.StatusCode != 400 {
		t.Fatalf("bad bbox: %d", r.StatusCode)
	}
}

func TestStatsFlushAndReport(t *testing.T) {
	e := newEnv(t, true)
	c := e.login()
	id, secret := e.createKey(c, map[string]any{"label": "counted"})
	for range 3 {
		e.do(nil, "GET", "/tiles/basemap/0/0/0.mvt?key="+secret, nil, nil)
	}
	e.do(nil, "GET", "/styles/outdoors.json?key="+secret, nil, nil)
	if err := e.stats.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Format("2006-01-02")
	var n int64
	if err := e.st.DB.QueryRow(`SELECT count FROM usage WHERE key_id = ? AND day = ? AND kind = 'tile'`, id, day).Scan(&n); err != nil || n != 4 {
		t.Fatalf("flushed usage %d %v", n, err)
	}
	var used *int64
	_ = e.st.DB.QueryRow(`SELECT last_used_at FROM api_keys WHERE id = ?`, id).Scan(&used)
	if used == nil {
		t.Fatal("last_used_at not recorded")
	}
	e.do(nil, "GET", "/tiles/basemap/0/0/0.mvt?key="+secret, nil, nil) // unflushed, still reported
	_, b := e.do(c, "GET", "/admin/api/stats?days=7", nil, nil)
	var rep struct {
		Daily []map[string]any `json:"daily"`
		ByKey []struct {
			KeyID    string `json:"key_id"`
			Label    string `json:"label"`
			Requests int64  `json:"requests"`
		} `json:"by_key"`
		Cache []struct {
			Name string `json:"name"`
		} `json:"cache"`
		Memory map[string]int64 `json:"memory"`
		Disk   struct {
			Generations []struct {
				ID    string `json:"id"`
				Bytes int64  `json:"bytes"`
			} `json:"generations"`
			FreeBytes int64 `json:"free_bytes"`
		} `json:"disk"`
		Processes []any `json:"processes"`
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	last := rep.Daily[len(rep.Daily)-1]
	if len(rep.Daily) != 7 || last["date"] != day || last["tiles"].(float64) != 5 || last["routes"].(float64) != 0 {
		t.Fatalf("daily: %v", rep.Daily)
	}
	if len(rep.Disk.Generations) != 1 || rep.Disk.Generations[0].ID != "g1" || rep.Disk.FreeBytes == 0 || rep.Memory["process_rss_bytes"] == 0 || len(rep.Cache) != 3 {
		t.Fatalf("stats: %s", b)
	}
	found := false
	for _, k := range rep.ByKey {
		if k.KeyID == strconv.FormatInt(id, 10) && k.Requests == 5 && k.Label == "counted" {
			found = true
		}
	}
	if !found {
		t.Fatalf("by_key: %s", b)
	}
	// key list usage
	_, kb := e.do(c, "GET", "/admin/api/keys", nil, nil)
	var kl struct {
		Keys []struct {
			Label string `json:"label"`
			Usage struct {
				Today int64   `json:"today"`
				Total int64   `json:"total"`
				Daily []int64 `json:"daily"`
			} `json:"usage"`
			Limits struct {
				RPM     *int     `json:"rpm"`
				Origins []string `json:"origins"`
			} `json:"limits"`
		} `json:"keys"`
	}
	_ = json.Unmarshal(kb, &kl)
	if len(kl.Keys) != 1 || kl.Keys[0].Usage.Today != 5 || kl.Keys[0].Usage.Total != 5 || len(kl.Keys[0].Usage.Daily) != 14 ||
		kl.Keys[0].Usage.Daily[13] != 5 || kl.Keys[0].Limits.RPM != nil || kl.Keys[0].Limits.Origins == nil {
		t.Fatalf("keys: %s", kb)
	}
}

func TestJobsBuildAndSSE(t *testing.T) {
	e := newEnv(t, false)
	c := e.login()
	if r, _ := e.do(c, "POST", "/admin/api/build", map[string]any{"refresh": true}, map[string]string{"Origin": e.ts.URL}); r.StatusCode != 503 {
		t.Fatalf("build without pipeline: %d", r.StatusCode)
	}
	release := make(chan struct{})
	e.jobs.Register("build", func(ctx context.Context, j *jobs.Job) error {
		var p struct{ Refresh bool }
		_ = j.Decode(&p)
		j.Stage("download")
		j.Progress(0.5, "halfway")
		j.Logf("refresh=%v", p.Refresh)
		<-release
		return nil
	})

	// subscribe to events before starting
	req, _ := http.NewRequest("GET", e.ts.URL+"/admin/api/events", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("sse content type %q", resp.Header.Get("Content-Type"))
	}
	events := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				ev = v
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				events <- ev + " " + v
			}
		}
	}()

	r, b := e.do(c, "POST", "/admin/api/build", map[string]any{"refresh_downloads": true}, map[string]string{"Origin": e.ts.URL})
	if r.StatusCode != 202 || !strings.Contains(string(b), `"refresh_downloads":true`) || !strings.Contains(string(b), `"name":"guard","status":"pending"`) {
		t.Fatalf("build: %d %s", r.StatusCode, b)
	}
	if r, _ := e.do(c, "POST", "/admin/api/build", map[string]any{}, map[string]string{"Origin": e.ts.URL}); r.StatusCode != 409 {
		t.Fatalf("second build: %d", r.StatusCode)
	}
	sawProgress, sawLog := false, false
	deadline := time.After(5 * time.Second)
	for !sawProgress || !sawLog {
		select {
		case ev := <-events:
			if strings.HasPrefix(ev, "job ") && strings.Contains(ev, `"name":"download","status":"running","progress":0.5,"detail":"halfway"`) {
				sawProgress = true
			}
			if strings.HasPrefix(ev, "log ") && strings.Contains(ev, `"msg":"refresh=true"`) && strings.Contains(ev, `"level":"info"`) {
				sawLog = true
			}
		case <-deadline:
			t.Fatalf("SSE: progress=%v log=%v", sawProgress, sawLog)
		}
	}
	close(release)
	var job jobs.JobJSON
	_ = json.Unmarshal(b, &job)
	jid, _ := strconv.ParseInt(job.ID, 10, 64)
	waitFor(t, func() bool {
		j, err := e.st.Job(context.Background(), jid)
		return err == nil && j.Status == jobs.StatusSucceeded
	})
	_, jb := e.do(c, "GET", "/admin/api/jobs/"+job.ID, nil, nil)
	var detail struct {
		jobs.JobJSON
		Log []jobs.LogJSON `json:"log"`
	}
	_ = json.Unmarshal(jb, &detail)
	if detail.Status != "succeeded" || detail.Progress != 1 || len(detail.Log) == 0 {
		t.Fatalf("job detail: %s", jb)
	}
	for _, st := range detail.Stages {
		want := "skipped"
		if st.Name == "download" {
			want = "done"
		}
		if st.Status != want {
			t.Fatalf("stage %s: %s (%s)", st.Name, st.Status, jb)
		}
	}
	if _, lb := e.do(c, "GET", "/admin/api/jobs", nil, nil); !strings.Contains(string(lb), `{"jobs":[{"id":"`+job.ID+`"`) {
		t.Fatalf("jobs list: %s", lb)
	}
	_, hb := e.do(c, "GET", "/admin/api/history", nil, nil)
	if !strings.Contains(string(hb), "build (manual) succeeded") {
		t.Fatalf("history: %s", hb)
	}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for range 100 {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestScheduleAndRegions(t *testing.T) {
	e := newEnv(t, true)
	c := e.login()
	o := map[string]string{"Origin": e.ts.URL}
	_, b := e.do(c, "GET", "/admin/api/schedule", nil, nil)
	if !strings.Contains(string(b), `"cron":"0 3 * * 0"`) || !strings.Contains(string(b), `"next_run":"`) || !strings.Contains(string(b), `"refresh_downloads":true`) {
		t.Fatalf("default schedule: %s", b)
	}
	if r, _ := e.do(c, "PUT", "/admin/api/schedule", map[string]any{"cron": "not a cron"}, o); r.StatusCode != 400 {
		t.Fatalf("bad cron: %d", r.StatusCode)
	}
	r, b := e.do(c, "PUT", "/admin/api/schedule", map[string]any{"cron": "30 4 * * *", "enabled": true, "refresh_downloads": false, "next_run": "ignored"}, o)
	if r.StatusCode != 200 || !strings.Contains(string(b), "T04:30:00Z") || !strings.Contains(string(b), `"refresh_downloads":false`) {
		t.Fatalf("set schedule: %d %s", r.StatusCode, b)
	}
	r, b = e.do(c, "PUT", "/admin/api/schedule", map[string]any{"enabled": false}, o)
	if r.StatusCode != 200 || !strings.Contains(string(b), `"next_run":null`) {
		t.Fatalf("disable: %s", b)
	}
	// regions
	r, b = e.do(c, "POST", "/admin/api/regions", map[string]any{"id": "us/delaware"}, o)
	if r.StatusCode != 201 || !strings.Contains(string(b), `"catalog_id":"us/delaware"`) || !strings.Contains(string(b), `"status":"installed"`) {
		t.Fatalf("add region: %d %s", r.StatusCode, b)
	}
	if r, _ := e.do(c, "POST", "/admin/api/regions", map[string]any{"id": "us/delaware"}, o); r.StatusCode != 409 {
		t.Fatalf("duplicate region: %d", r.StatusCode)
	}
	r, b = e.do(c, "POST", "/admin/api/regions", map[string]any{"name": "Permian Core", "bbox": []float64{-104, 31, -101, 33}}, o)
	if r.StatusCode != 201 || !strings.Contains(string(b), `"id":"custom/permian-core"`) || !strings.Contains(string(b), `"catalog_id":null`) || !strings.Contains(string(b), `"status":"pending"`) {
		t.Fatalf("add custom: %d %s", r.StatusCode, b)
	}
	r, b = e.do(c, "POST", "/admin/api/regions", map[string]any{"name": "Lease", "polygon": [][2]float64{{-103, 31}, {-102, 31}, {-102, 32}, {-103, 31}}}, o)
	if r.StatusCode != 201 || !strings.Contains(string(b), `"bbox":[-103,31,-102,32]`) || !strings.Contains(string(b), `"polygon":[[-103,31]`) {
		t.Fatalf("add polygon: %d %s", r.StatusCode, b)
	}
	if r, _ := e.do(c, "POST", "/admin/api/regions", map[string]any{"id": "../etc"}, o); r.StatusCode != 400 {
		t.Fatalf("bad id: %d", r.StatusCode)
	}
	e.do(c, "PUT", "/admin/api/schedule", map[string]any{"regions": []map[string]any{{"id": "us/delaware", "enabled": false}}}, o)
	if _, b := e.do(c, "GET", "/admin/api/schedule", nil, nil); !strings.Contains(string(b), `{"id":"us/delaware","enabled":false}`) {
		t.Fatalf("schedule regions: %s", b)
	}
	if _, b := e.do(c, "GET", "/admin/api/regions", nil, nil); !strings.HasPrefix(string(b), `{"regions":[`) {
		t.Fatalf("regions list: %s", b)
	}
	if r, _ := e.do(c, "DELETE", "/admin/api/regions/us%2Fdelaware", nil, o); r.StatusCode != 204 {
		t.Fatalf("delete encoded region: %d", r.StatusCode)
	}
	if _, b := e.do(c, "GET", "/admin/api/regions", nil, nil); strings.Contains(string(b), "delaware") {
		t.Fatalf("region not deleted: %s", b)
	}
	// generations reflect the disk, normalised for the UI
	_, b = e.do(c, "GET", "/admin/api/generations", nil, nil)
	var gl struct {
		Generations []struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Manifest struct {
				Regions []struct{ ID string } `json:"regions"`
				Sizes   map[string]int64      `json:"sizes"`
				Checks  []any                 `json:"checks"`
			} `json:"manifest"`
		} `json:"generations"`
	}
	_ = json.Unmarshal(b, &gl)
	if len(gl.Generations) != 1 || gl.Generations[0].Status != "live" || gl.Generations[0].Manifest.Regions[0].ID != "us/delaware" ||
		gl.Generations[0].Manifest.Sizes["basemap"] == 0 || gl.Generations[0].Manifest.Checks == nil {
		t.Fatalf("generations: %s", b)
	}
}

func TestGenerationSwap(t *testing.T) {
	e := newEnv(t, true)
	g2 := filepath.Join(e.dir, "generations", "g2")
	if err := os.MkdirAll(g2, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(g2, "basemap.pmtiles"))
	swapped := ""
	w := &tiles.Watcher{DataDir: e.dir, OnSwap: func(_ context.Context, _, n string) { e.tiles.Reload(); swapped = n }}
	w.Prime()
	tmp := filepath.Join(e.dir, "current.tmp")
	if err := os.Symlink(filepath.Join("generations", "g2"), tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(e.dir, "current")); err != nil {
		t.Fatal(err)
	}
	w.Check(context.Background())
	if swapped != "g2" || e.tiles.Generation() != "g2" {
		t.Fatalf("swap: %q %q", swapped, e.tiles.Generation())
	}
}

func TestUIFallback(t *testing.T) {
	e := newEnv(t, false)
	for _, p := range []string{"/admin/", "/admin/keys", "/admin/regions/us"} {
		if r, b := e.do(nil, "GET", p, nil, nil); r.StatusCode != 200 || string(b) != "<html>ui</html>" || r.Header.Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s: %d %s %v", p, r.StatusCode, b, r.Header)
		}
	}
	r, b := e.do(nil, "GET", "/admin/assets/app-123.js", nil, nil)
	if r.StatusCode != 200 || string(b) != "js" || !strings.Contains(r.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d %q %v", r.StatusCode, b, r.Header)
	}
	if r, _ := e.do(nil, "GET", "/admin/missing.js", nil, nil); r.StatusCode != 404 {
		t.Fatalf("missing asset: %d", r.StatusCode)
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, p := range []string{"/", "/admin"} {
		if r, _ := e.do(noRedirect, "GET", p, nil, nil); r.StatusCode != 302 || r.Header.Get("Location") != "/admin/" {
			t.Fatalf("%s redirect: %d %q", p, r.StatusCode, r.Header.Get("Location"))
		}
	}
	if r, _ := e.do(nil, "GET", "/nope", nil, nil); r.StatusCode != 404 {
		t.Fatalf("unknown path: %d", r.StatusCode)
	}
	if r, _ := e.do(nil, "GET", "/admin/api/nope", nil, nil); r.StatusCode != 401 {
		t.Fatalf("unknown admin path unauthenticated: %d", r.StatusCode)
	}
}
