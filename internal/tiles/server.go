// Package tiles serves the live generation's PMTiles archives (z/x/y tiles, TileJSON, the
// whole archive with HTTP Range), the GL styles with their glyphs and sprites, raster tiles
// proxied from the renderer, satellite imagery proxied from an open upstream, and offline
// pack extracts. Everything is cached on disk with LRU bounds.
package tiles

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jclement/mapthing/internal/auth"
	"github.com/jclement/mapthing/internal/stats"
	"github.com/jclement/mapthing/internal/tiles/pmtiles"
)

// Tilesets served from each generation: name → file.
var tilesets = map[string]string{
	"basemap": "basemap.pmtiles",
	"terrain": "terrain.pmtiles",
}

// Options configure a Server.
type Options struct {
	DataDir     string
	Assets      fs.FS  // contains styles/, fonts/, sprites/
	PublicURL   string // optional; else derived from each request
	RendererURL string // e.g. http://127.0.0.1:8081
	Stats       *stats.Stats

	RasterCacheBytes  int64
	ImageryCacheBytes int64
	PackCacheBytes    int64
	PackMaxTiles      uint64

	// ImageryUpstream returns the upstream URL for an imagery tile (default: ImageryUpstream).
	ImageryUpstream func(z, x, y int) (string, bool)
	HTTPClient      *http.Client
}

// Server is the tiles HTTP layer.
type Server struct {
	opt     Options
	client  *http.Client
	raster  *DiskCache
	imagery *DiskCache
	packs   *DiskCache

	mu   sync.RWMutex
	gen  string
	dir  string
	sets map[string]*pmtiles.Reader
}

// New creates a Server and opens the live generation, if any.
func New(opt Options) (*Server, error) {
	if opt.ImageryUpstream == nil {
		opt.ImageryUpstream = ImageryUpstream
	}
	if opt.PackMaxTiles == 0 {
		opt.PackMaxTiles = 2_000_000
	}
	s := &Server{opt: opt, client: opt.HTTPClient, sets: map[string]*pmtiles.Reader{}}
	if s.client == nil {
		s.client = &http.Client{Timeout: 60 * time.Second}
	}
	var err error
	if s.raster, err = NewDiskCache(filepath.Join(opt.DataDir, "raster-cache"), opt.RasterCacheBytes); err != nil {
		return nil, err
	}
	if s.imagery, err = NewDiskCache(filepath.Join(opt.DataDir, "imagery-cache"), opt.ImageryCacheBytes); err != nil {
		return nil, err
	}
	if s.packs, err = NewDiskCache(filepath.Join(opt.DataDir, "pack-cache"), opt.PackCacheBytes); err != nil {
		return nil, err
	}
	s.Reload()
	return s, nil
}

// Generation returns the live generation ID ("" if none).
func (s *Server) Generation() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gen
}

// Ready reports whether a basemap is being served.
func (s *Server) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sets["basemap"] != nil
}

// Reload reopens the archives of the current generation and drops cached raster tiles and
// packs of other generations. Old readers are closed after a grace period.
func (s *Server) Reload() {
	gen, dir, ok := CurrentGeneration(s.opt.DataDir)
	sets := map[string]*pmtiles.Reader{}
	if ok {
		for name, file := range tilesets {
			r, err := pmtiles.Open(filepath.Join(dir, file))
			if err != nil {
				slog.Warn("tileset unavailable", "tileset", name, "generation", gen, "err", err)
				continue
			}
			sets[name] = r
		}
	}
	s.mu.Lock()
	old := s.sets
	s.gen, s.dir, s.sets = gen, dir, sets
	s.mu.Unlock()
	if len(old) > 0 {
		time.AfterFunc(2*time.Minute, func() {
			for _, r := range old {
				_ = r.Close()
			}
		})
	}
	notCurrent := func(k string) bool { return gen == "" || !strings.HasPrefix(k, gen+"/") }
	s.raster.RemoveIf(notCurrent)
	s.packs.RemoveIf(notCurrent)
	slog.Info("tiles loaded", "generation", gen, "tilesets", len(sets))
}

func (s *Server) tileset(name string) (*pmtiles.Reader, string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sets[name], s.gen, s.dir
}

// CacheSizes reports bytes used by each cache.
func (s *Server) CacheSizes() map[string]int64 {
	return map[string]int64{"raster": s.raster.Size(), "imagery": s.imagery.Size(), "pack": s.packs.Size()}
}

// Mount registers the tile routes. wrap applies auth and counts usage by kind.
func (s *Server) Mount(mux *http.ServeMux, wrap func(kind string, h http.Handler) http.Handler) {
	tile := func(h http.HandlerFunc) http.Handler { return wrap(stats.KindTile, h) }
	raster := wrap(stats.KindRaster, http.HandlerFunc(s.handleRaster))
	mux.Handle("GET /tiles/{file}", tile(s.handleTilesetFile))
	mux.Handle("GET /tiles/{name}/{z}/{x}/{y}", tile(s.handleTile))
	mux.Handle("GET /styles", tile(s.handleStyleList))
	mux.Handle("GET /styles/{file}", tile(s.handleStyle))
	mux.Handle("GET /styles/v1/{user}/{name}", tile(s.handleStyle))
	mux.Handle("GET /styles/{name}/{size}/{z}/{x}/{y}", raster)
	mux.Handle("GET /styles/v1/{user}/{name}/tiles/{size}/{z}/{x}/{y}", raster)
	mux.Handle("GET /styles/v1/{user}/{name}/tiles/{z}/{x}/{y}", raster)
	mux.Handle("GET /fonts/{fontstack}/{range}", tile(s.handleGlyphs))
	mux.Handle("GET /sprites/{file}", tile(s.handleSprite))
	mux.Handle("GET /packs/{file}", wrap(stats.KindPack, http.HandlerFunc(s.handlePack)))
	for _, p := range []string{"/tiles/", "/styles/", "/fonts/", "/sprites/", "/packs/"} {
		mux.Handle("OPTIONS "+p, wrap(stats.KindTile, http.NotFoundHandler()))
	}
}

// baseURL is the public base URL for links inside styles and TileJSON.
func (s *Server) baseURL(r *http.Request) string {
	if s.opt.PublicURL != "" {
		return strings.TrimRight(s.opt.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.TrimSpace(strings.Split(p, ",")[0])
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = strings.TrimSpace(strings.Split(h, ",")[0])
	}
	return scheme + "://" + host
}

// keyQS is "?key=…" for key-authenticated callers (so browsers fetching tile URLs from a style
// carry it), "" for the admin session.
func keyQS(r *http.Request) string {
	if p, ok := auth.FromContext(r.Context()); ok && p.Secret != "" {
		return "?key=" + p.Secret
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	auth.WriteError(w, r, status, code, msg)
}

func noGeneration(w http.ResponseWriter, r *http.Request) {
	jsonError(w, r, http.StatusServiceUnavailable, "no_generation", "no map data has been built yet")
}

// detach returns a context that survives the client going away (for shared cache fills).
func detach(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), d)
}
