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

// errUpstream marks a failure of the renderer or imagery upstream (answered 502/503).
var errUpstream = errors.New("upstream unavailable")

// handleRaster serves raster tiles rendered from a style:
//
//	/styles/{name}/{size}/{z}/{x}/{y}[@2x].png
//	/styles/v1/{user}/{name}/tiles/{size}/{z}/{x}/{y}[@2x]   (Mapbox Static Tiles shape)
//	/styles/v1/{user}/{name}/tiles/{z}/{x}/{y}[@2x]          (Mapbox default 512)
//
// Tiles come from the renderer (GET {renderer}/render/{name}/{size}/{z}/{x}/{y}[@2x].png) and
// are cached on disk per generation.
func (s *Server) handleRaster(w http.ResponseWriter, r *http.Request) {
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
	if !ok || z > 22 || (ext != "" && ext != "png") {
		jsonError(w, r, http.StatusBadRequest, "bad_tile", "invalid tile address")
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
	tail := fmt.Sprintf("%s/%s/%d/%d/%d%s.png", name, size, z, x, y, scale)
	key := gen + "/" + tail
	p, hit, err := s.raster.Do(key, func() error {
		ctx, cancel := detach(r.Context(), 90*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.opt.RendererURL, "/")+"/render/"+tail, nil)
		if err != nil {
			return err
		}
		resp, err := s.client.Do(req)
		if err != nil {
			return fmt.Errorf("%w: renderer: %w", errUpstream, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%w: renderer answered %s", errUpstream, resp.Status)
		}
		_, err = s.raster.Put(key, func(w io.Writer) error {
			_, err := io.Copy(w, io.LimitReader(resp.Body, 32<<20))
			return err
		})
		return err
	})
	if s.opt.Stats != nil {
		s.opt.Stats.Cache("raster", hit)
	}
	if err != nil {
		if errors.Is(err, errUpstream) {
			jsonError(w, r, http.StatusServiceUnavailable, "renderer_unavailable", "the raster renderer is not available")
			return
		}
		jsonError(w, r, http.StatusInternalServerError, "internal", "could not render tile")
		return
	}
	serveCached(w, r, p, "image/png", `"`+key+`"`)
}

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
