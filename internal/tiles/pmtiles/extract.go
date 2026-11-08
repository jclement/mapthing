package pmtiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
)

// BBox is [west, south, east, north] in degrees.
type BBox [4]float64

// Valid reports whether the box is well-formed.
func (b BBox) Valid() bool {
	return b[0] >= -180 && b[2] <= 180 && b[1] >= -90 && b[3] <= 90 && b[0] < b[2] && b[1] < b[3]
}

const maxLat = 85.0511287798066

// TileRange returns the inclusive tile x/y range covering b at zoom z.
func TileRange(b BBox, z uint8) (x0, y0, x1, y1 uint32) {
	n := float64(uint64(1) << z)
	tx := func(lon float64) uint32 {
		v := math.Floor((lon + 180) / 360 * n)
		return uint32(math.Max(0, math.Min(n-1, v)))
	}
	ty := func(lat float64) uint32 {
		lat = math.Max(-maxLat, math.Min(maxLat, lat))
		r := lat * math.Pi / 180
		v := math.Floor((1 - math.Log(math.Tan(r)+1/math.Cos(r))/math.Pi) / 2 * n)
		return uint32(math.Max(0, math.Min(n-1, v)))
	}
	return tx(b[0]), ty(b[3]), tx(b[2]), ty(b[1])
}

// CountTiles returns how many tile addresses b covers over [minZ, maxZ].
func CountTiles(b BBox, minZ, maxZ uint8) uint64 {
	var n uint64
	for z := minZ; z <= maxZ; z++ {
		x0, y0, x1, y1 := TileRange(b, z)
		n += uint64(x1-x0+1) * uint64(y1-y0+1)
	}
	return n
}

// ErrTooManyTiles is returned when an extract exceeds its tile budget.
var ErrTooManyTiles = errors.New("extract covers too many tiles")

// Extract writes a new archive to out holding the tiles of src inside b over [minZ, maxZ].
// maxTiles bounds the number of tile addresses examined (0 = unlimited). spoolDir holds the
// temporary tile spool.
func Extract(ctx context.Context, src *Reader, b BBox, minZ, maxZ uint8, maxTiles uint64, spoolDir string, out io.Writer) (uint64, error) {
	if !b.Valid() {
		return 0, errors.New("invalid bbox")
	}
	minZ = max(minZ, src.Header.MinZoom)
	maxZ = min(maxZ, src.Header.MaxZoom)
	if minZ > maxZ {
		return 0, errors.New("zoom range outside archive")
	}
	if maxTiles > 0 && CountTiles(b, minZ, maxZ) > maxTiles {
		return 0, fmt.Errorf("%w (%d > %d); use a smaller box or lower maxzoom", ErrTooManyTiles, CountTiles(b, minZ, maxZ), maxTiles)
	}
	var ids []uint64
	for z := minZ; z <= maxZ; z++ {
		x0, y0, x1, y1 := TileRange(b, z)
		for x := x0; x <= x1; x++ {
			for y := y0; y <= y1; y++ {
				ids = append(ids, ZxyToID(z, x, y))
			}
		}
	}
	slices.Sort(ids)
	w, err := NewWriter(spoolDir)
	if err != nil {
		return 0, err
	}
	for i, id := range ids {
		if i%4096 == 0 && ctx.Err() != nil {
			w.Abort()
			return 0, ctx.Err()
		}
		data, ok, err := src.GetID(id)
		if err != nil {
			w.Abort()
			return 0, err
		}
		if !ok {
			continue
		}
		if err := w.Add(id, data); err != nil {
			w.Abort()
			return 0, err
		}
	}
	h := src.Header
	h.MinZoom, h.MaxZoom = minZ, maxZ
	sb := src.Header.Bounds()
	nb := BBox{math.Max(b[0], sb[0]), math.Max(b[1], sb[1]), math.Min(b[2], sb[2]), math.Min(b[3], sb[3])}
	if !nb.Valid() {
		nb = b
	}
	h.MinLonE7, h.MinLatE7 = int32(nb[0]*1e7), int32(nb[1]*1e7)
	h.MaxLonE7, h.MaxLatE7 = int32(nb[2]*1e7), int32(nb[3]*1e7)
	h.CenterLonE7, h.CenterLatE7 = int32((nb[0]+nb[2])/2*1e7), int32((nb[1]+nb[3])/2*1e7)
	h.CenterZoom = min(max(minZ, h.CenterZoom), maxZ)
	meta, err := src.Metadata()
	if err != nil {
		meta = []byte("{}")
	}
	meta = annotate(meta, nb, minZ, maxZ)
	count := w.Count()
	if err := w.Finish(out, h, meta); err != nil {
		return 0, err
	}
	return count, nil
}

// annotate records the extract's bounds and zooms in the metadata JSON.
func annotate(meta []byte, b BBox, minZ, maxZ uint8) []byte {
	m := map[string]any{}
	if err := json.Unmarshal(meta, &m); err != nil {
		m = map[string]any{}
	}
	m["mapthing:extract"] = map[string]any{"bbox": b, "minzoom": minZ, "maxzoom": maxZ}
	out, err := json.Marshal(m)
	if err != nil {
		return meta
	}
	return out
}
