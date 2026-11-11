package server

import (
	"bufio"
	"context"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jclement/mapthing/internal/store"
	"github.com/jclement/mapthing/internal/supervisor"
	"github.com/jclement/mapthing/internal/tiles"
)

// diskStats walks /data at most once a minute.
type diskStats struct {
	dataDir string
	mu      sync.Mutex
	at      time.Time
	last    diskReport
	extras  map[string]int64 // other directories, measured with the report
}

// extra returns the size of <data>/<name>, measured when the report last was.
func (d *diskStats) extra(name string) int64 {
	d.get()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.extras[name]
}

type diskReport struct {
	Generations map[string]int64 `json:"generations"`
	Downloads   map[string]int64 `json:"downloads"` // per region
	Elevation   int64            `json:"elevation_cache"`
	Logs        int64            `json:"logs"`
	Database    int64            `json:"database"`
	TotalBytes  uint64           `json:"volume_total_bytes"`
	FreeBytes   uint64           `json:"volume_free_bytes"`
	MeasuredAt  time.Time        `json:"measured_at"`
}

func newDiskStats(dataDir string) *diskStats { return &diskStats{dataDir: dataDir} }

func (d *diskStats) get() diskReport {
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Since(d.at) < time.Minute && !d.at.IsZero() {
		return d.last
	}
	r := diskReport{Generations: map[string]int64{}, Downloads: map[string]int64{}, MeasuredAt: time.Now().UTC()}
	if ents, err := os.ReadDir(filepath.Join(d.dataDir, "generations")); err == nil {
		for _, e := range ents {
			if e.IsDir() {
				r.Generations[e.Name()] = dirSize(filepath.Join(d.dataDir, "generations", e.Name()))
			}
		}
	}
	// downloads/<region...>/ — regions may be nested (us/texas): attribute files to the
	// directory that holds them, relative to downloads/.
	dl := filepath.Join(d.dataDir, "downloads")
	_ = filepath.WalkDir(dl, func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil //nolint:nilerr // skip unreadable entries
		}
		if fi, err := de.Info(); err == nil {
			rel, _ := filepath.Rel(dl, filepath.Dir(p))
			r.Downloads[filepath.ToSlash(rel)] += fi.Size()
		}
		return nil
	})
	r.Elevation = dirSize(filepath.Join(d.dataDir, "elevation"))
	r.Logs = dirSize(filepath.Join(d.dataDir, "logs"))
	for _, f := range []string{"mapthing.db", "mapthing.db-wal", "mapthing.db-shm"} {
		if fi, err := os.Stat(filepath.Join(d.dataDir, f)); err == nil {
			r.Database += fi.Size()
		}
	}
	var st syscall.Statfs_t
	if syscall.Statfs(d.dataDir, &st) == nil {
		r.TotalBytes = st.Blocks * uint64(st.Bsize)
		r.FreeBytes = st.Bavail * uint64(st.Bsize)
	}
	d.extras = map[string]int64{}
	for _, name := range []string{"cache", "work"} {
		d.extras[name] = dirSize(filepath.Join(d.dataDir, name))
	}
	d.at, d.last = time.Now(), r
	return r
}

// reset makes the next get measure again (after something large was deleted).
func (d *diskStats) reset() {
	d.mu.Lock()
	d.at = time.Time{}
	d.mu.Unlock()
}

func dirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, de fs.DirEntry, err error) error {
		if err == nil && !de.IsDir() {
			if fi, err := de.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// rssBytes reads the resident set size from /proc (Linux); 0 elsewhere.
func rssBytes() int64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				kb, _ := strconv.ParseInt(fields[0], 10, 64)
				return kb * 1024
			}
		}
	}
	return 0
}

func (s *Server) process() map[string]any {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return map[string]any{
		"rss_bytes":      rssBytes(),
		"go_heap_bytes":  ms.HeapAlloc,
		"goroutines":     runtime.NumGoroutine(),
		"uptime_seconds": int64(time.Since(s.d.Stats.Started()).Seconds()),
		"started_at":     s.d.Stats.Started().UTC(),
	}
}

// uiKind maps a usage kind to the admin UI's request bucket ("" = not shown).
func uiKind(kind string) string {
	switch {
	case kind == "tile":
		return "tiles"
	case kind == "raster":
		return "raster"
	case kind == "pack":
		return "packs"
	case strings.HasPrefix(kind, "elevation"):
		return "elevation"
	case kind == "raster-internal":
		return ""
	default: // route, matrix, snap, isochrone, valhalla, compat
		return "routes"
	}
}

var uiKinds = []string{"tiles", "raster", "routes", "elevation", "packs"}

