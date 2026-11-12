package elevation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFallback is on-demand terrain reduced to its contract: flat ground at
// 9000 m from source 4 (GLO-30) wherever it covers.
type fakeFallback struct {
	covers func(lon, lat float64) bool
	err    error

	mu       sync.Mutex
	prepared [][2]int
	tiles    map[[2]int]int
}

func (f *fakeFallback) Zoom() int { return 13 }
func (f *fakeFallback) Covers(lon, lat float64) bool {
	return f.covers == nil || f.covers(lon, lat)
}
func (f *fakeFallback) Prepare(_ context.Context, tiles [][2]int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared = append(f.prepared, tiles...)
	return f.err
}
func (f *fakeFallback) Tile(_ context.Context, x, y int) (*Tile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.tiles == nil {
		f.tiles = map[[2]int]int{}
	}
	f.tiles[[2]int{x, y}]++
	t := &Tile{Elev: make([]float32, TileSize*TileSize), Src: make([]uint8, TileSize*TileSize)}
	for i := range t.Elev {
		t.Elev[i], t.Src[i] = 9000, 4
	}
	return t, nil
}

func liveTerrain(t *testing.T, archive string) (*http.ServeMux, *Terrain) {
	t.Helper()
	data := t.TempDir()
	gen := filepath.Join(data, "generations", "g1")
	if err := os.MkdirAll(gen, 0o755); err != nil {
		t.Fatal(err)
	}
	if archive != "" {
		if err := os.Rename(archive, filepath.Join(gen, "terrain.pmtiles")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(gen, filepath.Join(data, "current")); err != nil {
		t.Fatal(err)
	}
	return newMuxFor(t, data)
}

// Built terrain wins, a tile at a time: where a built tile exists the archive
// answers, holes included (as the tile endpoint serves that tile), and the
// fallback is neither asked nor fetched. Only ground with no built tile is the
// fallback's.
func TestBuiltTerrainWinsOverFallback(t *testing.T) {
	// The archive's tiles cover testBox; they have data east of -104.1 only.
	mux, terrain := liveTerrain(t, writeTestArchive(t, testBox, -104.1, 99))
	fb := &fakeFallback{covers: func(lon, _ float64) bool { return lon > -110 }}
	terrain.SetFallback(fb)
	x0, _, _, _ := TileRange(testZ, testBox[0], testBox[1], testBox[2], testBox[3])
	builtWest, _ := WorldPixelToLonLat(float64(x0*TileSize), 0, testZ) // west edge of the built tiles

	res, err := terrain.Points(context.Background(), []LonLat{{-104.0, 32.0}, {-104.15, 32.0}, {-105.0, 32.0}, {-120, 32}})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Elevation == nil || *res[0].Elevation > 8000 || res[0].Source != "3dep13" {
		t.Errorf("built point: %+v", res[0])
	}
	if res[1].Elevation != nil {
		t.Errorf("a hole inside a built tile was filled on demand: %+v", res[1])
	}
	if r := res[2]; r.Elevation == nil || *r.Elevation != 9000 || r.Source != "glo30" {
		t.Errorf("a point with no built tile: %+v", r)
	}
	if res[3].Elevation != nil {
		t.Errorf("where nothing applies: %+v", res[3])
	}
	if len(fb.prepared) == 0 {
		t.Error("fallback tiles were not fetched together first")
	}
	// A profile from unbuilt ground into the built tiles: on demand up to their edge,
	// then the archive, hole and all. Samples right at the edge interpolate on their own side.
	p, err := terrain.Profile(context.Background(), []LonLat{{builtWest - 0.1, 32.0}, {-104.0, 32.0}}, ProfileOptions{IntervalM: 250})
	if err != nil {
		t.Fatal(err)
	}
	var built, demand, holes int
	for _, s := range p.Samples {
		switch {
		case s.Elevation == nil:
			holes++
			if s.Lon < builtWest || s.Lon > -104.09 {
				t.Fatalf("hole at %v, outside the built tiles' own hole", s.Lon)
			}
		case *s.Elevation == 9000:
			demand++
			if s.Lon >= builtWest {
				t.Fatalf("on-demand elevation at %v, inside a built tile", s.Lon)
			}
		default:
			built++
		}
	}
	if built == 0 || demand == 0 || holes == 0 {
		t.Errorf("profile: %d built, %d on demand, %d holes", built, demand, holes)
	}
	// No fallback tile over built ground was read or fetched.
	edge, _ := LonLatToWorldPixel(builtWest, 32, 13)
	for _, k := range append(fb.prepared, keysOf(fb.tiles)...) {
		if float64(k[0]*TileSize) >= edge {
			t.Errorf("fallback tile %v is over built ground", k)
		}
	}
	// The source list names what on demand may read.
	if _, body := get(t, mux, "/api/v1/elevation/sources"); !strings.Contains(toJSON(body), "hrdem") {
		t.Errorf("sources: %v", body)
	}
}

func keysOf(m map[[2]int]int) [][2]int {
	var out [][2]int
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestFallbackWithoutAnyBuiltTerrain(t *testing.T) {
	mux, terrain := liveTerrain(t, "")
	if rec, _ := get(t, mux, "/api/v1/elevation?points=-104,32"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no terrain, no fallback: %d", rec.Code)
	}
	terrain.SetFallback(&fakeFallback{})
	rec, body := get(t, mux, "/api/v1/elevation?points=-104,32")
	if rec.Code != http.StatusOK || !strings.Contains(toJSON(body), `"elevation_m":9000`) {
		t.Fatalf("%d %v", rec.Code, body)
	}
}

func TestFallbackErrorsAreAnsweredClearly(t *testing.T) {
	mux, terrain := liveTerrain(t, "")
	fb := &fakeFallback{err: ErrSourceUnavailable}
	terrain.SetFallback(fb)

	rec, body := get(t, mux, "/api/v1/elevation?points=-104,32")
	if rec.Code != http.StatusGatewayTimeout || rec.Header().Get("Retry-After") == "" {
		t.Errorf("native, source down: %d %v", rec.Code, body)
	}
	rec, body = get(t, mux, "/maps/api/elevation/json?locations=32,-104")
	if rec.Code != http.StatusGatewayTimeout || body["status"] != "UNKNOWN_ERROR" || body["error_message"] == "" {
		t.Errorf("google, source down: %d %v", rec.Code, body)
	}
	if res, ok := body["results"].([]any); !ok || len(res) != 0 {
		t.Errorf("google, source down: results = %v", body["results"])
	}

	fb.err = &LimitedError{RetryAfter: 4 * time.Second, Message: "rate limit reached"}
	rec, _ = get(t, mux, "/api/v1/elevation?points=-104,32")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "4" {
		t.Errorf("native, limited: %d Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	rec, body = get(t, mux, "/maps/api/elevation/json?locations=32,-104")
	if rec.Code != http.StatusTooManyRequests || body["status"] != "OVER_QUERY_LIMIT" {
		t.Errorf("google, limited: %d %v", rec.Code, body)
	}

	// A request that would need more tiles than one request may fetch is refused whole.
	fb.err = nil
	pts := make([]LonLat, 0, 2000)
	for i := 0; i < 2000; i++ {
		pts = append(pts, LonLat{-104 + float64(i)*0.05, 32 + float64(i%7)*0.05})
	}
	if _, err := terrain.Points(context.Background(), pts); !errors.Is(err, ErrTooLarge) {
		t.Errorf("huge request: %v", err)
	}
	if len(fb.tiles) != 0 {
		t.Error("a refused request fetched tiles")
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
