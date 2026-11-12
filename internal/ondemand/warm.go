package ondemand

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jclement/mapthing/internal/elevation"
)

// Warming fills the cache for an area ahead of time, so the area keeps
// working with no internet: every on-demand tile of the area, down to a
// chosen zoom, is fetched once and kept. It runs as a job; an estimate says
// first how much it will fetch and whether the cache can hold it.

// MaxWarmTiles bounds one warm job (about a million square kilometres at
// zoom 13 in mid latitudes). A larger area is better served by building the
// region.
const MaxWarmTiles = 250_000

// Planning figures per tile, measured against the live hosts (docs/sizing.md,
// "On-demand terrain"): what a finished tile takes in the cache, what is
// downloaded to make it, and how long a tile takes one worker.
const (
	warmTileBytes     = 45_000
	warmDownloadBytes = 600_000
	warmTileSeconds   = 0.45
	warmWorkers       = 6
)

// WarmRequest names an area and how deep to fill it.
type WarmRequest struct {
	// BBox is west, south, east, north.
	BBox elevation.BBox `json:"bbox"`
	// Rings optionally narrows the box to an outline (a region's boundary).
	Rings [][][2]float64 `json:"-"`
	// Label says what is being warmed, for the job's log ("Alberta", "a drawn box").
	Label string `json:"label,omitempty"`
	// MaxZoom is the deepest zoom to fill (MinZoom..MaxZoom).
	MaxZoom int `json:"max_zoom"`
}

// ErrWarm is a request that cannot be warmed as asked.
type ErrWarm struct{ Msg string }

func (e *ErrWarm) Error() string { return e.Msg }

func (r WarmRequest) validate() error {
	b := r.BBox
	if !(b[0] < b[2] && b[1] < b[3]) || b[0] < -180 || b[2] > 180 || b[1] < -90 || b[3] > 90 {
		return &ErrWarm{"bbox must be [west, south, east, north] with west < east and south < north"}
	}
	if r.MaxZoom < MinZoom || r.MaxZoom > MaxZoom {
		return &ErrWarm{fmt.Sprintf("max_zoom must be between %d and %d", MinZoom, MaxZoom)}
	}
	return nil
}

type warmTile struct {
	z, x, y int
	contour bool // a contour tile, drawn from the terrain tiles warmed before it
}

// warmContourBytes is the planning figure for a contour tile in the cache.
const warmContourBytes = 12_000

// tiles lists the tiles of the request that on demand would serve: inside the
// area, where the policy applies, and not already built.
func (m *Manager) warmTiles(r WarmRequest) ([]warmTile, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	p := m.Svc.Policy()
	built := m.built.Load()
	var out []warmTile
	for z := MinZoom; z <= r.MaxZoom; z++ {
		x0, y0, x1, y1 := elevation.TileRange(z, r.BBox[0], r.BBox[1], r.BBox[2], r.BBox[3])
		if n := (x1 - x0 + 1) * (y1 - y0 + 1); len(out)+n > 4*MaxWarmTiles {
			return nil, &ErrWarm{fmt.Sprintf("that area is too large to warm to zoom %d (more than %d tiles); choose a smaller area or a lower zoom, or build the region", r.MaxZoom, MaxWarmTiles)}
		}
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				w, s, e, n := elevation.TileBounds(z, x, y)
				box := elevation.BBox{w, s, e, n}
				if len(r.Rings) > 0 && !elevation.RingsIntersectBox(r.Rings, box) {
					continue
				}
				if !p.AllowsBox(box) {
					continue
				}
				if built != nil && (*built)(z, x, y) {
					continue
				}
				out = append(out, warmTile{z: z, x: x, y: y})
			}
		}
	}
	// Contours go with terrain: an area taken offline without its contour lines would be a
	// trap. Each contour zoom is warmed when the terrain it is drawn from is.
	for z := ContourMinZoom; z <= ContourMaxZoom; z++ {
		tz := ContourTerrainZoom(z)
		if tz > r.MaxZoom {
			continue
		}
		span := 1 << (tz - z)
		x0, y0, x1, y1 := elevation.TileRange(z, r.BBox[0], r.BBox[1], r.BBox[2], r.BBox[3])
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				w, s, e, n := elevation.TileBounds(z, x, y)
				if len(r.Rings) > 0 && !elevation.RingsIntersectBox(r.Rings, elevation.BBox{w, s, e, n}) {
					continue
				}
				// Where terrain was built, so were its contours.
				if !m.AllowsContour(z, x, y) || (built != nil && (*built)(tz, x*span, y*span)) {
					continue
				}
				out = append(out, warmTile{z, x, y, true})
			}
		}
	}
	if len(out) > MaxWarmTiles {
		return nil, &ErrWarm{fmt.Sprintf("that area is too large to warm to zoom %d (%d tiles, at most %d); choose a smaller area or a lower zoom, or build the region", r.MaxZoom, len(out), MaxWarmTiles)}
	}
	return out, nil
}

