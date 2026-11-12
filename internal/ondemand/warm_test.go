package ondemand_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/ondemand"
	"github.com/jclement/mapthing/internal/ondemand/ondemandtest"
)

type settings map[string]string

func (m settings) Setting(_ context.Context, k string) (string, error) {
	if v, ok := m[k]; ok {
		return v, nil
	}
	return "", os.ErrNotExist
}
func (m settings) SetSetting(_ context.Context, k, v string) error { m[k] = v; return nil }

func newManager(t *testing.T, cacheMB int64) (*ondemand.Manager, *ondemandtest.Hosts) {
	t.Helper()
	hosts := ondemandtest.New()
	t.Cleanup(hosts.Close)
	for lon := -99; lon <= -97; lon++ {
		hosts.Put3DEP(lon, 30, ondemandtest.Plane(1000), "v1")
	}
	m, err := ondemand.NewManager(context.Background(), ondemand.ManagerOptions{DataDir: t.TempDir(), Settings: settings{},
		Service: ondemand.Options{URLs: hosts.URLs(), Upstream: ondemand.NewUpstream(ondemand.UpstreamOptions{Attempts: 1, Timeout: 5 * time.Second})}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetConfig(context.Background(), ondemand.Config{Outside: ondemand.OutsideOnDemand, CacheMB: cacheMB}); err != nil {
		t.Fatal(err)
	}
	return m, hosts
}

func waitWarm(t *testing.T, m *ondemand.Manager) ondemand.WarmStatus {
	t.Helper()
	for i := 0; i < 600; i++ {
		if st := m.WarmStatus(); st.State != "running" {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the warm job did not end")
	return ondemand.WarmStatus{}
}

func TestWarmFillsAnAreaAndSaysFirstWhatItWillCost(t *testing.T) {
	m, hosts := newManager(t, 500)
	box := elevation.BBox{-98.30, 30.40, -98.10, 30.60}
	req := ondemand.WarmRequest{BBox: box, Label: "test box", MaxZoom: 12}

	est, err := m.EstimateWarm(req)
	if err != nil {
		t.Fatal(err)
	}
	if est.Tiles < 8 || est.Cached != 0 || est.ToFetch != est.Tiles || !est.Fits || !est.Applies ||
		est.CacheBytes <= 0 || est.DownloadBytes <= est.CacheBytes || est.Seconds <= 0 {
		t.Fatalf("estimate: %+v", est)
	}
	if n := hosts.Heads.Load() + hosts.Ranges.Load(); n != 0 {
		t.Fatalf("the estimate asked the hosts %d times", n)
	}
	// Tiles the server has built are left alone.
	bx, by := tileAt(-98.2, 30.5, 12)
	m.SetBuilt(func(z, x, y int) bool { return z == 12 && x == bx && y == by })
	if e2, _ := m.EstimateWarm(req); e2.Tiles != est.Tiles-1 {
		t.Fatalf("with one tile built: %d tiles, want %d", e2.Tiles, est.Tiles-1)
	}
	m.SetBuilt(func(int, int, int) bool { return false })

	if _, err := m.StartWarm(req); err != nil {
		t.Fatal(err)
	}
	st := waitWarm(t, m)
	if st.State != "done" || st.Done != est.Tiles || st.Fetched != est.Tiles || st.Failed != 0 || st.Bytes <= 0 || st.FinishedAt == nil {
		t.Fatalf("warm: %+v", st)
	}
	// Everything is cached now: with the hosts gone, every tile of the area still answers.
	hosts.Down.Store(true)
	for z := ondemand.MinZoom; z <= 12; z++ {
		x0, y0, x1, y1 := elevation.TileRange(z, box[0], box[1], box[2], box[3])
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				if png, cold, err := m.Tile(context.Background(), z, x, y); err != nil || cold || len(png) == 0 {
					t.Fatalf("after warming, with the hosts down, %d/%d/%d: cold=%v err=%v", z, x, y, cold, err)
				}
			}
		}
	}
	hosts.Down.Store(false)
	// A warm job keeps the tiles, not the source blocks they were made from.
	if st := m.Status(); st.Cache.Bytes > 3*int64(est.Tiles)*60_000 {
		t.Fatalf("cache holds %d bytes after warming %d tiles: source blocks were kept", st.Cache.Bytes, est.Tiles)
	}
	// Asked again, there is nothing left to fetch.
	if e3, _ := m.EstimateWarm(req); e3.Cached != e3.Tiles || e3.ToFetch != 0 || e3.Seconds != 0 {
		t.Fatalf("estimate after warming: %+v", e3)
	}
	before := hosts.Ranges.Load()
	if _, err := m.StartWarm(req); err != nil {
		t.Fatal(err)
	}
	if st := waitWarm(t, m); st.State != "done" || st.Fetched != 0 || st.Cached != est.Tiles || hosts.Ranges.Load() != before {
		t.Fatalf("second warm: %+v", st)
	}
}

func TestWarmRefusesWhatItCannotDo(t *testing.T) {
	m, hosts := newManager(t, 64)
	var we *ondemand.ErrWarm
	// A cache too small for the area: said before anything is fetched, with the size that would do.
	big := ondemand.WarmRequest{BBox: elevation.BBox{-98.9, 30.05, -97.1, 30.95}, MaxZoom: 13}
	est, err := m.EstimateWarm(big)
	if err != nil || est.Fits || est.NeedCacheMB <= 64 {
		t.Fatalf("estimate for an area the cache cannot hold: %+v %v", est, err)
	}
	if _, err := m.StartWarm(big); !errors.As(err, &we) || !strings.Contains(we.Msg, "raise the cache size") {
		t.Fatalf("start: %v", err)
	}
	// Bad requests.
	for _, r := range []ondemand.WarmRequest{
		{BBox: elevation.BBox{-98, 30, -99, 31}, MaxZoom: 12},
		{BBox: elevation.BBox{-98.3, 30.4, -98.1, 30.6}, MaxZoom: 15},
		{BBox: elevation.BBox{-170, -60, 170, 70}, MaxZoom: 13},
	} {
		if _, err := m.EstimateWarm(r); !errors.As(err, &we) {
			t.Errorf("%+v: %v", r, err)
		}
	}
	// Where on demand does not apply there is nothing to warm.
	m.Svc.SetPolicy(ondemand.Policy{})
	if _, err := m.StartWarm(ondemand.WarmRequest{BBox: elevation.BBox{-98.3, 30.4, -98.1, 30.6}, MaxZoom: 11}); !errors.As(err, &we) {
		t.Fatalf("warming where on demand is off: %v", err)
	}
	if n := hosts.Ranges.Load(); n != 0 {
		t.Fatalf("refused warm jobs fetched data (%d requests)", n)
	}
}

func TestWarmStopsWhenTheSourceIsDownAndCanBeCancelled(t *testing.T) {
	m, hosts := newManager(t, 500)
	req := ondemand.WarmRequest{BBox: elevation.BBox{-98.6, 30.2, -98.0, 30.8}, MaxZoom: 12}
	hosts.Down.Store(true)
	if _, err := m.StartWarm(req); err != nil {
		t.Fatal(err)
	}
	st := waitWarm(t, m)
	if st.State != "failed" || st.Error == "" || st.Fetched != 0 || st.Done >= st.Total {
		t.Fatalf("warm with the hosts down: %+v", st)
	}
	if s := m.Status(); s.Cache.Entries != 0 {
		t.Fatalf("%d entries cached from failures", s.Cache.Entries)
	}
	// Running: a second one is refused, and cancelling keeps what was fetched.
	hosts.Down.Store(false)
	hosts.Delay.Store(int64(40 * time.Millisecond))
	if _, err := m.StartWarm(req); err != nil {
		t.Fatal(err)
	}
	if _, err := m.StartWarm(req); !errors.Is(err, ondemand.ErrWarmBusy) {
		t.Fatalf("a second warm job while one runs: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	m.CancelWarm()
	st = waitWarm(t, m)
	if st.State != "cancelled" {
		t.Fatalf("after cancel: %+v", st)
	}
}
