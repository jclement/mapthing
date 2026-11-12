package tiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/jclement/mapthing/internal/elevation"
)

// OnDemandTerrain builds terrain tiles at request time for areas with no built terrain
// (internal/ondemand). It is optional, and off unless the operator turns it on.
type OnDemandTerrain interface {
	// Enabled reports whether on demand applies anywhere.
	Enabled() bool
	// Bounds is the box on demand applies to; false when it applies nowhere.
	Bounds() (elevation.BBox, bool)
	// Allows reports whether tile z/x/y may be built on demand.
	Allows(z, x, y int) bool
	// Has reports whether the tile is in the on-demand cache already.
	Has(z, x, y int) bool
	// Tile returns the tile as a Terrarium PNG (nil when no source has data there) and
	// whether the DEM hosts had to be asked.
	Tile(ctx context.Context, z, x, y int) (png []byte, cold bool, err error)
}

// HasTerrain reports whether the live terrain archive holds tile z/x/y (built terrain).
func (s *Server) HasTerrain(z, x, y int) bool {
	rd, _, _ := s.tileset("terrain")
	if rd == nil || z < 0 || z > int(rd.Header.MaxZoom) {
		return false
	}
	data, ok, err := rd.Get(uint8(z), uint32(x), uint32(y))
	return err == nil && ok && len(data) > 0
}

// serveOnDemandTerrain answers a terrain tile the live archive does not have, when on demand
// applies to it. It reports whether it answered.
func (s *Server) serveOnDemandTerrain(w http.ResponseWriter, r *http.Request, z, x, y int) bool {
	od := s.opt.OnDemand
	if od == nil || !od.Allows(z, x, y) {
		return false
	}
	ctx := r.Context()
	if s.opt.ColdGate != nil {
		ctx = s.opt.ColdGate(r)
	}
	png, cold, err := od.Tile(ctx, z, x, y)
	if s.opt.Stats != nil {
		s.opt.Stats.Cache("ondemand", !cold)
	}
	h := w.Header()
	var lim *elevation.LimitedError
	switch {
	case errors.As(err, &lim):
		h.Set("Cache-Control", "no-store")
		h.Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(lim.RetryAfter.Seconds())))))
		jsonError(w, r, http.StatusTooManyRequests, "rate_limited", lim.Message)
		return true
	case errors.Is(err, elevation.ErrSourceUnavailable):
		// The DEM host did not answer. Nothing was cached; the client may ask again.
		h.Set("Cache-Control", "no-store")
		h.Set("Retry-After", "30")
		jsonError(w, r, http.StatusGatewayTimeout, "terrain_source_unavailable", "this terrain is read on demand and its source did not answer; try again shortly")
		return true
	case err != nil:
		h.Set("Cache-Control", "no-store")
		jsonError(w, r, http.StatusInternalServerError, "internal", "could not build the terrain tile")
		return true
	}
	h.Set("Cache-Control", tileCache)
	if len(png) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	sum := sha256.Sum256(png)
	etag := `"od-` + hex.EncodeToString(sum[:8]) + `"`
	h.Set("ETag", etag)
	if match(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	h.Set("Content-Type", "image/png")
	h.Set("Content-Length", strconv.Itoa(len(png)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(png)
	}
	return true
}

// onDemandTileJSON describes terrain when there is no built archive at all and on demand
// covers the world outside built regions.
func (s *Server) onDemandTileJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"tilejson":    "3.0.0",
		"name":        "terrain",
		"scheme":      "xyz",
		"tiles":       []string{s.baseURL(r) + "/tiles/terrain/{z}/{x}/{y}.png" + keyQS(r)},
		"minzoom":     s.opt.OnDemandMinZoom,
		"maxzoom":     s.opt.OnDemandMaxZoom,
		"bounds":      []float64{-180, -85.0511, 180, 85.0511},
		"encoding":    "terrarium",
		"tileSize":    256,
		"attribution": elevation.AttributionAll(),
	})
}
