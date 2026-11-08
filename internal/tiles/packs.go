package tiles

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // MBTiles writer

	"github.com/jclement/mapthing/internal/tiles/pmtiles"
)

// handlePack serves /packs/extract.pmtiles?bbox=w,s,e,n&minzoom=&maxzoom=[&format=mbtiles]
// [&tileset=basemap|terrain]: a subset of a tileset, built on first request and cached per
// generation. Range requests are supported, so interrupted downloads can resume.
func (s *Server) handlePack(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	q := r.URL.Query()
	format := q.Get("format")
	switch {
	case format == "" && file == "extract.mbtiles":
		format = "mbtiles"
	case format == "":
		format = "pmtiles"
	}
	if (file != "extract.pmtiles" && file != "extract.mbtiles") || (format != "pmtiles" && format != "mbtiles") {
		jsonError(w, r, http.StatusNotFound, "not_found", "use /packs/extract.pmtiles or format=mbtiles")
		return
	}
	tileset := q.Get("tileset")
	if tileset == "" {
		tileset = "basemap"
	}
	if _, ok := tilesets[tileset]; !ok {
		jsonError(w, r, http.StatusBadRequest, "bad_request", "unknown tileset")
		return
	}
	bbox, err := parseBBox(q.Get("bbox"))
	if err != nil {
		jsonError(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	rd, gen, _ := s.tileset(tileset)
	if rd == nil {
		noGeneration(w, r)
		return
	}
	minZ, err1 := zoomParam(q.Get("minzoom"), rd.Header.MinZoom)
	maxZ, err2 := zoomParam(q.Get("maxzoom"), rd.Header.MaxZoom)
	if err1 != nil || err2 != nil || minZ > maxZ {
		jsonError(w, r, http.StatusBadRequest, "bad_request", "minzoom/maxzoom must be 0-24 with minzoom <= maxzoom")
		return
	}
	minZ, maxZ = max(minZ, rd.Header.MinZoom), min(maxZ, rd.Header.MaxZoom)
	if minZ > maxZ {
		jsonError(w, r, http.StatusBadRequest, "bad_request", "zoom range outside the tileset")
		return
	}
	if n := pmtiles.CountTiles(bbox, minZ, maxZ); n > s.opt.PackMaxTiles {
		jsonError(w, r, http.StatusRequestEntityTooLarge, "too_many_tiles",
			fmt.Sprintf("this pack covers %d tiles (limit %d); use a smaller box or a lower maxzoom", n, s.opt.PackMaxTiles))
		return
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s|%.6f,%.6f,%.6f,%.6f|%d|%d|%s", tileset, bbox[0], bbox[1], bbox[2], bbox[3], minZ, maxZ, format))
	id := hex.EncodeToString(sum[:8])
	key := fmt.Sprintf("%s/%s-%s.%s", gen, tileset, id, format)
	p, hit, err := s.packs.Do(key, func() error {
		ctx, cancel := detach(r.Context(), 30*time.Minute)
		defer cancel()
		if format == "mbtiles" {
			return s.buildMBTiles(ctx, key, rd, bbox, minZ, maxZ)
		}
		_, err := s.packs.Put(key, func(w io.Writer) error {
			_, err := pmtiles.Extract(ctx, rd, bbox, minZ, maxZ, s.opt.PackMaxTiles, s.packs.Dir(), w)
			return err
		})
		return err
	})
	if s.opt.Stats != nil {
		s.opt.Stats.Cache("pack", hit)
	}
	if err != nil {
		if errors.Is(err, pmtiles.ErrTooManyTiles) {
			jsonError(w, r, http.StatusRequestEntityTooLarge, "too_many_tiles", err.Error())
			return
		}
		jsonError(w, r, http.StatusInternalServerError, "internal", "could not build pack")
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
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="mapthing-%s-%s.%s"`, tileset, id, format))
	h.Set("Cache-Control", archiveCache)
	h.Set("ETag", fmt.Sprintf(`"%s-%s-%s"`, gen, id, format))
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

func parseBBox(s string) (pmtiles.BBox, error) {
	var b pmtiles.BBox
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return b, errors.New("bbox must be w,s,e,n in degrees")
	}
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return b, errors.New("bbox must be w,s,e,n in degrees")
		}
		b[i] = v
	}
	if !b.Valid() {
		return b, errors.New("bbox must be w,s,e,n with w<e and s<n")
	}
	return b, nil
}

func zoomParam(s string, def uint8) (uint8, error) {
	if s == "" {
		return def, nil
	}
	v, err := strconv.ParseUint(s, 10, 8)
	if err != nil || v > 24 {
		return 0, errors.New("bad zoom")
	}
	return uint8(v), nil
}

// buildMBTiles writes an MBTiles 1.3 file for the extract into the pack cache under key.
func (s *Server) buildMBTiles(ctx context.Context, key string, rd *pmtiles.Reader, b pmtiles.BBox, minZ, maxZ uint8) error {
	tmp, err := os.CreateTemp(s.packs.Dir(), ".tmp-mbtiles-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := writeMBTiles(ctx, tmpPath, rd, b, minZ, maxZ); err != nil {
		return err
	}
	_, err = s.packs.Put(key, func(w io.Writer) error {
		f, err := os.Open(tmpPath)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(w, f)
		return err
	})
	return err
}

func writeMBTiles(ctx context.Context, path string, rd *pmtiles.Reader, b pmtiles.BBox, minZ, maxZ uint8) error {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `CREATE TABLE metadata (name TEXT, value TEXT);
		CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB);
		CREATE UNIQUE INDEX tile_index ON tiles (zoom_level, tile_column, tile_row);`); err != nil {
		return err
	}
	h := rd.Header
	format := map[uint8]string{pmtiles.TypeMVT: "pbf", pmtiles.TypePNG: "png", pmtiles.TypeJPEG: "jpg", pmtiles.TypeWebP: "webp"}[h.TileType]
	meta := map[string]string{
		"name":    "mapthing extract",
		"format":  format,
		"bounds":  fmt.Sprintf("%f,%f,%f,%f", b[0], b[1], b[2], b[3]),
		"center":  fmt.Sprintf("%f,%f,%d", (b[0]+b[2])/2, (b[1]+b[3])/2, minZ),
		"minzoom": strconv.Itoa(int(minZ)),
		"maxzoom": strconv.Itoa(int(maxZ)),
		"type":    "baselayer",
	}
	if m, err := rd.Metadata(); err == nil {
		var mm map[string]any
		if json.Unmarshal(m, &mm) == nil {
			if vl, ok := mm["vector_layers"]; ok {
				j, _ := json.Marshal(map[string]any{"vector_layers": vl})
				meta["json"] = string(j)
			}
			if a, ok := mm["attribution"].(string); ok {
				meta["attribution"] = a
			}
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for k, v := range meta {
		if _, err := tx.ExecContext(ctx, `INSERT INTO metadata (name, value) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO tiles (zoom_level, tile_column, tile_row, tile_data) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for z := minZ; z <= maxZ; z++ {
		x0, y0, x1, y1 := pmtiles.TileRange(b, z)
		for x := x0; x <= x1; x++ {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			for y := y0; y <= y1; y++ {
				data, ok, err := rd.Get(z, x, y)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				// MBTiles stores gzip-compressed MVT, as PMTiles usually does; raw bytes pass through.
				tmsY := (uint32(1) << z) - 1 - y
				if _, err := stmt.ExecContext(ctx, z, x, tmsY, data); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}
