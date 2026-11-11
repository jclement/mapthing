package server

import (
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/jclement/mapthing/internal/scheduler"
	"github.com/jclement/mapthing/internal/tiles"
)

type diskPart struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// overview: GET /admin/api/overview → everything the admin home page shows, in one call:
// what is live, the health of every process, traffic by kind, memory, CPU and disk, and the
// next and last build. Shape: docs/architecture.md, "Admin API shapes".
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ready, readiness := s.readiness(ctx)
	commit, _ := buildInfo()

	// What is live.
	var generation any
	if id, dir, ok := tiles.CurrentGeneration(s.d.DataDir); ok {
		m := normaliseManifest(dir)
		g := map[string]any{"id": id, "regions": m.Regions, "bytes": m.Sizes["total"], "sizes": m.Sizes}
		if pm, _ := readManifest(dir); !pm.CreatedAt.IsZero() {
			g["created_at"] = pm.CreatedAt.UTC()
		} else if fi, err := os.Stat(filepath.Join(dir, "manifest.json")); err == nil {
			g["created_at"] = fi.ModTime().UTC()
		}
		generation = g
	}
	regions := 0
	if rs, err := s.d.Store.Regions(ctx); err == nil {
		regions = len(rs)
	}

	// Processes: the children, then this process, then whatever a build is running.
	procs := s.processes()
	own := s.procs.own()
	self := processJSON{Name: "mapthing", State: "running", Healthy: true, RSSBytes: own.RSSBytes,
		UptimeS: int64(time.Since(s.d.Stats.Started()).Seconds()), Role: "API, tiles, elevation, jobs, this page"}
	pid := os.Getpid()
	self.PID = &pid
	if own.CPUPercent >= 0 {
		v := math.Round(own.CPUPercent*10) / 10
		self.CPUPercent = &v
	}
	all := append([]processJSON{self}, procs...)
	if g, ok := s.procs.groups()[os.Getpid()]; ok && g.Processes > 1 {
		// Tools a build runs (Planetiler, Valhalla's builders, GDAL) are in this process's group.
		t := processJSON{Name: "build tools", State: "running", Healthy: true,
			RSSBytes: max(g.RSSBytes-own.RSSBytes, 0), Role: "Planetiler, Valhalla and GDAL, while a build runs"}
		if g.CPUPercent >= 0 && own.CPUPercent >= 0 {
			v := math.Round(max(g.CPUPercent-own.CPUPercent, 0)*10) / 10
			t.CPUPercent = &v
		}
		all = append(all, t)
	}
	used, total := memInfo()

	// Disk: the volume, what mapthing keeps on it, and what the next build needs free.
	disk := s.diskCtl.get()
	var sizes map[string]int64
	if s.d.Tiles != nil {
		sizes = s.d.Tiles.CacheSizes()
	}
	var gens, downloads int64
	for _, n := range disk.Generations {
		gens += n
	}
	for _, n := range disk.Downloads {
		downloads += n
	}
	parts := []diskPart{
		{"Generations", gens}, {"Downloaded extracts", downloads}, {"Elevation cache", disk.Elevation},
		{"Imagery cache", sizes["imagery"]}, {"Raster tile cache", sizes["raster"]}, {"Offline pack cache", sizes["pack"]},
		{"Build caches and scratch", dirSizeCached(s, "cache") + dirSizeCached(s, "work")},
		{"Logs", disk.Logs}, {"Database", disk.Database},
	}
	var data int64
	for _, p := range parts {
		data += p.Bytes
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].Bytes > parts[j].Bytes })
	guard := map[string]any{"known": false}
	if s.d.BuildEstimate != nil {
		if need, ok := s.d.BuildEstimate(ctx); ok {
			guard = map[string]any{"known": true, "need_bytes": need, "ok": int64(disk.FreeBytes) >= need}
		}
	}

	// Builds: the schedule, what is running, and how the last one went.
	st := s.d.Scheduler.State()
	builds := map[string]any{"schedule_enabled": st.Enabled, "next_run": st.NextRun, "cron": st.Cron, "active": nil, "last": nil}
	if cur, ok := s.d.Jobs.Current(); ok && cur.Name == scheduler.BuildJob {
		builds["active"] = s.jobView(cur)
	}
	if js, err := s.d.Store.Jobs(ctx, 20); err == nil {
		for _, j := range js {
			if j.Name != scheduler.BuildJob || j.FinishedAt == nil {
				continue
			}
			v := s.jobView(*j)
			last := map[string]any{"id": v.ID, "status": v.Status, "trigger": v.Trigger, "started_at": v.StartedAt,
				"finished_at": v.FinishedAt, "error": v.Error, "generation_id": v.GenerationID}
			if j.StartedAt != nil {
				last["duration_s"] = math.Round(j.FinishedAt.Sub(*j.StartedAt).Seconds())
			}
			builds["last"] = last
			break
		}
	}

	started := s.d.Stats.Started()
	writeJSON(w, http.StatusOK, map[string]any{
		"version": Version, "commit": commit, "started_at": started.UTC(),
		"ready": ready, "readiness": readiness,
		"generation": generation, "regions_installed": regions,
		"processes": all,
		"system": map[string]any{
			"cpus": cpuCount(), "load1": loadAvg(), "memory_used_bytes": used, "memory_total_bytes": total,
			"platform": runtime.GOOS + "/" + runtime.GOARCH,
		},
		"traffic": map[string]any{"since": started.UTC(), "kinds": s.d.Stats.Traffic.Snapshot(started)},
		"disk": map[string]any{
			"total_bytes": disk.TotalBytes, "free_bytes": disk.FreeBytes, "data_bytes": data, "parts": parts,
			"guard": guard, "measured_at": disk.MeasuredAt,
		},
		"builds": builds,
	})
}

// dirSizeCached measures a directory under the data directory, at most once a minute (with
// the rest of the disk report).
func dirSizeCached(s *Server, name string) int64 {
	return s.diskCtl.extra(name)
}
