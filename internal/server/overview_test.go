package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/jobs"
)

type overviewResp struct {
	Version    string `json:"version"`
	Ready      bool   `json:"ready"`
	Generation *struct {
		ID      string `json:"id"`
		Bytes   int64  `json:"bytes"`
		Regions []struct {
			ID   string    `json:"id"`
			Name string    `json:"name"`
			BBox []float64 `json:"bbox"`
		} `json:"regions"`
	} `json:"generation"`
	RegionsInstalled int `json:"regions_installed"`
	Processes        []struct {
		Name       string   `json:"name"`
		State      string   `json:"state"`
		PID        *int     `json:"pid"`
		RSSBytes   int64    `json:"rss_bytes"`
		CPUPercent *float64 `json:"cpu_percent"`
		UptimeS    int64    `json:"uptime_s"`
	} `json:"processes"`
	System struct {
		CPUs        int   `json:"cpus"`
		MemoryTotal int64 `json:"memory_total_bytes"`
	} `json:"system"`
	Traffic struct {
		Kinds []struct {
			Kind string `json:"kind"`
			Hour struct {
				Requests int64   `json:"requests"`
				Rejected int64   `json:"rejected"`
				P50ms    float64 `json:"p50_ms"`
				P95ms    float64 `json:"p95_ms"`
				PerMin   float64 `json:"per_min"`
			} `json:"hour"`
			Day struct {
				Requests int64 `json:"requests"`
			} `json:"day"`
			Series []int64 `json:"series"`
		} `json:"kinds"`
	} `json:"traffic"`
	Disk struct {
		TotalBytes uint64 `json:"total_bytes"`
		FreeBytes  uint64 `json:"free_bytes"`
		DataBytes  int64  `json:"data_bytes"`
		Parts      []struct {
			Name  string `json:"name"`
			Bytes int64  `json:"bytes"`
		} `json:"parts"`
		Guard struct {
			Known     bool  `json:"known"`
			NeedBytes int64 `json:"need_bytes"`
			OK        bool  `json:"ok"`
		} `json:"guard"`
	} `json:"disk"`
	Builds struct {
		ScheduleEnabled bool       `json:"schedule_enabled"`
		NextRun         *time.Time `json:"next_run"`
		Last            *struct {
			Status    string  `json:"status"`
			DurationS float64 `json:"duration_s"`
			Error     string  `json:"error"`
		} `json:"last"`
	} `json:"builds"`
}

