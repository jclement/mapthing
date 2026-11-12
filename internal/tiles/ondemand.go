package tiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/pmtiles"
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
	// Has reports whether the tile is in the on-demand cache already, made from the last
	// known release of its sources. It asks the DEM hosts nothing.
	Has(z, x, y int) bool
	// Revision identifies what on demand would serve for a box now: its policy and the
	// release of every source file that could contribute. A pack is current while it holds.
	Revision(ctx context.Context, box elevation.BBox) (string, error)
	// AllowsContour, HasContour and Contour are Allows, Has and Tile for contour tiles drawn
	// from on-demand terrain (gzipped vector tiles; nil when no line crosses the tile).
	AllowsContour(z, x, y int) bool
	HasContour(z, x, y int) bool
	Contour(ctx context.Context, z, x, y int) (mvt []byte, cold bool, err error)
	// Tile returns the tile as a Terrarium PNG (nil when no source has data there) and
	// whether the DEM hosts had to be asked.
	Tile(ctx context.Context, z, x, y int) (png []byte, cold bool, err error)
}

// TerrainTile returns a built terrain tile from the live archive, and false when it has none.
func (s *Server) TerrainTile(z, x, y int) ([]byte, bool) {
	rd, _, _ := s.tileset("terrain")
	if rd == nil || z < 0 || z > int(rd.Header.MaxZoom) || x < 0 || y < 0 {
		return nil, false
	}
	data, ok, err := rd.Get(uint8(z), uint32(x), uint32(y))
	if err != nil || !ok || len(data) == 0 {
		return nil, false
	}
	return data, true
}

// odKind is one of the two tilesets on demand can fill in: terrain, and contours drawn from it.
type odKind struct {
	name        string
	allows, has func(z, x, y int) bool
	tile        func(ctx context.Context, z, x, y int) ([]byte, bool, error)
	tileType    uint8
	compression uint8
	maxZoom     int
	contentType string
	errCode     string
}

func (s *Server) odKind(name string) (odKind, bool) {
	od := s.opt.OnDemand
	if od == nil {
		return odKind{}, false
	}
	switch name {
	case "terrain":
		return odKind{name: name, allows: od.Allows, has: od.Has, tile: od.Tile, tileType: pmtiles.TypePNG,
			compression: pmtiles.CompressionNone, maxZoom: s.opt.OnDemandMaxZoom, contentType: "image/png", errCode: "terrain_source_unavailable"}, true
	case "contours":
		return odKind{name: name, allows: od.AllowsContour, has: od.HasContour, tile: od.Contour, tileType: pmtiles.TypeMVT,
			compression: pmtiles.CompressionGzip, maxZoom: s.opt.OnDemandContourMaxZoom, contentType: "application/vnd.mapbox-vector-tile", errCode: "terrain_source_unavailable"}, true
	}
	return odKind{}, false
}

// serveOnDemand answers a terrain or contour tile the live archive does not have, when on
// demand applies to it. It reports whether it answered.
func (s *Server) serveOnDemand(w http.ResponseWriter, r *http.Request, name string, z, x, y int) bool {
	k, ok := s.odKind(name)
	if !ok || !k.allows(z, x, y) {
		return false
	}
	ctx := r.Context()
	if s.opt.ColdGate != nil {
		ctx = s.opt.ColdGate(r)
	}
	data, cold, err := k.tile(ctx, z, x, y)
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
		jsonError(w, r, http.StatusGatewayTimeout, k.errCode, "this "+map[string]string{"terrain": "terrain", "contours": "tile's terrain"}[name]+" is read on demand and its source did not answer; try again shortly")
		return true
	case err != nil:
		h.Set("Cache-Control", "no-store")
		jsonError(w, r, http.StatusInternalServerError, "internal", "could not build the tile")
		return true
	}
	h.Set("Cache-Control", tileCache)
	h.Set("Vary", "Accept-Encoding")
	if len(data) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	sum := sha256.Sum256(data)
	etag := `"od-` + hex.EncodeToString(sum[:8]) + `"`
	if k.compression == pmtiles.CompressionGzip {
		if acceptsGzip(r) {
			h.Set("Content-Encoding", "gzip")
		} else {
			if data, err = pmtiles.Decompress(data, pmtiles.CompressionGzip); err != nil {
				jsonError(w, r, http.StatusInternalServerError, "read_error", "could not decompress tile")
				return true
			}
			etag = strings.TrimSuffix(etag, `"`) + `-id"`
		}
	}
	h.Set("ETag", etag)
	if match(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	h.Set("Content-Type", k.contentType)
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
	return true
}

// onDemandContoursTileJSON describes contours when there is no built archive and on demand
// applies somewhere.
func (s *Server) onDemandContoursTileJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	b, _ := s.opt.OnDemand.Bounds()
	writeJSON(w, http.StatusOK, map[string]any{
		"tilejson":    "3.0.0",
		"name":        "contours",
		"scheme":      "xyz",
		"tiles":       []string{s.baseURL(r) + "/tiles/contours/{z}/{x}/{y}.mvt" + keyQS(r)},
		"minzoom":     s.opt.OnDemandContourMinZoom,
		"maxzoom":     s.opt.OnDemandContourMaxZoom,
		"bounds":      []float64{b[0], b[1], b[2], b[3]},
		"attribution": elevation.AttributionAll(),
		"vector_layers": []map[string]any{{"id": "contours", "fields": map[string]string{"ele": "Number", "major": "Boolean"},
			"minzoom": s.opt.OnDemandContourMinZoom, "maxzoom": s.opt.OnDemandContourMaxZoom}},
	})
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
