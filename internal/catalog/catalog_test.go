package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		w.Write([]byte(testIndex))
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
	os.Chtimes(c.indexPath(), old, old)
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
	os.MkdirAll(filepath.Join(dir, "catalog"), 0o755)
	p := filepath.Join(dir, "catalog", "geofabrik-index-v1.json")
	os.WriteFile(p, []byte(testIndex), 0o644)
	old := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(p, old, old)
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
	c.Load(context.Background())
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
	c.Load(context.Background())
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
			if r.ID != "custom/my-lease-area" || r.Custom.Parent.ID != tt.parent {
				t.Errorf("got id=%s parent=%s", r.ID, r.Custom.Parent.ID)
			}
		})
	}
	if _, err := c.CustomRegion("x", &BBox{1, 1, 0, 0}, nil); err == nil {
		t.Error("invalid bbox accepted")
	}
}

func TestFillSizes(t *testing.T) {
	c, hits := newTestCatalog(t)
	c.Load(context.Background())
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
	c.FillSizes(context.Background(), []string{"north-america/us/delaware"})
	if *hits != before {
		t.Error("cached size re-fetched")
	}
}

func TestIDFromURL(t *testing.T) {
	if got := IDFromURL("https://download.geofabrik.de/north-america/canada/alberta-latest.osm.pbf"); got != "north-america/canada/alberta" {
		t.Error(got)
	}
}
