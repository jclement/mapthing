// Package cog reads elevation blocks out of cloud-optimised GeoTIFFs with
// plain byte-range reads, so a caller can sample a small window of a
// multi-gigabyte DEM without downloading it. It understands exactly what the
// public DEM sources mapthing uses need: tiled classic TIFF and BigTIFF,
// one float32 (or int16) band, LZW or Deflate compression with predictor 1, 2
// or 3, and reduced-resolution overviews.
package cog

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// RangeReader returns n bytes at offset off of one remote file. Fewer bytes
// are returned only at the end of the file.
type RangeReader interface {
	ReadRange(ctx context.Context, off, n int64) ([]byte, error)
}

// ErrFormat reports a file this reader cannot use.
var ErrFormat = errors.New("cog: unsupported or corrupt file")

// PageSize is the unit in which header and tile-index bytes are read. Callers
// that cache ReadRange results see offsets aligned to it.
const PageSize = 64 << 10

// Level is one resolution of the file: level 0 is full resolution, each
// following one is an overview, coarser than the one before.
type Level struct {
	Width, Height int // pixels
	TileW, TileH  int // block size
	Across, Down  int // blocks per row and column
	// PixelW and PixelH are the pixel size in CRS units (PixelH positive).
	PixelW, PixelH float64

	offsets, counts array
	compression     int
	predictor       int
}

// array is a TIFF value array that may be far larger than we want to read.
type array struct {
	off   int64 // file offset of the first element
	size  int   // element size in bytes: 2, 4 or 8
	count int
	// inline holds the values when they were stored in the IFD entry itself.
	inline []byte
}

// File is an open COG: its geometry and where its blocks are.
type File struct {
	Levels []Level
	// OriginX and OriginY are the CRS coordinates of the outer corner of the
	// top-left pixel.
	OriginX, OriginY float64
	// NoData is the file's nodata value (HasNoData tells whether it has one).
	NoData    float64
	HasNoData bool
	// EPSG is the projected or geographic CRS code from the GeoKeys (0 if absent).
	EPSG int

	order      binary.ByteOrder
	big        bool
	bits       int
	sampleFmt  int // 1 uint, 2 int, 3 float
	meta       *pager
	headerRead int64 // bytes of header parsed (for diagnostics)
}

// pager reads the file's metadata in aligned pages and keeps them, so the
// tile-index lookups of neighbouring blocks cost no further requests.
type pager struct {
	src   RangeReader
	mu    sync.Mutex
	pages map[int64][]byte
}

func (p *pager) page(ctx context.Context, pn int64) ([]byte, error) {
	p.mu.Lock()
	pg, ok := p.pages[pn]
	p.mu.Unlock()
	if ok {
		return pg, nil
	}
	pg, err := p.src.ReadRange(ctx, pn*PageSize, PageSize)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.pages[pn] = pg
	p.mu.Unlock()
	return pg, nil
}

func (p *pager) read(ctx context.Context, off, n int64) ([]byte, error) {
	if n < 0 || off < 0 {
		return nil, ErrFormat
	}
	out := make([]byte, 0, n)
	for n > 0 {
		pn := off / PageSize
		pg, err := p.page(ctx, pn)
		if err != nil {
			return nil, err
		}
		in := off - pn*PageSize
		if in >= int64(len(pg)) {
			return nil, fmt.Errorf("%w: read past end of file", ErrFormat)
		}
		take := int64(len(pg)) - in
		if take > n {
			take = n
		}
		out = append(out, pg[in:in+take]...)
		off += take
		n -= take
	}
	return out, nil
}

// TIFF tags used here.
const (
	tagNewSubfileType = 254
	tagWidth          = 256
	tagHeight         = 257
	tagBits           = 258
	tagCompression    = 259
	tagSamplesPerPix  = 277
	tagPredictor      = 317
	tagTileWidth      = 322
	tagTileHeight     = 323
	tagTileOffsets    = 324
	tagTileByteCounts = 325
	tagSampleFormat   = 339
	tagPixelScale     = 33550
	tagTiepoint       = 33922
	tagGeoKeys        = 34735
	tagNoData         = 42113
)

type entry struct {
	tag   int
	typ   int
	count int
	// val is the value/offset field, raw.
	val []byte
}

