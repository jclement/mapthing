package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jclement/mapthing/internal/catalog"
	"github.com/jclement/mapthing/internal/store"
)

func TestGeojsonPolygonShapes(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		rings int
		pts   int
		bad   bool
	}{
		// Exactly what the admin API stores in regions.polygon (polygonRing output).
		{"stored bare ring", `[[-94.5,31.8],[-93.3,31.8],[-93.3,32.8],[-94.5,32.8]]`, 1, 4, false},
		{"geojson polygon", `{"type":"Polygon","coordinates":[[[-94.5,31.8],[-93.3,31.8],[-93.3,32.8],[-94.5,31.8]]]}`, 1, 4, false},
		{"feature", `{"type":"Feature","geometry":{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,0]]]}}`, 1, 4, false},
		{"multipolygon first part", `{"type":"MultiPolygon","coordinates":[[[[0,0],[1,0],[1,1],[0,0]]],[[[5,5],[6,5],[6,6],[5,5]]]]}`, 1, 4, false},
		{"null", `null`, 0, 0, false},
		{"two-point ring", `[[0,0],[1,1]]`, 0, 0, true},
		{"point", `{"type":"Point","coordinates":[0,0]}`, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := geojsonPolygon(json.RawMessage(tt.raw))
			if (err != nil) != tt.bad {
				t.Fatalf("err = %v", err)
			}
			if len(p) != tt.rings || (tt.rings > 0 && len(p[0]) != tt.pts) {
				t.Errorf("got %v", p)
			}
		})
	}
}

// A polygon region as the store holds it must resolve into a build region (it used to fail
// every build with "cannot unmarshal array").
func TestStoreRegionBareRing(t *testing.T) {
	cat := testCatalog(t)
	rg := &store.Region{ID: "custom/kent", Name: "Kent", Kind: "custom",
		BBox:    []float64{-75.7, 38.9, -75.4, 39.2},
		Polygon: json.RawMessage(`[[-75.7,38.9],[-75.4,38.9],[-75.4,39.2],[-75.7,39.2]]`)}
	r, err := storeRegion(t.Context(), cat, rg)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "custom/kent" || r.Custom == nil || len(r.Polygon) != 1 || len(r.Polygon[0]) < 4 {
		t.Errorf("region %+v", r)
	}
}

// testCatalog is a one-state catalog whose URLs point at a closed port (size lookups fail fast).
func testCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	const index = `{"type":"FeatureCollection","features":[
{"type":"Feature","properties":{"id":"us/delaware","name":"Delaware","urls":{"pbf":"http://127.0.0.1:1/north-america/us/delaware-latest.osm.pbf"}},
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
	if err := c.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGeojsonPolygon(t *testing.T) {
	cases := map[string]int{ // input → rings
		`[[-103,31],[-102,31],[-102,32],[-103,31]]`:                                                  1, // admin API's bare ring
		`{"type":"Polygon","coordinates":[[[-103,31],[-102,31],[-102,32],[-103,31]]]}`:               1,
		`{"type":"Feature","geometry":{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,0]]]}}`: 1,
		`null`: 0,
	}
	for in, want := range cases {
		got, err := geojsonPolygon(json.RawMessage(in))
		if err != nil || len(got) != want {
			t.Errorf("%s: %d rings, %v", in, len(got), err)
		}
	}
	if _, err := geojsonPolygon(json.RawMessage(`{"type":"Point","coordinates":[0,0]}`)); err == nil {
		t.Error("point accepted")
	}
}

func TestCatalogJSON(t *testing.T) {
	out := catalogJSON([]catalog.Entry{
		{ID: "north-america", Name: "North America"},
		{ID: "north-america/us/delaware", Name: "Delaware", Parent: "north-america/us", PBFURL: "https://x/delaware.osm.pbf", Size: 25 << 20,
			BBox: catalog.BBox{-75.8, 38.4, -75, 39.9}, ElevationSources: []string{"3dep13", "glo30"}},
	})
	b, _ := json.Marshal(out)
	s := string(b)
	for _, want := range []string{
		`"entries":[{"id":"north-america","name":"North America","parent":null,`,
		`"extract_bytes":0,"downloadable":false,"elevation_sources":[]}`,
		`"downloadable":true`,
		`"parent":"north-america/us"`, `"extract_bytes":26214400`, `"elevation_sources":["3dep13","glo30"]`,
		`"elevation_sources":[{"id":"hrdem"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
}
