package tiles

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/pmtiles"
)

const (
	tileCache    = "public, max-age=3600, stale-while-revalidate=86400"
	archiveCache = "public, max-age=3600, no-transform"
)

// parseZXY parses z, x and a y segment that may carry an extension and @2x ("12", "12.mvt",
// "12@2x.png"). It returns y, whether @2x was given, and the extension ("" if none).
func parseZXY(zs, xs, ys string) (z uint8, x, y uint32, retina bool, ext string, ok bool) {
	if i := strings.IndexByte(ys, '.'); i >= 0 {
		ys, ext = ys[:i], ys[i+1:]
	}
	if strings.HasSuffix(ys, "@2x") {
		ys, retina = strings.TrimSuffix(ys, "@2x"), true
	}
	zi, err1 := strconv.ParseUint(zs, 10, 8)
	xi, err2 := strconv.ParseUint(xs, 10, 32)
	yi, err3 := strconv.ParseUint(ys, 10, 32)
	if err1 != nil || err2 != nil || err3 != nil || zi > 24 {
		return 0, 0, 0, false, "", false
	}
	n := uint64(1) << zi
	if xi >= n || yi >= n {
		return 0, 0, 0, false, "", false
	}
	return uint8(zi), uint32(xi), uint32(yi), retina, ext, true
}

// handleTile serves /tiles/{name}/{z}/{x}/{y}.{ext} from an archive, or imagery.
func (s *Server) handleTile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	z, x, y, _, _, ok := parseZXY(r.PathValue("z"), r.PathValue("x"), r.PathValue("y"))
	if !ok {
		jsonError(w, r, http.StatusBadRequest, "bad_tile", "invalid tile address")
		return
	}
	if name == "imagery" {
		s.serveImagery(w, r, int(z), int(x), int(y))
		return
	}
	if _, known := tilesets[name]; !known {
		jsonError(w, r, http.StatusNotFound, "not_found", "unknown tileset")
		return
	}
	rd, gen, _ := s.tileset(name)
	if rd == nil {
		// No built terrain at all: on demand answers where it applies.
		if name == "terrain" && s.serveOnDemandTerrain(w, r, int(z), int(x), int(y)) {
			return
		}
		missingTileset(w, r, gen)
		return
	}
	etag := fmt.Sprintf(`"%s-%d-%d-%d"`, gen, z, x, y)
	data, found, err := rd.Get(z, x, y)
	if err != nil {
		jsonError(w, r, http.StatusInternalServerError, "read_error", "could not read tile")
		return
	}
	// Built terrain always wins. Only a tile the archive does not have is read on demand.
	if (!found || len(data) == 0) && name == "terrain" && s.serveOnDemandTerrain(w, r, int(z), int(x), int(y)) {
		return
	}
	h := w.Header()
	h.Set("Cache-Control", tileCache)
	h.Set("Vary", "Accept-Encoding")
	if !found || len(data) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.Set("Content-Type", rd.Header.ContentType())
	if rd.Header.TileCompression == pmtiles.CompressionGzip {
		if acceptsGzip(r) {
			h.Set("Content-Encoding", "gzip")
		} else {
			if data, err = pmtiles.Decompress(data, pmtiles.CompressionGzip); err != nil {
				jsonError(w, r, http.StatusInternalServerError, "read_error", "could not decompress tile")
				return
			}
			etag = strings.TrimSuffix(etag, `"`) + `-id"`
		}
	}
	h.Set("ETag", etag)
	if match(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(enc), "gzip") && strings.TrimSpace(q) != "q=0" {
			return true
		}
	}
	return false
}

func match(inm, etag string) bool {
	if inm == "" {
		return false
	}
	for _, t := range strings.Split(inm, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == etag || t == "*" {
			return true
		}
	}
	return false
}