// WarmEstimate says what a warm job would do, before it is started.
type WarmEstimate struct {
	// Tiles is how many tiles the area holds that on demand serves; Cached of
	// them are in the cache already and ToFetch are not.
	Tiles   int `json:"tiles"`
	Cached  int `json:"cached"`
	ToFetch int `json:"to_fetch"`
	// CacheBytes is how much the cache would grow; DownloadBytes how much
	// would be read from the DEM hosts; Seconds how long it would take.
	CacheBytes    int64   `json:"cache_bytes"`
	DownloadBytes int64   `json:"download_bytes"`
	Seconds       float64 `json:"seconds"`
	// CacheMaxBytes and CacheUsedBytes are the cache's budget and use now.
	CacheMaxBytes  int64 `json:"cache_max_bytes"`
	CacheUsedBytes int64 `json:"cache_used_bytes"`
	// Fits is false when the cache's budget cannot hold the area: warming it
	// would push out its own first tiles. NeedCacheMB is then the budget that
	// would hold it (with what is cached now).
	Fits        bool  `json:"fits"`
	NeedCacheMB int64 `json:"need_cache_mb,omitempty"`
	// Applies is false when on demand applies nowhere in the area (nothing to do).
	Applies bool `json:"applies"`
}

// EstimateWarm counts the tiles of a request and what fetching them costs. It
// asks the DEM hosts nothing.
func (m *Manager) EstimateWarm(r WarmRequest) (WarmEstimate, error) {
	tiles, err := m.warmTiles(r)
	if err != nil {
		return WarmEstimate{}, err
	}
	e := WarmEstimate{Tiles: len(tiles), CacheMaxBytes: m.cache.Max(), CacheUsedBytes: m.cache.Size(), Applies: len(tiles) > 0}
	var terrain, terrainCold, contourCold int64
	for _, t := range tiles {
		switch {
		case t.contour:
			if m.HasContour(t.z, t.x, t.y) {
				e.Cached++
			} else {
				contourCold++
			}
		default:
			terrain++
			if m.Svc.CachedNow(t.z, t.x, t.y) {
				e.Cached++
			} else {
				terrainCold++
			}
		}
	}
	e.ToFetch = e.Tiles - e.Cached
	e.CacheBytes = terrainCold*warmTileBytes + contourCold*warmContourBytes
	e.DownloadBytes = terrainCold * warmDownloadBytes
	e.Seconds = math.Ceil((float64(terrainCold)*warmTileSeconds + float64(contourCold)*0.05) / warmWorkers)
	// The whole area has to fit at once, beside nothing else: everything else
	// in the cache is older and would go first.
	whole := (terrain*warmTileBytes + (int64(e.Tiles)-terrain)*warmContourBytes) * 12 / 10
	e.Fits = whole <= e.CacheMaxBytes
	if !e.Fits {
		e.NeedCacheMB = (whole + 999_999) / 1_000_000
	}
	return e, nil
}

