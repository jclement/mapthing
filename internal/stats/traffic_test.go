package stats

import (
	"testing"
	"time"
)

func TestTrafficWindows(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 30, 0, time.UTC)
	tr := NewTraffic()
	tr.now = func() time.Time { return now }
	start := now.Add(-48 * time.Hour)

	// Three hours ago: 100 slow routes. Not in the last hour, in the last day.
	now = now.Add(-3 * time.Hour)
	for range 100 {
		tr.Observe("route", 900*time.Millisecond, 200)
	}
	// Ten minutes ago: 90 fast tiles, 9 at 40 ms and one 5xx at 2 s; two rejected.
	now = now.Add(3*time.Hour - 10*time.Minute)
	for range 90 {
		tr.Observe("tiles", 2*time.Millisecond, 200)
	}
	for range 9 {
		tr.Observe("tiles", 40*time.Millisecond, 200)
	}
	tr.Observe("tiles", 2*time.Second, 503)
	tr.Observe("tiles", time.Millisecond, 429)
	tr.Observe("tiles", time.Millisecond, 401)
	tr.Observe("", time.Millisecond, 200) // not shown: ignored
	now = now.Add(10 * time.Minute)

	snap := map[string]KindTraffic{}
	for _, k := range tr.Snapshot(start) {
		snap[k.Kind] = k
	}
	if len(snap) != len(TrafficKinds) {
		t.Fatalf("kinds %d, want every shown kind even with no traffic", len(snap))
	}
	tiles := snap["tiles"]
	if tiles.Hour.Requests != 102 || tiles.Hour.Errors != 1 || tiles.Hour.Rejected != 2 {
		t.Errorf("tiles hour: %+v", tiles.Hour)
	}
	if tiles.Hour.PerMin != 1.7 {
		t.Errorf("tiles per minute over the hour: %v, want 1.7", tiles.Hour.PerMin)
	}
	// p50 is among the 2 ms requests, p95 among the 40 ms ones; the estimate is the bucket's
	// upper bound, within 19% above the true value.
	if tiles.Hour.P50ms < 2 || tiles.Hour.P50ms > 2.4 {
		t.Errorf("tiles p50 %v ms, want about 2", tiles.Hour.P50ms)
	}
	if tiles.Hour.P95ms < 40 || tiles.Hour.P95ms > 48 {
		t.Errorf("tiles p95 %v ms, want about 40", tiles.Hour.P95ms)
	}
	if tiles.Series[49] != 102 || tiles.Series[59] != 0 || len(tiles.Series) != 60 {
		t.Errorf("tiles series: %v", tiles.Series)
	}
	route := snap["route"]
	if route.Hour.Requests != 0 || route.Hour.P95ms != 0 {
		t.Errorf("route hour should be empty: %+v", route.Hour)
	}
	if route.Day.Requests != 100 || route.Day.P50ms < 900 || route.Day.P50ms > 1071 {
		t.Errorf("route day: %+v", route.Day)
	}
	if snap["pack"].Day.Requests != 0 {
		t.Errorf("pack: %+v", snap["pack"])
	}

	// A day later the old minutes have aged out, though their slots were never rewritten.
	now = now.Add(25 * time.Hour)
	for _, k := range tr.Snapshot(start) {
		if k.Day.Requests != 0 {
			t.Errorf("%s still counts %d requests a day later", k.Kind, k.Day.Requests)
		}
	}
}

func TestTrafficRateSinceStart(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	tr := NewTraffic()
	tr.now = func() time.Time { return now }
	for range 30 {
		tr.Observe("raster", 100*time.Millisecond, 200)
	}
	// The server started 10 minutes ago: 30 requests is 3 a minute, not 0.5.
	k := tr.Snapshot(now.Add(-10 * time.Minute))[1]
	if k.Kind != "raster" || k.Hour.PerMin != 3 || k.Day.PerMin != 3 {
		t.Errorf("%+v", k)
	}
}

func TestTrafficKind(t *testing.T) {
	for _, c := range []struct{ kind, path, want string }{
		{KindTile, "/tiles/basemap/1/2/3.mvt", "tiles"},
		{KindTile, "/styles/overlay.json", "style"},
		{KindTile, "/fonts/Noto Sans Regular/0-255.pbf", "style"},
		{KindRaster, "/styles/overlay/512/1/2/3.png", "raster"},
		{KindPack, "/packs/extract.pmtiles", "pack"},
		{KindElevation, "/maps/api/elevation/json", "elevation"},
		{"route", "/api/v1/route", "route"},
		{"matrix", "/ors/v2/matrix/driving-car", "route"},
		{"raster-internal", "/tiles/basemap/1/2/3.mvt", ""},
	} {
		if got := TrafficKind(c.kind, c.path); got != c.want {
			t.Errorf("TrafficKind(%q, %q) = %q, want %q", c.kind, c.path, got, c.want)
		}
	}
}

func TestBuckets(t *testing.T) {
	for _, ms := range []float64{0.1, 0.25, 1, 3.3, 50, 999, 60000} {
		b := bucketOf(ms)
		if up := bucketBound(b); up < ms || (b > 0 && bucketBound(b-1) >= ms) {
			t.Errorf("%v ms → bucket %d (bound %v)", ms, b, up)
		}
	}
}
