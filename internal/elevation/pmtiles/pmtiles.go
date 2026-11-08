// Package pmtiles is a small, dependency-free reader and writer for PMTiles v3
// archives (https://github.com/protomaps/PMTiles/blob/main/spec/v3/spec.md).
//
// It covers what the terrain build and the elevation service need: writing an
// unclustered archive tile by tile (tiles may arrive in any order and identical
// tiles are stored once), and random tile reads with leaf directories cached.
package pmtiles

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Compression and tile-type codes from the spec.
const (
	CompressionUnknown = 0
	CompressionNone    = 1
	CompressionGzip    = 2

	TileTypeUnknown = 0
	TileTypeMVT     = 1
	TileTypePNG     = 2
	TileTypeJPEG    = 3
	TileTypeWebP    = 4

	headerLen   = 127
	rootMaxSize = 16384 - headerLen
)

// Header is the fixed 127-byte PMTiles v3 header.
type Header struct {
	RootOffset, RootLength         uint64
	MetadataOffset, MetadataLength uint64
	LeafOffset, LeafLength         uint64
	DataOffset, DataLength         uint64
	AddressedTiles                 uint64
	TileEntries                    uint64
	TileContents                   uint64
	Clustered                      bool
	InternalCompression            uint8
	TileCompression                uint8
	TileType                       uint8
	MinZoom, MaxZoom               uint8
	MinLonE7, MinLatE7             int32
	MaxLonE7, MaxLatE7             int32
	CenterZoom                     uint8
	CenterLonE7, CenterLatE7       int32
}

func (h *Header) marshal() []byte {
	b := make([]byte, headerLen)
	copy(b, "PMTiles")
	b[7] = 3
	le := binary.LittleEndian
	le.PutUint64(b[8:], h.RootOffset)
	le.PutUint64(b[16:], h.RootLength)
	le.PutUint64(b[24:], h.MetadataOffset)
	le.PutUint64(b[32:], h.MetadataLength)
	le.PutUint64(b[40:], h.LeafOffset)
	le.PutUint64(b[48:], h.LeafLength)
	le.PutUint64(b[56:], h.DataOffset)
	le.PutUint64(b[64:], h.DataLength)
	le.PutUint64(b[72:], h.AddressedTiles)
	le.PutUint64(b[80:], h.TileEntries)
	le.PutUint64(b[88:], h.TileContents)
	if h.Clustered {
		b[96] = 1
	}
	b[97] = h.InternalCompression
	b[98] = h.TileCompression
	b[99] = h.TileType
	b[100] = h.MinZoom
	b[101] = h.MaxZoom
	le.PutUint32(b[102:], uint32(h.MinLonE7))
	le.PutUint32(b[106:], uint32(h.MinLatE7))
	le.PutUint32(b[110:], uint32(h.MaxLonE7))
	le.PutUint32(b[114:], uint32(h.MaxLatE7))
	b[118] = h.CenterZoom
	le.PutUint32(b[119:], uint32(h.CenterLonE7))
	le.PutUint32(b[123:], uint32(h.CenterLatE7))
	return b
}

func unmarshalHeader(b []byte) (Header, error) {
	var h Header
	if len(b) < headerLen || string(b[:7]) != "PMTiles" {
		return h, errors.New("pmtiles: not a PMTiles archive")
	}
	if b[7] != 3 {
		return h, fmt.Errorf("pmtiles: unsupported spec version %d", b[7])
	}
	le := binary.LittleEndian
	h.RootOffset = le.Uint64(b[8:])
	h.RootLength = le.Uint64(b[16:])
	h.MetadataOffset = le.Uint64(b[24:])
	h.MetadataLength = le.Uint64(b[32:])
	h.LeafOffset = le.Uint64(b[40:])
	h.LeafLength = le.Uint64(b[48:])
	h.DataOffset = le.Uint64(b[56:])
	h.DataLength = le.Uint64(b[64:])
	h.AddressedTiles = le.Uint64(b[72:])
	h.TileEntries = le.Uint64(b[80:])
	h.TileContents = le.Uint64(b[88:])
	h.Clustered = b[96] == 1
	h.InternalCompression = b[97]
	h.TileCompression = b[98]
	h.TileType = b[99]
	h.MinZoom = b[100]
	h.MaxZoom = b[101]
	h.MinLonE7 = int32(le.Uint32(b[102:]))
	h.MinLatE7 = int32(le.Uint32(b[106:]))
	h.MaxLonE7 = int32(le.Uint32(b[110:]))
	h.MaxLatE7 = int32(le.Uint32(b[114:]))
	h.CenterZoom = b[118]
	h.CenterLonE7 = int32(le.Uint32(b[119:]))
	h.CenterLatE7 = int32(le.Uint32(b[123:]))
	return h, nil
}