// memInfo reads total and used system memory from /proc/meminfo (Linux).
func memInfo() (used, total int64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer func() { _ = f.Close() }()
	vals := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) > 0 {
			n, _ := strconv.ParseInt(fields[0], 10, 64)
			vals[k] = n * 1024
		}
	}
	return vals["MemTotal"] - vals["MemAvailable"], vals["MemTotal"]
}

func pidRSS(pid int) int64 {
	if pid <= 0 {
		return 0
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				kb, _ := strconv.ParseInt(f[0], 10, 64)
				return kb * 1024
			}
		}
	}
	return 0
}

type processJSON struct {
	Name     string `json:"name"`
	State    string `json:"state"` // running | starting | restarting | stopped
	Detail   string `json:"detail,omitempty"`
	PID      *int   `json:"pid"`
	UptimeS  int64  `json:"uptime_s"`
	Restarts int    `json:"restarts"`
	RSSBytes int64  `json:"rss_bytes"` // the process and everything it started
	Healthy  bool   `json:"healthy"`
	// CPUPercent is of one core, since the previous reading; null until there are two.
	CPUPercent *float64 `json:"cpu_percent"`
	Standby    bool     `json:"standby,omitempty"` // a replacement warming up (not counted for readiness)
	Role       string   `json:"role,omitempty"`    // what it is for, in a few words
}

// processRole says what a supervised child does, for the UI.
func processRole(name string) string {
	switch {
	case name == "renderer":
		return "raster tiles (MapLibre Native and its X server)"
	case strings.HasPrefix(name, "valhalla"):
		return "routing"
	}
	return ""
}

func (s *Server) processes() []processJSON {
	out := []processJSON{}
	if s.d.Supervisor == nil {
		return out
	}
	groups := s.procs.groups()
	for _, c := range s.d.Supervisor.Statuses() {
		p := processJSON{Name: c.Name, Restarts: c.Restarts, Detail: c.Message, Healthy: c.Healthy, Standby: c.Standby, Role: processRole(c.Name)}
		switch c.State {
		case supervisor.StateRunning, supervisor.StateUnhealthy:
			p.State = "running"
		case supervisor.StateStarting:
			p.State = "starting"
		case supervisor.StateBackoff:
			p.State = "restarting"
		default: // stopped, unavailable, waiting
			p.State = "stopped"
			if p.Detail == "" {
				p.Detail = c.State
			} else {
				p.Detail = c.State + ": " + p.Detail
			}
		}
		if c.PID > 0 {
			pid := c.PID
			p.PID = &pid
			p.RSSBytes = pidRSS(pid)
			// A child leads its own process group: count what it started too.
			if u, ok := groups[pid]; ok {
				p.RSSBytes = u.RSSBytes
				if u.CPUPercent >= 0 {
					v := math.Round(u.CPUPercent*10) / 10
					p.CPUPercent = &v
				}
			}
			if c.StartedAt != nil {
				p.UptimeS = int64(time.Since(*c.StartedAt).Seconds())
			}
		}
		out = append(out, p)
	}
	return out
}

