package build

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
)

// A minimal PMTiles v3 reader, enough for the verify stage to prove an archive opens and
// returns tiles. The serving code lives in internal/tiles.

type pmHeader struct {
	rootOff, rootLen       uint64
	leafOff                uint64
	dataOff                uint64
	internalComp, tileComp uint8
	tileType               uint8
	minZoom, maxZoom       uint8
	bounds                 [4]float64
}

type pmEntry struct {
	tileID, offset uint64
	length, runLen uint32
}

type pmReader struct {
	f *os.File
	h pmHeader
}

func openPMTiles(path string) (*pmReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 127)
	if _, err := io.ReadFull(f, buf); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("pmtiles header: %w", err)
	}
	if string(buf[:7]) != "PMTiles" || buf[7] != 3 {
		_ = f.Close()
		return nil, errors.New("not a PMTiles v3 archive")
	}
	le := binary.LittleEndian
	e7 := func(o int) float64 { return float64(int32(le.Uint32(buf[o:]))) / 1e7 }
	h := pmHeader{
		rootOff: le.Uint64(buf[8:]), rootLen: le.Uint64(buf[16:]),
		leafOff: le.Uint64(buf[40:]), dataOff: le.Uint64(buf[56:]),
		internalComp: buf[97], tileComp: buf[98], tileType: buf[99],
		minZoom: buf[100], maxZoom: buf[101],
		bounds: [4]float64{e7(102), e7(106), e7(110), e7(114)},
	}
	return &pmReader{f: f, h: h}, nil
}

func (r *pmReader) Close() error { return r.f.Close() }

func (r *pmReader) readAt(off, n uint64) ([]byte, error) {
	b := make([]byte, n)
	_, err := r.f.ReadAt(b, int64(off))
	return b, err
}

func (r *pmReader) dir(off, n uint64) ([]pmEntry, error) {
	raw, err := r.readAt(off, n)
	if err != nil {
		return nil, err
	}
	switch r.h.internalComp {
	case 1, 0: // none / unknown: try raw
	case 2:
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		raw, err = io.ReadAll(zr)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported internal compression %d", r.h.internalComp)
	}
	return parseDir(raw)
}

func parseDir(raw []byte) ([]pmEntry, error) {
	br := bytes.NewReader(raw)
	rd := func() uint64 { v, _ := binary.ReadUvarint(br); return v }
	n := rd()
	if n > 1<<24 {
		return nil, errors.New("directory too large")
	}
	es := make([]pmEntry, n)
	var last uint64
	for i := range es {
		last += rd()
		es[i].tileID = last
	}
	for i := range es {
		es[i].runLen = uint32(rd())
	}
	for i := range es {
		es[i].length = uint32(rd())
	}
	for i := range es {
		v := rd()
		if v == 0 && i > 0 {
			es[i].offset = es[i-1].offset + uint64(es[i-1].length)
		} else {
			es[i].offset = v - 1
		}
	}
	return es, nil
}

func findEntry(es []pmEntry, id uint64) (pmEntry, bool) {
	i := sort.Search(len(es), func(i int) bool { return es[i].tileID > id }) - 1
	if i < 0 {
		return pmEntry{}, false
	}
	e := es[i]
	if e.runLen == 0 || id-e.tileID < uint64(e.runLen) {
		return e, true
	}
	return pmEntry{}, false
}

// tileLen returns the stored length of tile z/x/y (0, false when absent).
func (r *pmReader) tileLen(z uint8, x, y uint32) (uint32, bool, error) {
	id := zxyToID(z, x, y)
	off, n := r.h.rootOff, r.h.rootLen
	for depth := 0; depth < 4; depth++ {
		es, err := r.dir(off, n)
		if err != nil {
			return 0, false, err
		}
		e, ok := findEntry(es, id)
		if !ok {
			return 0, false, nil
		}
		if e.runLen > 0 {
			return e.length, true, nil
		}
		off, n = r.h.leafOff+e.offset, uint64(e.length)
	}
	return 0, false, errors.New("directory too deep")
}

// zxyToID is the PMTiles Hilbert tile id.
func zxyToID(z uint8, x, y uint32) uint64 {
	acc := (uint64(1)<<(2*uint64(z)) - 1) / 3
	n := uint64(1) << z
	for s := n / 2; s > 0; s /= 2 {
		var rx, ry uint64
		if uint64(x)&s != 0 {
			rx = 1
		}
		if uint64(y)&s != 0 {
			ry = 1
		}
		acc += s * s * ((3 * rx) ^ ry)
		if ry == 0 {
			if rx == 1 {
				x = uint32(s - 1 - uint64(x)&(s-1))
				y = uint32(s - 1 - uint64(y)&(s-1))
			}
			x, y = y, x
		}
		x &= uint32(s - 1)
		y &= uint32(s - 1)
	}
	return acc
}

// lonLatToTile returns the XYZ tile containing lon/lat at zoom z.
func lonLatToTile(lon, lat float64, z uint8) (uint32, uint32) {
	n := math.Exp2(float64(z))
	x := (lon + 180) / 360 * n
	lr := lat * math.Pi / 180
	y := (1 - math.Log(math.Tan(lr)+1/math.Cos(lr))/math.Pi) / 2 * n
	clamp := func(v float64) uint32 { return uint32(math.Max(0, math.Min(n-1, math.Floor(v)))) }
	return clamp(x), clamp(y)
}
