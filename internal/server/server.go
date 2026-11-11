// Package server assembles mapthing's HTTP surface: open health endpoints, the admin API
// (session cookie), keyed map endpoints (tiles, styles, packs, and whatever feature packages
// mount), and the embedded admin UI.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jclement/mapthing/internal/auth"
	"github.com/jclement/mapthing/internal/config"
	"github.com/jclement/mapthing/internal/jobs"
	"github.com/jclement/mapthing/internal/keys"
	"github.com/jclement/mapthing/internal/logs"
	"github.com/jclement/mapthing/internal/scheduler"
	"github.com/jclement/mapthing/internal/stats"
	"github.com/jclement/mapthing/internal/store"
	"github.com/jclement/mapthing/internal/supervisor"
	"github.com/jclement/mapthing/internal/tiles"
)

// Version is set at build time (-ldflags "-X github.com/jclement/mapthing/internal/server.Version=…").
var Version = "dev"

// Deps are the parts the server ties together. Optional fields may be nil.
type Deps struct {
	DataDir    string
	Store      *store.Store
	Auth       *auth.Auth
	Keys       *keys.Service
	Stats      *stats.Stats
	Jobs       *jobs.Runner
	Scheduler  *scheduler.Scheduler
	Supervisor *supervisor.Supervisor
	Tiles      *tiles.Server
	Logs       *logs.Sink // the server's log (nil → the logs endpoints answer 503)
	Assets     fs.FS      // styles/fonts/sprites (for the style list)
	UI         fs.FS      // built admin UI (index.html at the root)

	// Mounts register feature endpoints (routing, elevation, compatibility APIs).
	Mounts []Mountable

	// Catalog returns the installable region catalog (pipeline). nil → 503 on /admin/api/catalog.
	Catalog func(ctx context.Context) (any, error)
	// ValidateRegion checks a catalog region id and returns what the catalog knows about it
	// (pipeline). nil → any well-formed id is accepted.
	ValidateRegion func(ctx context.Context, id string) (CatalogRegion, error)
	// EstimateCustom plans the Geofabrik extracts a custom area is clipped from (pipeline):
	// the cheapest set that together covers it. nil → custom regions carry no parent list.
	EstimateCustom func(ctx context.Context, bbox []float64, ring [][2]float64) (CustomEstimate, error)
	// ElevationSources lists DEM sources of the live generation (elevation). Optional.
	ElevationSources func() any
	// ActivateGeneration validates the generation directory <data>/generations/<id> (one a
	// build left, or one copied in from another machine) and makes it live. It returns an
	// *ActivateError for a refusal. nil → 503 on the activate endpoint.
	ActivateGeneration func(ctx context.Context, id string) (Activation, error)
	// GenerationInventory says what state each generation directory is in (pipeline) and how
	// much disk they use together. nil → generations are only "live" or "kept".
	GenerationInventory func() (gens []GenerationState, diskBytes int64)
	// DeleteGeneration removes a generation directory for good. It returns an *ActivateError
	// for a refusal (the live one, the one being built). nil → 503 on the delete endpoint.
	DeleteGeneration func(ctx context.Context, id string) error
	// BuildEstimate says how much free disk the next build needs (the build's own disk guard,
	// for the installed regions). ok is false when it cannot be worked out now (no regions, or
	// the catalog is not loaded). Optional.
	BuildEstimate func(ctx context.Context) (needBytes int64, ok bool)
	// Versions lists the software in the image (Valhalla, Planetiler, the renderer, GDAL…),
	// after mapthing itself and Go, which the server adds. Optional.
	Versions func() []Component
	// Settings returns the effective configuration for the Settings page (config.Describe).
	// nil → 503 on /admin/api/settings.
	Settings func() []config.Setting
	// Info is extra, static configuration shown by /admin/api/system (e.g. which parts this
	// server serves and whether the renderer is off). Optional.
	Info map[string]any
	// ReadyChecks are extra readiness probes (e.g. routing.Router.Ready). name → probe.
	ReadyChecks map[string]func(ctx context.Context) bool
}

// Server is the HTTP server.
type Server struct {
	d       Deps
	mux     *http.ServeMux
	diskCtl *diskStats
	procs   *procSampler
	// logStreams counts open live log tails (bounded: internal/server/logs.go).
	logStreams atomic.Int32
}