// Entry is one directory entry. RunLength 0 marks a pointer to a leaf directory.
type Entry struct {
	TileID    uint64
	Offset    uint64
	Length    uint32
	RunLength uint32
}

// ZxyToID converts a tile coordinate to its PMTiles (Hilbert) tile ID.
func ZxyToID(z uint8, x, y uint32) uint64 {
	var acc uint64
	for t := uint8(0); t < z; t++ {
		acc += uint64(1) << (2 * t)
	}
	n := uint64(1) << z
	tx, ty := uint64(x), uint64(y)
	var d uint64
	for s := n / 2; s > 0; s /= 2 {
		var rx, ry uint64
		if tx&s > 0 {
			rx = 1
		}
		if ty&s > 0 {
			ry = 1
		}
		d += s * s * ((3 * rx) ^ ry)
		// rotate
		if ry == 0 {
			if rx == 1 {
				tx = n - 1 - tx
				ty = n - 1 - ty
			}
			tx, ty = ty, tx
		}
	}
	return acc + d
}

// IDToZxy is the inverse of ZxyToID.
func IDToZxy(id uint64) (uint8, uint32, uint32) {
	var acc uint64
	var z uint8
	for {
		num := uint64(1) << (2 * z)
		if acc+num > id {
			break
		}
		acc += num
		z++
	}
	pos := id - acc
	n := uint64(1) << z
	var tx, ty uint64
	t := pos
	for s := uint64(1); s < n; s *= 2 {
		rx := 1 & (t / 2)
		ry := 1 & (t ^ rx)
		if ry == 0 {
			if rx == 1 {
				tx = s - 1 - tx
				ty = s - 1 - ty
			}
			tx, ty = ty, tx
		}
		tx += s * rx
		ty += s * ry
		t /= 4
	}
	return z, uint32(tx), uint32(ty)
}

func serializeEntries(entries []Entry) ([]byte, error) {
	var raw []byte
	raw = binary.AppendUvarint(raw, uint64(len(entries)))
	var last uint64
	for _, e := range entries {
		raw = binary.AppendUvarint(raw, e.TileID-last)
		last = e.TileID
	}
	for _, e := range entries {
		raw = binary.AppendUvarint(raw, uint64(e.RunLength))
	}
	for _, e := range entries {
		raw = binary.AppendUvarint(raw, uint64(e.Length))
	}
	for i, e := range entries {
		if i > 0 && e.Offset == entries[i-1].Offset+uint64(entries[i-1].Length) {
			raw = binary.AppendUvarint(raw, 0)
		} else {
			raw = binary.AppendUvarint(raw, e.Offset+1)
		}
	}
	return gzipBytes(raw)
}

func deserializeEntries(b []byte, compression uint8) ([]Entry, error) {
	raw, err := decompress(b, compression)
	if err != nil {
		return nil, err
	}
	r := bytes.NewReader(raw)
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, fmt.Errorf("pmtiles: directory: %w", err)
	}
	if n > uint64(len(raw)) {
		return nil, errors.New("pmtiles: directory: corrupt entry count")
	}
	entries := make([]Entry, n)
	var last uint64
	for i := range entries {
		v, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		last += v
		entries[i].TileID = last
	}
	for i := range entries {
		v, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		entries[i].RunLength = uint32(v)
	}
	for i := range entries {
		v, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		entries[i].Length = uint32(v)
	}
	for i := range entries {
		v, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		if v == 0 && i > 0 {
			entries[i].Offset = entries[i-1].Offset + uint64(entries[i-1].Length)
		} else {
			entries[i].Offset = v - 1
		}
	}
	return entries, nil
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decompress(b []byte, compression uint8) ([]byte, error) {
	switch compression {
	case CompressionNone, CompressionUnknown:
		return b, nil
	case CompressionGzip:
		r, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer func() { _ = r.Close() }()
		return io.ReadAll(r)
	default:
		return nil, fmt.Errorf("pmtiles: unsupported compression %d", compression)
	}
}

// findEntry returns the entry covering id: the last entry with TileID <= id,
// if it is a leaf pointer or its run covers id.
func findEntry(entries []Entry, id uint64) (Entry, bool) {
	lo, hi := 0, len(entries)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		switch {
		case entries[mid].TileID < id:
			lo = mid + 1
		case entries[mid].TileID > id:
			hi = mid - 1
		default:
			return entries[mid], true
		}
	}
	if hi >= 0 {
		e := entries[hi]
		if e.RunLength == 0 || id-e.TileID < uint64(e.RunLength) {
			return e, true
		}
	}
	return Entry{}, false
}
