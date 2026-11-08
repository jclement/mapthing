package elevation

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealArchive runs the HTTP API against a real terrain archive built by
// mt-terrain. Opt-in: MAPTHING_TEST_TERRAIN=path/to/terrain.pmtiles and
// MAPTHING_TEST_POINTS="lon,lat,expected_m;…" (expected values from the source
// raster; tolerance 0.5 m).
func TestRealArchive(t *testing.T) {
	arc := os.Getenv("MAPTHING_TEST_TERRAIN")
	if arc == "" {
		t.Skip("MAPTHING_TEST_TERRAIN not set")
	}
	dir := t.TempDir()
	abs, _ := filepath.Abs(arc)
	gen := filepath.Join(dir, "generations", "g")
	_ = os.MkdirAll(gen, 0o755)
	if err := os.Symlink(abs, filepath.Join(gen, "terrain.pmtiles")); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(gen, filepath.Join(dir, "current"))
	mux, svc := newMuxFor(t, dir)
	defer func() { _ = svc.Close() }()
	var locs []string
	var want []float64
	for _, p := range strings.Split(os.Getenv("MAPTHING_TEST_POINTS"), ";") {
		f := strings.Split(p, ",")
		if len(f) != 3 {
			continue
		}
		locs = append(locs, f[1]+","+f[0])
		var v float64
		if _, err := fmtSscan(f[2], &v); err != nil {
			t.Fatal(err)
		}
		want = append(want, v)
	}
	rr, body := get(t, mux, "/maps/api/elevation/json?locations="+strings.Join(locs, "|"))
	if rr.Code != 200 || body["status"] != "OK" {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	for i, r := range body["results"].([]any) {
		e := r.(map[string]any)["elevation"].(float64)
		t.Logf("%s: %.2f (want %.2f)", locs[i], e, want[i])
		if math.Abs(e-want[i]) > 0.5 {
			t.Errorf("%s: %.2f want %.2f", locs[i], e, want[i])
		}
	}
	// A path profile across the archive.
	rr, body = get(t, mux, "/maps/api/elevation/json?path="+strings.Join(locs, "|")+"&samples=100")
	if rr.Code != 200 || len(body["results"].([]any)) != 100 {
		t.Fatalf("path: %d %s", rr.Code, rr.Body.String())
	}
}