// New builds the router.
func New(d Deps) *Server {
	s := &Server{d: d, mux: http.NewServeMux(), diskCtl: newDiskStats(d.DataDir), procs: newProcSampler()}
	s.routes()
	return s
}

// Handler returns the root handler (with panic recovery, request logging and security headers).
func (s *Server) Handler() http.Handler {
	return recoverer(s.logRequests(securityHeaders(s.mux)))
}

func (s *Server) routes() {
	m := s.mux
	a := s.d.Auth
	admin := func(h http.HandlerFunc) http.Handler { return a.RequireAdmin(h) }

	m.HandleFunc("GET /healthz", s.healthz)
	m.HandleFunc("GET /readyz", s.readyz)

	m.HandleFunc("POST /admin/api/login", a.HandleLogin)
	m.HandleFunc("POST /admin/api/logout", a.HandleLogout)
	m.HandleFunc("GET /admin/api/session", a.HandleSession)

	m.Handle("GET /admin/api/keys", admin(s.listKeys))
	m.Handle("POST /admin/api/keys", admin(s.createKey))
	m.Handle("GET /admin/api/keys/{id}", admin(s.getKey))
	m.Handle("PUT /admin/api/keys/{id}", admin(s.updateKey))
	m.Handle("PATCH /admin/api/keys/{id}", admin(s.updateKey))
	m.Handle("DELETE /admin/api/keys/{id}", admin(s.revokeKey))

	m.Handle("GET /admin/api/overview", admin(s.overview))
	m.Handle("GET /admin/api/stats", admin(s.stats))
	m.Handle("GET /admin/api/system", admin(s.system))
	m.Handle("GET /admin/api/settings", admin(s.settings))
	m.Handle("POST /admin/api/system/restart/{name}", admin(s.restartChild))
	m.Handle("GET /admin/api/history", admin(s.history))

	m.Handle("GET /admin/api/jobs", admin(s.listJobs))
	m.Handle("GET /admin/api/jobs/{id}", admin(s.getJob))
	m.Handle("POST /admin/api/jobs/{id}/cancel", admin(s.cancelJob))
	m.Handle("GET /admin/api/events", a.RequireAdmin(s.d.Jobs.Hub().Handler(s.eventSnapshot)))
	m.Handle("POST /admin/api/build", admin(s.startBuild))
	m.Handle("GET /admin/api/generations", admin(s.generations))
	m.Handle("POST /admin/api/generations/{id}/activate", admin(s.activateGeneration))
	m.Handle("DELETE /admin/api/generations/{id}", admin(s.deleteGeneration))

	m.Handle("GET /admin/api/logs", admin(s.logsQuery))
	m.Handle("GET /admin/api/logs/download", admin(s.logsDownload))
	m.Handle("GET /admin/api/logs/stream", admin(s.logsStream))

	m.Handle("GET /admin/api/schedule", admin(s.getSchedule))
	m.Handle("PUT /admin/api/schedule", admin(s.putSchedule))

	m.Handle("GET /admin/api/catalog", admin(s.catalog))
	m.Handle("POST /admin/api/catalog/estimate", admin(s.estimateCustom))
	m.Handle("GET /admin/api/regions", admin(s.listRegions))
	m.Handle("POST /admin/api/regions", admin(s.addRegion))
	m.Handle("POST /admin/api/regions/check", admin(s.checkRegions))
	m.Handle("GET /admin/api/regions/{id...}", admin(s.getRegion))
	m.Handle("PATCH /admin/api/regions/{id...}", admin(s.patchRegion))
	m.Handle("DELETE /admin/api/regions/{id...}", admin(s.deleteRegion))
	notFound := admin(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "not_found", "no such admin endpoint")
	})
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		m.Handle(method+" /admin/api/", notFound)
	}

	if s.d.Tiles != nil {
		s.d.Tiles.Mount(m, a.Wrap)
	}
	for _, mt := range s.d.Mounts {
		mt.Mount(m, a.Wrap)
	}

	// Admin UI (Vite base "/admin/"). /admin/api/… is matched by the more specific patterns above.
	m.Handle("GET /admin/", s.ui())
	m.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
	m.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "not_found", "not found")
	})
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": Version})
}

