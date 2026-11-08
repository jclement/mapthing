package elevation

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jclement/mapthing/internal/elevation/pmtiles"
)

const testZ = 12

// plane is the synthetic terrain: linear in world-pixel coordinates (so
// bilinear interpolation must reproduce it exactly), defined at pixel centres.
var planeX0, planeY0 = LonLatToWorldPixel(-104, 32, testZ)

func plane(px, py float64) float64 { return 500 + 0.25*(px-planeX0) - 0.1*(py-planeY0) }

// writeTestArchive writes tiles covering bbox at testZ with plane(). Pixels
// west of noDataLon have no data; pixels north of src2Lat come from source 2.
func writeTestArchive(t *testing.T, bbox BBox, noDataLon, src2Lat float64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "terrain.pmtiles")
	w, err := pmtiles.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	x0, y0, x1, y1 := TileRange(testZ, bbox[0], bbox[1], bbox[2], bbox[3])
	for ty := y0; ty <= y1; ty++ {
		for tx := x0; tx <= x1; tx++ {
			elev := make([]float32, TileSize*TileSize)
			src := make([]uint8, TileSize*TileSize)
			for j := 0; j < TileSize; j++ {
				for i := 0; i < TileSize; i++ {
					px, py := float64(tx*TileSize+i)+0.5, float64(ty*TileSize+j)+0.5
					lon, lat := WorldPixelToLonLat(px, py, testZ)
					k := j*TileSize + i
					switch {
					case lon < noDataLon:
						elev[k], src[k] = float32(math.NaN()), 0
					case lat > src2Lat:
						elev[k], src[k] = float32(plane(px, py)), 2
					default:
						elev[k], src[k] = float32(plane(px, py)), 1
					}
				}
			}
			data, err := EncodeTile(elev, src)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Add(testZ, uint32(tx), uint32(ty), data); err != nil {
				t.Fatal(err)
			}
		}
	}
	meta := Metadata{Name: "test", Format: "png", Encoding: "terrarium", MaxZoom: testZ, Bounds: bbox,
		Sources: []MetaSource{
			{SourceInfo: SourceInfo{ID: "3dep13", ResolutionM: 10}},
			{SourceInfo: SourceInfo{ID: "glo30", ResolutionM: 30}},
		}}
	if err := w.Finish(pmtiles.Header{TileType: pmtiles.TileTypePNG, TileCompression: pmtiles.CompressionNone, MinZoom: testZ, MaxZoom: testZ}, meta); err != nil {
		t.Fatal(err)
	}
	return path
}

var testBox = BBox{-104.2, 31.8, -103.8, 32.2}

func TestBilinearOnPlane(t *testing.T) {
	path := writeTestArchive(t, testBox, -200, 90)
	a, err := OpenArchive(path, 8) // small cache: exercise eviction
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	for _, p := range [][2]float64{{-104.0, 32.0}, {-103.91234, 31.95678}, {-104.15, 32.13}} {
		s, err := a.Sample(p[0], p[1])
		if err != nil {
			t.Fatal(err)
		}
		px, py := LonLatToWorldPixel(p[0], p[1], testZ)
		want := plane(px, py)
		if s.Elevation == nil || math.Abs(*s.Elevation-want) > 0.011 { // 1/256 m quantisation + cm rounding
			t.Errorf("%v: got %v want %.3f", p, s.Elevation, want)
		}
		if s.Source != "3dep13" || s.Resolution < 10 {
			t.Errorf("%v: source %q res %v", p, s.Source, s.Resolution)
		}
	}
	// A point exactly on a tile corner reads across four tiles.
	x0, y0, _, _ := TileRange(testZ, -104, 32, -104, 32)
	lon, lat := WorldPixelToLonLat(float64(x0*TileSize), float64(y0*TileSize), testZ)
	s, _ := a.Sample(lon, lat)
	if s.Elevation == nil || math.Abs(*s.Elevation-plane(float64(x0*TileSize), float64(y0*TileSize))) > 0.011 {
		t.Errorf("tile corner: got %v", s.Elevation)
	}
	// Outside the archive: no data, not an error.
	s, err = a.Sample(10, 10)
	if err != nil || s.Elevation != nil || s.Source != "" {
		t.Errorf("outside: %+v %v", s, err)
	}
}

