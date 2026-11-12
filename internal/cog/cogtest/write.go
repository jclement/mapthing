// Package cogtest writes small cloud-optimised GeoTIFFs for tests: the same
// layouts the public DEM hosts publish, without needing GDAL or the network.
package cogtest

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"math"
	"strconv"
)

// Options describe the file to write.
type Options struct {
	Width, Height int
	Tile          int // block edge
	// Data holds Width×Height elevations, row-major; NaN marks nodata.
	Data []float32
	// OriginX and OriginY are the outer corner of the top-left pixel.
	OriginX, OriginY float64
	PixelW, PixelH   float64
	BigTIFF          bool
	Compression      int // 1 none, 5 LZW, 8 Deflate
	Predictor        int // 1 or 3
	NoData           float64
	EPSG             int
	Geographic       bool
	Overviews        int
	// Sparse leaves out blocks that hold no data, as the HRDEM mosaics do.
	Sparse bool
}

type level struct {
	w, h   int
	data   []float32
	blocks [][]byte
}

// Write returns the file.
func Write(o Options) []byte {
	le := binary.LittleEndian
	if o.Compression == 0 {
		o.Compression = 1
	}
	if o.Predictor == 0 {
		o.Predictor = 1
	}
	levels := []level{{w: o.Width, h: o.Height, data: o.Data}}
	for i := 0; i < o.Overviews; i++ {
		p := levels[len(levels)-1]
		l := level{w: (p.w + 1) / 2, h: (p.h + 1) / 2}
		l.data = make([]float32, l.w*l.h)
		for y := 0; y < l.h; y++ {
			for x := 0; x < l.w; x++ {
				var sum float64
				var n int
				for _, d := range [4][2]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
					sx, sy := 2*x+d[0], 2*y+d[1]
					if sx < p.w && sy < p.h {
						if v := p.data[sy*p.w+sx]; v == v {
							sum += float64(v)
							n++
						}
					}
				}
				if n > 0 {
					l.data[y*l.w+x] = float32(sum / float64(n))
				} else {
					l.data[y*l.w+x] = float32(math.NaN())
				}
			}
		}
		levels = append(levels, l)
	}
	for li := range levels {
		l := &levels[li]
		across, down := (l.w+o.Tile-1)/o.Tile, (l.h+o.Tile-1)/o.Tile
		for by := 0; by < down; by++ {
			for bx := 0; bx < across; bx++ {
				raw := make([]byte, o.Tile*o.Tile*4)
				any := false
				for y := 0; y < o.Tile; y++ {
					for x := 0; x < o.Tile; x++ {
						v := float32(o.NoData)
						sx, sy := bx*o.Tile+x, by*o.Tile+y
						if sx < l.w && sy < l.h {
							if d := l.data[sy*l.w+sx]; d == d {
								v, any = d, true
							}
						}
						le.PutUint32(raw[(y*o.Tile+x)*4:], math.Float32bits(v))
					}
				}
				if o.Sparse && !any {
					l.blocks = append(l.blocks, nil)
					continue
				}
				if o.Predictor == 3 {
					raw = floatPredict(raw, o.Tile)
				}
				switch o.Compression {
				case 5:
					raw = lzwEncode(raw)
				case 8:
					var b bytes.Buffer
					zw := zlib.NewWriter(&b)
					_, _ = zw.Write(raw)
					_ = zw.Close()
					raw = b.Bytes()
				}
				l.blocks = append(l.blocks, raw)
			}
		}
	}

	// Layout: header, then per level its directory followed by its out-of-line
	// values, then all block data.
	type ent struct {
		tag, typ uint16
		count    int
		val      []byte // inline (padded) or nil when out of line
		ext      []byte
	}
	u16 := func(v int) []byte { b := make([]byte, 2); le.PutUint16(b, uint16(v)); return b }
	u32 := func(v int) []byte { b := make([]byte, 4); le.PutUint32(b, uint32(v)); return b }
	f64s := func(vs ...float64) []byte {
		b := make([]byte, 8*len(vs))
		for i, v := range vs {
			le.PutUint64(b[i*8:], math.Float64bits(v))
		}
		return b
	}
	valSize, entSize, cntSize, nextSize, ptrType := 4, 12, 2, 4, uint16(4)
	hdr := 8
	if o.BigTIFF {
		valSize, entSize, cntSize, nextSize, ptrType = 8, 20, 8, 8, 16
		hdr = 16
	}
	ptrSize := valSize
	dirs := make([][]ent, len(levels))
	for li, l := range levels {
		n := len(l.blocks)
		es := []ent{}
		if li > 0 {
			es = append(es, ent{tag: 254, typ: 4, count: 1, val: u32(1)})
		}
		es = append(es,
			ent{tag: 256, typ: 4, count: 1, val: u32(l.w)},
			ent{tag: 257, typ: 4, count: 1, val: u32(l.h)},
			ent{tag: 258, typ: 3, count: 1, val: u16(32)},
			ent{tag: 259, typ: 3, count: 1, val: u16(o.Compression)},
			ent{tag: 277, typ: 3, count: 1, val: u16(1)},
			ent{tag: 317, typ: 3, count: 1, val: u16(o.Predictor)},
			ent{tag: 322, typ: 4, count: 1, val: u32(o.Tile)},
			ent{tag: 323, typ: 4, count: 1, val: u32(o.Tile)},
			ent{tag: 324, typ: ptrType, count: n, ext: make([]byte, n*ptrSize)},
			ent{tag: 325, typ: ptrType, count: n, ext: make([]byte, n*ptrSize)},
			ent{tag: 339, typ: 3, count: 1, val: u16(3)},
		)
		if li == 0 {
			key := 3072
			if o.Geographic {
				key = 2048
			}
			geo := []byte{}
			for _, v := range []int{1, 1, 0, 1, key, 0, 1, o.EPSG} {
				geo = append(geo, u16(v)...)
			}
			nd := append([]byte(strconv.FormatFloat(o.NoData, 'g', -1, 64)), 0)
			es = append(es,
				ent{tag: 33550, typ: 12, count: 3, ext: f64s(o.PixelW, o.PixelH, 0)},
				ent{tag: 33922, typ: 12, count: 6, ext: f64s(0, 0, 0, o.OriginX, o.OriginY, 0)},
				ent{tag: 34735, typ: 3, count: 8, ext: geo},
				ent{tag: 42113, typ: 2, count: len(nd), ext: nd},
			)
		}
		dirs[li] = es
	}
	// Positions.
	pos := hdr
	dirPos := make([]int, len(levels))
	extPos := make([][]int, len(levels))
	for li, es := range dirs {
		dirPos[li] = pos
		pos += cntSize + len(es)*entSize + nextSize
		extPos[li] = make([]int, len(es))
		// Small values first, the block index last, as GDAL lays a COG out: a reader
		// then gets every tag but the index with the first bytes of the file.
		for _, index := range []bool{false, true} {
			for i := range es {
				e := &es[i]
				if (e.tag == 324 || e.tag == 325) != index {
					continue
				}
				if e.ext != nil && len(e.ext) <= valSize && !index {
					e.val, e.ext = e.ext, nil
				}
				if e.ext != nil {
					extPos[li][i] = pos
					pos += len(e.ext)
					if pos%2 == 1 {
						pos++
					}
				}
			}
		}
	}
	// Block data, coarsest level first as GDAL lays COGs out.
	blockPos := make([][]int, len(levels))
	for li := len(levels) - 1; li >= 0; li-- {
		blockPos[li] = make([]int, len(levels[li].blocks))
		for bi, b := range levels[li].blocks {
			if b == nil {
				continue
			}
			blockPos[li][bi] = pos
			pos += len(b)
		}
	}
	out := make([]byte, pos)
	putPtr := func(b []byte, v int) {
		if o.BigTIFF {
			le.PutUint64(b, uint64(v))
		} else {
			le.PutUint32(b, uint32(v))
		}
	}
	copy(out, "II")
	if o.BigTIFF {
		le.PutUint16(out[2:], 43)
		le.PutUint16(out[4:], 8)
		le.PutUint64(out[8:], uint64(dirPos[0]))
	} else {
		le.PutUint16(out[2:], 42)
		le.PutUint32(out[4:], uint32(dirPos[0]))
	}
	for li, es := range dirs {
		p := dirPos[li]
		if o.BigTIFF {
			le.PutUint64(out[p:], uint64(len(es)))
		} else {
			le.PutUint16(out[p:], uint16(len(es)))
		}
		p += cntSize
		for i, e := range es {
			le.PutUint16(out[p:], e.tag)
			le.PutUint16(out[p+2:], e.typ)
			if o.BigTIFF {
				le.PutUint64(out[p+4:], uint64(e.count))
			} else {
				le.PutUint32(out[p+4:], uint32(e.count))
			}
			v := out[p+entSize-valSize : p+entSize]
			if e.ext == nil {
				copy(v, e.val)
			} else {
				putPtr(v, extPos[li][i])
				ext := e.ext
				if e.tag == 324 || e.tag == 325 {
					for bi, b := range levels[li].blocks {
						val := len(b)
						if e.tag == 324 {
							val = blockPos[li][bi]
						}
						putPtr(ext[bi*ptrSize:], val)
					}
				}
				copy(out[extPos[li][i]:], ext)
			}
			p += entSize
		}
		if li+1 < len(dirs) {
			putPtr(out[p:], dirPos[li+1])
		}
	}
	for li, l := range levels {
		for bi, b := range l.blocks {
			copy(out[blockPos[li][bi]:], b)
		}
	}
	return out
}

