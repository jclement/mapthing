package ondemand_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/diskcache"
	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/ondemand"
	"github.com/jclement/mapthing/internal/ondemand/ondemandtest"
)

type rig struct {
	hosts *ondemandtest.Hosts
	cache *diskcache.Cache
	svc   *ondemand.Service
	now   time.Time
	mu    sync.Mutex
}

func (r *rig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *rig) advance(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

func newRig(t *testing.T, cacheBytes int64) *rig {
	t.Helper()
	r := &rig{hosts: ondemandtest.New(), now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	t.Cleanup(r.hosts.Close)
	var err error
	if r.cache, err = diskcache.New(t.TempDir(), cacheBytes); err != nil {
		t.Fatal(err)
	}
	r.svc = ondemand.New(ondemand.Options{
		Upstream: ondemand.NewUpstream(ondemand.UpstreamOptions{Attempts: 2, Backoff: time.Millisecond, Timeout: 5 * time.Second}),
		Store:    ondemand.NewDiskStore(r.cache), URLs: r.hosts.URLs(), Now: r.clock,
	})
	return r
}

// tileAt returns the SampleZoom tile holding a point.
func tileAt(lon, lat float64, z int) (x, y int) {
	px, py := elevation.LonLatToWorldPixel(lon, lat, z)
	return int(px) / 256, int(py) / 256
}

func decode(t *testing.T, png []byte) *elevation.Tile {
	t.Helper()
	tile, err := elevation.DecodeTile(png)
	if err != nil {
		t.Fatal(err)
	}
	return tile
}

// checkPlane compares every pixel of a tile with the synthetic ground.
func checkPlane(t *testing.T, tile *elevation.Tile, z, x, y int, f func(lon, lat float64) float64, tol float64) {
	t.Helper()
	bad := 0
	for j := 0; j < 256; j++ {
		for i := 0; i < 256; i++ {
			lon, lat := elevation.WorldPixelToLonLat(float64(x*256)+float64(i)+0.5, float64(y*256)+float64(j)+0.5, z)
			got := float64(tile.Elev[j*256+i])
			if !ondemandtest.Near(got, f(lon, lat), tol) {
				if bad == 0 {
					t.Errorf("z%d pixel %d,%d (%.5f,%.5f) = %.3f, want %.3f", z, i, j, lon, lat, got, f(lon, lat))
				}
				bad++
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d pixels off", bad)
	}
}

func TestTileMatchesTheSourceAtEveryZoom(t *testing.T) {
	r := newRig(t, 64<<20)
	f := ondemandtest.Plane(1000)
	for lon := -99; lon <= -97; lon++ {
		for lat := 29; lat <= 31; lat++ {
			r.hosts.Put3DEP(lon, lat, f, "v1")
		}
	}
	for _, z := range []int{9, 10, 11, 12, 13} {
		x, y := tileAt(-97.6, 30.4, z)
		png, cold, err := r.svc.Tile(context.Background(), z, x, y)
		if err != nil {
			t.Fatalf("z%d: %v", z, err)
		}
		if !cold || len(png) == 0 {
			t.Fatalf("z%d: cold=%v, %d bytes", z, cold, len(png))
		}
		// The plane survives resampling exactly; what is left is the rounding step.
		step := math.Min(0.25*math.Exp2(float64(13-z)), 8)
		checkPlane(t, decode(t, png), z, x, y, f, step/2+0.02)
	}
	// Deepest zoom carries the per-pixel source: 3DEP is second in the table.
	x, y := tileAt(-97.6, 30.4, 13)
	png, cold, err := r.svc.Tile(context.Background(), 13, x, y)
	if err != nil || cold {
		t.Fatalf("second read: cold=%v err=%v", cold, err)
	}
	if src := decode(t, png).Src; src == nil || src[0] != 2 || src[65535] != 2 {
		t.Fatalf("source chunk = %v", src)
	}
}

func TestFallsThroughToTheNextSource(t *testing.T) {
	r := newRig(t, 64<<20)
	// 3DEP has only the western cell; GLO-30 has both. A tile across the
	// seam takes each pixel from the best source that has it.
	r.hosts.Put3DEP(-98, 30, ondemandtest.Plane(1000), "v1")
	r.hosts.PutGLO30(-98, 30, ondemandtest.Plane(5000), "v1")
	r.hosts.PutGLO30(-97, 30, ondemandtest.Plane(5000), "v1")
	x, y := tileAt(-97.0001, 30.5, 13)
	png, _, err := r.svc.Tile(context.Background(), 13, x, y)
	if err != nil {
		t.Fatal(err)
	}
	tile := decode(t, png)
	var west, east int
	for i, s := range tile.Src {
		lon, lat := elevation.WorldPixelToLonLat(float64(x*256+i%256)+0.5, float64(y*256+i/256)+0.5, 13)
		switch s {
		case 2:
			west++
			if lon > -96.99 || !ondemandtest.Near(float64(tile.Elev[i]), ondemandtest.Plane(1000)(lon, lat), 0.2) {
				t.Fatalf("3DEP pixel at %.5f = %v", lon, tile.Elev[i])
			}
		case 4:
			east++
			if lon < -97.01 || !ondemandtest.Near(float64(tile.Elev[i]), ondemandtest.Plane(5000)(lon, lat), 0.2) {
				t.Fatalf("GLO-30 pixel at %.5f = %v", lon, tile.Elev[i])
			}
		default:
			t.Fatalf("pixel %d has source %d", i, s)
		}
	}
	if west == 0 || east == 0 {
		t.Fatalf("west %d east %d: the tile does not straddle the seam", west, east)
	}
}

func TestOceanIsEmptyAndCostsNothing(t *testing.T) {
	r := newRig(t, 64<<20)
	x, y := tileAt(-40, 30, 12) // mid-Atlantic: only GLO-30 could cover, and has no file
	png, cold, err := r.svc.Tile(context.Background(), 12, x, y)
	if err != nil || png != nil || cold {
		t.Fatalf("png=%d cold=%v err=%v", len(png), cold, err)
	}
	if r.hosts.Ranges.Load() != 0 {
		t.Fatal("fetched data for open ocean")
	}
	heads := r.hosts.Heads.Load()
	if _, _, err := r.svc.Tile(context.Background(), 12, x, y); err != nil {
		t.Fatal(err)
	}
	if r.hosts.Heads.Load() != heads {
		t.Fatal("asked again about a file known to be missing")
	}
}

func TestConcurrentRequestsShareOneFetch(t *testing.T) {
	r := newRig(t, 64<<20)
	r.hosts.Put3DEP(-98, 30, ondemandtest.Plane(1000), "v1")
	r.hosts.Delay.Store(int64(30 * time.Millisecond))
	x, y := tileAt(-97.5, 30.5, 13)
	// Learn how many requests one cold tile takes.
	solo := newRig(t, 64<<20)
	solo.hosts.Put3DEP(-98, 30, ondemandtest.Plane(1000), "v1")
	if _, _, err := solo.svc.Tile(context.Background(), 13, x, y); err != nil {
		t.Fatal(err)
	}
	want := solo.hosts.Ranges.Load()

	var wg sync.WaitGroup
	errs := make([]error, 40)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = r.svc.Tile(context.Background(), 13, x, y)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := r.hosts.Ranges.Load(); got != want {
		t.Fatalf("40 concurrent requests made %d data requests, one request makes %d", got, want)
	}
	if got := r.hosts.Heads.Load(); got > solo.hosts.Heads.Load() {
		t.Fatalf("%d HEAD requests, one request makes %d", got, solo.hosts.Heads.Load())
	}
	if c := r.svc.Counters(); c.Built != 1 {
		t.Fatalf("built %d tiles", c.Built)
	}
}

func TestUpstreamFailureIsNotCached(t *testing.T) {
	r := newRig(t, 64<<20)
	r.hosts.Put3DEP(-98, 30, ondemandtest.Plane(1000), "v1")
	x, y := tileAt(-97.5, 30.5, 13)
	r.hosts.Down.Store(true)
	_, _, err := r.svc.Tile(context.Background(), 13, x, y)
	if !errors.Is(err, ondemand.ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	if r.cache.Len() != 0 {
		t.Fatalf("%d entries cached from a failure", r.cache.Len())
	}
	// Half-way failures too: the file is known, its data cannot be read.
	r.hosts.Down.Store(false)
	x2, y2 := tileAt(-97.2, 30.2, 13)
	if _, _, err := r.svc.Tile(context.Background(), 13, x2, y2); err != nil {
		t.Fatal(err)
	}
	r.hosts.Down.Store(true)
	_, _, err = r.svc.Tile(context.Background(), 13, x, y)
	if !errors.Is(err, ondemand.ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	if r.svc.Cached(context.Background(), 13, x, y) {
		t.Fatal("a failed tile is reported as cached")
	}
	// The hosts come back: the same tile is built, correct, and now cached.
	r.hosts.Down.Store(false)
	png, cold, err := r.svc.Tile(context.Background(), 13, x, y)
	if err != nil || !cold {
		t.Fatalf("after recovery: cold=%v err=%v", cold, err)
	}
	checkPlane(t, decode(t, png), 13, x, y, ondemandtest.Plane(1000), 0.2)
	if c := r.svc.Counters(); c.Failed != 2 {
		t.Fatalf("failed = %d", c.Failed)
	}
	// A cached tile is still served while the hosts are down.
	r.hosts.Down.Store(true)
	r.advance(48 * time.Hour) // its version is stale too; the hosts cannot be asked, so it stands
	if _, cold, err := r.svc.Tile(context.Background(), 13, x, y); err != nil || cold {
		t.Fatalf("cached tile with hosts down: cold=%v err=%v", cold, err)
	}
}

func TestNewUpstreamReleaseReplacesCachedTiles(t *testing.T) {
	r := newRig(t, 64<<20)
	r.hosts.Put3DEP(-98, 30, ondemandtest.Plane(1000), "v1")
	x, y := tileAt(-97.5, 30.5, 13)
	ctx := context.Background()
	if _, _, err := r.svc.Tile(ctx, 13, x, y); err != nil {
		t.Fatal(err)
	}
	// A new release appears. Within the version TTL nobody notices...
	r.hosts.Put3DEP(-98, 30, ondemandtest.Plane(2000), "v2")
	r.advance(23 * time.Hour)
	png, cold, err := r.svc.Tile(ctx, 13, x, y)
	if err != nil || cold {
		t.Fatalf("inside the TTL: cold=%v err=%v", cold, err)
	}
	checkPlane(t, decode(t, png), 13, x, y, ondemandtest.Plane(1000), 0.2)
	// ...after it, the host is asked, and the old tile and blocks are not used again.
	r.advance(2 * time.Hour)
	png, cold, err = r.svc.Tile(ctx, 13, x, y)
	if err != nil || !cold {
		t.Fatalf("after the TTL: cold=%v err=%v", cold, err)
	}
	checkPlane(t, decode(t, png), 13, x, y, ondemandtest.Plane(2000), 0.2)
	tile, err := r.svc.Decoded(ctx, x, y)
	if err != nil {
		t.Fatal(err)
	}
	checkPlane(t, tile, 13, x, y, ondemandtest.Plane(2000), 0.2)
}

func TestCacheStaysWithinItsBudget(t *testing.T) {
	const budget = 300 << 10
	r := newRig(t, budget)
	// Rough ground, so tiles and blocks do not compress to nothing.
	rough := func(lon, lat float64) float64 {
		return 500 + 300*math.Sin(lon*900)*math.Cos(lat*700) + 50*math.Sin(lon*9000+lat*8000)
	}
	r.hosts.Put3DEP(-98, 30, rough, "v1")
	ctx := context.Background()
	x0, y0 := tileAt(-97.9, 30.9, 13)
	var first [2]int
	for i := 0; i < 64; i++ {
		x, y := x0+i%8, y0+i/8
		if i == 0 {
			first = [2]int{x, y}
		}
		if _, _, err := r.svc.Tile(ctx, 13, x, y); err != nil {
			t.Fatal(err)
		}
		if r.cache.Size() > budget {
			t.Fatalf("cache holds %d bytes, budget %d", r.cache.Size(), budget)
		}
	}
	if r.cache.Size() < budget/2 {
		t.Fatalf("cache holds only %d bytes: the test does not fill it", r.cache.Size())
	}
	// The oldest tile was evicted; asking again rebuilds it.
	if r.svc.Cached(ctx, 13, first[0], first[1]) {
		t.Fatal("the first tile is still cached")
	}
	if _, cold, err := r.svc.Tile(ctx, 13, first[0], first[1]); err != nil || !cold {
		t.Fatalf("cold=%v err=%v", cold, err)
	}
}

func TestGateIsCalledOncePerColdRequest(t *testing.T) {
	r := newRig(t, 64<<20)
	r.hosts.Put3DEP(-98, 30, ondemandtest.Plane(1000), "v1")
	x, y := tileAt(-97.5, 30.5, 13)
	calls := 0
	ctx := ondemand.WithGate(context.Background(), func() error { calls++; return nil })
	for i := 0; i < 3; i++ { // three cold tiles in one request
		if _, _, err := r.svc.Tile(ctx, 13, x+i, y); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("gate called %d times for one request", calls)
	}
	// A cached tile never reaches the gate.
	ctx = ondemand.WithGate(context.Background(), func() error { t.Error("gate called for a cached tile"); return nil })
	if _, _, err := r.svc.Tile(ctx, 13, x, y); err != nil {
		t.Fatal(err)
	}
	// A refusing gate stops the request before anything is fetched.
	before := r.hosts.Ranges.Load()
	ctx = ondemand.WithGate(context.Background(), func() error {
		return &ondemand.ErrLimited{RetryAfter: 3 * time.Second, Message: "slow down"}
	})
	_, _, err := r.svc.Tile(ctx, 13, x+5, y)
	var lim *ondemand.ErrLimited
	if !errors.As(err, &lim) || lim.RetryAfter != 3*time.Second {
		t.Fatalf("err = %v", err)
	}
	if r.hosts.Ranges.Load() != before {
		t.Fatal("a refused request fetched data")
	}
}

func TestUpstreamIdentifiesItself(t *testing.T) {
	if !strings.Contains(ondemand.UserAgent, "mapthing") || !strings.Contains(ondemand.UserAgent, "https://github.com/") || strings.Contains(ondemand.UserAgent, "@") {
		t.Fatalf("User-Agent %q must name mapthing and its repository, and no person", ondemand.UserAgent)
	}
}

func TestPolicy(t *testing.T) {
	texas := ondemand.Area{ID: "texas", BBox: elevation.BBox{-100, 29, -97, 32},
		Rings: [][][2]float64{{{-100, 29}, {-97, 29}, {-100, 32}}}} // a triangle in the south-west half
	p := ondemand.Policy{Areas: []ondemand.Area{texas}}
	in := func(lon, lat float64, z int) bool {
		x, y := tileAt(lon, lat, z)
		return p.AllowsTile(z, x, y)
	}
	if !in(-99.5, 29.5, 12) {
		t.Error("inside the region: refused")
	}
	if in(-97.2, 31.8, 12) {
		t.Error("inside the box but outside the outline: allowed")
	}
	if in(-90, 40, 12) {
		t.Error("outside the region: allowed")
	}
	if in(-99.5, 29.5, 8) || in(-99.5, 29.5, 14) {
		t.Error("zooms outside the on-demand range: allowed")
	}
	if (ondemand.Policy{}).Enabled() || !(ondemand.Policy{Outside: true}).AllowsTile(12, 0, 0) {
		t.Error("off must be off, and the world must be the world")
	}
}