func TestOverview(t *testing.T) {
	e := newEnv(t, true, func(d *Deps) {
		d.BuildEstimate = func(context.Context) (int64, bool) { return 1 << 62, true } // more than any disk
	})
	if r, _ := e.do(nil, "GET", "/admin/api/overview", nil, nil); r.StatusCode != 401 {
		t.Fatalf("overview without a session: %d", r.StatusCode)
	}
	c := e.login()
	_, key := e.createKey(c, map[string]any{"label": "k"})
	auth := map[string]string{"Authorization": "Bearer " + key}
	for range 3 {
		if r, _ := e.do(nil, "GET", "/tiles/basemap/0/0/0.mvt", nil, auth); r.StatusCode != 200 {
			t.Fatalf("tile: %d", r.StatusCode)
		}
	}
	if r, _ := e.do(nil, "GET", "/styles/outdoors.json", nil, auth); r.StatusCode != 200 {
		t.Fatalf("style: %d", r.StatusCode)
	}
	// Refused requests are counted apart; the renderer's own requests are not shown at all.
	e.do(nil, "GET", "/tiles/basemap/0/0/0.mvt", nil, map[string]string{"Authorization": "Bearer mt_wrong"})
	e.do(nil, "GET", "/tiles/basemap/0/0/0.mvt", nil, map[string]string{"X-Mapthing-Internal": token})
	// A failed build, to be reported as the last one.
	e.jobs.Register("build", func(context.Context, *jobs.Job) error { return os.ErrDeadlineExceeded })
	if r, b := e.do(c, "POST", "/admin/api/build", map[string]any{}, map[string]string{"Origin": e.ts.URL}); r.StatusCode != 202 {
		t.Fatalf("build: %d %s", r.StatusCode, b)
	}
	_ = os.MkdirAll(filepath.Join(e.dir, "downloads", "us", "delaware"), 0o755)
	_ = os.WriteFile(filepath.Join(e.dir, "downloads", "us", "delaware", "x.osm.pbf"), make([]byte, 5000), 0o644)

	var o overviewResp
	waitFor(t, func() bool {
		r, b := e.do(c, "GET", "/admin/api/overview", nil, nil)
		if r.StatusCode != 200 {
			t.Fatalf("overview: %d %s", r.StatusCode, b)
		}
		if strings.Contains(string(b), key) {
			t.Fatal("the overview leaks a key")
		}
		o = overviewResp{}
		if err := json.Unmarshal(b, &o); err != nil {
			t.Fatal(err)
		}
		return o.Builds.Last != nil
	})
	if o.Generation == nil || o.Generation.ID != "g1" || len(o.Generation.Regions) != 1 || len(o.Generation.Regions[0].BBox) != 4 {
		t.Errorf("generation: %+v", o.Generation)
	}
	if o.Version == "" || o.System.CPUs < 1 {
		t.Errorf("version %q cpus %d", o.Version, o.System.CPUs)
	}
	if len(o.Processes) == 0 || o.Processes[0].Name != "mapthing" || o.Processes[0].PID == nil || o.Processes[0].State != "running" {
		t.Errorf("processes: %+v", o.Processes)
	}
	kinds := map[string]int{}
	for i, k := range o.Traffic.Kinds {
		kinds[k.Kind] = i
		if len(k.Series) != 60 {
			t.Errorf("%s: series of %d minutes", k.Kind, len(k.Series))
		}
	}
	for _, want := range []string{"tiles", "raster", "route", "elevation", "style", "pack"} {
		if _, ok := kinds[want]; !ok {
			t.Errorf("traffic lacks kind %q", want)
		}
	}
	tiles := o.Traffic.Kinds[kinds["tiles"]]
	if tiles.Hour.Requests != 4 || tiles.Hour.Rejected != 1 || tiles.Day.Requests != 4 || tiles.Hour.P95ms <= 0 || tiles.Hour.PerMin <= 0 {
		t.Errorf("tiles traffic: %+v (3 served and 1 refused; the internal request is not counted)", tiles)
	}
	if style := o.Traffic.Kinds[kinds["style"]]; style.Hour.Requests != 1 {
		t.Errorf("style traffic: %+v", style.Hour)
	}
	if o.Disk.TotalBytes == 0 || o.Disk.DataBytes == 0 || len(o.Disk.Parts) < 5 {
		t.Errorf("disk: %+v", o.Disk)
	}
	if !o.Disk.Guard.Known || o.Disk.Guard.OK {
		t.Errorf("guard: %+v (the estimate is larger than the disk: not ok)", o.Disk.Guard)
	}
	if !o.Builds.ScheduleEnabled || o.Builds.NextRun == nil || o.Builds.NextRun.Before(time.Now()) {
		t.Errorf("next build: %+v", o.Builds)
	}
	if o.Builds.Last.Status != "failed" || o.Builds.Last.Error == "" {
		t.Errorf("last build: %+v", o.Builds.Last)
	}
}

func TestParseProcStat(t *testing.T) {
	// A command name with spaces and parentheses, as the kernel prints it.
	line := "1234 (node (x) y) S 1 77 77 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 11 0 5000 987654321 4096 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0"
	pgrp, cpu, rss, ok := parseProcStat([]byte(line))
	if !ok || pgrp != 77 || cpu != 300 || rss != 4096 {
		t.Fatalf("pgrp=%d cpu=%d rss=%d ok=%v", pgrp, cpu, rss, ok)
	}
	if _, _, _, ok := parseProcStat([]byte("garbage")); ok {
		t.Error("garbage parsed")
	}
}

func TestProcSamplerGroups(t *testing.T) {
	dir := t.TempDir()
	write := func(pid, stat string) {
		_ = os.MkdirAll(filepath.Join(dir, pid), 0o755)
		_ = os.WriteFile(filepath.Join(dir, pid, "stat"), []byte(stat), 0o644)
	}
	stat := func(pid, pgrp, utime, rss string) string {
		return pid + " (x) S 1 " + pgrp + " " + pgrp + " 0 -1 0 0 0 0 0 " + utime + " 0 0 0 20 0 1 0 1 1 " + rss + " 0 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0"
	}
	write("10", stat("10", "10", "100", "1000")) // a child …
	write("11", stat("11", "10", "50", "500"))   // … and what it started
	write("20", stat("20", "20", "0", "7"))
	write("self", stat("1", "1", "0", "3"))
	p := newProcSampler()
	p.procFS = dir
	first := p.groups()
	page := int64(os.Getpagesize())
	if g := first[10]; g.RSSBytes != 1500*page || g.Processes != 2 || g.CPUPercent != -1 {
		t.Fatalf("group 10 first reading: %+v", g)
	}
	// Two seconds later the group has used one more CPU-second (100 ticks): 50% of a core.
	write("10", stat("10", "10", "180", "1000"))
	write("11", stat("11", "10", "70", "500"))
	p.at = p.at.Add(-2 * time.Second)
	if g := p.groups()[10]; g.CPUPercent < 45 || g.CPUPercent > 55 {
		t.Errorf("group 10 CPU: %.1f%%, want about 50", g.CPUPercent)
	}
	if u := p.own(); u.RSSBytes != 3*page {
		t.Errorf("own: %+v", u)
	}
}
