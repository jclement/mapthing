package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