var typeSize = map[int]int{1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 6: 1, 7: 1, 8: 2, 9: 4, 10: 8, 11: 4, 12: 8, 16: 8, 17: 8, 18: 8}

// Open parses the header and every image directory of the file behind src.
// It reads only metadata pages (usually one).
func Open(ctx context.Context, src RangeReader) (*File, error) {
	f := &File{meta: &pager{src: src, pages: map[int64][]byte{}}}
	h, err := f.meta.read(ctx, 0, 16)
	if err != nil {
		return nil, err
	}
	switch string(h[:2]) {
	case "II":
		f.order = binary.LittleEndian
	case "MM":
		f.order = binary.BigEndian
	default:
		return nil, fmt.Errorf("%w: not a TIFF", ErrFormat)
	}
	var next int64
	switch f.order.Uint16(h[2:]) {
	case 42:
		next = int64(f.order.Uint32(h[4:]))
	case 43:
		f.big = true
		next = int64(f.order.Uint64(h[8:]))
	default:
		return nil, fmt.Errorf("%w: not a TIFF", ErrFormat)
	}
	var fullW int
	var scaleX, scaleY float64
	for n := 0; next != 0 && n < 64; n++ {
		ents, nx, err := f.readIFD(ctx, next)
		if err != nil {
			return nil, err
		}
		next = nx
		lv, sub, err := f.level(ctx, ents)
		if err != nil {
			return nil, err
		}
		if sub&4 != 0 { // a transparency mask, not elevation
			continue
		}
		if len(f.Levels) == 0 {
			if err := f.georef(ctx, ents, &scaleX, &scaleY); err != nil {
				return nil, err
			}
			fullW = lv.Width
			lv.PixelW, lv.PixelH = scaleX, scaleY
		} else {
			if sub&1 == 0 {
				continue // a second full-resolution image; not something a DEM COG has
			}
			lv.PixelW = scaleX * float64(fullW) / float64(lv.Width)
			lv.PixelH = scaleY * float64(f.Levels[0].Height) / float64(lv.Height)
		}
		f.Levels = append(f.Levels, lv)
	}
	if len(f.Levels) == 0 {
		return nil, fmt.Errorf("%w: no image", ErrFormat)
	}
	sort.SliceStable(f.Levels[1:], func(i, j int) bool { return f.Levels[1+i].Width > f.Levels[1+j].Width })
	return f, nil
}

func (f *File) readIFD(ctx context.Context, off int64) ([]entry, int64, error) {
	cntSize, entSize, valSize, nextSize := int64(2), int64(12), 4, int64(4)
	if f.big {
		cntSize, entSize, valSize, nextSize = 8, 20, 8, 8
	}
	b, err := f.meta.read(ctx, off, cntSize)
	if err != nil {
		return nil, 0, err
	}
	var n int64
	if f.big {
		n = int64(f.order.Uint64(b))
	} else {
		n = int64(f.order.Uint16(b))
	}
	if n <= 0 || n > 4096 {
		return nil, 0, fmt.Errorf("%w: bad directory", ErrFormat)
	}
	b, err = f.meta.read(ctx, off+cntSize, n*entSize+nextSize)
	if err != nil {
		return nil, 0, err
	}
	f.headerRead = off + cntSize + n*entSize + nextSize
	ents := make([]entry, n)
	for i := range ents {
		e := b[int64(i)*entSize:]
		ents[i].tag = int(f.order.Uint16(e))
		ents[i].typ = int(f.order.Uint16(e[2:]))
		if f.big {
			ents[i].count = int(f.order.Uint64(e[4:]))
			ents[i].val = e[12 : 12+valSize]
		} else {
			ents[i].count = int(f.order.Uint32(e[4:]))
			ents[i].val = e[8 : 8+valSize]
		}
	}
	tail := b[n*entSize:]
	var next int64
	if f.big {
		next = int64(f.order.Uint64(tail))
	} else {
		next = int64(f.order.Uint32(tail))
	}
	return ents, next, nil
}

