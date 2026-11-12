package tiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/ondemand"
	"github.com/jclement/mapthing/internal/pmtiles"
)

// MaxPackFetch is how many terrain tiles one pack request may read from the DEM hosts. A pack
// that needs more is refused with what to do instead (warm the area, which is a job with
// progress, then ask again): a download request is no place for a quarter of an hour's fetching.
var MaxPackFetch = 2000

// errNotWarm refuses a pack that would have to fetch too much.
type errNotWarm struct{ missing int }

func (e *errNotWarm) Error() string {
	n := "more than " + strconv.Itoa(MaxPackFetch)
	if e.missing > 0 {
		n = strconv.Itoa(e.missing)
	}
	return fmt.Sprintf("this pack needs %s tiles that are read on demand and not cached yet (at most %d are fetched for one pack); warm the area first (admin: Settings, On-demand terrain, Warm an area), then ask again", n, MaxPackFetch)
}

// packError answers a pack over on-demand terrain that could not be made.
func (s *Server) packError(w http.ResponseWriter, r *http.Request, err error) {
	var nw *errNotWarm
	var lim *elevation.LimitedError
	switch {
	case errors.As(err, &nw):
		jsonError(w, r, http.StatusConflict, "not_warm", nw.Error())
	case errors.Is(err, ondemand.ErrBudget):
		jsonError(w, r, http.StatusConflict, "not_warm", (&errNotWarm{}).Error())
	case errors.Is(err, ondemand.ErrTooWide):
		jsonError(w, r, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("this box is too large for a pack over on-demand terrain (at most %.0f square degrees); use a smaller box", ondemand.MaxRevisionArea))
	case errors.As(err, &lim):
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(lim.RetryAfter.Seconds())))))
		jsonError(w, r, http.StatusTooManyRequests, "rate_limited", lim.Message)
	case errors.Is(err, elevation.ErrSourceUnavailable):
		w.Header().Set("Retry-After", "30")
		jsonError(w, r, http.StatusGatewayTimeout, "terrain_source_unavailable", "part of this pack is read on demand and its source did not answer; try again shortly")
	default:
		jsonError(w, r, http.StatusInternalServerError, "internal", "could not build pack")
	}
}

