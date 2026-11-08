package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jclement/mapthing/internal/elevation"
)

// A piece is one source file window the build fetches into the cache: a 1°
// cell (clipped to the build area) of one source COG.
type piece struct {
	source string
	key    string // unique within the source, e.g. "n39w076" or "3_4-n56w119"
	url    string // COG URL
	window elevation.BBox
}

// demSource knows how to find the COG(s) covering a 1° cell.
type demSource struct {
	def elevation.SourceDef
	// native resolution in the source CRS units (degrees or metres)
	nativeRes  float64
	geographic bool
	// cellURLs returns COG URLs (with a key suffix) for the cell whose
	// south-west corner is (lon, lat). nil = no data there.
	cellURLs func(ctx context.Context, f *fetcher, lon, lat int) ([]cellRef, error)
}

type cellRef struct {
	key string
	url string
}

const (
	url3DEP   = "https://prd-tnm.s3.amazonaws.com/StagedProducts/Elevation/13/TIFF/current/%s/USGS_13_%s.tif"
	urlGLO30  = "https://copernicus-dem-30m.s3.amazonaws.com/%[1]s/%[1]s.tif"
	urlMRDEM  = "https://canelevation-dem.s3.ca-central-1.amazonaws.com/mrdem-30/mrdem-30-dtm.tif"
	stacHRDEM = "https://datacube.services.geo.ca/stac/api/search"
)

func cellName3DEP(lon, lat int) string {
	// 3DEP names tiles by their north-west corner.
	return fmt.Sprintf("n%02dw%03d", lat+1, -lon)
}

