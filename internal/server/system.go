package server

import (
	"bufio"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jclement/mapthing/internal/store"
	"github.com/jclement/mapthing/internal/tiles"
)

// diskStats walks /data at most once a minute.
type diskStats struct {
	dataDir string
	mu      sync.Mutex
	at      time.Time
	last    diskReport
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
			return nil
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
	d.at, d.last = time.Now(), r
	return r
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

type dayRow struct {
	Day    string           `json:"day"`
	Total  int64            `json:"total"`
	ByKind map[string]int64 `json:"by_kind"`
}

type keyRow struct {
	KeyID  int64            `json:"key_id"`
	Label  string           `json:"label"`
	Prefix string           `json:"prefix,omitempty"`
	Total  int64            `json:"total"`
	ByKind map[string]int64 `json:"by_kind"`
}

type cacheRow struct {
	Hits    int64   `json:"hits"`
	Misses  int64   `json:"misses"`
	HitRate float64 `json:"hit_rate"`
	Bytes   int64   `json:"bytes"`
}

// stats: GET /admin/api/stats?days=30
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 366 {
		days = 30
	}
	since := time.Now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
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
	labels := map[int64]*store.APIKey{}
	for _, k := range keyList {
		labels[k.ID] = k
	}

	byDay := map[string]*dayRow{}
	byKey := map[int64]*keyRow{}
	byKind := map[string]int64{}
	var total int64
	for _, u := range usage {
		if u.Day < since {
			continue
		}
		d := byDay[u.Day]
		if d == nil {
			d = &dayRow{Day: u.Day, ByKind: map[string]int64{}}
			byDay[u.Day] = d
		}
		d.Total += u.Count
		d.ByKind[u.Kind] += u.Count
		k := byKey[u.KeyID]
		if k == nil {
			k = &keyRow{KeyID: u.KeyID, ByKind: map[string]int64{}, Label: "admin session"}
			if ak := labels[u.KeyID]; ak != nil {
				k.Label, k.Prefix = ak.Label, ak.Prefix
			} else if u.KeyID != 0 {
				k.Label = "deleted key"
			}
			byKey[u.KeyID] = k
		}
		k.Total += u.Count
		k.ByKind[u.Kind] += u.Count
		byKind[u.Kind] += u.Count
		total += u.Count
	}
	// a dense day series (zeros included) is easier to chart
	dayList := make([]*dayRow, 0, days)
	for i := days - 1; i >= 0; i-- {
		day := time.Now().UTC().AddDate(0, 0, -i).Format("2006-01-02")
		if d := byDay[day]; d != nil {
			dayList = append(dayList, d)
		} else {
			dayList = append(dayList, &dayRow{Day: day, ByKind: map[string]int64{}})
		}
	}
	keyRows := make([]*keyRow, 0, len(byKey))
	for _, k := range byKey {
		keyRows = append(keyRows, k)
	}
	sort.Slice(keyRows, func(i, j int) bool { return keyRows[i].Total > keyRows[j].Total })

	cacheOut := map[string]*cacheRow{}
	for _, name := range []string{"raster", "imagery", "pack"} {
		cacheOut[name] = &cacheRow{}
	}
	for _, c := range caches {
		if c.Day < since {
			continue
		}
		cr := cacheOut[c.Cache]
		if cr == nil {
			cr = &cacheRow{}
			cacheOut[c.Cache] = cr
		}
		cr.Hits += c.Hits
		cr.Misses += c.Misses
	}
	var sizes map[string]int64
	if s.d.Tiles != nil {
		sizes = s.d.Tiles.CacheSizes()
	}
	for name, cr := range cacheOut {
		if n := cr.Hits + cr.Misses; n > 0 {
			cr.HitRate = float64(cr.Hits) / float64(n)
		}
		cr.Bytes = sizes[name]
	}
	sinceStart, _ := s.d.Stats.Totals()
	resp := map[string]any{
		"days":          days,
		"since":         since,
		"total":         total,
		"by_day":        dayList,
		"by_key":        keyRows,
		"by_kind":       byKind,
		"since_start":   sinceStart,
		"cache":         cacheOut,
		"disk":          s.diskCtl.get(),
		"process":       s.process(),
		"subprocesses":  s.children(),
		"generation":    s.generation(),
		"sse_listeners": s.d.Jobs.Hub().Subscribers(),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) children() any {
	if s.d.Supervisor == nil {
		return []any{}
	}
	return s.d.Supervisor.Statuses()
}

func (s *Server) generation() string {
	if s.d.Tiles == nil {
		return ""
	}
	return s.d.Tiles.Generation()
}

// Attribution is the data attribution shown in the UI footer and the README.
var Attribution = []map[string]string{
	{"name": "OpenStreetMap contributors", "license": "ODbL 1.0", "url": "https://www.openstreetmap.org/copyright"},
	{"name": "USGS 3D Elevation Program", "license": "Public domain", "url": "https://www.usgs.gov/3d-elevation-program"},
	{"name": "Natural Resources Canada", "license": "Open Government Licence – Canada", "url": "https://open.canada.ca/en/open-government-licence-canada"},
	{"name": "Province of British Columbia", "license": "Open Government Licence – British Columbia", "url": "https://www2.gov.bc.ca/gov/content/data/open-data/open-government-licence-bc"},
	{"name": "Government of Alberta", "license": "Open Government Licence – Alberta", "url": "https://open.alberta.ca/licence"},
	{"name": "Copernicus DEM © DLR e.V. 2010–2014 and © Airbus Defence and Space GmbH 2014–2018", "license": "Copernicus DEM licence", "url": "https://spacedata.copernicus.eu/"},
}

// system: GET /admin/api/system
func (s *Server) system(w http.ResponseWriter, r *http.Request) {
	disk := s.diskCtl.get()
	resp := map[string]any{
		"version":      Version,
		"go_version":   runtime.Version(),
		"platform":     runtime.GOOS + "/" + runtime.GOARCH,
		"process":      s.process(),
		"disk":         map[string]any{"total_bytes": disk.TotalBytes, "free_bytes": disk.FreeBytes},
		"data_dir":     s.d.DataDir,
		"generation":   s.generation(),
		"subprocesses": s.children(),
		"styles":       tiles.Styles(s.d.Assets),
		"attribution":  Attribution,
		"imagery":      map[string]any{"attribution": tiles.ImageryAttribution()},
		"build_job":    s.d.Jobs.Registered("build"),
	}
	if s.d.ElevationSources != nil {
		resp["elevation_sources"] = s.d.ElevationSources()
	}
	writeJSON(w, http.StatusOK, resp)
}