// arrayOf describes an entry's values without reading them.
func (f *File) arrayOf(e entry) (array, error) {
	sz, ok := typeSize[e.typ]
	if !ok || e.count < 0 {
		return array{}, fmt.Errorf("%w: tag %d has type %d", ErrFormat, e.tag, e.typ)
	}
	a := array{size: sz, count: e.count}
	if sz*e.count <= len(e.val) {
		a.inline = e.val[:sz*e.count]
		return a, nil
	}
	if f.big {
		a.off = int64(f.order.Uint64(e.val))
	} else {
		a.off = int64(f.order.Uint32(e.val))
	}
	return a, nil
}

// bytesOf reads all of an entry's values (for small tags only).
func (f *File) bytesOf(ctx context.Context, e entry) ([]byte, int, error) {
	a, err := f.arrayOf(e)
	if err != nil {
		return nil, 0, err
	}
	if a.inline != nil {
		return a.inline, a.size, nil
	}
	if int64(a.size)*int64(a.count) > 1<<20 {
		return nil, 0, fmt.Errorf("%w: tag %d is too large", ErrFormat, e.tag)
	}
	b, err := f.meta.read(ctx, a.off, int64(a.size*a.count))
	return b, a.size, err
}

func (f *File) uintOf(e entry) int {
	switch typeSize[e.typ] {
	case 1:
		return int(e.val[0])
	case 2:
		return int(f.order.Uint16(e.val))
	case 4:
		return int(f.order.Uint32(e.val))
	case 8:
		return int(f.order.Uint64(e.val))
	}
	return 0
}

func (f *File) level(_ context.Context, ents []entry) (Level, int, error) {
	lv := Level{compression: 1, predictor: 1}
	sub, spp := 0, 1
	bits, sfmt := 0, 1
	for _, e := range ents {
		switch e.tag {
		case tagNewSubfileType:
			sub = f.uintOf(e)
		case tagWidth:
			lv.Width = f.uintOf(e)
		case tagHeight:
			lv.Height = f.uintOf(e)
		case tagBits:
			bits = f.uintOf(e)
		case tagCompression:
			lv.compression = f.uintOf(e)
		case tagSamplesPerPix:
			spp = f.uintOf(e)
		case tagPredictor:
			lv.predictor = f.uintOf(e)
		case tagTileWidth:
			lv.TileW = f.uintOf(e)
		case tagTileHeight:
			lv.TileH = f.uintOf(e)
		case tagSampleFormat:
			sfmt = f.uintOf(e)
		case tagTileOffsets, tagTileByteCounts:
			a, err := f.arrayOf(e)
			if err != nil {
				return lv, 0, err
			}
			if e.tag == tagTileOffsets {
				lv.offsets = a
			} else {
				lv.counts = a
			}
		}
	}
	if sub&4 != 0 {
		return lv, sub, nil
	}
	if lv.Width <= 0 || lv.Height <= 0 {
		return lv, 0, fmt.Errorf("%w: no image size", ErrFormat)
	}
	if lv.TileW <= 0 || lv.TileH <= 0 || lv.TileW > 4096 || lv.TileH > 4096 {
		return lv, 0, fmt.Errorf("%w: not tiled", ErrFormat)
	}
	if spp != 1 {
		return lv, 0, fmt.Errorf("%w: %d bands", ErrFormat, spp)
	}
	switch {
	case sfmt == 3 && bits == 32, sfmt == 2 && bits == 16, sfmt == 1 && bits == 16:
	default:
		return lv, 0, fmt.Errorf("%w: sample format %d with %d bits", ErrFormat, sfmt, bits)
	}
	switch lv.compression {
	case 1, 5, 8, 32946:
	default:
		return lv, 0, fmt.Errorf("%w: compression %d", ErrFormat, lv.compression)
	}
	if lv.predictor < 1 || lv.predictor > 3 || (lv.predictor == 3 && sfmt != 3) {
		return lv, 0, fmt.Errorf("%w: predictor %d", ErrFormat, lv.predictor)
	}
	if f.bits == 0 {
		f.bits, f.sampleFmt = bits, sfmt
	} else if f.bits != bits || f.sampleFmt != sfmt {
		return lv, 0, fmt.Errorf("%w: overviews differ in sample type", ErrFormat)
	}
	lv.Across = (lv.Width + lv.TileW - 1) / lv.TileW
	lv.Down = (lv.Height + lv.TileH - 1) / lv.TileH
	if lv.offsets.count != lv.Across*lv.Down || lv.counts.count != lv.offsets.count {
		return lv, 0, fmt.Errorf("%w: tile index does not match the image size", ErrFormat)
	}
	return lv, sub, nil
}

