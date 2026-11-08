package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A tiny index in Geofabrik's shape: a continent, a country, a state.
const testIndex = `{"type":"FeatureCollection","features":[
{"type":"Feature","properties":{"id":"north-america","name":"North America","urls":{"pbf":"https://download.geofabrik.de/north-america-latest.osm.pbf"}},
 "geometry":{"type":"Polygon","coordinates":[[[-170,10],[-50,10],[-50,85],[-170,85],[-170,10]]]}},
{"type":"Feature","properties":{"id":"us","parent":"north-america","name":"United States of America","urls":{"pbf":"https://download.geofabrik.de/north-america/us-latest.osm.pbf","updates":"https://download.geofabrik.de/north-america/us-updates"}},
 "geometry":{"type":"MultiPolygon","coordinates":[[[[-125,24],[-66,24],[-66,50],[-125,50],[-125,24]]]]}},
{"type":"Feature","properties":{"id":"us/delaware","parent":"us","name":"Delaware","urls":{"pbf":"https://download.geofabrik.de/north-america/us/delaware-latest.osm.pbf","updates":"https://download.geofabrik.de/north-america/us/delaware-updates"}},
 "geometry":{"type":"Polygon","coordinates":[[[-75.8,38.4],[-75.0,38.4],[-75.0,39.9],[-75.8,39.9],[-75.8,38.4]],[[-75.5,39.0],[-75.4,39.0],[-75.4,39.1],[-75.5,39.1],[-75.5,39.0]]]}},
{"type":"Feature","properties":{"id":"europe","name":"Europe","urls":{"pbf":"https://download.geofabrik.de/europe-latest.osm.pbf"}},
 "geometry":{"type":"Polygon","coordinates":[[[-30,30],[60,30],[60,80],[-30,80],[-30,30]]]}}
]}`

func newTestCatalog(t *testing.T) (*Catalog, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "12345")
			return
		}
		_, _ = w.Write([]byte(testIndex))
	}))
	t.Cleanup(srv.Close)
	c := New(t.TempDir())
	c.IndexURL = srv.URL
	c.Client = srv.Client()
	return c, &hits
}

func TestLoadParsesAndCaches(t *testing.T) {
	c, hits := newTestCatalog(t)
	if err := c.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *hits != 1 {
		t.Fatalf("hits=%d", *hits)
	}
	e, ok := c.Get("north-america/us/delaware")
	if !ok {
		t.Fatal("delaware missing")
	}
	if e.Parent != "north-america/us" || e.Name != "Delaware" {
		t.Errorf("entry %+v", e)
	}
	if e.BBox != (BBox{-75.8, 38.4, -75.0, 39.9}) {
		t.Errorf("bbox %v", e.BBox)
	}
	if r := e.Region(); len(r.Polygon) != 2 || len(r.Polygon[0]) != 5 {
		t.Errorf("region polygon %v", r.Polygon)
	}
	if !strings.HasSuffix(e.UpdatesURL, "delaware-updates") {
		t.Errorf("updates %q", e.UpdatesURL)
	}
	// Second load uses the cache.
	c2 := New(c.DataDir)
	c2.IndexURL = c.IndexURL
	if err := c2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *hits != 1 {
		t.Fatalf("cache not used, hits=%d", *hits)
	}
	// An old cache is refreshed.
	old := time.Now().Add(-8 * 24 * time.Hour)
	_ = os.Chtimes(c.indexPath(), old, old)
	c2.Client = c.Client
	if err := c2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *hits != 2 {
		t.Fatalf("stale cache not refreshed, hits=%d", *hits)
	}
}

func TestStaleCacheSurvivesFetchFailure(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "catalog"), 0o755)
	p := filepath.Join(dir, "catalog", "geofabrik-index-v1.json")
	_ = os.WriteFile(p, []byte(testIndex), 0o644)
	old := time.Now().Add(-30 * 24 * time.Hour)
	_ = os.Chtimes(p, old, old)
	c := New(dir)
	c.IndexURL = "http://127.0.0.1:1/nope"
	if err := c.Load(context.Background()); err != nil {
		t.Fatalf("expected stale cache fallback, got %v", err)
	}
	if _, ok := c.Get("north-america/us"); !ok {
		t.Fatal("entry missing")
	}
}

func TestEntriesFilter(t *testing.T) {
	c, _ := newTestCatalog(t)
	_ = c.Load(context.Background())
	c.Coverage = func(b BBox) []string { return []string{"test-dem"} }
	na := c.Entries(Filter{})
	if len(na) != 3 {
		t.Fatalf("north america entries=%d", len(na))
	}
	if na[0].ElevationSources[0] != "test-dem" {
		t.Errorf("coverage not applied")
	}
	if all := c.Entries(Filter{All: true}); len(all) != 4 {
		t.Fatalf("all=%d", len(all))
	}
}

