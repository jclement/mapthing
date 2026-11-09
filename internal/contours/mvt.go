package contours

import (
	"errors"
	"fmt"
	"math"
)

// A minimal Mapbox Vector Tile 2.1 encoder for one layer of line features
// (https://github.com/mapbox/vector-tile-spec), and the matching decoder used by tests and
// the CLI's stats.

// Extent is the tile coordinate range.
const Extent = 4096

// Feature is one contour level in a tile: every line at that elevation.
type Feature struct {
	Ele   int
	Major bool
	Lines [][][2]int32 // tile coordinates, 0..Extent (lines may run a little outside)
}

const (
	wireVarint = 0
	wireBytes  = 2

	cmdMoveTo = 1
	cmdLineTo = 2
	geomLine  = 2
)

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendTag(b []byte, field, wire int) []byte { return appendVarint(b, uint64(field<<3|wire)) }

func appendBytesField(b []byte, field int, data []byte) []byte {
	b = appendTag(b, field, wireBytes)
	b = appendVarint(b, uint64(len(data)))
	return append(b, data...)
}

func zigzag(v int64) uint64 { return uint64((v << 1) ^ (v >> 63)) }

// EncodeLayer encodes one layer as a complete tile. Features without a drawable line are
// left out; nil is returned when nothing is left.
func EncodeLayer(name string, feats []Feature) []byte {
	keys := []string{"ele", "major"}
	type val struct {
		isBool bool
		i      int64
		b      bool
	}
	var vals []val
	vidx := map[val]int{}
	valIndex := func(v val) int {
		if i, ok := vidx[v]; ok {
			return i
		}
		vidx[v] = len(vals)
		vals = append(vals, v)
		return len(vals) - 1
	}

	var layer []byte
	layer = appendTag(layer, 15, wireVarint) // version
	layer = appendVarint(layer, 2)
	layer = appendBytesField(layer, 1, []byte(name))
	nfeat := 0
	for _, f := range feats {
		geom := encodeLines(f.Lines)
		if len(geom) == 0 {
			continue
		}
		var fb, tags, packed []byte
		tags = appendVarint(tags, 0)
		tags = appendVarint(tags, uint64(valIndex(val{i: int64(f.Ele)})))
		tags = appendVarint(tags, 1)
		tags = appendVarint(tags, uint64(valIndex(val{isBool: true, b: f.Major})))
		fb = appendBytesField(fb, 2, tags)
		fb = appendTag(fb, 3, wireVarint)
		fb = appendVarint(fb, geomLine)
		for _, g := range geom {
			packed = appendVarint(packed, uint64(g))
		}
		fb = appendBytesField(fb, 4, packed)
		layer = appendBytesField(layer, 2, fb)
		nfeat++
	}
	if nfeat == 0 {
		return nil
	}
	for _, k := range keys {
		layer = appendBytesField(layer, 3, []byte(k))
	}
	for _, v := range vals {
		var vb []byte
		if v.isBool {
			vb = appendTag(vb, 7, wireVarint)
			if v.b {
				vb = appendVarint(vb, 1)
			} else {
				vb = appendVarint(vb, 0)
			}
		} else {
			vb = appendTag(vb, 6, wireVarint) // sint64
			vb = appendVarint(vb, zigzag(v.i))
		}
		layer = appendBytesField(layer, 4, vb)
	}
	layer = appendTag(layer, 5, wireVarint)
	layer = appendVarint(layer, Extent)
	return appendBytesField(nil, 3, layer)
}

// encodeLines returns the geometry commands for a multi-linestring. Repeated points are
// dropped; lines with fewer than two distinct points are skipped.
func encodeLines(lines [][][2]int32) []uint32 {
	var g []uint32
	var cx, cy int32
	for _, ln := range lines {
		pts := make([][2]int32, 0, len(ln))
		for _, p := range ln {
			if len(pts) > 0 && pts[len(pts)-1] == p {
				continue
			}
			pts = append(pts, p)
		}
		if len(pts) < 2 {
			continue
		}
		g = append(g, cmdMoveTo|1<<3,
			uint32(zigzag(int64(pts[0][0]-cx))), uint32(zigzag(int64(pts[0][1]-cy))))
		cx, cy = pts[0][0], pts[0][1]
		g = append(g, uint32(cmdLineTo|(len(pts)-1)<<3))
		for _, p := range pts[1:] {
			g = append(g, uint32(zigzag(int64(p[0]-cx))), uint32(zigzag(int64(p[1]-cy))))
			cx, cy = p[0], p[1]
		}
	}
	return g
}

// DecodedLayer is a decoded vector-tile layer (lines only).
type DecodedLayer struct {
	Name     string
	Extent   int
	Version  int
	Features []DecodedFeature
}

// DecodedFeature is one decoded feature.
type DecodedFeature struct {
	Type  int
	Props map[string]any
	Lines [][][2]int32
}

// Decode parses a vector tile (uncompressed). It understands what EncodeLayer writes and
// the common value types.
func Decode(b []byte) ([]DecodedLayer, error) {
	var out []DecodedLayer
	err := fields(b, func(f int, v uint64, data []byte) error {
		if f != 3 {
			return nil
		}
		l, err := decodeLayer(data)
		if err != nil {
			return err
		}
		out = append(out, l)
		return nil
	})
	return out, err
}