func (f *File) georef(ctx context.Context, ents []entry, sx, sy *float64) error {
	var haveScale, haveTie bool
	for _, e := range ents {
		switch e.tag {
		case tagPixelScale:
			b, _, err := f.bytesOf(ctx, e)
			if err != nil {
				return err
			}
			if e.typ != 12 || len(b) < 16 {
				return fmt.Errorf("%w: pixel scale", ErrFormat)
			}
			*sx = math.Float64frombits(f.order.Uint64(b))
			*sy = math.Float64frombits(f.order.Uint64(b[8:]))
			haveScale = *sx > 0 && *sy > 0
		case tagTiepoint:
			b, _, err := f.bytesOf(ctx, e)
			if err != nil {
				return err
			}
			if e.typ != 12 || len(b) < 48 {
				return fmt.Errorf("%w: tiepoint", ErrFormat)
			}
			v := make([]float64, 6)
			for i := range v {
				v[i] = math.Float64frombits(f.order.Uint64(b[i*8:]))
			}
			f.OriginX, f.OriginY = v[3], v[4]
			haveTie = true
			// A tiepoint at another raster position than (0,0) is shifted
			// below once the scale is known.
			defer func(i, j float64) {
				f.OriginX -= i * *sx
				f.OriginY += j * *sy
			}(v[0], v[1])
		case tagGeoKeys:
			b, sz, err := f.bytesOf(ctx, e)
			if err != nil {
				return err
			}
			if sz == 2 {
				for i := 8; i+8 <= len(b); i += 8 { // after the 4-short header, 4 shorts a key
					key, loc, val := f.order.Uint16(b[i:]), f.order.Uint16(b[i+2:]), f.order.Uint16(b[i+6:])
					// 3072 ProjectedCSType wins over 2048 GeographicType.
					if loc == 0 && (key == 3072 || (key == 2048 && f.EPSG == 0)) && val != 32767 {
						f.EPSG = int(val)
					}
				}
			}
		case tagNoData:
			b, _, err := f.bytesOf(ctx, e)
			if err != nil {
				return err
			}
			s := strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
			if v, err := strconv.ParseFloat(s, 64); err == nil {
				f.NoData, f.HasNoData = v, true
			}
		}
	}
	if !haveScale || !haveTie {
		return fmt.Errorf("%w: no georeferencing", ErrFormat)
	}
	return nil
}

// BlockRef says where one block's bytes are. Size 0 means the block is absent
// (a sparse file): every pixel in it is nodata.
type BlockRef struct {
	Level, Col, Row int
	Offset, Size    int64
}

// Locate finds the byte ranges of blocks (given as level, column, row). It
// reads tile-index pages as needed.
func (f *File) Locate(ctx context.Context, level, col, row int) (BlockRef, error) {
	ref := BlockRef{Level: level, Col: col, Row: row}
	if level < 0 || level >= len(f.Levels) {
		return ref, fmt.Errorf("cog: no level %d", level)
	}
	lv := &f.Levels[level]
	if col < 0 || row < 0 || col >= lv.Across || row >= lv.Down {
		return ref, nil
	}
	i := row*lv.Across + col
	// The offset and the size sit in two arrays, usually on two pages: read
	// them side by side.
	var (
		wg      sync.WaitGroup
		sizeErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		ref.Size, sizeErr = f.element(ctx, lv.counts, i)
	}()
	var err error
	ref.Offset, err = f.element(ctx, lv.offsets, i)
	wg.Wait()
	if err != nil {
		return ref, err
	}
	if sizeErr != nil {
		return ref, sizeErr
	}
	if ref.Offset == 0 {
		ref.Size = 0
	}
	if ref.Size < 0 || ref.Size > 256<<20 {
		return ref, fmt.Errorf("%w: block of %d bytes", ErrFormat, ref.Size)
	}
	return ref, nil
}

