package ondemand_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/contours"
	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/ondemand"
	"github.com/jclement/mapthing/internal/pmtiles"
)

// slope is ground that rises 2,000 m per degree of latitude: a contour of
// elevation E runs east-west at latitude 30 + (E-500)/2000.
func slope(_, lat float64) float64 { return 500 + 2000*(lat-30) }

func contourFeatures(t *testing.T, mvt []byte) []contours.DecodedFeature {
	t.Helper()
	raw, err := pmtiles.Decompress(mvt, pmtiles.CompressionGzip)
	if err != nil {
		t.Fatal(err)
	}
	ls, err := contours.Decode(raw)
	if err != nil || len(ls) != 1 || ls[0].Name != contours.LayerName {
		t.Fatalf("decode: %v, %d layers", err, len(ls))
	}
	return ls[0].Features
}

// checkContours compares every line of a contour tile with where the slope puts it.
func checkContours(t *testing.T, mvt []byte, z, x, y, interval int) {
	t.Helper()
	feats := contourFeatures(t, mvt)
	if len(feats) < 5 {
		t.Fatalf("z%d: only %d contour levels", z, len(feats))
	}
	for _, f := range feats {
		ele := int(f.Props["ele"].(int64))
		if ele%interval != 0 {
			t.Fatalf("z%d: a contour at %d m with a %d m interval", z, ele, interval)
		}
		wantLat := 30 + (float64(ele)-500)/2000
		_, wantPy := elevation.LonLatToWorldPixel(0, wantLat, z)
		wantY := (wantPy/256 - float64(y)) * contours.Extent
		for _, ln := range f.Lines {
			for _, p := range ln {
				if math.Abs(float64(p[1])-wantY) > 0.012*contours.Extent {
					t.Fatalf("z%d: the %d m line passes y=%d, want about %.0f", z, ele, p[1], wantY)
				}
			}
		}
	}
}

func TestContoursAreDrawnOnDemandFromDeeperTerrain(t *testing.T) {
	m, hosts := newManager(t, 500)
	for lon := -99; lon <= -97; lon++ {
		hosts.Put3DEP(lon, 30, slope, "c1")
	}
	ctx := context.Background()
	for _, tc := range []struct{ z, interval, terrainZoom int }{{10, 20, 12}, {11, 20, 13}, {12, 10, 13}} {
		if got := ondemand.ContourTerrainZoom(tc.z); got != tc.terrainZoom {
			t.Fatalf("z%d contours are drawn from terrain z%d, want z%d", tc.z, got, tc.terrainZoom)
		}
		x, y := tileAt(-98.0, 30.5, tc.z)
		if !m.AllowsContour(tc.z, x, y) || m.HasContour(tc.z, x, y) {
			t.Fatalf("z%d: allowed=%v cached=%v before the first request", tc.z, m.AllowsContour(tc.z, x, y), m.HasContour(tc.z, x, y))
		}
		mvt, cold, err := m.Contour(ctx, tc.z, x, y)
		if err != nil || !cold || len(mvt) == 0 {
			t.Fatalf("z%d: cold=%v err=%v %d bytes", tc.z, cold, err, len(mvt))
		}
		checkContours(t, mvt, tc.z, x, y, tc.interval)
		// Again: from the cache, the hosts not asked.
		before := hosts.Ranges.Load() + hosts.Heads.Load()
		again, cold, err := m.Contour(ctx, tc.z, x, y)
		if err != nil || cold || string(again) != string(mvt) || hosts.Ranges.Load()+hosts.Heads.Load() != before || !m.HasContour(tc.z, x, y) {
			t.Fatalf("z%d again: cold=%v err=%v", tc.z, cold, err)
		}
	}
	if m.AllowsContour(9, 0, 0) || m.AllowsContour(13, 0, 0) {
		t.Error("contour zooms outside 10 to 12 are allowed")
	}
	if c := m.Status().Counters; c.ContoursBuilt != 3 || c.ContourHits != 3 {
		t.Errorf("counters: %+v", c)
	}
}