func TestCustomRegion(t *testing.T) {
	c, _ := newTestCatalog(t)
	_ = c.Load(context.Background())
	tests := []struct {
		name    string
		bbox    *BBox
		poly    [][][2]float64
		parent  string
		wantErr error
	}{
		{"inside delaware", &BBox{-75.7, 38.5, -75.6, 38.6}, nil, "north-america/us/delaware", nil},
		{"crosses delaware edge", &BBox{-76.5, 38.5, -75.6, 38.6}, nil, "north-america/us", nil},
		{"polygon in delaware", nil, [][][2]float64{{{-75.7, 38.5}, {-75.6, 38.5}, {-75.6, 38.6}, {-75.7, 38.5}}}, "north-america/us/delaware", nil},
		{"corner in delaware hole", &BBox{-75.45, 39.05, -75.1, 39.5}, nil, "north-america/us", nil},
		{"ocean", &BBox{-40, 0, -39, 1}, nil, "", ErrNotCovered},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := c.CustomRegion("My Lease Area", tt.bbox, tt.poly)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err=%v want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if len(r.Polygon) != 1 || len(r.Polygon[0]) < 4 {
				t.Errorf("custom polygon %v", r.Polygon)
			}
			if r.ID != "custom/my-lease-area" || len(r.Custom.Parents) != 1 || r.Custom.Parents[0].ID != tt.parent {
				t.Errorf("got id=%s parents=%v", r.ID, r.Custom.Parents)
			}
		})
	}
	if _, err := c.CustomRegion("x", &BBox{1, 1, 0, 0}, nil); err == nil {
		t.Error("invalid bbox accepted")
	}
}

func TestFillSizes(t *testing.T) {
	c, hits := newTestCatalog(t)
	_ = c.Load(context.Background())
	// Point delaware's URL at the test server.
	c.mu.Lock()
	srvURL := c.IndexURL
	c.entries[c.byID["north-america/us/delaware"]].PBFURL = srvURL + "/de.pbf"
	c.mu.Unlock()
	if err := c.FillSizes(context.Background(), []string{"north-america/us/delaware"}); err != nil {
		t.Fatal(err)
	}
	e, _ := c.Get("north-america/us/delaware")
	if e.Size != 12345 {
		t.Fatalf("size=%d", e.Size)
	}
	before := *hits
	_ = c.FillSizes(context.Background(), []string{"north-america/us/delaware"})
	if *hits != before {
		t.Error("cached size re-fetched")
	}
}

func TestIDFromURL(t *testing.T) {
	if got := IDFromURL("https://download.geofabrik.de/north-america/canada/alberta-latest.osm.pbf"); got != "north-america/canada/alberta" {
		t.Error(got)
	}
}

// Geofabrik answers HEAD with 307 → the dated file → 200 + Content-Length. WarmSizes must follow
// that, cover every extract (not just one), persist sizes.json and reload it.
func TestWarmSizesPersists(t *testing.T) {
	var heads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "-latest.osm.pbf"):
			http.Redirect(w, r, strings.Replace(r.URL.Path, "-latest", "-261107", 1), http.StatusTemporaryRedirect)
		case r.Method == http.MethodHead:
			heads.Add(1)
			w.Header().Set("Content-Length", strconv.Itoa(1000+len(r.URL.Path)))
		default:
			_, _ = w.Write([]byte(testIndex))
		}
	}))
	defer srv.Close()
	// Every request (including the index's real download.geofabrik.de URLs) goes to srv.
	client := &http.Client{Transport: rewriteTransport{srv.URL}}
	dir := t.TempDir()
	c := New(dir)
	c.IndexURL, c.Client = srv.URL, client
	if err := c.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.WarmSizes(t.Context(), Filter{}); err != nil {
		t.Fatal(err)
	}
	if got := heads.Load(); got != 3 { // north-america, us, us/delaware
		t.Errorf("HEADs = %d, want 3", got)
	}
	c2 := New(dir)
	c2.IndexURL = srv.URL
	if err := c2.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, e := range c2.Entries(Filter{}) {
		if e.Size <= 1000 {
			t.Errorf("%s size %d after reload", e.ID, e.Size)
		}
	}
	// Fresh sizes are not fetched again.
	if err := c2.WarmSizes(t.Context(), Filter{}); err != nil {
		t.Fatal(err)
	}
	if got := heads.Load(); got != 3 {
		t.Errorf("cached sizes re-fetched (%d HEADs)", got)
	}
}

type rewriteTransport struct{ base string }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u, err := url.Parse(rt.base)
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host, r.Host = u.Scheme, u.Host, u.Host
	return http.DefaultTransport.RoundTrip(r)
}

func TestParseIndexUSStates(t *testing.T) {
	idx := `{"features":[
{"properties":{"id":"north-america","name":"North America","urls":{"pbf":"https://download.geofabrik.de/north-america-latest.osm.pbf"}}},
{"properties":{"id":"us","parent":"north-america","name":"United States of America","urls":{"pbf":"https://download.geofabrik.de/north-america/us-latest.osm.pbf"}}},
{"properties":{"id":"us/new-mexico","parent":"north-america","name":"us/new-mexico","urls":{"pbf":"https://download.geofabrik.de/north-america/us/new-mexico-latest.osm.pbf"}}},
{"properties":{"id":"alberta","parent":"canada","name":"Alberta","urls":{"pbf":"https://download.geofabrik.de/north-america/canada/alberta-latest.osm.pbf"}}},
{"properties":{"id":"canada","parent":"north-america","name":"Canada","urls":{"pbf":"https://download.geofabrik.de/north-america/canada-latest.osm.pbf"}}}
]}`
	es, err := parseIndex([]byte(idx))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Entry{}
	for _, e := range es {
		got[e.ID] = e
	}
	nm := got["north-america/us/new-mexico"]
	if nm.Name != "New Mexico" || nm.Parent != "north-america/us" {
		t.Errorf("new mexico: %q under %q", nm.Name, nm.Parent)
	}
	if ab := got["north-america/canada/alberta"]; ab.Name != "Alberta" || ab.Parent != "north-america/canada" {
		t.Errorf("alberta: %q under %q", ab.Name, ab.Parent)
	}
	if us := got["north-america/us"]; us.Parent != "north-america" {
		t.Errorf("us parent %q", us.Parent)
	}
}