// WarmProgress reports a warm job's state.
type WarmProgress struct {
	Done, Total     int
	Fetched, Cached int
	Empty, Failed   int
}

// WarmResult is how a warm job ended.
type WarmResult struct {
	WarmProgress
	Bytes    int64         // read from the DEM hosts
	Duration time.Duration // wall time
}

type warmKey struct{}

// warming reports whether ctx belongs to a warm job: source blocks are then
// not kept in the cache (only the finished tiles are wanted there, and the
// blocks are twenty times their size).
func warming(ctx context.Context) bool {
	v, _ := ctx.Value(warmKey{}).(bool)
	return v
}

// Warm fetches every tile of the request that is not cached, shallowest zoom
// first, and reports progress. A source that stops answering ends the job
// with an error; what was fetched stays cached, and running it again resumes.
func (m *Manager) Warm(ctx context.Context, r WarmRequest, progress func(WarmProgress)) (WarmResult, error) {
	tiles, err := m.warmTiles(r)
	if err != nil {
		return WarmResult{}, err
	}
	start := time.Now()
	_, bytes0, _ := m.Svc.up.Counters()
	wctx, cancel := context.WithCancel(context.WithValue(ctx, warmKey{}, true))
	defer cancel()
	var (
		done, fetched, cached, empty, failed atomic.Int64
		streak                               atomic.Int64 // failures in a row
		mu                                   sync.Mutex
		firstErr                             error
		wg                                   sync.WaitGroup
	)
	snapshot := func() WarmProgress {
		return WarmProgress{Done: int(done.Load()), Total: len(tiles), Fetched: int(fetched.Load()), Cached: int(cached.Load()),
			Empty: int(empty.Load()), Failed: int(failed.Load())}
	}
	jobs := make(chan warmTile)
	for i := 0; i < warmWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				var png []byte
				var cold bool
				var err error
				if t.contour {
					png, cold, err = m.Contour(wctx, t.z, t.x, t.y)
				} else {
					png, cold, err = m.Svc.Tile(wctx, t.z, t.x, t.y)
				}
				switch {
				case err != nil && wctx.Err() != nil:
					return
				case err != nil:
					failed.Add(1)
					// A few failures are the hosts' bad moments; a run of them is an outage.
					if streak.Add(1) >= 25 {
						mu.Lock()
						if firstErr == nil {
							firstErr = fmt.Errorf("stopped after %d tiles failed in a row: %w", 25, err)
							cancel()
						}
						mu.Unlock()
						return
					}
				default:
					streak.Store(0)
					switch {
					case len(png) == 0:
						empty.Add(1)
					case cold:
						fetched.Add(1)
					default:
						cached.Add(1)
					}
				}
				if d := done.Add(1); progress != nil && (d%20 == 0 || int(d) == len(tiles)) {
					progress(snapshot())
				}
			}
		}()
	}
feed:
	for _, t := range tiles {
		select {
		case jobs <- t:
		case <-wctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	// A stopped job's workers have left; the shared work they were waiting on is being
	// cancelled. The job is not over until that work is: only then may another start.
	for end := time.Now().Add(30 * time.Second); m.Svc.flight.warmActive() > 0 && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond)
	}
	_, bytes1, _ := m.Svc.up.Counters()
	res := WarmResult{WarmProgress: snapshot(), Bytes: bytes1 - bytes0, Duration: time.Since(start)}
	mu.Lock()
	err = firstErr
	mu.Unlock()
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && res.Failed > 0 {
		err = fmt.Errorf("%d of %d tiles could not be fetched; run it again to fetch them", res.Failed, res.Total)
	}
	var lim *ErrLimited
	if errors.As(err, &lim) {
		err = fmt.Errorf("held back by a rate limit: %w", err)
	}
	return res, err
}