func TestOnDemandContoursFollowTheirTerrain(t *testing.T) {
	m, hosts := newManager(t, 500)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	m2, err := ondemand.NewManager(context.Background(), ondemand.ManagerOptions{DataDir: t.TempDir(), Settings: settings{},
		Service: ondemand.Options{URLs: hosts.URLs(), Now: clock, Upstream: ondemand.NewUpstream(ondemand.UpstreamOptions{Attempts: 1, Timeout: 5 * time.Second})}})
	if err != nil {
		t.Fatal(err)
	}
	_ = m
	m2.Svc.SetPolicy(ondemand.Policy{Outside: true})
	for lon := -99; lon <= -97; lon++ {
		hosts.Put3DEP(lon, 30, slope, "c1")
	}
	ctx := context.Background()
	x, y := tileAt(-98.0, 30.5, 11)
	// A source that does not answer: an error, and nothing cached.
	hosts.Down.Store(true)
	if _, _, err := m2.Contour(ctx, 11, x, y); !errors.Is(err, ondemand.ErrUpstream) {
		t.Fatalf("hosts down: %v", err)
	}
	if m2.HasContour(11, x, y) || m2.Status().Cache.Entries != 0 {
		t.Fatal("a failed contour tile left something in the cache")
	}
	hosts.Down.Store(false)
	// One gate call for the whole tile, 36 terrain tiles and all; a refusal fetches nothing.
	calls := 0
	if _, _, err := m2.Contour(ondemand.WithGate(ctx, func() error { calls++; return nil }), 11, x, y); err != nil || calls != 1 {
		t.Fatalf("gate calls = %d, err = %v", calls, err)
	}
	before := hosts.Ranges.Load()
	var lim *ondemand.ErrLimited
	_, _, err = m2.Contour(ondemand.WithGate(ctx, func() error { return &ondemand.ErrLimited{Message: "no"} }), 11, x+1, y)
	if !errors.As(err, &lim) || hosts.Ranges.Load() != before {
		t.Fatalf("refused contour: %v, %d data requests", err, hosts.Ranges.Load()-before)
	}
	// The terrain's source publishes a new release: the lines move with it.
	first, _, _ := m2.Contour(ctx, 11, x, y)
	for lon := -99; lon <= -97; lon++ {
		hosts.Put3DEP(lon, 30, func(lon, lat float64) float64 { return slope(lon, lat) + 7 }, "c2")
	}
	mu.Lock()
	now = now.Add(25 * time.Hour)
	mu.Unlock()
	second, cold, err := m2.Contour(ctx, 11, x, y)
	if err != nil || !cold || string(second) == string(first) {
		t.Fatalf("after a new release: cold=%v err=%v same=%v", cold, err, string(second) == string(first))
	}
}

// At the edge of a built region a contour tile is drawn over the built terrain on that
// side, which is never fetched; a new generation makes it be drawn again.
func TestOnDemandContoursUseBuiltTerrainAtARegionEdge(t *testing.T) {
	m, hosts := newManager(t, 500)
	for lon := -99; lon <= -97; lon++ {
		hosts.Put3DEP(lon, 30, slope, "c1")
	}
	ctx := context.Background()
	x, y := tileAt(-98.0, 30.5, 12)
	// "Built": the terrain tiles west of the contour tile's own (the ring's west column).
	gen := "g1"
	builtX := x*2 - 1
	var asked sync.Map
	m.SetBuiltTerrain(ondemand.BuiltTerrain{
		Generation: func() string { return gen },
		Tile: func(z, tx, ty int) ([]byte, bool) {
			if z != 13 || tx != builtX {
				return nil, false
			}
			asked.Store([2]int{tx, ty}, true)
			e := make([]float32, 256*256)
			src := make([]uint8, 256*256)
			for i := range e {
				lon, lat := elevation.WorldPixelToLonLat(float64(tx*256+i%256)+0.5, float64(ty*256+i/256)+0.5, 13)
				e[i], src[i] = float32(slope(lon, lat)), 2
			}
			b, err := elevation.EncodeTile(e, src)
			if err != nil {
				t.Error(err)
			}
			return b, true
		},
	})
	mvt, _, err := m.Contour(ctx, 12, x, y)
	if err != nil {
		t.Fatal(err)
	}
	checkContours(t, mvt, 12, x, y, 10)
	n := 0
	asked.Range(func(k, _ any) bool {
		n++
		if c := k.([2]int); m.Svc.CachedNow(13, c[0], c[1]) {
			t.Errorf("terrain tile %v is built, and was fetched on demand all the same", c)
		}
		return true
	})
	if n == 0 {
		t.Fatal("the built terrain beside the tile was not used")
	}
	if !m.HasContour(12, x, y) {
		t.Fatal("not cached")
	}
	gen = "g2"
	if m.HasContour(12, x, y) {
		t.Fatal("a contour tile drawn over one generation's terrain is still current under another")
	}
}