func cellNameGLO30(lon, lat int) string {
	ns, ew := "N", "E"
	if lat < 0 {
		ns = "S"
	}
	if lon < 0 {
		ew = "W"
	}
	return fmt.Sprintf("Copernicus_DSM_COG_10_%s%02d_00_%s%03d_00_DEM", ns, abs(lat), ew, abs(lon))
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// demSources is the priority order used by the build (matches elevation.KnownSources).
func demSources() []*demSource {
	def := func(id string) elevation.SourceDef {
		d, ok := elevation.SourceByID(id)
		if !ok {
			panic("unknown source " + id)
		}
		return d
	}
	return []*demSource{
		{
			def: def("hrdem"), nativeRes: 2, geographic: false,
			cellURLs: func(ctx context.Context, f *fetcher, lon, lat int) ([]cellRef, error) {
				return f.hrdemItems(ctx, lon, lat)
			},
		},
		{
			def: def("3dep13"), nativeRes: 1.0 / 10800, geographic: true,
			cellURLs: func(ctx context.Context, f *fetcher, lon, lat int) ([]cellRef, error) {
				if lon >= 0 { // 3DEP is western hemisphere only
					return nil, nil
				}
				n := cellName3DEP(lon, lat)
				u := fmt.Sprintf(url3DEP, n, n)
				ok, err := f.exists(ctx, u)
				if err != nil || !ok {
					return nil, err
				}
				return []cellRef{{key: n, url: u}}, nil
			},
		},
		{
			def: def("mrdem"), nativeRes: 30, geographic: false,
			cellURLs: func(ctx context.Context, f *fetcher, lon, lat int) ([]cellRef, error) {
				return []cellRef{{key: cellName3DEP(lon, lat), url: urlMRDEM}}, nil
			},
		},
		{
			def: def("glo30"), nativeRes: 1.0 / 3600, geographic: true,
			cellURLs: func(ctx context.Context, f *fetcher, lon, lat int) ([]cellRef, error) {
				n := cellNameGLO30(lon, lat)
				u := fmt.Sprintf(urlGLO30, n)
				ok, err := f.exists(ctx, u)
				if err != nil || !ok {
					return nil, err
				}
				return []cellRef{{key: n, url: u}}, nil
			},
		},
	}
}

// inCoverage reports whether a cell box touches the source's static extent.
func (s *demSource) inCoverage(b elevation.BBox) bool {
	for _, c := range s.def.Coverage {
		if c.Intersects(b) {
			return true
		}
	}
	return false
}

// fetcher downloads source windows into the cache, once per piece per build,
// and reuses cached windows whose upstream ETag has not changed.
type fetcher struct {
	cacheDir string
	gdal     *gdalRunner
	http     *http.Client
	report   Reporter
	// cacheRes returns the cache resolution (metres on the ground) for a latitude.
	cacheResM func(lat float64) float64

	mu       sync.Mutex
	heads    map[string]headResult
	inflight map[string]*fetchCall
	stac     map[[2]int][]stacItem
	stacErr  error
	stacOnce sync.Once
	stacAll  []stacItem
	fetched  int
	reused   int
}

type headResult struct {
	ok           bool
	etag, lastMd string
	err          error
}

type fetchCall struct {
	done chan struct{}
	path string
	err  error
}

func newFetcher(cacheDir string, g *gdalRunner, r Reporter, cacheResM func(float64) float64) *fetcher {
	return &fetcher{
		cacheDir: cacheDir, gdal: g, report: r, cacheResM: cacheResM,
		http:  &http.Client{Timeout: 60 * time.Second},
		heads: map[string]headResult{}, inflight: map[string]*fetchCall{}, stac: map[[2]int][]stacItem{},
	}
}

// head issues (and memoises) a HEAD request.
func (f *fetcher) head(ctx context.Context, u string) headResult {
	f.mu.Lock()
	if h, ok := f.heads[u]; ok {
		f.mu.Unlock()
		return h
	}
	f.mu.Unlock()
	var h headResult
	for attempt := 0; attempt < 4; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
		resp, err := f.http.Do(req)
		if err != nil {
			h.err = err
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK:
			h = headResult{ok: true, etag: strings.Trim(resp.Header.Get("ETag"), `"`), lastMd: resp.Header.Get("Last-Modified")}
		case http.StatusNotFound, http.StatusForbidden:
			h = headResult{} // S3 answers 403 for missing keys in non-listable buckets
		default:
			h.err = fmt.Errorf("HEAD %s: %s", u, resp.Status)
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		break
	}
	f.mu.Lock()
	f.heads[u] = h
	f.mu.Unlock()
	return h
}

func (f *fetcher) exists(ctx context.Context, u string) (bool, error) {
	h := f.head(ctx, u)
	return h.ok, h.err
}

// --- HRDEM via STAC -------------------------------------------------------------

type stacItem struct {
	id   string
	bbox elevation.BBox
	dtm  string
}

// hrdemItems returns HRDEM mosaic items whose bbox intersects the cell.
func (f *fetcher) hrdemItems(ctx context.Context, lon, lat int) ([]cellRef, error) {
	f.stacOnce.Do(func() { f.stacAll, f.stacErr = fetchSTAC(ctx, f.http, "hrdem-mosaic-2m") })
	if f.stacErr != nil {
		return nil, f.stacErr
	}
	cell := elevation.BBox{float64(lon), float64(lat), float64(lon + 1), float64(lat + 1)}
	var out []cellRef
	for _, it := range f.stacAll {
		if it.bbox.Intersects(cell) {
			out = append(out, cellRef{key: it.id + "-" + cellName3DEP(lon, lat), url: it.dtm})
		}
	}
	return out, nil
}

// fetchSTAC lists every item of a collection (the HRDEM mosaics are a few dozen).
func fetchSTAC(ctx context.Context, c *http.Client, collection string) ([]stacItem, error) {
	next := stacHRDEM + "?collections=" + url.QueryEscape(collection) + "&limit=100"
	var items []stacItem
	for page := 0; next != "" && page < 50; page++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		resp, err := c.Do(req)
		if err != nil {
			return nil, fmt.Errorf("STAC %s: %w", collection, err)
		}
		var body struct {
			Features []struct {
				ID     string    `json:"id"`
				BBox   []float64 `json:"bbox"`
				Assets map[string]struct {
					Href string `json:"href"`
				} `json:"assets"`
			} `json:"features"`
			Links []struct {
				Rel  string `json:"rel"`
				Href string `json:"href"`
			} `json:"links"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("STAC %s: %w", collection, err)
		}
		for _, ft := range body.Features {
			a, ok := ft.Assets["dtm"]
			if !ok || len(ft.BBox) < 4 {
				continue
			}
			items = append(items, stacItem{id: ft.ID, bbox: elevation.BBox{ft.BBox[0], ft.BBox[1], ft.BBox[2], ft.BBox[3]}, dtm: a.Href})
		}
		next = ""
		for _, l := range body.Links {
			if l.Rel == "next" {
				next = l.Href
			}
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("STAC %s: no items", collection)
	}
	return items, nil
}

// --- fetching windows into the cache -----------------------------------------------

type cacheMeta struct {
	URL          string         `json:"url"`
	ETag         string         `json:"etag"`
	LastModified string         `json:"last_modified"`
	Window       elevation.BBox `json:"window"`
	ResM         float64        `json:"res_m"`
	FetchedAt    string         `json:"fetched_at"`
}

// fetch returns the local path of piece p, downloading it if the cache has no
// current copy. Concurrent calls for the same piece share one download.
func (f *fetcher) fetch(ctx context.Context, src *demSource, p piece) (string, error) {
	h := f.head(ctx, p.url)
	if h.err != nil && !h.ok {
		// Offline: fall back to any cached copy of this piece.
		if m, path := f.findCached(src, p, ""); path != "" {
			f.report.Logf("terrain: %s %s: upstream unreachable (%v); using cached copy from %s", src.def.ID, p.key, h.err, m.FetchedAt)
			return path, nil
		}
		return "", h.err
	}
	midLat := (p.window[1] + p.window[3]) / 2
	resM := math.Max(f.cacheResM(midLat), nativeResM(src, midLat))
	resM = math.Round(resM*100) / 100
	id := cacheID(p.url, h.etag, p.window, resM)
	dir := filepath.Join(f.cacheDir, src.def.ID)
	path := filepath.Join(dir, p.key+"_"+id+".tif")

	f.mu.Lock()
	if c, ok := f.inflight[path]; ok {
		f.mu.Unlock()
		<-c.done
		return c.path, c.err
	}
	c := &fetchCall{done: make(chan struct{})}
	f.inflight[path] = c
	f.mu.Unlock()
	defer close(c.done)

	c.path, c.err = f.download(ctx, src, p, h, resM, dir, path)
	return c.path, c.err
}

func (f *fetcher) download(ctx context.Context, src *demSource, p piece, h headResult, resM float64, dir, path string) (string, error) {
	if _, err := os.Stat(path); err == nil {
		f.mu.Lock()
		f.reused++
		f.mu.Unlock()
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	part := strings.TrimSuffix(path, ".tif") + ".part.tif"
	_ = os.Remove(part)
	var tr []string
	if src.geographic {
		midLat := (p.window[1] + p.window[3]) / 2
		ry := resM / 111320
		rx := resM / (111320 * math.Cos(midLat*math.Pi/180))
		tr = []string{fmt.Sprintf("%.10f", rx), fmt.Sprintf("%.10f", ry)}
	} else {
		tr = []string{fmt.Sprintf("%.3f", resM), fmt.Sprintf("%.3f", resM)}
	}
	resampling := "bilinear"
	if resM > 1.2*nativeResM(src, (p.window[1]+p.window[3])/2) {
		resampling = "average"
	}
	args := []string{"-q", "-overwrite", "--config", "GDAL_DISABLE_READDIR_ON_OPEN", "EMPTY_DIR", "-of", "GTiff", "-ot", "Float32",
		"-te", f6(p.window[0]), f6(p.window[1]), f6(p.window[2]), f6(p.window[3]), "-te_srs", "EPSG:4326",
		"-tr", tr[0], tr[1], "-r", resampling, "-dstnodata", "-32768",
		"-co", "COMPRESS=LERC_DEFLATE", "-co", "MAX_Z_ERROR=0.05", "-co", "TILED=YES", "-co", "BIGTIFF=IF_SAFER",
		"-wo", "NUM_THREADS=2",
		"/vsicurl/" + p.url, part}
	start := time.Now()
	if err := f.gdal.run(ctx, "gdalwarp", args...); err != nil {
		_ = os.Remove(part)
		return "", fmt.Errorf("fetch %s %s: %w", src.def.ID, p.key, err)
	}
	if err := os.Rename(part, path); err != nil {
		return "", err
	}
	m := cacheMeta{URL: p.url, ETag: h.etag, LastModified: h.lastMd, Window: p.window, ResM: resM, FetchedAt: time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.MarshalIndent(m, "", "  ")
	_ = os.WriteFile(strings.TrimSuffix(path, ".tif")+".json", b, 0o644)
	// Older copies of this piece (other window, resolution or upstream ETag) are now stale.
	old, _ := filepath.Glob(filepath.Join(dir, p.key+"_*.tif"))
	for _, o := range old {
		if o != path {
			_ = os.Remove(o)
			_ = os.Remove(strings.TrimSuffix(o, ".tif") + ".json")
		}
	}
	fi, _ := os.Stat(path)
	var size int64
	if fi != nil {
		size = fi.Size()
	}
	f.mu.Lock()
	f.fetched++
	f.mu.Unlock()
	f.report.Logf("terrain: fetched %s %s at %.1f m (%.1f MB, %s)", src.def.ID, p.key, resM, float64(size)/1e6, time.Since(start).Round(time.Second))
	return path, nil
}

// findCached returns any cached copy of the piece (used when offline).
func (f *fetcher) findCached(src *demSource, p piece, _ string) (cacheMeta, string) {
	matches, _ := filepath.Glob(filepath.Join(f.cacheDir, src.def.ID, p.key+"_*.tif"))
	for _, m := range matches {
		if strings.HasSuffix(m, ".part.tif") {
			continue
		}
		var meta cacheMeta
		if b, err := os.ReadFile(strings.TrimSuffix(m, ".tif") + ".json"); err == nil {
			_ = json.Unmarshal(b, &meta)
		}
		return meta, m
	}
	return cacheMeta{}, ""
}

func nativeResM(src *demSource, lat float64) float64 {
	if src.geographic {
		return src.nativeRes * 111320
	}
	return src.nativeRes
}

func cacheID(u, etag string, w elevation.BBox, resM float64) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s|%s|%.6f,%.6f,%.6f,%.6f|%.2f", u, etag, w[0], w[1], w[2], w[3], resM)
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func f6(v float64) string { return fmt.Sprintf("%.6f", v) }