// stats: GET /admin/api/stats?days=30 (shape: docs/architecture.md, Admin API shapes).
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 366 {
		days = 30
	}
	now := time.Now().UTC()
	since := now.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	usage, err := s.d.Store.UsageSince(r.Context(), since)
	if err != nil {
		storeError(w, r, err)
		return
	}
	caches, err := s.d.Store.CacheSince(r.Context(), since)
	if err != nil {
		storeError(w, r, err)
		return
	}
	pu, pc := s.d.Stats.Pending()
	for k, n := range pu {
		usage = append(usage, store.UsageRow{KeyID: k.KeyID, Day: k.Day, Kind: k.Kind, Count: n})
	}
	for k, v := range pc {
		caches = append(caches, store.CacheRow{Cache: k.Cache, Day: k.Day, Hits: v[0], Misses: v[1]})
	}
	keyList, err := s.d.Store.Keys(r.Context())
	if err != nil {
		storeError(w, r, err)
		return
	}
	labels := map[int64]string{}
	for _, k := range keyList {
		labels[k.ID] = k.Label
	}

	daily := make([]map[string]any, days)
	dayIdx := map[string]int{}
	for i := range days {
		d := now.AddDate(0, 0, -(days - 1 - i)).Format("2006-01-02")
		dayIdx[d] = i
		row := map[string]any{"date": d}
		for _, k := range uiKinds {
			row[k] = int64(0)
		}
		daily[i] = row
	}
	byKey := map[int64]int64{}
	for _, u := range usage {
		bucket := uiKind(u.Kind)
		if bucket == "" {
			continue
		}
		if i, ok := dayIdx[u.Day]; ok {
			daily[i][bucket] = daily[i][bucket].(int64) + u.Count
			byKey[u.KeyID] += u.Count
		}
	}
	type keyRow struct {
		KeyID    string `json:"key_id"`
		Label    string `json:"label"`
		Requests int64  `json:"requests"`
	}
	keyRows := []keyRow{}
	for id, n := range byKey {
		label, ok := labels[id]
		switch {
		case id == 0:
			label = "Admin UI"
		case !ok:
			label = "deleted key"
		}
		keyRows = append(keyRows, keyRow{KeyID: strconv.FormatInt(id, 10), Label: label, Requests: n})
	}
	sort.Slice(keyRows, func(i, j int) bool { return keyRows[i].Requests > keyRows[j].Requests })

	type cacheRow struct {
		Name      string `json:"name"`
		Hits      int64  `json:"hits"`
		Misses    int64  `json:"misses"`
		SizeBytes int64  `json:"size_bytes"`
	}
	var sizes map[string]int64
	if s.d.Tiles != nil {
		sizes = s.d.Tiles.CacheSizes()
	}
	cacheMap := map[string]*cacheRow{}
	for _, name := range []string{"raster", "imagery", "pack"} {
		cacheMap[name] = &cacheRow{Name: name, SizeBytes: sizes[name]}
	}
	for _, c := range caches {
		cr := cacheMap[c.Cache]
		if cr == nil {
			cr = &cacheRow{Name: c.Cache, SizeBytes: sizes[c.Cache]}
			cacheMap[c.Cache] = cr
		}
		cr.Hits += c.Hits
		cr.Misses += c.Misses
	}
	cacheRows := []cacheRow{}
	for _, cr := range cacheMap {
		cacheRows = append(cacheRows, *cr)
	}
	sort.Slice(cacheRows, func(i, j int) bool { return cacheRows[i].Name < cacheRows[j].Name })

	disk := s.diskCtl.get()
	type idBytes struct {
		ID    string `json:"id"`
		Name  string `json:"name,omitempty"`
		Bytes int64  `json:"bytes"`
	}
	gens := []idBytes{}
	for id, n := range disk.Generations {
		gens = append(gens, idBytes{ID: id, Bytes: n})
	}
	sort.Slice(gens, func(i, j int) bool { return gens[i].ID > gens[j].ID })
	names := map[string]string{}
	if rs, err := s.d.Store.Regions(r.Context()); err == nil {
		for _, rg := range rs {
			names[rg.ID] = rg.Name
		}
	}
	regions := []idBytes{}
	var downloads int64
	for id, n := range disk.Downloads {
		downloads += n
		name := names[id]
		if name == "" {
			name = id
		}
		regions = append(regions, idBytes{ID: id, Name: name, Bytes: n})
	}
	sort.Slice(regions, func(i, j int) bool { return regions[i].Bytes > regions[j].Bytes })
	used, total := memInfo()

	writeJSON(w, http.StatusOK, map[string]any{
		"daily":  daily,
		"by_key": keyRows,
		"cache":  cacheRows,
		"disk": map[string]any{
			"generations":           gens,
			"regions":               regions,
			"downloads_bytes":       downloads,
			"elevation_cache_bytes": disk.Elevation,
			"imagery_cache_bytes":   sizes["imagery"],
			"raster_cache_bytes":    sizes["raster"],
			"pack_cache_bytes":      sizes["pack"],
			"database_bytes":        disk.Database,
			"free_bytes":            disk.FreeBytes,
			"total_bytes":           disk.TotalBytes,
		},
		"memory":    map[string]any{"process_rss_bytes": rssBytes(), "system_used_bytes": used, "system_total_bytes": total},
		"processes": s.processes(),
	})
}

// Attribution is the data attribution shown in the UI footer and the README.
var Attribution = []map[string]string{
	{"name": "OpenStreetMap contributors", "license": "ODbL 1.0", "url": "https://www.openstreetmap.org/copyright"},
	{"name": "USGS 3D Elevation Program", "license": "Public domain", "url": "https://www.usgs.gov/3d-elevation-program"},
	{"name": "Natural Resources Canada", "license": "Open Government Licence – Canada", "url": "https://open.canada.ca/en/open-government-licence-canada"},
	{"name": "Province of British Columbia", "license": "Open Government Licence – British Columbia", "url": "https://www2.gov.bc.ca/gov/content/data/open-data/open-government-licence-bc"},
	{"name": "Government of Alberta", "license": "Open Government Licence – Alberta", "url": "https://open.alberta.ca/licence"},
	{"name": "Copernicus DEM", "license": "Copernicus DEM licence", "url": "https://spacedata.copernicus.eu/", "note": "© DLR e.V. 2010–2014 and © Airbus Defence and Space GmbH 2014–2018"},
}