// WarmStatus is the state of the warm job (one runs at a time), for the admin pages.
type WarmStatus struct {
	// State is idle (none has run since the server started), running, done,
	// failed or cancelled.
	State   string         `json:"state"`
	Label   string         `json:"label,omitempty"`
	BBox    elevation.BBox `json:"bbox"`
	MaxZoom int            `json:"max_zoom,omitempty"`
	Total   int            `json:"total"`
	Done    int            `json:"done"`
	Fetched int            `json:"fetched"`
	Cached  int            `json:"cached"`
	Empty   int            `json:"empty"`
	Failed  int            `json:"failed"`
	// Bytes is what was read from the DEM hosts.
	Bytes      int64      `json:"bytes"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// ErrWarmBusy is returned when a warm job is already running.
var ErrWarmBusy = errors.New("a warm job is already running")

// Region returns the on-demand region with the given id, for warming it.
func (m *Manager) Region(id string) (Area, bool) {
	for _, a := range m.Svc.Policy().Areas {
		if a.ID == id {
			return a, true
		}
	}
	return Area{}, false
}

// StartWarm starts warming an area in the background. It refuses an area on
// demand does not apply to, one the cache cannot hold, and a second job while
// one runs.
func (m *Manager) StartWarm(r WarmRequest) (WarmStatus, error) {
	est, err := m.EstimateWarm(r)
	if err != nil {
		return WarmStatus{}, err
	}
	switch {
	case !est.Applies:
		return WarmStatus{}, &ErrWarm{"on-demand terrain does not apply anywhere in that area (or everything there is built), so there is nothing to warm"}
	case !est.Fits:
		return WarmStatus{}, &ErrWarm{fmt.Sprintf("the cache cannot hold that area: raise the cache size to at least %d MB first, or choose a smaller area or a lower zoom", est.NeedCacheMB)}
	}
	m.warmMu.Lock()
	defer m.warmMu.Unlock()
	if m.warm.State == "running" {
		return m.warm, ErrWarmBusy
	}
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	m.warmCancel = cancel
	m.warm = WarmStatus{State: "running", Label: r.Label, BBox: r.BBox, MaxZoom: r.MaxZoom, Total: est.Tiles, StartedAt: &now}
	go func() {
		defer cancel()
		res, err := m.Warm(ctx, r, func(p WarmProgress) {
			m.warmMu.Lock()
			m.warm.Done, m.warm.Fetched, m.warm.Cached, m.warm.Empty, m.warm.Failed = p.Done, p.Fetched, p.Cached, p.Empty, p.Failed
			m.warmMu.Unlock()
		})
		end := time.Now().UTC()
		m.warmMu.Lock()
		defer m.warmMu.Unlock()
		w := &m.warm
		w.Total, w.Done, w.Fetched, w.Cached, w.Empty, w.Failed = res.Total, res.Done, res.Fetched, res.Cached, res.Empty, res.Failed
		w.Bytes, w.FinishedAt = res.Bytes, &end
		switch {
		case err == nil:
			w.State = "done"
		case errors.Is(err, context.Canceled):
			w.State = "cancelled"
		default:
			w.State, w.Error = "failed", err.Error()
		}
		if m.opt.Logf != nil {
			m.opt.Logf("on-demand terrain: warming %s to z%d %s: %d of %d tiles, %d fetched (%.1f MB), %d failed, in %s",
				r.Label, r.MaxZoom, w.State, res.Done, res.Total, res.Fetched, float64(res.Bytes)/1e6, res.Failed, res.Duration.Round(time.Second))
		}
	}()
	return m.warm, nil
}

// CancelWarm stops the running warm job, if any. What it fetched stays cached.
func (m *Manager) CancelWarm() {
	m.warmMu.Lock()
	defer m.warmMu.Unlock()
	if m.warm.State == "running" && m.warmCancel != nil {
		m.warmCancel()
	}
}

// WarmStatus returns the state of the latest warm job.
func (m *Manager) WarmStatus() WarmStatus {
	m.warmMu.Lock()
	defer m.warmMu.Unlock()
	w := m.warm
	if w.State == "" {
		w.State = "idle"
	}
	return w
}