// readyz: a generation is live and the supervised children are up. Children that are
// "unavailable" (binary absent, i.e. running outside the container) are reported but do not
// block readiness.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	checks := map[string]bool{}
	ready := true
	if s.d.Tiles != nil {
		checks["generation"] = s.d.Tiles.Ready()
		ready = ready && checks["generation"]
	}
	var children []supervisor.Status
	if s.d.Supervisor != nil {
		children = s.d.Supervisor.Statuses()
		for _, c := range children {
			ok := c.State == supervisor.StateRunning && c.Healthy
			if c.State == supervisor.StateUnavailable || c.Standby {
				continue
			}
			checks["child:"+c.Name] = ok
			ready = ready && ok
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	for name, f := range s.d.ReadyChecks {
		ok := f(ctx)
		checks[name] = ok
		ready = ready && ok
	}
	for i := range children {
		children[i].LogTail = nil
	}
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ready": ready, "checks": checks, "children": children})
}

// ui serves the embedded admin UI under /admin/: files as they are (hashed assets under
// /admin/assets/ cached forever), and index.html (no-cache) for client-side routes.
func (s *Server) ui() http.Handler {
	ui := s.d.UI
	var files http.Handler
	if ui != nil {
		files = http.StripPrefix("/admin", http.FileServerFS(ui))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ui == nil {
			placeholder(w)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/admin/")
		if p != "" {
			if fi, err := fs.Stat(ui, p); err == nil && !fi.IsDir() {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
			// Unknown file-like paths are real 404s; extension-less paths are client routes.
			if strings.Contains(p[strings.LastIndex(p, "/")+1:], ".") {
				http.NotFound(w, r)
				return
			}
		}
		idx, err := fs.ReadFile(ui, "index.html")
		if err != nil {
			placeholder(w)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(idx)
	})
}

func placeholder(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>mapthing</title>
<body style="font-family:system-ui;max-width:40rem;margin:4rem auto;padding:0 1rem">
<h1>mapthing</h1><p>The admin UI has not been built. Run <code>mise run build:web</code> and rebuild.</p>
<p>The API is up: <a href="/healthz">/healthz</a>, <a href="/readyz">/readyz</a>.</p></body>`)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	auth.WriteError(w, r, status, code, msg)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	// Unknown fields are ignored: clients may send back read-only fields they were given.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func storeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "not_found", "not found")
		return
	}
	slog.Error("store", "path", r.URL.Path, "err", err)
	writeError(w, r, http.StatusInternalServerError, "internal", "internal error")
}

type statusWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (w *statusWriter) WriteHeader(c int) { w.status = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logRequests times every request: keyed endpoints go into the traffic record (rate and
// latency by kind, for the overview), and 5xx answers into the log (every request with
// MAPTHING_DEBUG). The path is logged without its query string, which can carry a key.
func (s *Server) logRequests(h http.Handler) http.Handler {
	var traffic *stats.Traffic
	if s.d.Stats != nil {
		traffic = s.d.Stats.Traffic
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		ctx, info := auth.WithRequestInfo(r.Context())
		h.ServeHTTP(sw, r.WithContext(ctx))
		dur := time.Since(start)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		if info.Kind != "" && r.Method != http.MethodOptions {
			traffic.Observe(stats.TrafficKind(info.Kind, r.URL.Path), dur, sw.status)
		}
		if sw.status >= 500 || slog.Default().Enabled(r.Context(), slog.LevelDebug) {
			slog.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "bytes", sw.n,
				"dur", dur.Round(time.Millisecond), "ip", s.d.Auth.ClientIP(r))
		}
	})
}

func recoverer(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(p)
				}
				slog.Error("panic", "path", r.URL.Path, "panic", p, "stack", string(debug.Stack()))
				writeError(w, r, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		h.ServeHTTP(w, r)
	})
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		if !strings.HasPrefix(r.URL.Path, "/tiles/") && !strings.HasPrefix(r.URL.Path, "/styles/") {
			hd.Set("Referrer-Policy", "same-origin")
		}
		if strings.HasPrefix(r.URL.Path, "/admin/") && !strings.HasPrefix(r.URL.Path, "/admin/api/") {
			hd.Set("X-Frame-Options", "DENY")
		}
		h.ServeHTTP(w, r)
	})
}