// Commit and BuildDate are set at build time with -ldflags -X (else read from the Go build info).
var (
	Commit    = ""
	BuildDate = ""
)

func buildInfo() (commit, date string) {
	commit, date = Commit, BuildDate
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, st := range bi.Settings {
			switch st.Key {
			case "vcs.revision":
				if commit == "" {
					commit = st.Value
				}
			case "vcs.time":
				if date == "" {
					date = st.Value
				}
			}
		}
	}
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return commit, date
}

type readinessJSON struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// readiness evaluates the same checks as /readyz.
func (s *Server) readiness(ctx context.Context) (bool, []readinessJSON) {
	out := []readinessJSON{}
	ready := true
	if s.d.Tiles != nil {
		gen := s.d.Tiles.Generation()
		ok := s.d.Tiles.Ready()
		detail := "live generation " + gen
		if !ok {
			detail = "no map data has been built yet"
		}
		out = append(out, readinessJSON{Name: "map data", OK: ok, Detail: detail})
		ready = ready && ok
	}
	if s.d.Supervisor != nil {
		for _, c := range s.d.Supervisor.Statuses() {
			ok := c.State == supervisor.StateRunning && c.Healthy
			detail := c.State
			if c.Message != "" {
				detail += ": " + c.Message
			}
			if c.State != supervisor.StateUnavailable && !c.Standby {
				ready = ready && ok
			}
			out = append(out, readinessJSON{Name: c.Name, OK: ok, Detail: detail})
		}
	}
	names := make([]string, 0, len(s.d.ReadyChecks))
	for n := range s.d.ReadyChecks {
		names = append(names, n)
	}
	sort.Strings(names)
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for _, n := range names {
		ok := s.d.ReadyChecks[n](cctx)
		ready = ready && ok
		out = append(out, readinessJSON{Name: n, OK: ok})
	}
	return ready, out
}

// system: GET /admin/api/system (also the UI's session probe).
func (s *Server) system(w http.ResponseWriter, r *http.Request) {
	disk := s.diskCtl.get()
	commit, date := buildInfo()
	ready, readiness := s.readiness(r.Context())
	components := s.components()
	var gen any
	if g := s.generation(); g != "" {
		gen = g
	}
	resp := map[string]any{
		"version":            Version,
		"commit":             commit,
		"build_date":         date,
		"components":         components,
		"ready":              ready,
		"readiness":          readiness,
		"disk":               map[string]any{"free_bytes": disk.FreeBytes, "total_bytes": disk.TotalBytes},
		"current_generation": gen,
		"attribution":        Attribution,
		"imagery_source":     map[string]any{"name": s.imageryName(), "attribution": tiles.ImageryAttribution()},
		// extras
		"styles":   tiles.Styles(s.d.Assets),
		"platform": runtime.GOOS + "/" + runtime.GOARCH,
		"data_dir": s.d.DataDir,
		"process":  s.process(),
	}
	for k, v := range s.d.Info {
		resp[k] = v
	}
	if s.d.ElevationSources != nil {
		resp["elevation_sources"] = s.d.ElevationSources()
	}
	writeJSON(w, http.StatusOK, resp)
}

// Component is one piece of software in the running image and its version.
type Component struct {
	Name    string `json:"name"`
	Version string `json:"version"`          // "" when it could not be determined
	Detail  string `json:"detail,omitempty"` // what it is for, or where the version came from
}

// components lists mapthing, Go and whatever the image carries (Deps.Versions).
func (s *Server) components() []Component {
	commit, date := buildInfo()
	detail := "this server"
	if commit != "" {
		detail = "commit " + commit
		if date != "" {
			detail += ", built " + date
		}
	}
	out := []Component{
		{Name: "mapthing", Version: Version, Detail: detail},
		{Name: "Go", Version: strings.TrimPrefix(runtime.Version(), "go"), Detail: "the language mapthing is built with"},
	}
	if s.d.Versions != nil {
		out = append(out, s.d.Versions()...)
	}
	return out
}

// settings: GET /admin/api/settings → {settings:[…], versions:[…]}: the effective configuration,
// each value with where it came from, secrets masked; and the software versions. Read-only.
func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	if s.d.Settings == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "this server does not report its settings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": s.d.Settings(), "versions": s.components()})
}

func (s *Server) imageryName() string {
	if os.Getenv("MAPTHING_IMAGERY_URL") != "" {
		return "Custom imagery"
	}
	return "USGS The National Map / Sentinel-2 cloudless"
}

func (s *Server) generation() string {
	if s.d.Tiles == nil {
		return ""
	}
	return s.d.Tiles.Generation()
}