func TestNoDataAndSources(t *testing.T) {
	path := writeTestArchive(t, testBox, -104.0, 32.1)
	a, err := OpenArchive(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	if s, _ := a.Sample(-104.1, 32.0); s.Elevation != nil {
		t.Errorf("nodata area returned %v", *s.Elevation)
	}
	if s, _ := a.Sample(-103.9, 32.15); s.Source != "glo30" || s.Resolution < 30 {
		t.Errorf("north part: %+v", s)
	}
	// On the nodata edge, interpolation uses only valid neighbours: the value
	// must stay within the plane's local range rather than being pulled to 0.
	px, _ := LonLatToWorldPixel(-104.0, 32.0, testZ)
	lon, _ := WorldPixelToLonLat(math.Floor(px)+0.5, 0, testZ)
	s, _ := a.Sample(lon, 32.0)
	if s.Elevation == nil || *s.Elevation < 100 {
		t.Errorf("edge sample = %v", s.Elevation)
	}
}

func TestDensify(t *testing.T) {
	path := []LonLat{{-104, 32}, {-104, 32.01}, {-103.99, 32.01}}
	pts, dist := Densify(path, 100, 0)
	l1, l2 := HaversineM(path[0], path[1]), HaversineM(path[1], path[2])
	if got := dist[len(dist)-1]; math.Abs(got-(l1+l2)) > 1e-6 {
		t.Errorf("length %v want %v", got, l1+l2)
	}
	for i := 1; i < len(dist); i++ {
		if dist[i]-dist[i-1] > 100+1e-9 || dist[i] <= dist[i-1] {
			t.Fatalf("gap %v at %d", dist[i]-dist[i-1], i)
		}
	}
	if pts[0] != path[0] || pts[len(pts)-1] != path[2] {
		t.Errorf("ends not kept")
	}
	// The middle vertex must be present.
	found := false
	for _, p := range pts {
		found = found || p == path[1]
	}
	if !found {
		t.Errorf("vertex dropped")
	}
	// Google semantics: exactly N, ends included, evenly spaced.
	pts, dist = Densify(path, 0, 5)
	if len(pts) != 5 || pts[0] != path[0] || pts[4] != path[2] {
		t.Fatalf("samples=5: %v", pts)
	}
	for i := 1; i < 5; i++ {
		if math.Abs((dist[i]-dist[i-1])-(l1+l2)/4) > 1e-6 {
			t.Errorf("uneven spacing %v", dist)
		}
	}
	if pts, _ := Densify(path[:1], 0, 3); len(pts) != 3 || pts[2] != path[0] {
		t.Errorf("single point path: %v", pts)
	}
}

func TestGainLoss(t *testing.T) {
	ramp := []float64{}
	for i := 0; i <= 100; i++ {
		ramp = append(ramp, 100+float64(i)) // climb 100 m
	}
	for i := 1; i <= 40; i++ {
		ramp = append(ramp, 200-float64(i)) // descend 40 m
	}
	g, l := GainLoss(ramp, 0)
	if g != 100 || l != 40 {
		t.Errorf("raw ramp gain/loss = %v/%v", g, l)
	}
	g, l = GainLoss(ramp, 2)
	if g != 100 || l != 40 {
		t.Errorf("hysteresis ramp gain/loss = %v/%v", g, l)
	}
	// Flat with ±0.5 m noise: raw counts the noise, hysteresis does not.
	noisy := []float64{}
	for i := 0; i < 200; i++ {
		noisy = append(noisy, 300+0.5*float64(i%2))
	}
	g, l = GainLoss(noisy, 0)
	if g < 49 || l < 49 {
		t.Errorf("raw noisy = %v/%v", g, l)
	}
	g, l = GainLoss(noisy, 2)
	if g != 0 || l != 0 {
		t.Errorf("smoothed noisy = %v/%v", g, l)
	}
	// Noisy ramp: smoothed stays within a threshold of the true 50 m.
	nr := []float64{}
	for i := 0; i <= 500; i++ {
		nr = append(nr, float64(i)*0.1+0.8*math.Sin(float64(i)*2.1))
	}
	g, _ = GainLoss(nr, 2)
	if math.Abs(g-50) > 2.5 {
		t.Errorf("noisy ramp smoothed gain = %v, want ≈50", g)
	}
}

func TestPolyline(t *testing.T) {
	// The example from Google's polyline documentation.
	pts, err := DecodePolyline("_p~iF~ps|U_ulLnnqC_mqNvxq`@", 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []LonLat{{-120.2, 38.5}, {-120.95, 40.7}, {-126.453, 43.252}}
	if len(pts) != 3 {
		t.Fatalf("got %v", pts)
	}
	for i := range want {
		if math.Abs(pts[i][0]-want[i][0]) > 1e-9 || math.Abs(pts[i][1]-want[i][1]) > 1e-9 {
			t.Errorf("pt %d = %v want %v", i, pts[i], want[i])
		}
	}
	if enc := EncodePolyline(want, 5); enc != "_p~iF~ps|U_ulLnnqC_mqNvxq`@" {
		t.Errorf("encode = %q", enc)
	}
	p6 := EncodePolyline(want, 6)
	back, _ := DecodePolyline(p6, 6)
	if math.Abs(back[2][1]-43.252) > 1e-9 {
		t.Errorf("precision 6 round trip %v", back)
	}
	if _, err := DecodePolyline("_p~iF~ps|U_", 5); err == nil {
		t.Errorf("truncated polyline accepted")
	}
}

func newTestHandler(t *testing.T) (*http.ServeMux, *Terrain) {
	t.Helper()
	path := writeTestArchive(t, testBox, -104.1, 90)
	dir := t.TempDir()
	gen := filepath.Join(dir, "generations", "g1")
	_ = os.MkdirAll(gen, 0o755)
	if err := os.Rename(path, filepath.Join(gen, "terrain.pmtiles")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(gen, filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	mux, svc := newMuxFor(t, dir)
	t.Cleanup(func() { _ = svc.Close() })
	return mux, svc
}

func newMuxFor(t *testing.T, dataDir string) (*http.ServeMux, *Terrain) {
	t.Helper()
	svc := NewTerrainForData(dataDir)
	mux := http.NewServeMux()
	NewHandler(svc).Mount(mux, nil)
	return mux, svc
}

func fmtSscan(s string, v *float64) (int, error) { return fmt.Sscan(s, v) }

func get(t *testing.T, mux http.Handler, u string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", u, nil))
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return rr, body
}

func TestGoogleCompat(t *testing.T) {
	mux, _ := newTestHandler(t)
	cases := []struct {
		query  string
		code   int
		status string
		n      int
	}{
		{"locations=32.0,-104.0", 200, "OK", 1},
		{"locations=32.0,-104.0|32.1,-103.9", 200, "OK", 2},
		{"locations=enc:" + url.QueryEscape(EncodePolyline([]LonLat{{-104, 32}, {-103.9, 32.1}}, 5)), 200, "OK", 2},
		{"path=32.0,-104.0|32.1,-103.9&samples=7", 200, "OK", 7},
		{"path=enc:" + url.QueryEscape(EncodePolyline([]LonLat{{-104, 32}, {-103.9, 32.1}}, 5)) + "&samples=512", 200, "OK", 512},
		{"path=32.0,-104.0|32.1,-103.9", 400, "INVALID_REQUEST", 0},
		{"path=32.0,-104.0|32.1,-103.9&samples=513", 400, "INVALID_REQUEST", 0},
		{"locations=95,0", 400, "INVALID_REQUEST", 0},
		{"locations=abc", 400, "INVALID_REQUEST", 0},
		{"", 400, "INVALID_REQUEST", 0},
		{"locations=10,10", 200, "DATA_NOT_AVAILABLE", 1},
	}
	for _, c := range cases {
		rr, body := get(t, mux, "/maps/api/elevation/json?"+c.query)
		if rr.Code != c.code || body["status"] != c.status {
			t.Errorf("%s: %d %v (%s)", c.query, rr.Code, body["status"], rr.Body.String())
			continue
		}
		res, _ := body["results"].([]any)
		if len(res) != c.n {
			t.Errorf("%s: %d results want %d", c.query, len(res), c.n)
		}
	}
	// Shape of one result.
	_, body := get(t, mux, "/maps/api/elevation/json?locations=32.0,-104.0|32.05,-104.15")
	r0 := body["results"].([]any)[0].(map[string]any)
	loc := r0["location"].(map[string]any)
	if loc["lat"] != 32.0 || loc["lng"] != -104.0 || r0["resolution"].(float64) < 10 {
		t.Errorf("result shape: %v", r0)
	}
	px, py := LonLatToWorldPixel(-104, 32, testZ)
	if math.Abs(r0["elevation"].(float64)-plane(px, py)) > 0.011 {
		t.Errorf("elevation %v want %v", r0["elevation"], plane(px, py))
	}
	// Uncovered point (west of -104.1): elevation 0, resolution -1, status still OK.
	r1 := body["results"].([]any)[1].(map[string]any)
	if r1["elevation"] != 0.0 || r1["resolution"] != -1.0 || body["status"] != "OK" {
		t.Errorf("uncovered point: %v status %v", r1, body["status"])
	}
}

func TestNativeAPI(t *testing.T) {
	mux, _ := newTestHandler(t)
	rr, body := get(t, mux, "/api/v1/elevation?points=-104.0,32.0|-103.95,32.05")
	if rr.Code != 200 || len(body["results"].([]any)) != 2 {
		t.Fatalf("GET points: %d %s", rr.Code, rr.Body.String())
	}
	if s := body["sources"].([]any); len(s) != 1 {
		t.Errorf("sources: %v", s)
	}
	post := func(path, js string) (*httptest.ResponseRecorder, map[string]any) {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("POST", path, strings.NewReader(js)))
		var b map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &b)
		return rr, b
	}
	rr, _ = post("/api/v1/elevation", `{"points":[[-104.0,32.0]]}`)
	if rr.Code != 200 {
		t.Fatalf("POST points: %d %s", rr.Code, rr.Body.String())
	}
	rr, body = post("/api/v1/elevation/profile", `{"path":[[-104.0,32.0],[-103.95,32.0]],"interval_m":50}`)
	if rr.Code != 200 {
		t.Fatalf("profile: %d %s", rr.Code, rr.Body.String())
	}
	// Eastward on the plane: elevation rises 0.25 m per pixel.
	px0, py0 := LonLatToWorldPixel(-104, 32, testZ)
	px1, _ := LonLatToWorldPixel(-103.95, 32, testZ)
	wantGain := plane(px1, py0) - plane(px0, py0)
	if g := body["gain_raw_m"].(float64); math.Abs(g-wantGain) > 0.1 {
		t.Errorf("gain_raw %v want %v", g, wantGain)
	}
	if g := body["gain_m"].(float64); math.Abs(g-wantGain) > 0.1 {
		t.Errorf("gain %v want %v", g, wantGain)
	}
	if l := body["length_m"].(float64); math.Abs(l-HaversineM(LonLat{-104, 32}, LonLat{-103.95, 32})) > 0.01 {
		t.Errorf("length %v", l)
	}
	rr, body = post("/api/v1/elevation/profile", `{"polyline":"`+EncodePolyline([]LonLat{{-104, 32}, {-103.95, 32}}, 6)+`","precision":6,"samples":10}`)
	if rr.Code != 200 || len(body["samples"].([]any)) != 10 {
		t.Errorf("polyline profile: %d %s", rr.Code, rr.Body.String())
	}
	for _, bad := range []string{`{}`, `{"path":[[200,0]]}`, `not json`, `{"path":[[0,0]],"polyline":"a"}`} {
		if rr, _ := post("/api/v1/elevation/profile", bad); rr.Code != 400 {
			t.Errorf("%s: code %d", bad, rr.Code)
		}
	}
}

func TestSwapAndUnavailable(t *testing.T) {
	dir := t.TempDir()
	svc := NewTerrainForData(dir)
	defer func() { _ = svc.Close() }()
	svc.checkEvery = 0
	if _, err := svc.Points(context.Background(), []LonLat{{-104, 32}}); err != ErrUnavailable {
		t.Fatalf("no generation: err = %v", err)
	}
	mux := http.NewServeMux()
	NewHandler(svc).Mount(mux, nil)
	if rr, _ := get(t, mux, "/api/v1/elevation?points=-104,32"); rr.Code != 503 {
		t.Errorf("unavailable code %d", rr.Code)
	}
	// Generation 1 covers the box; generation 2 has no data west of -103.9.
	for i, nd := range []float64{-200, -103.9} {
		gen := filepath.Join(dir, "generations", []string{"a", "b"}[i])
		_ = os.MkdirAll(gen, 0o755)
		_ = os.Rename(writeTestArchive(t, testBox, nd, 90), filepath.Join(gen, "terrain.pmtiles"))
	}
	link := filepath.Join(dir, "current")
	_ = os.Symlink(filepath.Join(dir, "generations", "a"), link)
	s, err := svc.Points(context.Background(), []LonLat{{-104, 32}})
	if err != nil || s[0].Elevation == nil {
		t.Fatalf("gen a: %+v %v", s, err)
	}
	tmp := link + ".tmp"
	_ = os.Symlink(filepath.Join(dir, "generations", "b"), tmp)
	_ = os.Rename(tmp, link)
	s, _ = svc.Points(context.Background(), []LonLat{{-104, 32}})
	if s[0].Elevation != nil {
		t.Errorf("after swap, still reading old generation")
	}
}
