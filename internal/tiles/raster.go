package tiles

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DegradedHeader marks a raster tile the renderer drew without one of its sources (a failed
// terrain, imagery or vector request). Such tiles are served with no-store and never cached.
const DegradedHeader = "X-Mapthing-Degraded"

// errDegraded stops the cache fill for a degraded tile.
var errDegraded = errors.New("degraded render")

// errUpstream marks a failure of the renderer or imagery upstream (answered 502/503).
var errUpstream = errors.New("upstream unavailable")

// handleRaster serves raster tiles rendered from a style:
//
//	/styles/{name}/{size}/{z}/{x}/{y}[@2x].{png|jpg|webp}   (no extension = png)
//	/styles/v1/{user}/{name}/tiles/{size}/{z}/{x}/{y}[@2x]   (Mapbox Static Tiles shape)
//	/styles/v1/{user}/{name}/tiles/{z}/{x}/{y}[@2x]          (Mapbox default 512)
//
// The Mapbox-shaped URLs take an optional extension; without one they answer jpg for the
// satellite style and png otherwise, as Mapbox does.
//
// Tiles come from the renderer (GET {renderer}/render/{name}/{size}/{z}/{x}/{y}[@2x].{ext}) and
// are cached on disk per generation.
func (s *Server) handleRaster(w http.ResponseWriter, r *http.Request) {
	if s.opt.RendererOff {
		jsonError(w, r, http.StatusNotFound, "renderer_off", "raster tiles are turned off on this server (MAPTHING_RENDERER=off); vector tiles and styles are not affected")
		return
	}
	name := r.PathValue("name")
	size := r.PathValue("size")
	if size == "" {
		size = "512"
	}
	if size != "256" && size != "512" {
		jsonError(w, r, http.StatusBadRequest, "bad_request", "tile size must be 256 or 512")
		return
	}
	if !s.HasStyle(name) {
		jsonError(w, r, http.StatusNotFound, "not_found", "unknown style")
		return
	}
	z, x, y, retina, ext, ok := parseZXY(r.PathValue("z"), r.PathValue("x"), r.PathValue("y"))
	if !ok || z > 22 {
		jsonError(w, r, http.StatusBadRequest, "bad_tile", "invalid tile address")
		return
	}
	switch ext {
	case "png", "jpg", "webp":
	case "jpeg":
		ext = "jpg"
	case "":
		ext = "png"
		if r.PathValue("user") != "" && name == "satellite" { // Mapbox-shaped URL
			ext = "jpg"
		}
	default:
		jsonError(w, r, http.StatusBadRequest, "bad_format", "format must be png, jpg or webp")
		return
	}
	gen := s.Generation()
	if gen == "" {
		noGeneration(w, r)
		return
	}
	scale := ""
	if retina {
		scale = "@2x"
	}
	tail := fmt.Sprintf("%s/%s/%d/%d/%d%s.%s", name, size, z, x, y, scale, ext)
	key := gen + "/" + tail
	render := func() (body []byte, degraded bool, err error) {
		ctx, cancel := detach(r.Context(), 90*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.opt.RendererURL, "/")+"/render/"+tail, nil)
		if err != nil {
			return nil, false, err
		}
		resp, err := s.client.Do(req)
		if err != nil {
			return nil, false, fmt.Errorf("%w: renderer: %w", errUpstream, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, false, fmt.Errorf("%w: renderer answered %s", errUpstream, resp.Status)
		}
		body, err = io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		return body, resp.Header.Get(DegradedHeader) != "", err
	}
	var direct []byte
	p, hit, err := s.raster.Do(key, func() error {
		body, degraded, err := render()
		if err != nil {
			return err
		}
		if degraded { // drawn without a failed source: serve, never cache
			direct = body
			return errDegraded
		}
		_, err = s.raster.PutBytes(key, body)
		return err
	})
	if s.opt.Stats != nil {
		s.opt.Stats.Cache("raster", hit)
	}
	if errors.Is(err, errDegraded) {
		if direct == nil { // another request rendered it; render our own copy
			if direct, _, err = render(); err != nil {
				direct = nil
			}
		}
		if direct != nil {
			h := w.Header()
			h.Set("Content-Type", rasterTypes[ext])
			h.Set("Cache-Control", "no-store")
			h.Set(DegradedHeader, "1")
			_, _ = w.Write(direct)
			return
		}
	}
	if err != nil {
		if errors.Is(err, errUpstream) {
			jsonError(w, r, http.StatusServiceUnavailable, "renderer_unavailable", "the raster renderer is not available")
			return
		}
		jsonError(w, r, http.StatusInternalServerError, "internal", "could not render tile")
		return
	}
	serveCached(w, r, p, rasterTypes[ext], `"`+key+`"`)
}

var rasterTypes = map[string]string{"png": "image/png", "jpg": "image/jpeg", "webp": "image/webp"}

// serveCached sends a cached file with tile cache headers.
func serveCached(w http.ResponseWriter, r *http.Request, path, contentType, etag string) {
	f, err := os.Open(path)
	if err != nil {
		jsonError(w, r, http.StatusInternalServerError, "internal", "cache read failed")
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		jsonError(w, r, http.StatusInternalServerError, "internal", "cache read failed")
		return
	}
	if contentType == "" {
		head := make([]byte, 512)
		n, _ := f.ReadAt(head, 0)
		contentType = http.DetectContentType(head[:n])
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", tileCache)
	h.Set("ETag", etag)
	http.ServeContent(w, r, "", fi.ModTime(), f)
}