func decodeLayer(b []byte) (DecodedLayer, error) {
	l := DecodedLayer{Extent: 4096, Version: 1}
	var keys []string
	var vals []any
	var raw [][]byte
	err := fields(b, func(f int, v uint64, data []byte) error {
		switch f {
		case 15:
			l.Version = int(v)
		case 1:
			l.Name = string(data)
		case 2:
			raw = append(raw, data)
		case 3:
			keys = append(keys, string(data))
		case 4:
			var val any
			if err := fields(data, func(f int, v uint64, d []byte) error {
				switch f {
				case 1:
					val = string(d)
				case 4:
					val = int64(v)
				case 5:
					val = v
				case 6:
					val = int64(v>>1) ^ -int64(v&1)
				case 7:
					val = v != 0
				case 2:
					val = float64(math.Float32frombits(uint32(v)))
				case 3:
					val = math.Float64frombits(v)
				}
				return nil
			}); err != nil {
				return err
			}
			vals = append(vals, val)
		case 5:
			l.Extent = int(v)
		}
		return nil
	})
	if err != nil {
		return l, err
	}
	for _, fb := range raw {
		f := DecodedFeature{Props: map[string]any{}}
		var tags, geom []uint64
		if err := fields(fb, func(fn int, v uint64, d []byte) error {
			var err error
			switch fn {
			case 2:
				tags, err = packedVarints(d)
			case 3:
				f.Type = int(v)
			case 4:
				geom, err = packedVarints(d)
			}
			return err
		}); err != nil {
			return l, err
		}
		for k := 0; k+1 < len(tags); k += 2 {
			if int(tags[k]) >= len(keys) || int(tags[k+1]) >= len(vals) {
				return l, errors.New("mvt: tag index out of range")
			}
			f.Props[keys[tags[k]]] = vals[tags[k+1]]
		}
		lines, err := decodeGeom(geom)
		if err != nil {
			return l, err
		}
		f.Lines = lines
		l.Features = append(l.Features, f)
	}
	return l, nil
}

func decodeGeom(g []uint64) ([][][2]int32, error) {
	var lines [][][2]int32
	var cx, cy int32
	unz := func(v uint64) int32 { return int32(int64(v>>1) ^ -int64(v&1)) }
	for k := 0; k < len(g); {
		cmd, n := int(g[k]&7), int(g[k]>>3)
		k++
		if k+2*n > len(g) {
			return nil, errors.New("mvt: truncated geometry")
		}
		switch cmd {
		case cmdMoveTo:
			for ; n > 0; n-- {
				cx += unz(g[k])
				cy += unz(g[k+1])
				k += 2
				lines = append(lines, [][2]int32{{cx, cy}})
			}
		case cmdLineTo:
			if len(lines) == 0 {
				return nil, errors.New("mvt: LineTo before MoveTo")
			}
			for ; n > 0; n-- {
				cx += unz(g[k])
				cy += unz(g[k+1])
				k += 2
				lines[len(lines)-1] = append(lines[len(lines)-1], [2]int32{cx, cy})
			}
		default:
			return nil, fmt.Errorf("mvt: unexpected command %d", cmd)
		}
	}
	return lines, nil
}

// fields walks protobuf fields: varints give v, length-delimited give data.
func fields(b []byte, fn func(field int, v uint64, data []byte) error) error {
	for len(b) > 0 {
		key, n := readVarint(b)
		if n == 0 {
			return errors.New("mvt: bad varint")
		}
		b = b[n:]
		field, wire := int(key>>3), int(key&7)
		switch wire {
		case wireVarint:
			v, n := readVarint(b)
			if n == 0 {
				return errors.New("mvt: bad varint")
			}
			b = b[n:]
			if err := fn(field, v, nil); err != nil {
				return err
			}
		case wireBytes:
			l, n := readVarint(b)
			if n == 0 || uint64(len(b)-n) < l {
				return errors.New("mvt: truncated field")
			}
			data := b[n : n+int(l)]
			b = b[n+int(l):]
			if err := fn(field, 0, data); err != nil {
				return err
			}
		case 1: // 64-bit
			if len(b) < 8 {
				return errors.New("mvt: truncated fixed64")
			}
			var v uint64
			for i := 7; i >= 0; i-- {
				v = v<<8 | uint64(b[i])
			}
			b = b[8:]
			if err := fn(field, v, nil); err != nil {
				return err
			}
		case 5: // 32-bit
			if len(b) < 4 {
				return errors.New("mvt: truncated fixed32")
			}
			v := uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24
			b = b[4:]
			if err := fn(field, v, nil); err != nil {
				return err
			}
		default:
			return fmt.Errorf("mvt: unsupported wire type %d", wire)
		}
	}
	return nil
}

func readVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

func packedVarints(b []byte) ([]uint64, error) {
	var out []uint64
	for len(b) > 0 {
		v, n := readVarint(b)
		if n == 0 {
			return nil, errors.New("mvt: bad packed varint")
		}
		out = append(out, v)
		b = b[n:]
	}
	return out, nil
}