// onDemandPack serves a terrain or contours pack when on-demand terrain is on: built tiles
// come from the archive (what was built wins), the rest from the on-demand cache, made first
// if need be.
func (s *Server) onDemandPack(w http.ResponseWriter, r *http.Request, k odKind, rd *pmtiles.Reader, gen string, bbox pmtiles.BBox, q url.Values, format string) {
	od := s.opt.OnDemand
	defMin, defMax := uint8(0), uint8(k.maxZoom)
	if k.name == "contours" {
		defMin = uint8(s.opt.OnDemandContourMinZoom)
	}
	if rd != nil {
		defMin, defMax = rd.Header.MinZoom, max(rd.Header.MaxZoom, defMax)
	}
	minZ, err1 := zoomParam(q.Get("minzoom"), defMin)
	maxZ, err2 := zoomParam(q.Get("maxzoom"), defMax)
	if err1 != nil || err2 != nil || minZ > maxZ {
		jsonError(w, r, http.StatusBadRequest, "bad_request", "minzoom/maxzoom must be 0-24 with minzoom <= maxzoom")
		return
	}
	minZ, maxZ = max(minZ, defMin), min(maxZ, defMax)
	if minZ > maxZ {
		jsonError(w, r, http.StatusBadRequest, "bad_request", "zoom range outside the tileset")
		return
	}
	if n := pmtiles.CountTiles(bbox, minZ, maxZ); n > s.opt.PackMaxTiles {
		jsonError(w, r, http.StatusRequestEntityTooLarge, "too_many_tiles",
			fmt.Sprintf("this pack covers %d tiles (limit %d); use a smaller box or a lower maxzoom", n, s.opt.PackMaxTiles))
		return
	}
	built := func(z uint8, x, y uint32) ([]byte, bool, error) {
		if rd == nil || z > rd.Header.MaxZoom || z < rd.Header.MinZoom {
			return nil, false, nil
		}
		data, ok, err := rd.Get(z, x, y)
		return data, ok && len(data) > 0, err
	}
	if gen == "" {
		gen = "none"
	}
	gateCtx := r.Context()
	if s.opt.ColdGate != nil {
		gateCtx = s.opt.ColdGate(r)
	}
	// The pack is keyed (and its ETag made) from what on demand would serve for the box
	// now: a new release of a source file, or a change to where on demand applies, is a
	// new pack, and the old one is left to age out of the pack cache.
	rev, err := od.Revision(gateCtx, elevation.BBox(bbox))
	if err != nil {
		s.packError(w, r, err)
		return
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s+ondemand|%.6f,%.6f,%.6f,%.6f|%d|%d|%s|%s", k.name, bbox[0], bbox[1], bbox[2], bbox[3], minZ, maxZ, format, rev))
	id := hex.EncodeToString(sum[:8])
	key := fmt.Sprintf("%s/%s-%s.%s", gen, k.name, id, format)
	p, hit, err := s.packs.Do(key, func() error {
		ctx, cancel := detach(gateCtx, 30*time.Minute)
		defer cancel()
		// The cap is enforced where tiles are actually built, in the planning pass and the
		// writing pass alike: a tile that went stale or was evicted meanwhile counts too.
		ctx = ondemand.WithBudget(ctx, MaxPackFetch)
		// What has to come from on demand, and how much of it is not cached yet.
		type zxy struct {
			z    uint8
			x, y uint32
		}
		var need []zxy
		missing := 0
		for z := minZ; z <= maxZ; z++ {
			x0, y0, x1, y1 := pmtiles.TileRange(bbox, z)
			for x := x0; x <= x1; x++ {
				for y := y0; y <= y1; y++ {
					if _, ok, err := built(z, x, y); err != nil {
						return err
					} else if ok || !k.allows(int(z), int(x), int(y)) {
						continue
					}
					need = append(need, zxy{z, x, y})
					if !k.has(int(z), int(x), int(y)) {
						missing++
					}
				}
			}
		}
		if missing > MaxPackFetch {
			return &errNotWarm{missing}
		}
		// Fetch what is missing side by side; the tiles are then read back from the cache.
		var (
			wg    sync.WaitGroup
			mu    sync.Mutex
			first error
		)
		fctx, stop := context.WithCancel(ctx)
		defer stop()
		jobs := make(chan zxy)
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for t := range jobs {
					if _, _, err := k.tile(fctx, int(t.z), int(t.x), int(t.y)); err != nil {
						mu.Lock()
						if first == nil {
							first = err
							stop()
						}
						mu.Unlock()
					}
				}
			}()
		}
	feed:
		for _, t := range need {
			if k.has(int(t.z), int(t.x), int(t.y)) {
				continue
			}
			select {
			case jobs <- t:
			case <-fctx.Done():
				break feed
			}
		}
		close(jobs)
		wg.Wait()
		if first != nil {
			return first
		}
		get := func(z uint8, x, y uint32) ([]byte, bool, error) {
			if data, ok, err := built(z, x, y); err != nil || ok {
				return data, ok, err
			}
			if !k.allows(int(z), int(x), int(y)) {
				return nil, false, nil
			}
			data, _, err := k.tile(ctx, int(z), int(x), int(y))
			return data, len(data) > 0, err
		}
		meta := []byte(`{}`)
		if rd != nil {
			if m, err := rd.Metadata(); err == nil {
				meta = m
			}
		} else if k.name == "contours" {
			meta, _ = json.Marshal(map[string]any{"name": "contours", "format": "pbf", "type": "overlay", "attribution": elevation.AttributionAll(),
				"vector_layers": []map[string]any{{"id": "contours", "fields": map[string]string{"ele": "Number", "major": "Boolean"}, "minzoom": minZ, "maxzoom": maxZ}}})
		} else {
			meta, _ = json.Marshal(elevation.Metadata{Name: "mapthing terrain", Format: "png", Encoding: "terrarium", TileSize: elevation.TileSize,
				MinZoom: int(minZ), MaxZoom: int(maxZ), Attribution: elevation.AttributionAll()})
		}
		if format == "mbtiles" {
			return s.buildMBTilesFrom(ctx, key, get, k.tileType, meta, bbox, minZ, maxZ)
		}
		_, err := s.packs.Put(key, func(out io.Writer) error {
			pw, err := pmtiles.NewWriter(s.packs.Dir())
			if err != nil {
				return err
			}
			for z := minZ; z <= maxZ; z++ {
				x0, y0, x1, y1 := pmtiles.TileRange(bbox, z)
				for x := x0; x <= x1; x++ {
					if ctx.Err() != nil {
						pw.Abort()
						return ctx.Err()
					}
					for y := y0; y <= y1; y++ {
						data, ok, err := get(z, x, y)
						if err == nil && ok {
							err = pw.AddTile(z, x, y, data)
						}
						if err != nil {
							pw.Abort()
							return err
						}
					}
				}
			}
			h := pmtiles.Header{TileType: k.tileType, TileCompression: k.compression, InternalCompression: pmtiles.CompressionGzip, MinZoom: minZ, MaxZoom: maxZ,
				MinLonE7: int32(bbox[0] * 1e7), MinLatE7: int32(bbox[1] * 1e7), MaxLonE7: int32(bbox[2] * 1e7), MaxLatE7: int32(bbox[3] * 1e7),
				CenterZoom: minZ, CenterLonE7: int32((bbox[0] + bbox[2]) / 2 * 1e7), CenterLatE7: int32((bbox[1] + bbox[3]) / 2 * 1e7)}
			return pw.Finish(out, h, meta)
		})
		return err
	})
	if s.opt.Stats != nil {
		s.opt.Stats.Cache("pack", hit)
	}
	if err != nil {
		s.packError(w, r, err)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		jsonError(w, r, http.StatusInternalServerError, "internal", "pack read failed")
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		jsonError(w, r, http.StatusInternalServerError, "internal", "pack read failed")
		return
	}
	ct := "application/vnd.pmtiles"
	if format == "mbtiles" {
		ct = "application/vnd.sqlite3"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="mapthing-%s-%s.%s"`, k.name, id, format))
	h.Set("Cache-Control", archiveCache)
	h.Set("ETag", fmt.Sprintf(`"%s-%s-%s"`, gen, id, format))
	http.ServeContent(w, r, "", fi.ModTime(), f)
}
