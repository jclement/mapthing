package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jclement/mapthing/internal/catalog"
)

// offline fails every request: the tests must not reach the real extract host for sizes.
type offline struct{}

func (offline) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("offline (test)")
}

// testCatalog is a one-state catalog with real-looking extract URLs and no network.
func testCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	const index = `{"type":"FeatureCollection","features":[
{"type":"Feature","properties":{"id":"us/delaware","name":"Delaware","urls":{"pbf":"https://download.geofabrik.de/north-america/us/delaware-latest.osm.pbf"}},
 "geometry":{"type":"Polygon","coordinates":[[[-75.8,38.4],[-75.0,38.4],[-75.0,39.9],[-75.8,39.9],[-75.8,38.4]]]}}]}`
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "catalog"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog", "geofabrik-index-v1.json"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	c := catalog.New(dir)
	c.IndexURL = "http://127.0.0.1:1/index"
	c.Client = &http.Client{Transport: offline{}}
	if err := c.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "regions.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The file is what GET /admin/api/regions answers: an extract, a custom box and a custom
// polygon, with the fields a build has no use for.
func TestRegionsFromAdminAPIShape(t *testing.T) {
	cat := testCatalog(t)
	p := writeFile(t, `{"regions":[
 {"id":"north-america/us/delaware","name":"Delaware","catalog_id":"north-america/us/delaware","bbox":[-75.8,38.4,-75,39.9],"status":"installed",
  "extract_bytes":123,"disk_bytes":456,"data_date":"2026-11-07T20:21:02Z","downloaded_at":null,"elevation_sources":["3dep13"],"refresh":true,"added_at":"2026-11-01T00:00:00Z"},
 {"id":"custom/kent","name":"Kent County","catalog_id":null,"parent":"north-america/us/delaware","bbox":[-75.7,38.9,-75.4,39.2],"status":"pending",
  "extract_bytes":0,"disk_bytes":0,"data_date":null,"downloaded_at":null,"elevation_sources":[],"parents":[{"id":"north-america/us/delaware"}],"refresh":false,"added_at":"2026-11-01T00:00:00Z"},
 {"id":"custom/wedge","name":"Wedge","catalog_id":null,"bbox":[-75.7,38.5,-75.2,38.9],"polygon":[[-75.7,38.5],[-75.2,38.5],[-75.45,38.9]],"refresh":true}
]}`)
	rs, skip, err := regionsFromFile(t.Context(), cat, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 {
		t.Fatalf("regions %+v", rs)
	}
	de, kent, wedge := rs[0], rs[1], rs[2]
	if de.ID != "north-america/us/delaware" || de.Custom != nil || !strings.HasSuffix(de.PBFURL, "delaware-latest.osm.pbf") {
		t.Errorf("extract: %+v", de)
	}
	if kent.ID != "custom/kent" || kent.Name != "Kent County" || kent.Custom == nil || len(kent.Custom.Parents) != 1 ||
		kent.Custom.Parents[0].ID != "north-america/us/delaware" || kent.BBox != (catalog.BBox{-75.7, 38.9, -75.4, 39.2}) {
		t.Errorf("custom box: %+v", kent)
	}
	if wedge.ID != "custom/wedge" || wedge.Custom == nil || len(wedge.Polygon) != 1 || len(wedge.Polygon[0]) < 3 {
		t.Errorf("custom polygon: %+v", wedge)
	}
	if wedge.BBox[3] != 38.9 || wedge.BBox[0] != -75.7 {
		t.Errorf("polygon bbox %v", wedge.BBox)
	}
	if len(skip) != 1 || !skip["custom/kent"] {
		t.Errorf("refresh:false not carried: %v", skip)
	}
}

// Written by hand: a bare array, an extract by id alone, a custom area by name and box.
func TestRegionsFromHandWrittenFile(t *testing.T) {
	cat := testCatalog(t)
	rs, skip, err := regionsFromFile(t.Context(), cat, writeFile(t, `[
 {"id":"north-america/us/delaware"},
 {"name":"Dover Area","bbox":[-75.6,39.1,-75.4,39.2]},
 {"name":"Tri","polygon":{"type":"Polygon","coordinates":[[[-75.7,38.5],[-75.2,38.5],[-75.45,38.9],[-75.7,38.5]]]}}
]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 || rs[0].ID != "north-america/us/delaware" || rs[1].ID != "custom/dover-area" || rs[1].Custom == nil || rs[2].ID != "custom/tri" || len(skip) != 0 {
		t.Fatalf("regions %+v skip %v", rs, skip)
	}
}

func TestRegionsFileErrors(t *testing.T) {
	cat := testCatalog(t)
	for body, want := range map[string]string{
		`{"regions":[]}`:         "no regions",
		`{"other":1}`:            `want {"regions":[…]}`,
		`not json`:               "invalid character",
		`[{"id":"us/atlantis"}]`: "us/atlantis",
		`[{"id":"north-america/us/delaware"},{"id":"north-america/us/delaware"}]`: "listed twice",
		`[{"name":"Far","bbox":[10,10,11,11]}]`:                                   "no extract",
		`[{"name":"Bad","bbox":[-75,39,-76,38]}]`:                                 "w<e",
		`[{"name":"Short","bbox":[-75,39]}]`:                                      "[w,s,e,n]",
		`[{"bbox":[-75.6,39.1,-75.4,39.2]}]`:                                      "needs a name",
		`[{"name":"Two","polygon":[[-75.7,38.5],[-75.2,38.5]]}]`:                  "at least 3 points",
		`[{"name":"P","polygon":{"type":"Point","coordinates":[]}}]`:              "polygon must be",
		`[{}]`: "needs an id",
	} {
		_, _, err := regionsFromFile(t.Context(), cat, writeFile(t, body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", body, err, want)
		}
	}
	if _, _, err := regionsFromFile(t.Context(), cat, filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Errorf("missing file accepted")
	}
}
