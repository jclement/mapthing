package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jclement/mapthing/internal/catalog"
	"github.com/jclement/mapthing/internal/config"
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

// The build job offers the live generation's terrain for reuse, unless told to rebuild it.
func TestBuildOptionsReuseLiveTerrain(t *testing.T) {
	data := t.TempDir()
	f := &features{cfg: config.Config{DataDir: data}}
	regions := []catalog.Region{{ID: "x"}}
	skip := map[string]bool{"x": true}
	if o := f.buildOptions(regions, skip, true, false, nil); o.ReuseTerrain != "" || !o.Refresh || len(o.Regions) != 1 || !o.SkipRefresh["x"] {
		t.Fatalf("no generation yet: %+v", o)
	}
	gen := filepath.Join(data, "generations", "20261101T030000Z")
	if err := os.MkdirAll(gen, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("generations", "20261101T030000Z"), filepath.Join(data, "current")); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(gen)
	got, _ := filepath.EvalSymlinks(f.buildOptions(regions, nil, false, false, nil).ReuseTerrain)
	if got != want {
		t.Errorf("ReuseTerrain = %q, want the live generation %q", got, want)
	}
	if o := f.buildOptions(regions, nil, false, true, nil); o.ReuseTerrain != "" {
		t.Errorf("rebuild_terrain still offers %q", o.ReuseTerrain)
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]string{
		"valhalla_service 3.9.1\n":                         "3.9.1",
		"openjdk version \"21.0.8\" 2026-07-15\nOpenJDK …": "21.0.8",
		"osmium version 1.16.0\nlibosmium version 2.20.0":  "1.16.0",
		"GDAL 3.8.4, released 2024/02/08":                  "3.8.4",
		"v24.11.1":                                         "24.11.1",
		"":                                                 "",
		"no numbers here":                                  "",
	} {
		if got := parseVersion(in); got != want {
			t.Errorf("parseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every MAPTHING_* variable the code reads is either on the Settings page or known to be
// internal, so a new setting cannot be added without appearing there.
func TestEveryEnvVarIsDescribed(t *testing.T) {
	re := regexp.MustCompile(`"(MAPTHING_[A-Z0-9_]+)"`)
	seen := map[string]string{}
	for _, root := range []string{"../../internal", "../../cmd"} {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil //nolint:nilerr // skip what cannot be read
			}
			b, _ := os.ReadFile(p)
			for _, m := range re.FindAllSubmatch(b, -1) {
				seen[string(m[1])] = p
			}
			return nil
		})
	}
	if len(seen) < 30 {
		t.Fatalf("found only %d variables; is the scan looking in the right place?", len(seen))
	}
	for name, where := range seen {
		if strings.HasSuffix(name, "_") || strings.HasPrefix(name, "MAPTHING_TEST_") || strings.HasPrefix(name, "MAPTHING_FAKE_") {
			continue // prefixes, and test hooks
		}
		if !config.KnownVar(name) {
			t.Errorf("%s (read in %s) is not on the Settings page: add it to internal/config/settings.go", name, where)
		}
	}
}

// A scheduled build refreshes only the regions that are due: every other extract, and every
// parent extract nobody due is clipped from, is left as downloaded.
func TestSkipAllBut(t *testing.T) {
	tx := catalog.Region{ID: "us/texas"}
	nm := catalog.Region{ID: "us/new-mexico"}
	la := catalog.Region{ID: "us/louisiana"}
	regions := []catalog.Region{
		tx,
		{ID: "us/alberta"},
		{ID: "custom/permian", Custom: &catalog.Custom{Parents: []catalog.Region{tx, nm}}},
		{ID: "custom/haynesville", Custom: &catalog.Custom{Parents: []catalog.Region{la, tx}}},
	}
	keys := func(m map[string]bool) string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	// Only the Permian box is due: its two parents are refreshed, nothing else. Texas itself
	// is the same extract as the parent, so it is refreshed with it.
	if got := keys(skipAllBut(regions, []string{"custom/permian"})); got != "custom/haynesville,us/alberta,us/louisiana" {
		t.Errorf("permian due: skip %s", got)
	}
	// Only Alberta is due.
	if got := keys(skipAllBut(regions, []string{"us/alberta"})); got != "custom/haynesville,custom/permian,us/louisiana,us/new-mexico,us/texas" {
		t.Errorf("alberta due: skip %s", got)
	}
	// Nothing due (a rebuild from what is there): everything is skipped.
	if got := skipAllBut(regions, nil); len(got) != 6 {
		t.Errorf("nothing due: skip %s", keys(got))
	}
	// Everything due: nothing is skipped.
	if got := skipAllBut(regions, []string{"us/texas", "us/alberta", "custom/permian", "custom/haynesville"}); len(got) != 0 {
		t.Errorf("all due: skip %s", keys(got))
	}
}
