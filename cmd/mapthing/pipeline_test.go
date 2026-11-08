package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jclement/mapthing/internal/catalog"
)

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