func (f *File) element(ctx context.Context, a array, i int) (int64, error) {
	var b []byte
	if a.inline != nil {
		b = a.inline[i*a.size:]
	} else {
		var err error
		if b, err = f.meta.read(ctx, a.off+int64(i)*int64(a.size), int64(a.size)); err != nil {
			return 0, err
		}
	}
	switch a.size {
	case 2:
		return int64(f.order.Uint16(b)), nil
	case 4:
		return int64(f.order.Uint32(b)), nil
	case 8:
		return int64(f.order.Uint64(b)), nil
	}
	return 0, ErrFormat
}

// Decode expands one block's raw bytes (as located by Locate) into
// TileW×TileH elevations, row-major, with nodata as NaN. raw may be nil for an
// absent block.
func (f *File) Decode(level int, raw []byte) ([]float32, error) {
	lv := &f.Levels[level]
	n := lv.TileW * lv.TileH
	out := make([]float32, n)
	if len(raw) == 0 {
		nan := float32(math.NaN())
		for i := range out {
			out[i] = nan
		}
		return out, nil
	}
	bps := f.bits / 8
	buf := make([]byte, n*bps)
	switch lv.compression {
	case 1:
		if len(raw) < len(buf) {
			return nil, fmt.Errorf("%w: short block", ErrFormat)
		}
		copy(buf, raw)
	case 5:
		got, err := lzwDecode(buf, raw)
		if err != nil {
			return nil, err
		}
		if got < len(buf) {
			return nil, fmt.Errorf("%w: short LZW block (%d of %d bytes)", ErrFormat, got, len(buf))
		}
	case 8, 32946:
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrFormat, err)
		}
		if _, err := io.ReadFull(zr, buf); err != nil {
			return nil, fmt.Errorf("%w: short deflate block: %w", ErrFormat, err)
		}
	}
	rowBytes := lv.TileW * bps
	switch lv.predictor {
	case 2: // horizontal differencing of whole samples
		for r := 0; r < lv.TileH; r++ {
			row := buf[r*rowBytes : (r+1)*rowBytes]
			if bps == 2 {
				for i := 1; i < lv.TileW; i++ {
					f.order.PutUint16(row[i*2:], f.order.Uint16(row[i*2:])+f.order.Uint16(row[i*2-2:]))
				}
			} else {
				for i := 1; i < lv.TileW; i++ {
					f.order.PutUint32(row[i*4:], f.order.Uint32(row[i*4:])+f.order.Uint32(row[i*4-4:]))
				}
			}
		}
	case 3: // floating point: byte-wise differencing over byte planes, most significant first
		w := lv.TileW
		for r := 0; r < lv.TileH; r++ {
			row := buf[r*rowBytes : (r+1)*rowBytes]
			for i := 1; i < len(row); i++ {
				row[i] += row[i-1]
			}
			o := out[r*w : (r+1)*w]
			for i := range o {
				o[i] = math.Float32frombits(uint32(row[i])<<24 | uint32(row[w+i])<<16 | uint32(row[2*w+i])<<8 | uint32(row[3*w+i]))
			}
		}
	}
	if lv.predictor != 3 {
		switch {
		case f.sampleFmt == 3:
			for i := range out {
				out[i] = math.Float32frombits(f.order.Uint32(buf[i*4:]))
			}
		case f.sampleFmt == 2:
			for i := range out {
				out[i] = float32(int16(f.order.Uint16(buf[i*2:])))
			}
		default:
			for i := range out {
				out[i] = float32(f.order.Uint16(buf[i*2:]))
			}
		}
	}
	nan := float32(math.NaN())
	nd := float32(f.NoData)
	for i, v := range out {
		// Besides the declared nodata, treat absurd values as holes: some
		// sources carry -32767/-999999 fill without declaring it.
		if (f.HasNoData && v == nd) || v < -12000 || v > 9500 || v != v {
			out[i] = nan
		}
	}
	return out, nil
}

// HeaderBytes is the size of the parsed directory area, for diagnostics.
func (f *File) HeaderBytes() int64 { return f.headerRead }

// MetaPages returns how many metadata pages have been read so far.
func (f *File) MetaPages() int {
	f.meta.mu.Lock()
	defer f.meta.mu.Unlock()
	return len(f.meta.pages)
}
