// Package ondemandtest is a stand-in for the public DEM hosts: a local HTTP
// server with small synthetic COGs, for tests of on-demand terrain anywhere
// in mapthing.
package ondemandtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jclement/mapthing/internal/cog/cogtest"
	"github.com/jclement/mapthing/internal/ondemand"
)

// Hosts is the fake.
type Hosts struct {
	Server *httptest.Server

	mu    sync.Mutex
	files map[string]file // by URL path

	// Down makes every request answer 503.
	Down atomic.Bool
	// Delay is added to every data (Range) request.
	Delay atomic.Int64
	// Counts of requests by kind.
	Heads, Ranges, Index atomic.Int64
	// IndexBody, if set, is what the HRDEM index answers (a STAC search page).
	IndexBody atomic.Value // string
	// RedirectTo, if set, makes /redirect answer a redirect to that URL.
	RedirectTo atomic.Value // string

	seen sync.Map // path → *atomic.Int64
}

// Seen is how many requests asked for a path.
func (h *Hosts) Seen(path string) int64 {
	if v, ok := h.seen.Load(path); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

type file struct {
	body []byte
	etag string
}

// New starts the fake with no files.
func New() *Hosts {
	h := &Hosts{files: map[string]file{}}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	return h
}

// Close stops it.
func (h *Hosts) Close() { h.Server.Close() }

// URLs points a Service at the fake.
func (h *Hosts) URLs() ondemand.URLs {
	b := h.Server.URL
	return ondemand.URLs{ThreeDEP: b + "/3dep/%s/USGS_13_%s.tif", GLO30: b + "/glo30/%[1]s/%[1]s.tif", MRDEM: b + "/mrdem.tif", HRDEMIndex: b + "/stac"}
}

func (h *Hosts) serve(w http.ResponseWriter, r *http.Request) {
	if h.Down.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	c, _ := h.seen.LoadOrStore(r.URL.Path, new(atomic.Int64))
	c.(*atomic.Int64).Add(1)
	if r.URL.Path == "/redirect" {
		if to, _ := h.RedirectTo.Load().(string); to != "" {
			http.Redirect(w, r, to, http.StatusFound)
			return
		}
	}
	if r.URL.Path == "/stac" {
		h.Index.Add(1)
		if body, _ := h.IndexBody.Load().(string); body != "" {
			_, _ = w.Write([]byte(body))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"features": []any{}, "links": []any{}})
		return
	}
	h.mu.Lock()
	f, ok := h.files[r.URL.Path]
	h.mu.Unlock()
	if !ok {
		http.Error(w, "no such key", http.StatusForbidden) // as S3 does
		return
	}
	w.Header().Set("ETag", `"`+f.etag+`"`)
	if r.Method == http.MethodHead {
		h.Heads.Add(1)
		w.Header().Set("Content-Length", fmt.Sprint(len(f.body)))
		return
	}
	h.Ranges.Add(1)
	if d := h.Delay.Load(); d > 0 {
		time.Sleep(time.Duration(d))
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(f.body))
}

// Put publishes body at path under a version tag.
func (h *Hosts) Put(path string, body []byte, etag string) {
	h.mu.Lock()
	h.files[path] = file{body, etag}
	h.mu.Unlock()
}

// Remove unpublishes a path.
func (h *Hosts) Remove(path string) {
	h.mu.Lock()
	delete(h.files, path)
	h.mu.Unlock()
}

// Plane is the synthetic ground: a tilted plane, so bilinear sampling of it
// is exact and a test can predict any elevation.
func Plane(base float64) func(lon, lat float64) float64 {
	return func(lon, lat float64) float64 { return base + 40*lon + 25*lat }
}

// Put3DEP publishes a 1° 3DEP-style cell (LZW, floating-point predictor,
// overviews) whose south-west corner is lon, lat, with elevation f.
func (h *Hosts) Put3DEP(lon, lat int, f func(lon, lat float64) float64, etag string) {
	name := fmt.Sprintf("n%02dw%03d", lat+1, -lon)
	h.Put(fmt.Sprintf("/3dep/%s/USGS_13_%s.tif", name, name), geoCell(lon, lat, 1200, 6, f, 5, 3, 4269), etag)
}

// PutGLO30 publishes a 1° GLO-30-style cell (Deflate, floating-point predictor).
func (h *Hosts) PutGLO30(lon, lat int, f func(lon, lat float64) float64, etag string) {
	ns, ew := "N", "E"
	if lat < 0 {
		ns = "S"
	}
	if lon < 0 {
		ew = "W"
	}
	name := fmt.Sprintf("Copernicus_DSM_COG_10_%s%02d_00_%s%03d_00_DEM", ns, abs(lat), ew, abs(lon))
	h.Put("/glo30/"+name+"/"+name+".tif", geoCell(lon, lat, 600, 0, f, 8, 3, 4326), etag)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// geoCell writes a geographic 1° cell of n pixels per degree with pad extra
// pixels on every side (3DEP overlaps its neighbours by 6).
func geoCell(lon, lat, n, pad int, f func(lon, lat float64) float64, comp, pred, epsg int) []byte {
	size := n + 2*pad
	px := 1 / float64(n)
	ox, oy := float64(lon)-float64(pad)*px, float64(lat+1)+float64(pad)*px
	if pad == 0 { // GLO-30 pixel centres sit on whole degrees
		ox, oy = float64(lon)-px/2, float64(lat+1)+px/2
	}
	data := make([]float32, size*size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			data[y*size+x] = float32(f(ox+(float64(x)+0.5)*px, oy-(float64(y)+0.5)*px))
		}
	}
	return cogtest.Write(cogtest.Options{Width: size, Height: size, Tile: 256, Data: data, OriginX: ox, OriginY: oy, PixelW: px, PixelH: px,
		Compression: comp, Predictor: pred, NoData: -999999, EPSG: epsg, Geographic: true, Overviews: 3})
}

// PathOf3DEP is the URL path of a cell, for Remove.
func PathOf3DEP(lon, lat int) string {
	name := fmt.Sprintf("n%02dw%03d", lat+1, -lon)
	return fmt.Sprintf("/3dep/%s/USGS_13_%s.tif", name, name)
}

// Near reports whether a and b agree within tol (and are both real).
func Near(a, b, tol float64) bool {
	return !math.IsNaN(a) && !math.IsNaN(b) && math.Abs(a-b) <= tol
}

// IsTileKey reports whether a store key holds a finished tile.
func IsTileKey(k string) bool { return strings.HasPrefix(k, "tile/") }