// floatPredict applies TIFF predictor 3 to little-endian float32 rows.
func floatPredict(raw []byte, w int) []byte {
	out := make([]byte, len(raw))
	rowBytes := w * 4
	for r := 0; r*rowBytes < len(raw); r++ {
		in := raw[r*rowBytes : (r+1)*rowBytes]
		row := out[r*rowBytes : (r+1)*rowBytes]
		for i := 0; i < w; i++ {
			row[i], row[w+i], row[2*w+i], row[3*w+i] = in[i*4+3], in[i*4+2], in[i*4+1], in[i*4]
		}
		for i := len(row) - 1; i > 0; i-- {
			row[i] -= row[i-1]
		}
	}
	return out
}

// lzwEncode is TIFF LZW as libtiff writes it.
func lzwEncode(src []byte) []byte {
	var out bytes.Buffer
	var acc uint32
	var nacc uint
	width := uint(9)
	put := func(code int) {
		acc = acc<<width | uint32(code)
		nacc += width
		for nacc >= 8 {
			out.WriteByte(byte(acc >> (nacc - 8)))
			nacc -= 8
		}
	}
	table := map[uint32]int{}
	next := 258
	put(256)
	if len(src) == 0 {
		put(257)
	} else {
		ent := int(src[0])
		grow := func() {
			if next == 4094 {
				put(256)
				table = map[uint32]int{}
				next, width = 258, 9
			} else if next > 1<<width-1 {
				width++
			}
		}
		for _, c := range src[1:] {
			k := uint32(ent)<<8 | uint32(c)
			if code, ok := table[k]; ok {
				ent = code
				continue
			}
			put(ent)
			table[k] = next
			next++
			ent = int(c)
			grow()
		}
		put(ent)
		next++
		grow()
		put(257)
	}
	if nacc > 0 {
		out.WriteByte(byte(acc << (8 - nacc)))
	}
	return out.Bytes()
}
