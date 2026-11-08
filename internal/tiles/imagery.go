package tiles

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// userAgent identifies mapthing to imagery upstreams, as their usage policies ask.
const userAgent = "mapthing (+https://github.com/jclement/mapthing)"

// serveImagery proxies /tiles/imagery/{z}/{x}/{y}.jpg to the configured upstream, caching on disk.
func (s *Server) serveImagery(w http.ResponseWriter, r *http.Request, z, x, y int) {
	up, ok := s.opt.ImageryUpstream(z, x, y)
	if !ok {
		w.Header().Set("Cache-Control", tileCache)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	key := fmt.Sprintf("%d/%d/%d.img", z, x, y)
	p, hit, err := s.imagery.Do(key, func() error {
		ctx, cancel := detach(r.Context(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, up, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := s.client.Do(req)
		if err != nil {
			return fmt.Errorf("%w: %v", errUpstream, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/") {
			return fmt.Errorf("%w: imagery upstream answered %s (%s)", errUpstream, resp.Status, resp.Header.Get("Content-Type"))
		}
		_, err = s.imagery.Put(key, func(w io.Writer) error {
			_, err := io.Copy(w, io.LimitReader(resp.Body, 8<<20))
			return err
		})
		return err
	})
	if s.opt.Stats != nil {
		s.opt.Stats.Cache("imagery", hit)
	}
	if err != nil {
		jsonError(w, r, http.StatusBadGateway, "imagery_unavailable", "imagery upstream unavailable")
		return
	}
	serveCached(w, r, p, "", `"img-`+strings.ReplaceAll(strings.TrimSuffix(key, ".img"), "/", "-")+`"`)
}

func (s *Server) imageryTileJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"tilejson":    "3.0.0",
		"name":        "imagery",
		"scheme":      "xyz",
		"tiles":       []string{s.baseURL(r) + "/tiles/imagery/{z}/{x}/{y}.jpg" + keyQS(r)},
		"minzoom":     0,
		"maxzoom":     19,
		"tileSize":    256,
		"attribution": ImageryAttribution(),
	})
}
