// Package pmtiles reads and writes PMTiles v3 archives (https://github.com/protomaps/PMTiles,
// spec v3). It is a small, dependency-free implementation of the parts mapthing needs:
// tile lookup with leaf directories, metadata, and writing clustered archives (for packs and
// test fixtures).
package pmtiles

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// HeaderLen is the fixed header size.
const HeaderLen = 127

// Compression values.
const (
	CompressionUnknown = 0
	CompressionNone    = 1
	CompressionGzip    = 2
	CompressionBrotli  = 3
	CompressionZstd    = 4
)

// Tile types.
const (
	TypeUnknown = 0
	TypeMVT     = 1
	TypePNG     = 2
	TypeJPEG    = 3
	TypeWebP    = 4
	TypeAVIF    = 5
)

// Header is the PMTiles v3 header.
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

// Bounds returns [w, s, e, n] in degrees.
func (h Header) Bounds() [4]float64 {
	return [4]float64{float64(h.MinLonE7) / 1e7, float64(h.MinLatE7) / 1e7, float64(h.MaxLonE7) / 1e7, float64(h.MaxLatE7) / 1e7}
}

// Center returns [lon, lat, zoom].
func (h Header) Center() [3]float64 {
	return [3]float64{float64(h.CenterLonE7) / 1e7, float64(h.CenterLatE7) / 1e7, float64(h.CenterZoom)}
}

// ContentType returns the HTTP content type for the archive's tiles.
func (h Header) ContentType() string {
	switch h.TileType {
	case TypeMVT:
		return "application/vnd.mapbox-vector-tile"
	case TypePNG:
		return "image/png"
	case TypeJPEG:
		return "image/jpeg"
	case TypeWebP:
		return "image/webp"
	case TypeAVIF:
		return "image/avif"
	}
	return "application/octet-stream"
}

// Ext returns the conventional tile file extension.
func (h Header) Ext() string {
	switch h.TileType {
	case TypeMVT:
		return "mvt"
	case TypePNG:
		return "png"
	case TypeJPEG:
		return "jpg"
	case TypeWebP:
		return "webp"
	case TypeAVIF:
		return "avif"
	}
	return "bin"
}

// MarshalHeader encodes h into 127 bytes.
func MarshalHeader(h Header) []byte {
	b := make([]byte, HeaderLen)
	copy(b[0:7], "PMTiles")
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

// ErrNotPMTiles is returned for files without a PMTiles v3 header.
var ErrNotPMTiles = errors.New("not a PMTiles v3 archive")

// UnmarshalHeader decodes a header.
func UnmarshalHeader(b []byte) (Header, error) {
	var h Header
	if len(b) < HeaderLen || string(b[0:7]) != "PMTiles" {
		return h, ErrNotPMTiles
	}
	if b[7] != 3 {
		return h, fmt.Errorf("unsupported PMTiles version %d", b[7])
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

// Entry is one directory entry. RunLength 0 means Offset/Length point at a leaf directory.
type Entry struct {
	TileID    uint64
	Offset    uint64
	Length    uint32
	RunLength uint32
}

// MarshalDirectory serialises entries (sorted by TileID) and compresses them.
func MarshalDirectory(entries []Entry, compression uint8) ([]byte, error) {
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
	return Compress(raw, compression)
}

// UnmarshalDirectory decompresses and parses a directory.
func UnmarshalDirectory(b []byte, compression uint8) ([]Entry, error) {
	raw, err := Decompress(b, compression)
	if err != nil {
		return nil, err
	}
	r := bytes.NewReader(raw)
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	if n > uint64(len(raw)) { // each entry takes at least 4 bytes; cheap sanity bound
		return nil, errors.New("corrupt directory")
	}
	es := make([]Entry, n)
	var id uint64
	for i := range es {
		d, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		id += d
		es[i].TileID = id
	}
	for i := range es {
		v, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		es[i].RunLength = uint32(v)
	}
	for i := range es {
		v, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		es[i].Length = uint32(v)
	}
	for i := range es {
		v, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		if v == 0 && i > 0 {
			es[i].Offset = es[i-1].Offset + uint64(es[i-1].Length)
		} else {
			es[i].Offset = v - 1
		}
	}
	return es, nil
}

// findEntry returns the entry covering id: a tile run containing it, or a leaf whose range
// may contain it.
func findEntry(es []Entry, id uint64) (Entry, bool) {
	lo, hi := 0, len(es)-1
	for lo <= hi {
		m := (lo + hi) / 2
		switch {
		case es[m].TileID < id:
			lo = m + 1
		case es[m].TileID > id:
			hi = m - 1
		default:
			return es[m], true
		}
	}
	// hi is the last entry with TileID < id
	if hi >= 0 {
		e := es[hi]
		if e.RunLength == 0 {
			return e, true
		}
		if id-e.TileID < uint64(e.RunLength) {
			return e, true
		}
	}
	return Entry{}, false
}

// Compress compresses b (none or gzip).
func Compress(b []byte, c uint8) ([]byte, error) {
	switch c {
	case CompressionNone, CompressionUnknown:
		return b, nil
	case CompressionGzip:
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if _, err := zw.Write(b); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	return nil, fmt.Errorf("unsupported compression %d", c)
}

// Decompress decompresses b (none or gzip).
func Decompress(b []byte, c uint8) ([]byte, error) {
	switch c {
	case CompressionNone, CompressionUnknown:
		return b, nil
	case CompressionGzip:
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		return io.ReadAll(io.LimitReader(zr, 256<<20))
	}
	return nil, fmt.Errorf("unsupported compression %d", c)
}