// handleTilesetFile serves /tiles/{name}.json (TileJSON) and /tiles/{name}.pmtiles (archive).
func (s *Server) handleTilesetFile(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	name, ext, _ := strings.Cut(file, ".")
	switch {
	case ext == "json" && name == "imagery":
		s.imageryTileJSON(w, r)
	case ext == "json":
		s.tileJSON(w, r, name)
	case ext == "pmtiles":
		s.archive(w, r, name)
	default:
		jsonError(w, r, http.StatusNotFound, "not_found", "unknown tileset file")
	}
}

func (s *Server) tileJSON(w http.ResponseWriter, r *http.Request, name string) {
	if _, ok := tilesets[name]; !ok {
		jsonError(w, r, http.StatusNotFound, "not_found", "unknown tileset")
		return
	}
	rd, gen, _ := s.tileset(name)
	if rd == nil {
		if name == "terrain" && s.opt.OnDemand != nil && s.opt.OnDemand.Enabled() {
			s.onDemandTileJSON(w, r)
			return
		}
		missingTileset(w, r, gen)
		return
	}
	h := rd.Header
	tj := map[string]any{
		"tilejson": "3.0.0",
		"name":     name,
		"scheme":   "xyz",
		"tiles":    []string{fmt.Sprintf("%s/tiles/%s/{z}/{x}/{y}.%s%s", s.baseURL(r), name, h.Ext(), keyQS(r))},
		"minzoom":  h.MinZoom,
		"maxzoom":  h.MaxZoom,
		"bounds":   h.Bounds(),
		"center":   h.Center(),
		"version":  gen,
	}
	if meta, err := rd.Metadata(); err == nil {
		var m map[string]any
		if json.Unmarshal(meta, &m) == nil {
			for _, k := range []string{"vector_layers", "attribution", "description", "type"} {
				if v, ok := m[k]; ok {
					tj[k] = v
				}
			}
		}
	}
	if name == "terrain" {
		tj["encoding"] = "terrarium"
		tj["tileSize"] = 256
	}
	// The terrain build records tile_size and maxzoom in its metadata; they win over defaults.
	if meta, err := rd.Metadata(); err == nil {
		var m map[string]any
		if json.Unmarshal(meta, &m) == nil {
			if v, ok := m["tile_size"].(float64); ok && v > 0 {
				tj["tileSize"] = int(v)
			}
			if v, ok := m["maxzoom"].(float64); ok && v >= 0 && v <= 24 {
				tj["maxzoom"] = int(v)
			}
		}
	}
	if od := s.opt.OnDemand; name == "terrain" && od != nil {
		// Terrain is also served, on demand, beyond what was built: say so, or a client that
		// honours bounds never asks.
		if ob, ok := od.Bounds(); ok {
			b := h.Bounds()
			tj["bounds"] = []float64{min(b[0], ob[0]), min(b[1], ob[1]), max(b[2], ob[2]), max(b[3], ob[3])}
			tj["attribution"] = elevation.AttributionAll()
		}
	}
	if _, ok := tj["attribution"]; !ok {
		tj["attribution"] = `<a href="https://www.openstreetmap.org/copyright">© OpenStreetMap contributors</a>`
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, tj)
}

// archive serves the whole PMTiles file with Range support. Each request opens the file
// itself, so a long download survives a generation swap.
func (s *Server) archive(w http.ResponseWriter, r *http.Request, name string) {
	file, ok := tilesets[name]
	if !ok {
		jsonError(w, r, http.StatusNotFound, "not_found", "unknown tileset")
		return
	}
	_, gen, dir := s.tileset(name)
	if gen == "" {
		noGeneration(w, r)
		return
	}
	f, err := os.Open(filepath.Join(dir, file))
	if err != nil {
		noGeneration(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		jsonError(w, r, http.StatusInternalServerError, "read_error", "could not stat archive")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/vnd.pmtiles")
	h.Set("Cache-Control", archiveCache)
	h.Set("ETag", fmt.Sprintf(`"%s-%s-%d"`, gen, name, fi.Size()))
	h.Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, "", fi.ModTime(), io.NewSectionReader(f, 0, fi.Size()))
}
